//go:build unix

package warn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newLimiter(t *testing.T) (*Limiter, *clock, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache", "cronbeat", "warned.json")
	c := &clock{t: time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)}
	return NewLimiter(path, c.now), c, path
}

func TestFirstWarningIsShownThenSuppressedForSixHours(t *testing.T) {
	l, c, _ := newLimiter(t)
	if !l.Allow("network") {
		t.Fatal("first warning must be shown")
	}
	if l.Allow("network") {
		t.Fatal("repeat right away must be suppressed")
	}
	c.t = c.t.Add(Interval - time.Second)
	if l.Allow("network") {
		t.Fatal("still inside six hours")
	}
	c.t = c.t.Add(time.Second)
	if !l.Allow("network") {
		t.Fatal("six hours passed")
	}
	if l.Allow("network") {
		t.Fatal("the interval restarts after being shown")
	}
}

func TestKindsAreIndependent(t *testing.T) {
	l, _, _ := newLimiter(t)
	if !l.Allow("network") || !l.Allow("auth") || l.Allow("auth") || l.Allow("network") {
		t.Fatal("kinds must be tracked separately")
	}
}

func TestStateSurvivesANewLimiterInstance(t *testing.T) {
	l, c, path := newLimiter(t)
	l.Allow("auth")
	if NewLimiter(path, c.now).Allow("auth") {
		t.Fatal("state must be shared through the file between processes")
	}
}

func TestStateFileIsPrivate(t *testing.T) {
	l, _, path := newLimiter(t)
	l.Allow("auth")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stat = %v, %v", info, err)
	}
	dir, _ := os.Stat(filepath.Dir(path))
	if dir.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %04o", dir.Mode().Perm())
	}
}

func TestCorruptStateAllowsAndIsRepaired(t *testing.T) {
	l, c, path := newLimiter(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !l.Allow("auth") {
		t.Fatal("corrupt state must not hide warnings")
	}
	if NewLimiter(path, c.now).Allow("auth") {
		t.Fatal("state was not repaired")
	}
}

func TestTimestampInTheFutureIsTreatedAsExpired(t *testing.T) {
	l, c, path := newLimiter(t)
	l.Allow("auth")
	c.t = c.t.Add(-48 * time.Hour)
	if !NewLimiter(path, c.now).Allow("auth") {
		t.Fatal("a clock that went backwards must not silence warnings for days")
	}
}

func TestUnwritableStateStillAllowsAndDoesNotPanic(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := NewLimiter(filepath.Join(blocker, "sub", "warned.json"), time.Now)
	for range 2 {
		if !l.Allow("auth") {
			t.Fatal("without state every warning is shown")
		}
	}
}

func TestEmptyPathDisablesRateLimiting(t *testing.T) {
	l := NewLimiter("", time.Now)
	for range 2 {
		if !l.Allow("auth") {
			t.Fatal("no state, no limit")
		}
	}
}

func TestStatePath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := StatePath(env(nil), "/home/u"); got != "/home/u/.cache/cronbeat/warned.json" {
		t.Fatalf("default = %q", got)
	}
	if got := StatePath(env(map[string]string{"XDG_CACHE_HOME": "/xdg"}), "/home/u"); got != "/xdg/cronbeat/warned.json" {
		t.Fatalf("xdg = %q", got)
	}
	if got := StatePath(env(nil), ""); got != "" {
		t.Fatalf("no home = %q", got)
	}
}

func TestOldEntriesArePruned(t *testing.T) {
	l, c, path := newLimiter(t)
	l.Allow("old")
	c.t = c.t.Add(10 * 24 * time.Hour)
	l.Allow("fresh")
	data, _ := os.ReadFile(path) //nolint:gosec // test fixture needs this file mode or path
	if !strings.Contains(string(data), `"fresh"`) || strings.Contains(string(data), `"old"`) {
		t.Fatalf("state = %s", data)
	}
}
