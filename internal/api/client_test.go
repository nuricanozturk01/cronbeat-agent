package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "cb_agent_supersecretkey"

type harness struct {
	client *Client
	logs   *bytes.Buffer
	sleeps []time.Duration
}

func newHarness(t *testing.T, h http.HandlerFunc) *harness {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hr := &harness{logs: logs}
	hr.client = NewClient(srv.URL, testKey, srv.Client(), RetryConfig{
		MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 8 * time.Second, MaxRetryAfter: time.Minute,
	}, log)
	hr.client.sleep = func(_ context.Context, d time.Duration) error {
		hr.sleeps = append(hr.sleeps, d)
		return nil
	}
	hr.client.jitter = func() float64 { return 1 }
	return hr
}

func TestRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad headers: %v", r.Header)
		}
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	if err := h.client.SendEvent(context.Background(), Event{JobUID: "u", Type: EventJobStarted}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
	// jitter=1 gives the full exponential value: 1s then 2s
	if len(h.sleeps) != 2 || h.sleeps[0] != time.Second || h.sleeps[1] != 2*time.Second {
		t.Fatalf("sleeps = %v", h.sleeps)
	}
	if strings.Contains(h.logs.String(), testKey) {
		t.Fatal("API key leaked into logs")
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := h.client.Heartbeat(context.Background(), HeartbeatRequest{})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 500 {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	for _, status := range []int{400, 404, 422, 408} {
		var calls atomic.Int32
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
		})
		err := h.client.SendEvent(context.Background(), Event{})
		if calls.Load() != 1 || !IsPermanent(err) {
			t.Fatalf("status %d: calls=%d err=%v", status, calls.Load(), err)
		}
	}
}

func TestUnauthorizedNotRetriedAndLoggedClearly(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	err := h.client.SendEvent(context.Background(), Event{})
	if !IsUnauthorized(err) || IsPermanent(err) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "401") || strings.Contains(logs, testKey) {
		t.Fatalf("logs = %s", logs)
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := h.client.Heartbeat(context.Background(), HeartbeatRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(h.sleeps) != 1 || h.sleeps[0] != 7*time.Second {
		t.Fatalf("sleeps = %v", h.sleeps)
	}
}

func TestRetryAfterIsCapped(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "86400")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	_ = h.client.Heartbeat(context.Background(), HeartbeatRequest{})
	if len(h.sleeps) != 1 || h.sleeps[0] != time.Minute {
		t.Fatalf("sleeps = %v", h.sleeps)
	}
}

func TestNetworkErrorIsRetried(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused
	logs := &bytes.Buffer{}
	c := NewClient(url, testKey, nil, RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, slog.New(slog.NewJSONHandler(logs, nil)))
	var sleeps int
	c.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
	if err := c.Heartbeat(context.Background(), HeartbeatRequest{}); err == nil {
		t.Fatal("expected error")
	}
	if sleeps != 2 {
		t.Fatalf("sleeps = %d", sleeps)
	}
	if strings.Contains(logs.String(), testKey) {
		t.Fatal("key leaked")
	}
}

func TestSyncWireFormat(t *testing.T) {
	var body map[string]any
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/v1/sync" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"monitors":[{"namespace":"d","name":"a"}],"warnings":["plan limit"]}`))
	})
	resp, err := h.client.Sync(context.Background(), SyncRequest{ClusterName: "c", AgentVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Monitors) != 1 || resp.Warnings[0] != "plan limit" {
		t.Fatalf("resp = %+v", resp)
	}
	if list, ok := body["cronJobs"].([]any); !ok || len(list) != 0 {
		t.Fatalf("cronJobs must be an empty array, got %#v", body["cronJobs"])
	}
}

func TestEventOmitsEmptyOptionalFields(t *testing.T) {
	b, _ := json.Marshal(Event{
		ClusterName: "c", Namespace: "n", CronJobName: "j", JobUID: "u", Type: EventJobStarted,
		OccurredAt: time.Date(2026, 10, 3, 2, 0, 12, 0, time.UTC),
	})
	s := string(b)
	for _, k := range []string{"durationMs", "reason", "message"} {
		if strings.Contains(s, k) {
			t.Errorf("%s should be omitted: %s", k, s)
		}
	}
	if !strings.Contains(s, `"occurredAt":"2026-10-03T02:00:12Z"`) {
		t.Errorf("occurredAt format: %s", s)
	}
}

func TestCronJobTimeZoneOmittedWhenEmpty(t *testing.T) {
	b, _ := json.Marshal(CronJob{Namespace: "n", Name: "a", UID: "u", Schedule: "* * * * *", Suspend: true})
	s := string(b)
	if strings.Contains(s, "timeZone") || !strings.Contains(s, `"suspend":true`) {
		t.Fatalf("payload = %s", s)
	}
}

func TestUserAgentNamesTheAgentAndItsVersion(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "key", nil, RetryConfig{MaxAttempts: 1}, slog.New(slog.DiscardHandler), WithUserAgent("cronbeat-agent/0.2.0"))

	if err := c.Heartbeat(context.Background(), HeartbeatRequest{ClusterName: "c"}); err != nil {
		t.Fatal(err)
	}

	if seen != "cronbeat-agent/0.2.0" {
		t.Fatalf("user agent = %q", seen)
	}
}

func TestTheHTTPClientKeepsFewIdleConnectionsAndDoesNotFollowRedirects(t *testing.T) {
	c := NewHTTPClient()
	tr := c.Transport.(*http.Transport)

	if tr.MaxIdleConns > 4 || tr.MaxIdleConnsPerHost > 2 || tr.IdleConnTimeout == 0 {
		t.Fatalf("idle pool too generous: %d/%d %s", tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.IdleConnTimeout)
	}
	if c.Timeout != 15*time.Second {
		t.Fatalf("timeout = %s", c.Timeout)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://x.test", nil)
	if err := c.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirects must not be followed, got %v", err)
	}
}

func TestCronJobSchedulingFieldsAreOnlySentWhenSet(t *testing.T) {
	plain, err := json.Marshal(CronJob{Namespace: "n", Name: "a", Schedule: "* * * * *"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"concurrencyPolicy", "startingDeadlineSeconds", "lastScheduleTime", "lastSuccessfulTime", "activeJobs"} {
		if strings.Contains(string(plain), key) {
			t.Fatalf("%s must be left out when empty: %s", key, plain)
		}
	}
	full, err := json.Marshal(CronJob{
		Namespace: "n", Name: "a", Schedule: "* * * * *", ConcurrencyPolicy: "Forbid",
		StartingDeadlineSeconds: 120, LastScheduleTime: "2026-10-08T02:00:00Z", ActiveJobs: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"concurrencyPolicy":"Forbid"`, `"startingDeadlineSeconds":120`, `"lastScheduleTime":"2026-10-08T02:00:00Z"`, `"activeJobs":1`} {
		if !strings.Contains(string(full), want) {
			t.Fatalf("missing %s in %s", want, full)
		}
	}
}
