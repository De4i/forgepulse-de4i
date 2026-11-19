package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"sync"
)

// errPackMalformed marks a packfile the server refuses to unpack.
var errPackMalformed = errors.New("malformed pack")

// zlib readers and writers are the most expensive per-call allocation in the
// hot path, so they are pooled and reset rather than rebuilt.
var (
	zlibReaderPool = sync.Pool{New: func() any { return nil }}
	zlibWriterPool = sync.Pool{New: func() any { return nil }}
)

// inflate decompresses one zlib stream starting at b[0], returning the bytes
// and how many input bytes the stream consumed.
func inflate(b []byte) ([]byte, int, error) {
	r, ok := zlibReaderPool.Get().(io.ReadCloser)
	if !ok || r == nil {
		nr, err := zlib.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, 0, err
		}
		r = nr
	} else {
		if zr, ok := r.(zlib.Resetter); ok {
			if err := zr.Reset(bytes.NewReader(b), nil); err != nil {
				return nil, 0, err
			}
		}
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		zlibReaderPool.Put(r)
		return nil, 0, err
	}
	// The reader consumed exactly the stream; anything left over is the rest
	// of the pack, which the caller walks entry by entry.
	consumed := len(b) - remaining(r, b)
	zlibReaderPool.Put(r)
	return out.Bytes(), consumed, nil
}

// remaining reports how many input bytes the zlib reader has not consumed.
// The standard library reader wraps a bytes.Reader we own, so its offset is
// the count already read.
func remaining(r io.ReadCloser, b []byte) int {
	type counter interface{ Len() int }
	if res, ok := r.(interface {
		Reset(io.Reader, io.Reader) error
	}); ok {
		_ = res
	}
	if c, ok := r.(counter); ok {
		return c.Len()
	}
	return 0
}

// deflate compresses with the fast level the workloads reward.
func deflate(b []byte) []byte {
	w, ok := zlibWriterPool.Get().(io.WriteCloser)
	if !ok || w == nil {
		var buf bytes.Buffer
		zw, err := zlib.NewWriterLevel(&buf, 1)
		if err != nil {
			return nil
		}
		if _, err := zw.Write(b); err != nil {
			return nil
		}
		_ = zw.Close()
		return buf.Bytes()
	}
	return nil
}

// PackEntry is one decoded pack entry.
type PackEntry struct {
	Type       string
	Data       []byte // resolved content (deltas already applied)
	Offset     int
	BaseOffset int    // for OFS_DELTA
	BaseOID    string // for REF_DELTA
}

// readPack decodes every object in a packfile. Deltas are resolved against
// objects already in the store or earlier entries in the same pack.
func readPack(pack []byte, resolveBase func(string) (*Object, bool)) (map[string]*Object, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("%w: no PACK signature", errPackMalformed)
	}
	if v := be32(pack[4:8]); v != 2 && v != 3 {
		return nil, fmt.Errorf("%w: unsupported pack version %d", errPackMalformed, v)
	}
	count := int(be32(pack[8:12]))
	trailer := len(pack) - 20
	if trailer < 12 {
		return nil, fmt.Errorf("%w: truncated", errPackMalformed)
	}
	sum := sha1.Sum(pack[:trailer])
	if !bytes.Equal(sum[:], pack[trailer:]) {
		return nil, fmt.Errorf("%w: checksum mismatch", errPackMalformed)
	}

	type raw struct {
		code       byte
		data       []byte
		offset     int
		baseOffset int
		baseOID    string
	}
	raws := make([]raw, 0, count)
	byOffset := make(map[int]int, count) // entry offset -> index in raws

	off := 12
	for i := 0; i < count; i++ {
		if off >= trailer {
			return nil, fmt.Errorf("%w: entry %d past trailer", errPackMalformed, i)
		}
		entryOffset := off
		c := pack[off]
		code := (c >> 4) & 7
		size, n := readSizeVarint(pack[off:trailer])
		off += n
		var baseOffset int
		var baseOID string
		switch code {
		case 6: // OFS_DELTA
			d, n := readDistanceHeader(pack[off:trailer])
			off += n
			if d <= 0 || d > entryOffset {
				return nil, fmt.Errorf("%w: ofs-delta base out of range", errPackMalformed)
			}
			baseOffset = entryOffset - d
			if _, ok := byOffset[baseOffset]; !ok {
				return nil, fmt.Errorf("%w: ofs-delta base not preceding", errPackMalformed)
			}
		case 7: // REF_DELTA
			if off+20 > trailer {
				return nil, fmt.Errorf("%w: ref-delta base truncated", errPackMalformed)
			}
			baseOID = fmt.Sprintf("%x", pack[off:off+20])
			off += 20
		default:
			if _, ok := codeType[code]; !ok {
				return nil, fmt.Errorf("%w: unknown type code %d", errPackMalformed, code)
			}
		}
		// Both compressed and delta entries store the inflated size.
		inflated, n, err := inflateStream(pack[off:trailer])
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d inflate: %v", errPackMalformed, i, err)
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("%w: entry %d declares %d bytes, inflated %d",
				errPackMalformed, i, size, len(inflated))
		}
		off += n
		raww := raw{code: code, data: inflated, offset: entryOffset, baseOffset: baseOffset, baseOID: baseOID}
		byOffset[entryOffset] = len(raws)
		raws = append(raws, raww)
	}
	if off != trailer {
		return nil, fmt.Errorf("%w: %d trailing bytes before checksum", errPackMalformed, trailer-off)
	}

	// Resolve: non-delta entries directly, deltas once their base is ready.
	out := make(map[string]*Object, len(raws))
	resolved := make([]*Object, len(raws))
	var resolve func(i int) (*Object, error)
	resolve = func(i int) (*Object, error) {
		if resolved[i] != nil {
			return resolved[i], nil
		}
		e := raws[i]
		var o *Object
		switch e.code {
		case 6:
			j, ok := byOffset[e.baseOffset]
			if !ok {
				return nil, fmt.Errorf("%w: missing ofs base", errPackMalformed)
			}
			base, err := resolve(j)
			if err != nil {
				return nil, err
			}
			data, err := applyDelta(base.Data, e.data)
			if err != nil {
				return nil, err
			}
			o = &Object{Type: base.Type, Data: data}
		case 7:
			if base, ok := resolveBase(e.baseOID); ok {
				data, err := applyDelta(base.Data, e.data)
				if err != nil {
					return nil, err
				}
				o = &Object{Type: base.Type, Data: data}
			} else {
				return nil, fmt.Errorf("%w: unresolved ref-delta base %s", errPackMalformed, e.baseOID)
			}
		default:
			o = &Object{Type: codeType[e.code], Data: e.data}
		}
		resolved[i] = o
		out[hashObject(o.Type, o.Data)] = o
		return o, nil
	}
	for i := range raws {
		if _, err := resolve(i); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// inflateStream decompresses one zlib stream and returns how many input bytes
// were consumed, so the caller can walk to the next pack entry.
func inflateStream(b []byte) ([]byte, int, error) {
	br := bytes.NewReader(b)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := io.Copy(&out, zr); err != nil {
		return nil, 0, err
	}
	consumed := len(b) - br.Len()
	return out.Bytes(), consumed, nil
}

// applyDelta rebuilds a target from a base using Git's delta instruction set.
func applyDelta(base, delta []byte) ([]byte, error) {
	baseSize, n := readLEB128(delta)
	if n == 0 {
		return nil, fmt.Errorf("%w: delta without base size", errPackMalformed)
	}
	off := n
	targetSize, n := readLEB128(delta[off:])
	if n == 0 {
		return nil, fmt.Errorf("%w: delta without target size", errPackMalformed)
	}
	off += n
	if baseSize != len(base) {
		return nil, fmt.Errorf("%w: delta base size %d, have %d", errPackMalformed, baseSize, len(base))
	}
	out := make([]byte, 0, targetSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 == 0 {
			// Insert op: the low 7 bits are a literal length.
			if op == 0 {
				return nil, fmt.Errorf("%w: zero insert length", errPackMalformed)
			}
			if off+int(op) > len(delta) {
				return nil, fmt.Errorf("%w: insert past delta end", errPackMalformed)
			}
			out = append(out, delta[off:off+int(op)]...)
			off += int(op)
			continue
		}
		// Copy op: variable offset and size fields, selected by the opcode bits.
		var cpOff, cpSize int
		for i := 0; i < 4; i++ {
			if op&(1<<uint(i)) != 0 {
				if off >= len(delta) {
					return nil, fmt.Errorf("%w: copy offset truncated", errPackMalformed)
				}
				cpOff |= int(delta[off]) << uint(8*i)
				off++
			}
		}
		for i := 0; i < 3; i++ {
			if op&(1<<uint(4+i)) != 0 {
				if off >= len(delta) {
					return nil, fmt.Errorf("%w: copy size truncated", errPackMalformed)
				}
				cpSize |= int(delta[off]) << uint(8*i)
				off++
			}
		}
		if cpSize == 0 {
			cpSize = 0x10000
		}
		if cpOff+cpSize > len(base) {
			return nil, fmt.Errorf("%w: copy past base end", errPackMalformed)
		}
		out = append(out, base[cpOff:cpOff+cpSize]...)
	}
	if len(out) != targetSize {
		return nil, fmt.Errorf("%w: delta produced %d bytes, wanted %d", errPackMalformed, len(out), targetSize)
	}
	return out, nil
}

// makeDelta builds a copy/insert delta that turns base into target, or nil
// when the two are too dissimilar for a delta to pay off.
func makeDelta(base, target []byte) []byte {
	if len(base) < 256 || len(target) < 256 {
		return nil
	}
	if len(base) > 1<<20 || len(target) > 1<<20 {
		return nil
	}
	// Common prefix and suffix are emitted as copies; anything between them
	// is inserted literally. For the small repeated edits these workloads
	// make, that is both correct and cheap.
	limit := min(len(base), len(target))
	prefix := 0
	for prefix < limit && base[prefix] == target[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < limit-prefix && base[len(base)-1-suffix] == target[len(target)-1-suffix] {
		suffix++
	}
	if prefix+suffix < int(float64(len(target))*0.6) {
		return nil
	}
	out := make([]byte, 0, len(target)/2)
	out = leb128(out, len(base))
	out = leb128(out, len(target))
	out = appendCopyOp(out, 0, prefix)
	for off := prefix; off < len(target)-suffix; {
		end := min(off+127, len(target)-suffix)
		out = append(out, byte(end-off))
		out = append(out, target[off:end]...)
		off = end
	}
	out = appendCopyOp(out, len(base)-suffix, suffix)
	return out
}

// appendCopyOp appends one copy instruction.
func appendCopyOp(dst []byte, offset, size int) []byte {
	for size > 0 {
		take := min(size, 0xffffff)
		op := byte(0x80)
		var vals []byte
		for i := 0; i < 4; i++ {
			b := byte(offset >> uint(8*i))
			if b != 0 {
				op |= 1 << uint(i)
				vals = append(vals, b)
			}
		}
		for i := 0; i < 3; i++ {
			b := byte(take >> uint(8*i))
			if b != 0 {
				op |= 1 << uint(4+i)
				vals = append(vals, b)
			}
		}
		dst = append(dst, op)
		dst = append(dst, vals...)
		offset += take
		size -= take
	}
	return dst
}

// assemblePack writes objects as a packfile, optionally with OFS_DELTA entries
// against the previous blob.
func assemblePack(objs []*Object, deltas bool) []byte {
	buf := make([]byte, 12, 12+len(objs)*64)
	copy(buf, "PACK")
	putBE32(buf[4:], 2)
	putBE32(buf[8:], uint32(len(objs)))

	prevOffset, prevData, prevDepth := 0, []byte(nil), 0
	for _, o := range objs {
		start := len(buf)
		raw := o.Data
		payload := deflateFast(raw)
		header := objectHeader(packTypeCode(o.Type), len(raw))
		depth := 0

		if deltas && o.Type == TypeBlob && prevData != nil && prevDepth < 8 {
			if d := makeDelta(prevData, raw); d != nil {
				packed := deflateFast(d)
				info := append(objectHeader(6, len(d)), distanceHeader(start-prevOffset)...)
				if len(packed)+len(info) < len(payload)+len(header) {
					payload, header, depth = packed, info, prevDepth+1
				}
			}
		}
		if o.Type == TypeBlob {
			prevOffset, prevData, prevDepth = start, raw, depth
		}
		buf = append(buf, header...)
		buf = append(buf, payload...)
	}
	sum := sha1.Sum(buf)
	return append(buf, sum[:]...)
}

// deflateFast compresses with zlib level 1, the setting that maximises the
// speed workloads without inflating the payload beyond recognition.
func deflateFast(b []byte) []byte {
	var out bytes.Buffer
	w, err := zlib.NewWriterLevel(&out, 1)
	if err != nil {
		return b
	}
	if _, err := w.Write(b); err != nil {
		return b
	}
	if err := w.Close(); err != nil {
		return b
	}
	return out.Bytes()
}

// sideband wraps a payload in side-band-64k packets on the given band.
func sideband(body []byte, band byte) []byte {
	out := make([]byte, 0, len(body)+len(body)/60000*5+16)
	for off := 0; off < len(body); off += pktMaxPayload - 1 {
		n := min(pktMaxPayload-1, len(body)-off)
		out = appendHex4(out, n+5)
		out = append(out, band)
		out = append(out, body[off:off+n]...)
	}
	return out
}
