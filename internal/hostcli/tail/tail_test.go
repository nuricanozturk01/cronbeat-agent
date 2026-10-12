package tail

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func write(t *testing.T, r *Ring, s string) {
	t.Helper()
	n, err := r.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write(%q) = %d, %v", s, n, err)
	}
}

func TestKeepsEverythingBelowCapacity(t *testing.T) {
	r := New(16)
	write(t, r, "hello ")
	write(t, r, "world")
	if got := r.Snapshot(); got != "hello world" {
		t.Fatalf("Snapshot = %q", got)
	}
	if r.Truncated() {
		t.Fatal("Truncated = true below capacity")
	}
}

func TestExactCapacityIsNotTruncated(t *testing.T) {
	r := New(5)
	write(t, r, "abcde")
	if got := r.Snapshot(); got != "abcde" || r.Truncated() {
		t.Fatalf("Snapshot = %q truncated=%v", got, r.Truncated())
	}
}

func TestOverflowKeepsTheLastBytes(t *testing.T) {
	r := New(5)
	write(t, r, "abc")
	write(t, r, "defg")
	if got := r.Snapshot(); got != "cdefg" {
		t.Fatalf("Snapshot = %q", got)
	}
	if !r.Truncated() {
		t.Fatal("Truncated = false after overflow")
	}
}

func TestSingleWriteLargerThanCapacity(t *testing.T) {
	r := New(4)
	write(t, r, "0123456789")
	if got := r.Snapshot(); got != "6789" {
		t.Fatalf("Snapshot = %q", got)
	}
}

func TestManySmallWritesWrapAround(t *testing.T) {
	r := New(7)
	var all strings.Builder
	for i := range 50 {
		s := fmt.Sprintf("%d,", i)
		all.WriteString(s)
		write(t, r, s)
	}
	want := all.String()[all.Len()-7:]
	if got := r.Snapshot(); got != want {
		t.Fatalf("Snapshot = %q, want %q", got, want)
	}
}

func TestPartialUTF8AtTheCutIsDropped(t *testing.T) {
	r := New(5)
	write(t, r, "a€bc")
	write(t, r, "d")
	got := r.Snapshot()
	if got != "bcd" {
		t.Fatalf("Snapshot = %q, want the euro sign cut away entirely", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got)
	}
}

func TestWholeCharacterAtTheCutIsKept(t *testing.T) {
	r := New(6)
	write(t, r, "xx€abc")
	if got := r.Snapshot(); got != "€abc" {
		t.Fatalf("Snapshot = %q", got)
	}
}

func TestInvalidUTF8FromTheProgramIsReplaced(t *testing.T) {
	r := New(32)
	if _, err := r.Write([]byte{'o', 'k', 0xff, 0xfe, 'x'}); err != nil {
		t.Fatal(err)
	}
	got := r.Snapshot()
	if !utf8.ValidString(got) || !strings.HasPrefix(got, "ok") || !strings.HasSuffix(got, "x") {
		t.Fatalf("Snapshot = %q", got)
	}
}

func TestNULBytesAreStripped(t *testing.T) {
	r := New(16)
	write(t, r, "a\x00b\x00\x00c")
	if got := r.Snapshot(); got != "abc" {
		t.Fatalf("Snapshot = %q", got)
	}
}

func TestEmptyRing(t *testing.T) {
	if got := New(8).Snapshot(); got != "" {
		t.Fatalf("Snapshot = %q", got)
	}
}

func TestNonPositiveCapacityKeepsNothingAndDoesNotPanic(t *testing.T) {
	r := New(0)
	write(t, r, "abc")
	if got := r.Snapshot(); got != "" {
		t.Fatalf("Snapshot = %q", got)
	}
}

func TestConcurrentWritersDoNotRace(t *testing.T) {
	r := New(64)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_, _ = r.Write([]byte(strings.Repeat("abcdefgh"[w:w+1], 7)))
			}
		}()
	}
	wg.Wait()
	got := r.Snapshot()
	if len(got) != 64 || !utf8.ValidString(got) {
		t.Fatalf("Snapshot len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestLast(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than max", "abc", 10, "abc"},
		{"exact", "abc", 3, "abc"},
		{"cut ascii", "abcdef", 3, "def"},
		{"cut inside a character", "a€b", 3, "b"},
		{"character fits exactly", "a€b", 4, "€b"},
		{"zero", "abc", 0, ""},
		{"negative", "abc", -1, ""},
		{"empty", "", 5, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Last(tt.in, tt.max); got != tt.want {
				t.Fatalf("Last(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}
