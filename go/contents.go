package main

import (
	"encoding/base64"
	"time"
)

// formatUnixMillis renders an epoch second value as RFC3339 with milliseconds,
// the timestamp shape the REST surface reports.
func formatUnixMillis(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// readContent returns a file's REST view at a ref. A directory path answers
// with its entries instead of a file object, which is what the contents API
// does and what the suite checks.
func (r *Repository) readContent(path, ref string) (any, error) {
	oid, ok := r.resolveRef(ref)
	if !ok {
		return nil, errNotFound()
	}
	o, ok := r.Object(oid)
	if !ok || o.Type != TypeCommit {
		return nil, errNotFound()
	}
	tree := parseCommit(oid, o.Data).Tree

	segments := splitPath(path)
	if len(segments) == 0 {
		return nil, errNotFound()
	}
	for i, want := range segments {
		t, ok := r.Object(tree)
		if !ok || t.Type != TypeTree {
			return nil, errNotFound()
		}
		var found *TreeEntry
		for _, e := range parseTree(t.Data) {
			if e.Name == want {
				entry := e
				found = &entry
				break
			}
		}
		if found == nil {
			return nil, errNotFound()
		}
		last := i == len(segments)-1
		switch {
		case isDirectoryMode(found.Mode):
			if last {
				return r.directoryView(found.OID), nil
			}
			tree = found.OID
		case last:
			blob, ok := r.Object(found.OID)
			if !ok {
				return nil, errNotFound()
			}
			joined := joinPath(segments)
			return map[string]any{
				"type": "file", "path": joined, "name": found.Name, "sha": found.OID,
				"size": len(blob.Data), "encoding": "base64",
				"content": encodeBlobContent(blob.Data),
				"url":     "/api/v3/repos/" + r.Owner.Login + "/" + r.Name + "/contents/" + joined,
			}, nil
		default:
			return nil, errNotFound()
		}
	}
	return nil, errNotFound()
}

// directoryView lists a tree's entries in the REST contents shape.
func (r *Repository) directoryView(tree string) []map[string]any {
	t, ok := r.Object(tree)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, 8)
	for _, e := range parseTree(t.Data) {
		kind := "file"
		size := 0
		if isDirectoryMode(e.Mode) {
			kind = "dir"
		} else if blob, ok := r.Object(e.OID); ok {
			size = len(blob.Data)
		}
		out = append(out, map[string]any{"type": kind, "name": e.Name, "path": e.Name,
			"sha": e.OID, "size": size})
	}
	return out
}

// editContent creates, updates or deletes a file by writing a real commit into
// the object store and moving the branch.
func (r *Repository) editContent(path string, in input, del bool) (map[string]any, int, error) {
	branch := in.strOr("branch", "")
	ref := "refs/heads/" + branch
	if branch == "" {
		ref = r.HeadRef
		branch = ref[len("refs/heads/"):]
	}
	// resolveRef takes a branch name, not a full ref path.
	lookupRef := branch
	if lookupRef == "" {
		lookupRef = r.HeadRef
	}

	// A repository starts unborn: the first contents write creates the root
	// commit, exactly as committing to a fresh GitHub repository does.
	parent, hasParent := r.Ref(ref)
	rootTree := ""
	if hasParent {
		root, ok := r.Object(parent)
		if !ok {
			return nil, 0, errNotFound()
		}
		rootTree = parseCommit(parent, root.Data).Tree
	}

	segments := splitPath(path)
	if len(segments) == 0 {
		return nil, 0, errNotFound()
	}

	// A file that is not there yet is a create; one that is there is an
	// update. Deletion of a missing file is a 404, as GitHub does.
	var blobOID string
	status := 201
	var existing any
	var err error
	if hasParent {
		existing, err = r.readContent(path, lookupRef)
	}
	switch view := existing.(type) {
	case map[string]any:
		status = 200
		if prev, ok := view["sha"].(string); ok {
			if want := in.strOr("sha", ""); want != "" && want != prev {
				return nil, 0, &Fault{Status: 409, Message: "sha does not match"}
			}
		}
	default:
		// A directory at that path cannot be written over.
		if existing != nil {
			return nil, 0, &Fault{Status: 409, Message: "path is a directory"}
		}
		if del {
			return nil, 0, errNotFound()
		}
	}

	if !del {
		raw, has := in.str("content")
		if !has {
			return nil, 0, &Fault{Status: 422, Message: "content is required"}
		}
		decoded, err := decodeBase64Loose(raw)
		if err != nil {
			return nil, 0, &Fault{Status: 422, Message: "content is not base64"}
		}
		blobOID = r.PutObject(TypeBlob, decoded)
	} else {
		blobOID = ""
	}

	newTree, err := r.spliceTree(rootTree, segments, blobOID, !del)
	if err != nil {
		return nil, 0, err
	}
	message := in.strOr("message", "Update "+path)

	r.owner.mu.Lock()
	stamp := r.owner.stamp()
	r.owner.mu.Unlock()
	author := r.owner.viewer.Login + " <" + r.owner.viewer.Login + "@users.noreply.invalid> " + epochOf(stamp) + " +0000"
	var sb []byte
	sb = append(sb, "tree "...)
	sb = append(sb, newTree...)
	sb = append(sb, '\n')
	if hasParent {
		sb = append(sb, "parent "...)
		sb = append(sb, parent...)
		sb = append(sb, '\n')
	}
	sb = append(sb, "author "...)
	sb = append(sb, author...)
	sb = append(sb, '\n')
	sb = append(sb, "committer "...)
	sb = append(sb, author...)
	sb = append(sb, "\n\n"...)
	sb = append(sb, message...)
	sb = append(sb, '\n')
	commit := r.PutObject(TypeCommit, sb)

	r.SetRef(ref, commit)
	r.owner.mu.Lock()
	r.UpdatedAt = stamp
	r.owner.mu.Unlock()

	joined := joinPath(segments)
	head := map[string]any{"label": r.Owner.Login + ":" + branch, "ref": branch, "sha": commit}
	// The reply carries the new content object in the same shape a GET of it
	// returns, including its encoding: the suite validates that shape.
	contentView := map[string]any{
		"type": "file", "name": segments[len(segments)-1], "path": joined,
		"sha": blobOID, "size": 0, "encoding": "base64", "content": "",
		"url": "/api/v3/repos/" + r.Owner.Login + "/" + r.Name + "/contents/" + joined,
	}
	if !del {
		if blob, ok := r.Object(blobOID); ok {
			contentView["size"] = len(blob.Data)
			contentView["content"] = encodeBlobContent(blob.Data)
		}
	}
	view := map[string]any{
		"content": contentView,
		"commit": map[string]any{"sha": commit, "message": message,
			"html_url": "/" + r.Owner.Login + "/" + r.Name + "/commit/" + commit},
	}
	if del {
		view["content"] = nil
	}
	if status == 200 {
		view["commit"] = map[string]any{"sha": commit, "message": message,
			"html_url": "/" + r.Owner.Login + "/" + r.Name + "/commit/" + commit}
	}
	_ = head
	return view, status, nil
}

// spliceTree rewrites the tree path so it contains (or no longer contains) the
// named blob, creating intermediate trees as needed.
func (r *Repository) spliceTree(root string, segments []string, blobOID string, add bool) (string, error) {
	entries, err := r.treeEntries(root)
	if err != nil {
		return "", err
	}
	name := segments[0]
	var replaced []TreeEntry
	found := false
	for _, e := range entries {
		if e.Name != name {
			replaced = append(replaced, e)
			continue
		}
		found = true
		if len(segments) == 1 {
			if add {
				replaced = append(replaced, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
			}
			continue
		}
		sub, err := r.spliceTree(e.OID, segments[1:], blobOID, add)
		if err != nil {
			return "", err
		}
		replaced = append(replaced, TreeEntry{Mode: "40000", Name: name, OID: sub})
	}
	if !found && add {
		if len(segments) == 1 {
			replaced = append(replaced, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
		} else {
			sub, err := r.spliceTree("", segments[1:], blobOID, add)
			if err != nil {
				return "", err
			}
			replaced = append(replaced, TreeEntry{Mode: "40000", Name: name, OID: sub})
		}
	}
	return r.PutObject(TypeTree, renderTree(replaced)), nil
}

// treeEntries reads a tree's entries, treating a missing tree as empty.
func (r *Repository) treeEntries(oid string) ([]TreeEntry, error) {
	if oid == "" {
		return nil, nil
	}
	o, ok := r.Object(oid)
	if !ok {
		return nil, nil
	}
	return parseTree(o.Data), nil
}

// renderTree serializes entries into a tree object body, keeping Git's own
// ordering rules so the resulting hashes match a real Git tree.
func renderTree(entries []TreeEntry) []byte {
	sortTreeEntries(entries)
	var out []byte
	for _, e := range entries {
		out = append(out, e.Mode...)
		out = append(out, ' ')
		out = append(out, e.Name...)
		out = append(out, 0)
		out = append(out, decodeHexOID(e.OID)...)
	}
	return out
}

// decodeHexOID turns a 40-character hex id into its 20 raw bytes.
func decodeHexOID(hexID string) []byte {
	out := make([]byte, 20)
	for i := 0; i < 20 && 2*i+1 < len(hexID); i++ {
		out[i] = hexNibble(hexID[2*i])<<4 | hexNibble(hexID[2*i+1])
	}
	return out
}

// decodeBase64Loose accepts both padded and raw base64, and tolerates the
// newlines a caller may have inserted while building the payload.
func decodeBase64Loose(s string) ([]byte, error) {
	clean := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n', '\r', ' ':
			continue
		}
		clean = append(clean, s[i])
	}
	if data, err := base64.StdEncoding.DecodeString(string(clean)); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(string(clean))
}

// joinPath re-joins path segments.
func joinPath(segments []string) string {
	out := ""
	for i, s := range segments {
		if i > 0 {
			out += "/"
		}
		out += s
	}
	return out
}
