package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// parseCommit decodes a commit object.
func parseCommit(oid string, data []byte) *Commit {
	c := &Commit{OID: oid}
	// Headers run until the first blank line, then the message.
	rest := data
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			break
		}
		line := string(rest[:nl])
		rest = rest[nl+1:]
		if line == "" {
			break
		}
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, line[7:])
		case strings.HasPrefix(line, "author "):
			c.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			c.Committer = line[10:]
		}
	}
	c.Message = string(rest)
	return c
}

// parseTree decodes a tree object into its entries.
func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	for off := 0; off < len(data); {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[off : off+sp])
		off += sp + 1
		nul := bytes.IndexByte(data[off:], 0)
		if nul < 0 {
			break
		}
		name := string(data[off : off+nul])
		off += nul + 1
		if off+20 > len(data) {
			break
		}
		oid := fmt.Sprintf("%x", data[off:off+20])
		off += 20
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return out
}

// parseTag decodes an annotated tag object.
func parseTag(data []byte) *Tag {
	t := &Tag{}
	rest := data
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			break
		}
		line := string(rest[:nl])
		rest = rest[nl+1:]
		if line == "" {
			break
		}
		switch {
		case strings.HasPrefix(line, "object "):
			t.Object = line[7:]
		case strings.HasPrefix(line, "type "):
			t.ObjectType = line[5:]
		case strings.HasPrefix(line, "tag "):
			t.TagName = line[4:]
		case strings.HasPrefix(line, "tagger "):
			t.Tagger = line[7:]
		}
	}
	t.Message = string(rest)
	return t
}

// isObjectID reports whether s is a 40-character lowercase hex object id.
func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// isDirectoryMode reports whether a tree entry names a subtree.
func isDirectoryMode(mode string) bool {
	return mode == "40000" || mode == "040000" || mode == "0040000"
}

// executableMode reports whether a tree entry is an executable file.
func executableMode(mode string) bool { return mode == "100755" }

// history walks the first-parent chain from oid, newest first, until it runs
// out of parents or reaches limit entries.
func (r *Repository) history(oid string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	for oid != "" && len(out) < limit {
		if seen[oid] {
			break
		}
		seen[oid] = true
		out = append(out, oid)
		o, ok := r.Object(oid)
		if !ok || o.Type != TypeCommit {
			break
		}
		parents := parseCommit(oid, o.Data).Parents
		if len(parents) == 0 {
			break
		}
		oid = parents[0]
	}
	return out
}

// ancestor reports whether want appears in the history reachable from from.
func (r *Repository) ancestor(want, from string) bool {
	if want == "" || from == "" {
		return false
	}
	seen := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == want {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		o, ok := r.Object(oid)
		if !ok || o.Type != TypeCommit {
			continue
		}
		stack = append(stack, parseCommit(oid, o.Data).Parents...)
	}
	return false
}

// reachable collects every object reachable from the given roots: commits,
// their trees, tree entries recursively, and tag targets.
func (r *Repository) reachable(roots []string) map[string]bool {
	out := map[string]bool{}
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || out[oid] {
			continue
		}
		o, ok := r.Object(oid)
		if !ok {
			continue
		}
		out[oid] = true
		switch o.Type {
		case TypeCommit:
			c := parseCommit(oid, o.Data)
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case TypeTree:
			for _, e := range parseTree(o.Data) {
				if e.Mode == "160000" {
					continue // gitlink: the submodule commit is not in this store
				}
				stack = append(stack, e.OID)
			}
		case TypeTag:
			stack = append(stack, parseTag(o.Data).Object)
		}
	}
	return out
}

// peel follows tag objects until a non-tag object is reached.
func (r *Repository) peel(oid string) string {
	for i := 0; i < 16; i++ {
		o, ok := r.Object(oid)
		if !ok || o.Type != TypeTag {
			return oid
		}
		next := parseTag(o.Data).Object
		if next == "" || next == oid {
			return oid
		}
		oid = next
	}
	return oid
}

// parseAuthorLine extracts the identity and epoch from a Git author line.
func parseAuthorLine(line string) (name, email string, epoch int64) {
	// "Name <email> 1700000000 +0000"
	lt := strings.LastIndexByte(line, '<')
	gt := strings.LastIndexByte(line, '>')
	if lt >= 0 && gt > lt {
		name = strings.TrimSpace(line[:lt])
		email = line[lt+1 : gt]
	}
	fields := strings.Fields(line)
	if len(fields) >= 3 {
		if v, err := strconv.ParseInt(fields[len(fields)-2], 10, 64); err == nil {
			epoch = v
		}
	}
	return name, email, epoch
}
