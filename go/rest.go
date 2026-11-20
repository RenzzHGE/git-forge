// REST v3 surface for the git forge (github-core-clone-01).
//
// Owns every route under /api/v3. All state lives in the shared store
// (store.go), so the git smart-HTTP, GraphQL and MCP surfaces see exactly
// the same repos, objects, issues and pulls this handler creates.
//
// Invariants (mirroring rest.ts):
//   - Every response is application/json and carries a non-empty
//     X-GitHub-Request-Id header (req-<counter>, unique per request).
//   - Every error body is { message: <non-empty string> }.
//   - Every /api/v3 route requires `Authorization: Bearer <token>` where the
//     token is MAKEFUN_SHOP_TOKEN (read at request time). Missing/wrong
//     token -> 401 { message }.
//   - A repo belongs to the login the token names (ctx.viewerLogin); the same
//     repo name under another owner -> 404, never 403.
//   - Deterministic: commit epochs derive from store.tick(), merged_at is a
//     fixed ISO string, ids derive from counters. No wall clock, no random.

package main

import (
	"encoding/base64"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const restJSONType = "application/json; charset=utf-8"
const restMergedAt = "2025-01-01T00:00:00.000Z"
const commitEpochBase = 1700000000

var restRequestCounter int64

func nextRequestID() string {
	n := atomic.AddInt64(&restRequestCounter, 1)
	return "req-" + itoa(int(n))
}

func restSendJSON(w http.ResponseWriter, status int, body any, extraHeaders map[string]string) {
	text := append(Marshal(body), '\n')
	h := w.Header()
	h.Set("Content-Type", restJSONType)
	h.Set("Content-Length", itoa(len(text)))
	h.Set("X-GitHub-Request-Id", nextRequestID())
	for k, v := range extraHeaders {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

func restSendNoContent(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", restJSONType)
	h.Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(204)
}

func restSendError(w http.ResponseWriter, status int, message string) {
	if message == "" {
		message = "Error"
	}
	restSendJSON(w, status, O("message", message), nil)
}

// readJSONBody reads the request body. Empty body -> {}. Malformed JSON or a
// non-object JSON value -> not ok.
func readJSONBody(r *http.Request) (map[string]any, bool) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, false
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return map[string]any{}, true
	}
	v, err := parseJSONValue([]byte(text))
	if err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return m, true
}

func originOf(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return "http://" + host
}

// jsParseInt mirrors JavaScript's parseInt(s, 10): leading whitespace, an
// optional sign, then leading digits. No digits -> NaN.
func jsParseInt(s string) (int, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\v' || s[i] == '\f') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	n := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

func intParam(q queryParams, name string, fallback int) int {
	raw, ok := q.get(name)
	if !ok {
		return fallback
	}
	n, valid := jsParseInt(raw)
	if !valid || n < 1 {
		return fallback
	}
	return n
}

// paginate returns the page slice and a Link header value ("" when no next page).
func paginate[T any](items []T, q queryParams, r *http.Request) ([]T, string) {
	perPage := intParam(q, "per_page", 30)
	if perPage > 100 {
		perPage = 100
	}
	pageNum := intParam(q, "page", 1)
	start := (pageNum - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	page := items[start:end]
	link := ""
	if start+perPage < len(items) {
		params := parseQueryOrdered(r.URL.RawQuery)
		params = querySet(params, "per_page", itoa(perPage))
		params = querySet(params, "page", itoa(pageNum+1))
		u := originOf(r) + r.URL.EscapedPath() + "?" + queryString(params)
		link = "<" + u + ">; rel=\"next\""
	}
	return page, link
}

func withLink(link string) map[string]string {
	if link == "" {
		return nil
	}
	return map[string]string{"link": link}
}

// ---------------------------------------------------------------------------
// Git object helpers
// ---------------------------------------------------------------------------

type restTreeEntry struct {
	mode string
	name string
	sha  string
}

func restTreeSortKey(e restTreeEntry) string {
	if e.mode == "40000" {
		return e.name + "/"
	}
	return e.name
}

// parseTreeRest parses raw tree content; unlike git.go's strict version it
// stops silently at a malformed entry (mirrors rest.ts).
func parseTreeRest(content []byte) []restTreeEntry {
	var entries []restTreeEntry
	i := 0
	for i < len(content) {
		space := indexByte(content, 0x20, i)
		if space < 0 {
			break
		}
		nul := indexByte(content, 0x00, space+1)
		if nul < 0 || nul+21 > len(content) {
			break
		}
		mode := string(content[i:space])
		name := string(content[space+1 : nul])
		sha := hexEncode(content[nul+1 : nul+21])
		entries = append(entries, restTreeEntry{mode: mode, name: name, sha: sha})
		i = nul + 21
	}
	return entries
}

func indexByte(b []byte, c byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func hexEncode(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexd[c>>4], hexd[c&0x0f])
	}
	return string(out)
}

func serializeTreeRest(entries []restTreeEntry) []byte {
	sorted := make([]restTreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return restTreeSortKey(sorted[i]) < restTreeSortKey(sorted[j]) })
	var buf []byte
	for _, e := range sorted {
		buf = append(buf, []byte(e.mode+" "+e.name+"\x00")...)
		raw, _ := hexDecode(e.sha)
		buf = append(buf, raw...)
	}
	return buf
}

func hexDecode(s string) ([]byte, bool) {
	if len(s)%2 != 0 {
		return nil, false
	}
	out := make([]byte, 0, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		hi, ok1 := hexVal(s[i])
		lo, ok2 := hexVal(s[i+1])
		if !ok1 || !ok2 {
			return nil, false
		}
		out = append(out, hi<<4|lo)
	}
	return out, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func storeObjectRest(repo *Repo, typ GitObjectType, content []byte) string {
	sha := GitObjectID(typ, content)
	repo.Objects[sha] = &GitObject{Type: typ, Content: content}
	return sha
}

func getTreeRest(repo *Repo, treeSha string) []restTreeEntry {
	if treeSha == "" {
		return nil
	}
	obj := repo.Objects[treeSha]
	if obj == nil || obj.Type != ObjTree {
		return nil
	}
	return parseTreeRest(obj.Content)
}

// buildTreeRest returns the new tree sha after setting path -> blobSha in
// the tree rooted at parentTreeSha ("" = empty tree), creating intermediate
// trees as needed. blobSha "" removes the path instead.
func buildTreeRest(repo *Repo, parentTreeSha, path, blobSha string) string {
	segments := splitNonEmpty(path, "/")
	var setIn func(treeSha string, idx int) string
	setIn = func(treeSha string, idx int) string {
		entries := getTreeRest(repo, treeSha)
		name := segments[idx]
		last := idx == len(segments)-1
		existing := -1
		for i, e := range entries {
			if e.name == name {
				existing = i
				break
			}
		}
		if last {
			if blobSha == "" {
				if existing >= 0 {
					entries = append(entries[:existing], entries[existing+1:]...)
				}
			} else if existing >= 0 {
				entries[existing] = restTreeEntry{mode: "100644", name: name, sha: blobSha}
			} else {
				entries = append(entries, restTreeEntry{mode: "100644", name: name, sha: blobSha})
			}
		} else {
			var childSha string
			if existing >= 0 && entries[existing].mode == "40000" {
				childSha = entries[existing].sha
			}
			newChildSha := setIn(childSha, idx+1)
			if existing >= 0 {
				entries[existing] = restTreeEntry{mode: "40000", name: name, sha: newChildSha}
			} else {
				entries = append(entries, restTreeEntry{mode: "40000", name: name, sha: newChildSha})
			}
		}
		return storeObjectRest(repo, ObjTree, serializeTreeRest(entries))
	}
	return setIn(parentTreeSha, 0)
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// commitObjectRest builds a commit object; returns { sha, content }. Epoch
// is deterministic.
func commitObjectRest(treeSha, parentSha, message, authorLogin string, epoch int) (string, []byte) {
	ident := authorLogin + " <" + authorLogin + "@example.invalid> " + itoa(epoch) + " +0000"
	lines := []string{"tree " + treeSha}
	if parentSha != "" {
		lines = append(lines, "parent "+parentSha)
	}
	lines = append(lines, "author "+ident, "committer "+ident, "")
	if strings.HasSuffix(message, "\n") {
		lines = append(lines, message)
	} else {
		lines = append(lines, message+"\n")
	}
	content := []byte(strings.Join(lines, "\n"))
	return GitObjectID(ObjCommit, content), content
}

type parsedCommitRest struct {
	tree        string
	parents     []string
	authorName  string
	authorEmail string
	authorEpoch int
	message     string
}

var restAuthorRe = regexp.MustCompile(`^author (.+) <([^>]*)> (\d+) [+-]\d{4}$`)

func parseCommitRest(content []byte) parsedCommitRest {
	text := string(content)
	headEnd := strings.Index(text, "\n\n")
	head := text
	message := ""
	if headEnd >= 0 {
		head = text[:headEnd]
		message = text[headEnd+2:]
	}
	message = strings.TrimRight(message, "\n")
	var out parsedCommitRest
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			out.tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			out.parents = append(out.parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			if m := restAuthorRe.FindStringSubmatch(line); m != nil {
				out.authorName = m[1]
				out.authorEmail = m[2]
				out.authorEpoch, _ = strconv.Atoi(m[3])
			}
		}
	}
	out.message = message
	return out
}

// getFileAtTreeRest returns the blob sha for path in the tree rooted at
// treeSha, or "".
func getFileAtTreeRest(repo *Repo, treeSha, path string) string {
	if treeSha == "" {
		return ""
	}
	segments := splitNonEmpty(path, "/")
	current := treeSha
	for i, seg := range segments {
		if current == "" {
			return ""
		}
		entries := getTreeRest(repo, current)
		var entry *restTreeEntry
		for j := range entries {
			if entries[j].name == seg {
				entry = &entries[j]
				break
			}
		}
		if entry == nil {
			return ""
		}
		if i == len(segments)-1 {
			if entry.mode == "100644" {
				return entry.sha
			}
			return ""
		}
		if entry.mode != "40000" {
			return ""
		}
		current = entry.sha
	}
	return ""
}

type dirEntryRest struct {
	name string
	path string
	sha  string
	typ  string // file | dir
}

// listDirAtTreeRest returns immediate children of path ("" = root).
func listDirAtTreeRest(repo *Repo, treeSha, path string) []dirEntryRest {
	if treeSha == "" {
		return nil
	}
	segments := splitNonEmpty(path, "/")
	current := treeSha
	for _, seg := range segments {
		if current == "" {
			return nil
		}
		entries := getTreeRest(repo, current)
		var entry *restTreeEntry
		for j := range entries {
			if entries[j].name == seg {
				entry = &entries[j]
				break
			}
		}
		if entry == nil || entry.mode != "40000" {
			return nil
		}
		current = entry.sha
	}
	if current == "" {
		return nil
	}
	var out []dirEntryRest
	for _, e := range getTreeRest(repo, current) {
		p := e.name
		if len(segments) > 0 {
			p = strings.Join(segments, "/") + "/" + e.name
		}
		typ := "file"
		if e.mode == "40000" {
			typ = "dir"
		}
		out = append(out, dirEntryRest{name: e.name, path: p, sha: e.sha, typ: typ})
	}
	return out
}

type commitListEntry struct {
	sha    string
	commit parsedCommitRest
}

// listCommitsRest returns commits reachable from tipSha, newest first
// (depth-first, parents in order).
func listCommitsRest(repo *Repo, tipSha string) []commitListEntry {
	var out []commitListEntry
	seen := map[string]bool{}
	var visit func(sha string)
	visit = func(sha string) {
		if sha == "" || seen[sha] {
			return
		}
		seen[sha] = true
		obj := repo.Objects[sha]
		if obj == nil || obj.Type != ObjCommit {
			return
		}
		parsed := parseCommitRest(obj.Content)
		out = append(out, commitListEntry{sha: sha, commit: parsed})
		for _, parent := range parsed.parents {
			visit(parent)
		}
	}
	visit(tipSha)
	return out
}

func flattenTreeRest(repo *Repo, treeSha, prefix string, out map[string]string) {
	if treeSha == "" {
		return
	}
	for _, e := range getTreeRest(repo, treeSha) {
		p := e.name
		if prefix != "" {
			p = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			flattenTreeRest(repo, e.sha, p, out)
		} else {
			out[p] = e.sha
		}
	}
}

func countLinesRest(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	parts := strings.Split(string(buf), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return len(parts)
}

func diffTreesRest(repo *Repo, baseTreeSha, headTreeSha string) []*PullFile {
	base := map[string]string{}
	head := map[string]string{}
	flattenTreeRest(repo, baseTreeSha, "", base)
	flattenTreeRest(repo, headTreeSha, "", head)
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var files []*PullFile
	for _, path := range sorted {
		b, bOk := base[path]
		h, hOk := head[path]
		if bOk && hOk && b == h {
			continue
		}
		if !bOk && hOk {
			var content []byte
			if obj := repo.Objects[h]; obj != nil {
				content = obj.Content
			}
			additions := countLinesRest(content)
			files = append(files, &PullFile{Filename: path, Status: "added", Additions: additions, Deletions: 0, Changes: additions})
		} else if hOk == false && bOk {
			var content []byte
			if obj := repo.Objects[b]; obj != nil {
				content = obj.Content
			}
			deletions := countLinesRest(content)
			files = append(files, &PullFile{Filename: path, Status: "removed", Additions: 0, Deletions: deletions, Changes: deletions})
		} else if bOk && hOk {
			var bContent, hContent []byte
			if obj := repo.Objects[b]; obj != nil {
				bContent = obj.Content
			}
			if obj := repo.Objects[h]; obj != nil {
				hContent = obj.Content
			}
			additions := countLinesRest(hContent)
			deletions := countLinesRest(bContent)
			files = append(files, &PullFile{Filename: path, Status: "modified", Additions: additions, Deletions: deletions, Changes: additions + deletions})
		}
	}
	return files
}

// resolveRefRest resolves a ref-ish (branch name, full ref, or commit sha)
// to a commit sha, or "".
func resolveRefRest(repo *Repo, ref string) string {
	if sha, ok := repo.Refs[ref]; ok {
		return sha
	}
	full := "refs/heads/" + ref
	if sha, ok := repo.Refs[full]; ok {
		return sha
	}
	if obj := repo.Objects[ref]; obj != nil && obj.Type == ObjCommit {
		return ref
	}
	return ""
}

func tipOfBranchRest(repo *Repo, branch string) string {
	if sha, ok := repo.Refs["refs/heads/"+branch]; ok {
		return sha
	}
	return ""
}

// ---------------------------------------------------------------------------
// Serializers
// ---------------------------------------------------------------------------

func isoFromCounter(counter int) string {
	return time.Unix(int64(commitEpochBase+counter), 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

func repoJSON(repo *Repo) *Obj {
	return O(
		"id", repo.ID,
		"node_id", GlobalID("Repository", repo.ID),
		"name", repo.Name,
		"full_name", repo.FullName,
		"owner", O("login", repo.Owner, "id", 1, "type", "User"),
		"private", repo.Private,
		"description", repo.Description,
		"fork", false,
		"default_branch", repo.DefaultBranch,
		"created_at", isoFromCounter(repo.ID),
		"updated_at", isoFromCounter(repo.ID),
		"html_url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name,
		"url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name,
	)
}

func labelJSON(label *Label) *Obj {
	return O("name", label.Name, "color", label.Color, "description", "")
}

func issueJSON(issue *Issue, repo *Repo) *Obj {
	labels := []any{}
	for _, name := range issue.Labels {
		if label, ok := repo.Labels.Get(name); ok {
			labels = append(labels, labelJSON(label))
		} else {
			labels = append(labels, O("name", name, "color", "000000"))
		}
	}
	return O(
		"id", issue.ID,
		"node_id", GlobalID("Issue", issue.ID),
		"number", issue.Number,
		"title", issue.Title,
		"body", issue.Body,
		"state", issue.State,
		"labels", labels,
		"user", O("login", repo.Owner, "id", 1, "type", "User"),
		"created_at", isoFromCounter(issue.CreatedAt),
		"updated_at", isoFromCounter(issue.CreatedAt),
		"comments", len(issue.Comments),
		"comments_url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues/"+itoa(issue.Number)+"/comments",
	)
}

func pullJSON(pull *Pull, repo *Repo) *Obj {
	return O(
		"id", pull.ID,
		"node_id", GlobalID("PullRequest", pull.ID),
		"number", pull.Number,
		"title", pull.Title,
		"body", pull.Body,
		"state", pull.State,
		"merged", pull.Merged,
		"merged_at", nullableString(pull.MergedAt),
		"merge_commit_sha", nullableString(pull.MergeCommitSha),
		"head", O("ref", pull.Head.Ref, "sha", pull.Head.Sha, "repo", O("full_name", repo.FullName)),
		"base", O("ref", pull.Base.Ref, "sha", pull.Base.Sha, "repo", O("full_name", repo.FullName)),
		"user", O("login", repo.Owner, "id", 1, "type", "User"),
		"created_at", isoFromCounter(pull.CreatedAt),
		"updated_at", isoFromCounter(pull.CreatedAt),
	)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func commentJSON(comment *Comment, repo *Repo) *Obj {
	return O(
		"id", comment.ID,
		"node_id", GlobalID("IssueComment", comment.ID),
		"body", comment.Body,
		"user", O("login", repo.Owner, "id", 1, "type", "User"),
		"created_at", isoFromCounter(comment.CreatedAt),
		"updated_at", isoFromCounter(comment.CreatedAt),
	)
}

// parseJSONValue parses JSON text into generic values (map[string]any,
// []any, string, bool, nil, float64). It mirrors JSON.parse closely enough
// for request bodies: duplicate keys keep the last, trailing garbage is an
// error.
func parseJSONValue(data []byte) (any, error) {
	dec := newJSONDecoder(data)
	v, err := dec.parseValue()
	if err != nil {
		return nil, err
	}
	dec.skipWS()
	if dec.pos != len(dec.data) {
		return nil, errJSONSyntax
	}
	return v, nil
}

var errJSONSyntax = &jsonError{"unexpected token"}

type jsonError struct{ msg string }

func (e *jsonError) Error() string { return e.msg }

type jsonDecoder struct {
	data []byte
	pos  int
}

func newJSONDecoder(data []byte) *jsonDecoder { return &jsonDecoder{data: data} }

func (d *jsonDecoder) skipWS() {
	for d.pos < len(d.data) {
		c := d.data[d.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			d.pos++
		} else {
			break
		}
	}
}

func (d *jsonDecoder) parseValue() (any, error) {
	d.skipWS()
	if d.pos >= len(d.data) {
		return nil, errJSONSyntax
	}
	switch c := d.data[d.pos]; {
	case c == '{':
		return d.parseObject()
	case c == '[':
		return d.parseArray()
	case c == '"':
		s, err := d.parseString()
		return s, err
	case c == 't':
		if d.consume("true") {
			return true, nil
		}
		return nil, errJSONSyntax
	case c == 'f':
		if d.consume("false") {
			return false, nil
		}
		return nil, errJSONSyntax
	case c == 'n':
		if d.consume("null") {
			return nil, nil
		}
		return nil, errJSONSyntax
	default:
		return d.parseNumber()
	}
}

func (d *jsonDecoder) consume(s string) bool {
	if d.pos+len(s) > len(d.data) {
		return false
	}
	if string(d.data[d.pos:d.pos+len(s)]) != s {
		return false
	}
	d.pos += len(s)
	return true
}

func (d *jsonDecoder) parseObject() (any, error) {
	d.pos++ // {
	m := map[string]any{}
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return m, nil
	}
	for {
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != '"' {
			return nil, errJSONSyntax
		}
		key, err := d.parseString()
		if err != nil {
			return nil, err
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return nil, errJSONSyntax
		}
		d.pos++
		val, err := d.parseValue()
		if err != nil {
			return nil, err
		}
		m[key] = val
		d.skipWS()
		if d.pos >= len(d.data) {
			return nil, errJSONSyntax
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return m, nil
		}
		if d.data[d.pos] != ',' {
			return nil, errJSONSyntax
		}
		d.pos++
	}
}

func (d *jsonDecoder) parseArray() (any, error) {
	d.pos++ // [
	var arr []any
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		return arr, nil
	}
	for {
		val, err := d.parseValue()
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
		d.skipWS()
		if d.pos >= len(d.data) {
			return nil, errJSONSyntax
		}
		if d.data[d.pos] == ']' {
			d.pos++
			return arr, nil
		}
		if d.data[d.pos] != ',' {
			return nil, errJSONSyntax
		}
		d.pos++
	}
}

func (d *jsonDecoder) parseString() (string, error) {
	// d.data[d.pos] == '"'
	d.pos++
	var sb strings.Builder
	for d.pos < len(d.data) {
		c := d.data[d.pos]
		if c == '"' {
			d.pos++
			return sb.String(), nil
		}
		if c == '\\' {
			d.pos++
			if d.pos >= len(d.data) {
				return "", errJSONSyntax
			}
			e := d.data[d.pos]
			d.pos++
			switch e {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				if d.pos+4 > len(d.data) {
					return "", errJSONSyntax
				}
				cp, err := strconv.ParseUint(string(d.data[d.pos:d.pos+4]), 16, 32)
				if err != nil {
					return "", errJSONSyntax
				}
				d.pos += 4
				if cp >= 0xD800 && cp <= 0xDBFF {
					// High surrogate: try a following \uXXXX low surrogate.
					if d.pos+6 <= len(d.data) && d.data[d.pos] == '\\' && d.data[d.pos+1] == 'u' {
						lo, err := strconv.ParseUint(string(d.data[d.pos+2:d.pos+6]), 16, 32)
						if err == nil && lo >= 0xDC00 && lo <= 0xDFFF {
							d.pos += 6
							r := 0x10000 + (rune(cp-0xD800) << 10) + rune(lo-0xDC00)
							sb.WriteRune(r)
							continue
						}
					}
					sb.WriteRune(utf8.RuneError)
				} else if cp >= 0xDC00 && cp <= 0xDFFF {
					sb.WriteRune(utf8.RuneError)
				} else {
					sb.WriteRune(rune(cp))
				}
			default:
				return "", errJSONSyntax
			}
			continue
		}
		if c < 0x20 {
			return "", errJSONSyntax
		}
		// Copy one UTF-8 rune (invalid bytes become U+FFFD, like Node's
		// Buffer.toString('utf8') + JSON.parse path).
		_, size := utf8.DecodeRune(d.data[d.pos:])
		if size == 1 && d.data[d.pos] >= 0x80 {
			// Invalid byte: JSON.parse in Node would reject it, but the
			// body was already decoded to a JS string with U+FFFD, so the
			// value a handler sees is U+FFFD.
			sb.WriteRune(utf8.RuneError)
			d.pos++
		} else {
			sb.Write(d.data[d.pos : d.pos+size])
			d.pos += size
		}
	}
	return "", errJSONSyntax
}

func (d *jsonDecoder) parseNumber() (any, error) {
	start := d.pos
	if d.pos < len(d.data) && d.data[d.pos] == '-' {
		d.pos++
	}
	// Strict JSON number grammar: -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
	if d.pos >= len(d.data) {
		return nil, errJSONSyntax
	}
	if d.data[d.pos] == '0' {
		d.pos++
	} else if d.data[d.pos] >= '1' && d.data[d.pos] <= '9' {
		for d.pos < len(d.data) && d.data[d.pos] >= '0' && d.data[d.pos] <= '9' {
			d.pos++
		}
	} else {
		return nil, errJSONSyntax
	}
	if d.pos < len(d.data) && d.data[d.pos] == '.' {
		d.pos++
		if d.pos >= len(d.data) || d.data[d.pos] < '0' || d.data[d.pos] > '9' {
			return nil, errJSONSyntax
		}
		for d.pos < len(d.data) && d.data[d.pos] >= '0' && d.data[d.pos] <= '9' {
			d.pos++
		}
	}
	if d.pos < len(d.data) && (d.data[d.pos] == 'e' || d.data[d.pos] == 'E') {
		d.pos++
		if d.pos < len(d.data) && (d.data[d.pos] == '+' || d.data[d.pos] == '-') {
			d.pos++
		}
		if d.pos >= len(d.data) || d.data[d.pos] < '0' || d.data[d.pos] > '9' {
			return nil, errJSONSyntax
		}
		for d.pos < len(d.data) && d.data[d.pos] >= '0' && d.data[d.pos] <= '9' {
			d.pos++
		}
	}
	f, err := strconv.ParseFloat(string(d.data[start:d.pos]), 64)
	if err != nil {
		return nil, errJSONSyntax
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleRest handles one REST request. Returns true when this handler
// answered the request, false when the path is not a REST route it owns.
func handleRest(w http.ResponseWriter, r *http.Request, pathname, method string, viewerLogin string) bool {
	segments := splitNonEmpty(pathname, "/")
	if len(segments) < 2 || segments[0] != "api" || segments[1] != "v3" {
		return false
	}

	// Auth: every /api/v3 route requires the bearer token (read at request time).
	token := shopToken()
	auth := r.Header.Get("Authorization")
	if token == "" || auth != "Bearer "+token {
		restSendError(w, 401, "Bad credentials")
		return true
	}

	rest := segments[2:]
	q := parseQueryOrdered(r.URL.RawQuery)

	bodyNeeded := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
	var body map[string]any
	if bodyNeeded {
		read, ok := readJSONBody(r)
		if !ok {
			restSendError(w, 400, "Problems parsing JSON")
			return true
		}
		body = read
	} else {
		body = map[string]any{}
	}

	str := func(v any) (string, bool) {
		s, ok := v.(string)
		return s, ok
	}
	boolean := func(v any, fallback bool) bool {
		if b, ok := v.(bool); ok {
			return b
		}
		return fallback
	}

	// Lock: reads share, mutations exclude. The response is written while
	// holding the lock (writes are to a local socket buffer; Node's
	// single-threaded handler is atomic the same way).
	if method == "GET" {
		storeMu.RLock()
		defer storeMu.RUnlock()
	} else {
		storeMu.Lock()
		defer storeMu.Unlock()
	}

	// ---- API root ------------------------------------------------------------
	if len(rest) == 0 {
		if method != "GET" {
			restSendError(w, 404, "Not Found")
			return true
		}
		restSendJSON(w, 200, O(
			"current_user_url", "/api/v3/user",
			"rate_limit_url", "/api/v3/rate_limit",
			"user_url", "/api/v3/users/{user}",
			"repository_url", "/api/v3/repos/{owner}/{repo}",
		), map[string]string{"x-oauth-scopes": "repo, read:org"})
		return true
	}

	// ---- /meta ---------------------------------------------------------------
	if len(rest) == 1 && rest[0] == "meta" {
		if method != "GET" {
			restSendError(w, 404, "Not Found")
			return true
		}
		restSendJSON(w, 200, O("installed_version", "1.0.0"), nil)
		return true
	}

	// ---- /user ---------------------------------------------------------------
	if len(rest) == 1 && rest[0] == "user" && method == "GET" {
		restSendJSON(w, 200, O("login", viewerLogin, "id", 1, "type", "User"), nil)
		return true
	}

	// ---- POST /user/repos + GET /user/repos ----------------------------------
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		if method == "POST" {
			name, ok := str(body["name"])
			if !ok || name == "" || strings.Contains(name, "/") {
				restSendError(w, 422, "Repository name is required")
				return true
			}
			if store.GetRepo(viewerLogin, name) != nil {
				restSendError(w, 422, "Repository already exists")
				return true
			}
			desc, _ := str(body["description"])
			repo := store.CreateRepo(viewerLogin, name, desc, boolean(body["private"], false))
			restSendJSON(w, 201, repoJSON(repo), nil)
			return true
		}
		if method == "GET" {
			repos := store.ListReposByOwner(viewerLogin)
			page, link := paginate(repos, q, r)
			arr := []any{}
			for _, repo := range page {
				arr = append(arr, repoJSON(repo))
			}
			restSendJSON(w, 200, arr, withLink(link))
			return true
		}
		restSendError(w, 404, "Not Found")
		return true
	}

	// ---- /repos/{owner}/{repo}... --------------------------------------------
	if len(rest) >= 3 && rest[0] == "repos" {
		owner := rest[1]
		repoName := rest[2]
		tail := rest[3:]

		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			restSendError(w, 404, "Not Found")
			return true
		}

		// GET/PATCH/DELETE /repos/{owner}/{repo}
		if len(tail) == 0 {
			if method == "GET" {
				restSendJSON(w, 200, repoJSON(repo), nil)
				return true
			}
			if method == "PATCH" {
				if desc, ok := str(body["description"]); ok {
					repo.Description = desc
				}
				restSendJSON(w, 200, repoJSON(repo), nil)
				return true
			}
			if method == "DELETE" {
				store.DeleteRepo(owner, repoName)
				restSendNoContent(w)
				return true
			}
			restSendError(w, 404, "Not Found")
			return true
		}

		// ---- git/refs ----------------------------------------------------------
		if tail[0] == "git" && len(tail) >= 2 && tail[1] == "refs" {
			if method == "POST" && len(tail) == 2 {
				ref, okRef := str(body["ref"])
				sha, okSha := str(body["sha"])
				if !okRef || !strings.HasPrefix(ref, "refs/heads/") || ref == "refs/heads/" || !okSha {
					restSendError(w, 422, "Invalid ref or sha")
					return true
				}
				obj := repo.Objects[sha]
				if obj == nil || obj.Type != ObjCommit {
					restSendError(w, 422, "Object does not exist")
					return true
				}
				if _, exists := repo.Refs[ref]; exists {
					restSendError(w, 422, "Reference already exists")
					return true
				}
				repo.Refs[ref] = sha
				restSendJSON(w, 201, O("ref", ref, "object", O("sha", sha)), nil)
				return true
			}
			if method == "GET" && len(tail) >= 4 && tail[2] == "heads" {
				branch := strings.Join(tail[3:], "/")
				sha, ok := repo.Refs["refs/heads/"+branch]
				if !ok {
					restSendError(w, 404, "Not Found")
					return true
				}
				restSendJSON(w, 200, O("ref", "refs/heads/"+branch, "object", O("sha", sha)), nil)
				return true
			}
			restSendError(w, 404, "Not Found")
			return true
		}

		// ---- branches ----------------------------------------------------------
		if tail[0] == "branches" {
			var branchNames []string
			for ref := range repo.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					branchNames = append(branchNames, ref[len("refs/heads/"):])
				}
			}
			sort.Strings(branchNames)
			if method == "GET" && len(tail) == 1 {
				arr := []any{}
				for _, name := range branchNames {
					arr = append(arr, O("name", name, "commit", O("sha", repo.Refs["refs/heads/"+name])))
				}
				restSendJSON(w, 200, arr, nil)
				return true
			}
			if method == "GET" && len(tail) >= 2 {
				branch := strings.Join(tail[1:], "/")
				sha, ok := repo.Refs["refs/heads/"+branch]
				if !ok {
					restSendError(w, 404, "Not Found")
					return true
				}
				restSendJSON(w, 200, O("name", branch, "commit", O("sha", sha)), nil)
				return true
			}
			restSendError(w, 404, "Not Found")
			return true
		}

		// ---- commits -----------------------------------------------------------
		if tail[0] == "commits" && len(tail) == 1 && method == "GET" {
			var tip string
			if refParam, ok := q.get("sha"); ok {
				tip = resolveRefRest(repo, refParam)
			} else {
				tip = tipOfBranchRest(repo, repo.DefaultBranch)
			}
			if tip == "" {
				restSendJSON(w, 200, []any{}, nil)
				return true
			}
			commits := listCommitsRest(repo, tip)
			arr := []any{}
			for _, c := range commits {
				date := time.Unix(int64(c.commit.authorEpoch), 0).UTC().Format("2006-01-02T15:04:05.000Z")
				parents := []any{}
				for _, p := range c.commit.parents {
					parents = append(parents, O("sha", p))
				}
				arr = append(arr, O(
					"sha", c.sha,
					"commit", O(
						"message", c.commit.message,
						"author", O("name", c.commit.authorName, "email", c.commit.authorEmail, "date", date),
						"committer", O("name", c.commit.authorName, "email", c.commit.authorEmail, "date", date),
					),
					"parents", parents,
				))
			}
			restSendJSON(w, 200, arr, nil)
			return true
		}

		// ---- contents ----------------------------------------------------------
		if tail[0] == "contents" {
			contentPath := strings.Join(tail[1:], "/")
			if method == "GET" {
				var tip string
				if refParam, ok := q.get("ref"); ok {
					tip = resolveRefRest(repo, refParam)
				} else {
					tip = tipOfBranchRest(repo, repo.DefaultBranch)
				}
				if tip == "" {
					restSendError(w, 404, "Not Found")
					return true
				}
				tipObj := repo.Objects[tip]
				if tipObj == nil || tipObj.Type != ObjCommit {
					restSendError(w, 404, "Not Found")
					return true
				}
				treeSha := parseCommitRest(tipObj.Content).tree
				if contentPath == "" {
					children := listDirAtTreeRest(repo, treeSha, "")
					arr := []any{}
					for _, c := range children {
						arr = append(arr, O("type", c.typ, "name", c.name, "path", c.path, "sha", c.sha, "size", 0))
					}
					restSendJSON(w, 200, arr, nil)
					return true
				}
				if blobSha := getFileAtTreeRest(repo, treeSha, contentPath); blobSha != "" {
					blob := repo.Objects[blobSha]
					var bytes []byte
					if blob != nil {
						bytes = blob.Content
					}
					name := contentPath
					if idx := strings.LastIndex(contentPath, "/"); idx >= 0 {
						name = contentPath[idx+1:]
					}
					restSendJSON(w, 200, O(
						"type", "file",
						"encoding", "base64",
						"content", base64.StdEncoding.EncodeToString(bytes),
						"sha", blobSha,
						"size", len(bytes),
						"name", name,
						"path", contentPath,
					), nil)
					return true
				}
				children := listDirAtTreeRest(repo, treeSha, contentPath)
				if len(children) > 0 {
					arr := []any{}
					for _, c := range children {
						arr = append(arr, O("type", c.typ, "name", c.name, "path", c.path, "sha", c.sha, "size", 0))
					}
					restSendJSON(w, 200, arr, nil)
					return true
				}
				restSendError(w, 404, "Not Found")
				return true
			}

			if method == "PUT" {
				message, okMsg := str(body["message"])
				contentB64, okContent := str(body["content"])
				if !okMsg || message == "" || !okContent {
					restSendError(w, 422, "message and content are required")
					return true
				}
				bytes, ok := lenientB64Decode(contentB64)
				if !ok {
					restSendError(w, 400, "content is not valid base64")
					return true
				}
				branch, _ := str(body["branch"])
				if branch == "" {
					branch = repo.DefaultBranch
				}
				refName := "refs/heads/" + branch
				tip, _ := repo.Refs[refName]
				var parentTreeSha string
				if tip != "" {
					if tipObj := repo.Objects[tip]; tipObj != nil && tipObj.Type == ObjCommit {
						parentTreeSha = parseCommitRest(tipObj.Content).tree
					}
				}
				currentBlob := getFileAtTreeRest(repo, parentTreeSha, contentPath)
				givenSha, hasSha := str(body["sha"])
				isUpdate := currentBlob != ""
				if hasSha {
					if currentBlob == "" {
						restSendError(w, 404, "Not Found")
						return true
					}
					if givenSha != currentBlob {
						restSendError(w, 409, "sha does not match the current file")
						return true
					}
				} else if isUpdate {
					restSendError(w, 422, "sha is required to update an existing file")
					return true
				}
				blobSha := storeObjectRest(repo, ObjBlob, bytes)
				treeSha := buildTreeRest(repo, parentTreeSha, contentPath, blobSha)
				epoch := commitEpochBase + store.Tick()
				commitSha, commitContent := commitObjectRest(treeSha, tip, message, viewerLogin, epoch)
				repo.Objects[commitSha] = &GitObject{Type: ObjCommit, Content: commitContent}
				repo.Refs[refName] = commitSha
				status := 201
				if isUpdate {
					status = 200
				}
				name := contentPath
				if idx := strings.LastIndex(contentPath, "/"); idx >= 0 {
					name = contentPath[idx+1:]
				}
				restSendJSON(w, status, O(
					"content", O(
						"name", name,
						"path", contentPath,
						"sha", blobSha,
						"size", len(bytes),
						"type", "file",
						"encoding", "base64",
						"content", contentB64,
					),
					"commit", O("sha", commitSha, "message", message),
				), nil)
				return true
			}

			if method == "DELETE" {
				message, okMsg := str(body["message"])
				givenSha, okSha := str(body["sha"])
				if !okMsg || message == "" || !okSha {
					restSendError(w, 422, "message and sha are required")
					return true
				}
				branch, _ := str(body["branch"])
				if branch == "" {
					branch = repo.DefaultBranch
				}
				refName := "refs/heads/" + branch
				tip, _ := repo.Refs[refName]
				if tip == "" {
					restSendError(w, 404, "Not Found")
					return true
				}
				tipObj := repo.Objects[tip]
				var parentTreeSha string
				if tipObj != nil && tipObj.Type == ObjCommit {
					parentTreeSha = parseCommitRest(tipObj.Content).tree
				}
				currentBlob := getFileAtTreeRest(repo, parentTreeSha, contentPath)
				if currentBlob == "" {
					restSendError(w, 404, "Not Found")
					return true
				}
				if givenSha != currentBlob {
					restSendError(w, 409, "sha does not match the current file")
					return true
				}
				treeSha := buildTreeRest(repo, parentTreeSha, contentPath, "")
				epoch := commitEpochBase + store.Tick()
				commitSha, commitContent := commitObjectRest(treeSha, tip, message, viewerLogin, epoch)
				repo.Objects[commitSha] = &GitObject{Type: ObjCommit, Content: commitContent}
				repo.Refs[refName] = commitSha
				restSendJSON(w, 200, O("content", nil, "commit", O("sha", commitSha, "message", message)), nil)
				return true
			}

			restSendError(w, 404, "Not Found")
			return true
		}

		// ---- labels ------------------------------------------------------------
		if tail[0] == "labels" && len(tail) == 1 {
			if method == "POST" {
				name, ok := str(body["name"])
				if !ok || name == "" {
					restSendError(w, 422, "Label name is required")
					return true
				}
				color, okc := str(body["color"])
				if !okc {
					color = "000000"
				}
				label := &Label{Name: name, Color: color}
				repo.Labels.Set(name, label)
				restSendJSON(w, 201, labelJSON(label), nil)
				return true
			}
			if method == "GET" {
				arr := []any{}
				for _, l := range repo.Labels.Values() {
					arr = append(arr, labelJSON(l))
				}
				restSendJSON(w, 200, arr, nil)
				return true
			}
			restSendError(w, 404, "Not Found")
			return true
		}

		// ---- issues ------------------------------------------------------------
		if tail[0] == "issues" {
			if len(tail) == 1 {
				if method == "POST" {
					title, ok := str(body["title"])
					if !ok || title == "" {
						restSendError(w, 422, "Title is required")
						return true
					}
					var labelNames []string
					if arr, isArr := body["labels"].([]any); isArr {
						for _, l := range arr {
							if s, isStr := l.(string); isStr {
								labelNames = append(labelNames, s)
							}
						}
					}
					for _, name := range labelNames {
						if !repo.Labels.Has(name) {
							restSendError(w, 422, "Label does not exist: "+name)
							return true
						}
					}
					issue := &Issue{
						ID:        store.NextIssueID,
						Number:    repo.IssueSeq + 1,
						Title:     title,
						Body:      stringOr(body["body"], ""),
						State:     "open",
						Labels:    labelNames,
						CreatedAt: store.Tick(),
						Comments:  nil,
					}
					store.NextIssueID++
					repo.IssueSeq++
					repo.Issues = append(repo.Issues, issue)
					restSendJSON(w, 201, issueJSON(issue, repo), nil)
					return true
				}
				if method == "GET" {
					state, _ := q.get("state")
					if state == "" {
						state = "open"
					}
					issues := repo.Issues
					if state == "open" || state == "closed" {
						var filtered []*Issue
						for _, i := range issues {
							if i.State == state {
								filtered = append(filtered, i)
							}
						}
						issues = filtered
					}
					direction, _ := q.get("direction")
					if direction == "" {
						direction = "desc"
					}
					sorted := make([]*Issue, len(issues))
					copy(sorted, issues)
					sort.SliceStable(sorted, func(a, b int) bool {
						if direction == "asc" {
							return sorted[a].CreatedAt < sorted[b].CreatedAt
						}
						return sorted[a].CreatedAt > sorted[b].CreatedAt
					})
					page, link := paginate(sorted, q, r)
					arr := []any{}
					for _, i := range page {
						arr = append(arr, issueJSON(i, repo))
					}
					restSendJSON(w, 200, arr, withLink(link))
					return true
				}
				restSendError(w, 404, "Not Found")
				return true
			}

			// /issues/{number}...
			number, err := strconv.Atoi(tail[1])
			if err != nil {
				restSendError(w, 404, "Not Found")
				return true
			}
			var issue *Issue
			for _, i := range repo.Issues {
				if i.Number == number {
					issue = i
					break
				}
			}
			if issue == nil {
				restSendError(w, 404, "Not Found")
				return true
			}
			if len(tail) == 2 {
				if method == "GET" {
					restSendJSON(w, 200, issueJSON(issue, repo), nil)
					return true
				}
				if method == "PATCH" {
					if state, ok := str(body["state"]); ok && (state == "open" || state == "closed") {
						issue.State = state
					}
					if title, ok := str(body["title"]); ok {
						issue.Title = title
					}
					if bodyText, ok := str(body["body"]); ok {
						issue.Body = bodyText
					}
					restSendJSON(w, 200, issueJSON(issue, repo), nil)
					return true
				}
				restSendError(w, 404, "Not Found")
				return true
			}
			if len(tail) == 3 && tail[2] == "comments" {
				if method == "POST" {
					text, ok := str(body["body"])
					if !ok || text == "" {
						restSendError(w, 422, "Comment body is required")
						return true
					}
					comment := &Comment{ID: store.NextCommentID, Body: text, CreatedAt: store.Tick()}
					store.NextCommentID++
					issue.Comments = append(issue.Comments, comment)
					restSendJSON(w, 201, commentJSON(comment, repo), nil)
					return true
				}
				if method == "GET" {
					arr := []any{}
					for _, c := range issue.Comments {
						arr = append(arr, commentJSON(c, repo))
					}
					restSendJSON(w, 200, arr, nil)
					return true
				}
				restSendError(w, 404, "Not Found")
				return true
			}
			restSendError(w, 404, "Not Found")
			return true
		}

		// ---- pulls -------------------------------------------------------------
		if tail[0] == "pulls" {
			if len(tail) == 1 {
				if method == "POST" {
					title, okTitle := str(body["title"])
					head, okHead := str(body["head"])
					base, okBase := str(body["base"])
					if !okTitle || title == "" || !okHead || head == "" || !okBase || base == "" {
						restSendError(w, 422, "title, head and base are required")
						return true
					}
					headSha := tipOfBranchRest(repo, head)
					baseSha := tipOfBranchRest(repo, base)
					if headSha == "" || baseSha == "" {
						restSendError(w, 422, "head or base branch does not exist")
						return true
					}
					var headTree, baseTree string
					if obj := repo.Objects[headSha]; obj != nil && obj.Type == ObjCommit {
						headTree = parseCommitRest(obj.Content).tree
					}
					if obj := repo.Objects[baseSha]; obj != nil && obj.Type == ObjCommit {
						baseTree = parseCommitRest(obj.Content).tree
					}
					pull := &Pull{
						ID:        store.NextPullID,
						Number:    repo.PullSeq + 1,
						Title:     title,
						Body:      stringOr(body["body"], ""),
						State:     "open",
						Merged:    false,
						MergedAt:  "",
						Head:      PullRef{Ref: head, Sha: headSha},
						Base:      PullRef{Ref: base, Sha: baseSha},
						Files:     diffTreesRest(repo, baseTree, headTree),
						CreatedAt: store.Tick(),
					}
					store.NextPullID++
					repo.PullSeq++
					repo.Pulls = append(repo.Pulls, pull)
					restSendJSON(w, 201, pullJSON(pull, repo), nil)
					return true
				}
				if method == "GET" {
					state, _ := q.get("state")
					if state == "" {
						state = "open"
					}
					pulls := repo.Pulls
					if state == "open" || state == "closed" {
						var filtered []*Pull
						for _, p := range pulls {
							if p.State == state {
								filtered = append(filtered, p)
							}
						}
						pulls = filtered
					}
					page, link := paginate(pulls, q, r)
					arr := []any{}
					for _, p := range page {
						arr = append(arr, pullJSON(p, repo))
					}
					restSendJSON(w, 200, arr, withLink(link))
					return true
				}
				restSendError(w, 404, "Not Found")
				return true
			}

			number, err := strconv.Atoi(tail[1])
			if err != nil {
				restSendError(w, 404, "Not Found")
				return true
			}
			var pull *Pull
			for _, p := range repo.Pulls {
				if p.Number == number {
					pull = p
					break
				}
			}
			if pull == nil {
				restSendError(w, 404, "Not Found")
				return true
			}
			if len(tail) == 2 && method == "GET" {
				restSendJSON(w, 200, pullJSON(pull, repo), nil)
				return true
			}
			if len(tail) == 3 && tail[2] == "files" && method == "GET" {
				arr := []any{}
				for _, f := range pull.Files {
					arr = append(arr, O("filename", f.Filename, "status", f.Status, "additions", f.Additions, "deletions", f.Deletions, "changes", f.Changes))
				}
				restSendJSON(w, 200, arr, nil)
				return true
			}
			if len(tail) == 3 && tail[2] == "merge" && method == "PUT" {
				if pull.Merged {
					restSendJSON(w, 200, O("merged", true, "sha", nullableString(pull.MergeCommitSha), "message", "Pull request #"+itoa(pull.Number)+" already merged"), nil)
					return true
				}
				if pull.State != "open" {
					restSendError(w, 405, "Pull request is closed")
					return true
				}
				baseTip := tipOfBranchRest(repo, pull.Base.Ref)
				headTip := tipOfBranchRest(repo, pull.Head.Ref)
				if baseTip == "" || headTip == "" {
					restSendError(w, 422, "head or base branch does not exist")
					return true
				}
				// Conflict: base moved past the PR's base sha AND a touched
				// file's current base content differs from the PR head's.
				if baseTip != pull.Base.Sha {
					var baseTree, headTree string
					if obj := repo.Objects[baseTip]; obj != nil && obj.Type == ObjCommit {
						baseTree = parseCommitRest(obj.Content).tree
					}
					if obj := repo.Objects[headTip]; obj != nil && obj.Type == ObjCommit {
						headTree = parseCommitRest(obj.Content).tree
					}
					for _, file := range pull.Files {
						baseBlob := getFileAtTreeRest(repo, baseTree, file.Filename)
						headBlob := getFileAtTreeRest(repo, headTree, file.Filename)
						if baseBlob != headBlob {
							restSendError(w, 405, "Merge conflict")
							return true
						}
					}
				}
				var headTree string
				if obj := repo.Objects[headTip]; obj != nil && obj.Type == ObjCommit {
					headTree = parseCommitRest(obj.Content).tree
				}
				if headTree == "" {
					restSendError(w, 422, "head branch has no commits")
					return true
				}
				message := "Merge pull request #" + itoa(pull.Number) + " from " + repo.Owner + "/" + pull.Head.Ref
				epoch := commitEpochBase + store.Tick()
				mergeSha, mergeContent := commitObjectRest(headTree, baseTip, message, viewerLogin, epoch)
				repo.Objects[mergeSha] = &GitObject{Type: ObjCommit, Content: mergeContent}
				repo.Refs["refs/heads/"+pull.Base.Ref] = mergeSha
				pull.State = "closed"
				pull.Merged = true
				pull.MergedAt = restMergedAt
				pull.MergeCommitSha = mergeSha
				restSendJSON(w, 200, O("merged", true, "sha", mergeSha, "message", message), nil)
				return true
			}
			restSendError(w, 404, "Not Found")
			return true
		}

		// Unknown sub-route under an existing repo.
		restSendError(w, 404, "Not Found")
		return true
	}

	// Unknown /api/v3 route.
	restSendError(w, 404, "Not Found")
	return true
}

func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}

// lenientB64Decode mirrors Node's Buffer.from(s, 'base64'): invalid
// characters are ignored, missing padding is tolerated.
func lenientB64Decode(s string) ([]byte, bool) {
	filtered := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			filtered = append(filtered, c)
		}
	}
	// Pad to a multiple of 4.
	for len(filtered)%4 != 0 {
		filtered = append(filtered, '=')
	}
	// Strip padding for RawStdEncoding when the length is a multiple of 4
	// but the padding is misplaced; StdEncoding handles the normal case.
	decoded, err := base64.StdEncoding.DecodeString(string(filtered))
	if err == nil {
		return decoded, true
	}
	trimmed := strings.TrimRight(string(filtered), "=")
	decoded, err = base64.RawStdEncoding.DecodeString(trimmed)
	if err == nil {
		return decoded, true
	}
	return nil, false
}
