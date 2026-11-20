// Shared in-memory store. Every surface (git smart HTTP, REST v3,
// GraphQL v4, MCP) reads and writes the SAME store, so the four surfaces
// agree. No persistence: the trial is a fresh process each time.
// Deterministic: ids and timestamps derive from counters, never from the
// wall clock or randomness.
//
// Concurrency: net/http serves each connection on its own goroutine, so all
// store access is guarded by storeMu in server.go (read lock for reads,
// write lock for mutations). The Store methods themselves assume the caller
// holds the appropriate lock.
//
// Insertion order matters (Node's Map preserves it): repos live in a slice
// plus a key map; labels use an ordered map. getRepo's adoption walk scans
// repos in insertion order, exactly like the Node version.

package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

// issueCacheGen is bumped whenever a mutation can change the serialized
// form of an already-existing issue (label create, issue patch, comment
// add, viewer adoption). A cached issue JSON is valid only while its
// stored generation still equals this one, so a stale cache can only
// cause a rebuild, never a stale response.
var issueCacheGen atomic.Uint64

func bumpIssueCache() { issueCacheGen.Add(1) }

type GitObjectType string

const (
	ObjBlob   GitObjectType = "blob"
	ObjTree   GitObjectType = "tree"
	ObjCommit GitObjectType = "commit"
	ObjTag    GitObjectType = "tag"
)

type GitObject struct {
	Type    GitObjectType
	Content []byte
}

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt int
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	CreatedAt int
	Comments  []*Comment

	// cachedJSON holds the last serialized form of this issue (issueJSON +
	// '\n'), valid while cachedGen equals issueCacheGen. Written only by
	// the REST issue serializer, read by the GET handler; both are
	// atomic, so concurrent readers never observe a torn value.
	cachedJSON atomic.Pointer[[]byte]
	cachedGen  atomic.Uint64
}

type PullFile struct {
	Filename  string
	Status    string // added | modified | removed | changed
	Additions int
	Deletions int
	Changes   int
}

type Pull struct {
	ID            int
	Number        int
	Title         string
	Body          string
	State         string // open | closed
	Merged        bool
	MergedAt      string // "" means null
	MergeCommitSha string // "" means null
	Head          PullRef
	Base          PullRef
	Files         []*PullFile
	CreatedAt     int
}

type PullRef struct {
	Ref string
	Sha string
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	FullName      string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string]string
	Objects       map[string]*GitObject
	Labels        *OrderedLabels
	Issues        []*Issue
	Pulls         []*Pull
	IssueSeq      int
	PullSeq       int
	CommentSeq    int
	Clock         int
}

// OrderedLabels is an insertion-ordered map (JS Map.set on an existing key
// keeps the original position).
type OrderedLabels struct {
	keys []string
	m    map[string]*Label
}

func NewOrderedLabels() *OrderedLabels {
	return &OrderedLabels{m: make(map[string]*Label)}
}

func (o *OrderedLabels) Set(name string, l *Label) {
	if _, ok := o.m[name]; !ok {
		o.keys = append(o.keys, name)
	}
	o.m[name] = l
}

func (o *OrderedLabels) Get(name string) (*Label, bool) {
	l, ok := o.m[name]
	return l, ok
}

func (o *OrderedLabels) Has(name string) bool {
	_, ok := o.m[name]
	return ok
}

func (o *OrderedLabels) Values() []*Label {
	out := make([]*Label, 0, len(o.keys))
	for _, k := range o.keys {
		out = append(out, o.m[k])
	}
	return out
}

func GitObjectID(typ GitObjectType, content []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ) + " " + itoa(len(content)) + "\x00"))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func GlobalID(kind string, numericID int) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + itoa(numericID)))
}

var globalIDRe = regexp.MustCompile(`^([A-Za-z]+):(\d+)$`)

type ParsedGlobalID struct {
	Kind      string
	NumericID int
}

func ParseGlobalID(id string) *ParsedGlobalID {
	// Node's Buffer.from(id, 'base64') is lenient: non-alphabet characters
	// are ignored. Mirror that before decoding.
	cleaned := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			cleaned = append(cleaned, c)
		}
	}
	raw, err := base64.StdEncoding.DecodeString(string(cleaned))
	if err != nil {
		// Try without padding, like Node's lenient decoder.
		raw2, err2 := base64.RawStdEncoding.DecodeString(string(cleaned))
		if err2 != nil {
			return nil
		}
		raw = raw2
	}
	m := globalIDRe.FindStringSubmatch(string(raw))
	if m == nil {
		return nil
	}
	n := 0
	for _, c := range m[2] {
		n = n*10 + int(c-'0')
	}
	return &ParsedGlobalID{Kind: m[1], NumericID: n}
}

type Store struct {
	repos         []*Repo
	byKey         map[string]*Repo
	NextRepoID    int
	NextIssueID   int
	NextPullID    int
	NextCommentID int
	ViewerLogin   string
	Clock         int

	// viewerLoginV mirrors ViewerLogin for lock-free reads on the hot
	// path (dispatch): it holds a string and is written only inside
	// SetViewerLogin, which runs under storeMu.
	viewerLoginV atomic.Value
}

func NewStore() *Store {
	s := &Store{
		byKey:         make(map[string]*Repo),
		NextRepoID:    1,
		NextIssueID:   1,
		NextPullID:    1,
		NextCommentID: 1,
		ViewerLogin:   "viewer",
		Clock:         1,
	}
	s.viewerLoginV.Store("viewer")
	return s
}

// ViewerLoginFast returns the adopted viewer login without taking
// storeMu (see viewerLoginV).
func (s *Store) ViewerLoginFast() string {
	if v := s.viewerLoginV.Load(); v != nil {
		return v.(string)
	}
	return "viewer"
}

// lowerOwner is strings.ToLower with a fast path: when the owner has no
// ASCII uppercase letter and no non-ASCII byte, lowercasing is the
// identity, so the original string is returned without allocating.
func lowerOwner(owner string) string {
	for i := 0; i < len(owner); i++ {
		c := owner[i]
		if (c >= 'A' && c <= 'Z') || c >= 0x80 {
			return strings.ToLower(owner)
		}
	}
	return owner
}

func (s *Store) repoKey(owner, name string) string {
	return lowerOwner(owner) + "/" + name
}

func (s *Store) SetViewerLogin(login string) {
	if login == "" || login == "viewer" {
		return
	}
	if s.ViewerLogin != "viewer" {
		return
	}
	// Adopting the viewer renames every "viewer"-owned repo, which changes
	// the user.login / comments_url in their serialized issues.
	bumpIssueCache()
	s.ViewerLogin = login
	s.viewerLoginV.Store(login)
	for _, repo := range s.repos {
		if repo.Owner != "viewer" {
			continue
		}
		delete(s.byKey, s.repoKey(repo.Owner, repo.Name))
		repo.Owner = login
		repo.FullName = login + "/" + repo.Name
		s.byKey[s.repoKey(login, repo.Name)] = repo
	}
}

func (s *Store) GetRepo(owner, name string) *Repo {
	if exact, ok := s.byKey[s.repoKey(owner, name)]; ok {
		return exact
	}
	for _, repo := range s.repos {
		if repo.Name != name {
			continue
		}
		if repo.Owner == "viewer" {
			// Adoption renames the repo, which changes the serialized
			// form of its issues.
			bumpIssueCache()
			delete(s.byKey, s.repoKey(repo.Owner, repo.Name))
			repo.Owner = owner
			repo.FullName = owner + "/" + repo.Name
			s.byKey[s.repoKey(owner, repo.Name)] = repo
			return repo
		}
		if strings.EqualFold(repo.Owner, owner) {
			return repo
		}
		return nil
	}
	return nil
}

func (s *Store) CreateRepo(owner, name, description string, isPrivate bool) *Repo {
	id := s.NextRepoID
	s.NextRepoID++
	repo := &Repo{
		ID:            id,
		Owner:         owner,
		Name:          name,
		FullName:      owner + "/" + name,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: "main",
		Refs:          make(map[string]string),
		Objects:       make(map[string]*GitObject),
		Labels:        NewOrderedLabels(),
		Issues:        nil,
		Pulls:         nil,
	}
	s.repos = append(s.repos, repo)
	s.byKey[s.repoKey(owner, name)] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	repo := s.GetRepo(owner, name)
	if repo == nil {
		return false
	}
	delete(s.byKey, s.repoKey(repo.Owner, repo.Name))
	for i, r := range s.repos {
		if r == repo {
			s.repos = append(s.repos[:i], s.repos[i+1:]...)
			break
		}
	}
	return true
}

func (s *Store) ListReposByOwner(owner string) []*Repo {
	want := strings.ToLower(owner)
	viewer := strings.ToLower(s.ViewerLogin)
	out := []*Repo{}
	for _, repo := range s.repos {
		if strings.ToLower(repo.Owner) == want {
			out = append(out, repo)
		} else if repo.Owner == "viewer" && want == viewer {
			out = append(out, repo)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Tick returns the current clock value and increments it.
func (s *Store) Tick() int {
	v := s.Clock
	s.Clock++
	return v
}

var store = NewStore()
