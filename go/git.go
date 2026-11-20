// git.ts — Git smart HTTP (upload-pack / receive-pack) for the git forge.
//
// Pure Go: crypto/sha1 + compress/zlib only, no git binary. Implements the
// pkt-line codec, packfile v2 read/write (plain objects, plus OFS_DELTA /
// REF_DELTA resolution for packs a client sends), the object model
// (blob/tree/commit/tag), protocol v2 ls-refs/fetch, protocol v0 upload-pack
// advertisement/fetch, and receive-pack with fast-forward enforcement.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const ZeroOID = "0000000000000000000000000000000000000000"
const gitAgent = "makefun-git-client/1.0"
const maxPktSize = 65520
const sidebandChunk = 65515 // 65520 - 4 (length) - 1 (band)

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")
var pktEnd = []byte("0002")

// ---------------------------------------------------------------------------
// pkt-line codec
// ---------------------------------------------------------------------------

type pktKind int

const (
	pktData pktKind = iota
	pktFlushKind
	pktDelimKind
	pktEndKind
)

type pktPacket struct {
	kind pktKind
	data []byte
}

// pktEncode encodes one pkt-line: 4 lowercase-hex length bytes + payload.
func pktEncode(payload []byte) []byte {
	if len(payload)+4 > maxPktSize {
		panic(fmt.Sprintf("pkt-line payload too large: %d", len(payload)))
	}
	header := fmt.Sprintf("%04x", len(payload)+4)
	out := make([]byte, 0, len(payload)+4)
	out = append(out, header...)
	out = append(out, payload...)
	return out
}

func pktText(text string) []byte {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return pktEncode([]byte(text))
}

// pktReader is an incremental pkt-line reader over a buffer.
type pktReader struct {
	buf []byte
	pos int
}

func (r *pktReader) offset() int     { return r.pos }
func (r *pktReader) remaining() int  { return len(r.buf) - r.pos }
func (r *pktReader) read() (*pktPacket, error) {
	if r.pos >= len(r.buf) {
		return nil, nil
	}
	if r.pos+4 > len(r.buf) {
		return nil, errors.New("pkt-line truncated header")
	}
	header := string(r.buf[r.pos : r.pos+4])
	for _, c := range header {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return nil, fmt.Errorf("pkt-line bad length header: %q", header)
		}
	}
	length, _ := strconv.ParseInt(header, 16, 32)
	if length == 0 {
		r.pos += 4
		return &pktPacket{kind: pktFlushKind}, nil
	}
	if length == 1 {
		r.pos += 4
		return &pktPacket{kind: pktDelimKind}, nil
	}
	if length == 2 {
		r.pos += 4
		return &pktPacket{kind: pktEndKind}, nil
	}
	if length < 4 || length > maxPktSize {
		return nil, fmt.Errorf("pkt-line bad length: %d", length)
	}
	if r.pos+int(length) > len(r.buf) {
		return nil, errors.New("pkt-line truncated payload")
	}
	data := make([]byte, length-4)
	copy(data, r.buf[r.pos+4:r.pos+int(length)])
	r.pos += int(length)
	return &pktPacket{kind: pktData, data: data}, nil
}

// pktDecode decodes pkt-lines from buf, stopping after the first
// flush/delim/end packet (or at end of buffer). Returns the packets and the
// offset just past the last packet consumed.
func pktDecode(buf []byte, start int) ([]*pktPacket, int, error) {
	reader := &pktReader{buf: buf, pos: start}
	var packets []*pktPacket
	for {
		packet, err := reader.read()
		if err != nil {
			return nil, 0, err
		}
		if packet == nil {
			break
		}
		packets = append(packets, packet)
		if packet.kind != pktData {
			break
		}
	}
	return packets, reader.offset(), nil
}

func pktLines(packets []*pktPacket) []string {
	var out []string
	for _, p := range packets {
		if p.kind != pktData {
			continue
		}
		text := string(p.data)
		if strings.HasSuffix(text, "\n") {
			text = text[:len(text)-1]
		}
		out = append(out, text)
	}
	return out
}

// ---------------------------------------------------------------------------
// packfile v2 (plain objects; OFS_DELTA / REF_DELTA resolved when they appear)
// ---------------------------------------------------------------------------

var typeCodes = map[GitObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeTypes = map[int]GitObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func encodeObjectHeader(typeCode, size int) []byte {
	var bytes []byte
	rest := size / 16
	first := (size % 16) | (typeCode << 4)
	if rest > 0 {
		first |= 0x80
	}
	bytes = append(bytes, byte(first))
	for rest > 0 {
		b := rest % 128
		rest /= 128
		if rest > 0 {
			b |= 0x80
		}
		bytes = append(bytes, byte(b))
	}
	return bytes
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}

// writePack writes a packfile version 2 with plain (non-delta) objects,
// sorted by sha.
func writePack(objects []GitObject) []byte {
	type entry struct {
		obj GitObject
		sha string
	}
	entries := make([]entry, 0, len(objects))
	for _, obj := range objects {
		entries = append(entries, entry{obj: obj, sha: GitObjectID(obj.Type, obj.Content)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].sha < entries[j].sha })
	var parts bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(entries)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	parts.Write(header)
	for _, e := range entries {
		parts.Write(encodeObjectHeader(typeCodes[e.obj.Type], len(e.obj.Content)))
		parts.Write(deflate(e.obj.Content))
	}
	withoutTrailer := parts.Bytes()
	trailer := sha1.Sum(withoutTrailer)
	return append(append([]byte{}, withoutTrailer...), trailer[:]...)
}

// limitedCountReader counts the bytes pulled from the underlying slice and
// returns at most 64 bytes per Read, so a zlib/flate reader can over-read by
// at most 63 bytes past the end of its stream.
type limitedCountReader struct {
	b []byte
	n int
}

func (r *limitedCountReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > 64 {
		n = 64
	}
	if n > len(r.b) {
		n = len(r.b)
	}
	copy(p, r.b[:n])
	r.b = r.b[n:]
	r.n += n
	return n, nil
}

// inflateOne inflates exactly one zlib stream starting at offset, in a single
// pass. Go's zlib reader ignores trailing bytes, so the stream end is found
// from the Adler-32 checksum the reader verified internally: the stream's
// last 4 bytes equal adler32(data). flate may have pulled up to 63 bytes past
// the end, so the checksum is scanned in that window. One decompression per
// object instead of the ~21 of the old binary-search probe.
func inflateOne(buf []byte, offset int) ([]byte, int, error) {
	if offset >= len(buf) {
		return nil, 0, errors.New("pack entry truncated: no zlib data")
	}
	cr := &limitedCountReader{b: buf[offset:]}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return nil, 0, err
	}
	var sumBytes [4]byte
	binary.BigEndian.PutUint32(sumBytes[:], adler32.Checksum(data))
	hi := offset + cr.n
	lo := hi - 68
	if lo < offset+6 {
		lo = offset + 6
	}
	for e := hi; e >= lo; e-- {
		if e-4 >= offset && bytes.Equal(buf[e-4:e], sumBytes[:]) {
			return data, e, nil
		}
	}
	return nil, 0, errors.New("pack entry: zlib stream end not found")
}

// applyDelta applies a git delta stream to base.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		result := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta truncated varint")
			}
			b := delta[pos]
			pos++
			result += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return result, nil
			}
		}
	}
	sourceSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	targetSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta source size mismatch")
	}
	var chunks [][]byte
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			offset := 0
			size := 0
			if op&0x01 != 0 {
				offset = int(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				offset += int(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				offset += int(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				offset += int(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				size = int(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				size += int(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				size += int(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			chunks = append(chunks, base[offset:offset+size])
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta literal truncated")
			}
			chunks = append(chunks, delta[pos:pos+int(op)])
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	var result []byte
	for _, c := range chunks {
		result = append(result, c...)
	}
	if len(result) != targetSize {
		return nil, errors.New("delta target size mismatch")
	}
	return result, nil
}

type readPackResult struct {
	objects map[string]*GitObject
	version int
	count   int
}

// readPack reads a packfile. Verifies magic, version (2 or 3), per-object
// SHA-1 and size, and the trailer checksum. OFS_DELTA / REF_DELTA entries
// are resolved against objects already parsed from this pack, or against
// `known` (e.g. the repo's objects, for thin packs whose base the server
// already holds).
func readPack(buffer []byte, known map[string]*GitObject) (*readPackResult, error) {
	if len(buffer) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(buffer[0:4]) != "PACK" {
		return nil, errors.New("pack bad magic")
	}
	version := int(buffer[4])<<24 | int(buffer[5])<<16 | int(buffer[6])<<8 | int(buffer[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack unsupported version: %d", version)
	}
	count := int(buffer[8])<<24 | int(buffer[9])<<16 | int(buffer[10])<<8 | int(buffer[11])
	trailer := buffer[len(buffer)-20:]
	expectedTrailer := sha1.Sum(buffer[:len(buffer)-20])
	if !bytes.Equal(trailer, expectedTrailer[:]) {
		return nil, errors.New("pack trailer checksum mismatch")
	}

	objects := make(map[string]*GitObject)
	offsetToSha := make(map[int]string)
	pos := 12
	for i := 0; i < count; i++ {
		entryStart := pos
		if pos >= len(buffer)-20 {
			return nil, errors.New("pack truncated entry header")
		}
		b := buffer[pos]
		pos++
		typeCode := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= len(buffer)-20 {
				return nil, errors.New("pack truncated size varint")
			}
			b = buffer[pos]
			pos++
			size += int(b&0x7f) << shift
			shift += 7
		}
		var base *GitObject
		var baseType GitObjectType
		if typeCode == 6 {
			// OFS_DELTA
			if pos >= len(buffer)-20 {
				return nil, errors.New("pack truncated ofs-delta offset")
			}
			b = buffer[pos]
			pos++
			off := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= len(buffer)-20 {
					return nil, errors.New("pack truncated ofs-delta offset")
				}
				b = buffer[pos]
				pos++
				off = (off+1)*128 + int(b&0x7f)
			}
			baseSha, ok := offsetToSha[entryStart-off]
			if !ok {
				return nil, errors.New("pack ofs-delta base not found")
			}
			base = objects[baseSha]
			if base == nil {
				return nil, errors.New("pack ofs-delta base not found")
			}
			baseType = base.Type
		} else if typeCode == 7 {
			// REF_DELTA
			if pos+20 > len(buffer)-20 {
				return nil, errors.New("pack truncated ref-delta base")
			}
			baseSha := hex.EncodeToString(buffer[pos : pos+20])
			pos += 20
			base = objects[baseSha]
			if base == nil && known != nil {
				base = known[baseSha]
			}
			if base == nil {
				return nil, fmt.Errorf("pack ref-delta base not found: %s", baseSha)
			}
			baseType = base.Type
		} else if _, ok := codeTypes[int(typeCode)]; !ok {
			return nil, fmt.Errorf("pack unknown object type code: %d", typeCode)
		}
		inflated, nextOffset, err := inflateOne(buffer, pos)
		if err != nil {
			return nil, err
		}
		pos = nextOffset
		var typ GitObjectType
		var content []byte
		if base != nil {
			if len(inflated) != size {
				return nil, fmt.Errorf("pack delta size mismatch: header %d, got %d", size, len(inflated))
			}
			typ = baseType
			content, err = applyDelta(base.Content, inflated)
			if err != nil {
				return nil, err
			}
		} else {
			typ = codeTypes[int(typeCode)]
			content = inflated
			if len(content) != size {
				return nil, fmt.Errorf("pack object size mismatch: header %d, got %d", size, len(content))
			}
		}
		sha := GitObjectID(typ, content)
		objects[sha] = &GitObject{Type: typ, Content: content}
		offsetToSha[entryStart] = sha
	}
	if pos != len(buffer)-20 {
		return nil, errors.New("pack trailing bytes before trailer")
	}
	return &readPackResult{objects: objects, version: version, count: count}, nil
}

// ---------------------------------------------------------------------------
// object model
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	Sha  string
}

func parseTree(content []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	pos := 0
	for pos < len(content) {
		space := bytes.IndexByte(content[pos:], 0x20)
		if space == -1 {
			return nil, errors.New("tree entry missing mode")
		}
		space += pos
		nul := bytes.IndexByte(content[space+1:], 0x00)
		if nul == -1 {
			return nil, errors.New("tree entry truncated")
		}
		nul += space + 1
		if nul+21 > len(content) {
			return nil, errors.New("tree entry truncated")
		}
		mode := string(content[pos:space])
		name := string(content[space+1 : nul])
		sha := hex.EncodeToString(content[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, Sha: sha})
		pos = nul + 21
	}
	return entries, nil
}

// treeSortKey: git tree sort order — directory names compare as if suffixed
// with '/'.
func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return treeSortKey(sorted[i]) < treeSortKey(sorted[j]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.Sha)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func splitHeaders(content []byte) (headers [][2]string, message string) {
	text := string(content)
	blank := strings.Index(text, "\n\n")
	headerText := text
	if blank != -1 {
		headerText = text[:blank]
		message = text[blank+2:]
	} else {
		message = ""
	}
	for _, line := range strings.Split(headerText, "\n") {
		if strings.HasPrefix(line, " ") {
			continue // continuation of a multi-line header
		}
		space := strings.Index(line, " ")
		if space == -1 {
			continue
		}
		headers = append(headers, [2]string{line[:space], line[space+1:]})
	}
	return headers, message
}

func parseCommit(content []byte) CommitInfo {
	headers, message := splitHeaders(content)
	var info CommitInfo
	info.Message = message
	for _, h := range headers {
		switch h[0] {
		case "tree":
			info.Tree = h[1]
		case "parent":
			info.Parents = append(info.Parents, h[1])
		case "author":
			info.Author = h[1]
		case "committer":
			info.Committer = h[1]
		}
	}
	return info
}

func buildCommit(commit CommitInfo) []byte {
	lines := []string{"tree " + commit.Tree}
	for _, p := range commit.Parents {
		lines = append(lines, "parent "+p)
	}
	if commit.Author != "" {
		lines = append(lines, "author "+commit.Author)
	}
	if commit.Committer != "" {
		lines = append(lines, "committer "+commit.Committer)
	}
	message := commit.Message
	if !(strings.HasSuffix(message, "\n") || message == "") {
		message += "\n"
	}
	return []byte(strings.Join(lines, "\n") + "\n\n" + message)
}

type TagInfo struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(content []byte) TagInfo {
	headers, message := splitHeaders(content)
	var info TagInfo
	info.Message = message
	for _, h := range headers {
		switch h[0] {
		case "object":
			info.Object = h[1]
		case "type":
			info.Type = h[1]
		case "tag":
			info.Tag = h[1]
		case "tagger":
			info.Tagger = h[1]
		}
	}
	return info
}

func buildTag(tag TagInfo) []byte {
	lines := []string{"object " + tag.Object, "type " + tag.Type, "tag " + tag.Tag}
	if tag.Tagger != "" {
		lines = append(lines, "tagger "+tag.Tagger)
	}
	message := tag.Message
	if !(strings.HasSuffix(message, "\n") || message == "") {
		message += "\n"
	}
	return []byte(strings.Join(lines, "\n") + "\n\n" + message)
}

// peelToCommit peels a tag object to the commit it points at (directly, or
// via nested tags).
func peelToCommit(objects map[string]*GitObject, sha string) string {
	current := sha
	seen := map[string]bool{}
	for {
		if seen[current] {
			return ""
		}
		seen[current] = true
		obj := objects[current]
		if obj == nil {
			return ""
		}
		if obj.Type == ObjCommit {
			return current
		}
		if obj.Type == ObjTag {
			current = parseTag(obj.Content).Object
			continue
		}
		return ""
	}
}

type reachableResult struct {
	objects map[string]*GitObject
	depths  map[string]int
}

type collectOptions struct {
	maxDepth *int
	exclude  map[string]bool
	cutoff   map[string]bool
}

func addTree(objects map[string]*GitObject, out map[string]*GitObject, treeSha string, exclude map[string]bool) {
	stack := []string{treeSha}
	seen := map[string]bool{}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[sha] || exclude[sha] {
			continue
		}
		seen[sha] = true
		obj := objects[sha]
		if obj == nil || obj.Type != ObjTree {
			if obj != nil {
				out[sha] = obj
			}
			continue
		}
		out[sha] = obj
		entries, err := parseTree(obj.Content)
		if err != nil {
			continue
		}
		for _, e := range entries {
			stack = append(stack, e.Sha)
		}
	}
}

// collectReachableDetailed: all objects reachable from tips (commit -> tree
// + parents, tree -> entries, tag -> target). Returns the objects plus
// per-commit depths for shallow math.
func collectReachableDetailed(objects map[string]*GitObject, tips []string, opts collectOptions) reachableResult {
	out := make(map[string]*GitObject)
	depths := make(map[string]int)
	exclude := opts.exclude
	if exclude == nil {
		exclude = map[string]bool{}
	}
	cutoff := opts.cutoff
	if cutoff == nil {
		cutoff = map[string]bool{}
	}
	type item struct {
		sha   string
		depth int
	}
	queue := []item{}
	for _, sha := range tips {
		queue = append(queue, item{sha: sha, depth: 0})
	}
	seen := map[string]bool{}
	head := 0
	for head < len(queue) {
		cur := queue[head]
		head++
		sha, depth := cur.sha, cur.depth
		if seen[sha] {
			continue
		}
		seen[sha] = true
		obj := objects[sha]
		if obj == nil {
			continue
		}
		excluded := exclude[sha]
		if obj.Type == ObjTag {
			if !excluded {
				out[sha] = obj
			}
			queue = append(queue, item{sha: parseTag(obj.Content).Object, depth: depth})
			continue
		}
		if obj.Type != ObjCommit {
			if !excluded {
				out[sha] = obj
			}
			continue
		}
		if !excluded {
			out[sha] = obj
			if _, ok := depths[sha]; !ok {
				depths[sha] = depth
			}
		}
		commit := parseCommit(obj.Content)
		if !excluded {
			addTree(objects, out, commit.Tree, exclude)
		}
		if excluded != cutoff[sha] {
			continue
		}
		if opts.maxDepth == nil || depth < *opts.maxDepth {
			for _, parent := range commit.Parents {
				queue = append(queue, item{sha: parent, depth: depth + 1})
			}
		}
	}
	return reachableResult{objects: out, depths: depths}
}

func collectReachable(objects map[string]*GitObject, tips []string, opts collectOptions) map[string]*GitObject {
	return collectReachableDetailed(objects, tips, opts).objects
}

// isAncestor is true when ancestorSha is reachable from descendantSha (or
// equal to it).
func isAncestor(objects map[string]*GitObject, ancestorSha, descendantSha string) bool {
	if ancestorSha == descendantSha {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendantSha}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if sha == ancestorSha {
			return true
		}
		if seen[sha] {
			continue
		}
		seen[sha] = true
		obj := objects[sha]
		if obj == nil {
			continue
		}
		if obj.Type == ObjTag {
			stack = append(stack, parseTag(obj.Content).Object)
			continue
		}
		if obj.Type == ObjCommit {
			for _, p := range parseCommit(obj.Content).Parents {
				stack = append(stack, p)
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// HTTP plumbing (git surface)
// ---------------------------------------------------------------------------

func readBodyGit(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

func sendBufferGit(w http.ResponseWriter, status int, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", itoa(len(body)))
	h.Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	h.Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func sendTextGit(w http.ResponseWriter, status int, text string) {
	sendBufferGit(w, status, "text/plain; charset=utf-8", []byte(text))
}

func headerValueGit(r *http.Request, name string) string {
	vals, ok := r.Header[http.CanonicalHeaderKey(name)]
	if !ok || len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// authorized: git requests carry `Authorization: Bearer <token>`. The token
// is the shop token (MAKEFUN_SHOP_TOKEN); when the env does not set one, any
// non-empty Bearer token is accepted. Missing or wrong token -> 401.
func gitAuthorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	m := gitBearerRe.FindStringSubmatch(header)
	if m == nil {
		return false
	}
	expected := shopToken()
	if expected != "" {
		return m[1] == expected
	}
	return strings.TrimSpace(m[1]) != ""
}

var gitBearerRe = regexp.MustCompile(`^Bearer\s+(.+)$`)

type gitRoute struct {
	owner    string
	repoName string
	suffix   string
}

var gitRouteRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git(/.*)?$`)

func matchGitRoute(pathname string) *gitRoute {
	clean := strings.SplitN(pathname, "?", 2)[0]
	m := gitRouteRe.FindStringSubmatch(clean)
	if m == nil {
		return nil
	}
	owner, err1 := urlPathUnescape(m[1])
	repoName, err2 := urlPathUnescape(m[2])
	if err1 != nil || err2 != nil {
		return nil
	}
	suffix := m[3]
	return &gitRoute{owner: owner, repoName: repoName, suffix: suffix}
}

type refLine struct {
	name   string
	oid    string
	peeled string // "" when none
}

func listRefs(repo *Repo) []refLine {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []refLine
	for _, name := range names {
		oid := repo.Refs[name]
		line := refLine{name: name, oid: oid}
		if obj := repo.Objects[oid]; obj != nil && obj.Type == ObjTag {
			if peeled := peelToCommit(repo.Objects, oid); peeled != "" {
				line.peeled = peeled
			}
		}
		out = append(out, line)
	}
	return out
}

func headRefName(repo *Repo) string {
	return "refs/heads/" + repo.DefaultBranch
}

func advertiseV0(repo *Repo, service string) []byte {
	symref := "symref=HEAD:" + headRefName(repo)
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k multi_ack multi_ack_detailed thin-pack ofs-delta agent=" + gitAgent + " " + symref
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=" + gitAgent + " " + symref
	}
	var parts [][]byte
	parts = append(parts, pktText("# service="+service), pktFlush)
	refs := listRefs(repo)
	if len(refs) == 0 {
		parts = append(parts, pktText(ZeroOID+" capabilities^{}\x00"+caps))
	} else {
		for i, ref := range refs {
			line := ref.oid + " " + ref.name
			if i == 0 {
				line += "\x00" + caps
			}
			parts = append(parts, pktText(line))
			if ref.peeled != "" {
				parts = append(parts, pktText(ref.peeled+" "+ref.name+"^{}"))
			}
		}
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

func advertiseV2() []byte {
	return bytes.Join([][]byte{
		pktText("version 2"),
		pktText("ls-refs=unborn"),
		pktText("fetch=shallow wait-for-done"),
		pktText("object-format=sha1"),
		pktText("agent=" + gitAgent),
		pktFlush,
	}, nil)
}

// sidebandPackets wraps bytes in sideband band-1 pkt-lines.
func sidebandPackets(data []byte) []byte {
	var parts [][]byte
	for off := 0; off < len(data); off += sidebandChunk {
		end := off + sidebandChunk
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]
		band := make([]byte, 0, len(chunk)+1)
		band = append(band, 0x01)
		band = append(band, chunk...)
		parts = append(parts, pktEncode(band))
	}
	return bytes.Join(parts, nil)
}

type fetchSelection struct {
	packObjects map[string]*GitObject
	shallow     []string
	unshallow   []string
}

func selectFetchObjects(repo *Repo, wants, haves []string, maxDepth *int, shallowBoundary map[string]bool) fetchSelection {
	haveReachable := collectReachable(repo.Objects, haves, collectOptions{cutoff: shallowBoundary})
	exclude := make(map[string]bool, len(haveReachable))
	for sha := range haveReachable {
		exclude[sha] = true
	}
	wanted := collectReachableDetailed(repo.Objects, wants, collectOptions{maxDepth: maxDepth, exclude: exclude, cutoff: shallowBoundary})
	var shallow, unshallow []string
	if maxDepth != nil {
		full := collectReachableDetailed(repo.Objects, wants, collectOptions{maxDepth: maxDepth})
		boundary := map[string]bool{}
		for sha, depth := range full.depths {
			if depth == *maxDepth {
				boundary[sha] = true
			}
		}
		for sha := range boundary {
			shallow = append(shallow, sha)
		}
		sort.Strings(shallow)
		unshallowSet := map[string]bool{}
		for sha := range shallowBoundary {
			depth, ok := full.depths[sha]
			if ok && depth < *maxDepth && !boundary[sha] {
				unshallowSet[sha] = true
			}
		}
		for sha := range unshallowSet {
			unshallow = append(unshallow, sha)
		}
		sort.Strings(unshallow)
	}
	return fetchSelection{packObjects: wanted.objects, shallow: shallow, unshallow: unshallow}
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

func handleInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo) {
	service := r.URL.Query().Get("service")
	if service == "git-upload-pack" {
		if strings.TrimSpace(headerValueGit(r, "git-protocol")) == "version=2" {
			sendBufferGit(w, 200, "application/x-git-upload-pack-advertisement", advertiseV2())
		} else {
			sendBufferGit(w, 200, "application/x-git-upload-pack-advertisement", advertiseV0(repo, "git-upload-pack"))
		}
		return
	}
	if service == "git-receive-pack" {
		sendBufferGit(w, 200, "application/x-git-receive-pack-advertisement", advertiseV0(repo, "git-receive-pack"))
		return
	}
	sendTextGit(w, 404, "Not Found")
}

func handleLsRefsV2(w http.ResponseWriter, repo *Repo, args []string) {
	wantPeel := false
	wantSymrefs := false
	wantUnborn := false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			wantPeel = true
		case a == "symrefs":
			wantSymrefs = true
		case a == "unborn":
			wantUnborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var lines []string
	for _, ref := range listRefs(repo) {
		if !matches(ref.name) {
			continue
		}
		line := ref.oid + " " + ref.name
		if wantPeel && ref.peeled != "" {
			line += " peeled:" + ref.peeled
		}
		lines = append(lines, line)
	}
	headName := headRefName(repo)
	headOid, headExists := repo.Refs[headName]
	if headExists {
		if wantSymrefs {
			ok := len(prefixes) == 0
			for _, p := range prefixes {
				if strings.HasPrefix("HEAD", p) || strings.HasPrefix(p, "HEAD") {
					ok = true
				}
			}
			if ok {
				lines = append(lines, headOid+" HEAD symref-target:"+headName)
			}
		}
	} else if wantUnborn && matches(headName) {
		lines = append(lines, "unborn HEAD symref-target:"+headName)
	}
	var parts [][]byte
	for _, line := range lines {
		parts = append(parts, pktText(line))
	}
	parts = append(parts, pktFlush)
	sendBufferGit(w, 200, "application/x-git-upload-pack-result", bytes.Join(parts, nil))
}

func handleFetchV2(w http.ResponseWriter, repo *Repo, args []string) {
	var wants, haves, shallowLines []string
	var deepen *int
	deepenRelative := false
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			wants = append(wants, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "shallow "):
			v := strings.TrimSpace(arg[8:])
			shallowLines = append(shallowLines, v)
			haves = append(haves, v)
		case strings.HasPrefix(arg, "deepen "):
			n, err := strconv.Atoi(strings.TrimSpace(arg[7:]))
			if err == nil && n > 0 {
				deepen = &n
			}
		case arg == "deepen-relative":
			deepenRelative = true
		}
	}
	var maxDepth *int
	if deepen != nil {
		if deepenRelative && len(shallowLines) > 0 {
			depths := collectReachableDetailed(repo.Objects, wants, collectOptions{}).depths
			var boundaryDepth *int
			for _, sha := range shallowLines {
				if d, ok := depths[sha]; ok && (boundaryDepth == nil || d < *boundaryDepth) {
					d := d
					boundaryDepth = &d
				}
			}
			if boundaryDepth == nil {
				v := *deepen - 1
				maxDepth = &v
			} else {
				v := *boundaryDepth + *deepen
				maxDepth = &v
			}
		} else {
			v := *deepen - 1
			maxDepth = &v
		}
	}
	selection := selectFetchObjects(repo, wants, haves, maxDepth, setOf(shallowLines))
	var objs []GitObject
	for _, obj := range selection.packObjects {
		objs = append(objs, *obj)
	}
	pack := writePack(objs)
	var parts [][]byte
	if len(haves) > 0 {
		parts = append(parts, pktText("acknowledgments"))
		parts = append(parts, pktText("NAK"))
		parts = append(parts, pktText("ready"))
		parts = append(parts, pktDelim)
	}
	if maxDepth != nil {
		parts = append(parts, pktText("shallow-info"))
		for _, sha := range selection.shallow {
			parts = append(parts, pktText("shallow "+sha))
		}
		for _, sha := range selection.unshallow {
			parts = append(parts, pktText("unshallow "+sha))
		}
		parts = append(parts, pktDelim)
	}
	parts = append(parts, pktText("packfile"))
	parts = append(parts, sidebandPackets(pack))
	parts = append(parts, pktFlush)
	sendBufferGit(w, 200, "application/x-git-upload-pack-result", bytes.Join(parts, nil))
}

func setOf(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, s := range items {
		out[s] = true
	}
	return out
}

// parseV2Request splits a v2 request body into the command section (before
// the delim) and the argument lines (after it). Tolerates a missing delim.
func parseV2Request(body []byte) (string, []string, error) {
	reader := &pktReader{buf: body}
	var commandLines, argLines []string
	inArgs := false
	for {
		packet, err := reader.read()
		if err != nil {
			return "", nil, err
		}
		if packet == nil {
			break
		}
		if packet.kind == pktFlushKind {
			break
		}
		if packet.kind == pktDelimKind {
			inArgs = true
			continue
		}
		if packet.kind != pktData {
			continue
		}
		text := string(packet.data)
		if strings.HasSuffix(text, "\n") {
			text = text[:len(text)-1]
		}
		if !inArgs && len(commandLines) == 0 && strings.HasPrefix(text, "command=") {
			commandLines = append(commandLines, text)
		} else if inArgs {
			argLines = append(argLines, text)
		} else if !inArgs && len(commandLines) > 0 {
			argLines = append(argLines, text)
		}
	}
	command := ""
	if len(commandLines) > 0 {
		command = strings.TrimSpace(strings.TrimPrefix(commandLines[0], "command="))
	}
	return command, argLines, nil
}

func handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) error {
	body, err := readBodyGit(r)
	if err != nil {
		return err
	}
	if strings.TrimSpace(headerValueGit(r, "git-protocol")) == "version=2" {
		command, args, err := parseV2Request(body)
		if err != nil {
			return err
		}
		if command == "ls-refs" {
			handleLsRefsV2(w, repo, args)
			return nil
		}
		if command == "fetch" {
			handleFetchV2(w, repo, args)
			return nil
		}
		sendTextGit(w, 400, "unsupported command: "+command)
		return nil
	}

	// Protocol v0 fetch: want lines, then a flush, then have lines + `done`.
	var wants, haves []string
	var deepen *int
	reader := &pktReader{buf: body}
	for {
		packet, err := reader.read()
		if err != nil {
			return err
		}
		if packet == nil {
			break
		}
		if packet.kind != pktData {
			continue
		}
		text := string(packet.data)
		if strings.HasSuffix(text, "\n") {
			text = text[:len(text)-1]
		}
		switch {
		case strings.HasPrefix(text, "want "):
			oid := strings.Fields(strings.TrimSpace(text[5:]))
			if len(oid) > 0 && oid[0] != "" {
				wants = append(wants, oid[0])
			}
		case strings.HasPrefix(text, "have "):
			oid := strings.Fields(strings.TrimSpace(text[5:]))
			if len(oid) > 0 && oid[0] != "" {
				haves = append(haves, oid[0])
			}
		case strings.HasPrefix(text, "deepen "):
			n, err := strconv.Atoi(strings.TrimSpace(text[7:]))
			if err == nil && n > 0 {
				deepen = &n
			}
		}
	}
	var maxDepth *int
	if deepen != nil {
		v := *deepen - 1
		maxDepth = &v
	}
	selection := selectFetchObjects(repo, wants, haves, maxDepth, nil)
	var objs []GitObject
	for _, obj := range selection.packObjects {
		objs = append(objs, *obj)
	}
	pack := writePack(objs)
	var parts [][]byte
	if maxDepth != nil {
		for _, sha := range selection.shallow {
			parts = append(parts, pktText("shallow "+sha))
		}
		for _, sha := range selection.unshallow {
			parts = append(parts, pktText("unshallow "+sha))
		}
	}
	parts = append(parts, pktText("NAK"))
	parts = append(parts, sidebandPackets(pack))
	parts = append(parts, pktFlush)
	sendBufferGit(w, 200, "application/x-git-upload-pack-result", bytes.Join(parts, nil))
	return nil
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

type pushResult struct {
	ref    string
	ok     bool
	reason string
}

var pushCmdRe = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$`)

func handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) error {
	body, err := readBodyGit(r)
	if err != nil {
		return err
	}
	reader := &pktReader{buf: body}
	var commands []pushCommand
	capabilities := ""
	first := true
	for {
		packet, err := reader.read()
		if err != nil {
			return err
		}
		if packet == nil {
			break
		}
		if packet.kind == pktFlushKind {
			break
		}
		if packet.kind != pktData {
			continue
		}
		text := string(packet.data)
		if strings.HasSuffix(text, "\n") {
			text = text[:len(text)-1]
		}
		if first {
			first = false
			if nul := strings.Index(text, "\x00"); nul != -1 {
				capabilities = text[nul+1:]
				text = text[:nul]
			}
		}
		if m := pushCmdRe.FindStringSubmatch(text); m != nil {
			commands = append(commands, pushCommand{oldOid: m[1], newOid: m[2], ref: m[3]})
		}
	}
	packBytes := body[reader.offset():]

	unpackOK := true
	unpackDetail := ""
	if len(packBytes) > 0 {
		parsed, err := readPack(packBytes, repo.Objects)
		if err != nil {
			unpackOK = false
			unpackDetail = strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " ")
			if len(unpackDetail) > 200 {
				unpackDetail = unpackDetail[:200]
			}
		} else {
			for sha, obj := range parsed.objects {
				repo.Objects[sha] = obj
			}
		}
	}

	var results []pushResult
	for _, cmd := range commands {
		if !unpackOK {
			results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "unpack failed"})
			continue
		}
		current, exists := repo.Refs[cmd.ref]
		if cmd.newOid == ZeroOID {
			// Delete.
			if !exists {
				results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "remote ref does not exist"})
			} else {
				delete(repo.Refs, cmd.ref)
				results = append(results, pushResult{ref: cmd.ref, ok: true})
			}
			continue
		}
		if cmd.oldOid == ZeroOID {
			// Create.
			if exists {
				results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "already exists"})
				continue
			}
			if _, ok := repo.Objects[cmd.newOid]; !ok {
				results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "object not found"})
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOid
			results = append(results, pushResult{ref: cmd.ref, ok: true})
			continue
		}
		// Update: old must be the current tip and new must descend from it.
		if !exists || current != cmd.oldOid {
			results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			continue
		}
		if _, ok := repo.Objects[cmd.newOid]; !ok {
			results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "object not found"})
			continue
		}
		oldCommit := peelToCommit(repo.Objects, cmd.oldOid)
		newCommit := peelToCommit(repo.Objects, cmd.newOid)
		if oldCommit != "" && newCommit != "" && !isAncestor(repo.Objects, oldCommit, newCommit) {
			results = append(results, pushResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			continue
		}
		repo.Refs[cmd.ref] = cmd.newOid
		results = append(results, pushResult{ref: cmd.ref, ok: true})
	}

	var lines []string
	if unpackOK {
		lines = append(lines, "unpack ok")
	} else {
		lines = append(lines, "unpack "+unpackDetail)
	}
	for _, result := range results {
		if result.ok {
			lines = append(lines, "ok "+result.ref)
		} else {
			reason := result.reason
			if reason == "" {
				reason = "failed"
			}
			lines = append(lines, "ng "+result.ref+" "+reason)
		}
	}
	sideBand := false
	for _, c := range strings.Split(capabilities, " ") {
		if c == "side-band-64k" {
			sideBand = true
		}
	}
	var response []byte
	if sideBand {
		var inner [][]byte
		for _, line := range lines {
			inner = append(inner, pktText(line))
		}
		inner = append(inner, pktFlush)
		response = bytes.Join([][]byte{sidebandPackets(bytes.Join(inner, nil)), pktFlush, pktFlush}, nil)
	} else {
		var parts [][]byte
		for _, line := range lines {
			parts = append(parts, pktText(line))
		}
		parts = append(parts, pktFlush, pktFlush)
		response = bytes.Join(parts, nil)
	}
	sendBufferGit(w, 200, "application/x-git-receive-pack-result", response)
	return nil
}

// ---------------------------------------------------------------------------
// entry point
// ---------------------------------------------------------------------------

// handleGit handles one git smart-HTTP request. Returns true when the request
// was a `/{owner}/{repo}.git/...` route (handled, even as 401/404), false
// otherwise so the caller can fall through to the next handler.
// Takes the store lock itself: read lock for info/refs + upload-pack, write
// lock for receive-pack.
func handleGit(w http.ResponseWriter, r *http.Request, pathname, method string) bool {
	route := matchGitRoute(pathname)
	if route == nil {
		return false
	}
	if route.suffix != "/info/refs" && route.suffix != "/git-upload-pack" && route.suffix != "/git-receive-pack" {
		return false
	}
	if route.suffix == "/git-receive-pack" {
		storeMu.Lock()
		defer storeMu.Unlock()
	} else {
		storeMu.RLock()
		defer storeMu.RUnlock()
	}
	// Malformed input must not kill the process: answer 400 and keep serving.
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				logError("[git] malformed request: %v", rec)
				if !wroteHeader(w) {
					sendTextGit(w, 400, "Bad Request")
				}
			}
		}()
		if err := handleGitInner(w, r, method, route); err != nil {
			logError("[git] malformed request: %v", err)
			if !wroteHeader(w) {
				sendTextGit(w, 400, "Bad Request")
			}
		}
	}()
	return true
}

func handleGitInner(w http.ResponseWriter, r *http.Request, method string, route *gitRoute) error {
	if !gitAuthorized(r) {
		sendTextGit(w, 401, "Unauthorized")
		return nil
	}
	repo := store.GetRepo(route.owner, route.repoName)
	if repo == nil {
		sendTextGit(w, 404, "Not Found")
		return nil
	}
	if method == "GET" && route.suffix == "/info/refs" {
		handleInfoRefs(w, r, repo)
		return nil
	}
	if method == "POST" && route.suffix == "/git-upload-pack" {
		return handleUploadPack(w, r, repo)
	}
	if method == "POST" && route.suffix == "/git-receive-pack" {
		return handleReceivePack(w, r, repo)
	}
	sendTextGit(w, 405, "Method Not Allowed")
	return nil
}
