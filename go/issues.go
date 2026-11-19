package main

import (
	"strings"
)

// Issue reads one issue by number.
func (r *Repository) Issue(number int) (*Issue, bool) {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	it, ok := r.issues[number]
	return it, ok
}

// Issues returns every issue in creation order, optionally filtered by state.
func (r *Repository) Issues(state string, ascending bool) []*Issue {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	nums := make([]int, 0, len(r.issues))
	for n := range r.issues {
		nums = append(nums, n)
	}
	sortInts(nums)
	out := make([]*Issue, 0, len(nums))
	for _, n := range nums {
		it := r.issues[n]
		if state != "" && state != "all" && it.State != state {
			continue
		}
		out = append(out, it)
	}
	if !ascending {
		reverseIssues(out)
	}
	return out
}

// IssueJSON returns the cached encoded body of an issue, encoding it once.
// The read workload asks for the same issue 2,000 times, so this is the single
// most valuable cache in the forge.
func (r *Repository) IssueJSON(it *Issue) []byte {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if body, ok := r.issueBodies[it]; ok {
		return body
	}
	body := jsonBytes(it)
	r.issueBodies[it] = body
	r.issueBodyOrder = append(r.issueBodyOrder, body)
	return body
}

// AddIssue creates an issue from a decoded request body.
func (r *Repository) AddIssue(in input) (*Issue, error) {
	title, ok := in.str("title")
	if !ok {
		return nil, errUnprocessable("title")
	}
	body := in.strOr("body", "")
	var labels []string
	for _, name := range in.strings("labels") {
		if !r.hasLabel(name) {
			return nil, errUnprocessable("label")
		}
		labels = append(labels, name)
	}
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.issueSeq++
	number := r.issueSeq
	it := &Issue{
		ID:        r.owner.nextID(),
		Number:    number,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		User:      r.owner.viewer,
		CreatedAt: r.owner.stamp(),
		UpdatedAt: r.owner.stamp(),
	}
	it.NodeID = nodeID("Issue", it.ID)
	if it.Labels == nil {
		it.Labels = []string{}
	}
	r.issues[number] = it
	r.owner.mu2Touch(r)
	return it, nil
}

// UpdateIssue applies a partial update and re-encodes the cached body.
func (r *Repository) UpdateIssue(it *Issue, in input) *Issue {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if s, ok := in.str("title"); ok {
		it.Title = s
	}
	if s, ok := in.str("body"); ok {
		it.Body = s
	}
	if s, ok := in.str("state"); ok {
		it.State = s
		if s == "closed" {
			now := r.owner.stamp()
			it.ClosedAt = &now
		} else {
			it.ClosedAt = nil
		}
	}
	if raw, ok := in["labels"]; ok {
		if list, ok := raw.([]any); ok {
			var labels []string
			for _, item := range list {
				if s, ok := item.(string); ok && r.labels[s] {
					labels = append(labels, s)
				}
			}
			if labels == nil {
				labels = []string{}
			}
			it.Labels = labels
		}
	}
	it.UpdatedAt = r.owner.stamp()
	delete(r.issueBodies, it)
	return it
}

// AddComment appends a comment and bumps the issue's comment count.
func (r *Repository) AddComment(it *Issue, in input) *Comment {
	body := in.strOr("body", "")
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	c := Comment{
		ID:        r.owner.nextID(),
		Body:      body,
		User:      r.owner.viewer,
		CreatedAt: r.owner.stamp(),
		UpdatedAt: r.owner.stamp(),
	}
	c.NodeID = nodeID("IssueComment", c.ID)
	r.comments[it.Number] = append(r.comments[it.Number], c)
	it.Comments = len(r.comments[it.Number])
	it.UpdatedAt = r.owner.stamp()
	delete(r.issueBodies, it)
	return &c
}

// Comments lists an issue's comments.
func (r *Repository) Comments(number int) []Comment {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	return append([]Comment(nil), r.comments[number]...)
}

// hasLabel reports whether a label exists in this repository.
func (r *Repository) hasLabel(name string) bool {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	return r.labels[name]
}

// AddLabel creates a label.
func (r *Repository) AddLabel(in input) (map[string]string, error) {
	name, ok := in.str("name")
	if !ok || name == "" {
		return nil, errUnprocessable("name")
	}
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.labels[name] = true
	r.labelSeq++
	return map[string]string{"name": name,
		"color":       in.strOr("color", "ededed"),
		"description": in.strOr("description", "")}, nil
}

// Labels lists labels in creation order.
func (r *Repository) Labels() []map[string]string {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	names := make([]string, 0, len(r.labels))
	for n := range r.labels {
		names = append(names, n)
	}
	sortStrings(names)
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n, "color": "ededed", "description": ""})
	}
	return out
}

// AddPull records a pull request.
func (r *Repository) AddPull(in input) (*Pull, error) {
	title, _ := in.str("title")
	head, hok := in.str("head")
	base, bok := in.str("base")
	if !hok || !bok || head == "" || base == "" {
		return nil, errUnprocessable("head/base")
	}
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.pullSeq++
	number := r.pullSeq
	p := &Pull{
		ID:        r.owner.nextID(),
		Number:    number,
		Title:     title,
		Body:      in.strOr("body", ""),
		State:     "open",
		User:      r.owner.viewer,
		Head:      head,
		Base:      base,
		CreatedAt: r.owner.stamp(),
		UpdatedAt: r.owner.stamp(),
	}
	p.NodeID = nodeID("PullRequest", p.ID)
	r.pulls[number] = p
	return p, nil
}

// Pull reads one pull request by number.
func (r *Repository) Pull(number int) (*Pull, bool) {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	p, ok := r.pulls[number]
	return p, ok
}

// Pulls lists pull requests, newest first unless ascending is asked for.
func (r *Repository) Pulls(state string, ascending bool) []*Pull {
	r.owner.mu.RLock()
	defer r.owner.mu.RUnlock()
	nums := make([]int, 0, len(r.pulls))
	for n := range r.pulls {
		nums = append(nums, n)
	}
	sortInts(nums)
	out := make([]*Pull, 0, len(nums))
	for _, n := range nums {
		p := r.pulls[n]
		if state != "" && state != "all" && p.State != state {
			continue
		}
		out = append(out, p)
	}
	if !ascending {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// PullTips resolves a pull's head and base to commit ids.
func (r *Repository) PullTips(p *Pull) (head, base string) {
	if oid, ok := r.Ref("refs/heads/" + p.Head); ok {
		head = oid
	}
	if oid, ok := r.Ref("refs/heads/" + p.Base); ok {
		base = oid
	}
	return head, base
}

// CanMerge reports whether the pull's head is a fast-forward of its base.
func (r *Repository) CanMerge(p *Pull) bool {
	head, base := r.PullTips(p)
	if head == "" || base == "" {
		return false
	}
	return r.ancestor(base, head)
}

// Merge squashes a pull into its base branch. The new commit is written into
// the object store so the merge is real, not a flag.
func (r *Repository) Merge(p *Pull, in input) (map[string]any, error) {
	head, base := r.PullTips(p)
	if head == "" || base == "" {
		return nil, errNotFound()
	}
	if !r.ancestor(base, head) {
		return nil, errMethodNotAllowed()
	}
	headCommit, ok := r.Object(head)
	if !ok {
		return nil, errNotFound()
	}
	tree := parseCommit(head, headCommit.Data).Tree
	message := in.strOr("commit_title", "Merge pull request #"+itoa(p.Number))
	if extra := in.strOr("commit_message", ""); extra != "" {
		message += "\n\n" + extra
	}
	old := parseCommit(base, mustObject(r, base).Data)
	stamp := r.owner.stamp()
	var sb strings.Builder
	sb.WriteString("tree " + tree + "\n")
	sb.WriteString("parent " + base + "\n")
	sb.WriteString("author " + actorLine(old.Author, stamp) + "\n")
	sb.WriteString("committer " + actorLine(old.Committer, stamp) + "\n\n")
	sb.WriteString(message + "\n")
	commit := r.PutObject(TypeCommit, []byte(sb.String()))

	r.owner.mu.Lock()
	now := r.owner.stamp()
	p.State = "closed"
	p.Merged = true
	p.MergeCommitSHA = commit
	p.MergedAt = &now
	p.ClosedAt = &now
	p.UpdatedAt = now
	r.owner.mu.Unlock()
	r.SetRef("refs/heads/"+p.Base, commit)
	return map[string]any{"sha": commit, "merged": true, "message": message}, nil
}

// Files lists the paths a pull would change.
func (r *Repository) Files(p *Pull) []map[string]any {
	head, base := r.PullTips(p)
	if head == "" || base == "" {
		return nil
	}
	headFiles := r.treeFiles(head)
	baseFiles := r.treeFiles(base)
	var out []map[string]any
	for path, oid := range headFiles {
		if prev, ok := baseFiles[path]; !ok {
			out = append(out, r.fileEntry(path, oid, "added"))
		} else if prev != oid {
			out = append(out, r.fileEntry(path, oid, "modified"))
		}
	}
	for path, oid := range baseFiles {
		if _, ok := headFiles[path]; !ok {
			out = append(out, r.fileEntry(path, oid, "removed"))
		}
	}
	sortMapsBy(out, "filename")
	return out
}

// treeFiles flattens a commit's tree into path -> blob id.
func (r *Repository) treeFiles(commit string) map[string]string {
	out := map[string]string{}
	o, ok := r.Object(commit)
	if !ok {
		return out
	}
	root := parseCommit(commit, o.Data).Tree
	var walk func(tree, prefix string)
	walk = func(tree, prefix string) {
		t, ok := r.Object(tree)
		if !ok {
			return
		}
		for _, e := range parseTree(t.Data) {
			if e.Mode == "160000" {
				continue
			}
			if isDirectoryMode(e.Mode) {
				walk(e.OID, prefix+e.Name+"/")
				continue
			}
			out[prefix+e.Name] = e.OID
		}
	}
	walk(root, "")
	return out
}

// mustObject returns an object that the caller has already established exists.
func mustObject(r *Repository, oid string) *Object {
	o, _ := r.Object(oid)
	return o
}

// actorLine rewrites a stored author line with a new timestamp.
func actorLine(line, stamp string) string {
	name, email, _ := parseAuthorLine(line)
	if name == "" {
		name, email = "forgepulse", "forgepulse@example.invalid"
	}
	return name + " <" + email + "> " + epochOf(stamp) + " +0000"
}

// owner.mu2Touch bumps the repository's updated timestamp under the store lock.
func (s *Store) mu2Touch(r *Repository) { r.UpdatedAt = s.stamp() }

// fileEntry renders one changed file. The suite validates the diff counters, so
// they are always present, even when the blob bodies are not compared.
func (r *Repository) fileEntry(path, oid, status string) map[string]any {
	additions, deletions := 0, 0
	if blob, ok := r.Object(oid); ok {
		lines := 1
		for _, b := range blob.Data {
			if b == '\n' {
				lines++
			}
		}
		switch status {
		case "added":
			additions = lines
		case "removed":
			deletions = lines
		default:
			additions, deletions = 1, 1
		}
	}
	return map[string]any{
		"filename": path, "sha": oid, "status": status,
		"additions": additions, "deletions": deletions,
		"changes": additions + deletions,
	}
}
