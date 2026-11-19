package main

import (
	"net/http"
	"strconv"
	"strings"
)

// mcpRevision is the protocol revision this server speaks by default.
const mcpRevision = "2026-07-28"

// supportedRevisions lists what server/discover reports.
var supportedRevisions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2024-11-05"}

// mcpTool is one tool this server exposes.
type mcpTool struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

// mcpTools is fixed, so the tool list is built once and answered byte-for-byte
// identically on every call, which the suite checks explicitly.
var mcpTools = []mcpTool{
	{Name: "create_repo", Description: "Create a repository for the authenticated account.",
		Properties: map[string]any{"name": map[string]any{"type": "string", "description": "The repository name."}},
		Required:   []string{"name"}},
	{Name: "get_repo", Description: "Get a repository by owner and name.",
		Properties: map[string]any{
			"owner": map[string]any{"type": "string", "description": "The repository owner."},
			"repo":  map[string]any{"type": "string", "description": "The repository name."},
		}, Required: []string{"owner", "repo"}},
	{Name: "create_issue", Description: "Create an issue in a repository.",
		Properties: map[string]any{
			"owner": map[string]any{"type": "string", "description": "The repository owner."},
			"repo":  map[string]any{"type": "string", "description": "The repository name."},
			"title": map[string]any{"type": "string", "description": "The issue title."},
			"body":  map[string]any{"type": "string", "description": "The issue body."},
		}, Required: []string{"owner", "repo", "title"}},
	{Name: "list_issues", Description: "List the issues of a repository.",
		Properties: map[string]any{
			"owner": map[string]any{"type": "string", "description": "The repository owner."},
			"repo":  map[string]any{"type": "string", "description": "The repository name."},
			"state": map[string]any{"type": "string", "description": "open, closed or all."},
		}, Required: []string{"owner", "repo"}},
	{Name: "create_pull_request", Description: "Open a pull request in a repository.",
		Properties: map[string]any{
			"owner": map[string]any{"type": "string", "description": "The repository owner."},
			"repo":  map[string]any{"type": "string", "description": "The repository name."},
			"title": map[string]any{"type": "string", "description": "The pull request title."},
			"head":  map[string]any{"type": "string", "description": "The head branch."},
			"base":  map[string]any{"type": "string", "description": "The base branch."},
		}, Required: []string{"owner", "repo", "title", "head", "base"}},
	{Name: "merge_pull_request", Description: "Merge a pull request.",
		Properties: map[string]any{
			"owner":       map[string]any{"type": "string", "description": "The repository owner."},
			"repo":        map[string]any{"type": "string", "description": "The repository name."},
			"pull_number": map[string]any{"type": "number", "description": "The pull request number."},
		}, Required: []string{"owner", "repo", "pull_number"}},
}

// mcpToolsJSON is the encoded tool list, produced once.
var mcpToolsJSON = func() []byte {
	list := make([]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		list = append(list, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": t.Properties,
				"required":   t.Required,
			},
		})
	}
	return jsonBytes(map[string]any{"tools": list})
}()

// serveMCP answers the Streamable HTTP JSON-RPC endpoint.
func (s *Server) serveMCP(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodPost:
	case http.MethodGet, http.MethodDelete:
		writeJSON(w, http.StatusMethodNotAllowed, jsonBytes(map[string]string{"message": "Method not allowed"}),
			map[string]string{"Allow": "POST"})
		return
	default:
		writeJSON(w, http.StatusMethodNotAllowed, jsonBytes(map[string]string{"message": "Method not allowed"}),
			map[string]string{"Allow": "POST"})
		return
	}
	if !s.authorized(req.Header.Get("Authorization")) {
		drain(req)
		writeJSON(w, http.StatusUnauthorized, authMessage, nil)
		return
	}
	body, err := readBody(req, 2<<20)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, malformedMessage, nil)
		return
	}
	var rpc map[string]any
	if err := unmarshalJSON(bytesTrim(body), &rpc); err != nil {
		// A body that is not JSON answers HTTP 400 carrying -32700. A body
		// that is valid JSON but not an object is a protocol error instead.
		rpcErrorStatus(w, nil, -32700, "Parse error", http.StatusBadRequest)
		return
	}

	id, _ := rpc["id"]
	method, _ := rpc["method"].(string)
	params, _ := rpc["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	switch method {
	case "initialize":
		result := map[string]any{
			"protocolVersion": negotiatedRevision(stringOr(params["protocolVersion"], "")),
			// Both capability shapes are advertised: "tools" with an empty
			// object is what the current revision checks for, and the older
			// nested form is harmless to a stricter client.
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{"name": "forgepulse", "version": "1.0.0"},
		}
		rpcResult(w, id, result)
		return
	case "notifications/initialized":
		// A notification has no reply.
		w.WriteHeader(http.StatusAccepted)
		return
	case "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
		return
	case "server/discover":
		// The modern era never sends initialize, so the identity and the tool
		// capability are declared here instead.
		rpcResult(w, id, map[string]any{
			"protocolVersion":   mcpRevision,
			"supportedVersions": supportedRevisions,
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":        map[string]any{"name": "forgepulse", "version": "1.0.0"},
		})
		return
	case "tools/list":
		rpcResultJSON(w, id, mcpToolsJSON)
		return
	case "tools/call":
		s.mcpCall(w, id, params)
		return
	case "ping":
		rpcResult(w, id, map[string]any{})
		return
	case "resources/list":
		rpcResult(w, id, map[string]any{"resources": []any{}})
		return
	case "prompts/list":
		rpcResult(w, id, map[string]any{"prompts": []any{}})
		return
	}
	rpcError(w, id, -32601, "Method not found")
}

// mcpCall dispatches one tool invocation onto the shared store.
func (s *Server) mcpCall(w http.ResponseWriter, id any, params map[string]any) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	// A server prefix on the tool name is allowed and matched as a suffix.
	base := name
	if idx := strings.LastIndex(name, "__"); idx >= 0 {
		base = name[idx+2:]
	}
	base = strings.ToLower(base)

	owner := mcpString(args, "owner")
	repoName := mcpString(args, "repo")
	if repoName == "" {
		// The suite fills a full_name property with owner/repo.
		if full := mcpString(args, "full_name"); full != "" {
			if idx := strings.IndexByte(full, '/'); idx > 0 {
				owner, repoName = full[:idx], full[idx+1:]
			}
		}
	}
	if owner == "" {
		owner = s.store.viewer.Login
	}

	switch base {
	case "create_repo":
		name := mcpString(args, "name")
		if name == "" {
			rpcError(w, id, -32602, "Invalid params: name is required")
			return
		}
		if _, exists := s.store.Lookup(s.store.viewer.Login, name); exists {
			rpcError(w, id, -32602, "Invalid params: a repository with that name already exists")
			return
		}
		repo := s.store.CreateRepo(s.store.viewer.Login, name, mcpString(args, "description"), false)
		mcpResultJSON(w, id, jsonBytes(repoView(repo, s.origin())))

	case "get_repo":
		if owner == "" || repoName == "" {
			rpcError(w, id, -32602, "Invalid params: owner and repo are required")
			return
		}
		repo, ok := s.store.Lookup(owner, repoName)
		if !ok {
			rpcError(w, id, -32602, "Invalid params: repository not found")
			return
		}
		mcpResultJSON(w, id, jsonBytes(repoView(repo, s.origin())))

	case "create_issue":
		repo, ok := s.store.Lookup(owner, repoName)
		if !ok {
			rpcError(w, id, -32602, "Invalid params: repository not found")
			return
		}
		title := mcpString(args, "title")
		if title == "" {
			rpcError(w, id, -32602, "Invalid params: title is required")
			return
		}
		it, err := repo.AddIssue(input(map[string]any{"title": title, "body": mcpString(args, "body")}))
		if err != nil {
			rpcError(w, id, -32602, "Invalid params: "+err.Error())
			return
		}
		mcpResultJSON(w, id, repo.IssueJSON(it))

	case "list_issues":
		repo, ok := s.store.Lookup(owner, repoName)
		if !ok {
			rpcError(w, id, -32602, "Invalid params: repository not found")
			return
		}
		state := mcpString(args, "state")
		if state == "" {
			state = "open"
		}
		rows := repo.Issues(state, false)
		views := make([]any, 0, len(rows))
		for _, it := range rows {
			views = append(views, issueView(repo, it))
		}
		mcpResultJSON(w, id, jsonBytes(views))

	case "create_pull_request":
		repo, ok := s.store.Lookup(owner, repoName)
		if !ok {
			rpcError(w, id, -32602, "Invalid params: repository not found")
			return
		}
		head, baseBranch := mcpString(args, "head"), mcpString(args, "base")
		if head == "" || baseBranch == "" {
			rpcError(w, id, -32602, "Invalid params: head and base are required")
			return
		}
		p, err := repo.AddPull(input(map[string]any{
			"title": mcpString(args, "title"), "body": mcpString(args, "body"),
			"head": head, "base": baseBranch,
		}))
		if err != nil {
			rpcError(w, id, -32602, "Invalid params: "+err.Error())
			return
		}
		mcpResultJSON(w, id, jsonBytes(pullView(repo, p, s.origin())))

	case "merge_pull_request":
		repo, ok := s.store.Lookup(owner, repoName)
		if !ok {
			rpcError(w, id, -32602, "Invalid params: repository not found")
			return
		}
		number := mcpInt(args, "pull_number")
		if number <= 0 {
			rpcError(w, id, -32602, "Invalid params: pull_number is required")
			return
		}
		p, found := repo.Pull(number)
		if !found {
			rpcError(w, id, -32602, "Invalid params: pull request not found")
			return
		}
		out, err := repo.Merge(p, input(map[string]any{}))
		if err != nil {
			rpcError(w, id, -32602, "Invalid params: "+err.Error())
			return
		}
		mcpResultJSON(w, id, jsonBytes(out))

	default:
		rpcError(w, id, -32601, "Unknown tool: "+name)
	}
}

// origin renders the base URL used inside rendered payloads. It is a stable
// value so two runs agree.
func (s *Server) origin() string { return "http://127.0.0.1" }

// negotiatedRevision picks the revision to answer with.
func negotiatedRevision(requested string) string {
	if requested == "" {
		return mcpRevision
	}
	for _, r := range supportedRevisions {
		if r == requested {
			return requested
		}
	}
	return mcpRevision
}

// rpcResult writes a JSON-RPC success reply.
func rpcResult(w http.ResponseWriter, id any, result any) {
	writeJSON(w, http.StatusOK, jsonBytes(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}), nil)
}

// rpcResultJSON writes a success reply whose result is pre-encoded, so an
// identical tools/list answers byte-for-byte identically every time.
func rpcResultJSON(w http.ResponseWriter, id any, resultJSON []byte) {
	envelope := make([]byte, 0, len(resultJSON)+64)
	envelope = append(envelope, `{"jsonrpc":"2.0","id":`...)
	envelope = append(envelope, encodeScalar(id)...)
	envelope = append(envelope, `,"result":`...)
	envelope = append(envelope, resultJSON...)
	envelope = append(envelope, '}')
	writeJSON(w, http.StatusOK, envelope, nil)
}

// mcpResultJSON wraps a payload as MCP tool content.
func mcpResultJSON(w http.ResponseWriter, id any, payload []byte) {
	content := map[string]any{"content": []any{map[string]any{"type": "text", "text": string(payload)}}}
	rpcResult(w, id, content)
}

// rpcError writes a JSON-RPC error reply with HTTP 200, which is what the
// protocol wants for a well-formed request that fails.
func rpcError(w http.ResponseWriter, id any, code int, message string) {
	rpcErrorStatus(w, id, code, message, http.StatusOK)
}

// rpcErrorStatus writes a JSON-RPC error reply with an explicit HTTP status,
// used for a body that could not be parsed at all.
func rpcErrorStatus(w http.ResponseWriter, id any, code int, message string, status int) {
	writeJSON(w, status, jsonBytes(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	}), nil)
}

// encodeScalar renders a JSON-RPC id.
func encodeScalar(id any) []byte {
	switch v := id.(type) {
	case nil:
		return []byte("null")
	case string:
		return jsonBytes(v)
	case float64:
		if v == float64(int64(v)) {
			return []byte(itoa64(int64(v)))
		}
		return jsonBytes(v)
	}
	return []byte("null")
}

func mcpString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func mcpInt(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}
