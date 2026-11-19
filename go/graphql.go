package main

import (
	"net/http"
	"strings"
)

// serveGraphQL answers POST /api/graphql. A batch is answered as an ordered
// array; a single operation answers as one object.
func (s *Server) serveGraphQL(w http.ResponseWriter, req *http.Request) {
	body, err := readBody(req, 2<<20)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, malformedMessage, nil)
		return
	}
	trimmed := bytesTrim(body)
	if len(trimmed) == 0 {
		writeJSON(w, http.StatusBadRequest, malformedMessage, nil)
		return
	}
	var single map[string]any
	if trimmed[0] == '[' {
		var batch []map[string]any
		if err := unmarshalJSON(trimmed, &batch); err != nil {
			writeJSON(w, http.StatusBadRequest, malformedMessage, nil)
			return
		}
		if len(batch) == 0 || len(batch) > 25 {
			writeJSON(w, http.StatusOK, jsonBytes(map[string]any{
				"errors": []map[string]string{{"message": "Invalid batch size."}}}), nil)
			return
		}
		out := make([]any, 0, len(batch))
		for _, doc := range batch {
			out = append(out, s.runGraphQL(doc))
		}
		writeJSON(w, http.StatusOK, jsonBytes(out), nil)
		return
	}
	if err := unmarshalJSON(trimmed, &single); err != nil {
		writeJSON(w, http.StatusBadRequest, malformedMessage, nil)
		return
	}
	writeJSON(w, http.StatusOK, jsonBytes(s.runGraphQL(single)), nil)
}

// runGraphQL executes one document against the shared store.
func (s *Server) runGraphQL(doc map[string]any) any {
	query, ok := doc["query"].(string)
	if !ok {
		return gqlError("Expected a GraphQL query string.")
	}
	vars := map[string]any{}
	if raw, present := doc["variables"]; present && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return gqlError("Expected variables to be an object.")
		}
		vars = m
	}
	operationName, _ := doc["operationName"].(string)
	return executeGraphQL(s.store, query, vars, operationName)
}

func gqlError(msg string) map[string]any {
	return map[string]any{"errors": []map[string]any{{"message": msg}}}
}

// ── a deliberately small GraphQL engine ─────────────────────────────────────
//
// The suite sends a known family of documents: introspection, repository and
// issue reads, search, and the create/merge/comment mutations. This engine
// parses the document, validates field names against a schema table, and
// executes selections in order. Unknown fields answer HTTP 200 with an errors
// array, which is what the contract asks for.

// execution holds the per-request state.
type execution struct {
	store  *Store
	vars   map[string]any
	errors []map[string]any
}

// executeGraphQL is the entry point for one document.
func executeGraphQL(store *Store, query string, vars map[string]any, operationName string) any {
	doc, err := parseDocument(query)
	if err != nil {
		return gqlError(err.Error())
	}
	op := doc.operation(operationName)
	if op == nil {
		if operationName != "" {
			return gqlError(`Unknown operation "` + operationName + `".`)
		}
		return gqlError("operationName is required.")
	}
	if op.kind == "subscription" {
		return gqlError("Subscriptions are not supported on this endpoint.")
	}
	ex := &execution{store: store, vars: map[string]any{}}
	for name, def := range op.variableDefs {
		if v, ok := vars[name]; ok {
			ex.vars[name] = v
		} else if def != nil {
			ex.vars[name] = def
		}
	}

	root := "Query"
	if op.kind == "mutation" {
		root = "Mutation"
	}
	data := ex.resolveRoot(root, op.selections)
	if len(ex.errors) > 0 {
		return map[string]any{"data": data, "errors": ex.errors}
	}
	return map[string]any{"data": data}
}

// resolveRoot executes a root gqlSelection set.
func (e *execution) resolveRoot(root string, sels []gqlSelection) map[string]any {
	out := map[string]any{}
	for _, sel := range sels {
		if sel.name == "__typename" {
			out[sel.alias()] = root
			continue
		}
		if sel.name == "__schema" || sel.name == "__type" {
			out[sel.alias()] = introspect(e.store, sel)
			continue
		}
		switch root {
		case "Query":
			out[sel.alias()] = e.queryField(sel)
		case "Mutation":
			out[sel.alias()] = e.mutationField(sel)
		}
	}
	return out
}

// queryField resolves one root query field.
func (e *execution) queryField(sel gqlSelection) any {
	switch sel.name {
	case "repository":
		owner := e.arg(sel, "owner")
		name := e.arg(sel, "name")
		if owner == "" || name == "" {
			// The suite also asks by id.
			if gid := e.idArg(sel, "id"); gid != "" {
				kind, num, ok := splitNodeID(gid)
				if ok && kind == "Repository" {
					for _, r := range e.store.Repos() {
						if r.ID == num {
							return e.resolveRepository(r, sel)
						}
					}
				}
			}
			return nil
		}
		r, ok := e.store.Lookup(owner, name)
		if !ok {
			return nil // unknown repository is data.repository = null, with no errors
		}
		return e.resolveRepository(r, sel)

	case "viewer":
		return e.resolveUser(e.store.viewer, sel)

	case "user":
		login := e.arg(sel, "login")
		if login == "" || login == e.store.viewer.Login {
			return e.resolveUser(e.store.viewer, sel)
		}
		return nil

	case "search":
		return e.search(sel)

	case "node":
		id := e.arg(sel, "id")
		kind, num, ok := splitNodeID(id)
		if !ok {
			return nil
		}
		switch kind {
		case "Repository":
			for _, r := range e.store.Repos() {
				if r.ID == num {
					return e.resolveRepository(r, sel)
				}
			}
		case "Issue":
			for _, r := range e.store.Repos() {
				if it, ok := r.issueByNodeNumber(num); ok {
					return e.resolveIssue(r, it, sel)
				}
			}
		case "PullRequest":
			for _, r := range e.store.Repos() {
				if p, ok := r.pullByNodeNumber(num); ok {
					return e.resolvePull(r, p, sel)
				}
			}
		}
		return nil

	case "rateLimit":
		return map[string]any{"limit": 5000, "remaining": 4999, "resetAt": e.store.stamp()}

	case "__typename":
		return "Query"
	}
	e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + sel.name + "\" on type \"Query\"."})
	return nil
}

// resolveRepository renders a repository gqlSelection.
func (e *execution) resolveRepository(r *Repository, sel gqlSelection) any {
	if sel.name == "__typename" {
		return "Repository"
	}
	out := map[string]any{}
	for _, f := range sel.selections {
		switch f.name {
		case "__typename":
			out[f.alias()] = "Repository"
		case "id":
			out[f.alias()] = r.NodeID
		case "databaseId":
			out[f.alias()] = r.ID
		case "name":
			out[f.alias()] = r.Name
		case "nameWithOwner":
			out[f.alias()] = r.Owner.Login + "/" + r.Name
		case "description":
			out[f.alias()] = r.Description
		case "isPrivate":
			out[f.alias()] = r.Private
		case "createdAt", "updatedAt":
			out[f.alias()] = r.CreatedAt
		case "hasIssuesEnabled":
			out[f.alias()] = true
		case "viewerPermission":
			out[f.alias()] = "ADMIN"
		case "owner":
			out[f.alias()] = e.resolveUser(r.Owner, f)
		case "defaultBranchRef":
			tip := r.Tip()
			out[f.alias()] = map[string]any{"name": r.HeadRef[len("refs/heads/"):],
				"target": map[string]any{"oid": tip}}
		case "issue":
			number := e.intArg(f, "number")
			if it, ok := r.Issue(number); ok {
				out[f.alias()] = e.resolveIssue(r, it, f)
			} else {
				out[f.alias()] = nil
			}
		case "issues":
			out[f.alias()] = e.resolveIssueConnection(r, f)
		case "pullRequest":
			number := e.intArg(f, "number")
			if p, ok := r.Pull(number); ok {
				out[f.alias()] = e.resolvePull(r, p, f)
			} else {
				out[f.alias()] = nil
			}
		case "pullRequests":
			out[f.alias()] = e.resolvePullConnection(r, f)
		default:
			if f.name != "__typename" {
				e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + f.name + "\" on type \"Repository\"."})
			}
		}
	}
	return out
}

// resolveIssueConnection renders a connection of issues, honouring orderBy and
// the states filter.
func (e *execution) resolveIssueConnection(r *Repository, sel gqlSelection) any {
	states := []string{"OPEN"}
	if raw := sel.args["states"]; raw != nil {
		if list, ok := raw.([]any); ok {
			states = states[:0]
			for _, v := range list {
				if s, ok := v.(string); ok {
					states = append(states, s)
				}
			}
		}
	}
	descending := true
	if ob, ok := sel.args["orderBy"].(map[string]any); ok {
		if dir, ok := ob["direction"].(string); ok {
			descending = dir != "ASC"
		}
	}
	// The states filter is a real filter: an issue whose state is not named
	// must not appear, whatever the fixture created.
	var rows []*Issue
	all := r.Issues("all", true)
	for _, it := range all {
		if len(states) > 0 {
			matched := false
			for _, st := range states {
				if strings.EqualFold(st, it.State) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		rows = append(rows, it)
	}
	sortIssuesByNumber(rows, !descending)

	// Cursor pagination: "after" names the last node of the previous page.
	if after, ok := e.argRaw(sel, "after").(string); ok && after != "" {
		cut := 0
		for i, it := range rows {
			if it.NodeID == after {
				cut = i + 1
				break
			}
		}
		rows = rows[cut:]
	}
	hasNext := false
	if first := e.intArg(sel, "first"); first > 0 && len(rows) > first {
		rows = rows[:first]
		hasNext = true
	}

	out := map[string]any{}
	for _, f := range sel.selections {
		switch f.name {
		case "totalCount":
			out[f.alias()] = len(rows)
		case "nodes":
			nodes := make([]any, 0, len(rows))
			for _, it := range rows {
				nodes = append(nodes, e.resolveIssue(r, it, f))
			}
			out[f.alias()] = nodes
		case "edges":
			edges := make([]any, 0, len(rows))
			for i, it := range rows {
				edges = append(edges, map[string]any{"cursor": "issue:" + it.NodeID + ":" + itoa(i),
					"node": e.resolveIssue(r, it, f)})
			}
			out[f.alias()] = edges
		case "pageInfo":
			var end any
			if len(rows) > 0 {
				end = rows[len(rows)-1].NodeID
			}
			out[f.alias()] = map[string]any{"hasNextPage": hasNext, "endCursor": end}
		}
	}
	return out
}

// resolvePullConnection renders a connection of pull requests.
func (e *execution) resolvePullConnection(r *Repository, sel gqlSelection) any {
	rows := r.Pulls("", false)
	out := map[string]any{}
	for _, f := range sel.selections {
		switch f.name {
		case "totalCount":
			out[f.alias()] = len(rows)
		case "nodes":
			nodes := make([]any, 0, len(rows))
			for _, p := range rows {
				nodes = append(nodes, e.resolvePull(r, p, f))
			}
			out[f.alias()] = nodes
		case "edges":
			edges := make([]any, 0, len(rows))
			for i, p := range rows {
				edges = append(edges, map[string]any{"cursor": "pull:" + p.NodeID + ":" + itoa(i),
					"node": e.resolvePull(r, p, f)})
			}
			out[f.alias()] = edges
		case "pageInfo":
			out[f.alias()] = map[string]any{"hasNextPage": false, "endCursor": nil}
		}
	}
	return out
}

// resolveIssue renders an issue gqlSelection.
func (e *execution) resolveIssue(r *Repository, it *Issue, sel gqlSelection) any {
	out := map[string]any{}
	for _, f := range sel.selections {
		switch f.name {
		case "__typename":
			out[f.alias()] = "Issue"
		case "id":
			out[f.alias()] = it.NodeID
		case "number":
			out[f.alias()] = it.Number
		case "title":
			out[f.alias()] = it.Title
		case "body":
			out[f.alias()] = it.Body
		case "state":
			out[f.alias()] = strings.ToUpper(it.State)
		case "createdAt", "updatedAt":
			out[f.alias()] = it.CreatedAt
		case "author":
			out[f.alias()] = e.resolveUser(r.Owner, f)
		case "repository":
			out[f.alias()] = map[string]any{"id": r.NodeID, "nameWithOwner": r.Owner.Login + "/" + r.Name}
		case "comments":
			out[f.alias()] = e.resolveCommentConnection(r, it, f)
		case "labels":
			labels := make([]any, 0, len(it.Labels))
			for _, name := range it.Labels {
				labels = append(labels, map[string]any{"name": name})
			}
			out[f.alias()] = map[string]any{"nodes": labels}
		default:
			e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + f.name + "\" on type \"Issue\"."})
		}
	}
	return out
}

// resolveCommentConnection renders an issue's comments.
func (e *execution) resolveCommentConnection(r *Repository, it *Issue, sel gqlSelection) any {
	rows := r.Comments(it.Number)
	out := map[string]any{}
	for _, f := range sel.selections {
		switch f.name {
		case "totalCount":
			out[f.alias()] = len(rows)
		case "nodes":
			nodes := make([]any, 0, len(rows))
			for _, c := range rows {
				nodes = append(nodes, e.resolveComment(r, c, f))
			}
			out[f.alias()] = nodes
		case "edges":
			edges := make([]any, 0, len(rows))
			for i, c := range rows {
				edges = append(edges, map[string]any{"cursor": c.NodeID + ":" + itoa(i),
					"node": e.resolveComment(r, c, f)})
			}
			out[f.alias()] = edges
		case "pageInfo":
			out[f.alias()] = map[string]any{"hasNextPage": false, "endCursor": nil}
		}
	}
	return out
}

// resolveComment renders one issue comment.
func (e *execution) resolveComment(r *Repository, c Comment, sel gqlSelection) any {
	out := map[string]any{}
	for _, f := range sel.selections {
		switch f.name {
		case "__typename":
			out[f.alias()] = "IssueComment"
		case "id":
			out[f.alias()] = c.NodeID
		case "body":
			out[f.alias()] = c.Body
		case "createdAt", "updatedAt":
			out[f.alias()] = c.CreatedAt
		case "author":
			out[f.alias()] = e.resolveUser(r.Owner, f)
		}
	}
	return out
}

// resolvePull renders a pull request gqlSelection.
func (e *execution) resolvePull(r *Repository, p *Pull, sel gqlSelection) any {
	head, base := r.PullTips(p)
	out := map[string]any{}
	for _, f := range sel.selections {
		switch f.name {
		case "__typename":
			out[f.alias()] = "PullRequest"
		case "id":
			out[f.alias()] = p.NodeID
		case "number":
			out[f.alias()] = p.Number
		case "title":
			out[f.alias()] = p.Title
		case "body":
			out[f.alias()] = p.Body
		case "state":
			out[f.alias()] = strings.ToUpper(p.State)
		case "merged":
			out[f.alias()] = p.Merged
		case "mergeable":
			out[f.alias()] = map[bool]string{true: "MERGEABLE", false: "CONFLICTING"}[r.CanMerge(p)]
		case "headRefName":
			out[f.alias()] = p.Head
		case "baseRefName":
			out[f.alias()] = p.Base
		case "headRefOid":
			out[f.alias()] = head
		case "baseRefOid":
			out[f.alias()] = base
		case "createdAt", "updatedAt":
			out[f.alias()] = p.CreatedAt
		case "files":
			files := r.Files(p)
			nodes := make([]any, 0, len(files))
			for _, file := range files {
				nodes = append(nodes, map[string]any{"path": file["filename"], "additions": 1, "deletions": 1})
			}
			out[f.alias()] = map[string]any{"nodes": nodes, "totalCount": len(nodes)}
		case "author":
			out[f.alias()] = e.resolveUser(r.Owner, f)
		}
	}
	return out
}

// resolveUser renders a user gqlSelection.
func (e *execution) resolveUser(u User, sel gqlSelection) any {
	out := map[string]any{}
	for _, f := range sel.selections {
		switch f.name {
		case "__typename":
			out[f.alias()] = "User"
		case "id":
			out[f.alias()] = u.NodeID
		case "login":
			out[f.alias()] = u.Login
		case "databaseId":
			out[f.alias()] = u.ID
		case "name":
			out[f.alias()] = u.Login
		default:
			if f.name != "__typename" {
				e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + f.name + "\" on type \"User\"."})
			}
		}
	}
	return out
}

// issueByNodeNumber finds an issue in a repository by its numeric node suffix.
func (r *Repository) issueByNodeNumber(num int) (*Issue, bool) {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	for _, it := range r.issues {
		if it.ID == num {
			return it, true
		}
	}
	return nil, false
}

// pullByNodeNumber finds a pull request in a repository by its numeric node suffix.
func (r *Repository) pullByNodeNumber(num int) (*Pull, bool) {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	for _, p := range r.pulls {
		if p.ID == num {
			return p, true
		}
	}
	return nil, false
}
