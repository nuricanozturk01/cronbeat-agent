// Package app wires the informers, the event queue and the syncer together.
package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
	"cronbeat.dev/cronbeat-agent/internal/config"
	"cronbeat.dev/cronbeat-agent/internal/health"
	"cronbeat.dev/cronbeat-agent/internal/k8s"
	syncer "cronbeat.dev/cronbeat-agent/internal/sync"
	"cronbeat.dev/cronbeat-agent/internal/version"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultCacheSyncTimeout = 2 * time.Minute
	defaultShutdownGrace    = 10 * time.Second
)

// Backend is what the agent needs from the CronBeat API client.
type Backend interface {
	Sync(ctx context.Context, req api.SyncRequest) (api.SyncResponse, error)
	Heartbeat(ctx context.Context, req api.HeartbeatRequest) error
	SendEvent(ctx context.Context, ev api.Event) error
}

// Timing holds the durations; zero values mean the production defaults.
type Timing struct {
	CacheSyncTimeout  time.Duration
	ShutdownGrace     time.Duration
	SlowRetry         time.Duration
	SyncInterval      time.Duration
	HeartbeatInterval time.Duration
	ChangeDebounce    time.Duration
	MinChangeGap      time.Duration
	RetryBase         time.Duration
}

// Options are the dependencies of Run.
type Options struct {
	Config  config.Config
	Kube    kubernetes.Interface
	API     Backend
	Log     *slog.Logger
	Health  *health.Health
	Timing  Timing
	Version string
	Now     func() time.Time
}

func (o *Options) defaults() {
	if o.Timing.CacheSyncTimeout <= 0 {
		o.Timing.CacheSyncTimeout = defaultCacheSyncTimeout
	}
	if o.Timing.ShutdownGrace <= 0 {
		o.Timing.ShutdownGrace = defaultShutdownGrace
	}
	if o.Timing.SlowRetry <= 0 {
		o.Timing.SlowRetry = syncer.DefaultSlowRetry
	}
	if o.Version == "" {
		o.Version = version.Version
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Health == nil {
		o.Health = health.New(health.DefaultMaxSilence)
	}
}

// Run starts the agent and blocks until ctx ends. It returns nil on a clean
// shutdown and an error when the informer caches cannot sync.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	cfg, log := o.Config, o.Log

	known := syncer.NewKnownSet()
	queue := syncer.NewQueue(o.API, syncer.DefaultQueueSize, o.Timing.SlowRetry, log)
	tracker := k8s.NewTracker()
	proc := k8s.NewJobProcessor(cfg.ClusterName, queue, tracker, o.Now, log)
	proc.SetKnown(known.Has)
	watcher := k8s.NewWatcher(o.Kube, cfg.WatchNamespaces, proc, tracker, log)

	if err := watcher.Start(ctx, o.Timing.CacheSyncTimeout); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("informer caches did not sync in time; check the agent ClusterRole/RBAC and API server access")
		}
		return err
	}
	log.Info("informer caches synced")
	o.Health.SetReady()

	s := &syncer.Syncer{
		API: o.API, Source: watcher, Replay: watcher, Known: known, Changes: watcher.CronJobChanges(),
		ClusterName: cfg.ClusterName, AgentVersion: o.Version,
		SyncInterval: o.Timing.SyncInterval, HeartbeatInterval: o.Timing.HeartbeatInterval,
		ChangeDebounce: o.Timing.ChangeDebounce, MinChangeGap: o.Timing.MinChangeGap, RetryBase: o.Timing.RetryBase,
		Beat: o.Health.Beat, Log: log,
	}
	done := make(chan struct{}, 2)
	go func() { queue.Run(ctx); done <- struct{}{} }()
	go func() { s.Run(ctx); done <- struct{}{} }()

	<-ctx.Done()
	log.Info("shutting down", "pendingEvents", queue.Pending())
	<-done
	<-done
	watcher.Stop()

	drainCtx, cancel := context.WithTimeout(context.Background(), o.Timing.ShutdownGrace)
	defer cancel()
	queue.Drain(drainCtx)
	log.Info("stopped")
	return nil
}
