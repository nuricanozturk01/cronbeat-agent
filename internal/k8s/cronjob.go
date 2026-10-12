package k8s

import (
	"sort"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToAPICronJob builds the sync payload entry of a CronJob. timeZone is omitted
// when empty, a missing suspend flag counts as false.
func ToAPICronJob(cj *batchv1.CronJob) api.CronJob {
	out := api.CronJob{
		Namespace: cj.Namespace,
		Name:      cj.Name,
		UID:       string(cj.UID),
		Schedule:  cj.Spec.Schedule,
	}
	if cj.Spec.TimeZone != nil {
		out.TimeZone = *cj.Spec.TimeZone
	}
	if cj.Spec.Suspend != nil {
		out.Suspend = *cj.Spec.Suspend
	}
	out.ConcurrencyPolicy = string(cj.Spec.ConcurrencyPolicy)
	if cj.Spec.StartingDeadlineSeconds != nil {
		out.StartingDeadlineSeconds = *cj.Spec.StartingDeadlineSeconds
	}
	out.LastScheduleTime = formatTime(cj.Status.LastScheduleTime)
	out.LastSuccessfulTime = formatTime(cj.Status.LastSuccessfulTime)
	out.ActiveJobs = len(cj.Status.Active)
	settings := readAnnotations(cj)
	out.GraceSeconds, out.MaxRuntimeSeconds = settings.grace, settings.maxRuntime
	out.Ignored, out.Problem = settings.ignored, settings.problem
	return out
}

func formatTime(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func sortCronJobs(list []api.CronJob) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Namespace != list[j].Namespace {
			return list[i].Namespace < list[j].Namespace
		}
		return list[i].Name < list[j].Name
	})
}
