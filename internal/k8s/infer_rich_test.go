package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func withMemoryLimit(pod *corev1.Pod, container, limit string) *corev1.Pod {
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: container,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)},
		},
	})
	return pod
}

func waitingWith(name, reason, message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message},
	}}
}

func failureOf(t *testing.T, pods ...*corev1.Pod) FailureInfo {
	t.Helper()
	var states []ContainerState
	for _, p := range pods {
		states = append(states, ExtractStates(p)...)
	}
	return InferFailure(failedJob("BackoffLimitExceeded", ""), states)
}

func TestOOMKilledNamesTheMemoryLimitWhenTheContainerHasOne(t *testing.T) {
	pod := withMemoryLimit(podWith("p", terminated("backup", "OOMKilled", 137, true)), "backup", "256Mi")

	info := failureOf(t, pod)

	if info.Reason != "OOMKilled" {
		t.Fatalf("reason = %q", info.Reason)
	}
	if !strings.Contains(info.Message, "container backup terminated: OOMKilled (exit code 137, memory limit 256Mi)") {
		t.Fatalf("message = %q", info.Message)
	}
}

func TestOOMKilledWithoutALimitKeepsTheShortMessage(t *testing.T) {
	info := failureOf(t, podWith("p", terminated("backup", "OOMKilled", 137, true)))

	if !strings.Contains(info.Message, "container backup terminated: OOMKilled (exit code 137);") {
		t.Fatalf("message = %q", info.Message)
	}
}

func TestExitCodesThatHaveAWellKnownMeaningAreExplained(t *testing.T) {
	tests := []struct {
		code int32
		want string
	}{
		{126, "exit code 126, the command could not be executed"},
		{127, "exit code 127, command not found"},
		{137, "exit code 137, killed with SIGKILL"},
		{139, "exit code 139, segmentation fault (SIGSEGV)"},
		{143, "exit code 143, stopped with SIGTERM"},
		{1, "exit code 1)"},
		{2, "exit code 2)"},
	}
	for _, tc := range tests {
		info := failureOf(t, podWith("p", terminated("app", "Error", tc.code, false)))
		if !strings.Contains(info.Message, tc.want) {
			t.Errorf("code %d: message %q does not contain %q", tc.code, info.Message, tc.want)
		}
	}
}

func TestWaitingContainersShowWhatKubernetesSays(t *testing.T) {
	info := failureOf(t, podWith("p", waitingWith("app", "CreateContainerConfigError", `secret "db-credentials" not found`)))

	if info.Reason != "CreateContainerConfigError" {
		t.Fatalf("reason = %q", info.Reason)
	}
	if !strings.Contains(info.Message, `container app waiting: CreateContainerConfigError (secret "db-credentials" not found)`) {
		t.Fatalf("message = %q", info.Message)
	}
}

func TestAWaitingMessageIsCutBeforeItGrows(t *testing.T) {
	info := failureOf(t, podWith("p", waitingWith("app", "ImagePullBackOff", strings.Repeat("x", 1000))))

	if len([]rune(info.Message)) > maxMessageLen {
		t.Fatalf("message has %d characters", len([]rune(info.Message)))
	}
	if !strings.Contains(info.Message, "ImagePullBackOff (") {
		t.Fatalf("message = %q", info.Message)
	}
}

func TestAPodThatCouldNotBeScheduledIsReportedWithTheSchedulerMessage(t *testing.T) {
	pod := podWith("p")
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message: "0/3 nodes are available: 3 Insufficient memory.",
	}}

	info := failureOf(t, pod)

	if info.Reason != "Unschedulable" {
		t.Fatalf("reason = %q", info.Reason)
	}
	if !strings.Contains(info.Message, "pod unschedulable: 0/3 nodes are available: 3 Insufficient memory.") {
		t.Fatalf("message = %q", info.Message)
	}
}

func TestAnEvictedPodIsReportedWithTheNodeReason(t *testing.T) {
	pod := podWith("p")
	pod.Status.Reason = "Evicted"
	pod.Status.Message = "The node was low on resource: memory."

	info := failureOf(t, pod)

	if info.Reason != "Evicted" || !strings.Contains(info.Message, "pod evicted: The node was low on resource: memory.") {
		t.Fatalf("info = %+v", info)
	}
}

func TestAContainerProblemBeatsAPodLevelOneAndOOMBeatsEverything(t *testing.T) {
	sched := podWith("a")
	sched.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "no nodes",
	}}
	crash := podWith("b", terminated("app", "Error", 1, false))
	oom := podWith("c", terminated("job", "OOMKilled", 137, false))

	if got := failureOf(t, sched, crash).Reason; got != "Error" {
		t.Fatalf("a crashing container is more specific than an unschedulable pod, got %q", got)
	}
	if got := failureOf(t, sched, crash, oom).Reason; got != "OOMKilled" {
		t.Fatalf("OOMKilled must win, got %q", got)
	}
}
