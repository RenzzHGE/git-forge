// HTTP plumbing: the integrated server — git smart HTTP, REST v3, GraphQL
// v4 and MCP over one shared store, so the four surfaces agree.
// Dispatch: /healthz (no auth, the env's boot probe) first, then the four
// surfaces in path order, then a JSON 404 for everything else. Each handler
// returns false when the route is not its own, so the fall-through is safe.

package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

var storeMu sync.RWMutex

// shopToken reads MAKEFUN_SHOP_TOKEN at request time (mirrors the Node
// handlers, which read process.env per request).
func shopToken() string {
	return os.Getenv("MAKEFUN_SHOP_TOKEN")
}

// trackingWriter records whether WriteHeader was called, so error paths can
// decide whether a 500 is still writable.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(status int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

func wroteHeader(w http.ResponseWriter) bool {
	if tw, ok := w.(*trackingWriter); ok {
		return tw.wrote
	}
	return false
}

func logError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// urlPathUnescape mirrors decodeURIComponent: percent-decoding that errors
// on a malformed escape.
func urlPathUnescape(s string) (string, error) {
	return url.PathUnescape(s)
}

// ---------------------------------------------------------------------------
// Ordered query parameters (mirrors URLSearchParams: insertion order,
// set() replaces in place, serialization is application/x-www-form-urlencoded)
// ---------------------------------------------------------------------------

type queryParam struct {
	key   string
	value string
}

type queryParams []queryParam

func parseQueryOrdered(raw string) queryParams {
	var out queryParams
	if raw == "" {
		return out
	}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value := pair, ""
		if idx := strings.Index(pair, "="); idx >= 0 {
			key, value = pair[:idx], pair[idx+1:]
		}
		out = append(out, queryParam{key: queryUnescape(key), value: queryUnescape(value)})
	}
	return out
}

// queryUnescape decodes '+' as space and %XX sequences.
func queryUnescape(s string) string {
	if !strings.ContainsAny(s, "+%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '+':
			b.WriteByte(' ')
		case c == '%' && i+2 < len(s):
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (q queryParams) get(name string) (string, bool) {
	for _, p := range q {
		if p.key == name {
			return p.value, true
		}
	}
	return "", false
}

// querySet mirrors URLSearchParams.set: replace all occurrences with one
// value at the first occurrence's position, or append.
func querySet(q queryParams, name, value string) queryParams {
	out := make(queryParams, 0, len(q)+1)
	placed := false
	for _, p := range q {
		if p.key == name {
			if !placed {
				out = append(out, queryParam{key: name, value: value})
				placed = true
			}
			continue
		}
		out = append(out, p)
	}
	if !placed {
		out = append(out, queryParam{key: name, value: value})
	}
	return out
}

// queryString serializes like URLSearchParams.toString():
// application/x-www-form-urlencoded (space -> '+', everything except
// [A-Za-z0-9*-._] percent-encoded with uppercase hex).
func queryString(q queryParams) string {
	var b strings.Builder
	for i, p := range q {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(formEncode(p.key))
		b.WriteByte('=')
		b.WriteString(formEncode(p.value))
	}
	return b.String()
}

func formEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '*' || c == '-' || c == '.' || c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&0x0f])
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

// ownerFromSegments returns the owner named in a repo URL, if this
// request carries one:
//   /api/v3/repos/{owner}/{repo}...   (REST)
//   /{owner}/{repo}.git/...           (git smart HTTP)
func ownerFromSegments(segments []string) string {
	// REST: api/v3/repos/{owner}/...
	if len(segments) >= 4 && segments[0] == "api" && segments[1] == "v3" && segments[2] == "repos" {
		return segments[3]
	}
	// git: {owner}/{repo}.git/...
	if len(segments) >= 2 && strings.HasSuffix(segments[1], ".git") {
		return segments[0]
	}
	return ""
}

func appSendJSON(w http.ResponseWriter, status int, body *Obj) {
	text := MarshalNL(body)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", itoa(len(text)))
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

func dispatchHandler(w http.ResponseWriter, r *http.Request) {
	tw := &trackingWriter{ResponseWriter: w}
	path := r.URL.EscapedPath()
	method := r.Method

	// The env's boot check calls this twice: 200 {"ok":true}.
	if method == "GET" && path == "/healthz" {
		appSendJSON(tw, 200, O("ok", true))
		return
	}

	// Split the path once: the viewer lookup and the REST handler both
	// need the segments.
	segments := splitNonEmpty(path, "/")

	// Learn the viewer login from the first owner seen in a repo URL.
	if owner := ownerFromSegments(segments); owner != "" {
		storeMu.Lock()
		store.SetViewerLogin(owner)
		storeMu.Unlock()
	}
	viewerLogin := func() string {
		storeMu.RLock()
		defer storeMu.RUnlock()
		return store.ViewerLogin
	}()

	// A handler must never kill the process: an uncaught panic (malformed
	// input, a bug in one surface) would abort the whole trial and zero
	// every check after it. Answer 500 and keep serving.
	defer func() {
		if rec := recover(); rec != nil {
			logError("[app] handler error: %v", rec)
			if !tw.wrote {
				appSendJSON(tw, 500, O("message", "Internal Server Error"))
			}
		}
	}()

	if handleMcp(tw, r, path, method, viewerLogin) {
		return
	}
	if handleGraphql(tw, r, path, method, viewerLogin) {
		return
	}
	if handleRest(tw, r, path, method, viewerLogin, segments) {
		return
	}
	if handleGit(tw, r, path, method) {
		return
	}

	// Everything else, every method: GitHub's 404 body.
	appSendJSON(tw, 404, O("message", "Not Found"))
}
