// Package syncer delivers events, the periodic CronJob sync and heartbeats to
// the CronBeat API.
package syncer

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
)

const (
	// DefaultQueueSize bounds the in-memory event queue.
	DefaultQueueSize = 1000
	// DefaultSlowRetry is the wait between delivery rounds after the API
	// client exhausted its own retries (or answered 401).
	DefaultSlowRetry = 30 * time.Second
	defaultMaxSeen   = 20000
)

// EventSender is the part of the API client the queue needs.
type EventSender interface {
	SendEvent(ctx context.Context, ev api.Event) error
}

// Queue is a bounded in-memory event queue with a single delivery worker, so
// informer callbacks never block.
//
// Guarantee: every (jobUid, type) pair is enqueued at most once per process
// lifetime. Events live only in memory: they are lost if the process stops
// before delivery, and dropped (with an error log) when the queue is full.
// After a restart the informers replay current Jobs and the backend
// de-duplicates by jobUid:type, which recovers recent events.
type Queue struct {
	sender    EventSender
	log       *slog.Logger
	ch        chan api.Event
	slowRetry time.Duration
	maxSeen   int

	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
}

// NewQueue creates a queue with the given capacity.
func NewQueue(sender EventSender, size int, slowRetry time.Duration, log *slog.Logger) *Queue {
	return &Queue{
		sender:    sender,
		log:       log,
		ch:        make(chan api.Event, size),
		slowRetry: slowRetry,
		maxSeen:   defaultMaxSeen,
		seen:      map[string]struct{}{},
	}
}

// Emit enqueues the event unless the same (jobUid, type) was seen before. It
// never blocks.
func (q *Queue) Emit(ev api.Event) {
	key := ev.DedupeKey()
	q.mu.Lock()
	if _, dup := q.seen[key]; dup {
		q.mu.Unlock()
		return
	}
	q.markLocked(key)
	q.mu.Unlock()

	select {
	case q.ch <- ev:
	default:
		q.mu.Lock()
		delete(q.seen, key) // allow a later informer update to retry
		q.mu.Unlock()
		q.log.Error("event queue full, dropping event",
			"type", ev.Type, "namespace", ev.Namespace, "cronJob", ev.CronJobName)
	}
}

func (q *Queue) markLocked(key string) {
	q.seen[key] = struct{}{}
	q.order = append(q.order, key)
	for len(q.order) > q.maxSeen {
		delete(q.seen, q.order[0])
		q.order = q.order[1:]
	}
}

// Forget releases the dedupe entries of a deleted Job.
func (q *Queue) Forget(jobUID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range []string{api.EventJobStarted, api.EventJobSucceeded, api.EventJobFailed} {
		delete(q.seen, jobUID+":"+t)
	}
}

// Pending returns the number of queued events.
func (q *Queue) Pending() int { return len(q.ch) }

// Run delivers events until ctx is done.
func (q *Queue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-q.ch:
			q.deliver(ctx, ev)
		}
	}
}

// Drain delivers what is still queued until ctx (a short grace context) ends.
func (q *Queue) Drain(ctx context.Context) {
	for {
		select {
		case ev := <-q.ch:
			q.deliver(ctx, ev)
			if ctx.Err() != nil {
				return
			}
		default:
			return
		}
	}
}

func (q *Queue) deliver(ctx context.Context, ev api.Event) {
	for {
		err := q.sender.SendEvent(ctx, ev)
		if err == nil {
			q.log.Debug("event delivered", "type", ev.Type, "namespace", ev.Namespace, "cronJob", ev.CronJobName)
			return
		}
		if ctx.Err() != nil {
			q.log.Warn("event not delivered before shutdown",
				"type", ev.Type, "namespace", ev.Namespace, "cronJob", ev.CronJobName)
			return
		}
		if api.IsPermanent(err) {
			q.log.Error("event rejected by the API, dropping it",
				"type", ev.Type, "namespace", ev.Namespace, "cronJob", ev.CronJobName, "error", err.Error())
			return
		}
		if api.IsUnauthorized(err) {
			q.log.Error("event delivery blocked: API key rejected (401), retrying slowly",
				"retryIn", q.slowRetry.String())
		} else {
			q.log.Warn("event delivery failed, retrying slowly",
				"type", ev.Type, "retryIn", q.slowRetry.String(), "error", err.Error())
		}
		timer := time.NewTimer(q.slowRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
