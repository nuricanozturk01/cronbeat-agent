// Package api implements the CronBeat agent API client (PLAN 8.3).
package api

import "time"

// Limits of the CronBeat API (the backend answers 400 above them).
const (
	// MaxCronJobs is the most CronJobs one sync may carry.
	MaxCronJobs = 5000
	// MaxTextLength is the longest namespace, name, uid, schedule or time zone.
	MaxTextLength = 253
)

// Event types accepted by POST /agent/v1/events.
const (
	EventJobStarted   = "job_started"
	EventJobSucceeded = "job_succeeded"
	EventJobFailed    = "job_failed"
)

// CronJob is one entry of the full list sent to POST /agent/v1/sync.
type CronJob struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Schedule  string `json:"schedule"`
	TimeZone  string `json:"timeZone,omitempty"`
	Suspend   bool   `json:"suspend"`
	// GraceSeconds and MaxRuntimeSeconds come from the cronbeat.dev/grace and
	// cronbeat.dev/max-runtime annotations, 0 means "not set".
	GraceSeconds      int `json:"graceSeconds,omitempty"`
	MaxRuntimeSeconds int `json:"maxRuntimeSeconds,omitempty"`
	// What the CronJob itself says about why a run may not have come: its
	// concurrency policy and starting deadline, when it last scheduled and
	// succeeded (RFC 3339 UTC) and how many Jobs it has running.
	ConcurrencyPolicy       string `json:"concurrencyPolicy,omitempty"`
	StartingDeadlineSeconds int64  `json:"startingDeadlineSeconds,omitempty"`
	LastScheduleTime        string `json:"lastScheduleTime,omitempty"`
	LastSuccessfulTime      string `json:"lastSuccessfulTime,omitempty"`
	ActiveJobs              int    `json:"activeJobs,omitempty"`
	// Ignored (cronbeat.dev/ignore) keeps the CronJob out of the sync. Problem
	// describes an annotation that was left out because its value is invalid.
	// Neither goes over the wire.
	Ignored bool   `json:"-"`
	Problem string `json:"-"`
}

// Stable is the entry without what changes with every run (last schedule and
// success time, active Jobs). A change in what Stable returns is a reason to
// sync at once; the rest travels with the next periodic sync.
func (c CronJob) Stable() CronJob {
	c.LastScheduleTime, c.LastSuccessfulTime, c.ActiveJobs = "", "", 0
	return c
}

// Valid reports whether the API accepts this entry. One rejected entry would
// make the API refuse the whole sync, so invalid ones are left out.
func (c CronJob) Valid() bool {
	return c.Namespace != "" && c.Name != "" && c.Schedule != "" &&
		len(c.Namespace) <= MaxTextLength && len(c.Name) <= MaxTextLength &&
		len(c.UID) <= MaxTextLength && len(c.Schedule) <= MaxTextLength && len(c.TimeZone) <= MaxTextLength
}

// SyncRequest is the body of POST /agent/v1/sync.
type SyncRequest struct {
	ClusterName  string    `json:"clusterName"`
	AgentVersion string    `json:"agentVersion"`
	CronJobs     []CronJob `json:"cronJobs"`
}

// MonitorRef identifies a monitor returned by the sync endpoint.
type MonitorRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// SyncResponse is the 200 response of POST /agent/v1/sync.
type SyncResponse struct {
	Monitors []MonitorRef `json:"monitors"`
	Warnings []string     `json:"warnings"`
}

// Event is the body of POST /agent/v1/events.
type Event struct {
	ClusterName string    `json:"clusterName"`
	Namespace   string    `json:"namespace"`
	CronJobName string    `json:"cronJobName"`
	JobUID      string    `json:"jobUid"`
	Type        string    `json:"type"`
	OccurredAt  time.Time `json:"occurredAt"`
	DurationMs  *int64    `json:"durationMs,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Message     string    `json:"message,omitempty"`
}

// DedupeKey mirrors the backend dedupe key (jobUid:type).
func (e Event) DedupeKey() string {
	return e.JobUID + ":" + e.Type
}

// HeartbeatRequest is the body of POST /agent/v1/heartbeat.
type HeartbeatRequest struct {
	ClusterName  string `json:"clusterName"`
	AgentVersion string `json:"agentVersion"`
	CronJobCount int    `json:"cronJobCount"`
}
