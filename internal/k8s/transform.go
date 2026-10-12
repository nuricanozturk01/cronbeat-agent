package k8s

import (
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// The informers keep every object in memory. A Pod or a CronJob carries a full
// pod template (containers, env, volumes) and managed fields that the agent
// never reads, which is most of its size. These functions run before an object
// enters the informer cache and keep only what the agent uses: the identity,
// the resource version the informer needs, and the few fields read by the
// processor, the failure inference and the sync payload. Anything else
// (including tombstones) is passed through untouched.

func asTransform(trim func(any) any) cache.TransformFunc {
	return func(obj any) (any, error) { return trim(obj), nil }
}

// TrimCronJob keeps the fields of the sync payload.
func TrimCronJob(obj any) any {
	cj, ok := obj.(*batchv1.CronJob)
	if !ok {
		return obj
	}
	meta := identity(cj.ObjectMeta)
	for key, value := range cj.Annotations {
		if strings.HasPrefix(key, annotationPrefix) {
			if meta.Annotations == nil {
				meta.Annotations = map[string]string{}
			}
			meta.Annotations[key] = value
		}
	}
	return &batchv1.CronJob{
		ObjectMeta: meta,
		Spec: batchv1.CronJobSpec{
			Schedule: cj.Spec.Schedule,
			TimeZone: cj.Spec.TimeZone,
			Suspend:  cj.Spec.Suspend,

			ConcurrencyPolicy:       cj.Spec.ConcurrencyPolicy,
			StartingDeadlineSeconds: cj.Spec.StartingDeadlineSeconds,
		},
		Status: batchv1.CronJobStatus{
			LastScheduleTime:   cj.Status.LastScheduleTime,
			LastSuccessfulTime: cj.Status.LastSuccessfulTime,
			Active:             make([]corev1.ObjectReference, len(cj.Status.Active)),
		},
	}
}

// TrimJob keeps what OnJob and InferFailure read.
func TrimJob(obj any) any {
	job, ok := obj.(*batchv1.Job)
	if !ok {
		return obj
	}
	meta := identity(job.ObjectMeta)
	meta.CreationTimestamp = job.CreationTimestamp
	meta.DeletionTimestamp = job.DeletionTimestamp
	meta.OwnerReferences = ownerRefs(job.OwnerReferences)
	conditions := make([]batchv1.JobCondition, 0, len(job.Status.Conditions))
	for _, c := range job.Status.Conditions {
		conditions = append(conditions, batchv1.JobCondition{
			Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message,
			LastTransitionTime: c.LastTransitionTime,
		})
	}
	return &batchv1.Job{
		ObjectMeta: meta,
		Status: batchv1.JobStatus{
			StartTime:      job.Status.StartTime,
			CompletionTime: job.Status.CompletionTime,
			Conditions:     conditions,
		},
	}
}

// TrimPod keeps what PodJobName and ExtractStates read.
func TrimPod(obj any) any {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj
	}
	meta := identity(pod.ObjectMeta)
	meta.DeletionTimestamp = pod.DeletionTimestamp
	meta.OwnerReferences = ownerRefs(pod.OwnerReferences)
	for _, key := range []string{labelJobName, labelJobNameLegacy} {
		if v, found := pod.Labels[key]; found {
			if meta.Labels == nil {
				meta.Labels = map[string]string{}
			}
			meta.Labels[key] = v
		}
	}
	return &corev1.Pod{
		ObjectMeta: meta,
		Spec: corev1.PodSpec{
			InitContainers: trimContainers(pod.Spec.InitContainers),
			Containers:     trimContainers(pod.Spec.Containers),
		},
		Status: corev1.PodStatus{
			Reason:                pod.Status.Reason,
			Message:               pod.Status.Message,
			Conditions:            trimConditions(pod.Status.Conditions),
			InitContainerStatuses: trimStatuses(pod.Status.InitContainerStatuses),
			ContainerStatuses:     trimStatuses(pod.Status.ContainerStatuses),
		},
	}
}

// trimContainers keeps the name and the memory limit, which tells how much
// memory an OOMKilled container had. Image, command and environment are dropped.
func trimContainers(in []corev1.Container) []corev1.Container {
	var out []corev1.Container
	for i := range in {
		q, ok := in[i].Resources.Limits[corev1.ResourceMemory]
		if !ok {
			continue
		}
		out = append(out, corev1.Container{
			Name:      in[i].Name,
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: q}},
		})
	}
	return out
}

// trimConditions keeps the PodScheduled condition, where the scheduler says why
// a pod could not be placed.
func trimConditions(in []corev1.PodCondition) []corev1.PodCondition {
	var out []corev1.PodCondition
	for _, c := range in {
		if c.Type == corev1.PodScheduled {
			out = append(out, corev1.PodCondition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message})
		}
	}
	return out
}

func identity(m metav1.ObjectMeta) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Namespace: m.Namespace, Name: m.Name, UID: m.UID, ResourceVersion: m.ResourceVersion,
	}
}

func ownerRefs(refs []metav1.OwnerReference) []metav1.OwnerReference {
	if len(refs) == 0 {
		return nil
	}
	out := make([]metav1.OwnerReference, 0, len(refs))
	for _, r := range refs {
		out = append(out, metav1.OwnerReference{Kind: r.Kind, Name: r.Name})
	}
	return out
}

func trimStatuses(in []corev1.ContainerStatus) []corev1.ContainerStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.ContainerStatus, 0, len(in))
	for i := range in {
		out = append(out, corev1.ContainerStatus{
			Name:                 in[i].Name,
			State:                trimState(in[i].State),
			LastTerminationState: trimState(in[i].LastTerminationState),
		})
	}
	return out
}

func trimState(s corev1.ContainerState) corev1.ContainerState {
	var out corev1.ContainerState
	if t := s.Terminated; t != nil {
		out.Terminated = &corev1.ContainerStateTerminated{Reason: t.Reason, ExitCode: t.ExitCode}
	}
	if w := s.Waiting; w != nil {
		out.Waiting = &corev1.ContainerStateWaiting{Reason: w.Reason, Message: w.Message}
	}
	return out
}
