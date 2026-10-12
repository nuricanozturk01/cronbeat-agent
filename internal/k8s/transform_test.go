package k8s

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func bigPod() *corev1.Pod {
	containers := make([]corev1.Container, 0, 6)
	for range 6 {
		containers = append(containers, corev1.Container{
			Name: "app", Image: "registry.example.test/team/app:1.2.3", Command: []string{"/bin/run", "--flag"},
			Env:       []corev1.EnvVar{{Name: "A", Value: "b"}, {Name: "C", Value: "d"}},
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
		})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "backup-1-abc", UID: "pod-uid", ResourceVersion: "42",
			Labels: map[string]string{
				"batch.kubernetes.io/job-name": "backup-1", "job-name": "backup-1",
				"app": "backup", "team": "platform", "controller-uid": "x",
			},
			Annotations:   map[string]string{"big": "annotation-" + string(make([]byte, 600))},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kube-controller-manager", Operation: "Update"}},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Job", Name: "backup-1", APIVersion: "batch/v1", UID: "job-uid"},
			},
		},
		Spec: corev1.PodSpec{Containers: containers, NodeName: "node-1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Image: "registry.example.test/team/app:1.2.3", ContainerID: "containerd://abcdef",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", ExitCode: 137, Message: "long message " + string(make([]byte, 300)),
				}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
			}},
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  "init",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pulling"}},
			}},
		},
	}
}

func TestTrimPodKeepsEverythingTheAgentReadsAndDropsTheRest(t *testing.T) {
	pod := bigPod()
	want := ExtractStates(pod)

	got := TrimPod(pod).(*corev1.Pod)

	if gotStates := ExtractStates(got); len(gotStates) != len(want) {
		t.Fatalf("states differ: got %+v want %+v", gotStates, want)
	} else {
		for i := range want {
			if gotStates[i] != want[i] {
				t.Fatalf("state %d: got %+v want %+v", i, gotStates[i], want[i])
			}
		}
	}
	if PodJobName(got) != "backup-1" {
		t.Fatalf("job name = %q", PodJobName(got))
	}
	if got.Namespace != "default" || got.Name != "backup-1-abc" || got.ResourceVersion != "42" || got.UID != "pod-uid" {
		t.Fatalf("identity lost: %+v", got.ObjectMeta)
	}
	if len(got.Spec.Containers) != 0 || got.Annotations != nil || got.ManagedFields != nil {
		t.Fatalf("heavy fields kept: %+v", got)
	}
	if _, kept := got.Labels["team"]; kept {
		t.Fatal("unrelated labels were kept")
	}
	if got.Size() >= pod.Size()/4 {
		t.Fatalf("pod is not much smaller: %d -> %d bytes", pod.Size(), got.Size())
	}
}

func TestTrimPodKeepsTheDeletionTimestampAndOwnerFallback(t *testing.T) {
	now := metav1.NewTime(t0)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "p", DeletionTimestamp: &now,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "owner-job"}},
	}}

	got := TrimPod(pod).(*corev1.Pod)

	if got.DeletionTimestamp == nil {
		t.Fatal("a terminating pod must stay recognisable")
	}
	if PodJobName(got) != "owner-job" {
		t.Fatalf("owner reference fallback lost: %q", PodJobName(got))
	}
}

func bigJob() *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "backup-123", UID: "job-uid", ResourceVersion: "7",
			CreationTimestamp: metav1.NewTime(t0),
			Labels:            map[string]string{"a": "b"},
			Annotations:       map[string]string{"x": string(make([]byte, 800))},
			ManagedFields:     []metav1.ManagedFieldsEntry{{Manager: "m"}},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "CronJob", Name: "backup", APIVersion: "batch/v1", UID: "cj-uid"},
			},
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: bigPod().Spec}},
		Status: batchv1.JobStatus{
			StartTime: ptr(metav1.NewTime(t0)), CompletionTime: ptr(metav1.NewTime(t0.Add(time.Minute))),
			Succeeded: 1,
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
				Message: "limit", LastTransitionTime: metav1.NewTime(t0.Add(53 * time.Second)),
				LastProbeTime: metav1.NewTime(t0),
			}},
		},
	}
}

func TestTrimJobKeepsWhatTheProcessorReads(t *testing.T) {
	job := bigJob()
	var base []api2Event
	p := NewJobProcessor("prod", &collectSink{events: &base}, NewTracker(), fixedNow, nopLog)
	p.OnJob(job)
	want := append([]api2Event(nil), base...)
	base = base[:0]

	got := TrimJob(job).(*batchv1.Job)
	p.OnJob(got)

	if len(base) != len(want) {
		t.Fatalf("events: got %d want %d", len(base), len(want))
	}
	for i := range want {
		g, w := base[i], want[i]
		if g.Type != w.Type || g.JobUID != w.JobUID || !g.OccurredAt.Equal(w.OccurredAt) || g.Reason != w.Reason || g.Message != w.Message {
			t.Fatalf("event %d: got %+v want %+v", i, g, w)
		}
	}
	if cj, ok := OwnerCronJob(got); !ok || cj != "backup" {
		t.Fatalf("owner = %q %v", cj, ok)
	}
	if len(got.Spec.Template.Spec.Containers) != 0 || got.Annotations != nil || got.ManagedFields != nil {
		t.Fatal("heavy fields kept")
	}
	if got.Size() >= job.Size()/4 {
		t.Fatalf("job is not much smaller: %d -> %d bytes", job.Size(), got.Size())
	}
}

func TestTrimCronJobKeepsTheSyncFields(t *testing.T) {
	tz := "Europe/Istanbul"
	suspend := true
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "backup", UID: "cj-uid", ResourceVersion: "9",
			Annotations: map[string]string{"x": string(make([]byte, 800))},
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 2 * * *", TimeZone: &tz, Suspend: &suspend,
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: bigPod().Spec}}},
		},
		Status: batchv1.CronJobStatus{
			LastScheduleTime:   ptr(metav1.NewTime(t0)),
			LastSuccessfulTime: ptr(metav1.NewTime(t0.Add(-time.Hour))),
			Active:             []corev1.ObjectReference{{Name: "backup-1", Namespace: "default", UID: "job-uid"}},
		},
	}
	cj.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	cj.Spec.StartingDeadlineSeconds = ptr(int64(90))

	got := TrimCronJob(cj).(*batchv1.CronJob)

	if ToAPICronJob(got) != ToAPICronJob(cj) {
		t.Fatalf("sync payload changed: %+v vs %+v", ToAPICronJob(got), ToAPICronJob(cj))
	}
	if got.ResourceVersion != "9" {
		t.Fatalf("resource version lost: %q", got.ResourceVersion)
	}
	if len(got.Spec.JobTemplate.Spec.Template.Spec.Containers) != 0 || got.Annotations != nil {
		t.Fatal("heavy fields kept")
	}
	if got.Size() >= cj.Size()/4 {
		t.Fatalf("cronjob is not much smaller: %d -> %d bytes", cj.Size(), got.Size())
	}
}

func TestTrimLeavesOtherObjectsAndTombstonesAlone(t *testing.T) {
	tomb := cache.DeletedFinalStateUnknown{Key: "default/x", Obj: bigJob()}

	for name, trim := range map[string]func(any) any{"pod": TrimPod, "job": TrimJob, "cronjob": TrimCronJob} {
		if got := trim(tomb); got.(cache.DeletedFinalStateUnknown).Key != "default/x" {
			t.Errorf("%s: tombstone changed", name)
		}
		if got := trim("not a kubernetes object"); got != "not a kubernetes object" {
			t.Errorf("%s: unknown object changed", name)
		}
	}
}

func TestTrimPodKeepsOnlyTheMemoryLimitOfContainersAndTheSchedulerMessage(t *testing.T) {
	pod := bigPod()
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("256Mi"), corev1.ResourceCPU: resource.MustParse("500m"),
	}
	pod.Status.Reason, pod.Status.Message = "Evicted", "The node was low on resource: memory."
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available"},
		{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady", Message: "not needed"},
	}
	want := ExtractStates(pod)

	got := TrimPod(pod).(*corev1.Pod)

	gotStates := ExtractStates(got)
	if len(gotStates) != len(want) {
		t.Fatalf("states differ: got %+v want %+v", gotStates, want)
	}
	for i := range want {
		if gotStates[i] != want[i] {
			t.Fatalf("state %d: got %+v want %+v", i, gotStates[i], want[i])
		}
	}
	if len(got.Spec.Containers) != 1 || got.Spec.Containers[0].Image != "" || len(got.Spec.Containers[0].Env) != 0 {
		t.Fatalf("only the name and memory limit may stay: %+v", got.Spec.Containers)
	}
	if _, cpu := got.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]; cpu {
		t.Fatal("the cpu limit is not needed")
	}
	if len(got.Status.Conditions) != 1 || got.Status.Conditions[0].Type != corev1.PodScheduled {
		t.Fatalf("only PodScheduled may stay: %+v", got.Status.Conditions)
	}
}

func TestTrimCronJobKeepsOnlyCronBeatAnnotations(t *testing.T) {
	cj := cronJob("backup", "* * * * *")
	cj.Annotations = map[string]string{
		"cronbeat.dev/grace": "10m", "cronbeat.dev/ignore": "false",
		"kubectl.kubernetes.io/last-applied-configuration": string(make([]byte, 2000)), "team": "platform",
	}

	got := TrimCronJob(cj).(*batchv1.CronJob)

	if len(got.Annotations) != 2 || got.Annotations["cronbeat.dev/grace"] != "10m" {
		t.Fatalf("annotations = %+v", got.Annotations)
	}
	if ToAPICronJob(got) != ToAPICronJob(cj) {
		t.Fatal("trimming must not change the sync payload")
	}
}
