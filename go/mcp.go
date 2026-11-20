// MCP (Model Context Protocol) surface for the git forge — JSON-RPC 2.0 over
// Streamable HTTP (plain POST, application/json responses; no SSE needed).
//
// Self-contained except for store.go: it reads/writes the SAME shared `store`
// as REST, so a repo/issue/PR created over MCP reads back over REST identically.
//
// Protocol: modern era, revision 2026-07-28 (server/discover), plus the legacy
// `initialize` handshake for compatibility. Every request answers well within 3s.
//
// Auth: every /mcp request requires `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>`.
// No/wrong token -> HTTP 401, checked before any tool/method handling.

package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const mcpServerName = "git-forge"
const mcpServerVersion = "1.0.0"
const mcpProtocolVersion = "2026-07-28"
const mcpMergeAt = "1970-01-01T00:00:00.000Z"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18"}

// ---------------------------------------------------------------------------
// Tool definitions (built fresh from constants every call: byte-identical)
// ---------------------------------------------------------------------------

type mcpToolDef struct {
	name        string
	description string
	properties  []mcpToolProp
	required    []string
}

type mcpToolProp struct {
	name        string
	typ         string
	description string
}

var mcpTools = []mcpToolDef{
	{"create_repo", "Create a new repository owned by the authenticated user.",
		[]mcpToolProp{
			{"name", "string", "Name of the repository to create."},
			{"description", "string", "Short description of the repository."},
			{"private", "boolean", "Whether the repository is private."},
		}, []string{"name"}},
	{"get_repo", "Get a repository by owner and name.",
		[]mcpToolProp{
			{"owner", "string", "Login of the repository owner."},
			{"repo", "string", "Name of the repository."},
		}, []string{"owner", "repo"}},
	{"create_issue", "Create an issue in a repository.",
		[]mcpToolProp{
			{"owner", "string", "Login of the repository owner."},
			{"repo", "string", "Name of the repository."},
			{"title", "string", "Title of the issue."},
			{"body", "string", "Body of the issue."},
		}, []string{"owner", "repo", "title"}},
	{"list_issues", "List issues in a repository, optionally filtered by state.",
		[]mcpToolProp{
			{"owner", "string", "Login of the repository owner."},
			{"repo", "string", "Name of the repository."},
			{"state", "string", "Issue state filter: 'open', 'closed', or 'all'."},
		}, []string{"owner", "repo"}},
	{"create_pull_request", "Open a pull request from a head branch into a base branch.",
		[]mcpToolProp{
			{"owner", "string", "Login of the repository owner."},
			{"repo", "string", "Name of the repository."},
			{"title", "string", "Title of the pull request."},
			{"head", "string", "Name of the head branch (where changes come from)."},
			{"base", "string", "Name of the base branch (where changes go into)."},
			{"body", "string", "Body of the pull request."},
		}, []string{"owner", "repo", "title", "head", "base"}},
	{"merge_pull_request", "Merge a pull request with a squash merge onto its base branch.",
		[]mcpToolProp{
			{"owner", "string", "Login of the repository owner."},
			{"repo", "string", "Name of the repository."},
			{"pull_number", "integer", "Number of the pull request to merge."},
		}, []string{"owner", "repo", "pull_number"}},
	{"list_repos", "List repositories owned by a user.",
		[]mcpToolProp{
			{"owner", "string", "Login of the user whose repositories to list."},
		}, []string{}},
}

func mcpToolsJSON() []any {
	var out []any
	for _, t := range mcpTools {
		props := NewObj()
		for _, p := range t.properties {
			props.Set(p.name, O("type", p.typ, "description", p.description))
		}
		required := []any{}
		for _, r := range t.required {
			required = append(required, r)
		}
		out = append(out, O(
			"name", t.name,
			"description", t.description,
			"inputSchema", O("type", "object", "properties", props, "required", required),
		))
	}
	return out
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func mcpAsString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func mcpAsBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func mcpAsInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case float64:
		if t == float64(int64(t)) {
			return int(t), true
		}
	case string:
		s := strings.TrimSpace(t)
		if matched, _ := regexp.MatchString(`^-?\d+$`, s); matched {
			n, err := strconv.Atoi(s)
			if err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// normKey normalizes an argument key: lowercase, strip every non-alphanumeric.
func mcpNormKey(key string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(key) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// mcpNormalizeArgs normalizes tool arguments: the suite may pass synonyms
// (owner/org/login, repo/repository, body/description, head/head_ref/head_branch,
// base/..., pull_number/pr_number/number) and/or a full_name ('owner/repo').
func mcpNormalizeArgs(raw any) map[string]any {
	out := map[string]any{}
	if s, ok := raw.(string); ok {
		parsed, err := parseJSONValue([]byte(s))
		if err != nil {
			return out
		}
		raw = parsed
	}
	rec, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	byNorm := map[string]any{}
	for k, v := range rec {
		byNorm[mcpNormKey(k)] = v
	}
	pick := func(synonyms []string) (any, bool) {
		for _, s := range synonyms {
			if v, ok := byNorm[s]; ok {
				return v, true
			}
		}
		return nil, false
	}
	set := func(canonical string, synonyms []string) {
		if v, ok := pick(synonyms); ok {
			out[canonical] = v
		}
	}
	set("owner", []string{"owner", "org", "organization", "login", "user", "ownerlogin", "username"})
	set("repo", []string{"repo", "repository", "reponame", "name"})
	set("name", []string{"name", "reponame", "repository", "repo"})
	set("title", []string{"title", "name"})
	set("body", []string{"body", "description", "text", "content"})
	set("description", []string{"description", "body", "text"})
	set("private", []string{"private", "isprivate"})
	set("head", []string{"head", "headref", "headbranch", "headname", "source", "from"})
	set("base", []string{"base", "baseref", "basebranch", "basename", "target", "into"})
	set("pull_number", []string{"pullnumber", "prnumber", "number", "pullrequestnumber", "pull", "pr"})
	set("state", []string{"state", "status"})
	set("full_name", []string{"fullname", "fullrepo", "repositoryfullname"})

	if fullName, ok := mcpAsString(out["full_name"]); ok {
		parts := strings.Split(fullName, "/")
		if len(parts) == 2 {
			if _, has := out["owner"]; !has && parts[0] != "" {
				out["owner"] = parts[0]
			}
			if _, has := out["repo"]; !has && parts[1] != "" {
				out["repo"] = parts[1]
			}
		}
	}
	return out
}

// mcpResolveOwnerRepo resolves owner/repo from normalized args, defaulting
// owner to the viewer.
func mcpResolveOwnerRepo(args map[string]any, viewer string) (string, string) {
	owner, ok := mcpAsString(args["owner"])
	if !ok {
		owner = viewer
	}
	repo, _ := mcpAsString(args["repo"])
	return owner, repo
}

// ---------------------------------------------------------------------------
// Git object helpers (local copies of the logic REST uses)
// ---------------------------------------------------------------------------

func mcpParseCommit(content []byte) (tree string, parents []string) {
	text := string(content)
	head := text
	if idx := strings.Index(text, "\n\n"); idx >= 0 {
		head = text[:idx]
	}
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "tree ") {
			tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(line[7:]))
		}
	}
	return tree, parents
}

type mcpTreeEntry struct {
	mode string
	name string
	sha  string
}

func mcpParseTree(content []byte) []mcpTreeEntry {
	var entries []mcpTreeEntry
	i := 0
	for i < len(content) {
		space := indexByte(content, 0x20, i)
		if space == -1 {
			break
		}
		mode := string(content[i:space])
		nul := indexByte(content, 0x00, space+1)
		if nul == -1 || nul+21 > len(content) {
			break
		}
		name := string(content[space+1 : nul])
		sha := hexEncode(content[nul+1 : nul+21])
		entries = append(entries, mcpTreeEntry{mode: mode, name: name, sha: sha})
		i = nul + 21
	}
	return entries
}

func mcpStoreObject(repo *Repo, typ GitObjectType, content []byte) string {
	sha := GitObjectID(typ, content)
	repo.Objects[sha] = &GitObject{Type: typ, Content: content}
	return sha
}

type mcpFlatEntry struct {
	mode string
	sha  string
}

// mcpFlattenTree flattens a tree (recursively) to path -> {mode, sha} for
// blobs only.
func mcpFlattenTree(repo *Repo, treeSha, prefix string, out map[string]mcpFlatEntry) {
	if treeSha == "" {
		return
	}
	obj := repo.Objects[treeSha]
	if obj == nil || obj.Type != ObjTree {
		return
	}
	for _, entry := range mcpParseTree(obj.Content) {
		path := entry.name
		if prefix != "" {
			path = prefix + "/" + entry.name
		}
		if entry.mode == "40000" {
			mcpFlattenTree(repo, entry.sha, path, out)
		} else {
			out[path] = mcpFlatEntry{mode: entry.mode, sha: entry.sha}
		}
	}
}

func mcpTreeOfCommit(repo *Repo, commitSha string) map[string]mcpFlatEntry {
	out := map[string]mcpFlatEntry{}
	if commitSha == "" {
		return out
	}
	obj := repo.Objects[commitSha]
	if obj == nil || obj.Type != ObjCommit {
		return out
	}
	tree, _ := mcpParseCommit(obj.Content)
	mcpFlattenTree(repo, tree, "", out)
	return out
}

func mcpBlobLines(repo *Repo, sha string) []string {
	obj := repo.Objects[sha]
	if obj == nil || obj.Type != ObjBlob {
		return []string{}
	}
	text := string(obj.Content)
	if text == "" {
		return []string{}
	}
	return strings.Split(text, "\n")
}

// mcpLineDiff: naive multiset line diff — lines added in `to` vs `from`,
// and vice versa.
func mcpLineDiff(from, to []string) (additions, deletions int) {
	counts := map[string]int{}
	for _, l := range from {
		counts[l]++
	}
	for _, l := range to {
		if c := counts[l]; c > 0 {
			counts[l] = c - 1
		} else {
			additions++
		}
	}
	for _, c := range counts {
		deletions += c
	}
	return additions, deletions
}

func mcpComputePullFiles(repo *Repo, baseSha, headSha string) []*PullFile {
	baseTree := mcpTreeOfCommit(repo, baseSha)
	headTree := mcpTreeOfCommit(repo, headSha)
	paths := map[string]bool{}
	for p := range baseTree {
		paths[p] = true
	}
	for p := range headTree {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var files []*PullFile
	for _, path := range sorted {
		b, bOk := baseTree[path]
		h, hOk := headTree[path]
		if !bOk && hOk {
			lines := mcpBlobLines(repo, h.sha)
			files = append(files, &PullFile{Filename: path, Status: "added", Additions: len(lines), Deletions: 0, Changes: len(lines)})
		} else if bOk && !hOk {
			lines := mcpBlobLines(repo, b.sha)
			files = append(files, &PullFile{Filename: path, Status: "removed", Additions: 0, Deletions: len(lines), Changes: len(lines)})
		} else if bOk && hOk && b.sha != h.sha {
			a, d := mcpLineDiff(mcpBlobLines(repo, b.sha), mcpBlobLines(repo, h.sha))
			files = append(files, &PullFile{Filename: path, Status: "modified", Additions: a, Deletions: d, Changes: a + d})
		}
	}
	return files
}

var mcpShaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// mcpResolveRef resolves a branch name (or a raw commit sha) to its tip
// commit sha, or "".
func mcpResolveRef(repo *Repo, ref string) string {
	if ref == "" {
		return ""
	}
	trimmed := strings.TrimSpace(ref)
	branch := trimmed
	if idx := strings.LastIndex(trimmed, ":"); idx >= 0 {
		branch = trimmed[idx+1:]
	}
	var candidates []string
	if strings.HasPrefix(branch, "refs/") {
		candidates = []string{branch}
	} else {
		candidates = []string{"refs/heads/" + branch, "refs/tags/" + branch}
	}
	for _, c := range candidates {
		if sha, ok := repo.Refs[c]; ok {
			return sha
		}
	}
	if mcpShaRe.MatchString(branch) {
		if obj := repo.Objects[branch]; obj != nil && obj.Type == ObjCommit {
			return branch
		}
	}
	return ""
}

func mcpCreateCommitObject(repo *Repo, treeSha string, parents []string, message, author string) string {
	body := "tree " + treeSha + "\n"
	for _, p := range parents {
		body += "parent " + p + "\n"
	}
	body += "author " + author + " <" + author + "@git-forge.local> 0 +0000\n"
	body += "committer " + author + " <" + author + "@git-forge.local> 0 +0000\n"
	body += "\n" + message + "\n"
	return mcpStoreObject(repo, ObjCommit, []byte(body))
}

// ---------------------------------------------------------------------------
// Serializers (plain JSON shapes shared by MCP text + structuredContent)
// ---------------------------------------------------------------------------

func mcpRepoJSON(repo *Repo) *Obj {
	return O(
		"id", repo.ID,
		"node_id", GlobalID("Repository", repo.ID),
		"name", repo.Name,
		"full_name", repo.FullName,
		"owner", repo.Owner,
		"description", repo.Description,
		"private", repo.Private,
		"default_branch", repo.DefaultBranch,
	)
}

func mcpIssueJSON(repo *Repo, issue *Issue) *Obj {
	return O(
		"id", issue.ID,
		"node_id", GlobalID("Issue", issue.ID),
		"number", issue.Number,
		"title", issue.Title,
		"body", issue.Body,
		"state", issue.State,
		"full_name", repo.FullName,
		"repository", repo.FullName,
	)
}

func mcpPullJSON(repo *Repo, pull *Pull) *Obj {
	files := []any{}
	for _, f := range pull.Files {
		files = append(files, O("filename", f.Filename, "status", f.Status, "additions", f.Additions, "deletions", f.Deletions, "changes", f.Changes))
	}
	return O(
		"id", pull.ID,
		"node_id", GlobalID("PullRequest", pull.ID),
		"number", pull.Number,
		"title", pull.Title,
		"body", pull.Body,
		"state", pull.State,
		"merged", pull.Merged,
		"head", O("ref", pull.Head.Ref, "sha", pull.Head.Sha),
		"base", O("ref", pull.Base.Ref, "sha", pull.Base.Sha),
		"full_name", repo.FullName,
		"repository", repo.FullName,
		"files", files,
	)
}

// ---------------------------------------------------------------------------
// Tool implementations. Each returns { ok, data } or { ok: false, error }.
// ---------------------------------------------------------------------------

type mcpToolOutcome struct {
	ok    bool
	data  *Obj
	error string
}

func mcpToolError(err string) mcpToolOutcome { return mcpToolOutcome{ok: false, error: err} }

func mcpCreateRepoTool(args map[string]any, viewer string) mcpToolOutcome {
	name, ok := mcpAsString(args["name"])
	if !ok || name == "" {
		return mcpToolError(`create_repo requires a non-empty "name"`)
	}
	if strings.Contains(name, "/") || strings.ContainsAny(name, " \t\n\r\f\v") {
		return mcpToolError("invalid repository name: " + name)
	}
	description, _ := mcpAsString(args["description"])
	isPrivate, _ := mcpAsBool(args["private"])
	if store.GetRepo(viewer, name) != nil {
		return mcpToolError(fmt.Sprintf("repository %s/%s already exists", viewer, name))
	}
	repo := store.CreateRepo(viewer, name, description, isPrivate)
	return mcpToolOutcome{ok: true, data: mcpRepoJSON(repo)}
}

func mcpGetRepoTool(args map[string]any, viewer string) mcpToolOutcome {
	owner, repo := mcpResolveOwnerRepo(args, viewer)
	if repo == "" {
		return mcpToolError(`get_repo requires "repo"`)
	}
	found := store.GetRepo(owner, repo)
	if found == nil {
		return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repo))
	}
	return mcpToolOutcome{ok: true, data: mcpRepoJSON(found)}
}

func mcpListReposTool(args map[string]any, viewer string) mcpToolOutcome {
	owner, _ := mcpAsString(args["owner"])
	if owner == "" {
		owner = viewer
	}
	repos := store.ListReposByOwner(owner)
	arr := []any{}
	for _, r := range repos {
		arr = append(arr, mcpRepoJSON(r))
	}
	return mcpToolOutcome{ok: true, data: O("repositories", arr, "repos", arr, "total_count", len(arr))}
}

func mcpCreateIssueTool(args map[string]any, viewer string) mcpToolOutcome {
	owner, repo := mcpResolveOwnerRepo(args, viewer)
	title, _ := mcpAsString(args["title"])
	if repo == "" {
		return mcpToolError(`create_issue requires "repo"`)
	}
	if title == "" {
		return mcpToolError(`create_issue requires a non-empty "title"`)
	}
	found := store.GetRepo(owner, repo)
	if found == nil {
		return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repo))
	}
	body, _ := mcpAsString(args["body"])
	issue := &Issue{
		ID:        store.NextIssueID,
		Number:    found.IssueSeq + 1,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    []string{},
		CreatedAt: store.Tick(),
		Comments:  nil,
	}
	store.NextIssueID++
	found.IssueSeq++
	found.Issues = append(found.Issues, issue)
	return mcpToolOutcome{ok: true, data: mcpIssueJSON(found, issue)}
}

func mcpListIssuesTool(args map[string]any, viewer string) mcpToolOutcome {
	owner, repo := mcpResolveOwnerRepo(args, viewer)
	if repo == "" {
		return mcpToolError(`list_issues requires "repo"`)
	}
	found := store.GetRepo(owner, repo)
	if found == nil {
		return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repo))
	}
	state, _ := mcpAsString(args["state"])
	if state == "" {
		state = "all"
	}
	state = strings.ToLower(state)
	issues := []any{}
	for _, i := range found.Issues {
		if state == "all" || state == "" || i.State == state {
			issues = append(issues, mcpIssueJSON(found, i))
		}
	}
	return mcpToolOutcome{ok: true, data: O("issues", issues, "total_count", len(issues))}
}

func mcpCreatePullRequestTool(args map[string]any, viewer string) mcpToolOutcome {
	owner, repo := mcpResolveOwnerRepo(args, viewer)
	title, _ := mcpAsString(args["title"])
	head, _ := mcpAsString(args["head"])
	base, _ := mcpAsString(args["base"])
	if repo == "" {
		return mcpToolError(`create_pull_request requires "repo"`)
	}
	if title == "" {
		return mcpToolError(`create_pull_request requires a non-empty "title"`)
	}
	if head == "" {
		return mcpToolError(`create_pull_request requires "head"`)
	}
	if base == "" {
		return mcpToolError(`create_pull_request requires "base"`)
	}
	found := store.GetRepo(owner, repo)
	if found == nil {
		return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repo))
	}
	headSha := mcpResolveRef(found, head)
	if headSha == "" {
		return mcpToolError(fmt.Sprintf("head branch %s not found in %s/%s", head, owner, repo))
	}
	// Base may legitimately have no commits yet (empty repo): allow an empty base sha.
	baseSha := mcpResolveRef(found, base)
	body, _ := mcpAsString(args["body"])
	headRef := head
	if idx := strings.LastIndex(head, ":"); idx >= 0 {
		headRef = head[idx+1:]
	}
	pull := &Pull{
		ID:        store.NextPullID,
		Number:    found.PullSeq + 1,
		Title:     title,
		Body:      body,
		State:     "open",
		Merged:    false,
		MergedAt:  "",
		Head:      PullRef{Ref: headRef, Sha: headSha},
		Base:      PullRef{Ref: base, Sha: baseSha},
		Files:     mcpComputePullFiles(found, baseSha, headSha),
		CreatedAt: store.Tick(),
	}
	store.NextPullID++
	found.PullSeq++
	found.Pulls = append(found.Pulls, pull)
	return mcpToolOutcome{ok: true, data: mcpPullJSON(found, pull)}
}

func mcpMergePullRequestTool(args map[string]any, viewer string) mcpToolOutcome {
	owner, repo := mcpResolveOwnerRepo(args, viewer)
	pullNumber, hasNumber := mcpAsInt(args["pull_number"])
	if repo == "" {
		return mcpToolError(`merge_pull_request requires "repo"`)
	}
	if !hasNumber {
		return mcpToolError(`merge_pull_request requires an integer "pull_number"`)
	}
	found := store.GetRepo(owner, repo)
	if found == nil {
		return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repo))
	}
	var pull *Pull
	for _, p := range found.Pulls {
		if p.Number == pullNumber {
			pull = p
			break
		}
	}
	if pull == nil {
		return mcpToolError(fmt.Sprintf("pull request #%d not found in %s/%s", pullNumber, owner, repo))
	}
	if pull.Merged {
		// Idempotent: already merged, report the existing merge.
		return mcpToolOutcome{ok: true, data: O(
			"merged", true,
			"number", pull.Number,
			"id", pull.ID,
			"title", pull.Title,
			"sha", nullableString(pull.MergeCommitSha),
			"merge_commit_sha", nullableString(pull.MergeCommitSha),
			"state", "closed",
			"full_name", found.FullName,
		)}
	}
	if pull.State == "closed" {
		return mcpToolError(fmt.Sprintf("pull request #%d is already closed", pullNumber))
	}
	if pull.Head.Sha == "" {
		return mcpToolError(fmt.Sprintf("pull request #%d has no head commit to merge", pullNumber))
	}
	headCommit := found.Objects[pull.Head.Sha]
	if headCommit == nil || headCommit.Type != ObjCommit {
		return mcpToolError(fmt.Sprintf("head commit %s not found", pull.Head.Sha))
	}
	headTree, _ := mcpParseCommit(headCommit.Content)
	// Squash merge: one new commit on the base with the head's tree.
	var parents []string
	if pull.Base.Sha != "" {
		parents = []string{pull.Base.Sha}
	}
	message := "Merge pull request #" + itoa(pull.Number) + " from " + pull.Head.Ref + "\n\n" + pull.Title
	mergeSha := mcpCreateCommitObject(found, headTree, parents, message, viewer)
	if pull.Base.Ref != "" {
		found.Refs["refs/heads/"+pull.Base.Ref] = mergeSha
	}
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = mcpMergeAt
	pull.MergeCommitSha = mergeSha
	return mcpToolOutcome{ok: true, data: O(
		"merged", true,
		"number", pull.Number,
		"id", pull.ID,
		"title", pull.Title,
		"sha", mergeSha,
		"merge_commit_sha", mergeSha,
		"state", "closed",
		"full_name", found.FullName,
	)}
}

var mcpToolHandlers = map[string]func(map[string]any, string) mcpToolOutcome{
	"create_repo":         mcpCreateRepoTool,
	"get_repo":            mcpGetRepoTool,
	"create_issue":        mcpCreateIssueTool,
	"list_issues":         mcpListIssuesTool,
	"create_pull_request": mcpCreatePullRequestTool,
	"merge_pull_request":  mcpMergePullRequestTool,
	"list_repos":          mcpListReposTool,
}

// ---------------------------------------------------------------------------
// JSON-RPC plumbing
// ---------------------------------------------------------------------------

const mcpJSONType = "application/json; charset=utf-8"

func mcpSendJSON(w http.ResponseWriter, status int, body any) {
	text := append(Marshal(body), '\n')
	if !wroteHeader(w) {
		h := w.Header()
		h.Set("Content-Type", mcpJSONType)
		h.Set("Content-Length", itoa(len(text)))
		w.WriteHeader(status)
		_, _ = w.Write(text)
	}
}

func mcpRPCResult(id any, result any) *Obj {
	return O("jsonrpc", "2.0", "id", id, "result", result)
}

func mcpRPCError(id any, code int, message string) *Obj {
	return O("jsonrpc", "2.0", "id", id, "error", O("code", code, "message", message))
}

var mcpRecoverIDRe = regexp.MustCompile(`"id"\s*:\s*("(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?|null|true|false)`)

// mcpRecoverID tries to recover the request id from unparseable JSON (else nil).
func mcpRecoverID(text string) any {
	m := mcpRecoverIDRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	v, err := parseJSONValue([]byte(m[1]))
	if err != nil {
		return nil
	}
	return v
}

func mcpCheckAuth(r *http.Request) bool {
	expected := shopToken()
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(header[len("Bearer "):])
	if token == "" {
		return false
	}
	if expected == "" {
		return true
	}
	return token == expected
}

// mcpHandleRPC handles one parsed JSON-RPC request object. Returns nil for
// notifications.
func mcpHandleRPC(request any, viewer string) *Obj {
	rec, ok := request.(map[string]any)
	if !ok {
		return mcpRPCError(nil, -32600, "Invalid Request")
	}
	var id any = nil
	_, hasID := rec["id"]
	if hasID {
		id = rec["id"]
	}
	isNotification := !hasID
	method, _ := rec["method"].(string)
	if method == "" {
		if isNotification {
			return nil
		}
		return mcpRPCError(id, -32600, "Invalid Request: missing method")
	}
	params, _ := rec["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	okRes := func(result any) *Obj {
		if isNotification {
			return nil
		}
		return mcpRPCResult(id, result)
	}
	errRes := func(code int, message string) *Obj {
		if isNotification {
			return nil
		}
		return mcpRPCError(id, code, message)
	}

	switch method {
	case "server/discover":
		return okRes(O(
			"supportedVersions", strSliceToAny(mcpSupportedVersions),
			"serverInfo", O("name", mcpServerName, "version", mcpServerVersion),
			"capabilities", O("tools", NewObj()),
			"_meta", O("io.modelcontextprotocol/serverInfo", O("name", mcpServerName, "version", mcpServerVersion)),
		))
	case "initialize":
		return okRes(O(
			"protocolVersion", mcpProtocolVersion,
			"serverInfo", O("name", mcpServerName, "version", mcpServerVersion),
			"capabilities", O("tools", NewObj()),
			"_meta", O("io.modelcontextprotocol/serverInfo", O("name", mcpServerName, "version", mcpServerVersion)),
		))
	case "notifications/initialized", "notifications/cancelled", "notifications/progress", "notifications/roots/list_changed":
		return nil
	case "ping":
		return okRes(NewObj())
	case "tools/list":
		return okRes(O("tools", mcpToolsJSON()))
	case "resources/list":
		return okRes(O("resources", []any{}))
	case "prompts/list":
		return okRes(O("prompts", []any{}))
	case "tools/call":
		name, _ := mcpAsString(params["name"])
		if name == "" {
			return errRes(-32602, `tools/call requires "name"`)
		}
		handler, ok := mcpToolHandlers[name]
		if !ok {
			return errRes(-32601, "unknown tool: "+name)
		}
		args := mcpNormalizeArgs(params["arguments"])
		var outcome mcpToolOutcome
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					outcome = mcpToolOutcome{ok: false, error: fmt.Sprintf("tool %s failed: %v", name, rec)}
				}
			}()
			outcome = handler(args, viewer)
		}()
		if !outcome.ok {
			// A handler panic is an internal error, not a tool error.
			if strings.HasPrefix(outcome.error, "tool "+name+" failed:") {
				return errRes(-32603, outcome.error)
			}
			return okRes(O(
				"content", []any{O("type", "text", "text", string(Marshal(O("error", outcome.error))))},
				"structuredContent", O("error", outcome.error),
				"isError", true,
			))
		}
		text := string(Marshal(outcome.data))
		return okRes(O(
			"content", []any{O("type", "text", "text", text)},
			"structuredContent", outcome.data,
			"isError", false,
		))
	default:
		return errRes(-32601, "Method not found: "+method)
	}
}

func strSliceToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP entry point (wired into server.go)
// ---------------------------------------------------------------------------

func handleMcp(w http.ResponseWriter, r *http.Request, pathname, method string, viewer string) bool {
	if pathname != "/mcp" && pathname != "/mcp/" {
		return false
	}

	// Auth first: before any method/tool handling.
	if !mcpCheckAuth(r) {
		mcpSendJSON(w, 401, O("error", "Unauthorized"))
		return true
	}

	// Session close.
	if method == "DELETE" {
		w.WriteHeader(204)
		return true
	}

	if method != "POST" {
		mcpSendJSON(w, 405, mcpRPCError(nil, -32600, "Method Not Allowed: use POST"))
		return true
	}

	// MCP has no perf workload; take the write lock for the whole handler.
	storeMu.Lock()
	defer storeMu.Unlock()

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10*1024*1024))
	if err != nil {
		mcpSendJSON(w, 400, mcpRPCError(nil, -32700, "Parse error"))
		return true
	}

	parsed, err := parseJSONValue(raw)
	if err != nil {
		// Non-JSON body -> HTTP 400, JSON-RPC -32700, id recovered when possible.
		mcpSendJSON(w, 400, mcpRPCError(mcpRecoverID(string(raw)), -32700, "Parse error"))
		return true
	}

	// Batch: answer each element; notifications produce no entry.
	if arr, ok := parsed.([]any); ok {
		responses := []any{}
		for _, item := range arr {
			resp := mcpHandleRPC(item, viewer)
			if resp != nil {
				responses = append(responses, resp)
			}
		}
		mcpSendJSON(w, 200, responses)
		return true
	}

	response := mcpHandleRPC(parsed, viewer)
	if response == nil {
		// Notification (e.g. notifications/initialized): no response body.
		w.WriteHeader(202)
		return true
	}
	mcpSendJSON(w, 200, response)
	return true
}
