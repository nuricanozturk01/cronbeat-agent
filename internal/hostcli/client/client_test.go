package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/config"
)

const canary = "cb_agent_CANARYSECRETVALUE123456"

type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeps = append(f.sleeps, d)
	f.t = f.t.Add(d)
	return nil
}

func (f *fakeClock) total() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum time.Duration
	for _, d := range f.sleeps {
		sum += d
	}
	return sum
}

type recorder struct {
	mu     sync.Mutex
	bodies []string
	heads  []http.Header
	paths  []string
}

func (r *recorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, string(body))
	r.heads = append(r.heads, req.Header.Clone())
	r.paths = append(r.paths, req.URL.Path)
}

func (r *recorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *fakeClock, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	clock := &fakeClock{t: time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)}
	c := New(srv.URL, config.Secret(canary), WithHTTPClient(srv.Client()))
	c.sleep, c.now = clock.sleep, clock.now
	return c, clock, rec
}

func sampleRequest(runType string) Request {
	grace, maxRuntime, exit, dur := 300, 3600, 2, int64(1234)
	req := Request{
		AgentVersion: "0.1.0",
		Job:          Job{Name: "nightly-backup", Schedule: "0 2 * * *", TimeZone: "Europe/Istanbul", GraceSeconds: &grace, MaxRuntimeSeconds: &maxRuntime},
		Run:          Run{ID: "3f2b8c1e-5a4d-4e6f-8b7a-1c2d3e4f5a6b", Type: runType},
	}
	if runType != RunStarted {
		req.Run.DurationMs = &dur
		req.Run.ExitCode = &exit
	}
	return req
}

func accepted(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"monitorId":"m-1","created":true,"warnings":["grace is capped"]}`)
}

func TestPostRunSendsTheContractRequest(t *testing.T) {
	c, _, rec := newTestClient(t, accepted)
	resp, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if resp.MonitorID != "m-1" || !resp.Created || len(resp.Warnings) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	h := rec.heads[0]
	if h.Get("Authorization") != "Bearer "+canary || h.Get("Content-Type") != "application/json" ||
		!strings.HasPrefix(h.Get("User-Agent"), "cronbeat-host/") {
		t.Fatalf("headers = %v", h)
	}
	if rec.paths[0] != "/agent/v1/host/runs" {
		t.Fatalf("path = %s", rec.paths[0])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(rec.bodies[0]), &got); err != nil {
		t.Fatal(err)
	}
	run := got["run"].(map[string]any)
	if run["id"] != "3f2b8c1e-5a4d-4e6f-8b7a-1c2d3e4f5a6b" || run["type"] != "failed" || run["exitCode"] != float64(2) || run["durationMs"] != float64(1234) {
		t.Fatalf("run = %v", run)
	}
	if _, has := run["signal"]; has {
		t.Fatal("empty signal must be omitted")
	}
}

func TestStartedRunOmitsFinishFields(t *testing.T) {
	c, _, rec := newTestClient(t, accepted)
	if _, err := c.PostRun(context.Background(), sampleRequest(RunStarted), StartedPolicy); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"durationMs", "exitCode", "signal", "output"} {
		if strings.Contains(rec.bodies[0], field) {
			t.Fatalf("started body contains %s: %s", field, rec.bodies[0])
		}
	}
}

func TestZeroExitCodeAndDurationAreSent(t *testing.T) {
	c, _, rec := newTestClient(t, accepted)
	zero, zeroMs := 0, int64(0)
	req := sampleRequest(RunSucceeded)
	req.Run.ExitCode, req.Run.DurationMs = &zero, &zeroMs
	if _, err := c.PostRun(context.Background(), req, FinishPolicy); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.bodies[0], `"exitCode":0`) || !strings.Contains(rec.bodies[0], `"durationMs":0`) {
		t.Fatalf("body = %s", rec.bodies[0])
	}
}

func TestServerErrorIsRetriedWithBackoffUntilSuccess(t *testing.T) {
	calls := 0
	c, clock, rec := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		accepted(w, r)
	})
	if _, err := c.PostRun(context.Background(), sampleRequest(RunSucceeded), FinishPolicy); err != nil {
		t.Fatal(err)
	}
	if rec.calls() != 2 || len(clock.sleeps) != 1 || clock.sleeps[0] != 300*time.Millisecond {
		t.Fatalf("calls=%d sleeps=%v", rec.calls(), clock.sleeps)
	}
}

func TestAtMostThreeAttemptsWithTheSpecifiedBackoff(t *testing.T) {
	c, clock, rec := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	_, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 502 {
		t.Fatalf("err = %v", err)
	}
	if rec.calls() != 3 || len(clock.sleeps) != 2 || clock.sleeps[0] != 300*time.Millisecond || clock.sleeps[1] != 900*time.Millisecond {
		t.Fatalf("calls=%d sleeps=%v", rec.calls(), clock.sleeps)
	}
}

func TestRetryAfterIsHonouredAndCappedAtThreeSeconds(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"1", time.Second},
		{"2", 2 * time.Second},
		{"120", 3 * time.Second},
		{"0", 300 * time.Millisecond},
		{"-5", 300 * time.Millisecond},
		{"garbage", 300 * time.Millisecond},
		{"", 300 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run("Retry-After "+tt.header, func(t *testing.T) {
			calls := 0
			c, clock, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					if tt.header != "" {
						w.Header().Set("Retry-After", tt.header)
					}
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				accepted(w, r)
			})
			if _, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy); err != nil {
				t.Fatal(err)
			}
			if len(clock.sleeps) != 1 || clock.sleeps[0] != tt.want {
				t.Fatalf("sleeps = %v, want [%v]", clock.sleeps, tt.want)
			}
		})
	}
}

func TestRetryAfterAsHTTPDate(t *testing.T) {
	calls := 0
	var clock *fakeClock
	c, fc, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", clock.now().Add(2*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		accepted(w, r)
	})
	clock = fc
	if _, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy); err != nil {
		t.Fatal(err)
	}
	if len(fc.sleeps) != 1 || fc.sleeps[0] < time.Second || fc.sleeps[0] > 3*time.Second {
		t.Fatalf("sleeps = %v", fc.sleeps)
	}
}

func TestPermanentClientErrorsAreNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413, 422, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, clock, rec := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"title":"nope","detail":"plan_limit_exceeded: monitor limit reached"}`)
			})
			_, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy)
			var se *StatusError
			if !errors.As(err, &se) || se.Status != status {
				t.Fatalf("err = %v", err)
			}
			if rec.calls() != 1 || len(clock.sleeps) != 0 {
				t.Fatalf("calls=%d sleeps=%v", rec.calls(), clock.sleeps)
			}
			if status != 307 && !strings.Contains(err.Error(), "plan_limit_exceeded") {
				t.Fatalf("detail missing from %q", err)
			}
		})
	}
}

func TestStartedPolicyMakesExactlyOneAttempt(t *testing.T) {
	c, clock, rec := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if _, err := c.PostRun(context.Background(), sampleRequest(RunStarted), StartedPolicy); err == nil {
		t.Fatal("expected an error")
	}
	if rec.calls() != 1 || len(clock.sleeps) != 0 {
		t.Fatalf("calls=%d sleeps=%v", rec.calls(), clock.sleeps)
	}
}

func TestRetriesUseTheIdenticalBody(t *testing.T) {
	calls := 0
	c, _, rec := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		accepted(w, r)
	})
	if _, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy); err != nil {
		t.Fatal(err)
	}
	if rec.calls() != 3 || rec.bodies[0] != rec.bodies[1] || rec.bodies[1] != rec.bodies[2] {
		t.Fatalf("bodies differ across retries: %q", rec.bodies)
	}
}

func TestResetConnectionIsRetried(t *testing.T) {
	calls := 0
	c, clock, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		accepted(w, r)
	})
	if _, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy); err != nil {
		t.Fatal(err)
	}
	if len(clock.sleeps) != 1 {
		t.Fatalf("sleeps = %v", clock.sleeps)
	}
}

func TestRetryAfterNeverPushesPastTheTotalBudget(t *testing.T) {
	c, clock, rec := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	policy := FinishPolicy
	policy.TotalBudget = 5 * time.Second
	if _, err := c.PostRun(context.Background(), sampleRequest(RunFailed), policy); err == nil {
		t.Fatal("expected an error")
	}
	if rec.calls() != 2 || clock.total() > 5*time.Second {
		t.Fatalf("calls=%d total sleep=%v", rec.calls(), clock.total())
	}
}

func TestDefaultFinishBudgetNeverExceedsTwelveSeconds(t *testing.T) {
	c, clock, rec := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, _ = c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy)
	if rec.calls() > 3 || clock.total() > 12*time.Second {
		t.Fatalf("calls=%d total sleep=%v", rec.calls(), clock.total())
	}
	if FinishPolicy.TotalBudget != 12*time.Second || FinishPolicy.AttemptTimeout != 4*time.Second || FinishPolicy.MaxAttempts != 3 {
		t.Fatalf("FinishPolicy = %+v", FinishPolicy)
	}
}

func TestSlowServerHitsTheAttemptTimeout(t *testing.T) {
	release := make(chan struct{})
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusAccepted)
	})
	t.Cleanup(func() { close(release) })
	policy := Policy{AttemptTimeout: 150 * time.Millisecond, MaxAttempts: 1, TotalBudget: time.Second}
	start := time.Now()
	_, err := c.PostRun(context.Background(), sampleRequest(RunStarted), policy)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if Kind(err) != "network" {
		t.Fatalf("Kind = %q", Kind(err))
	}
}

func TestCancelledContextStopsImmediately(t *testing.T) {
	c, clock, rec := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.PostRun(ctx, sampleRequest(RunFailed), FinishPolicy); err == nil {
		t.Fatal("expected an error")
	}
	if rec.calls() > 1 || len(clock.sleeps) != 0 {
		t.Fatalf("calls=%d sleeps=%v", rec.calls(), clock.sleeps)
	}
}

func TestUnreachableServerFailsFast(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New(url, config.Secret(canary))
	clock := &fakeClock{t: time.Now()}
	c.sleep, c.now = clock.sleep, clock.now
	start := time.Now()
	_, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy)
	if err == nil || Kind(err) != "network" {
		t.Fatalf("err = %v kind=%q", err, Kind(err))
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("connection refused took %v", time.Since(start))
	}
	if len(clock.sleeps) != 2 {
		t.Fatalf("sleeps = %v", clock.sleeps)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("redirect target was contacted")
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	c := New(origin.URL, config.Secret(canary))
	_, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v", err)
	}
}

func TestKeyNeverAppearsInErrorsOrFormattedClient(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"invalid key `+canary+` for Bearer `+canary+`"}`)
	})
	_, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy)
	all := []string{
		err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err),
		fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprintf("%s", c),
	}
	for _, s := range all {
		if strings.Contains(s, canary) {
			t.Fatalf("key leaked in %q", s)
		}
	}
	if Kind(err) != "auth" {
		t.Fatalf("Kind = %q", Kind(err))
	}
}

func TestErrorDetailIsSanitizedAndBounded(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "<html>\x1b[31mred\x00\nline2 "+strings.Repeat("x", 5000)+"</html>")
	})
	_, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy)
	msg := err.Error()
	if len(msg) > 400 || strings.ContainsAny(msg, "\x1b\x00\n") {
		t.Fatalf("message not sanitized (%d bytes): %q", len(msg), msg)
	}
}

func TestKindClassification(t *testing.T) {
	tests := map[int]string{400: "bad-request", 401: "auth", 403: "forbidden", 413: "bad-request", 429: "rate-limited", 500: "server", 503: "server", 404: "other"}
	for status, want := range tests {
		if got := Kind(&StatusError{Status: status}); got != want {
			t.Errorf("Kind(%d) = %q, want %q", status, got, want)
		}
	}
	if Kind(errors.New("dial tcp: refused")) != "network" || Kind(nil) != "" {
		t.Error("Kind of plain and nil errors")
	}
}

func TestCheckParsesTheKeyOwner(t *testing.T) {
	c, _, rec := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"prod-db-1","kind":"host","version":1}`)
	})
	resp, err := c.Check(context.Background(), Policy{AttemptTimeout: time.Second, MaxAttempts: 1, TotalBudget: time.Second})
	if err != nil || resp.Name != "prod-db-1" || resp.Kind != "host" || resp.Version != 1 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if rec.paths[0] != "/agent/v1/host/check" || rec.heads[0].Get("Authorization") != "Bearer "+canary {
		t.Fatalf("path=%s headers=%v", rec.paths[0], rec.heads[0])
	}
}

func TestGarbageSuccessBodyIsNotAnError(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "not json")
	})
	if _, err := c.PostRun(context.Background(), sampleRequest(RunFailed), FinishPolicy); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestContractGolden(t *testing.T) {
	golden := loadGolden(t)
	for name, req := range map[string]Request{
		"started":          sampleRequest(RunStarted),
		"succeeded":        withExit(sampleRequest(RunSucceeded), 0, "", ""),
		"failed":           withExit(sampleRequest(RunFailed), 2, "", "backup failed: disk full\n"),
		"failed_by_signal": withExit(sampleRequest(RunFailed), 137, "SIGKILL", ""),
	} {
		got, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, name, got, golden[name])
	}
}
