package syncer

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
)

const (
	// DefaultSyncInterval is the full CronJob sync period (PLAN 12.1).
	DefaultSyncInterval = 5 * time.Minute
	// DefaultHeartbeatInterval is the heartbeat period (PLAN 12.1).
	DefaultHeartbeatInterval = 60 * time.Second
	// DefaultChangeDebounce waits for a burst of CronJob changes to settle.
	DefaultChangeDebounce = 2 * time.Second
	// DefaultMinChangeGap is the shortest time between two syncs started by changes.
	DefaultMinChangeGap = 15 * time.Second
	// DefaultRetryBase is the first wait after a failed sync, it doubles up to the sync interval.
	DefaultRetryBase = 15 * time.Second

	maxLoggedSkipped = 5
	jitterShare      = 0.2
)

// API is the part of the API client the syncer needs.
type API interface {
	Sync(ctx context.Context, req api.SyncRequest) (api.SyncResponse, error)
	Heartbeat(ctx context.Context, req api.HeartbeatRequest) error
}

// CronJobSource lists the CronJobs currently known to the informers.
type CronJobSource interface {
	CronJobs() []api.CronJob
	// CronJobCount must be cheap: the heartbeat calls it every minute.
	CronJobCount() int
}

// Replayer runs the known Jobs through the event processor again.
type Replayer interface {
	ReplayJobs()
}

type outcome int

const (
	synced outcome = iota
	// failedTransient can heal by itself: a network error, 401, 429 or 5xx.
	failedTransient
	// failedPermanent cannot be fixed by sending the same thing again.
	failedPermanent
)

// Syncer sends the CronJob list on start, after CronJob changes and
// periodically, and sends heartbeats.
//
// After every successful sync it records which CronJobs the API has a monitor
// for and replays the cached Jobs, so events that had to wait for their
// CronJob (or were dropped by a full queue) are sent without waiting for the
// next Job update.
type Syncer struct {
	API               API
	Source            CronJobSource
	Replay            Replayer
	Known             *KnownSet
	Changes           <-chan struct{}
	ClusterName       string
	AgentVersion      string
	SyncInterval      time.Duration
	HeartbeatInterval time.Duration
	ChangeDebounce    time.Duration
	MinChangeGap      time.Duration
	RetryBase         time.Duration
	// Jitter returns a number in [0,1) used to spread retries (rand by default).
	Jitter func() float64
	// Beat is called at the start of every heartbeat round (liveness probe).
	Beat func()
	Log  *slog.Logger
}

func (s *Syncer) defaults() {
	if s.SyncInterval <= 0 {
		s.SyncInterval = DefaultSyncInterval
	}
	if s.HeartbeatInterval <= 0 {
		s.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if s.ChangeDebounce <= 0 {
		s.ChangeDebounce = DefaultChangeDebounce
	}
	if s.RetryBase <= 0 {
		s.RetryBase = DefaultRetryBase
	}
	if s.Jitter == nil {
		s.Jitter = rand.Float64
	}
}

// payload returns the entries the API accepts and how many were left out.
func (s *Syncer) payload() ([]api.CronJob, []string) {
	all := s.Source.CronJobs()
	valid := make([]api.CronJob, 0, len(all))
	var skipped, problems []string
	for _, c := range all {
		if c.Problem != "" {
			problems = append(problems, c.Namespace+"/"+c.Name+": "+c.Problem)
		}
		if c.Valid() {
			valid = append(valid, c)
		} else {
			skipped = append(skipped, c.Namespace+"/"+c.Name)
		}
	}
	if len(problems) > 0 {
		s.Log.Warn("annotations with an invalid value were ignored",
			"count", len(problems), "examples", firstOf(problems, maxLoggedSkipped))
	}
	return valid, skipped
}

// SyncOnce sends the full CronJob list and logs backend warnings.
func (s *Syncer) SyncOnce(ctx context.Context) outcome {
	list, skipped := s.payload()
	if len(skipped) > 0 {
		s.Log.Warn("CronJobs the API cannot accept were left out of the sync",
			"count", len(skipped), "examples", firstOf(skipped, maxLoggedSkipped))
	}
	if len(list) > api.MaxCronJobs {
		// The API treats the list as complete and pauses every CronJob it does
		// not contain, so a truncated list would do harm. Report and wait.
		s.Log.Error("too many CronJobs for one sync, nothing was sent",
			"cronJobs", len(list), "limit", api.MaxCronJobs,
			"hint", "set WATCH_NAMESPACES to split the cluster across several agents")
		return failedPermanent
	}
	resp, err := s.API.Sync(ctx, api.SyncRequest{
		ClusterName: s.ClusterName, AgentVersion: s.AgentVersion, CronJobs: list,
	})
	if err != nil {
		if ctx.Err() != nil {
			return failedTransient
		}
		if api.IsPermanent(err) {
			s.Log.Error("cronjob sync was rejected, it will not be retried before the next interval",
				"error", err.Error())
			return failedPermanent
		}
		s.Log.Error("cronjob sync failed, retrying soon", "error", err.Error())
		return failedTransient
	}
	for _, w := range resp.Warnings {
		s.Log.Warn("cronbeat api warning", "warning", w)
	}
	if s.Known != nil {
		s.Known.Replace(resp.Monitors)
	}
	s.Log.Info("cronjob sync done", "cronJobs", len(list), "monitors", len(resp.Monitors))
	if s.Replay != nil {
		s.Replay.ReplayJobs()
	}
	return synced
}

// HeartbeatOnce sends one heartbeat.
func (s *Syncer) HeartbeatOnce(ctx context.Context) {
	if s.Beat != nil {
		s.Beat()
	}
	err := s.API.Heartbeat(ctx, api.HeartbeatRequest{
		ClusterName: s.ClusterName, AgentVersion: s.AgentVersion, CronJobCount: s.Source.CronJobCount(),
	})
	if err != nil && ctx.Err() == nil {
		s.Log.Error("heartbeat failed, will retry on the next interval", "error", err.Error())
	}
}

// Run syncs and heartbeats immediately, then periodically until ctx ends. The
// two loops are independent so a slow sync never delays heartbeats.
func (s *Syncer) Run(ctx context.Context) {
	s.defaults()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.heartbeatLoop(ctx)
	}()
	s.syncLoop(ctx)
	<-done
}

func (s *Syncer) heartbeatLoop(ctx context.Context) {
	tick := time.NewTicker(s.HeartbeatInterval)
	defer tick.Stop()
	s.HeartbeatOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.HeartbeatOnce(ctx)
		}
	}
}

func (s *Syncer) syncLoop(ctx context.Context) {
	var lastSync time.Time
	failures := 0
	delay := time.Duration(0)
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-s.Changes: // a nil channel never fires
			timer.Stop()
			if !s.settle(ctx, lastSync) {
				return
			}
		}
		result := s.SyncOnce(ctx)
		lastSync = time.Now()
		switch result {
		case synced, failedPermanent:
			failures = 0
			delay = s.SyncInterval
		case failedTransient:
			failures++
			delay = s.retryDelay(failures)
		}
	}
}

// settle waits for a burst of changes to end and for the minimum gap after the
// last sync. It returns false when ctx ended.
func (s *Syncer) settle(ctx context.Context, lastSync time.Time) bool {
	if !sleep(ctx, s.ChangeDebounce) {
		return false
	}
	if gap := time.Until(lastSync.Add(s.MinChangeGap)); gap > 0 && !sleep(ctx, gap) {
		return false
	}
	for {
		select {
		case <-s.Changes:
		default:
			return true
		}
	}
}

// retryDelay is the wait after the failures-th failed sync in a row: the base
// doubles each time, never goes above the sync interval, and gets up to 20%
// of random extra time so agents of many clusters do not retry in step.
func (s *Syncer) retryDelay(failures int) time.Duration {
	delay := s.SyncInterval
	if failures > 0 && failures < 32 {
		if grown := s.RetryBase << (failures - 1); grown > 0 && grown < delay {
			delay = grown
		}
	}
	extra := time.Duration(s.Jitter() * jitterShare * float64(delay))
	return min(delay+extra, s.SyncInterval)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func firstOf(list []string, n int) string {
	if len(list) > n {
		list = list[:n]
	}
	return strings.Join(list, ", ")
}
