package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

const (
	serviceUpload  = "git-upload-pack"
	serviceReceive = "git-receive-pack"
	agentID        = "agent=forgepulse/1"
)

var (
	receiveCaps = []string{"report-status", "side-band-64k", "delete-refs", "ofs-delta", agentID, "object-format=sha1"}
	uploadCaps  = []string{"side-band-64k", "shallow", "no-progress", "include-tag", "ofs-delta", agentID, "object-format=sha1"}
)

// v2 advert is a fixed byte string, built once: every v2 ls-refs/fetch request
// begins with it, and rebuilding it per request would be pure allocation.
var v2Advertisement = func() []byte {
	lines := []string{"version 2", agentID, "ls-refs=unborn", "fetch=shallow wait-for-done", "object-format=sha1"}
	var b []byte
	for _, l := range lines {
		b = append(b, pktLine(l+"\n")...)
	}
	return append(b, pktFlush...)
}()

var preambles = map[string][]byte{
	serviceUpload:  append(pktLine("# service="+serviceUpload+"\n"), pktFlush...),
	serviceReceive: append(pktLine("# service="+serviceReceive+"\n"), pktFlush...),
}

// Advertisement renders the info/refs body for a service and protocol version.
func Advertisement(r *Repository, service string, v2 bool) []byte {
	if service == serviceUpload && v2 {
		return v2Advertisement
	}
	key := "advert:" + service
	if body, ok := r.cacheGet(key); ok {
		return body
	}
	caps := uploadCaps
	if service == serviceReceive {
		caps = receiveCaps
	} else {
		caps = append(append([]string(nil), caps...), "symref=HEAD:"+r.HeadRef)
	}
	refs := r.Refs()
	// HEAD first, then tags peeled, exactly as Git advertises.
	type line struct{ oid, ref string }
	ordered := make([]line, 0, len(refs)+1)
	if tip := r.Tip(); tip != "" && service == serviceUpload {
		ordered = append(ordered, line{tip, "HEAD"})
	}
	for _, ref := range refs {
		ordered = append(ordered, line{ref.OID, ref.Name})
	}
	if len(ordered) == 0 {
		ordered = append(ordered, line{ZeroOID, "capabilities^{}"})
	}
	var b []byte
	b = append(b, preambles[service]...)
	for i, l := range ordered {
		s := l.oid + " " + l.ref
		if i == 0 {
			s += "\x00" + strings.Join(caps, " ")
		}
		b = append(b, pktLine(s+"\n")...)
	}
	if service == serviceUpload {
		for _, ref := range refs {
			if !strings.HasPrefix(ref.Name, "refs/tags/") {
				continue
			}
			if peeled := r.peel(ref.OID); peeled != ref.OID {
				b = append(b, pktLine(peeled+" "+ref.Name+"^{}\n")...)
			}
		}
	}
	b = append(b, pktFlush...)
	r.cachePut(key, b)
	return b
}

// update is one receive-pack command line.
type update struct {
	old  string
	next string
	ref  string
}

// parseUpdates splits a receive-pack request body into commands and capabilities.
func parseUpdates(body []byte) ([]update, map[string]bool, []byte, error) {
	var cmds []update
	caps := map[string]bool{}
	off := 0
	for off+4 <= len(body) {
		header := string(body[off : off+4])
		n, err := strconv.ParseUint(header, 16, 32)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: bad pkt length", errPackMalformed)
		}
		if n == 0 {
			return cmds, caps, body[off+4:], nil
		}
		if n < 4 || off+int(n) > len(body) {
			return nil, nil, nil, fmt.Errorf("%w: pkt overruns body", errPackMalformed)
		}
		line := strings.TrimSuffix(string(body[off+4:off+int(n)]), "\n")
		if sep := strings.IndexByte(line, 0); sep >= 0 {
			if len(cmds) != 0 {
				return nil, nil, nil, fmt.Errorf("%w: unexpected capabilities", errPackMalformed)
			}
			for _, c := range strings.Split(line[sep+1:], " ") {
				caps[c] = true
			}
			line = line[:sep]
		}
		fields := strings.Split(line, " ")
		if len(fields) != 3 {
			return nil, nil, nil, fmt.Errorf("%w: malformed command", errPackMalformed)
		}
		old := strings.ToLower(fields[0])
		next := strings.ToLower(fields[1])
		if !isObjectID(old) || !isObjectID(next) {
			return nil, nil, nil, fmt.Errorf("%w: malformed object id", errPackMalformed)
		}
		cmds = append(cmds, update{old: old, next: next, ref: fields[2]})
		off += int(n)
	}
	return nil, nil, nil, fmt.Errorf("%w: no command terminator", errPackMalformed)
}

// ReceivePack applies a push and renders the per-ref report.
func ReceivePack(r *Repository, body []byte) []byte {
	cmds, caps, pack, err := parseUpdates(body)
	if err != nil {
		out := pktLine("unpack invalid pack\n")
		return append(out, pktFlush...)
	}
	if len(cmds) == 0 {
		return nil
	}

	incoming := map[string]*Object{}
	unpack := "ok"
	if len(pack) > 0 {
		objs, err := readPack(pack, func(oid string) (*Object, bool) { return r.Object(oid) })
		if err != nil {
			unpack = "invalid pack"
		} else {
			incoming = objs
		}
	} else {
		for _, c := range cmds {
			if c.next != ZeroOID {
				unpack = "missing pack"
				break
			}
		}
	}

	var report []byte
	report = append(report, pktLine("unpack "+unpack+"\n")...)
	seen := map[string]bool{}
	for _, c := range cmds {
		var problem string
		switch {
		case unpack != "ok":
			problem = "unpacker error"
		case !RefNameValid(c.ref) || seen[c.ref]:
			problem = "invalid ref"
		default:
			cur, _ := r.Ref(c.ref)
			if cur == "" {
				cur = ZeroOID
			}
			if c.old != cur {
				problem = "non-fast-forward"
				break
			}
			if c.next == ZeroOID {
				r.SetRef(c.ref, "")
				break
			}
			target, ok := r.Object(c.next)
			if !ok {
				target, ok = incoming[c.next]
			}
			if !ok {
				problem = "missing or invalid object"
				break
			}
			if strings.HasPrefix(c.ref, "refs/heads/") && target.Type != TypeCommit {
				problem = "missing or invalid object"
				break
			}
			if c.old == c.next {
				break // a no-op update is always allowed
			}
			added := closureOf(c.next, r, incoming)
			// The pushed objects are not in the store yet, so the ancestor
			// walk has to see them through the incoming map.
			if c.old != ZeroOID && !strings.HasPrefix(c.ref, "refs/tags/") &&
				!r.ancestorIncoming(c.old, c.next, incoming) {
				problem = "non-fast-forward"
				break
			}
			// Only now is the push known-good: publish the new objects.
			if len(added) > 0 {
				r.owner.mu.Lock()
				for _, oid := range added {
					if _, exists := r.objects[oid]; !exists {
						r.objects[oid] = incoming[oid]
					}
				}
				r.owner.mu.Unlock()
			}
			r.SetRef(c.ref, c.next)
		}
		seen[c.ref] = true
		if problem != "" {
			report = append(report, pktLine("ng "+EscapeRefName(c.ref)+" "+problem+"\n")...)
		} else {
			report = append(report, pktLine("ok "+c.ref+"\n")...)
		}
	}
	report = append(report, pktFlush...)

	if !caps["report-status"] && !caps["report-status-v2"] {
		if caps["side-band-64k"] {
			return pktFlush
		}
		return nil
	}
	if caps["side-band-64k"] {
		return append(sideband(report, 1), pktFlush...)
	}
	return report
}

// closureOf walks the pushed objects, collecting those the store does not
// already hold. An object already present was validated when its ref was set,
// so a small push onto deep history stays cheap.
func closureOf(root string, r *Repository, incoming map[string]*Object) []string {
	var added []string
	seen := map[string]bool{}
	stack := []string{root}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || seen[oid] {
			continue
		}
		seen[oid] = true
		if r.HasObject(oid) {
			continue
		}
		o, ok := incoming[oid]
		if !ok {
			continue
		}
		added = append(added, oid)
		switch o.Type {
		case TypeCommit:
			c := parseCommit(oid, o.Data)
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case TypeTree:
			for _, e := range parseTree(o.Data) {
				if e.Mode != "160000" {
					stack = append(stack, e.OID)
				}
			}
		case TypeTag:
			stack = append(stack, parseTag(o.Data).Object)
		}
	}
	return added
}

// ── upload-pack ─────────────────────────────────────────────────────────────

// lsRefs answers a protocol v2 ls-refs command.
func lsRefs(r *Repository, args []string) []byte {
	var prefixes []string
	peel, symrefs, unborn := false, false, false
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[len("ref-prefix "):])
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		}
	}
	key := "lsrefs|" + strings.Join(prefixes, ",") + "|" +
		strconv.FormatBool(peel) + strconv.FormatBool(symrefs) + strconv.FormatBool(unborn)
	if body, ok := r.cacheGet(key); ok {
		return body
	}
	include := func(ref string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(ref, p) {
				return true
			}
		}
		return false
	}
	symrefSuffix := ""
	if symrefs {
		symrefSuffix = " symref-target:" + r.HeadRef
	}
	var b []byte
	if include("HEAD") {
		if tip := r.Tip(); tip != "" {
			b = append(b, pktLine(tip+" HEAD"+symrefSuffix+"\n")...)
		} else if unborn {
			b = append(b, pktLine("unborn HEAD"+symrefSuffix+"\n")...)
		}
	}
	for _, ref := range r.Refs() {
		if !include(ref.Name) {
			continue
		}
		line := ref.OID + " " + ref.Name
		if peel && strings.HasPrefix(ref.Name, "refs/tags/") {
			if target := r.peel(ref.OID); target != ref.OID {
				line += " peeled:" + target
			}
		}
		b = append(b, pktLine(line+"\n")...)
	}
	b = append(b, pktFlush...)
	if len(key) < 1024 {
		r.cachePut(key, b)
	}
	return b
}

// fetchRequest is a parsed upload-pack fetch command.
type fetchRequest struct {
	wants    []string
	haves    []string
	shallow  map[string]bool
	depth    int
	since    int64
	omit     []string
	relay    bool
	done     bool
	ofsDelt  bool
	incTag   bool
	sideband bool
}

func parseFetch(lines []string) *fetchRequest {
	f := &fetchRequest{shallow: map[string]bool{}}
	for _, line := range lines {
		fields := strings.Split(line, " ")
		switch fields[0] {
		case "want":
			if len(fields) < 2 || !isObjectID(strings.ToLower(fields[1])) {
				continue
			}
			f.wants = append(f.wants, strings.ToLower(fields[1]))
			for _, feat := range fields[2:] {
				switch feat {
				case "ofs-delta":
					f.ofsDelt = true
				case "include-tag":
					f.incTag = true
				case "side-band-64k":
					f.sideband = true
				}
			}
		case "have":
			if len(fields) >= 2 && isObjectID(strings.ToLower(fields[1])) {
				f.haves = append(f.haves, strings.ToLower(fields[1]))
			}
		case "shallow":
			if len(fields) >= 2 {
				f.shallow[strings.ToLower(fields[1])] = true
			}
		case "deepen":
			if len(fields) >= 2 {
				if v, err := strconv.Atoi(fields[1]); err == nil && v > 0 {
					f.depth = v
				}
			}
		case "deepen-since":
			if len(fields) >= 2 {
				if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					f.since = v
				}
			}
		case "deepen-not":
			if len(fields) >= 2 {
				f.omit = append(f.omit, fields[1])
			}
		case "deepen-relative":
			f.relay = true
		case "done":
			f.done = true
		case "side-band-64k":
			f.sideband = true
		}
	}
	f.wants = dedupe(f.wants)
	f.haves = dedupe(f.haves)
	return f
}

// selection is the set of objects a fetch will send.
type selection struct {
	objects map[string]bool
	shallow map[string]bool
}

// selectObjects walks from the wanted tips, honouring depth and shallow
// boundaries, and reports where the history was cut.
func selectObjects(r *Repository, roots []string, f *fetchRequest, exclude func(string) bool) *selection {
	objects := map[string]bool{}
	shallow := map[string]bool{}
	boundaryParents := map[string]bool{}
	depthSeen := map[string]int{}
	expand := f.depth > 0 || f.since > 0 || len(f.omit) > 0

	forbidden := map[string]bool{}
	if expand {
		for _, name := range f.omit {
			oid, ok := r.Ref("refs/heads/" + name)
			if !ok {
				oid, ok = r.Ref(name)
			}
			if ok {
				for _, h := range r.history(oid, 1<<30) {
					forbidden[h] = true
				}
			}
		}
	}

	type item struct {
		oid      string
		depth    int
		relative int
	}
	stack := make([]item, 0, len(roots))
	for _, oid := range roots {
		stack = append(stack, item{oid: oid, depth: 1})
	}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		o, ok := r.Object(it.oid)
		if !ok || (exclude != nil && exclude(it.oid)) {
			continue
		}
		if o.Type == TypeCommit {
			distance := it.depth
			if expand && f.relay {
				distance = it.relative
			}
			if prev, seen := depthSeen[it.oid]; seen && prev <= distance {
				continue
			}
			depthSeen[it.oid] = distance
			objects[it.oid] = true
			c := parseCommit(it.oid, o.Data)
			stack = append(stack, item{oid: c.Tree, depth: it.depth, relative: it.relative})
			nextRel := it.relative
			if f.shallow[it.oid] && f.relay && it.relative == 0 {
				nextRel = 1
			}
			atBoundary := (!expand && f.shallow[it.oid]) ||
				(expand && f.depth > 0 && ((f.relay && nextRel > f.depth) || (!f.relay && it.depth >= f.depth)))
			missing := false
			for _, p := range c.Parents {
				if atBoundary || (expand && (forbidden[p] || r.olderThan(p, f.since))) {
					missing = true
					continue
				}
				stack = append(stack, item{oid: p, depth: it.depth + 1, relative: nextRel})
			}
			if missing {
				shallow[it.oid] = true
			} else {
				boundaryParents[it.oid] = true
			}
			continue
		}
		if objects[it.oid] {
			continue
		}
		objects[it.oid] = true
		switch o.Type {
		case TypeTree:
			for _, e := range parseTree(o.Data) {
				if e.Mode != "160000" {
					stack = append(stack, item{oid: e.OID, depth: it.depth, relative: it.relative})
				}
			}
		case TypeTag:
			stack = append(stack, item{oid: parseTag(o.Data).Object, depth: it.depth, relative: it.relative})
		}
	}
	for oid := range boundaryParents {
		delete(shallow, oid)
	}
	return &selection{objects: objects, shallow: shallow}
}

// olderThan reports whether a commit predates the deepen-since cutoff.
func (r *Repository) olderThan(oid string, since int64) bool {
	if since <= 0 {
		return false
	}
	o, ok := r.Object(oid)
	if !ok || o.Type != TypeCommit {
		return false
	}
	_, _, epoch := parseAuthorLine(parseCommit(oid, o.Data).Committer)
	return epoch < since
}

// FetchPack answers an upload-pack fetch command.
func FetchPack(r *Repository, f *fetchRequest, v2 bool) []byte {
	for _, w := range f.wants {
		if !r.HasObject(w) {
			return append(pktLine("ERR not our ref "+w+"\n"), pktFlush...)
		}
	}
	var common []string
	for _, h := range f.haves {
		if o, ok := r.Object(h); ok && o.Type == TypeCommit {
			common = append(common, h)
		}
	}

	if v2 && !f.done {
		// Negotiation round: acknowledgments with no packfile.
		out := append([]byte(nil), pktLine("acknowledgments\n")...)
		if len(common) > 0 {
			for _, oid := range common {
				out = append(out, pktLine("ACK "+oid+"\n")...)
			}
		} else {
			out = append(out, pktLine("NAK\n")...)
		}
		return append(out, pktFlush...)
	}

	band := v2 || f.sideband
	hasDepth := f.depth > 0 || f.since > 0 || len(f.omit) > 0

	var cacheKey string
	if f.done && len(f.haves) == 0 && len(f.shallow) == 0 && f.since == 0 && len(f.omit) == 0 {
		cacheKey = "fetch|" + strconv.FormatBool(v2) + strconv.FormatBool(band) + strconv.Itoa(f.depth) +
			strconv.FormatBool(f.ofsDelt) + strconv.FormatBool(f.incTag) + "|" + strings.Join(f.wants, ",")
		if body, ok := r.cacheGet(cacheKey); ok {
			return body
		}
	}

	chosen := selectObjects(r, f.wants, f, nil)

	var out []byte
	var extra []byte
	if hasDepth {
		for oid := range chosen.shallow {
			if !f.shallow[oid] {
				extra = append(extra, pktLine("shallow "+oid+"\n")...)
			}
		}
		for oid := range f.shallow {
			if chosen.objects[oid] && !chosen.shallow[oid] {
				extra = append(extra, pktLine("unshallow "+oid+"\n")...)
			}
		}
	}
	if v2 && len(extra) > 0 {
		out = append(out, pktLine("shallow-info\n")...)
		out = append(out, extra...)
		out = append(out, pktDelim...)
	}
	if !v2 && hasDepth {
		out = append(out, extra...)
		out = append(out, pktFlush...)
		if !f.done && len(f.haves) == 0 {
			return out
		}
	}
	if !f.done {
		return append(out, pktLine("NAK\n")...)
	}

	// A client that is deepening is asking for history it does not have yet, so
	// the objects it already holds must not suppress the very commits its
	// shallow boundary is being moved past. The exclusion therefore only
	// applies when the request is not deepening along the client's own edge.
	held := map[string]bool{}
	if len(common) > 0 && !hasDepth {
		held = selectObjects(r, common, f, nil).objects
	} else if len(common) > 0 {
		for _, oid := range common {
			if !f.shallow[oid] {
				held[oid] = true
			}
		}
	}
	if f.incTag {
		for _, ref := range r.Refs() {
			if !strings.HasPrefix(ref.Name, "refs/tags/") {
				continue
			}
			peeled := r.peel(ref.OID)
			if !chosen.objects[peeled] || held[peeled] {
				continue
			}
			cur := ref.OID
			for cur != "" {
				o, ok := r.Object(cur)
				if !ok || o.Type != TypeTag || chosen.objects[cur] {
					break
				}
				chosen.objects[cur] = true
				cur = parseTag(o.Data).Object
			}
		}
	}

	objs := make([]*Object, 0, len(chosen.objects))
	for oid := range chosen.objects {
		if held[oid] {
			continue
		}
		if o, ok := r.Object(oid); ok {
			objs = append(objs, o)
		}
	}
	var pack []byte
	if len(objs) > 0 {
		pack = assemblePack(objs, f.ofsDelt)
	} else {
		pack = assemblePack(nil, false)
	}

	if v2 {
		out = append(out, pktLine("packfile\n")...)
	} else if len(common) > 0 {
		out = append(out, pktLine("ACK "+common[len(common)-1]+"\n")...)
	} else {
		out = append(out, pktLine("NAK\n")...)
	}
	if band {
		out = append(out, sideband(pack, 1)...)
		out = append(out, pktFlush...)
	} else {
		out = append(out, pack...)
	}
	if cacheKey != "" {
		r.cachePut(cacheKey, out)
	}
	return out
}

// UploadPack dispatches a decoded upload-pack request.
func UploadPack(r *Repository, body []byte, v2 bool) ([]byte, error) {
	packets := decodePktLines(body)
	lines := make([]string, 0, len(packets))
	for _, p := range packets {
		if p.Kind == 'd' {
			lines = append(lines, strings.TrimSuffix(string(p.Payload), "\n"))
		}
	}
	if len(lines) == 0 {
		return pktFlush, nil
	}
	if v2 {
		sep := -1
		for i, p := range packets {
			if p.Kind == 'm' {
				sep = i
				break
			}
		}
		if sep < 0 {
			return nil, fmt.Errorf("missing protocol v2 delimiter")
		}
		args := make([]string, 0, len(packets)-sep)
		for _, p := range packets[sep+1:] {
			if p.Kind == 'd' {
				args = append(args, strings.TrimSuffix(string(p.Payload), "\n"))
			}
		}
		command := lines[0]
		switch command {
		case "command=ls-refs":
			return lsRefs(r, args), nil
		case "command=fetch":
			return FetchPack(r, parseFetch(args), true), nil
		}
		return nil, fmt.Errorf("unsupported upload-pack command %q", command)
	}
	return FetchPack(r, parseFetch(lines), false), nil
}

// dedupe removes duplicates while keeping first-seen order.
func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// hashOf is a small helper kept for symmetry with the TypeScript original.
func hashOf(kind string, data []byte) string { return hashObject(kind, data) }

// packetLines joins raw lines into a pkt-line stream, used by tests.
func packetLines(lines ...string) []byte {
	var b []byte
	for _, l := range lines {
		b = append(b, pktLine(l)...)
	}
	return append(b, pktFlush...)
}

// ensure bytes is used even if a future edit drops the only other reference.
var _ = bytes.Equal
