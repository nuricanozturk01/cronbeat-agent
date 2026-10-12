package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
	"cronbeat.dev/cronbeat-agent/internal/config"
	"cronbeat.dev/cronbeat-agent/internal/health"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

const key = "cb_agent_test"

var nopLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// backend is a stand-in for the CronBeat API. It records what it received in
// order and, like the real one, creates a monitor for every CronJob of a sync
// except the ones listed in refuse.
type backend struct {
	mu         sync.Mutex
	order      []string // "sync", "event:<type>:<cronJob>"
	syncs      []api.SyncRequest
	events     []api.Event
	heartbeats int
	failSyncs  int
	refuse     map[string]bool
}

func (b *backend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var req api.SyncRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.failSyncs > 0 {
			b.failSyncs--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		b.order = append(b.order, "sync")
		b.syncs = append(b.syncs, req)
		resp := api.SyncResponse{Monitors: []api.MonitorRef{}}
		for _, c := range req.CronJobs {
			if !b.refuse[c.Namespace+"/"+c.Name] {
				resp.Monitors = append(resp.Monitors, api.MonitorRef{Namespace: c.Namespace, Name: c.Name})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/agent/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var ev api.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		b.mu.Lock()
		b.order = append(b.order, "event:"+ev.Type+":"+ev.CronJobName)
		b.events = append(b.events, ev)
		b.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/agent/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		b.mu.Lock()
		b.heartbeats++
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+key {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func (b *backend) snapshot() (order []string, events []api.Event, syncs int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.order...), append([]api.Event(nil), b.events...), len(b.syncs)
}

func cronJob(name string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID("uid-" + name)},
		Spec:       batchv1.CronJobSpec{Schedule: "*/5 * * * *"},
	}
}

func completedJob(cronJobName, jobName string) *batchv1.Job {
	start := time.Now().Add(-2 * time.Minute)
	done := start.Add(40 * time.Second)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: jobName, UID: types.UID("job-" + jobName),
			CreationTimestamp: metav1.NewTime(start),
			OwnerReferences:   []metav1.OwnerReference{{Kind: "CronJob", Name: cronJobName}},
		},
		Status: batchv1.JobStatus{
			StartTime: &metav1.Time{Time: start}, CompletionTime: &metav1.Time{Time: done},
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(done),
			}},
		},
	}
}

type running struct {
	backend *backend
	kube    *fake.Clientset
	health  *health.Health
	cancel  context.CancelFunc
	done    chan error
	once    sync.Once
	err     error
	stopped bool
}

func (r *running) stop() (error, bool) {
	r.once.Do(func() {
		r.cancel()
		select {
		case r.err = <-r.done:
			r.stopped = true
		case <-time.After(5 * time.Second):
		}
	})
	return r.err, r.stopped
}

func start(t *testing.T, b *backend, objects ...runtime.Object) *running {
	t.Helper()
	srv := httptest.NewServer(b.handler())
	t.Cleanup(srv.Close)
	kube := fake.NewSimpleClientset(objects...)
	client := api.NewClient(srv.URL, key, srv.Client(), api.RetryConfig{
		MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxRetryAfter: time.Second,
	}, nopLog)
	h := health.New(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{backend: b, kube: kube, health: h, cancel: cancel, done: make(chan error, 1)}
	go func() {
		r.done <- Run(ctx, Options{
			Config: config.Config{ClusterName: "prod"},
			Kube:   kube, API: client, Log: nopLog, Health: h,
			Timing: Timing{
				CacheSyncTimeout: 10 * time.Second, ShutdownGrace: time.Second, SlowRetry: 20 * time.Millisecond,
				SyncInterval: time.Hour, HeartbeatInterval: time.Hour,
				ChangeDebounce: 30 * time.Millisecond, MinChangeGap: 0, RetryBase: 30 * time.Millisecond,
			},
		})
	}()
	t.Cleanup(func() {
		if _, ok := r.stop(); !ok {
			t.Error("the agent did not stop")
		}
	})
	return r
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: not met in time", what)
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

func count(list []string, want string) int {
	n := 0
	for _, v := range list {
		if v == want {
			n++
		}
	}
	return n
}

func TestTheFirstSyncComesBeforeAnyEventAndEveryEventIsSentOnce(t *testing.T) {
	b := &backend{}
	start(t, b, cronJob("backup"), completedJob("backup", "backup-1"))

	eventually(t, "both events", func() bool { order, _, _ := b.snapshot(); return len(order) >= 3 })
	time.Sleep(300 * time.Millisecond)

	order, events, _ := b.snapshot()
	if order[0] != "sync" {
		t.Fatalf("the CronJob must be known before its events, order = %v", order)
	}
	if count(order, "event:job_started:backup") != 1 || count(order, "event:job_succeeded:backup") != 1 || len(events) != 2 {
		t.Fatalf("each event exactly once, order = %v", order)
	}
	for _, ev := range events {
		if ev.ClusterName != "prod" || ev.Namespace != "default" || ev.JobUID != "job-backup-1" {
			t.Fatalf("event = %+v", ev)
		}
	}
}

func TestANewCronJobIsSyncedWithinSecondsAndItsFirstRunIsNotLost(t *testing.T) {
	b := &backend{}
	r := start(t, b)
	eventually(t, "first sync", func() bool { _, _, syncs := b.snapshot(); return syncs == 1 })
	ctx := context.Background()

	// the CronJob appears and its first Job finishes long before the next 5 minute sync
	if _, err := r.kube.BatchV1().CronJobs("default").Create(ctx, cronJob("nightly"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.kube.BatchV1().Jobs("default").Create(ctx, completedJob("nightly", "nightly-1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "events of the new CronJob", func() bool {
		order, _, _ := b.snapshot()
		return indexOf(order, "event:job_succeeded:nightly") >= 0
	})
	order, _, _ := b.snapshot()
	syncIdx := indexOf(order[1:], "sync") + 1
	if syncIdx == 0 || syncIdx > indexOf(order, "event:job_started:nightly") {
		t.Fatalf("a sync with the new CronJob must come before its events, order = %v", order)
	}
	if count(order, "event:job_started:nightly") != 1 || count(order, "event:job_succeeded:nightly") != 1 {
		t.Fatalf("each event exactly once, order = %v", order)
	}
}

func TestNothingIsReportedWhileTheAPIIsDownAndNothingIsLostWhenItReturns(t *testing.T) {
	b := &backend{failSyncs: 3}
	start(t, b, cronJob("backup"), completedJob("backup", "backup-1"))

	time.Sleep(60 * time.Millisecond)
	if order, _, _ := b.snapshot(); len(order) != 0 && order[0] != "sync" {
		t.Fatalf("events must wait for a successful sync, got %v", order)
	}

	eventually(t, "recovery", func() bool { order, _, _ := b.snapshot(); return len(order) >= 3 })

	order, _, _ := b.snapshot()
	if order[0] != "sync" || count(order, "event:job_started:backup") != 1 || count(order, "event:job_succeeded:backup") != 1 {
		t.Fatalf("order = %v", order)
	}
}

func TestJobsOfACronJobTheAPIDoesNotMonitorSendNothing(t *testing.T) {
	b := &backend{refuse: map[string]bool{"default/over-the-limit": true}}
	start(t, b,
		cronJob("over-the-limit"), completedJob("over-the-limit", "over-1"),
		cronJob("backup"), completedJob("backup", "backup-1"))

	eventually(t, "the monitored CronJob is reported", func() bool {
		order, _, _ := b.snapshot()
		return indexOf(order, "event:job_succeeded:backup") >= 0
	})
	time.Sleep(300 * time.Millisecond)

	order, _, _ := b.snapshot()
	if indexOf(order, "event:job_started:over-the-limit") >= 0 || indexOf(order, "event:job_succeeded:over-the-limit") >= 0 {
		t.Fatalf("events of an unmonitored CronJob are wasted traffic, order = %v", order)
	}
}

func TestTheAgentIsReadyOnceItsCachesSyncedAndStopsCleanly(t *testing.T) {
	b := &backend{}
	r := start(t, b, cronJob("backup"))

	eventually(t, "ready", func() bool {
		rec := httptest.NewRecorder()
		r.health.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code == http.StatusOK
	})
	eventually(t, "heartbeat", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.heartbeats >= 1
	})

	err, ok := r.stop()
	if !ok || err != nil {
		t.Fatalf("stopped = %v, Run = %v", ok, err)
	}
}
