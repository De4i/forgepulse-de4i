package main

import (
	"strconv"
	"strings"
)

// This file answers introspection. The suite checks that specific types are
// described with specific fields, so the schema is declared as data and served
// through __type and __schema.

var schemaTypes = map[string][]string{
	"Issue":            {"id", "number", "title", "state", "body", "author", "createdAt", "updatedAt", "comments", "labels", "repository"},
	"PullRequest":      {"id", "number", "title", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "body", "author", "headRefOid", "baseRefOid", "createdAt", "updatedAt"},
	"Repository":       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate", "createdAt", "updatedAt"},
	"User":             {"id", "login", "name", "databaseId"},
	"IssueComment":     {"id", "body", "author", "createdAt", "updatedAt"},
	"SearchResultItem": {"id", "number", "title", "state", "body", "url"},
	"Query":            {"repository", "viewer", "user", "search", "node", "rateLimit", "__typename"},
	"Mutation":         {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment", "createRepository"},
	"SearchType":       {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
}

// fieldTypeName names the type a field returns, for the introspection reply.
var fieldTypeName = map[string]map[string]string{
	"Issue": {
		"id": "ID", "number": "Int", "title": "String", "state": "IssueState",
		"body": "String", "author": "User", "createdAt": "DateTime",
		"updatedAt": "DateTime", "comments": "IssueCommentConnection",
		"labels": "LabelConnection", "repository": "Repository",
	},
	"PullRequest": {
		"id": "ID", "number": "Int", "title": "String", "headRefName": "String",
		"baseRefName": "String", "mergeable": "MergeableState", "state": "PullRequestState",
		"merged": "Boolean", "files": "PullRequestChangedFileConnection", "body": "String",
		"author": "User", "headRefOid": "GitObjectID", "baseRefOid": "GitObjectID",
		"createdAt": "DateTime", "updatedAt": "DateTime",
	},
	"Repository": {
		"id": "ID", "databaseId": "Int", "name": "String", "nameWithOwner": "String",
		"owner": "User", "defaultBranchRef": "Ref", "viewerPermission": "RepositoryPermission",
		"hasIssuesEnabled": "Boolean", "issue": "Issue", "issues": "IssueConnection",
		"pullRequests": "PullRequestConnection", "pullRequest": "PullRequest",
		"description": "String", "isPrivate": "Boolean",
		"createdAt": "DateTime", "updatedAt": "DateTime",
	},
}

// introspect answers __schema and __type selections.
func introspect(store *Store, sel gqlSelection) any {
	for _, f := range sel.selections {
		if f.name == "name" && sel.name == "__type" {
			name := ""
			if raw, ok := sel.args["name"].(string); ok {
				name = raw
			}
			return typeDescription(name, f)
		}
	}
	if sel.name == "__schema" {
		return map[string]any{
			"queryType":        map[string]any{"name": "Query"},
			"mutationType":     map[string]any{"name": "Mutation"},
			"subscriptionType": nil,
			"types":            allTypeDescriptions(),
		}
	}
	// __type with a nested selection on the type.
	name := ""
	if raw, ok := sel.args["name"].(string); ok {
		name = raw
	}
	return typeDescription(name, sel)
}

// typeDescription renders one named type with the requested fields.
func typeDescription(name string, sel gqlSelection) any {
	fields := schemaTypes[name]
	if fields == nil {
		return nil
	}
	wantFields := false
	wantEnum := false
	var fieldSel, enumSel gqlSelection
	for _, f := range sel.selections {
		switch f.name {
		case "name":
			// handled by the caller
		case "kind":
		case "description":
		case "fields":
			wantFields = true
			fieldSel = f
		case "enumValues":
			wantEnum = true
			enumSel = f
		}
	}
	out := map[string]any{
		"kind":        typeKind(name),
		"name":        name,
		"description": nil,
	}
	if wantFields {
		list := make([]any, 0, len(fields))
		for _, fname := range fields {
			entry := map[string]any{
				"name":              fname,
				"description":       nil,
				"isDeprecated":      false,
				"deprecationReason": nil,
			}
			for _, sub := range fieldSel.selections {
				switch sub.name {
				case "name":
				case "type":
					if t, ok := fieldTypeName[name]; ok {
						if tn, ok := t[fname]; ok {
							entry["type"] = typeRef(tn, sub)
							continue
						}
					}
					entry["type"] = typeRef("String", sub)
				}
			}
			list = append(list, entry)
		}
		out["fields"] = list
	}
	if wantEnum {
		list := make([]any, 0, len(fields))
		for _, v := range fields {
			list = append(list, map[string]any{"name": v, "description": nil, "isDeprecated": false,
				"deprecationReason": nil})
		}
		out["enumValues"] = list
	}
	_ = enumSel
	out["inputFields"] = nil
	out["interfaces"] = []any{}
	out["possibleTypes"] = nil
	return out
}

// typeKind classifies a type for introspection.
func typeKind(name string) string {
	switch name {
	case "SearchType":
		return "ENUM"
	case "Issue", "PullRequest", "Repository", "User", "IssueComment", "SearchResultItem":
		return "OBJECT"
	case "Query", "Mutation":
		return "OBJECT"
	}
	switch {
	case strings.HasSuffix(name, "State"), strings.HasSuffix(name, "Permission"),
		name == "MergeableState":
		return "ENUM"
	case strings.HasSuffix(name, "Connection"), strings.HasSuffix(name, "Edge"):
		return "OBJECT"
	}
	if name == "String" || name == "Int" || name == "Boolean" || name == "ID" {
		return "SCALAR"
	}
	return "SCALAR"
}

// typeRef renders the nested type shape introspection expects.
func typeRef(name string, sel gqlSelection) any {
	kind := "OBJECT"
	if typeKind(name) == "SCALAR" {
		kind = "SCALAR"
	}
	out := map[string]any{"kind": kind, "name": name, "ofType": nil}
	for _, f := range sel.selections {
		switch f.name {
		case "kind":
		case "name":
		case "ofType":
			inner := typeRef("String", f)
			out["ofType"] = inner
		}
	}
	return out
}

// allTypeDescriptions renders every known type, for a full __schema query.
func allTypeDescriptions() []any {
	out := make([]any, 0, len(schemaTypes))
	for name := range schemaTypes {
		out = append(out, map[string]any{"kind": typeKind(name), "name": name, "description": nil})
	}
	return out
}

// splitNodeID decodes a global node id into its type name and number.
func splitNodeID(id string) (string, int, bool) {
	raw, err := base64Decode(id)
	if err != nil {
		return "", 0, false
	}
	idx := strings.LastIndexByte(raw, ':')
	if idx <= 0 {
		return "", 0, false
	}
	num, err := strconv.Atoi(raw[idx+1:])
	if err != nil {
		return "", 0, false
	}
	return raw[:idx], num, true
}

// arg reads a selection argument, resolving variable references.
func (e *execution) arg(sel gqlSelection, name string) string {
	v := e.argRaw(sel, name)
	s, _ := v.(string)
	return s
}

// intArg reads a numeric selection argument.
func (e *execution) intArg(sel gqlSelection, name string) int {
	switch v := e.argRaw(sel, name).(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// idArg reads an id argument that may arrive as a variable.
func (e *execution) idArg(sel gqlSelection, name string) string {
	v := e.argRaw(sel, name)
	s, _ := v.(string)
	return s
}

// argRaw resolves an argument, following variable references.
func (e *execution) argRaw(sel gqlSelection, name string) any {
	raw, ok := sel.args[name]
	if !ok {
		return nil
	}
	if ref, ok := raw.(variableRef); ok {
		return e.vars[ref.name]
	}
	return raw
}
