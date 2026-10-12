package k8s

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func cronJob(name, schedule string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: "uid-" + "cj-" + "x"},
		Spec:       batchv1.CronJobSpec{Schedule: schedule},
	}
}

func startWatcher(t *testing.T, client *fake.Clientset, sink Sink, known KnownFunc, tracker *Tracker) (*Watcher, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	proc := NewJobProcessor("prod", sink, tracker, fixedNow, nopLog)
	proc.SetKnown(known)
	w := NewWatcher(client, nil, proc, tracker, nopLog)
	if err := w.Start(ctx, 10*time.Second); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); w.Stop() })
	return w, cancel
}

func drained(ch <-chan struct{}) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		case <-time.After(150 * time.Millisecond):
			return n
		}
	}
}

func TestWatcherOnlyTracksPodsThatBelongToJobs(t *testing.T) {
	jobPod := podWith("job-pod", terminated("app", "OOMKilled", 137, true))
	plain := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{terminated("web", "Error", 1, false)}},
	}
	client := fake.NewSimpleClientset(jobPod, plain)
	tr := NewTracker()

	startWatcher(t, client, newSink(), nil, tr)

	waitFor(t, func() bool { return len(tr.States("default", "backup-123")) == 1 })
	if tr.Len() != 1 {
		t.Fatalf("tracked jobs = %d, only the Job pod may be tracked", tr.Len())
	}
}

func TestWatcherCronJobChangesNotifyOnlyWhenTheSyncPayloadChanges(t *testing.T) {
	client := fake.NewSimpleClientset(cronJob("backup", "0 2 * * *"))
	w, _ := startWatcher(t, client, newSink(), nil, NewTracker())
	ctx := context.Background()

	if n := drained(w.CronJobChanges()); n != 0 {
		t.Fatalf("the initial list must not notify, got %d", n)
	}

	status := cronJob("backup", "0 2 * * *")
	status.Status.LastScheduleTime = ptr(metav1.NewTime(t0))
	status.Annotations = map[string]string{"touched": "yes"}
	if _, err := client.BatchV1().CronJobs("default").Update(ctx, status, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := drained(w.CronJobChanges()); n != 0 {
		t.Fatalf("a status or annotation change must not notify, got %d", n)
	}

	if _, err := client.BatchV1().CronJobs("default").Create(ctx, cronJob("new", "* * * * *"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := drained(w.CronJobChanges()); n != 1 {
		t.Fatalf("a new CronJob must notify once, got %d", n)
	}

	changed := cronJob("backup", "30 3 * * *")
	if _, err := client.BatchV1().CronJobs("default").Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := drained(w.CronJobChanges()); n != 1 {
		t.Fatalf("a schedule change must notify, got %d", n)
	}

	if err := client.BatchV1().CronJobs("default").Delete(ctx, "new", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := drained(w.CronJobChanges()); n != 1 {
		t.Fatalf("a deleted CronJob must notify, got %d", n)
	}
}

func TestWatcherKeepsOnlyOnePendingSignalWhileNobodyListensAndNeverBlocks(t *testing.T) {
	client := fake.NewSimpleClientset()
	w, _ := startWatcher(t, client, newSink(), nil, NewTracker())

	for i := range 20 {
		name := "job-" + string(rune('a'+i))
		if _, err := client.BatchV1().CronJobs("default").Create(context.Background(), cronJob(name, "* * * * *"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return w.CronJobCount() == 20 })

	if n := drained(w.CronJobChanges()); n != 1 {
		t.Fatalf("pending signals = %d, a burst must leave exactly one", n)
	}
}

func TestWatcherCountsCronJobsWithoutBuildingThePayload(t *testing.T) {
	client := fake.NewSimpleClientset(cronJob("a", "* * * * *"), cronJob("b", "* * * * *"), cronJob("c", "* * * * *"))
	w, _ := startWatcher(t, client, newSink(), nil, NewTracker())

	if got := w.CronJobCount(); got != 3 {
		t.Fatalf("count = %d", got)
	}
}

func TestReplayJobsSendsWhatWasHeldBackBecauseTheCronJobWasUnknown(t *testing.T) {
	job := failedJob("", "")
	job.Status = batchv1.JobStatus{StartTime: ptr(metav1.NewTime(t0))}
	client := fake.NewSimpleClientset(cronJob("backup", "* * * * *"), job)
	sink := newSink()
	var known atomic.Bool

	w, _ := startWatcher(t, client, sink, func(string, string) bool { return known.Load() }, NewTracker())

	time.Sleep(300 * time.Millisecond)
	if got := sink.types(); len(got) != 0 {
		t.Fatalf("events for an unknown CronJob must be held back, got %v", got)
	}

	known.Store(true)
	w.ReplayJobs()

	if got := sink.types(); len(got) != 1 || got[0] != api.EventJobStarted {
		t.Fatalf("after the CronJob became known the job must be reported once, got %v", got)
	}
	w.ReplayJobs()
	if got := sink.types(); len(got) != 1 {
		t.Fatalf("replaying again must not duplicate, got %v", got)
	}
}

func TestProcessorHoldsBackJobsOfUnknownCronJobsWithoutRememberingThem(t *testing.T) {
	sink := newSink()
	p := NewJobProcessor("prod", sink, NewTracker(), fixedNow, nopLog)
	known := map[string]bool{}
	p.SetKnown(func(ns, name string) bool { return known[ns+"/"+name] })
	job := failedJob("", "")
	job.Status = batchv1.JobStatus{StartTime: ptr(metav1.NewTime(t0))}

	p.OnJob(job)
	if len(sink.types()) != 0 {
		t.Fatal("an unknown CronJob must not produce events")
	}

	known["default/backup"] = true
	p.OnJob(job)
	if got := sink.types(); len(got) != 1 {
		t.Fatalf("a known CronJob must produce events, got %v", got)
	}
}

func inNamespace(ns string, cj *batchv1.CronJob) *batchv1.CronJob {
	cj.Namespace = ns
	return cj
}

func TestWatcherWithNamespacesIgnoresEverythingOutsideThem(t *testing.T) {
	job := failedJob("", "")
	job.Namespace = "team-b"
	job.Status = batchv1.JobStatus{StartTime: ptr(metav1.NewTime(t0))}
	client := fake.NewSimpleClientset(
		inNamespace("team-a", cronJob("backup", "* * * * *")),
		inNamespace("team-b", cronJob("backup", "* * * * *")),
		inNamespace("team-c", cronJob("report", "* * * * *")),
		job)
	sink := newSink()
	tracker := NewTracker()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proc := NewJobProcessor("prod", sink, tracker, fixedNow, nopLog)
	w := NewWatcher(client, []string{"team-a", "team-c"}, proc, tracker, nopLog)
	if err := w.Start(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)

	var seen []string
	for _, c := range w.CronJobs() {
		seen = append(seen, c.Namespace+"/"+c.Name)
	}
	if len(seen) != 2 || w.CronJobCount() != 2 {
		t.Fatalf("only the watched namespaces may be reported, got %v", seen)
	}
	for _, s := range seen {
		if s != "team-a/backup" && s != "team-c/report" {
			t.Fatalf("unexpected CronJob %s", s)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := sink.types(); len(got) != 0 {
		t.Fatalf("a Job outside the watched namespaces must not produce events, got %v", got)
	}
}
