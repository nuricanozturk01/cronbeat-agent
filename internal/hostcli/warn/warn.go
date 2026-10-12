//go:build unix

// Package warn rate limits warnings so a broken CronBeat connection does not fill the cron mail.
// The state is a best effort file: any problem with it means the warning is shown.
package warn

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/atomicfile"
)

const (
	// Interval is how long a warning kind stays quiet after it was shown.
	Interval = 6 * time.Hour

	pruneAfter   = 7 * 24 * time.Hour
	maxStateSize = 64 * 1024
)

// Limiter remembers when each kind of warning was last shown.
type Limiter struct {
	path string
	now  func() time.Time
}

// NewLimiter returns a limiter backed by the state file at path; an empty path disables rate limiting.
func NewLimiter(path string, now func() time.Time) *Limiter {
	return &Limiter{path: path, now: now}
}

// StatePath returns the default state file location, or "" when there is no home directory.
func StatePath(getenv func(string) string, home string) string {
	if dir := getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "cronbeat", "warned.json")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "cronbeat", "warned.json")
}

// Allow reports whether a warning of this kind should be shown now, and records it.
func (l *Limiter) Allow(kind string) bool {
	if l.path == "" {
		return true
	}
	now := l.now()
	state := l.load()
	if shown, ok := state[kind]; ok {
		age := now.Sub(time.Unix(shown, 0))
		if age >= 0 && age < Interval {
			return false
		}
	}
	state[kind] = now.Unix()
	for k, shown := range state {
		if now.Sub(time.Unix(shown, 0)) > pruneAfter {
			delete(state, k)
		}
	}
	l.save(state)
	return true
}

func (l *Limiter) load() map[string]int64 {
	state := map[string]int64{}
	f, err := os.Open(l.path)
	if err != nil {
		return state
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxStateSize))
	if err != nil || json.Unmarshal(data, &state) != nil || state == nil {
		return map[string]int64{}
	}
	return state
}

func (l *Limiter) save(state map[string]int64) {
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = atomicfile.Write(l.path, data, 0o600, 0o700)
}
