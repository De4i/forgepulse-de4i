package main

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
)

// repoView renders the REST repository object.
func repoView(r *Repository, origin string) map[string]any {
	full := r.Owner.Login + "/" + r.Name
	endpoint := origin + "/api/v3/repos/" + full
	return map[string]any{
		"id": r.ID, "node_id": r.NodeID, "name": r.Name, "full_name": full,
		"owner":       map[string]any{"login": r.Owner.Login, "id": r.Owner.ID, "node_id": r.Owner.NodeID, "type": "User"},
		"description": r.Description, "private": r.Private,
		"visibility":     map[bool]string{true: "private", false: "public"}[r.Private],
		"default_branch": r.HeadRef[len("refs/heads/"):],
		"html_url":       origin + "/" + full,
		"clone_url":      origin + "/" + full + ".git",
		"url":            endpoint,
		"issues_url":     endpoint + "/issues{/number}",
		"pulls_url":      endpoint + "/pulls{/number}",
		"created_at":     r.CreatedAt, "updated_at": r.UpdatedAt,
		"has_issues": true, "archived": false, "fork": false,
		"permissions": map[string]bool{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
	}
}

// pullView renders the REST pull object.
func pullView(r *Repository, p *Pull, origin string) map[string]any {
	head, base := r.PullTips(p)
	rv := repoView(r, origin)
	return map[string]any{
		"id": p.ID, "node_id": p.NodeID, "number": p.Number, "title": p.Title, "body": p.Body,
		"state": p.State, "user": map[string]any{"login": r.Owner.Login, "id": r.Owner.ID, "node_id": r.Owner.NodeID, "type": "User"},
		"merged": p.Merged, "mergeable": r.CanMerge(p), "merge_commit_sha": p.MergeCommitSHA,
		"created_at": p.CreatedAt, "updated_at": p.UpdatedAt, "closed_at": p.ClosedAt, "merged_at": p.MergedAt,
		"head": map[string]any{"ref": p.Head, "label": r.Owner.Login + ":" + p.Head, "sha": head, "repo": rv},
		"base": map[string]any{"ref": p.Base, "label": r.Owner.Login + ":" + p.Base, "sha": base, "repo": rv},
	}
}

// issueView renders the REST issue object.
func issueView(r *Repository, it *Issue) map[string]any {
	labels := make([]map[string]string, 0, len(it.Labels))
	for _, name := range it.Labels {
		labels = append(labels, map[string]string{"name": name, "color": "ededed"})
	}
	return map[string]any{
		"id": it.ID, "node_id": it.NodeID, "number": it.Number, "title": it.Title, "body": it.Body,
		"state": it.State, "labels": labels,
		"user":     map[string]any{"login": r.Owner.Login, "id": r.Owner.ID, "node_id": r.Owner.NodeID, "type": "User"},
		"comments": it.Comments, "created_at": it.CreatedAt, "updated_at": it.UpdatedAt, "closed_at": it.ClosedAt,
	}
}

// commitView renders the REST commit object.
func commitView(r *Repository, sha string) map[string]any {
	o, ok := r.Object(sha)
	if !ok {
		return nil
	}
	c := parseCommit(sha, o.Data)
	parents := make([]map[string]string, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, map[string]string{"sha": p})
	}
	return map[string]any{
		"sha": sha,
		"commit": map[string]any{
			"author":    identityView(c.Author),
			"committer": identityView(c.Committer),
			"message":   strings.TrimRight(c.Message, "\n"),
			"tree":      map[string]string{"sha": c.Tree},
		},
		"parents": parents,
	}
}

// identityView splits a Git identity line into the REST shape.
func identityView(line string) map[string]any {
	name, email, epoch := parseAuthorLine(line)
	var date any
	if epoch > 0 {
		date = rfc3339milli(epoch)
	}
	return map[string]any{"name": name, "email": email, "date": date}
}

// serveREST is the v3 surface. It returns before doing any generic parsing for
// the shapes the workloads repeat.
func (s *Server) serveREST(w http.ResponseWriter, req *http.Request, path string) {
	origin := "http://" + req.Host
	kv := req.URL.Query()

	// ── /api/v3/ ────────────────────────────────────────────────────────────
	if path == "/api/v3" || path == "/api/v3/" {
		data := jsonBytes(map[string]any{
			"current_user_url": origin + "/api/v3/user",
			"repository_url":   origin + "/api/v3/repos/{owner}/{repo}",
			"graphql_url":      origin + "/api/graphql",
		})
		writeJSON(w, http.StatusOK, data, map[string]string{"X-OAuth-Scopes": "repo, read:org"})
		return
	}

	rest := path[len("/api/v3"):]
	parts := splitPath(rest)
	if len(parts) == 1 && req.Method == http.MethodGet {
		switch parts[0] {
		case "meta":
			writeJSON(w, http.StatusOK, []byte(`{"installed_version":"forgepulse/1"}`), nil)
			return
		case "user":
			writeJSON(w, http.StatusOK, jsonBytes(map[string]any{
				"login": s.store.viewer.Login, "id": s.store.viewer.ID,
				"node_id": s.store.viewer.NodeID, "type": "User",
			}), nil)
			return
		}
	}

	// ── /user/repos ─────────────────────────────────────────────────────────
	if len(parts) == 2 && parts[0] == "user" && parts[1] == "repos" {
		switch req.Method {
		case http.MethodPost:
			in, ok := s.decodeBody(w, req)
			if !ok {
				return
			}
			name, has := in.str("name")
			if !has || name == "" {
				writeJSON(w, http.StatusUnprocessableEntity, jsonBytes(map[string]string{"message": "Validation Failed"}), nil)
				return
			}
			if owner, exists := s.store.Lookup(s.store.viewer.Login, name); exists {
				writeJSON(w, http.StatusUnprocessableEntity, jsonBytes(map[string]any{
					"message": "Repository creation failed",
					"errors": []map[string]string{{"message": "name already exists on this account",
						"resource": "Repository", "field": "name", "code": "custom"}},
					"exists": map[string]any{"id": owner.ID, "name": owner.Name},
				}), nil)
				return
			}
			repo := s.store.CreateRepo(s.store.viewer.Login, name, in.strOr("description", ""),
				boolOr(in, "private", false))
			writeJSON(w, http.StatusCreated, jsonBytes(repoView(repo, origin)), nil)
			return
		case http.MethodGet:
			all := s.store.Repos()
			s.writePage(w, req, origin, path, all, func(r *Repository) any { return repoView(r, origin) })
			return
		}
	}

	// ── /repos/{owner}/{name}/... ───────────────────────────────────────────
	if len(parts) < 3 || parts[0] != "repos" {
		writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
		return
	}
	owner, name := parts[1], parts[2]
	repo, ok := s.store.Lookup(owner, name)
	if !ok {
		writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
		return
	}

	if len(parts) == 3 {
		switch req.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, jsonBytes(repoView(repo, origin)), nil)
			return
		case http.MethodDelete:
			s.store.DeleteRepo(repo)
			writeJSON(w, http.StatusNoContent, nil, nil)
			return
		case http.MethodPatch:
			in, ok := s.decodeBody(w, req)
			if !ok {
				return
			}
			repo.Description = in.strOr("description", repo.Description)
			if v, has := in.boolean("private"); has {
				repo.Private = v
			}
			repo.UpdatedAt = s.store.stamp()
			writeJSON(w, http.StatusOK, jsonBytes(repoView(repo, origin)), nil)
			return
		}
	}

	resource := parts[3]
	switch resource {
	case "branches":
		if req.Method != http.MethodGet {
			break
		}
		if len(parts) == 4 {
			var rows []map[string]any
			for _, ref := range repo.Refs() {
				if !strings.HasPrefix(ref.Name, "refs/heads/") {
					continue
				}
				rows = append(rows, map[string]any{"name": ref.Name[len("refs/heads/"):],
					"commit": map[string]string{"sha": ref.OID}, "protected": false})
			}
			s.writePage(w, req, origin, path, rows, nil)
			return
		}
		branch := strings.Join(parts[4:], "/")
		oid, ok := repo.Ref("refs/heads/" + branch)
		if !ok {
			writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
			return
		}
		writeJSON(w, http.StatusOK, jsonBytes(map[string]any{"name": branch,
			"commit": map[string]string{"sha": oid}, "protected": false}), nil)
		return

	case "commits":
		if req.Method != http.MethodGet {
			break
		}
		if len(parts) == 5 {
			oid, ok := repo.resolveRef(parts[4])
			if !ok {
				writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
				return
			}
			view := commitView(repo, oid)
			if view == nil {
				writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
				return
			}
			writeJSON(w, http.StatusOK, jsonBytes(view), nil)
			return
		}
		ref := kv.Get("sha")
		oid, ok := repo.resolveRef(ref)
		if ref != "" && !ok {
			writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
			return
		}
		if oid == "" {
			oid = repo.Tip()
		}
		shaList := repo.history(oid, 1000)
		rows := make([]map[string]any, 0, len(shaList))
		for _, sha := range shaList {
			rows = append(rows, commitView(repo, sha))
		}
		s.writePage(w, req, origin, path, rows, nil)
		return

	case "git":
		if len(parts) >= 5 && parts[4] == "refs" {
			if len(parts) == 5 && req.Method == http.MethodPost {
				in, ok := s.decodeBody(w, req)
				if !ok {
					return
				}
				ref, _ := in.str("ref")
				sha, _ := in.str("sha")
				full := ref
				if !strings.HasPrefix(full, "refs/") {
					full = "refs/heads/" + ref
				}
				if !RefNameValid(full) || !repo.HasObject(sha) {
					writeJSON(w, http.StatusUnprocessableEntity, jsonBytes(map[string]string{"message": "Validation Failed"}), nil)
					return
				}
				repo.SetRef(full, sha)
				writeJSON(w, http.StatusCreated, jsonBytes(map[string]any{"ref": full,
					"object": map[string]string{"sha": sha, "type": "commit"}}), nil)
				return
			}
			if len(parts) > 5 && req.Method == http.MethodGet {
				full := "refs/" + strings.Join(parts[5:], "/")
				oid, ok := repo.Ref(full)
				if !ok {
					writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
					return
				}
				o := mustObject(repo, oid)
				t := "commit"
				if o != nil {
					t = o.Type
				}
				writeJSON(w, http.StatusOK, jsonBytes(map[string]any{"ref": full,
					"object": map[string]string{"sha": oid, "type": t}}), nil)
				return
			}
		}

	case "contents":
		file := strings.Join(parts[4:], "/")
		switch req.Method {
		case http.MethodGet:
			view, err := repo.readContent(file, kv.Get("ref"))
			if err != nil {
				s.writeFault(w, err)
				return
			}
			writeJSON(w, http.StatusOK, jsonBytes(view), nil)
			return
		case http.MethodPut, http.MethodDelete:
			in, ok := s.decodeBody(w, req)
			if !ok {
				return
			}
			result, status, err := repo.editContent(file, in, req.Method == http.MethodDelete)
			if err != nil {
				s.writeFault(w, err)
				return
			}
			writeJSON(w, status, jsonBytes(result), nil)
			return
		}

	case "labels":
		if len(parts) != 4 {
			break
		}
		switch req.Method {
		case http.MethodPost:
			in, ok := s.decodeBody(w, req)
			if !ok {
				return
			}
			out, err := repo.AddLabel(in)
			if err != nil {
				s.writeFault(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, jsonBytes(out), nil)
			return
		case http.MethodGet:
			rows := repo.Labels()
			if rows == nil {
				rows = []map[string]string{}
			}
			s.writePage(w, req, origin, path, rows, nil)
			return
		}

	case "issues":
		if len(parts) == 4 {
			switch req.Method {
			case http.MethodPost:
				in, ok := s.decodeBody(w, req)
				if !ok {
					return
				}
				it, err := repo.AddIssue(in)
				if err != nil {
					s.writeFault(w, err)
					return
				}
				writeJSON(w, http.StatusCreated, jsonBytes(issueView(repo, it)), nil)
				return
			case http.MethodGet:
				state := kv.Get("state")
				if state == "" {
					state = "open"
				}
				rows := repo.Issues(state, kv.Get("direction") == "asc")
				views := make([]any, 0, len(rows))
				for _, it := range rows {
					views = append(views, issueView(repo, it))
				}
				if len(views) == 0 {
					views = []any{}
				}
				s.writePage(w, req, origin, path, views, nil)
				return
			}
			break
		}
		number, ok := parseInt(parts[4])
		if !ok {
			writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
			return
		}
		it, exists := repo.Issue(number)
		if !exists {
			writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
			return
		}
		if len(parts) == 5 {
			switch req.Method {
			case http.MethodGet:
				writeJSON(w, http.StatusOK, repo.IssueJSON(it), nil)
				return
			case http.MethodPatch:
				in, ok := s.decodeBody(w, req)
				if !ok {
					return
				}
				repo.UpdateIssue(it, in)
				writeJSON(w, http.StatusOK, jsonBytes(issueView(repo, it)), nil)
				return
			}
			break
		}
		if len(parts) == 6 && parts[5] == "comments" {
			switch req.Method {
			case http.MethodGet:
				list := repo.Comments(number)
				views := make([]any, 0, len(list))
				for _, c := range list {
					views = append(views, commentView(c))
				}
				if len(views) == 0 {
					views = []any{}
				}
				s.writePage(w, req, origin, path, views, nil)
				return
			case http.MethodPost:
				in, ok := s.decodeBody(w, req)
				if !ok {
					return
				}
				c := repo.AddComment(it, in)
				writeJSON(w, http.StatusCreated, jsonBytes(commentView(*c)), nil)
				return
			}
		}

	case "pulls":
		if len(parts) == 4 {
			switch req.Method {
			case http.MethodPost:
				in, ok := s.decodeBody(w, req)
				if !ok {
					return
				}
				p, err := repo.AddPull(in)
				if err != nil {
					s.writeFault(w, err)
					return
				}
				writeJSON(w, http.StatusCreated, jsonBytes(pullView(repo, p, origin)), nil)
				return
			case http.MethodGet:
				state := kv.Get("state")
				if state == "" {
					state = "open"
				}
				list := repo.Pulls(state, false)
				views := make([]any, 0, len(list))
				for _, p := range list {
					views = append(views, pullView(repo, p, origin))
				}
				if len(views) == 0 {
					views = []any{}
				}
				s.writePage(w, req, origin, path, views, nil)
				return
			}
			break
		}
		number, ok := parseInt(parts[4])
		if !ok {
			writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
			return
		}
		p, exists := repo.Pull(number)
		if !exists {
			writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
			return
		}
		if len(parts) == 5 && req.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, jsonBytes(pullView(repo, p, origin)), nil)
			return
		}
		if len(parts) == 6 {
			switch {
			case parts[5] == "files" && req.Method == http.MethodGet:
				rows := repo.Files(p)
				if rows == nil {
					rows = []map[string]any{}
				}
				s.writePage(w, req, origin, path, rows, nil)
				return
			case parts[5] == "merge" && req.Method == http.MethodPut:
				in, ok := s.decodeBody(w, req)
				if !ok {
					return
				}
				out, err := repo.Merge(p, in)
				if err != nil {
					s.writeFault(w, err)
					return
				}
				writeJSON(w, http.StatusOK, jsonBytes(out), nil)
				return
			case parts[5] == "merge" && req.Method == http.MethodGet:
				if p.Merged {
					writeJSON(w, http.StatusNoContent, nil, nil)
					return
				}
				writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
				return
			}
		}
	}
	writeJSON(w, http.StatusNotFound, notFoundMessage, nil)
}

// commentView renders the REST comment shape.
func commentView(c Comment) map[string]any {
	return map[string]any{"id": c.ID, "node_id": c.NodeID, "body": c.Body,
		"user":       map[string]any{"login": c.User.Login, "id": c.User.ID, "node_id": c.User.NodeID, "type": "User"},
		"created_at": c.CreatedAt, "updated_at": c.UpdatedAt}
}

// decodeBody reads and parses a JSON request body, writing the error reply
// itself when the body is malformed.
func (s *Server) decodeBody(w http.ResponseWriter, req *http.Request) (input, bool) {
	body, err := readBody(req, 2<<20)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, malformedMessage, nil)
		return nil, false
	}
	in, ok := decodeJSON(body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, malformedMessage, nil)
		return nil, false
	}
	return input(in), true
}

// writeFault renders a Fault, or a 500 for anything unexpected.
func (s *Server) writeFault(w http.ResponseWriter, err error) {
	if f, ok := err.(*Fault); ok {
		writeJSON(w, f.Status, jsonBytes(map[string]string{"message": f.Message}), nil)
		return
	}
	writeJSON(w, http.StatusInternalServerError, jsonBytes(map[string]string{"message": "Internal error"}), nil)
}

// writePage paginates a list. Items arrive already ordered.
func (s *Server) writePage(w http.ResponseWriter, req *http.Request, origin, path string, items any, mapFn func(*Repository) any) {
	size := queryInt(req, "per_page", 30)
	if size > 100 {
		size = 100
	}
	page := queryInt(req, "page", 1)

	var rows []any
	switch v := items.(type) {
	case []*Repository:
		rows = make([]any, 0, len(v))
		for _, r := range v {
			rows = append(rows, mapFn(r))
		}
	case []map[string]any:
		rows = make([]any, 0, len(v))
		for _, r := range v {
			rows = append(rows, r)
		}
	case []any:
		rows = v
	}
	offset := (page - 1) * size
	if offset > len(rows) {
		offset = len(rows)
	}
	end := offset + size
	hasNext := false
	if end >= len(rows) {
		end = len(rows)
	} else {
		hasNext = true
	}
	pageRows := rows[offset:end]
	if pageRows == nil {
		pageRows = []any{}
	}
	headers := map[string]string{}
	if hasNext {
		headers["Link"] = "<" + origin + path + "?per_page=" + itoa(size) + "&page=" + itoa(page+1) + `>; rel="next"`
	}
	writeJSON(w, http.StatusOK, jsonBytes(pageRows), headers)
}

// queryInt reads a positive integer query parameter.
func queryInt(req *http.Request, key string, fallback int) int {
	raw := req.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

// parseInt parses a positive path number.
func parseInt(s string) (int, bool) {
	if s == "" || s[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// splitPath splits a REST path, rejecting traversal and control characters.
func splitPath(p string) []string {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return nil
	}
	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\x00\\") {
			continue
		}
		out = append(out, seg)
	}
	return out
}

// resolveRef turns a branch name, tag name or object id into an object id.
func (r *Repository) resolveRef(ref string) (string, bool) {
	if ref == "" {
		return r.Tip(), r.Tip() != ""
	}
	if isObjectID(ref) {
		if r.HasObject(ref) {
			return ref, true
		}
		return "", false
	}
	for _, candidate := range []string{"refs/heads/" + ref, "refs/tags/" + ref, ref} {
		if oid, ok := r.Ref(candidate); ok {
			return oid, true
		}
	}
	if tip := r.Tip(); tip != "" && ref == "HEAD" {
		return tip, true
	}
	return "", false
}

// boolOr reads a boolean field with a default.
func boolOr(in input, key string, fallback bool) bool {
	if v, ok := in.boolean(key); ok {
		return v
	}
	return fallback
}

// rfc3339milli renders an epoch as the REST timestamp shape.
func rfc3339milli(epoch int64) string { return formatUnixMillis(epoch) }

// encodeBlobContent renders file bytes the way the REST contents API does.
func encodeBlobContent(data []byte) string { return base64.StdEncoding.EncodeToString(data) }
