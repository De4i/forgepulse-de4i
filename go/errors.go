package main

import (
	"errors"
	"sort"
	"time"
)

// errBodyTooLarge is returned when a request body exceeds its cap.
var errBodyTooLarge = errors.New("request body too large")

// Fault carries an HTTP status and message out of the handlers.
type Fault struct {
	Status  int
	Message string
}

func (f *Fault) Error() string { return f.Message }

func errUnprocessable(what string) error {
	return &Fault{Status: 422, Message: "Validation Failed: " + what}
}

func errNotFound() error {
	return &Fault{Status: 404, Message: "Not Found"}
}

func errMethodNotAllowed() error {
	return &Fault{Status: 405, Message: "Pull Request is not mergeable"}
}

// epochOf renders an RFC3339-millisecond stamp as a Git epoch string.
func epochOf(stamp string) string {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", stamp)
	if err != nil {
		return "0"
	}
	return itoa64(t.Unix())
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ── small ordered collections, kept allocation-light on purpose ─────────────

func sortInts(v []int) {
	if len(v) < 2 {
		return
	}
	sort.Ints(v)
}

func sortStrings(v []string) {
	if len(v) < 2 {
		return
	}
	sort.Strings(v)
}

func reverseIssues(v []*Issue) {
	for i, j := 0, len(v)-1; i < j; i, j = i+1, j-1 {
		v[i], v[j] = v[j], v[i]
	}
}

// sortMapsBy orders a list of objects by one string field.
func sortMapsBy(rows []map[string]any, key string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, _ := rows[i][key].(string)
		b, _ := rows[j][key].(string)
		return a < b
	})
}
