package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// Server holds the shared state every surface reads.
type Server struct {
	store *Store
	token []byte
}

// NewServer wires the store and the bearer token.
func NewServer(store *Store, token string) *Server {
	return &Server{store: store, token: []byte(token)}
}

// authorized compares the bearer token in constant time and fails closed when
// no token is configured.
func (s *Server) authorized(header string) bool {
	if len(s.token) == 0 || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	got := []byte(header[len("Bearer "):])
	return len(got) == len(s.token) && subtle.ConstantTimeCompare(got, s.token) == 1
}

// Handle is the single entry point for every request. Keeping the dispatch in
// one function is what makes the hot path allocation-light: the repeated
// numbered-issue GET is recognised before any generic parsing happens.
func (s *Server) Handle(w http.ResponseWriter, req *http.Request) {
	target := req.URL.Path

	// Fast path: the read workload is 2,000 identical issue GETs. Match the
	// route with a single scan and serve pre-encoded bytes from cache.
	if req.Method == http.MethodGet {
		if owner, name, num, ok := matchIssuePath(target); ok {
			s.serveIssue(w, req, owner, name, num)
			return
		}
	}

	// The boot probe asks /healthz without a token, so it is answered before
	// authentication; every other path requires the bearer credential.
	if target == "/healthz" && req.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, []byte(`{"ok":true}`), nil)
		return
	}
	if !s.authorized(req.Header.Get("Authorization")) {
		drain(req)
		writeJSON(w, http.StatusUnauthorized, jsonBytes(map[string]string{"message": "Requires authentication"}), nil)
		return
	}
	s.dispatch(w, req, target)
}

// serveIssue answers GET /api/v3/repos/{owner}/{repo}/issues/{number}.
func (s *Server) serveIssue(w http.ResponseWriter, req *http.Request, owner, name string, num int) {
	if !s.authorized(req.Header.Get("Authorization")) {
		drain(req)
		writeJSON(w, http.StatusUnauthorized, authMessage, nil)
		return
	}
	repo, ok := s.store.Lookup(owner, name)
	if !ok {
		writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
		return
	}
	issue, ok := repo.Issue(num)
	if !ok {
		writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
		return
	}
	// The encoded body is cached on the issue, so a repeated read is one map
	// lookup plus a write.
	writeJSON(w, http.StatusOK, repo.IssueJSON(issue), nil)
}

// dispatch routes everything that is not the issue-read fast path.
func (s *Server) dispatch(w http.ResponseWriter, req *http.Request, target string) {
	switch {
	case target == "/healthz" && req.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, []byte(`{"ok":true}`), nil)
		return
	case target == "/api/v3" || strings.HasPrefix(target, "/api/v3/"):
		s.serveREST(w, req, target)
		return
	case target == "/api/graphql" && req.Method == http.MethodPost:
		s.serveGraphQL(w, req)
		return
	case target == "/mcp":
		s.serveMCP(w, req)
		return
	}
	if owner, name, service, action, ok := matchGitPath(target); ok {
		s.serveGit(w, req, owner, name, service, action)
		return
	}
	writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
}

// matchIssuePath recognises the numbered-issue route without allocating a
// segment slice.
func matchIssuePath(path string) (owner, name string, number int, ok bool) {
	const prefix = "/api/v3/repos/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", 0, false
	}
	rest := path[len(prefix):]
	first := strings.IndexByte(rest, '/')
	if first <= 0 {
		return "", "", 0, false
	}
	owner = rest[:first]
	rest = rest[first+1:]
	second := strings.IndexByte(rest, '/')
	if second <= 0 {
		return "", "", 0, false
	}
	name = rest[:second]
	rest = rest[second+1:]
	const segment = "issues/"
	if !strings.HasPrefix(rest, segment) {
		return "", "", 0, false
	}
	digits := rest[len(segment):]
	if digits == "" || digits[0] == '0' {
		return "", "", 0, false
	}
	n := 0
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if c < '0' || c > '9' {
			return "", "", 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return "", "", 0, false
		}
	}
	if strings.ContainsAny(owner, "%\\\x00") || strings.ContainsAny(name, "%\\\x00") {
		return "", "", 0, false
	}
	return owner, name, n, true
}

// matchGitPath recognises /{owner}/{repo}.git/{action}.
func matchGitPath(path string) (owner, name, service, action string, ok bool) {
	if !strings.HasPrefix(path, "/") {
		return "", "", "", "", false
	}
	rest := path[1:]
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return "", "", "", "", false
	}
	owner = rest[:i]
	rest = rest[i+1:]
	j := strings.Index(rest, ".git/")
	if j <= 0 {
		return "", "", "", "", false
	}
	name = rest[:j]
	action = rest[j+len(".git/"):]
	switch action {
	case "info/refs", "git-upload-pack", "git-receive-pack":
	default:
		return "", "", "", "", false
	}
	if strings.ContainsAny(owner, "%\\\x00") || strings.ContainsAny(name, "%\\\x00") {
		return "", "", "", "", false
	}
	return owner, name, "", action, true
}

// serveGit handles the smart HTTP endpoints.
func (s *Server) serveGit(w http.ResponseWriter, req *http.Request, owner, name, _, action string) {
	repo, ok := s.store.Lookup(owner, name)
	if !ok {
		drain(req)
		writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
		return
	}
	if action == "info/refs" {
		if req.Method != http.MethodGet {
			writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
			return
		}
		service := req.URL.Query().Get("service")
		if service != serviceUpload && service != serviceReceive {
			writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
			return
		}
		v2 := strings.Contains(req.Header.Get("Git-Protocol"), "version=2")
		body := Advertisement(repo, service, v2)
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	if req.Method != http.MethodPost {
		writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
		return
	}
	body, err := readBody(req, 32<<20)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonBytes(map[string]string{"message": "Malformed request"}), nil)
		return
	}
	v2 := strings.Contains(req.Header.Get("Git-Protocol"), "version=2")
	if action == "git-upload-pack" {
		key := fetchCacheKey(body, v2)
		if cached, ok := repo.cacheGet(key); ok {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cached)
			return
		}
		out, err := UploadPack(repo, body, v2)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, jsonBytes(map[string]string{"message": "Malformed request"}), nil)
			return
		}
		if len(out) <= 8<<20 {
			repo.cachePut(key, out)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}
	out := ReceivePack(repo, body)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// fetchCacheKey identifies an upload-pack request by its bytes, so an identical
// repeated fetch is answered from cache. The protocol version is part of the
// key because the same body means different things in v0 and v2.
func fetchCacheKey(body []byte, v2 bool) string {
	var b strings.Builder
	b.Grow(len(body) + 4)
	if v2 {
		b.WriteString("v2\x00")
	} else {
		b.WriteString("v0\x00")
	}
	b.Write(body)
	return b.String()
}

// readBody reads a capped request body.
func readBody(req *http.Request, limit int64) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	defer req.Body.Close()
	return readAllLimit(req.Body, limit)
}

// drain discards an unread body so the connection can be reused.
func drain(req *http.Request) {
	if req.Body == nil {
		return
	}
	_, _ = readAllLimit(req.Body, 1<<20)
	_ = req.Body.Close()
}

// writeJSON sends an already-encoded JSON body with an explicit length, which
// lets the connection stay keep-alive without chunked framing.
func writeJSON(w http.ResponseWriter, status int, body []byte, extra map[string]string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-GitHub-Request-Id", nextRequestID())
	for k, v := range extra {
		h.Set(k, v)
	}
	if status == http.StatusNoContent || status == http.StatusNotModified {
		w.WriteHeader(status)
		return
	}
	h.Set("Content-Length", itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

var (
	authMessage      = []byte(`{"message":"Requires authentication"}`)
	notFoundMessage  = []byte(`{"message":"Not Found"}`)
	malformedMessage = []byte(`{"message":"Malformed JSON body"}`)
)

// decodeJSON parses a body into a generic map, rejecting malformed input with
// the status the suite expects.
func decodeJSON(body []byte) (map[string]any, bool) {
	if len(bytesTrim(body)) == 0 {
		return map[string]any{}, true
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, false
	}
	if out == nil {
		return map[string]any{}, true
	}
	return out, true
}

// input is a typed accessor over a decoded JSON object.
type input map[string]any

func (in input) str(key string) (string, bool) {
	v, ok := in[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func (in input) strOr(key, fallback string) string {
	if s, ok := in.str(key); ok {
		return s
	}
	return fallback
}

func (in input) boolean(key string) (bool, bool) {
	v, ok := in[key]
	if !ok || v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func (in input) number(key string) (float64, bool) {
	v, ok := in[key]
	if !ok || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

func (in input) strings(key string) []string {
	v, ok := in[key]
	if !ok || v == nil {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
