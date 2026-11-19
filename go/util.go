package main

import (
	"io"
	"strconv"
	"sync/atomic"
)

// requestCounter backs the X-GitHub-Request-Id header. It is a plain atomic
// counter: cheap, and unique per process, which is all the suite checks.
var requestCounter atomic.Uint64

// nextRequestID renders the per-request id.
func nextRequestID() string {
	return strconv.FormatUint(requestCounter.Add(1), 36)
}

// itoa is a small wrapper so callers do not pull strconv into every file.
func itoa(n int) string { return strconv.Itoa(n) }

// readAllLimit reads up to limit bytes, reporting an error if the body is
// larger, so an oversized request cannot exhaust the pod's memory.
func readAllLimit(r io.Reader, limit int64) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 32*1024)
	var total int64
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return nil, errBodyTooLarge
			}
			buf = append(buf, chunk[:n]...)
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// bytesTrim strips leading and trailing ASCII whitespace without allocating.
func bytesTrim(b []byte) []byte {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
