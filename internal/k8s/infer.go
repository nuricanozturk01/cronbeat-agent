// Package k8s watches CronJobs, Jobs and Pods and turns them into CronBeat events.
package k8s

import (
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// StateKind tells how a container ended up in a bad state.
type StateKind int

const (
	// KindTerminated is a container that terminated abnormally.
	KindTerminated StateKind = iota
	// KindWaiting is a container stuck in a waiting state (ImagePullBackOff...).
	KindWaiting
	// KindPod is a problem of the pod itself: it could not be scheduled or was evicted.
	KindPod
)

const (
	reasonOOMKilled        = "OOMKilled"
	reasonBackoffLimit     = "BackoffLimitExceeded"
	maxMessageLen          = 500
	waitingContainerCreate = "ContainerCreating"
	waitingPodInit         = "PodInitializing"
	reasonUnschedulable    = "Unschedulable"
	reasonEvicted          = "Evicted"
	maxStateDetailLen      = 200
)

// ContainerState is the last known abnormal state of one container.
type ContainerState struct {
	Container string
	Kind      StateKind
	Reason    string
	ExitCode  int32
	// Detail is what Kubernetes says about the state: the kubelet's waiting
	// message or the scheduler's reason. It is cut to maxStateDetailLen.
	Detail string
	// MemoryLimit is the memory limit of an OOMKilled container, when it has one.
	MemoryLimit string
}

// Message renders the human readable description of the state.
func (s ContainerState) Message() string {
	switch s.Kind {
	case KindWaiting:
		if s.Detail != "" {
			return fmt.Sprintf("container %s waiting: %s (%s)", s.Container, s.Reason, s.Detail)
		}
		return fmt.Sprintf("container %s waiting: %s", s.Container, s.Reason)
	case KindPod:
		return fmt.Sprintf("pod %s: %s", strings.ToLower(s.Reason), s.Detail)
	}
	code := fmt.Sprintf("exit code %d", s.ExitCode)
	switch {
	case s.Reason == reasonOOMKilled && s.MemoryLimit != "":
		code += ", memory limit " + s.MemoryLimit
	case s.Reason != reasonOOMKilled:
		if hint := exitCodeHint(s.ExitCode); hint != "" {
			code += ", " + hint
		}
	}
	return fmt.Sprintf("container %s terminated: %s (%s)", s.Container, s.Reason, code)
}

// exitCodeHint explains the exit codes that shells and the kernel give a
// well known meaning. Other codes belong to the program and are not guessed.
func exitCodeHint(code int32) string {
	switch code {
	case 126:
		return "the command could not be executed"
	case 127:
		return "command not found"
	case 137:
		return "killed with SIGKILL"
	case 139:
		return "segmentation fault (SIGSEGV)"
	case 143:
		return "stopped with SIGTERM"
	}
	return ""
}

// rank orders candidates, lower is more specific.
func (s ContainerState) rank() int {
	switch {
	case s.Kind == KindTerminated && s.Reason == reasonOOMKilled:
		return 0
	case s.Kind == KindTerminated:
		return 1
	case s.Kind == KindPod:
		return 2
	default:
		return 3
	}
}

// FailureInfo is the inferred reason and message of a failed Job.
type FailureInfo struct {
	Reason  string
	Message string
}

// ExtractStates returns the abnormal container states of a pod: terminated
// containers (current or last state) with a non-zero exit code or reason
// OOMKilled/Error, and waiting containers with a failure reason.
func ExtractStates(pod *corev1.Pod) []ContainerState {
	var out []ContainerState
	limits := memoryLimits(pod)
	add := func(statuses []corev1.ContainerStatus) {
		for i := range statuses {
			cs := &statuses[i]
			if t := abnormalTerminated(cs.LastTerminationState.Terminated); t != nil {
				out = append(out, terminatedState(cs.Name, t, limits))
			} else if t := abnormalTerminated(cs.State.Terminated); t != nil {
				out = append(out, terminatedState(cs.Name, t, limits))
			}
			if w := cs.State.Waiting; w != nil && w.Reason != "" &&
				w.Reason != waitingContainerCreate && w.Reason != waitingPodInit {
				out = append(out, ContainerState{
					Container: cs.Name, Kind: KindWaiting, Reason: w.Reason,
					Detail: truncate(strings.TrimSpace(w.Message), maxStateDetailLen),
				})
			}
		}
	}
	add(pod.Status.InitContainerStatuses)
	add(pod.Status.ContainerStatuses)
	return append(out, podLevelStates(pod)...)
}

func terminatedState(name string, t *corev1.ContainerStateTerminated, limits map[string]string) ContainerState {
	return ContainerState{
		Container: name, Kind: KindTerminated, Reason: t.Reason, ExitCode: t.ExitCode,
		MemoryLimit: limits[name],
	}
}

func podLevelStates(pod *corev1.Pod) []ContainerState {
	var out []ContainerState
	if pod.Status.Reason == reasonEvicted {
		out = append(out, ContainerState{
			Kind: KindPod, Reason: reasonEvicted,
			Detail: truncate(strings.TrimSpace(pod.Status.Message), maxStateDetailLen),
		})
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			out = append(out, ContainerState{
				Kind: KindPod, Reason: reasonUnschedulable,
				Detail: truncate(strings.TrimSpace(c.Message), maxStateDetailLen),
			})
		}
	}
	return out
}

// memoryLimits maps container names to their memory limit, the only part of the
// pod spec the agent reads (to say how much memory an OOMKilled container had).
func memoryLimits(pod *corev1.Pod) map[string]string {
	var out map[string]string
	for _, list := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range list {
			if q, ok := list[i].Resources.Limits[corev1.ResourceMemory]; ok && !q.IsZero() {
				if out == nil {
					out = map[string]string{}
				}
				out[list[i].Name] = q.String()
			}
		}
	}
	return out
}

func abnormalTerminated(t *corev1.ContainerStateTerminated) *corev1.ContainerStateTerminated {
	if t == nil {
		return nil
	}
	if t.ExitCode == 0 && t.Reason != reasonOOMKilled {
		return nil
	}
	if t.Reason == "" {
		c := *t
		c.Reason = "Error"
		return &c
	}
	return t
}

// InferFailure applies PLAN 12.2. The Failed condition reason wins, except
// for the generic BackoffLimitExceeded wrapper: when container states are
// known, the more specific container reason (OOMKilled, ImagePullBackOff...)
// is reported instead (see README, "How failure reasons are inferred").
// states are the last known container states of the job's pods, most recent
// pod first.
func InferFailure(job *batchv1.Job, states []ContainerState) FailureInfo {
	var condReason, condMessage string
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			condReason, condMessage = c.Reason, c.Message
			break
		}
	}
	best := pickState(states)

	info := FailureInfo{Reason: condReason}
	var parts []string
	if best != nil {
		parts = append(parts, best.Message())
		if condReason == "" || condReason == reasonBackoffLimit {
			info.Reason = best.Reason
		}
	}
	if condMessage != "" {
		parts = append(parts, condMessage)
	} else if condReason != "" && (best == nil || info.Reason != condReason) {
		parts = append(parts, "job failed: "+condReason)
	}
	info.Message = truncate(strings.Join(parts, "; "), maxMessageLen)
	return info
}

func pickState(states []ContainerState) *ContainerState {
	if len(states) == 0 {
		return nil
	}
	sorted := make([]ContainerState, len(states))
	copy(sorted, states)
	// stable: equal ranks keep the "most recent pod first" order
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].rank() < sorted[j].rank() })
	return &sorted[0]
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
