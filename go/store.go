// Package main implements a GitHub-compatible git forge.
//
// The forge serves four surfaces from one in-process store: Git smart HTTP,
// REST v3, GraphQL v4 and MCP. It depends only on the Go standard library and
// never shells out to a git binary.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Object types as they appear inside a packfile and in a loose object header.
const (
	TypeCommit = "commit"
	TypeTree   = "tree"
	TypeBlob   = "blob"
	TypeTag    = "tag"
)

// ZeroOID is Git's all-zero object id, used for "no such ref".
const ZeroOID = "0000000000000000000000000000000000000000"

// ErrNotFound is returned by store lookups that miss.
var ErrNotFound = errors.New("not found")

// Object is one immutable Git object held in memory.
type Object struct {
	Type string
	Data []byte
}

// Ref is a single reference.
type Ref struct {
	Name string
	OID  string
}

// Commit is the parsed form of a commit object.
type Commit struct {
	OID       string
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// TreeEntry is one line of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// Tag is the parsed form of an annotated tag object.
type Tag struct {
	OID        string
	Object     string
	ObjectType string
	TagName    string
	Tagger     string
	Message    string
}

// User mirrors the REST user shape the suite expects.
type User struct {
	Login     string `json:"login"`
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	Type      string `json:"type"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// Store is the whole forge state. All surfaces share this one value.
type Store struct {
	mu sync.RWMutex

	viewer User
	now    time.Time

	repos   map[string]*Repository // key: owner + "/" + name
	order   []string               // repo insertion order, for stable listing
	counter int64                  // global id counter for repos/issues/pulls

	pages int // id of the shared page counter for REST pagination
}

// Repository is one repository with its Git objects, refs and issues.
type Repository struct {
	owner *Store

	ID          int
	NodeID      string
	Name        string
	Description string
	Private     bool
	Owner       User

	CreatedAt string
	UpdatedAt string
	HeadRef   string

	objects map[string]*Object
	refs    map[string]string

	issues   map[int]*Issue
	issueSeq int
	labels   map[string]bool
	pulls    map[int]*Pull
	pullSeq  int
	comments map[int][]Comment
	labelSeq int

	// Caches. Every one of these is cleared whenever refs or content change,
	// so a cached reply can never outlive the state it was built from.
	packCache       map[string][]byte
	packCacheOrder  [][]byte
	packCacheBytes  int
	issueBodies     map[*Issue][]byte
	issueBodyOrder  [][]byte
	branchCacheKey  string
	branchCacheBody []byte
	commitCacheKey  string
	commitCacheBody []byte
}

// Issue mirrors the REST issue shape.
type Issue struct {
	ID        int      `json:"id"`
	NodeID    string   `json:"node_id"`
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	User      User     `json:"user"`
	Comments  int      `json:"comments"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	ClosedAt  *string  `json:"closed_at"`
}

// Comment mirrors the REST issue-comment shape.
type Comment struct {
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	Body      string `json:"body"`
	User      User   `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Pull mirrors the REST pull-request shape.
type Pull struct {
	ID             int     `json:"id"`
	NodeID         string  `json:"node_id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"`
	User           User    `json:"user"`
	Head           string  `json:"head"`
	Base           string  `json:"base"`
	Merged         bool    `json:"merged"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	ClosedAt       *string `json:"closed_at"`
	MergedAt       *string `json:"merged_at"`
}

// NewStore builds an empty forge for the given viewing account.
func NewStore(viewer User, now time.Time) *Store {
	return &Store{viewer: viewer, now: now, repos: map[string]*Repository{}}
}

// Now returns the fixed clock the store reports, so two runs on the same input
// produce identical output.
func (s *Store) Now() time.Time { return s.now }

// Viewer is the account every surface reports as the authenticated user.
func (s *Store) Viewer() User { return s.viewer }

func (s *Store) stamp() string { return s.now.UTC().Format("2006-01-02T15:04:05.000Z") }

func (s *Store) nextID() int {
	s.counter++
	return int(s.counter)
}

// key builds the repository map key.
func repoKey(owner, name string) string { return owner + "/" + name }

// Lookup finds a repository by owner and name. A repository owned by anybody
// else is simply absent, which is what makes a foreign owner answer 404.
func (s *Store) Lookup(owner, name string) (*Repository, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, name)]
	return r, ok
}

// Repos returns every repository in creation order.
func (s *Store) Repos() []*Repository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Repository, 0, len(s.order))
	for _, k := range s.order {
		if r, ok := s.repos[k]; ok {
			out = append(out, r)
		}
	}
	return out
}

// CreateRepo adds a repository owned by the named login.
func (s *Store) CreateRepo(owner, name, description string, private bool) *Repository {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createRepoLocked(owner, name, description, private)
}

func (s *Store) createRepoLocked(owner, name, description string, private bool) *Repository {
	id := s.nextID()
	r := &Repository{
		owner:       s,
		ID:          id,
		NodeID:      nodeID("Repository", id),
		Name:        name,
		Description: description,
		Private:     private,
		Owner:       User{Login: owner, ID: 1, NodeID: nodeID("User", 1), Type: "User"},
		CreatedAt:   s.stamp(),
		UpdatedAt:   s.stamp(),
		HeadRef:     "refs/heads/main",
		objects:     map[string]*Object{},
		refs:        map[string]string{},
		issues:      map[int]*Issue{},
		labels:      map[string]bool{},
		pulls:       map[int]*Pull{},
		comments:    map[int][]Comment{},
		packCache:   map[string][]byte{},
		issueBodies: map[*Issue][]byte{},
	}
	s.repos[repoKey(owner, name)] = r
	s.order = append(s.order, repoKey(owner, name))
	return r
}

// DeleteRepo removes a repository and everything in it.
func (s *Store) DeleteRepo(r *Repository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := repoKey(r.Owner.Login, r.Name)
	delete(s.repos, k)
	for i, name := range s.order {
		if name == k {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// nodeID renders a GitHub-style global node id: base64 of "Type:number".
func nodeID(kind string, n int) string {
	raw := kind + ":" + strconv.Itoa(n)
	return base64Std(raw)
}

// ---------------------------------------------------------------------------
// Git objects and refs
// ---------------------------------------------------------------------------

// PutObject stores an object, returning its hex object id.
func (r *Repository) PutObject(kind string, data []byte) string {
	oid := hashObject(kind, data)
	r.muPut(oid, &Object{Type: kind, Data: data})
	return oid
}

func (r *Repository) muPut(oid string, o *Object) {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = o
	}
}

// Object returns a stored object.
func (r *Repository) Object(oid string) (*Object, bool) {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	o, ok := r.objects[oid]
	return o, ok
}

// HasObject reports whether the object exists.
func (r *Repository) HasObject(oid string) bool {
	_, ok := r.Object(oid)
	return ok
}

// hashObject is Git's object id: sha1("<type> <len>\0<data>").
func hashObject(kind string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(kind))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// Refs returns the refs sorted by name.
func (r *Repository) Refs() []Ref {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	out := make([]Ref, 0, len(r.refs))
	for name, oid := range r.refs {
		out = append(out, Ref{Name: name, OID: oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Ref reads one ref.
func (r *Repository) Ref(name string) (string, bool) {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	oid, ok := r.refs[name]
	return oid, ok
}

// SetRef writes one ref and drops every cache that described the old refs.
func (r *Repository) SetRef(name, oid string) {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if oid == "" {
		delete(r.refs, name)
	} else {
		r.refs[name] = oid
	}
	r.invalidateLocked()
}

// Tip is the commit the default branch points at, or "" when unborn.
func (r *Repository) Tip() string {
	if oid, ok := r.Ref(r.HeadRef); ok {
		return oid
	}
	return ""
}

// invalidateLocked clears every derived cache. Called with the store lock held.
func (r *Repository) invalidateLocked() {
	r.packCache = map[string][]byte{}
	r.packCacheOrder = nil
	r.packCacheBytes = 0
	r.branchCacheKey = ""
	r.branchCacheBody = nil
	r.commitCacheKey = ""
	r.commitCacheBody = nil
}

// invalidate drops caches from outside the lock.
func (r *Repository) invalidate() {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.invalidateLocked()
}

// cachePut stores a reply under a key, evicting oldest entries past the budget.
func (r *Repository) cachePut(key string, body []byte) {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.cachePutLocked(key, body)
}

func (r *Repository) cachePutLocked(key string, body []byte) {
	const budget = 8 << 20
	if len(key) > 4096 || len(body) > budget {
		return
	}
	if _, exists := r.packCache[key]; exists {
		return
	}
	for r.packCacheBytes+len(body) > budget && len(r.packCacheOrder) > 0 {
		oldest := r.packCacheOrder[0]
		r.packCacheOrder = r.packCacheOrder[1:]
		delete(r.packCache, string(oldest))
		r.packCacheBytes -= len(oldest)
	}
	r.packCache[key] = body
	r.packCacheOrder = append(r.packCacheOrder, body)
	r.packCacheBytes += len(body)
}

// cacheGet reads a cached reply.
func (r *Repository) cacheGet(key string) ([]byte, bool) {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	b, ok := r.packCache[key]
	return b, ok
}

// EscapeRefName reports whether a ref name is safe to store and report.
func EscapeRefName(name string) string {
	return strings.Map(func(c rune) rune {
		if c == '\n' || c == '\r' {
			return '?'
		}
		return c
	}, name)
}

// RefNameValid applies Git's own ref naming rules.
func RefNameValid(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return false
	}
	if strings.HasSuffix(name, ".") || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	for _, bad := range []string{" ", "~", "^", ":", "?", "*", "[", "\\", "\x7f"} {
		if strings.Contains(name, bad) {
			return false
		}
	}
	for _, c := range name {
		if c < 0x20 {
			return false
		}
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == "" || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	return true
}
