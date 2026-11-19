package main

import (
	"testing"
)

// TestDeltaRoundTrip proves the delta encoder and decoder agree, which is the
// single most failure-prone part of the pack path.
func TestDeltaRoundTrip(t *testing.T) {
	base := make([]byte, 4096)
	for i := range base {
		base[i] = byte(i * 7)
	}
	target := append([]byte(nil), base...)
	copy(target[2000:2050], []byte("a much shorter replacement region of bytes"))

	d := makeDelta(base, target)
	if d == nil {
		t.Fatal("makeDelta refused a near-identical pair")
	}
	got, err := applyDelta(base, d)
	if err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	if string(got) != string(target) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), len(target))
	}
}

// TestDeltaPrefixSuffixOnly covers the append case, where the whole target is
// the base plus a tail.
func TestDeltaPrefixSuffixOnly(t *testing.T) {
	base := make([]byte, 2048)
	for i := range base {
		base[i] = byte(i)
	}
	target := append(append([]byte(nil), base...), []byte("extra tail bytes")...)

	d := makeDelta(base, target)
	if d == nil {
		t.Fatal("makeDelta refused a pure append")
	}
	got, err := applyDelta(base, d)
	if err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	if len(got) != len(target) {
		t.Fatalf("length mismatch: %d vs %d", len(got), len(target))
	}
}

// TestPackRoundTrip writes a pack and reads it back, checking every object id.
func TestPackRoundTrip(t *testing.T) {
	blobs := []*Object{
		{Type: TypeBlob, Data: []byte("hello world")},
		{Type: TypeBlob, Data: repeatBytes("payload ", 500)},
		{Type: TypeBlob, Data: append(repeatBytes("payload ", 500), []byte("tail")...)},
	}
	objs := append([]*Object{
		{Type: TypeTree, Data: buildTree("100644", "a.txt", hashObject(TypeBlob, blobs[0].Data))},
	}, blobs...)

	pack := assemblePack(objs, true)
	got, err := readPack(pack, func(string) (*Object, bool) { return nil, false })
	if err != nil {
		t.Fatalf("readPack: %v", err)
	}
	if len(got) != len(objs) {
		t.Fatalf("read %d objects, wrote %d", len(got), len(objs))
	}
	for _, o := range objs {
		want := hashObject(o.Type, o.Data)
		back, ok := got[want]
		if !ok {
			t.Fatalf("object %s missing after round trip", want)
		}
		if back.Type != o.Type || string(back.Data) != string(o.Data) {
			t.Fatalf("object %s differs after round trip", want)
		}
	}
}

// TestPackRejectsCorruption proves the checksum is enforced.
func TestPackRejectsCorruption(t *testing.T) {
	objs := []*Object{{Type: TypeBlob, Data: []byte("content")}}
	pack := assemblePack(objs, false)
	pack[len(pack)-1] ^= 0xff
	if _, err := readPack(pack, func(string) (*Object, bool) { return nil, false }); err == nil {
		t.Fatal("readPack accepted a pack with a bad trailer")
	}
}

// TestPktLineRoundTrip covers the framing used by every Git reply.
func TestPktLineRoundTrip(t *testing.T) {
	in := []byte("0008NAK\n0000")
	ps := decodePktLines(in)
	if len(ps) != 2 || ps[0].Kind != 'd' || string(ps[0].Payload) != "NAK\n" || ps[1].Kind != 'f' {
		t.Fatalf("decoded %#v", ps)
	}
}

func repeatBytes(s string, n int) []byte {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return out
}

// buildTree renders one tree object containing a single entry.
func buildTree(mode, name, oid string) []byte {
	b := []byte(mode)
	b = append(b, ' ')
	b = append(b, name...)
	b = append(b, 0)
	raw := make([]byte, 20)
	for i := 0; i < 20; i++ {
		raw[i] = hexNibble(oid[2*i])<<4 | hexNibble(oid[2*i+1])
	}
	return append(b, raw...)
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return 0
}
