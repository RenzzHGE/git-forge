// Raw TCP + epoll HTTP/1.1 transport.
//
// Replaces net/http as the server's transport while keeping every handler
// (dispatchHandler and the git/REST/GraphQL/MCP surfaces) untouched: each
// parsed request is turned into a *http.Request and handed to
// dispatchHandler with an adapter ResponseWriter that buffers the whole
// response, exactly as the handlers already build it.
//
// Why: net/http pays, per connection, a goroutine (with its stack and
// scheduler churn) plus a general-purpose request parser and a per-request
// allocation shower; under the bench's 50 concurrent clones and 2,000
// sequential keep-alive reads that transport share is a real slice of the
// measured time. This transport is one epoll fd, one accept loop, and a
// bounded worker pool (no goroutine per connection), a hand parser over the
// buffered bytes, and a single write of the whole response.
//
// Concurrency: an fd's events are EPOLLONESHOT, so at most one worker owns
// a connection at a time; the worker reads until EAGAIN, dispatches every
// complete pipelined request in the buffer, and flushes the response with
// non-blocking writes, re-arming EPOLLIN or EPOLLOUT as needed. The
// handlers' own locking (storeMu) is unchanged.

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	epollMaxHeaderBytes = 64 * 1024
	epollMaxBodyBytes   = 16 * 1024 * 1024
	epollReadChunk      = 32 * 1024
)

// ---------------------------------------------------------------------------
// ResponseWriter adapter: buffers status, headers and body; the transport
// serializes them into one response after the handler returns.
// ---------------------------------------------------------------------------

type epollResponseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        []byte
}

func newEpollResponseWriter() *epollResponseWriter {
	return &epollResponseWriter{header: make(http.Header)}
}

func (w *epollResponseWriter) Header() http.Header { return w.header }

func (w *epollResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
}

func (w *epollResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.body = append(w.body, b...)
	return len(b), nil
}

// build serializes the buffered response: status line, handler headers
// (sorted, for determinism), Content-Length when the handler did not set
// one (the framing keep-alive needs), and Connection.
func (w *epollResponseWriter) build(keepAlive bool) []byte {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(status))
	b.WriteByte(' ')
	b.WriteString(statusText(status))
	b.WriteString("\r\n")
	_, hasLength := w.header["Content-Length"]
	keys := make([]string, 0, len(w.header))
	for k := range w.header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range w.header[k] {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\r\n")
		}
	}
	if !hasLength && status != http.StatusNoContent && status != http.StatusNotModified {
		b.WriteString("Content-Length: ")
		b.WriteString(strconv.Itoa(len(w.body)))
		b.WriteString("\r\n")
	}
	if keepAlive {
		b.WriteString("Connection: keep-alive\r\n")
	} else {
		b.WriteString("Connection: close\r\n")
	}
	b.WriteString("\r\n")
	b.Write(w.body)
	return b.Bytes()
}

func statusText(status int) string {
	if s := http.StatusText(status); s != "" {
		return s
	}
	return "Status"
}

// ---------------------------------------------------------------------------
// Request parsing (over buffered bytes; no allocation on the fast path
// beyond the request itself)
// ---------------------------------------------------------------------------

type parsedRequest struct {
	method        string
	target        string
	protoMajor    int
	protoMinor    int
	host          string
	header        http.Header
	body          []byte
	keepAlive     bool
	expectContinue bool
}

// parseRequests consumes complete requests from buf. It returns the number
// of bytes consumed, the parsed requests (in order), and whether more data
// is needed (true) or the stream is broken (false with err != nil).
func parseRequests(buf []byte) (consumed int, reqs []parsedRequest, needMore bool, err error) {
	for {
		// Headers end.
		hdrEnd := bytes.Index(buf[consumed:], []byte("\r\n\r\n"))
		if hdrEnd < 0 {
			if len(buf)-consumed > epollMaxHeaderBytes {
				return consumed, reqs, false, fmt.Errorf("headers too large")
			}
			return consumed, reqs, true, nil
		}
		head := buf[consumed : consumed+hdrEnd]
		bodyStart := consumed + hdrEnd + 4
		// Request line.
		lineEnd := bytes.Index(head, []byte("\r\n"))
		if lineEnd < 0 {
			lineEnd = len(head)
		}
		line := string(head[:lineEnd])
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return consumed, reqs, false, fmt.Errorf("bad request line")
		}
		method, target, version := parts[0], parts[1], parts[2]
		var major, minor int
		switch version {
		case "HTTP/1.1":
			major, minor = 1, 1
		case "HTTP/1.0":
			major, minor = 1, 0
		default:
			return consumed, reqs, false, fmt.Errorf("unsupported version %q", version)
		}
		// Headers.
		header := make(http.Header)
		host := ""
		rest := head[lineEnd:]
		if len(rest) > 0 {
			rest = rest[2:] // skip the CRLF after the request line
		}
		contentLength := -1
		chunked := false
		keepAlive := major == 1 && minor == 1 // 1.1 defaults to keep-alive
		expectContinue := false
		for len(rest) > 0 {
			eol := bytes.Index(rest, []byte("\r\n"))
			if eol < 0 {
				eol = len(rest)
			}
			hline := rest[:eol]
			rest = rest[min(eol+2, len(rest)):]
			if len(hline) == 0 {
				continue
			}
			colon := bytes.IndexByte(hline, ':')
			if colon <= 0 {
				return consumed, reqs, false, fmt.Errorf("bad header line")
			}
			name := string(hline[:colon])
			value := strings.TrimLeft(string(hline[colon+1:]), " \t")
			canon := http.CanonicalHeaderKey(name)
			switch canon {
			case "Host":
				host = value
			case "Content-Length":
				n, perr := strconv.Atoi(strings.TrimSpace(value))
				if perr != nil || n < 0 {
					return consumed, reqs, false, fmt.Errorf("bad Content-Length")
				}
				if contentLength >= 0 {
					return consumed, reqs, false, fmt.Errorf("duplicate Content-Length")
				}
				contentLength = n
			case "Transfer-Encoding":
				if strings.Contains(strings.ToLower(value), "chunked") {
					chunked = true
				}
			case "Connection":
				lv := strings.ToLower(value)
				if strings.Contains(lv, "close") {
					keepAlive = false
				} else if strings.Contains(lv, "keep-alive") {
					keepAlive = true
				}
			case "Expect":
				if strings.Contains(strings.ToLower(value), "100-continue") {
					expectContinue = true
				}
			default:
				header[canon] = append(header[canon], value)
			}
		}
		if major == 1 && minor == 1 && host == "" {
			return consumed, reqs, false, fmt.Errorf("HTTP/1.1 without Host")
		}
		if contentLength >= 0 && chunked {
			return consumed, reqs, false, fmt.Errorf("Content-Length with chunked encoding")
		}
		// Body.
		avail := buf[bodyStart:]
		var body []byte
		var bodyLen int
		if chunked {
			decoded, n, cerr := decodeChunked(avail)
			if cerr != nil {
				return consumed, reqs, false, cerr
			}
			if n < 0 {
				return consumed, reqs, true, nil
			}
			body, bodyLen = decoded, n
		} else if contentLength > 0 {
			if contentLength > epollMaxBodyBytes {
				return consumed, reqs, false, fmt.Errorf("body too large")
			}
			if len(avail) < contentLength {
				return consumed, reqs, true, nil
			}
			body, bodyLen = avail[:contentLength], contentLength
		} else {
			body, bodyLen = nil, 0
		}
		consumed = bodyStart + bodyLen
		reqs = append(reqs, parsedRequest{
			method:         method,
			target:         target,
			protoMajor:     major,
			protoMinor:     minor,
			host:           host,
			header:         header,
			body:           body,
			keepAlive:      keepAlive,
			expectContinue: expectContinue,
		})
		if consumed >= len(buf) {
			return consumed, reqs, true, nil
		}
	}
}

// decodeChunked decodes a chunked body starting at buf. It returns the
// decoded body, the bytes consumed, or n = -1 when more data is needed.
func decodeChunked(buf []byte) ([]byte, int, error) {
	var out []byte
	pos := 0
	for {
		eol := bytes.Index(buf[pos:], []byte("\r\n"))
		if eol < 0 {
			return nil, -1, nil
		}
		sizeLine := string(buf[pos : pos+eol])
		if semi := strings.IndexByte(sizeLine, ';'); semi >= 0 {
			sizeLine = sizeLine[:semi]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if err != nil || size < 0 {
			return nil, 0, fmt.Errorf("bad chunk size")
		}
		pos += eol + 2
		if size == 0 {
			// Trailers until an empty line.
			for {
				teol := bytes.Index(buf[pos:], []byte("\r\n"))
				if teol < 0 {
					return nil, -1, nil
				}
				if teol == 0 {
					pos += 2
					return out, pos, nil
				}
				pos += teol + 2
			}
		}
		if int64(len(buf)-pos) < size+2 {
			return nil, -1, nil
		}
		out = append(out, buf[pos:pos+int(size)]...)
		pos += int(size) + 2
		if len(out) > epollMaxBodyBytes {
			return nil, 0, fmt.Errorf("body too large")
		}
	}
}

// toHTTPRequest builds the *http.Request the handlers expect. The URL is
// parsed with ParseRequestURI so EscapedPath() matches net/http's.
func (p *parsedRequest) toHTTPRequest(remoteAddr string) (*http.Request, error) {
	u, err := url.ParseRequestURI(p.target)
	if err != nil {
		// Absolute-form targets are rare here; try a plain parse.
		u, err = url.Parse(p.target)
		if err != nil {
			return nil, err
		}
	}
	var body io.ReadCloser
	if len(p.body) > 0 {
		body = io.NopCloser(bytes.NewReader(p.body))
	} else {
		body = io.NopCloser(bytes.NewReader(nil))
	}
	return &http.Request{
		Method:        p.method,
		URL:           u,
		Proto:         fmt.Sprintf("HTTP/%d.%d", p.protoMajor, p.protoMinor),
		ProtoMajor:    p.protoMajor,
		ProtoMinor:    p.protoMinor,
		Header:        p.header,
		Body:          body,
		Host:          p.host,
		RemoteAddr:    remoteAddr,
		RequestURI:    p.target,
		ContentLength: int64(len(p.body)),
	}, nil
}

// ---------------------------------------------------------------------------
// Connection state
// ---------------------------------------------------------------------------

type epollConn struct {
	fd         int
	remoteAddr string
	in         []byte // unparsed bytes
	out        []byte // response bytes waiting to be written
	outOff     int
	closed     bool
}

type epollServer struct {
	listenFd int
	epFd     int
	port     int
	jobs     chan int
	mu       sync.Mutex
	conns    map[int]*epollConn
	shutdown chan struct{}
	once     sync.Once
}

func newEpollServer() (*epollServer, error) {
	fd, port, err := epollListen()
	if err != nil {
		return nil, err
	}
	epFd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("epoll create: %w", err)
	}
	s := &epollServer{
		listenFd: fd,
		epFd:     epFd,
		port:     port,
		jobs:     make(chan int, 4096),
		conns:    make(map[int]*epollConn),
		shutdown: make(chan struct{}),
	}
	if err := s.ctl(fd, syscall.EPOLLIN, false); err != nil {
		syscall.Close(epFd)
		syscall.Close(fd)
		return nil, err
	}
	workers := runtime.GOMAXPROCS(0) * 8
	if workers < 16 {
		workers = 16
	}
	for i := 0; i < workers; i++ {
		go s.worker()
	}
	go s.loop()
	return s, nil
}

// epollListen binds an ephemeral port (dual-stack when available) and
// returns the listening fd and port.
func epollListen() (int, int, error) {
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err == nil {
		// Dual-stack: accept IPv4-mapped connections too.
		_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		var sa syscall.SockaddrInet6
		sa.Port = 0
		if berr := syscall.Bind(fd, &sa); berr == nil {
			if lerr := syscall.Listen(fd, 256); lerr == nil {
				if addr, gerr := syscall.Getsockname(fd); gerr == nil {
					if inet6, ok := addr.(*syscall.SockaddrInet6); ok {
						return fd, inet6.Port, nil
					}
				}
			}
		}
		syscall.Close(fd)
	}
	fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("socket: %w", err)
	}
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	var sa syscall.SockaddrInet4
	sa.Port = 0
	if err := syscall.Bind(fd, &sa); err != nil {
		syscall.Close(fd)
		return 0, 0, fmt.Errorf("bind: %w", err)
	}
	if err := syscall.Listen(fd, 256); err != nil {
		syscall.Close(fd)
		return 0, 0, fmt.Errorf("listen: %w", err)
	}
	addr, err := syscall.Getsockname(fd)
	if err != nil {
		syscall.Close(fd)
		return 0, 0, fmt.Errorf("getsockname: %w", err)
	}
	inet4, ok := addr.(*syscall.SockaddrInet4)
	if !ok {
		syscall.Close(fd)
		return 0, 0, fmt.Errorf("unexpected sockaddr %T", addr)
	}
	return fd, inet4.Port, nil
}

// ctl (re-)arms fd in the epoll set. oneshot=true for connections (one
// worker owns the fd until it re-arms), false for the listener.
func (s *epollServer) ctl(fd int, events uint32, oneshot bool) error {
	ev := events
	if oneshot {
		ev |= syscall.EPOLLONESHOT | syscall.EPOLLRDHUP
	}
	event := &syscall.EpollEvent{Events: ev, Fd: int32(fd)}
	// ADD first; if the fd is already registered, MOD.
	if err := syscall.EpollCtl(s.epFd, syscall.EPOLL_CTL_ADD, fd, event); err != nil {
		if err == syscall.EEXIST {
			return syscall.EpollCtl(s.epFd, syscall.EPOLL_CTL_MOD, fd, event)
		}
		return err
	}
	return nil
}

func (s *epollServer) loop() {
	events := make([]syscall.EpollEvent, 128)
	for {
		n, err := syscall.EpollWait(s.epFd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			select {
			case <-s.shutdown:
				return
			default:
			}
			// epoll fd closed at shutdown; any other error is fatal
			// to the loop, so log and return (the process exits via
			// the shutdown path anyway).
			return
		}
		for i := 0; i < n; i++ {
			ev := events[i]
			fd := int(ev.Fd)
			if fd == s.listenFd {
				s.acceptAll()
				continue
			}
			if ev.Events&(syscall.EPOLLERR|syscall.EPOLLHUP) != 0 {
				s.closeConn(fd)
				continue
			}
			select {
			case s.jobs <- fd:
			case <-s.shutdown:
				return
			}
		}
	}
}

func (s *epollServer) acceptAll() {
	for {
		fd, sa, err := syscall.Accept4(s.listenFd, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				return
			}
			if err == syscall.EINTR {
				continue
			}
			return
		}
		// Nagle off: the bench's sequential keep-alive reads are
		// small request / small response pairs, where Nagle plus the
		// peer's delayed ACK can stall a round trip by tens of ms.
		// (Go's net package sets this by default; raw sockets do not.)
		_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		remote := ""
		if sa != nil {
			switch a := sa.(type) {
			case *syscall.SockaddrInet4:
				remote = fmt.Sprintf("%d.%d.%d.%d:%d", a.Addr[0], a.Addr[1], a.Addr[2], a.Addr[3], a.Port)
			case *syscall.SockaddrInet6:
				remote = fmt.Sprintf("[%x]:%d", a.Addr, a.Port)
			}
		}
		c := &epollConn{fd: fd, remoteAddr: remote}
		s.mu.Lock()
		s.conns[fd] = c
		s.mu.Unlock()
		if err := s.ctl(fd, syscall.EPOLLIN, true); err != nil {
			s.closeConn(fd)
		}
	}
}

func (s *epollServer) closeConn(fd int) {
	s.mu.Lock()
	c, ok := s.conns[fd]
	if ok {
		delete(s.conns, fd)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	c.closed = true
	// Best effort: remove from epoll, then close (closing alone also
	// removes it).
	_ = syscall.EpollCtl(s.epFd, syscall.EPOLL_CTL_DEL, fd, nil)
	_ = syscall.Close(fd)
}

func (s *epollServer) worker() {
	scratch := make([]byte, epollReadChunk)
	for fd := range s.jobs {
		s.handle(fd, scratch)
	}
}

// handle runs one readiness event for fd: read until EAGAIN, dispatch
// every complete request in the buffer, flush what it can, then re-arm
// (EPOLLOUT while output remains, else EPOLLIN) or close.
func (s *epollServer) handle(fd int, scratch []byte) {
	s.mu.Lock()
	c, ok := s.conns[fd]
	s.mu.Unlock()
	if !ok || c.closed {
		return
	}
	defer func() {
		// Never let a bug in one connection kill the server.
		if rec := recover(); rec != nil {
			logError("[epoll] connection %d: %v", fd, rec)
			s.closeConn(fd)
		}
	}()

	// Read until EAGAIN (or EOF/error).
	for {
		n, err := syscall.Read(fd, scratch)
		if n > 0 {
			c.in = append(c.in, scratch[:n]...)
			if len(c.in) > epollMaxHeaderBytes+epollMaxBodyBytes {
				s.send400AndClose(c)
				return
			}
			continue
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			break
		}
		if err == syscall.EINTR {
			continue
		}
		// EOF (n == 0) or a read error: drop the connection.
		s.closeConn(fd)
		return
	}

	// Dispatch complete requests, in order (pipelining included).
	keepAlive := true
	for {
		consumed, reqs, needMore, perr := parseRequests(c.in)
		if perr != nil {
			s.send400AndClose(c)
			return
		}
		if len(reqs) == 0 {
			if !needMore {
				s.send400AndClose(c)
				return
			}
			break
		}
		c.in = append(c.in[:0], c.in[consumed:]...)
		for _, pr := range reqs {
			if pr.expectContinue {
				// The client waits for the interim response before
				// sending the body; answer now (the body may still
				// be in flight, in which case the next parse waits).
				c.out = append(c.out, []byte("HTTP/1.1 100 Continue\r\n\r\n")...)
			}
			req, rerr := pr.toHTTPRequest(c.remoteAddr)
			if rerr != nil {
				s.send400AndClose(c)
				return
			}
			rw := newEpollResponseWriter()
			dispatchHandler(rw, req)
			c.out = append(c.out, rw.build(pr.keepAlive)...)
			keepAlive = pr.keepAlive
			if !pr.keepAlive {
				break
			}
		}
		if !keepAlive {
			break
		}
	}

	// Flush output (non-blocking); whatever remains waits for EPOLLOUT.
	for c.outOff < len(c.out) {
		n, err := syscall.Write(fd, c.out[c.outOff:])
		if n > 0 {
			c.outOff += n
			continue
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			break
		}
		if err == syscall.EINTR {
			continue
		}
		s.closeConn(fd)
		return
	}
	if c.outOff == len(c.out) {
		c.out = c.out[:0]
		c.outOff = 0
	}
	if !keepAlive && c.outOff == len(c.out) {
		s.closeConn(fd)
		return
	}
	if len(c.out) > 0 {
		// Output remains: wait until writable.
		if err := s.ctl(fd, syscall.EPOLLOUT, true); err != nil {
			s.closeConn(fd)
		}
		return
	}
	// No output pending: wait for the next request.
	if err := s.ctl(fd, syscall.EPOLLIN, true); err != nil {
		s.closeConn(fd)
	}
}

func (s *epollServer) send400AndClose(c *epollConn) {
	resp := []byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	// Best effort, non-blocking; the peer gets a 400 or a reset.
	_, _ = syscall.Write(c.fd, resp)
	s.closeConn(c.fd)
}

// close stops accepting and wakes the loop; the process exits right after,
// so in-flight workers are abandoned exactly like net/http's Close.
func (s *epollServer) close() {
	s.once.Do(func() {
		close(s.shutdown)
		_ = syscall.Close(s.listenFd)
		_ = syscall.Close(s.epFd)
	})
}
