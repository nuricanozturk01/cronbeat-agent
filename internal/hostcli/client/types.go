// Package client talks to the CronBeat host API with short, bounded budgets so a
// slow or unreachable CronBeat never delays the wrapped job noticeably.
package client

// Run types of the host API.
const (
	RunStarted   = "started"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// Job identifies the monitored job.
type Job struct {
	Name              string `json:"name"`
	Schedule          string `json:"schedule,omitempty"`
	TimeZone          string `json:"timeZone,omitempty"`
	GraceSeconds      *int   `json:"graceSeconds,omitempty"`
	MaxRuntimeSeconds *int   `json:"maxRuntimeSeconds,omitempty"`
}

// Run is one event of a job run.
type Run struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	DurationMs *int64 `json:"durationMs,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	Signal     string `json:"signal,omitempty"`
	Output     string `json:"output,omitempty"`
}

// Request is the body of POST /agent/v1/host/runs.
type Request struct {
	AgentVersion string `json:"agentVersion,omitempty"`
	Job          Job    `json:"job"`
	Run          Run    `json:"run"`
}

// Response is the 202 body of POST /agent/v1/host/runs.
type Response struct {
	MonitorID string   `json:"monitorId"`
	Created   bool     `json:"created"`
	Warnings  []string `json:"warnings"`
}

// CheckResponse is the 200 body of POST /agent/v1/host/check.
type CheckResponse struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Version int    `json:"version"`
}
