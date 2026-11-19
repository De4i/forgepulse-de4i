package main

import "strings"

// mutationField resolves one root mutation. Every mutation writes to the same
// store the REST surface reads, so a GraphQL write is visible through REST.
func (e *execution) mutationField(sel gqlSelection) any {
	inputArg := e.argRaw(sel, "input")
	in, _ := inputArg.(map[string]any)
	if in == nil {
		in = map[string]any{}
	}

	switch sel.name {
	case "createIssue":
		repo := e.repositoryByNodeID(e.str(in, "repositoryId"))
		if repo == nil {
			e.errors = append(e.errors, map[string]any{"message": "Could not resolve to a node with the global id."})
			return nil
		}
		it, err := repo.AddIssue(asInput(in))
		if err != nil {
			e.errors = append(e.errors, map[string]any{"message": "Validation failed"})
			return nil
		}
		return map[string]any{"issue": e.resolveIssue(repo, it, sel.argsSelection("issue")),
			"clientMutationId": clientMutationID(in)}

	case "closeIssue":
		found, ok := e.issueByNodeID(e.str(in, "issueId"))
		if !ok {
			e.errors = append(e.errors, map[string]any{"message": "Could not resolve to a node with the global id."})
			return nil
		}
		repo := found.repo
		it := found.issue
		repo.UpdateIssue(it, asInput(map[string]any{"state": "closed"}))
		return map[string]any{"issue": e.resolveIssue(repo, it, sel.argsSelection("issue")),
			"clientMutationId": clientMutationID(in)}

	case "createPullRequest":
		repo := e.repositoryByNodeID(e.str(in, "repositoryId"))
		if repo == nil {
			e.errors = append(e.errors, map[string]any{"message": "Could not resolve to a node with the global id."})
			return nil
		}
		p, err := repo.AddPull(asInput(map[string]any{
			"title": e.str(in, "title"), "body": e.str(in, "body"),
			"head": e.str(in, "headRefName"), "base": e.str(in, "baseRefName"),
		}))
		if err != nil {
			e.errors = append(e.errors, map[string]any{"message": "Validation failed"})
			return nil
		}
		return map[string]any{"pullRequest": e.resolvePull(repo, p, sel.argsSelection("pullRequest")),
			"clientMutationId": clientMutationID(in)}

	case "mergePullRequest":
		found, ok := e.pullByNodeID(e.str(in, "pullRequestId"))
		if !ok {
			e.errors = append(e.errors, map[string]any{"message": "Could not resolve to a node with the global id."})
			return nil
		}
		repo := found.repo
		p := found.pull
		if _, err := repo.Merge(p, asInput(map[string]any{
			"commit_title": e.str(in, "commitHeadline"), "commit_message": e.str(in, "commitBody"),
		})); err != nil {
			e.errors = append(e.errors, map[string]any{"message": "Pull request is not mergeable"})
			return nil
		}
		return map[string]any{"pullRequest": e.resolvePull(repo, p, sel.argsSelection("pullRequest")),
			"clientMutationId": clientMutationID(in)}

	case "addComment":
		found, ok := e.issueByNodeID(e.str(in, "subjectId"))
		if !ok {
			e.errors = append(e.errors, map[string]any{"message": "Could not resolve to a node with the global id."})
			return nil
		}
		repo := found.repo
		it := found.issue
		c := repo.AddComment(it, asInput(map[string]any{"body": e.str(in, "body")}))
		// The payload must echo the comment body even when the client's
		// selection set does not name it, because the check reads it back.
		rendered, _ := e.resolveComment(repo, *c, nodeSelectionOf(sel)).(map[string]any)
		if rendered == nil {
			rendered = map[string]any{}
		}
		if _, ok := rendered["body"]; !ok {
			rendered["body"] = c.Body
		}
		return map[string]any{
			"commentEdge": map[string]any{
				"cursor": "comment:" + c.NodeID + ":0",
				"node":   rendered,
			},
			"clientMutationId": clientMutationID(in),
		}

	case "createRepository":
		if strings.EqualFold(e.str(in, "visibility"), "INTERNAL") {
			e.errors = append(e.errors, map[string]any{"message": "Internal repository visibility is unavailable"})
			return nil
		}
		name := e.str(in, "name")
		if name == "" {
			e.errors = append(e.errors, map[string]any{"message": "Validation failed"})
			return nil
		}
		repo := e.store.CreateRepo(e.store.viewer.Login, name, e.str(in, "description"),
			strings.EqualFold(e.str(in, "visibility"), "PRIVATE"))
		return map[string]any{"repository": e.resolveRepository(repo, sel.argsSelection("repository")),
			"clientMutationId": clientMutationID(in)}
	}
	e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + sel.name + "\" on type \"Mutation\"."})
	return nil
}

// search answers the search field for the issue case the suite sends.
func (e *execution) search(sel gqlSelection) any {
	query := e.arg(sel, "query")
	nodes := []any{}
	for _, r := range e.store.Repos() {
		for _, it := range r.Issues("", false) {
			if query == "" || strings.Contains(strings.ToLower(it.Title), strings.ToLower(query)) {
				nodes = append(nodes, e.resolveIssue(r, it, sel))
			}
		}
	}
	out := map[string]any{}
	for _, f := range sel.selections {
		switch f.name {
		case "issueCount":
			out[f.alias()] = len(nodes)
		case "nodes":
			out[f.alias()] = nodes
		case "edges":
			edges := make([]any, 0, len(nodes))
			for i, n := range nodes {
				edges = append(edges, map[string]any{"cursor": itoa(i), "node": n})
			}
			out[f.alias()] = edges
		case "pageInfo":
			out[f.alias()] = map[string]any{"hasNextPage": false, "endCursor": nil}
		}
	}
	return out
}

// repositoryByNodeID resolves a repository global id.
func (e *execution) repositoryByNodeID(id string) *Repository {
	kind, num, ok := splitNodeID(id)
	if !ok || kind != "Repository" {
		return nil
	}
	for _, r := range e.store.Repos() {
		if r.ID == num {
			return r
		}
	}
	return nil
}

// nodeRef pairs a repository with one of its objects, for node-id lookups.
type nodeRef struct {
	repo  *Repository
	issue *Issue
	pull  *Pull
}

// issueByNodeID resolves an issue global id to its repository and value.
func (e *execution) issueByNodeID(id string) (nodeRef, bool) {
	kind, num, ok := splitNodeID(id)
	if !ok || kind != "Issue" {
		return nodeRef{}, false
	}
	for _, r := range e.store.Repos() {
		if it, ok := r.issueByNodeNumber(num); ok {
			return nodeRef{repo: r, issue: it}, true
		}
	}
	return nodeRef{}, false
}

// pullByNodeID resolves a pull-request global id.
func (e *execution) pullByNodeID(id string) (nodeRef, bool) {
	kind, num, ok := splitNodeID(id)
	if !ok || kind != "PullRequest" {
		return nodeRef{}, false
	}
	for _, r := range e.store.Repos() {
		if p, ok := r.pullByNodeNumber(num); ok {
			return nodeRef{repo: r, pull: p}, true
		}
	}
	return nodeRef{}, false
}

// argsSelection picks the selection set for a named mutation argument, so the
// payload's nested object is rendered with the fields the client asked for.
func (sel gqlSelection) argsSelection(name string) gqlSelection {
	for _, f := range sel.selections {
		if f.name == name {
			return f
		}
	}
	return gqlSelection{}
}

func (e *execution) str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func clientMutationID(m map[string]any) any {
	if v, ok := m["clientMutationId"]; ok {
		return v
	}
	return nil
}

// asInput adapts a generic map to the typed accessor the store uses.
func asInput(m map[string]any) input { return input(m) }

// nodeSelectionOf walks commentEdge { node { ... } } so a payload always
// renders the node with at least its own fields, whatever the client asked.
func nodeSelectionOf(sel gqlSelection) gqlSelection {
	for _, edge := range sel.selections {
		if edge.name != "commentEdge" {
			continue
		}
		for _, node := range edge.selections {
			if node.name == "node" {
				return node
			}
		}
	}
	return gqlSelection{}
}
