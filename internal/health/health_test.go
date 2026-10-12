package health

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ now atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *clock) advance(d time.Duration) { c.now.Add(int64(d)) }

func newHealth(maxSilence time.Duration) (*Health, *clock) {
	c := &clock{}
	c.now.Store(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).UnixNano())
	h := New(maxSilence)
	h.now = c.Now
	h.Beat()
	return h, c
}

func get(t *testing.T, h http.Handler, method, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

func TestAFreshAgentIsAliveButNotReadyUntilItsCachesSynced(t *testing.T) {
	h, _ := newHealth(10 * time.Minute)

	if got := get(t, h.Handler(), http.MethodGet, "/healthz"); got != http.StatusOK {
		t.Fatalf("healthz = %d", got)
	}
	if got := get(t, h.Handler(), http.MethodGet, "/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz before sync = %d", got)
	}

	h.SetReady()

	if got := get(t, h.Handler(), http.MethodGet, "/readyz"); got != http.StatusOK {
		t.Fatalf("readyz after sync = %d", got)
	}
}

func TestAnAgentWhoseHeartbeatLoopWentSilentIsNoLongerAlive(t *testing.T) {
	h, clk := newHealth(10 * time.Minute)

	clk.advance(9 * time.Minute)
	if got := get(t, h.Handler(), http.MethodGet, "/healthz"); got != http.StatusOK {
		t.Fatalf("still within the allowance, got %d", got)
	}

	clk.advance(2 * time.Minute)
	if got := get(t, h.Handler(), http.MethodGet, "/healthz"); got != http.StatusServiceUnavailable {
		t.Fatalf("a silent loop must fail the probe, got %d", got)
	}

	h.Beat()
	if got := get(t, h.Handler(), http.MethodGet, "/healthz"); got != http.StatusOK {
		t.Fatalf("a beat revives it, got %d", got)
	}
}

func TestOnlyTheProbePathsAnswerAndOnlyToReads(t *testing.T) {
	h, _ := newHealth(time.Minute)
	handler := h.Handler()

	for _, path := range []string{"/", "/metrics", "/healthz/extra", "/admin"} {
		if got := get(t, handler, http.MethodGet, path); got != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, got)
		}
	}
	if got := get(t, handler, http.MethodPost, "/healthz"); got != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", got)
	}
	if got := get(t, handler, http.MethodHead, "/healthz"); got != http.StatusOK {
		t.Errorf("HEAD = %d", got)
	}
}

func TestTheProbeAnswersCarryNoInformation(t *testing.T) {
	h, _ := newHealth(time.Minute)
	rec := httptest.NewRecorder()

	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if body, _ := io.ReadAll(rec.Body); string(body) != "ok\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestServeAnswersAndStopsWithTheContext(t *testing.T) {
	h, _ := newHealth(time.Minute)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, lis, h.Handler()) }()

	resp, err := http.Get("http://" + lis.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
