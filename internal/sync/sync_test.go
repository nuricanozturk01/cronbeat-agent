package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
)

var nopLog = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeSender struct {
	mu    sync.Mutex
	sent  []api.Event
	errs  []error // consumed one per call
	calls int
}

func (f *fakeSender) SendEvent(_ context.Context, ev api.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return err
		}
	}
	f.sent = append(f.sent, ev)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func ev(uid, typ string) api.Event { return api.Event{JobUID: uid, Type: typ} }

func TestQueueDeduplicatesPerJobAndType(t *testing.T) {
	s := &fakeSender{}
	q := NewQueue(s, 10, time.Millisecond, nopLog)
	q.Emit(ev("a", api.EventJobStarted))
	q.Emit(ev("a", api.EventJobStarted))
	q.Emit(ev("a", api.EventJobFailed))
	q.Emit(ev("b", api.EventJobStarted))
	if q.Pending() != 3 {
		t.Fatalf("pending = %d", q.Pending())
	}
	q.Drain(context.Background())
	if s.count() != 3 {
		t.Fatalf("sent = %d", s.count())
	}
	q.Emit(ev("a", api.EventJobStarted)) // still known after delivery
	if q.Pending() != 0 {
		t.Fatal("re-sent a delivered event")
	}
	q.Forget("a")
	q.Emit(ev("a", api.EventJobStarted))
	if q.Pending() != 1 {
		t.Fatal("Forget should release the dedupe entry")
	}
}

func TestQueueFullDropsWithoutBlockingAndAllowsRetry(t *testing.T) {
	q := NewQueue(&fakeSender{}, 1, time.Millisecond, nopLog)
	q.Emit(ev("a", api.EventJobStarted))
	done := make(chan struct{})
	go func() { q.Emit(ev("b", api.EventJobStarted)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Emit blocked on a full queue")
	}
	q.Drain(context.Background())
	q.Emit(ev("b", api.EventJobStarted)) // dropped earlier, so accepted now
	if q.Pending() != 1 {
		t.Fatalf("pending = %d", q.Pending())
	}
}

func TestQueueRetriesTransientAndUnauthorizedButDropsPermanent(t *testing.T) {
	s := &fakeSender{errs: []error{
		&api.StatusError{Status: 502},
		&api.StatusError{Status: 401},
		nil,                           // delivered on the third call
		&api.StatusError{Status: 400}, // dropped
	}}
	q := NewQueue(s, 10, time.Millisecond, nopLog)
	q.Emit(ev("a", api.EventJobStarted))
	q.Emit(ev("b", api.EventJobStarted))
	q.Drain(context.Background())
	if s.count() != 1 || s.calls != 4 {
		t.Fatalf("sent=%d calls=%d", s.count(), s.calls)
	}
}

func TestQueueRunStopsOnContext(t *testing.T) {
	s := &fakeSender{errs: []error{errors.New("down"), errors.New("down"), errors.New("down")}}
	q := NewQueue(s, 10, time.Hour, nopLog)
	q.Emit(ev("a", api.EventJobStarted))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

type fakeAPI struct {
	mu         sync.Mutex
	syncReqs   []api.SyncRequest
	heartbeats []api.HeartbeatRequest
	syncErrs   []error // consumed one per Sync call
	monitors   []api.MonitorRef
	syncedAt   []time.Time
}

func (f *fakeAPI) Sync(_ context.Context, r api.SyncRequest) (api.SyncResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncReqs = append(f.syncReqs, r)
	f.syncedAt = append(f.syncedAt, time.Now())
	if len(f.syncErrs) > 0 {
		err := f.syncErrs[0]
		f.syncErrs = f.syncErrs[1:]
		if err != nil {
			return api.SyncResponse{}, err
		}
	}
	return api.SyncResponse{Monitors: f.monitors, Warnings: []string{"limit"}}, nil
}

func (f *fakeAPI) Heartbeat(_ context.Context, r api.HeartbeatRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats = append(f.heartbeats, r)
	return nil
}

func (f *fakeAPI) syncs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncReqs)
}

type fakeSource struct {
	mu       sync.Mutex
	jobs     []api.CronJob
	listed   int
	counted  int
	replayed int
}

func (s *fakeSource) CronJobs() []api.CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed++
	return append([]api.CronJob(nil), s.jobs...)
}

func (s *fakeSource) CronJobCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counted++
	return len(s.jobs)
}

func (s *fakeSource) ReplayJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replayed++
}

func (s *fakeSource) stats() (listed, counted, replayed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listed, s.counted, s.replayed
}

func cj(namespace, name string) api.CronJob {
	return api.CronJob{Namespace: namespace, Name: name, UID: "uid-" + name, Schedule: "* * * * *"}
}

func fast(a *fakeAPI, src *fakeSource) *Syncer {
	return &Syncer{
		API: a, Source: src, Replay: src, Known: NewKnownSet(),
		ClusterName: "prod", AgentVersion: "0.2.0",
		SyncInterval: time.Hour, HeartbeatInterval: time.Hour,
		ChangeDebounce: 20 * time.Millisecond, MinChangeGap: 0, RetryBase: 20 * time.Millisecond,
		Jitter: func() float64 { return 0 },
		Log:    nopLog,
	}
}

func runSyncer(t *testing.T, s *Syncer) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Run did not stop")
		}
	})
	return cancel
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: not met in time", what)
}

func TestKnownSetIsReplacedAsAWhole(t *testing.T) {
	k := NewKnownSet()
	if k.Has("n", "a") {
		t.Fatal("empty set knows nothing")
	}

	k.Replace([]api.MonitorRef{{Namespace: "n", Name: "a"}, {Namespace: "n", Name: "b"}})
	if !k.Has("n", "a") || !k.Has("n", "b") || k.Has("x", "a") || k.Len() != 2 {
		t.Fatalf("set = %d", k.Len())
	}

	k.Replace([]api.MonitorRef{{Namespace: "n", Name: "b"}})
	if k.Has("n", "a") || !k.Has("n", "b") {
		t.Fatal("a CronJob the API no longer reports must be forgotten")
	}
}

func TestSyncerSyncsAndHeartbeatsOnStartWithoutBuildingThePayloadForTheHeartbeat(t *testing.T) {
	a := &fakeAPI{monitors: []api.MonitorRef{{Namespace: "n", Name: "a"}}}
	src := &fakeSource{jobs: []api.CronJob{cj("n", "a"), cj("n", "b")}}
	s := fast(a, src)
	beats := 0
	var mu sync.Mutex
	s.Beat = func() { mu.Lock(); beats++; mu.Unlock() }

	runSyncer(t, s)
	eventually(t, "sync and heartbeat", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.syncReqs) == 1 && len(a.heartbeats) == 1
	})

	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.syncReqs[0].CronJobs) != 2 || a.syncReqs[0].ClusterName != "prod" || a.syncReqs[0].AgentVersion != "0.2.0" {
		t.Fatalf("sync = %+v", a.syncReqs[0])
	}
	if a.heartbeats[0].CronJobCount != 2 {
		t.Fatalf("heartbeat = %+v", a.heartbeats[0])
	}
	listed, counted, _ := src.stats()
	if listed != 1 || counted != 1 {
		t.Fatalf("the heartbeat must count, not list: listed=%d counted=%d", listed, counted)
	}
	mu.Lock()
	defer mu.Unlock()
	if beats != 1 {
		t.Fatalf("liveness beats = %d", beats)
	}
}

func TestASuccessfulSyncMakesTheReportedCronJobsKnownAndReplaysJobs(t *testing.T) {
	a := &fakeAPI{monitors: []api.MonitorRef{{Namespace: "n", Name: "a"}}}
	src := &fakeSource{jobs: []api.CronJob{cj("n", "a"), cj("n", "b")}}
	s := fast(a, src)

	runSyncer(t, s)
	eventually(t, "replay", func() bool { _, _, replayed := src.stats(); return replayed == 1 })

	if !s.Known.Has("n", "a") {
		t.Fatal("a CronJob with a monitor must be known")
	}
	if s.Known.Has("n", "b") {
		t.Fatal("a CronJob without a monitor (limit, bad schedule) must stay unknown")
	}
}

func TestAFailedSyncKeepsWhatWasKnownAndReplaysNothing(t *testing.T) {
	a := &fakeAPI{syncErrs: []error{&api.StatusError{Status: 500}}}
	src := &fakeSource{jobs: []api.CronJob{cj("n", "a")}}
	s := fast(a, src)
	s.Known.Replace([]api.MonitorRef{{Namespace: "n", Name: "old"}})
	s.RetryBase = time.Hour

	runSyncer(t, s)
	eventually(t, "first sync", func() bool { return a.syncs() == 1 })
	time.Sleep(60 * time.Millisecond)

	if !s.Known.Has("n", "old") {
		t.Fatal("a failed sync must not wipe the known CronJobs")
	}
	if _, _, replayed := src.stats(); replayed != 0 {
		t.Fatalf("replayed %d times after a failure", replayed)
	}
}

func TestATransientFailureIsRetriedSoonAndNotOnlyAtTheNextInterval(t *testing.T) {
	a := &fakeAPI{syncErrs: []error{&api.StatusError{Status: 503}, &api.StatusError{Status: 503}}}
	src := &fakeSource{jobs: []api.CronJob{cj("n", "a")}}
	s := fast(a, src)

	runSyncer(t, s)

	eventually(t, "third attempt succeeds", func() bool { return a.syncs() >= 3 })
	eventually(t, "replay after recovery", func() bool { _, _, replayed := src.stats(); return replayed == 1 })
}

func TestARetryDelayGrowsAndNeverExceedsTheSyncInterval(t *testing.T) {
	s := &Syncer{SyncInterval: 5 * time.Minute, RetryBase: 15 * time.Second, Jitter: func() float64 { return 0 }}

	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := s.retryDelay(i + 1); got != w {
			t.Fatalf("retry %d = %s, want %s", i+1, got, w)
		}
	}
	s.Jitter = func() float64 { return 1 }
	if got := s.retryDelay(1); got != 18*time.Second {
		t.Fatalf("jitter must add at most 20%%, got %s", got)
	}
	if got := s.retryDelay(40); got > 5*time.Minute {
		t.Fatalf("delay %s above the interval", got)
	}
}

func TestAPermanentRejectionIsNotRetriedQuickly(t *testing.T) {
	a := &fakeAPI{syncErrs: []error{&api.StatusError{Status: 400, Body: "bad"}}}
	src := &fakeSource{jobs: []api.CronJob{cj("n", "a")}}
	s := fast(a, src)
	s.RetryBase = 10 * time.Millisecond

	runSyncer(t, s)
	eventually(t, "first sync", func() bool { return a.syncs() == 1 })
	time.Sleep(150 * time.Millisecond)

	if a.syncs() != 1 {
		t.Fatalf("a 400 cannot be fixed by retrying, syncs = %d", a.syncs())
	}
}

func TestInvalidCronJobsAreSkippedAndTheRestIsStillSynced(t *testing.T) {
	long := make([]byte, 254)
	for i := range long {
		long[i] = 'a'
	}
	bad := []api.CronJob{
		{Namespace: "n", Name: string(long), Schedule: "* * * * *"},
		{Namespace: "n", Name: "no-schedule"},
		{Namespace: "", Name: "no-namespace", Schedule: "* * * * *"},
		{Namespace: "n", Name: "tz", Schedule: "* * * * *", TimeZone: string(long)},
	}
	a := &fakeAPI{}
	src := &fakeSource{jobs: append([]api.CronJob{cj("n", "good")}, bad...)}
	s := fast(a, src)

	runSyncer(t, s)
	eventually(t, "sync", func() bool { return a.syncs() == 1 })

	a.mu.Lock()
	defer a.mu.Unlock()
	if got := a.syncReqs[0].CronJobs; len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("one bad CronJob must not block the rest, sent %+v", got)
	}
}

// lockedBuffer lets a test read what the syncer goroutine logged.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAnnotationSettingsAreSentAndAnInvalidOneIsLoggedNotSent(t *testing.T) {
	tuned := cj("n", "tuned")
	tuned.GraceSeconds, tuned.MaxRuntimeSeconds = 900, 3600
	broken := cj("n", "broken")
	broken.Problem = `cronbeat.dev/grace "soon": not a duration`
	logs := &lockedBuffer{}
	a := &fakeAPI{}
	src := &fakeSource{jobs: []api.CronJob{tuned, broken, cj("n", "plain")}}
	s := fast(a, src)
	s.Log = slog.New(slog.NewTextHandler(logs, nil))

	runSyncer(t, s)
	eventually(t, "sync", func() bool { return a.syncs() == 1 })

	a.mu.Lock()
	req := a.syncReqs[0]
	a.mu.Unlock()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `"graceSeconds":900`) || !strings.Contains(text, `"maxRuntimeSeconds":3600`) {
		t.Fatalf("the configured values must be sent: %s", text)
	}
	if strings.Contains(text, "not a duration") || strings.Contains(text, "ignored") || strings.Contains(text, "Problem") {
		t.Fatalf("local fields must stay local: %s", text)
	}
	if strings.Count(text, "graceSeconds") != 1 {
		t.Fatalf("unset values must be omitted: %s", text)
	}
	if len(req.CronJobs) != 3 {
		t.Fatalf("an invalid annotation must not remove the CronJob from the sync, sent %d", len(req.CronJobs))
	}
	if !strings.Contains(logs.String(), "n/broken") || !strings.Contains(logs.String(), "cronbeat.dev/grace") {
		t.Fatalf("the problem must be logged, logs = %s", logs.String())
	}
}

func TestMoreCronJobsThanTheAPIAcceptsAreNeverSentPartially(t *testing.T) {
	jobs := make([]api.CronJob, 0, api.MaxCronJobs+1)
	for i := 0; i <= api.MaxCronJobs; i++ {
		jobs = append(jobs, cj("n", "job-"+itoa(i)))
	}
	a := &fakeAPI{}
	src := &fakeSource{jobs: jobs}
	s := fast(a, src)
	s.Known.Replace([]api.MonitorRef{{Namespace: "n", Name: "kept"}})

	runSyncer(t, s)
	time.Sleep(150 * time.Millisecond)

	if a.syncs() != 0 {
		t.Fatal("a partial list would make the API pause every CronJob it left out")
	}
	if !s.Known.Has("n", "kept") {
		t.Fatal("the known set must stay")
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var out []byte
	for ; i > 0; i /= 10 {
		out = append([]byte{digits[i%10]}, out...)
	}
	return string(out)
}

func TestAChangeTriggersAnImmediateSyncAndABurstIsCoalesced(t *testing.T) {
	a := &fakeAPI{}
	src := &fakeSource{jobs: []api.CronJob{cj("n", "a")}}
	changes := make(chan struct{}, 1)
	s := fast(a, src)
	s.Changes = changes

	runSyncer(t, s)
	eventually(t, "first sync", func() bool { return a.syncs() == 1 })

	changes <- struct{}{}
	eventually(t, "sync after the change", func() bool { return a.syncs() == 2 })
	for range 5 {
		select {
		case changes <- struct{}{}:
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	eventually(t, "sync after the burst", func() bool { return a.syncs() >= 3 })
	time.Sleep(150 * time.Millisecond)

	if got := a.syncs(); got > 4 {
		t.Fatalf("a burst must be coalesced, syncs = %d", got)
	}
}

func TestTriggeredSyncsRespectTheMinimumGap(t *testing.T) {
	a := &fakeAPI{}
	src := &fakeSource{jobs: []api.CronJob{cj("n", "a")}}
	changes := make(chan struct{}, 1)
	s := fast(a, src)
	s.Changes = changes
	s.MinChangeGap = 200 * time.Millisecond

	runSyncer(t, s)
	eventually(t, "first sync", func() bool { return a.syncs() == 1 })
	changes <- struct{}{}
	eventually(t, "second sync", func() bool { return a.syncs() == 2 })

	a.mu.Lock()
	gap := a.syncedAt[1].Sub(a.syncedAt[0])
	a.mu.Unlock()
	if gap < 180*time.Millisecond {
		t.Fatalf("two syncs only %s apart", gap)
	}
}

func TestRunStopsPromptlyWhileWaitingForTheNextSync(t *testing.T) {
	a := &fakeAPI{}
	src := &fakeSource{}
	s := fast(a, src)
	s.Changes = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	eventually(t, "first sync", func() bool { return a.syncs() == 1 })

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}
