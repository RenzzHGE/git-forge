// GraphQL v4 surface for the git forge (POST /api/graphql).
//
// A small, dependency-free GraphQL parser + executor over the shared store.
// Handles exactly the documents the suite sends: query/mutation operations,
// variables, inline arguments (string/int/enum/list/input-object/null),
// field aliases, inline fragments, fragment spreads, __type introspection,
// and batched request bodies.
//
// Auth: POST /api/graphql requires `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>`.
// Only an auth failure is a 401; every other outcome (including field errors
// and execution errors) is HTTP 200 with an errors array. A malformed JSON
// body is HTTP 400.

package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Introspection tables (what __type reports)
// ---------------------------------------------------------------------------

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "author", "comments", "createdAt", "url"},
	"PullRequest": {"id", "number", "title", "body", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"User":        {"id", "login"},
	"Query":       {"viewer", "repository", "node", "__type", "__schema"},
	"Mutation":    {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
}

var gqlTypeEnumValues = map[string][]string{
	"SearchType":      {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":      {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":  {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"OrderDirection":  {"ASC", "DESC"},
	"IssueOrderField": {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
}

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type tokenKind string

const (
	tokName   tokenKind = "name"
	tokInt    tokenKind = "int"
	tokFloat  tokenKind = "float"
	tokString tokenKind = "string"
	tokPunct  tokenKind = "punct"
	tokSpread tokenKind = "spread"
	tokEOF    tokenKind = "eof"
)

type gqlToken struct {
	kind  tokenKind
	value string
}

var gqlPunct = map[byte]bool{
	'!': true, '$': true, '(': true, ')': true, ':': true, '=': true,
	'@': true, '[': true, ']': true, '{': true, '}': true, '|': true, '&': true,
}

func gqlLex(src string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		// BOM (U+FEFF) is skipped like whitespace.
		if c == 0xEF && i+2 < n && src[i+1] == 0xBB && src[i+2] == 0xBF {
			i += 3
			continue
		}
		if c == '#' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.' {
			tokens = append(tokens, gqlToken{kind: tokSpread, value: "..."})
			i += 3
			continue
		}
		if gqlPunct[c] {
			tokens = append(tokens, gqlToken{kind: tokPunct, value: string(c)})
			i++
			continue
		}
		if c == '"' {
			if i+2 < n && src[i+1] == '"' && src[i+2] == '"' {
				end := strings.Index(src[i+3:], `"""`)
				if end == -1 {
					return nil, fmt.Errorf("Unterminated block string")
				}
				val := strings.ReplaceAll(src[i+3:i+3+end], `\"""`, `"""`)
				tokens = append(tokens, gqlToken{kind: tokString, value: val})
				i = i + 3 + end + 3
				continue
			}
			j := i + 1
			var out strings.Builder
			for j < n && src[j] != '"' {
				if src[j] == '\\' && j+1 < n {
					e := src[j+1]
					if e == 'u' && j+5 < n {
						cp, err := strconv.ParseUint(src[j+2:j+6], 16, 32)
						if err == nil {
							out.WriteRune(rune(cp))
							j += 6
							continue
						}
					}
					m := map[byte]string{'n': "\n", 't': "\t", 'r': "\r", 'b': "\b", 'f': "\f", '"': `"`, '\\': `\`, '/': "/"}
					if rep, ok := m[e]; ok {
						out.WriteString(rep)
					} else {
						out.WriteByte(e)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			if j >= n {
				return nil, fmt.Errorf("Unterminated string")
			}
			tokens = append(tokens, gqlToken{kind: tokString, value: out.String()})
			i = j + 1
			continue
		}
		if c == '-' || (c >= '0' && c <= '9') {
			j := i
			if src[j] == '-' {
				j++
			}
			for j < n && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			isFloat := false
			if j < n && src[j] == '.' {
				isFloat = true
				j++
				for j < n && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j < n && (src[j] == 'e' || src[j] == 'E') {
				isFloat = true
				j++
				if j < n && (src[j] == '+' || src[j] == '-') {
					j++
				}
				for j < n && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			kind := tokInt
			if isFloat {
				kind = tokFloat
			}
			tokens = append(tokens, gqlToken{kind: kind, value: src[i:j]})
			i = j
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < n {
				d := src[j]
				if (d >= 'A' && d <= 'Z') || (d >= 'a' && d <= 'z') || (d >= '0' && d <= '9') || d == '_' {
					j++
				} else {
					break
				}
			}
			tokens = append(tokens, gqlToken{kind: tokName, value: src[i:j]})
			i = j
			continue
		}
		return nil, fmt.Errorf("Unexpected character %q in GraphQL document", string(c))
	}
	tokens = append(tokens, gqlToken{kind: tokEOF, value: ""})
	return tokens, nil
}

// ---------------------------------------------------------------------------
// Parser (AST)
// ---------------------------------------------------------------------------

type gqlValueNode struct {
	kind   string // Variable | String | Int | Float | Boolean | Enum | Null | List | Object
	name   string
	str    string
	num    float64
	b      bool
	values []*gqlValueNode
	fields []gqlValueField
}

type gqlValueField struct {
	name  string
	value *gqlValueNode
}

type gqlArgument struct {
	name  string
	value *gqlValueNode
}

type gqlSelection struct {
	kind          string // Field | FragmentSpread | InlineFragment
	alias         string
	name          string
	args          []gqlArgument
	selectionSet  []*gqlSelection
	typeCondition string
}

type gqlOperation struct {
	operation           string // query | mutation | subscription
	name                string
	variableDefinitions []gqlVarDef
	selectionSet        []*gqlSelection
}

type gqlVarDef struct {
	name         string
	defaultValue *gqlValueNode
}

type gqlFragment struct {
	name          string
	typeCondition string
	selectionSet  []*gqlSelection
}

type gqlDefinition struct {
	operation *gqlOperation
	fragment  *gqlFragment
}

type gqlParser struct {
	pos    int
	tokens []gqlToken
}

func (p *gqlParser) peek() gqlToken  { return p.tokens[p.pos] }
func (p *gqlParser) next() gqlToken  { t := p.tokens[p.pos]; p.pos++; return t }
func (p *gqlParser) isPunct(v string) bool {
	t := p.peek()
	return t.kind == tokPunct && t.value == v
}

func (p *gqlParser) expectPunct(v string) error {
	t := p.next()
	if t.kind != tokPunct || t.value != v {
		return fmt.Errorf("Expected '%s' but got '%s'", v, t.value)
	}
	return nil
}

func (p *gqlParser) expectName(want ...string) (string, error) {
	t := p.next()
	if t.kind != tokName {
		return "", fmt.Errorf("Expected a name but got '%s'", t.value)
	}
	if len(want) > 0 && t.value != want[0] {
		return "", fmt.Errorf("Expected '%s' but got '%s'", want[0], t.value)
	}
	return t.value, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.isPunct("@") {
		p.next()
		if _, err := p.expectName(); err != nil {
			return err
		}
		if p.isPunct("(") {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseDocument() ([]gqlDefinition, error) {
	var defs []gqlDefinition
	for p.peek().kind != tokEOF {
		def, err := p.parseDefinition()
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
	return defs, nil
}

func (p *gqlParser) parseDefinition() (gqlDefinition, error) {
	t := p.peek()
	if t.kind == tokPunct && t.value == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return gqlDefinition{}, err
		}
		return gqlDefinition{operation: &gqlOperation{operation: "query", selectionSet: sel}}, nil
	}
	if t.kind == tokName && (t.value == "query" || t.value == "mutation" || t.value == "subscription") {
		op := p.next().value
		var name string
		if p.peek().kind == tokName {
			name = p.next().value
		}
		varDefs, err := p.parseVariableDefinitions()
		if err != nil {
			return gqlDefinition{}, err
		}
		if err := p.skipDirectives(); err != nil {
			return gqlDefinition{}, err
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return gqlDefinition{}, err
		}
		return gqlDefinition{operation: &gqlOperation{operation: op, name: name, variableDefinitions: varDefs, selectionSet: sel}}, nil
	}
	if t.kind == tokName && t.value == "fragment" {
		p.next()
		name, err := p.expectName()
		if err != nil {
			return gqlDefinition{}, err
		}
		if _, err := p.expectName("on"); err != nil {
			return gqlDefinition{}, err
		}
		typeCond, err := p.expectName()
		if err != nil {
			return gqlDefinition{}, err
		}
		if err := p.skipDirectives(); err != nil {
			return gqlDefinition{}, err
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return gqlDefinition{}, err
		}
		return gqlDefinition{fragment: &gqlFragment{name: name, typeCondition: typeCond, selectionSet: sel}}, nil
	}
	return gqlDefinition{}, fmt.Errorf("Unexpected token '%s' at start of definition", t.value)
}

func (p *gqlParser) parseVariableDefinitions() ([]gqlVarDef, error) {
	var defs []gqlVarDef
	if !p.isPunct("(") {
		return defs, nil
	}
	p.next()
	for !p.isPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		if err := p.parseType(); err != nil {
			return nil, err
		}
		var defVal *gqlValueNode
		if p.isPunct("=") {
			p.next()
			v, err := p.parseValue(true)
			if err != nil {
				return nil, err
			}
			defVal = v
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		defs = append(defs, gqlVarDef{name: name, defaultValue: defVal})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return defs, nil
}

func (p *gqlParser) parseType() error {
	if p.isPunct("[") {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	if p.isPunct("!") {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	for !p.isPunct("}") {
		if p.peek().kind == tokEOF {
			return nil, fmt.Errorf("Unterminated selection set")
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if p.peek().kind == tokSpread {
		p.next()
		if p.peek().kind == tokName && p.peek().value == "on" {
			p.next()
			typeCond, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{kind: "InlineFragment", typeCondition: typeCond, selectionSet: sel}, nil
		}
		if p.peek().kind == tokName {
			name := p.next().value
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			return &gqlSelection{kind: "FragmentSpread", name: name}, nil
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlSelection{kind: "InlineFragment", selectionSet: sel}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return nil, err
	}
	alias := ""
	name := first
	if p.isPunct(":") {
		p.next()
		alias = first
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	args, err := p.parseArguments()
	if err != nil {
		return nil, err
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	var selectionSet []*gqlSelection
	if p.isPunct("{") {
		selectionSet, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return &gqlSelection{kind: "Field", alias: alias, name: name, args: args, selectionSet: selectionSet}, nil
}

func (p *gqlParser) parseArguments() ([]gqlArgument, error) {
	var args []gqlArgument
	if !p.isPunct("(") {
		return args, nil
	}
	p.next()
	for !p.isPunct(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		val, err := p.parseValue(false)
		if err != nil {
			return nil, err
		}
		args = append(args, gqlArgument{name: name, value: val})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue(isConst bool) (*gqlValueNode, error) {
	t := p.next()
	if t.kind == tokPunct && t.value == "$" {
		if isConst {
			return nil, fmt.Errorf("Variable in constant value")
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: "Variable", name: name}, nil
	}
	if t.kind == tokPunct && t.value == "[" {
		var values []*gqlValueNode
		for !(p.peek().kind == tokPunct && p.peek().value == "]") {
			v, err := p.parseValue(isConst)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		p.next()
		return &gqlValueNode{kind: "List", values: values}, nil
	}
	if t.kind == tokPunct && t.value == "{" {
		var fields []gqlValueField
		for !(p.peek().kind == tokPunct && p.peek().value == "}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue(isConst)
			if err != nil {
				return nil, err
			}
			fields = append(fields, gqlValueField{name: name, value: v})
		}
		p.next()
		return &gqlValueNode{kind: "Object", fields: fields}, nil
	}
	if t.kind == tokString {
		return &gqlValueNode{kind: "String", str: t.value}, nil
	}
	if t.kind == tokInt {
		n, _ := strconv.ParseInt(t.value, 10, 64)
		return &gqlValueNode{kind: "Int", num: float64(n)}, nil
	}
	if t.kind == tokFloat {
		f, _ := strconv.ParseFloat(t.value, 64)
		return &gqlValueNode{kind: "Float", num: f}, nil
	}
	if t.kind == tokName {
		switch t.value {
		case "true":
			return &gqlValueNode{kind: "Boolean", b: true}, nil
		case "false":
			return &gqlValueNode{kind: "Boolean", b: false}, nil
		case "null":
			return &gqlValueNode{kind: "Null"}, nil
		}
		return &gqlValueNode{kind: "Enum", str: t.value}, nil
	}
	return nil, fmt.Errorf("Unexpected value token '%s'", t.value)
}

// ---------------------------------------------------------------------------
// Value resolution (AST value + variables -> Go value)
// ---------------------------------------------------------------------------

type gqlVariables map[string]any

func gqlValueFromAst(node *gqlValueNode, variables gqlVariables) any {
	switch node.kind {
	case "Variable":
		return variables[node.name]
	case "String":
		return node.str
	case "Int":
		return int(node.num)
	case "Float":
		return node.num
	case "Boolean":
		return node.b
	case "Enum":
		return node.str
	case "Null":
		return nil
	case "List":
		out := []any{}
		for _, v := range node.values {
			out = append(out, gqlValueFromAst(v, variables))
		}
		return out
	case "Object":
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = gqlValueFromAst(f.value, variables)
		}
		return out
	}
	return nil
}

func gqlArgsMap(args []gqlArgument, variables gqlVariables) map[string]any {
	out := map[string]any{}
	for _, a := range args {
		out[a.name] = gqlValueFromAst(a.value, variables)
	}
	return out
}

// jsString mirrors JavaScript's String(v): undefined/null -> "" in the
// contexts this executor uses (String(undefined) is "undefined", but the
// Node code only calls String() on values defaulted to '' first).
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return formatFloatJS(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// asInt converts a JSON/AST number to int when it is integral.
func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		if t == float64(int64(t)) {
			return int(t), true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Git object helpers (for PR diffs and squash merges)
// ---------------------------------------------------------------------------

var gqlTreeRe = regexp.MustCompile(`(?m)^tree ([0-9a-f]{40})`)

func gqlParseCommitTreeSha(content []byte) string {
	m := gqlTreeRe.FindSubmatch(content)
	if m == nil {
		return ""
	}
	return string(m[1])
}

type gqlTreeEntry struct {
	mode string
	name string
	sha  string
}

func gqlParseTree(content []byte) []gqlTreeEntry {
	var entries []gqlTreeEntry
	i := 0
	for i < len(content) {
		space := indexByte(content, 0x20, i)
		if space == -1 {
			break
		}
		mode := string(content[i:space])
		nul := indexByte(content, 0x00, space+1)
		if nul == -1 {
			break
		}
		name := string(content[space+1 : nul])
		sha := hexEncode(content[nul+1 : nul+21])
		entries = append(entries, gqlTreeEntry{mode: mode, name: name, sha: sha})
		i = nul + 21
	}
	return entries
}

func gqlFlattenTree(repo *Repo, treeSha, prefix string, out map[string]string) {
	obj := repo.Objects[treeSha]
	if obj == nil || obj.Type != ObjTree {
		return
	}
	for _, entry := range gqlParseTree(obj.Content) {
		path := entry.name
		if prefix != "" {
			path = prefix + "/" + entry.name
		}
		if entry.mode == "40000" {
			gqlFlattenTree(repo, entry.sha, path, out)
		} else {
			out[path] = entry.sha
		}
	}
}

func gqlTreeOfCommit(repo *Repo, commitSha string) map[string]string {
	out := map[string]string{}
	if commitSha == "" {
		return out
	}
	obj := repo.Objects[commitSha]
	if obj == nil || obj.Type != ObjCommit {
		return out
	}
	treeSha := gqlParseCommitTreeSha(obj.Content)
	if treeSha == "" {
		return out
	}
	gqlFlattenTree(repo, treeSha, "", out)
	return out
}

func gqlDiffFiles(repo *Repo, baseSha, headSha string) []*PullFile {
	base := gqlTreeOfCommit(repo, baseSha)
	head := gqlTreeOfCommit(repo, headSha)
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
			files = append(files, &PullFile{Filename: path, Status: "added", Additions: 1, Deletions: 0, Changes: 1})
		} else if bOk && !hOk {
			files = append(files, &PullFile{Filename: path, Status: "removed", Additions: 0, Deletions: 1, Changes: 1})
		} else {
			files = append(files, &PullFile{Filename: path, Status: "modified", Additions: 1, Deletions: 1, Changes: 1})
		}
	}
	return files
}

// ---------------------------------------------------------------------------
// Store lookups
// ---------------------------------------------------------------------------

func gqlFindRepoByNumericID(id int) *Repo {
	for _, repo := range store.repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func gqlFindIssueByGlobalID(id string) (*Repo, *Issue) {
	parsed := ParseGlobalID(id)
	if parsed == nil || parsed.Kind != "Issue" {
		return nil, nil
	}
	for _, repo := range store.repos {
		for _, issue := range repo.Issues {
			if issue.ID == parsed.NumericID {
				return repo, issue
			}
		}
	}
	return nil, nil
}

func gqlFindPullByGlobalID(id string) (*Repo, *Pull) {
	parsed := ParseGlobalID(id)
	if parsed == nil || parsed.Kind != "PullRequest" {
		return nil, nil
	}
	for _, repo := range store.repos {
		for _, pull := range repo.Pulls {
			if pull.ID == parsed.NumericID {
				return repo, pull
			}
		}
	}
	return nil, nil
}

func gqlFindRepoByGlobalID(id string) *Repo {
	parsed := ParseGlobalID(id)
	if parsed == nil || parsed.Kind != "Repository" {
		return nil
	}
	return gqlFindRepoByNumericID(parsed.NumericID)
}

func gqlStateOut(state string) string {
	if state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

// ---------------------------------------------------------------------------
// Cursors + issue connection
// ---------------------------------------------------------------------------

func gqlEncodeCursor(number int) string {
	return base64Encode("cursor:" + itoa(number))
}

func base64Encode(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	b := []byte(s)
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		n = uint32(b[i]) << 16
		if rem > 1 {
			n |= uint32(b[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(b[i+2])
		}
		out.WriteByte(alphabet[(n>>18)&63])
		out.WriteByte(alphabet[(n>>12)&63])
		if rem > 1 {
			out.WriteByte(alphabet[(n>>6)&63])
		} else {
			out.WriteByte('=')
		}
		if rem > 2 {
			out.WriteByte(alphabet[n&63])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

var gqlCursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func gqlDecodeCursor(after any) (int, bool) {
	s, ok := after.(string)
	if !ok || s == "" {
		return 0, false
	}
	raw, err := lenientB64DecodeRaw(s)
	if err != nil {
		return 0, false
	}
	m := gqlCursorRe.FindStringSubmatch(string(raw))
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

func lenientB64DecodeRaw(s string) ([]byte, error) {
	filtered := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			filtered = append(filtered, c)
		}
	}
	for len(filtered)%4 != 0 {
		filtered = append(filtered, '=')
	}
	decoded, err := base64DecodeStd(string(filtered))
	if err == nil {
		return decoded, nil
	}
	trimmed := strings.TrimRight(string(filtered), "=")
	return base64DecodeRaw(trimmed)
}

func base64DecodeStd(s string) ([]byte, error) {
	// Standard base64 decode (with padding).
	if len(s)%4 != 0 {
		return nil, fmt.Errorf("illegal base64 length")
	}
	var out []byte
	val := func(c byte) (int, bool) {
		switch {
		case c >= 'A' && c <= 'Z':
			return int(c - 'A'), true
		case c >= 'a' && c <= 'z':
			return int(c-'a') + 26, true
		case c >= '0' && c <= '9':
			return int(c-'0') + 52, true
		case c == '+':
			return 62, true
		case c == '/':
			return 63, true
		}
		return 0, false
	}
	for i := 0; i < len(s); i += 4 {
		var n uint32
		pad := 0
		for j := 0; j < 4; j++ {
			c := s[i+j]
			if c == '=' {
				pad++
				n <<= 6
				continue
			}
			v, ok := val(c)
			if !ok {
				return nil, fmt.Errorf("illegal base64 char")
			}
			n = n<<6 | uint32(v)
		}
		if pad == 0 {
			out = append(out, byte(n>>16), byte(n>>8), byte(n))
		} else if pad == 1 {
			out = append(out, byte(n>>16), byte(n>>8))
		} else if pad == 2 {
			out = append(out, byte(n>>16))
		} else {
			return nil, fmt.Errorf("illegal base64 padding")
		}
	}
	return out, nil
}

func base64DecodeRaw(s string) ([]byte, error) {
	// Raw (unpadded) base64 decode.
	if s == "" {
		return []byte{}, nil
	}
	padded := s
	for len(padded)%4 != 0 {
		padded += "="
	}
	return base64DecodeStd(padded)
}

type gqlPageInfo struct {
	HasNextPage     bool
	EndCursor       any
	HasPreviousPage bool
	StartCursor     any
}

type gqlConnection struct {
	Nodes    []any
	PageInfo *gqlPageInfo
}

func gqlBuildIssueConnection(repo *Repo, args map[string]any) *gqlConnection {
	list := make([]*Issue, len(repo.Issues))
	copy(list, repo.Issues)
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, s := range states {
			wanted[strings.ToLower(jsString(s))] = true
		}
		var filtered []*Issue
		for _, i := range list {
			if wanted[i.State] {
				filtered = append(filtered, i)
			}
		}
		list = filtered
	}
	direction := "ASC"
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		direction = strings.ToUpper(jsString(orderBy["direction"]))
		if direction == "" {
			direction = "ASC"
		}
	}
	sort.SliceStable(list, func(a, b int) bool {
		if direction == "DESC" {
			return list[a].CreatedAt > list[b].CreatedAt
		}
		return list[a].CreatedAt < list[b].CreatedAt
	})
	afterNum, hasAfter := gqlDecodeCursor(args["after"])
	if hasAfter {
		idx := -1
		for i, iss := range list {
			if iss.Number == afterNum {
				idx = i
				break
			}
		}
		if idx >= 0 {
			list = list[idx+1:]
		}
	}
	first, hasFirst := asInt(args["first"])
	hasNextPage := hasFirst && len(list) > first
	var page []*Issue
	if hasFirst {
		if first < len(list) {
			page = list[:first]
		} else {
			page = list
		}
	} else {
		page = list
	}
	var endCursor, startCursor any
	if len(page) > 0 {
		endCursor = gqlEncodeCursor(page[len(page)-1].Number)
		startCursor = gqlEncodeCursor(page[0].Number)
	}
	nodes := make([]any, len(page))
	for i, iss := range page {
		nodes[i] = iss
	}
	return &gqlConnection{
		Nodes:    nodes,
		PageInfo: &gqlPageInfo{HasNextPage: hasNextPage, EndCursor: endCursor, HasPreviousPage: hasAfter, StartCursor: startCursor},
	}
}

// uuidV4 mirrors Node's randomUUID (version 4).
func uuidV4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type gqlFieldError struct {
	message string
	path    []any
}

func (e *gqlFieldError) Error() string { return e.message }

type gqlExecContext struct {
	viewerLogin string
	variables   gqlVariables
	fragments   map[string]*gqlFragment
}

type gqlResolved struct {
	value any
	typ   string // GraphQL type of an object value; "" for scalars/lists handled by caller
}

func gqlUnknownField(typeName, field string, path []any) *gqlFieldError {
	p := append(append([]any{}, path...), field)
	return &gqlFieldError{message: fmt.Sprintf("Cannot query field %q on type %q.", field, typeName), path: p}
}

// Parent value types.
type gqlUser struct{ login string }
type gqlRef struct {
	name string
	repo *Repo
}
type gqlGitObject struct{ oid string }
type gqlIssuePayload struct{ issue *Issue }
type gqlPullPayload struct{ pull *Pull }
type gqlCommentNode struct {
	id   string
	body string
}
type gqlCommentEdge struct{ node *gqlCommentNode }
type gqlAddCommentPayload struct{ commentEdge *gqlCommentEdge }
type gqlPullFileNode struct {
	path      string
	additions int
	deletions int
	changes   int
	status    string
}
type gqlTypeVal struct {
	name       string
	kind       string
	fields     []any // []gqlFieldVal or nil
	enumValues []any // []gqlEnumVal or nil
}
type gqlFieldVal struct{ name string }
type gqlEnumVal struct{ name string }
type gqlSchemaVal struct {
	queryType    *gqlTypeRefVal
	mutationType *gqlTypeRefVal
}
type gqlTypeRefVal struct{ name string }
type gqlEdgeParent struct {
	cursor    string
	node      any
	innerType string
}

func gqlResolveField(ctx *gqlExecContext, parentType string, parent any, field *gqlSelection, args map[string]any, path []any) (gqlResolved, error) {
	name := field.name

	// Meta fields available on every type.
	if name == "__typename" {
		return gqlResolved{value: parentType}, nil
	}

	switch parentType {
	case "Query":
		if name == "viewer" {
			return gqlResolved{value: &gqlUser{login: ctx.viewerLogin}, typ: "User"}, nil
		}
		if name == "repository" {
			repo := store.GetRepo(jsString(args["owner"]), jsString(args["name"]))
			if repo == nil {
				return gqlResolved{value: nil, typ: "Repository"}, nil
			}
			return gqlResolved{value: repo, typ: "Repository"}, nil
		}
		if name == "node" {
			parsed := ParseGlobalID(jsString(args["id"]))
			if parsed == nil {
				return gqlResolved{value: nil}, nil
			}
			switch parsed.Kind {
			case "Repository":
				repo := gqlFindRepoByNumericID(parsed.NumericID)
				if repo == nil {
					return gqlResolved{value: nil, typ: "Repository"}, nil
				}
				return gqlResolved{value: repo, typ: "Repository"}, nil
			case "Issue":
				_, issue := gqlFindIssueByGlobalID(jsString(args["id"]))
				if issue == nil {
					return gqlResolved{value: nil, typ: "Issue"}, nil
				}
				return gqlResolved{value: issue, typ: "Issue"}, nil
			case "PullRequest":
				_, pull := gqlFindPullByGlobalID(jsString(args["id"]))
				if pull == nil {
					return gqlResolved{value: nil, typ: "PullRequest"}, nil
				}
				return gqlResolved{value: pull, typ: "PullRequest"}, nil
			}
			return gqlResolved{value: nil}, nil
		}
		if name == "__type" {
			typeName := jsString(args["name"])
			fields, hasFields := gqlTypeFields[typeName]
			enums, hasEnums := gqlTypeEnumValues[typeName]
			kind := "OBJECT"
			if hasEnums {
				kind = "ENUM"
			}
			var fieldVals []any
			if hasFields {
				for _, f := range fields {
					fieldVals = append(fieldVals, &gqlFieldVal{name: f})
				}
			}
			var enumVals []any
			if hasEnums {
				for _, e := range enums {
					enumVals = append(enumVals, &gqlEnumVal{name: e})
				}
			}
			return gqlResolved{value: &gqlTypeVal{name: typeName, kind: kind, fields: fieldVals, enumValues: enumVals}, typ: "__Type"}, nil
		}
		if name == "__schema" {
			return gqlResolved{value: &gqlSchemaVal{queryType: &gqlTypeRefVal{name: "Query"}, mutationType: &gqlTypeRefVal{name: "Mutation"}}, typ: "__Schema"}, nil
		}
		return gqlResolved{}, gqlUnknownField("Query", name, path)

	case "Mutation":
		if name == "createIssue" {
			input, _ := args["input"].(map[string]any)
			if input == nil {
				input = map[string]any{}
			}
			repo := gqlFindRepoByGlobalID(jsString(input["repositoryId"]))
			if repo == nil {
				return gqlResolved{}, &gqlFieldError{message: fmt.Sprintf("Repository not found for id %q.", jsString(input["repositoryId"])), path: append(append([]any{}, path...), name)}
			}
			issue := &Issue{
				ID:        store.NextIssueID,
				Number:    repo.IssueSeq + 1,
				Title:     jsString(input["title"]),
				Body:      stringOr(input["body"], ""),
				State:     "open",
				Labels:    []string{},
				CreatedAt: store.Tick(),
				Comments:  nil,
			}
			store.NextIssueID++
			repo.IssueSeq++
			repo.Issues = append(repo.Issues, issue)
			return gqlResolved{value: &gqlIssuePayload{issue: issue}, typ: "CreateIssuePayload"}, nil
		}
		if name == "closeIssue" {
			input, _ := args["input"].(map[string]any)
			if input == nil {
				input = map[string]any{}
			}
			_, issue := gqlFindIssueByGlobalID(jsString(input["issueId"]))
			if issue == nil {
				return gqlResolved{}, &gqlFieldError{message: fmt.Sprintf("Issue not found for id %q.", jsString(input["issueId"])), path: append(append([]any{}, path...), name)}
			}
			issue.State = "closed"
			bumpIssueCache()
			return gqlResolved{value: &gqlIssuePayload{issue: issue}, typ: "CloseIssuePayload"}, nil
		}
		if name == "createPullRequest" {
			input, _ := args["input"].(map[string]any)
			if input == nil {
				input = map[string]any{}
			}
			repo := gqlFindRepoByGlobalID(jsString(input["repositoryId"]))
			if repo == nil {
				return gqlResolved{}, &gqlFieldError{message: fmt.Sprintf("Repository not found for id %q.", jsString(input["repositoryId"])), path: append(append([]any{}, path...), name)}
			}
			headRef := jsString(input["headRefName"])
			baseRef := jsString(input["baseRefName"])
			headSha := repo.Refs["refs/heads/"+headRef]
			baseSha := repo.Refs["refs/heads/"+baseRef]
			pull := &Pull{
				ID:        store.NextPullID,
				Number:    repo.PullSeq + 1,
				Title:     jsString(input["title"]),
				Body:      stringOr(input["body"], ""),
				State:     "open",
				Merged:    false,
				MergedAt:  "",
				Head:      PullRef{Ref: headRef, Sha: headSha},
				Base:      PullRef{Ref: baseRef, Sha: baseSha},
				Files:     gqlDiffFiles(repo, baseSha, headSha),
				CreatedAt: store.Tick(),
			}
			store.NextPullID++
			repo.PullSeq++
			repo.Pulls = append(repo.Pulls, pull)
			return gqlResolved{value: &gqlPullPayload{pull: pull}, typ: "CreatePullRequestPayload"}, nil
		}
		if name == "mergePullRequest" {
			input, _ := args["input"].(map[string]any)
			if input == nil {
				input = map[string]any{}
			}
			repo, pull := gqlFindPullByGlobalID(jsString(input["pullRequestId"]))
			if pull == nil {
				return gqlResolved{}, &gqlFieldError{message: fmt.Sprintf("Pull request not found for id %q.", jsString(input["pullRequestId"])), path: append(append([]any{}, path...), name)}
			}
			if !pull.Merged {
				headTree := gqlTreeOfCommit(repo, pull.Head.Sha)
				baseSha := pull.Base.Sha
				newSha := baseSha
				if len(headTree) > 0 || baseSha != "" {
					var headTreeSha string
					if headObj := repo.Objects[pull.Head.Sha]; headObj != nil && headObj.Type == ObjCommit {
						headTreeSha = gqlParseCommitTreeSha(headObj.Content)
					}
					if headTreeSha != "" {
						message := "Merge pull request #" + itoa(pull.Number) + "\n\n" + pull.Title + "\n"
						lines := []string{"tree " + headTreeSha}
						if baseSha != "" {
							lines = append(lines, "parent "+baseSha)
						}
						lines = append(lines, "author "+ctx.viewerLogin+" <"+ctx.viewerLogin+"@example.com> 0 +0000")
						lines = append(lines, "committer "+ctx.viewerLogin+" <"+ctx.viewerLogin+"@example.com> 0 +0000")
						lines = append(lines, "", message)
						content := []byte(strings.Join(lines, "\n"))
						newSha = GitObjectID(ObjCommit, content)
						repo.Objects[newSha] = &GitObject{Type: ObjCommit, Content: content}
						repo.Refs["refs/heads/"+pull.Base.Ref] = newSha
					}
				}
				pull.Merged = true
				pull.MergedAt = "2026-01-01T00:00:00.000Z"
				pull.MergeCommitSha = newSha
				pull.State = "closed"
			}
			return gqlResolved{value: &gqlPullPayload{pull: pull}, typ: "MergePullRequestPayload"}, nil
		}
		if name == "addComment" {
			input, _ := args["input"].(map[string]any)
			if input == nil {
				input = map[string]any{}
			}
			_, issue := gqlFindIssueByGlobalID(jsString(input["subjectId"]))
			if issue == nil {
				return gqlResolved{}, &gqlFieldError{message: fmt.Sprintf("Issue not found for id %q.", jsString(input["subjectId"])), path: append(append([]any{}, path...), name)}
			}
			commentID := store.NextCommentID
			store.NextCommentID++
			comment := &Comment{ID: commentID, Body: jsString(input["body"]), CreatedAt: store.Tick()}
			issue.Comments = append(issue.Comments, comment)
			bumpIssueCache()
			return gqlResolved{value: &gqlAddCommentPayload{commentEdge: &gqlCommentEdge{node: &gqlCommentNode{id: GlobalID("Comment", commentID), body: comment.Body}}}, typ: "AddCommentPayload"}, nil
		}
		return gqlResolved{}, gqlUnknownField("Mutation", name, path)

	case "Repository":
		repo := parent.(*Repo)
		switch name {
		case "id":
			return gqlResolved{value: GlobalID("Repository", repo.ID)}, nil
		case "databaseId":
			return gqlResolved{value: repo.ID}, nil
		case "name":
			return gqlResolved{value: repo.Name}, nil
		case "nameWithOwner":
			return gqlResolved{value: repo.FullName}, nil
		case "owner":
			return gqlResolved{value: &gqlUser{login: repo.Owner}, typ: "User"}, nil
		case "defaultBranchRef":
			return gqlResolved{value: &gqlRef{name: repo.DefaultBranch, repo: repo}, typ: "Ref"}, nil
		case "viewerPermission":
			return gqlResolved{value: "ADMIN"}, nil
		case "hasIssuesEnabled":
			return gqlResolved{value: true}, nil
		case "issue":
			num, _ := asInt(args["number"])
			var found *Issue
			for _, i := range repo.Issues {
				if i.Number == num {
					found = i
					break
				}
			}
			if found == nil {
				return gqlResolved{value: nil, typ: "Issue"}, nil
			}
			return gqlResolved{value: found, typ: "Issue"}, nil
		case "issues":
			return gqlResolved{value: gqlBuildIssueConnection(repo, args), typ: "IssueConnection"}, nil
		case "pullRequests":
			first, hasFirst := asInt(args["first"])
			var nodes []any
			if hasFirst {
				n := first
				if n > len(repo.Pulls) {
					n = len(repo.Pulls)
				}
				for _, p := range repo.Pulls[:n] {
					nodes = append(nodes, p)
				}
			} else {
				for _, p := range repo.Pulls {
					nodes = append(nodes, p)
				}
			}
			return gqlResolved{value: &gqlConnection{Nodes: nodes, PageInfo: &gqlPageInfo{HasNextPage: hasFirst && len(repo.Pulls) > first, EndCursor: nil, HasPreviousPage: false, StartCursor: nil}}, typ: "PullRequestConnection"}, nil
		case "pullRequest":
			num, _ := asInt(args["number"])
			var found *Pull
			for _, p := range repo.Pulls {
				if p.Number == num {
					found = p
					break
				}
			}
			if found == nil {
				return gqlResolved{value: nil, typ: "PullRequest"}, nil
			}
			return gqlResolved{value: found, typ: "PullRequest"}, nil
		}
		return gqlResolved{}, gqlUnknownField("Repository", name, path)

	case "Issue":
		issue := parent.(*Issue)
		switch name {
		case "id":
			return gqlResolved{value: GlobalID("Issue", issue.ID)}, nil
		case "number":
			return gqlResolved{value: issue.Number}, nil
		case "title":
			return gqlResolved{value: issue.Title}, nil
		case "state":
			return gqlResolved{value: gqlStateOut(issue.State)}, nil
		case "body":
			return gqlResolved{value: issue.Body}, nil
		case "author":
			return gqlResolved{value: &gqlUser{login: ctx.viewerLogin}, typ: "User"}, nil
		case "createdAt":
			return gqlResolved{value: itoa(issue.CreatedAt)}, nil
		case "url":
			return gqlResolved{value: ""}, nil
		case "comments":
			first, hasFirst := asInt(args["first"])
			comments := issue.Comments
			if hasFirst && first < len(comments) {
				comments = comments[:first]
			}
			nodes := []any{}
			for _, c := range comments {
				nodes = append(nodes, &gqlCommentNode{id: GlobalID("Comment", c.ID), body: c.Body})
			}
			return gqlResolved{value: &gqlConnection{Nodes: nodes, PageInfo: &gqlPageInfo{HasNextPage: false, EndCursor: nil, HasPreviousPage: false, StartCursor: nil}}, typ: "CommentConnection"}, nil
		}
		return gqlResolved{}, gqlUnknownField("Issue", name, path)

	case "PullRequest":
		pull := parent.(*Pull)
		switch name {
		case "id":
			return gqlResolved{value: GlobalID("PullRequest", pull.ID)}, nil
		case "number":
			return gqlResolved{value: pull.Number}, nil
		case "title":
			return gqlResolved{value: pull.Title}, nil
		case "body":
			return gqlResolved{value: pull.Body}, nil
		case "state":
			return gqlResolved{value: gqlStateOut(pull.State)}, nil
		case "headRefName":
			return gqlResolved{value: pull.Head.Ref}, nil
		case "baseRefName":
			return gqlResolved{value: pull.Base.Ref}, nil
		case "mergeable":
			return gqlResolved{value: "MERGEABLE"}, nil
		case "merged":
			return gqlResolved{value: pull.Merged}, nil
		case "files":
			first, hasFirst := asInt(args["first"])
			files := pull.Files
			if hasFirst && first < len(files) {
				files = files[:first]
			}
			nodes := []any{}
			for _, f := range files {
				nodes = append(nodes, &gqlPullFileNode{path: f.Filename, additions: f.Additions, deletions: f.Deletions, changes: f.Changes, status: f.Status})
			}
			return gqlResolved{value: &gqlConnection{Nodes: nodes, PageInfo: &gqlPageInfo{HasNextPage: false, EndCursor: nil, HasPreviousPage: false, StartCursor: nil}}, typ: "PullRequestFileConnection"}, nil
		}
		return gqlResolved{}, gqlUnknownField("PullRequest", name, path)

	case "User":
		user := parent.(*gqlUser)
		if name == "login" {
			return gqlResolved{value: user.login}, nil
		}
		if name == "id" {
			return gqlResolved{value: GlobalID("User", 0)}, nil
		}
		return gqlResolved{}, gqlUnknownField("User", name, path)

	case "Ref":
		ref := parent.(*gqlRef)
		if name == "name" {
			return gqlResolved{value: ref.name}, nil
		}
		if name == "target" {
			sha := ref.repo.Refs["refs/heads/"+ref.name]
			return gqlResolved{value: &gqlGitObject{oid: sha}, typ: "GitObject"}, nil
		}
		return gqlResolved{}, gqlUnknownField("Ref", name, path)

	case "GitObject":
		obj := parent.(*gqlGitObject)
		if name == "oid" {
			return gqlResolved{value: obj.oid}, nil
		}
		return gqlResolved{}, gqlUnknownField("GitObject", name, path)

	case "IssueConnection", "PullRequestConnection", "CommentConnection", "PullRequestFileConnection":
		conn := parent.(*gqlConnection)
		itemType := ""
		switch parentType {
		case "IssueConnection":
			itemType = "Issue"
		case "PullRequestConnection":
			itemType = "PullRequest"
		case "CommentConnection":
			itemType = "Comment"
		case "PullRequestFileConnection":
			itemType = "PullRequestFile"
		}
		if name == "nodes" {
			return gqlResolved{value: conn.Nodes, typ: "List:" + itemType}, nil
		}
		if name == "edges" {
			edges := []any{}
			for i, n := range conn.Nodes {
				cursor := 0
				switch v := n.(type) {
				case *Issue:
					cursor = v.Number
				case *Pull:
					cursor = v.Number
				default:
					cursor = i + 1
				}
				edges = append(edges, &gqlEdgeParent{cursor: gqlEncodeCursor(cursor), node: n})
			}
			return gqlResolved{value: edges, typ: "List:Edge:" + itemType}, nil
		}
		if name == "pageInfo" {
			return gqlResolved{value: conn.PageInfo, typ: "PageInfo"}, nil
		}
		if name == "totalCount" {
			return gqlResolved{value: len(conn.Nodes)}, nil
		}
		return gqlResolved{}, gqlUnknownField(parentType, name, path)

	case "PageInfo":
		info := parent.(*gqlPageInfo)
		switch name {
		case "hasNextPage":
			return gqlResolved{value: info.HasNextPage}, nil
		case "endCursor":
			return gqlResolved{value: info.EndCursor}, nil
		case "hasPreviousPage":
			return gqlResolved{value: info.HasPreviousPage}, nil
		case "startCursor":
			return gqlResolved{value: info.StartCursor}, nil
		}
		return gqlResolved{}, gqlUnknownField("PageInfo", name, path)

	case "CreateIssuePayload", "CloseIssuePayload":
		payload := parent.(*gqlIssuePayload)
		if name == "issue" {
			return gqlResolved{value: payload.issue, typ: "Issue"}, nil
		}
		return gqlResolved{}, gqlUnknownField(parentType, name, path)

	case "CreatePullRequestPayload", "MergePullRequestPayload":
		payload := parent.(*gqlPullPayload)
		if name == "pullRequest" {
			return gqlResolved{value: payload.pull, typ: "PullRequest"}, nil
		}
		return gqlResolved{}, gqlUnknownField(parentType, name, path)

	case "AddCommentPayload":
		payload := parent.(*gqlAddCommentPayload)
		if name == "commentEdge" {
			return gqlResolved{value: payload.commentEdge, typ: "IssueCommentEdge"}, nil
		}
		return gqlResolved{}, gqlUnknownField("AddCommentPayload", name, path)

	case "IssueCommentEdge":
		edge := parent.(*gqlCommentEdge)
		if name == "node" {
			return gqlResolved{value: edge.node, typ: "Comment"}, nil
		}
		if name == "cursor" {
			return gqlResolved{value: ""}, nil
		}
		return gqlResolved{}, gqlUnknownField("IssueCommentEdge", name, path)

	case "Comment":
		comment := parent.(*gqlCommentNode)
		if name == "id" {
			return gqlResolved{value: comment.id}, nil
		}
		if name == "body" {
			return gqlResolved{value: comment.body}, nil
		}
		return gqlResolved{}, gqlUnknownField("Comment", name, path)

	case "PullRequestFile":
		file := parent.(*gqlPullFileNode)
		switch name {
		case "path":
			return gqlResolved{value: file.path}, nil
		case "additions":
			return gqlResolved{value: file.additions}, nil
		case "deletions":
			return gqlResolved{value: file.deletions}, nil
		case "changes":
			return gqlResolved{value: file.changes}, nil
		case "status":
			return gqlResolved{value: file.status}, nil
		}
		return gqlResolved{}, gqlUnknownField("PullRequestFile", name, path)

	case "__Type":
		t := parent.(*gqlTypeVal)
		switch name {
		case "name":
			return gqlResolved{value: t.name}, nil
		case "kind":
			return gqlResolved{value: t.kind}, nil
		case "fields":
			if t.fields == nil {
				return gqlResolved{value: nil}, nil
			}
			return gqlResolved{value: t.fields, typ: "List:__Field"}, nil
		case "enumValues":
			if t.enumValues == nil {
				return gqlResolved{value: nil}, nil
			}
			return gqlResolved{value: t.enumValues, typ: "List:__EnumValue"}, nil
		}
		return gqlResolved{}, gqlUnknownField("__Type", name, path)

	case "__Field", "__EnumValue":
		f := parent.(interface{ getName() string })
		if name == "name" {
			return gqlResolved{value: f.getName()}, nil
		}
		return gqlResolved{}, gqlUnknownField(parentType, name, path)

	case "__Schema":
		s := parent.(*gqlSchemaVal)
		if name == "queryType" {
			return gqlResolved{value: s.queryType, typ: "__TypeRef"}, nil
		}
		if name == "mutationType" {
			return gqlResolved{value: s.mutationType, typ: "__TypeRef"}, nil
		}
		return gqlResolved{}, gqlUnknownField("__Schema", name, path)

	case "__TypeRef":
		r := parent.(*gqlTypeRefVal)
		if name == "name" {
			return gqlResolved{value: r.name}, nil
		}
		return gqlResolved{}, gqlUnknownField("__TypeRef", name, path)

	default:
		// Generic fallback: plain objects resolve their own keys.
		if obj, ok := parent.(map[string]any); ok && obj != nil {
			if v, exists := obj[name]; exists {
				if name == "node" && v != nil {
					if _, isObj := v.(map[string]any); isObj {
						return gqlResolved{value: v, typ: "Comment"}, nil
					}
				}
				return gqlResolved{value: v}, nil
			}
		}
		return gqlResolved{}, gqlUnknownField(parentType, name, path)
	}
}

func (f *gqlFieldVal) getName() string { return f.name }
func (f *gqlEnumVal) getName() string  { return f.name }

// gqlExecuteSelectionSet resolves a selection set against a parent value of
// the given type.
func gqlExecuteSelectionSet(ctx *gqlExecContext, parentType string, parent any, selections []*gqlSelection, path []any) (*Obj, error) {
	out := NewObj()
	for _, sel := range selections {
		if sel.kind == "FragmentSpread" {
			frag, ok := ctx.fragments[sel.name]
			if !ok {
				return nil, &gqlFieldError{message: fmt.Sprintf("Unknown fragment %q.", sel.name), path: path}
			}
			if frag.typeCondition != parentType && !(frag.typeCondition == "Node" && (parentType == "Repository" || parentType == "Issue" || parentType == "PullRequest")) {
				continue
			}
			sub, err := gqlExecuteSelectionSet(ctx, parentType, parent, frag.selectionSet, path)
			if err != nil {
				return nil, err
			}
			mergeObj(out, sub)
			continue
		}
		if sel.kind == "InlineFragment" {
			if sel.typeCondition != "" && sel.typeCondition != parentType {
				continue
			}
			sub, err := gqlExecuteSelectionSet(ctx, parentType, parent, sel.selectionSet, path)
			if err != nil {
				return nil, err
			}
			mergeObj(out, sub)
			continue
		}
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		args := gqlArgsMap(sel.args, ctx.variables)
		resolved, err := gqlResolveField(ctx, parentType, parent, sel, args, path)
		if err != nil {
			return nil, err
		}
		val, err := gqlCompleteValue(ctx, resolved, sel.selectionSet, append(append([]any{}, path...), key))
		if err != nil {
			return nil, err
		}
		out.Set(key, val)
	}
	return out, nil
}

func mergeObj(dst, src *Obj) {
	for i, k := range src.keys {
		dst.Set(k, src.vals[i])
	}
}

// gqlCompleteValue completes a resolved value: applies a sub-selection, or
// returns the scalar as-is.
func gqlCompleteValue(ctx *gqlExecContext, resolved gqlResolved, selectionSet []*gqlSelection, path []any) (any, error) {
	value, typ := resolved.value, resolved.typ
	if value == nil {
		return nil, nil
	}
	if selectionSet == nil {
		return value, nil
	}
	if arr, ok := value.([]any); ok {
		itemType := ""
		if strings.HasPrefix(typ, "List:") {
			itemType = typ[len("List:"):]
		}
		if strings.HasPrefix(itemType, "Edge:") {
			inner := itemType[len("Edge:"):]
			out := []any{}
			for i, item := range arr {
				edge := item.(*gqlEdgeParent)
				edge.innerType = inner
				v, err := gqlExecuteSelectionSetWithEdges(ctx, "Edge", edge, selectionSet, append(append([]any{}, path...), i))
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}
		if itemType == "" {
			return value, nil
		}
		out := []any{}
		for i, item := range arr {
			v, err := gqlExecuteSelectionSet(ctx, itemType, item, selectionSet, append(append([]any{}, path...), i))
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	if typ == "" {
		return nil, &gqlFieldError{message: fmt.Sprintf("Field at %s has a sub-selection but no object type.", gqlPathString(path)), path: path}
	}
	return gqlExecuteSelectionSet(ctx, typ, value, selectionSet, path)
}

func gqlPathString(path []any) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = fmt.Sprintf("%v", p)
	}
	return strings.Join(parts, ".")
}

func gqlExecuteEdgeSelectionSet(ctx *gqlExecContext, parent *gqlEdgeParent, selections []*gqlSelection, path []any) (*Obj, error) {
	out := NewObj()
	for _, sel := range selections {
		if sel.kind != "Field" {
			continue
		}
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		if sel.name == "node" {
			if sel.selectionSet != nil {
				v, err := gqlExecuteSelectionSet(ctx, parent.innerType, parent.node, sel.selectionSet, append(append([]any{}, path...), key))
				if err != nil {
					return nil, err
				}
				out.Set(key, v)
			} else {
				out.Set(key, parent.node)
			}
		} else if sel.name == "cursor" {
			out.Set(key, parent.cursor)
		} else {
			out.Set(key, nil)
		}
	}
	return out, nil
}

func gqlExecuteSelectionSetWithEdges(ctx *gqlExecContext, parentType string, parent any, selections []*gqlSelection, path []any) (*Obj, error) {
	if parentType == "Edge" {
		return gqlExecuteEdgeSelectionSet(ctx, parent.(*gqlEdgeParent), selections, path)
	}
	return gqlExecuteSelectionSet(ctx, parentType, parent, selections, path)
}

// ---------------------------------------------------------------------------
// One GraphQL document -> {data, errors}
// ---------------------------------------------------------------------------

func gqlExecuteDocument(entry map[string]any, viewerLogin string) *Obj {
	query, _ := entry["query"].(string)
	if query == "" || strings.TrimSpace(query) == "" {
		return O("data", nil, "errors", []any{O("message", "Must provide query string.")})
	}
	variables := gqlVariables{}
	if entry["variables"] != nil {
		switch v := entry["variables"].(type) {
		case string:
			parsed, err := parseJSONValue([]byte(v))
			if err != nil {
				return O("data", nil, "errors", []any{O("message", "Variables are invalid JSON.")})
			}
			if m, ok := parsed.(map[string]any); ok {
				variables = m
			}
		case map[string]any:
			variables = v
		}
	}
	operationName, _ := entry["operationName"].(string)

	tokens, err := gqlLex(query)
	if err != nil {
		return O("data", nil, "errors", []any{O("message", err.Error())})
	}
	definitions, err := (&gqlParser{tokens: tokens}).parseDocument()
	if err != nil {
		return O("data", nil, "errors", []any{O("message", err.Error())})
	}

	fragments := map[string]*gqlFragment{}
	var operations []*gqlOperation
	for _, def := range definitions {
		if def.fragment != nil {
			fragments[def.fragment.name] = def.fragment
		} else if def.operation != nil {
			operations = append(operations, def.operation)
		}
	}
	if len(operations) == 0 {
		return O("data", nil, "errors", []any{O("message", "No operation found in document.")})
	}
	var operation *gqlOperation
	if operationName != "" {
		for _, o := range operations {
			if o.name == operationName {
				operation = o
				break
			}
		}
		if operation == nil {
			return O("data", nil, "errors", []any{O("message", fmt.Sprintf("Unknown operation named %q.", operationName))})
		}
	} else {
		operation = operations[0]
	}

	// Apply variable defaults.
	vars := gqlVariables{}
	for k, v := range variables {
		vars[k] = v
	}
	for _, def := range operation.variableDefinitions {
		if _, ok := vars[def.name]; !ok && def.defaultValue != nil {
			vars[def.name] = gqlValueFromAst(def.defaultValue, vars)
		}
	}

	ctx := &gqlExecContext{viewerLogin: viewerLogin, variables: vars, fragments: fragments}
	rootType := "Query"
	if operation.operation == "mutation" {
		rootType = "Mutation"
	}
	data, err := gqlExecuteSelectionSetWithEdges(ctx, rootType, map[string]any{}, operation.selectionSet, []any{})
	if err != nil {
		if fe, ok := err.(*gqlFieldError); ok {
			errObj := O("message", fe.message)
			if fe.path != nil {
				errObj.Set("path", fe.path)
			}
			return O("data", nil, "errors", []any{errObj})
		}
		return O("data", nil, "errors", []any{O("message", err.Error())})
	}
	return O("data", data, "errors", []any{})
}

func gqlSendJSON(w http.ResponseWriter, status int, body any) {
	text := MarshalNL(body)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(text)))
	h.Set("X-GitHub-Request-Id", "req-"+uuidV4())
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

func handleGraphql(w http.ResponseWriter, r *http.Request, pathname, method string, viewerLogin string) bool {
	if pathname != "/api/graphql" && pathname != "/api/graphql/" {
		return false
	}
	if method != "POST" {
		return false
	}

	expected := shopToken()
	auth := r.Header.Get("Authorization")
	if expected != "" && auth != "Bearer "+expected {
		gqlSendJSON(w, 401, O("message", "Bad credentials"))
		return true
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		gqlSendJSON(w, 400, O("message", "Problems parsing JSON"))
		return true
	}
	body, err := parseJSONValue(raw)
	if err != nil {
		gqlSendJSON(w, 400, O("message", "Problems parsing JSON"))
		return true
	}

	// Lock: mutations write, queries read. Parse the document first (no
	// store access), then execute under the appropriate lock. The scan
	// over-approximates: any `mutation` operation keyword in the document
	// takes the write lock, even if operationName selects a query — that
	// only costs concurrency, never correctness.
	isMutation := false
	scanEntry := func(m map[string]any) {
		if q, ok := m["query"].(string); ok && gqlHasMutationOp(q) {
			isMutation = true
		}
	}
	if m, ok := body.(map[string]any); ok {
		scanEntry(m)
	}
	if arr, ok := body.([]any); ok {
		for _, entry := range arr {
			if m, ok := entry.(map[string]any); ok {
				scanEntry(m)
			}
		}
	}
	if isMutation {
		storeMu.Lock()
		defer storeMu.Unlock()
	} else {
		storeMu.RLock()
		defer storeMu.RUnlock()
	}

	if arr, ok := body.([]any); ok {
		results := []any{}
		for _, entry := range arr {
			if m, ok := entry.(map[string]any); ok {
				results = append(results, gqlExecuteDocument(m, viewerLogin))
			} else {
				results = append(results, O("data", nil, "errors", []any{O("message", "Batch entry must be an object.")}))
			}
		}
		gqlSendJSON(w, 200, results)
		return true
	}
	if m, ok := body.(map[string]any); ok {
		gqlSendJSON(w, 200, gqlExecuteDocument(m, viewerLogin))
		return true
	}
	gqlSendJSON(w, 400, O("message", "Problems parsing JSON"))
	return true
}

// gqlHasMutationOp reports whether the document contains a `mutation`
// operation keyword (lexed, so string contents do not count). Over-approximate
// on purpose: a false positive only takes the write lock.
func gqlHasMutationOp(query string) bool {
	tokens, err := gqlLex(query)
	if err != nil {
		return true // unparseable: take the safe lock; the executor reports the error
	}
	for i, t := range tokens {
		if t.kind != tokName || t.value != "mutation" {
			continue
		}
		// An operation keyword appears at definition start: first token, or
		// right after a closing brace of the previous definition.
		if i == 0 {
			return true
		}
		prev := tokens[i-1]
		if prev.kind == tokPunct && prev.value == "}" {
			return true
		}
	}
	return false
}
