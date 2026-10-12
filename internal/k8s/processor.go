package k8s

import (
	"log/slog"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultMaxEventAge is how old an event may be and still be sent. It stops an
// agent restart from replaying the whole Job history of the cluster.
const DefaultMaxEventAge = time.Hour

// Sink receives events. Emit must not block and must be idempotent per
// (jobUid, type); Forget releases per-job bookkeeping.
type Sink interface {
	Emit(ev api.Event)
	Forget(jobUID string)
}

// KnownFunc tells whether the backend knows a CronJob (it has a monitor). The
// backend ignores events of CronJobs it does not know, so they are held back
// until a sync reports the CronJob and the jobs are replayed.
type KnownFunc func(namespace, cronJob string) bool

// JobProcessor converts Job state into events.
type JobProcessor struct {
	cluster  string
	sink     Sink
	tracker  *Tracker
	now      func() time.Time
	maxAge   time.Duration
	log      *slog.Logger
	failures func(*batchv1.Job, []ContainerState) FailureInfo
	known    KnownFunc
}

// NewJobProcessor builds a processor. now is the clock (time.Now in production).
func NewJobProcessor(cluster string, sink Sink, tracker *Tracker, now func() time.Time, log *slog.Logger) *JobProcessor {
	return &JobProcessor{
		cluster: cluster, sink: sink, tracker: tracker, now: now,
		maxAge: DefaultMaxEventAge, log: log, failures: InferFailure,
	}
}

// SetKnown sets the gate for unknown CronJobs. Without it every CronJob counts
// as known. Call it before the processor is used.
func (p *JobProcessor) SetKnown(known KnownFunc) { p.known = known }

// OwnerCronJob returns the name of the CronJob owning the Job, if any.
func OwnerCronJob(job *batchv1.Job) (string, bool) {
	for _, ref := range job.OwnerReferences {
		if ref.Kind == "CronJob" {
			return ref.Name, true
		}
	}
	return "", false
}

// OnJob handles an added or updated Job. It is safe to call repeatedly for
// the same Job: the sink de-duplicates per (jobUid, type).
func (p *JobProcessor) OnJob(job *batchv1.Job) {
	cronJob, ok := OwnerCronJob(job)
	if !ok {
		return
	}
	if p.known != nil && !p.known(job.Namespace, cronJob) {
		p.log.Debug("held back until the CronJob is known to the API",
			"namespace", job.Namespace, "cronJob", cronJob)
		return
	}
	base := api.Event{
		ClusterName: p.cluster,
		Namespace:   job.Namespace,
		CronJobName: cronJob,
		JobUID:      string(job.UID),
	}

	started := base
	started.Type = api.EventJobStarted
	started.OccurredAt = job.CreationTimestamp.UTC().Truncate(time.Second)
	if started.OccurredAt.IsZero() {
		started.OccurredAt = p.now().UTC().Truncate(time.Second)
	}
	p.emit(started)

	if cond := trueCondition(job, batchv1.JobComplete); cond != nil {
		ev := base
		ev.Type = api.EventJobSucceeded
		ev.OccurredAt = p.finishTime(job, cond)
		ev.DurationMs = duration(job.Status.StartTime, ev.OccurredAt)
		p.emit(ev)
	}
	if cond := trueCondition(job, batchv1.JobFailed); cond != nil {
		ev := base
		ev.Type = api.EventJobFailed
		ev.OccurredAt = p.finishTime(job, cond)
		ev.DurationMs = duration(job.Status.StartTime, ev.OccurredAt)
		info := p.failures(job, p.tracker.States(job.Namespace, job.Name))
		ev.Reason, ev.Message = info.Reason, info.Message
		p.emit(ev)
	}
}

// OnJobDeleted releases memory held for a deleted Job.
func (p *JobProcessor) OnJobDeleted(job *batchv1.Job) {
	p.tracker.Forget(job.Namespace, job.Name)
	p.sink.Forget(string(job.UID))
}

func (p *JobProcessor) emit(ev api.Event) {
	if p.now().Sub(ev.OccurredAt) > p.maxAge {
		p.log.Debug("skipping old event", "type", ev.Type, "namespace", ev.Namespace, "cronJob", ev.CronJobName)
		return
	}
	p.sink.Emit(ev)
}

func (p *JobProcessor) finishTime(job *batchv1.Job, cond *batchv1.JobCondition) time.Time {
	if cond.Type == batchv1.JobComplete && job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.UTC().Truncate(time.Second)
	}
	if !cond.LastTransitionTime.IsZero() {
		return cond.LastTransitionTime.UTC().Truncate(time.Second)
	}
	return p.now().UTC().Truncate(time.Second)
}

func trueCondition(job *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

func duration(start *metav1.Time, end time.Time) *int64 {
	if start == nil || start.IsZero() || end.IsZero() {
		return nil
	}
	ms := end.Sub(start.Time).Milliseconds()
	if ms < 0 {
		return nil
	}
	return &ms
}
