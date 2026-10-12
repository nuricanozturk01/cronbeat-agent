// Package tail keeps the last bytes of a process output in a bounded ring.
package tail

import (
	"bytes"
	"strings"
	"sync"
	"unicode/utf8"
)

// Ring is a fixed capacity byte ring that is safe for concurrent writers.
type Ring struct {
	mu        sync.Mutex
	buf       []byte
	next      int
	full      bool
	truncated bool
}

// New returns a ring that keeps at most capacity bytes.
func New(capacity int) *Ring {
	return &Ring{buf: make([]byte, max(capacity, 0))}
}

// Write never fails and never blocks on the amount of data.
func (r *Ring) Write(p []byte) (int, error) {
	n := len(p)
	size := len(r.buf)
	r.mu.Lock()
	defer r.mu.Unlock()
	if size == 0 {
		r.truncated = r.truncated || n > 0
		return n, nil
	}
	if n >= size {
		r.truncated = r.truncated || n > size || r.full || r.next > 0
		copy(r.buf, p[n-size:])
		r.next, r.full = 0, true
		return n, nil
	}
	first := copy(r.buf[r.next:], p)
	if first < n {
		copy(r.buf, p[first:])
		r.full = true
		r.truncated = true
	}
	r.next = (r.next + n) % size
	if r.next == 0 {
		r.full = true
	}
	return n, nil
}

// Snapshot returns the kept bytes as valid UTF-8 without NUL characters.
func (r *Ring) Snapshot() string {
	r.mu.Lock()
	raw := make([]byte, 0, len(r.buf))
	if r.full {
		raw = append(raw, r.buf[r.next:]...)
	}
	raw = append(raw, r.buf[:r.next]...)
	truncated := r.truncated
	r.mu.Unlock()

	if truncated {
		raw = dropLeadingPartialCharacter(raw)
	}
	raw = bytes.ReplaceAll(raw, []byte{0}, nil)
	return strings.ToValidUTF8(string(raw), "�")
}

// Truncated reports whether older bytes were dropped.
func (r *Ring) Truncated() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.truncated
}

func dropLeadingPartialCharacter(b []byte) []byte {
	i := 0
	for i < len(b) && i < utf8.UTFMax && !utf8.RuneStart(b[i]) {
		i++
	}
	return b[i:]
}

// Last returns at most limit trailing bytes of s without splitting a UTF-8 character.
func Last(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := len(s) - limit
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}
