package k8s

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

var (
	t0       = time.Date(2026, 10, 3, 2, 0, 0, 0, time.UTC)
	nopLog   = slog.New(slog.NewTextHandler(io.Discard, nil))
	fixedNow = func() time.Time { return t0.Add(2 * time.Minute) }
)

func failedJob(reason, message string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "backup-123", UID: types.UID("job-uid"),
			CreationTimestamp: metav1.NewTime(t0),
			OwnerReferences:   []metav1.OwnerReference{{Kind: "CronJob", Name: "backup"}},
		},
		Status: batchv1.JobStatus{
			StartTime: ptr(metav1.NewTime(t0)),
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message,
				LastTransitionTime: metav1.NewTime(t0.Add(53 * time.Second)),
			}},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func podWith(name string, statuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: name,
			Labels: map[string]string{"batch.kubernetes.io/job-name": "backup-123"},
		},
		Status: corev1.PodStatus{ContainerStatuses: statuses},
	}
}

func terminated(name, reason string, code int32, last bool) corev1.ContainerStatus {
	st := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code}}
	if last {
		return corev1.ContainerStatus{Name: name, LastTerminationState: st}
	}
	return corev1.ContainerStatus{Name: name, State: st}
}

func waiting(name, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
}

func TestInferFailure(t *testing.T) {
	tests := []struct {
		name       string
		job        *batchv1.Job
		pods       []*corev1.Pod // most recent first
		wantReason string
		wantMsg    string
	}{
		{
			name:       "deadline exceeded condition wins over pod state",
			job:        failedJob("DeadlineExceeded", "Job was active longer than specified deadline"),
			pods:       []*corev1.Pod{podWith("p", terminated("app", "Error", 1, false))},
			wantReason: "DeadlineExceeded",
			wantMsg:    "container app terminated: Error (exit code 1); Job was active longer than specified deadline",
		},
		{
			name:       "backoff limit replaced by OOMKilled from last state",
			job:        failedJob("BackoffLimitExceeded", "Job has reached the specified backoff limit"),
			pods:       []*corev1.Pod{podWith("p", terminated("backup", "OOMKilled", 137, true))},
			wantReason: "OOMKilled",
			wantMsg:    "container backup terminated: OOMKilled (exit code 137); Job has reached the specified backoff limit",
		},
		{
			name:       "plain Error exit code from current state",
			job:        failedJob("BackoffLimitExceeded", ""),
			pods:       []*corev1.Pod{podWith("p", terminated("app", "Error", 2, false))},
			wantReason: "Error",
			wantMsg:    "container app terminated: Error (exit code 2); job failed: BackoffLimitExceeded",
		},
		{
			name:       "ImagePullBackOff waiting",
			job:        failedJob("BackoffLimitExceeded", ""),
			pods:       []*corev1.Pod{podWith("p", waiting("app", "ImagePullBackOff"))},
			wantReason: "ImagePullBackOff",
			wantMsg:    "container app waiting: ImagePullBackOff; job failed: BackoffLimitExceeded",
		},
		{
			name: "CrashLoopBackOff with last terminated prefers the terminated reason",
			job:  failedJob("BackoffLimitExceeded", ""),
			pods: []*corev1.Pod{podWith("p", func() corev1.ContainerStatus {
				s := waiting("app", "CrashLoopBackOff")
				s.LastTerminationState = terminated("app", "Error", 1, false).State
				return s
			}())},
			wantReason: "Error",
			wantMsg:    "container app terminated: Error (exit code 1); job failed: BackoffLimitExceeded",
		},
		{
			name:       "CrashLoopBackOff alone",
			job:        failedJob("", ""),
			pods:       []*corev1.Pod{podWith("p", waiting("app", "CrashLoopBackOff"))},
			wantReason: "CrashLoopBackOff",
			wantMsg:    "container app waiting: CrashLoopBackOff",
		},
		{
			name:       "OOMKilled preferred over other containers and pods",
			job:        failedJob("BackoffLimitExceeded", ""),
			pods:       []*corev1.Pod{podWith("new", terminated("a", "Error", 1, false)), podWith("old", terminated("b", "OOMKilled", 137, false))},
			wantReason: "OOMKilled",
			wantMsg:    "container b terminated: OOMKilled (exit code 137); job failed: BackoffLimitExceeded",
		},
		{
			name:       "ContainerCreating and Completed are not failures",
			job:        failedJob("BackoffLimitExceeded", "limit"),
			pods:       []*corev1.Pod{podWith("p", waiting("a", "ContainerCreating"), terminated("b", "Completed", 0, false))},
			wantReason: "BackoffLimitExceeded",
			wantMsg:    "limit",
		},
		{
			name:       "no pods known: condition only",
			job:        failedJob("BackoffLimitExceeded", ""),
			wantReason: "BackoffLimitExceeded",
			wantMsg:    "job failed: BackoffLimitExceeded",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			for i := len(tc.pods) - 1; i >= 0; i-- { // oldest first so the first listed is the most recent
				tr.Update("default", "backup-123", tc.pods[i].Name, ExtractStates(tc.pods[i]))
			}
			got := InferFailure(tc.job, tr.States("default", "backup-123"))
			if got.Reason != tc.wantReason || got.Message != tc.wantMsg {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestPodJobNameLabelsAndFallback(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"job-name": "legacy"}}}
	if PodJobName(p) != "legacy" {
		t.Fatal("legacy label")
	}
	p = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "owned"}}}}
	if PodJobName(p) != "owned" {
		t.Fatal("owner fallback")
	}
	if PodJobName(&corev1.Pod{}) != "" {
		t.Fatal("no job")
	}
}

func TestTrackerIsBounded(t *testing.T) {
	tr := newTracker(3, 2)
	st := []ContainerState{{Container: "c", Reason: "Error", ExitCode: 1}}
	for _, j := range []string{"a", "b", "c", "d"} {
		tr.Update("ns", j, "p", st)
	}
	if tr.Len() != 3 || tr.States("ns", "a") != nil {
		t.Fatalf("len=%d, oldest job should be evicted", tr.Len())
	}
	tr.Update("ns", "d", "p2", st)
	tr.Update("ns", "d", "p3", st)
	if n := len(tr.States("ns", "d")); n != 2 {
		t.Fatalf("pods per job not capped: %d", n)
	}
	tr.Forget("ns", "d")
	if tr.States("ns", "d") != nil {
		t.Fatal("Forget failed")
	}
}

type recordingSink struct {
	mu     sync.Mutex
	events []api.Event
	seen   map[string]bool
	forgot []string
}

func newSink() *recordingSink { return &recordingSink{seen: map[string]bool{}} }

func (s *recordingSink) Emit(ev api.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[ev.DedupeKey()] {
		return
	}
	s.seen[ev.DedupeKey()] = true
	s.events = append(s.events, ev)
}

func (s *recordingSink) Forget(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgot = append(s.forgot, uid)
}

func (s *recordingSink) types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		out = append(out, e.Type)
	}
	return out
}

func TestProcessorSucceededJob(t *testing.T) {
	sink := newSink()
	p := NewJobProcessor("prod", sink, NewTracker(), fixedNow, nopLog)
	job := failedJob("", "")
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	job.Status.CompletionTime = ptr(metav1.NewTime(t0.Add(53 * time.Second)))
	p.OnJob(job)
	p.OnJob(job) // update / resync: no duplicates
	if got := sink.types(); len(got) != 2 || got[0] != api.EventJobStarted || got[1] != api.EventJobSucceeded {
		t.Fatalf("types = %v", got)
	}
	ev := sink.events[1]
	if ev.DurationMs == nil || *ev.DurationMs != 53000 || ev.CronJobName != "backup" || ev.ClusterName != "prod" || ev.JobUID != "job-uid" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestProcessorIgnoresJobsWithoutCronJobOwnerAndOldEvents(t *testing.T) {
	sink := newSink()
	p := NewJobProcessor("prod", sink, NewTracker(), fixedNow, nopLog)
	orphan := failedJob("X", "")
	orphan.OwnerReferences = nil
	p.OnJob(orphan)

	old := failedJob("BackoffLimitExceeded", "")
	old.CreationTimestamp = metav1.NewTime(t0.Add(-5 * time.Hour))
	old.Status.Conditions[0].LastTransitionTime = metav1.NewTime(t0.Add(-4 * time.Hour))
	p.OnJob(old)
	if len(sink.events) != 0 {
		t.Fatalf("unexpected events: %v", sink.types())
	}
}

func TestProcessorFailedUsesLastKnownStateAfterPodDeletion(t *testing.T) {
	tr := NewTracker()
	pod := podWith("p", terminated("backup", "OOMKilled", 137, true))
	tr.Update("default", "backup-123", "p", ExtractStates(pod))
	// the pod is gone from the cluster by now; only the tracker remembers it
	sink := newSink()
	p := NewJobProcessor("prod", sink, tr, fixedNow, nopLog)
	p.OnJob(failedJob("BackoffLimitExceeded", "Job has reached the specified backoff limit"))
	ev := sink.events[len(sink.events)-1]
	if ev.Type != api.EventJobFailed || ev.Reason != "OOMKilled" || ev.DurationMs == nil || *ev.DurationMs != 53000 {
		t.Fatalf("event = %+v", ev)
	}
	p.OnJobDeleted(failedJob("", ""))
	if tr.Len() != 0 || len(sink.forgot) != 1 || sink.forgot[0] != "job-uid" {
		t.Fatal("deleted job must be forgotten")
	}
}

func TestToAPICronJob(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "n", Name: "a", UID: "u"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *"},
	}
	got := ToAPICronJob(cj)
	if got.TimeZone != "" || got.Suspend {
		t.Fatalf("got %+v", got)
	}
	cj.Spec.TimeZone = ptr("Europe/Istanbul")
	cj.Spec.Suspend = ptr(true)
	got = ToAPICronJob(cj)
	if got.TimeZone != "Europe/Istanbul" || !got.Suspend || got.UID != "u" || got.Schedule != "0 2 * * *" {
		t.Fatalf("got %+v", got)
	}
}

func TestToAPICronJobCarriesWhyARunMayBeMissing(t *testing.T) {
	last := metav1.NewTime(t0)
	done := metav1.NewTime(t0.Add(-24 * time.Hour))
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "n", Name: "a", UID: "u"},
		Spec: batchv1.CronJobSpec{
			Schedule:                "0 2 * * *",
			ConcurrencyPolicy:       batchv1.ForbidConcurrent,
			StartingDeadlineSeconds: ptr(int64(120)),
		},
		Status: batchv1.CronJobStatus{
			LastScheduleTime:   &last,
			LastSuccessfulTime: &done,
			Active:             []corev1.ObjectReference{{Name: "a-1"}, {Name: "a-2"}},
		},
	}

	got := ToAPICronJob(cj)

	if got.ConcurrencyPolicy != "Forbid" || got.StartingDeadlineSeconds != 120 || got.ActiveJobs != 2 {
		t.Fatalf("spec or active jobs missing: %+v", got)
	}
	if got.LastScheduleTime != t0.UTC().Format(time.RFC3339) ||
		got.LastSuccessfulTime != t0.Add(-24*time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("times missing: %+v", got)
	}
}

func TestToAPICronJobLeavesOutWhatIsNotSet(t *testing.T) {
	got := ToAPICronJob(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "n", Name: "a", UID: "u"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *"},
	})

	if got.LastScheduleTime != "" || got.LastSuccessfulTime != "" || got.ActiveJobs != 0 ||
		got.StartingDeadlineSeconds != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestOnlyTheStableViewOfACronJobDecidesWhetherASyncIsNeeded(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "n", Name: "a", UID: "u"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *", ConcurrencyPolicy: batchv1.ForbidConcurrent},
	}
	before := ToAPICronJob(cj)
	last := metav1.NewTime(t0)
	cj.Status.LastScheduleTime = &last
	cj.Status.Active = []corev1.ObjectReference{{Name: "a-1"}}
	after := ToAPICronJob(cj)

	if before == after {
		t.Fatal("the payload must carry the new status")
	}
	if before.Stable() != after.Stable() {
		t.Fatal("a run starting is not a reason to sync")
	}
	cj.Spec.ConcurrencyPolicy = batchv1.AllowConcurrent
	if ToAPICronJob(cj).Stable() == after.Stable() {
		t.Fatal("a changed concurrency policy is a reason to sync")
	}
}

// End to end with informers on a fake clientset: the pod is deleted before the
// Job fails, the reason must still come from the last known pod state.
func TestWatcherWithFakeClientset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	running := failedJob("", "")
	running.Status = batchv1.JobStatus{StartTime: ptr(metav1.NewTime(t0))}
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backup", UID: "cj-uid"},
		Spec:       batchv1.CronJobSpec{Schedule: "* * * * *"},
	}
	client := fake.NewSimpleClientset(cj, running, podWith("p", terminated("backup", "OOMKilled", 137, true)))

	sink := newSink()
	tr := NewTracker()
	w := NewWatcher(client, nil, NewJobProcessor("prod", sink, tr, fixedNow, nopLog), tr, nopLog)
	if err := w.Start(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); w.Stop() }()
	if got := w.CronJobs(); len(got) != 1 || got[0].Name != "backup" {
		t.Fatalf("cronjobs = %+v", got)
	}

	waitFor(t, func() bool { return len(sink.types()) == 1 }) // job_started from the initial list
	if err := client.CoreV1().Pods("default").Delete(ctx, "p", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	failed := failedJob("BackoffLimitExceeded", "limit")
	if _, err := client.BatchV1().Jobs("default").Update(ctx, failed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(sink.types()) == 2 })
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if ev := sink.events[1]; ev.Type != api.EventJobFailed || ev.Reason != "OOMKilled" {
		t.Fatalf("event = %+v", ev)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

type api2Event = api.Event

type collectSink struct{ events *[]api.Event }

func (c *collectSink) Emit(ev api.Event) { *c.events = append(*c.events, ev) }
func (c *collectSink) Forget(string)     {}
