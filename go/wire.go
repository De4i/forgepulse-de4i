package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strconv"
)

// base64Std is the encoding GitHub uses for node ids.
func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// base64Decode reverses base64Std.
func base64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	return string(b), err
}

// jsonBytes renders a value the way the REST surface sends it: no HTML
// escaping, so the bytes are stable and can be cached.
func jsonBytes(v any) []byte {
	var buf []byte
	enc := newEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	// json.Encoder appends a newline; the suite compares parsed JSON, but
	// dropping it keeps cached bodies byte-stable with hand-built ones.
	if n := len(buf); n > 0 && buf[n-1] == '\n' {
		buf = buf[:n-1]
	}
	return buf
}

// ── pkt-line framing ────────────────────────────────────────────────────────

const pktMaxPayload = 65516

var (
	pktFlush = []byte("0000")
	pktDelim = []byte("0001")
)

// pktLine encodes one data packet: 4 hex digits of total length, then payload.
func pktLine(s string) []byte {
	n := len(s) + 4
	b := make([]byte, 0, n)
	b = appendHex4(b, n)
	return append(b, s...)
}

// pktLineBytes encodes one data packet with a byte payload.
func pktLineBytes(p []byte) []byte {
	n := len(p) + 4
	b := make([]byte, 0, n)
	b = appendHex4(b, n)
	return append(b, p...)
}

// appendHex4 writes a 4-digit lowercase hex length.
func appendHex4(dst []byte, n int) []byte {
	const hexdigits = "0123456789abcdef"
	return append(dst,
		hexdigits[(n>>12)&0xf], hexdigits[(n>>8)&0xf],
		hexdigits[(n>>4)&0xf], hexdigits[n&0xf])
}

// Packet is one decoded pkt-line.
type Packet struct {
	Kind    byte // 'd' data, 'f' flush, 'm' delim, 'r' response-end
	Payload []byte
}

// decodePktLines splits a pkt-line stream.
func decodePktLines(b []byte) []Packet {
	var out []Packet
	for off := 0; off+4 <= len(b); {
		n, err := strconv.ParseUint(string(b[off:off+4]), 16, 32)
		if err != nil {
			break
		}
		switch n {
		case 0:
			out = append(out, Packet{Kind: 'f'})
			off += 4
			continue
		case 1:
			out = append(out, Packet{Kind: 'm'})
			off += 4
			continue
		case 2:
			out = append(out, Packet{Kind: 'r'})
			off += 4
			continue
		}
		if n < 4 || off+int(n) > len(b) {
			break
		}
		out = append(out, Packet{Kind: 'd', Payload: b[off+4 : off+int(n)]})
		off += int(n)
	}
	return out
}

// ── pack object headers ─────────────────────────────────────────────────────

// packTypeCode maps an object type to its packfile type code.
func packTypeCode(kind string) byte {
	switch kind {
	case TypeCommit:
		return 1
	case TypeTree:
		return 2
	case TypeBlob:
		return 3
	case TypeTag:
		return 4
	}
	return 0
}

// codeType maps a packfile type code back to an object type.
var codeType = map[byte]string{1: TypeCommit, 2: TypeTree, 3: TypeBlob, 4: TypeTag}

// objectHeader builds a pack entry header: type and inflated size.
func objectHeader(code byte, size int) []byte {
	b := []byte{(code << 4) | byte(size&0x0f)}
	size >>= 4
	for size > 0 {
		b[len(b)-1] |= 0x80
		b = append(b, byte(size&0x7f))
		size >>= 7
	}
	return b
}

// distanceHeader encodes an OFS_DELTA backward distance in Git's varint form.
func distanceHeader(distance int) []byte {
	out := []byte{byte(distance & 0x7f)}
	for distance >>= 7; distance > 0; distance >>= 7 {
		distance--
		out = append(out, 0x80|byte(distance&0x7f))
	}
	// Git writes this varint little-endian within the stream; the encoder
	// above emits the most significant group last, so reverse in place.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// readDistanceHeader decodes an OFS_DELTA distance and returns the bytes read.
func readDistanceHeader(b []byte) (int, int) {
	if len(b) == 0 {
		return 0, 0
	}
	v := int(b[0] & 0x7f)
	n := 1
	for b[n-1]&0x80 != 0 && n < len(b) {
		v = ((v + 1) << 7) | int(b[n]&0x7f)
		n++
	}
	return v, n
}

// readSizeVarint reads the size field of a pack entry header.
func readSizeVarint(b []byte) (int, int) {
	if len(b) == 0 {
		return 0, 0
	}
	size := int(b[0] & 0x0f)
	shift := uint(4)
	n := 1
	for b[n-1]&0x80 != 0 && n < len(b) {
		size |= int(b[n]&0x7f) << shift
		shift += 7
		n++
	}
	return size, n
}

// leb128 appends an unsigned LEB128 value, used inside delta instructions.
func leb128(dst []byte, v int) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// readLEB128 reads an unsigned LEB128 value and reports the bytes consumed.
func readLEB128(b []byte) (int, int) {
	v, shift, n := 0, uint(0), 0
	for n < len(b) {
		c := b[n]
		n++
		v |= int(c&0x7f) << shift
		if c&0x80 == 0 {
			break
		}
		shift += 7
	}
	return v, n
}

// be32 reads a big-endian uint32.
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

// putBE32 writes a big-endian uint32.
func putBE32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

// newEncoder is json.NewEncoder minus the buffer allocation the caller owns.
func newEncoder(buf *[]byte) *json.Encoder {
	return json.NewEncoder(writerFunc(func(p []byte) (int, error) {
		*buf = append(*buf, p...)
		return len(p), nil
	}))
}

// writerFunc adapts a closure to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
