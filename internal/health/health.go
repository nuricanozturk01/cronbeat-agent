// Package health serves the liveness and readiness probes of the agent.
package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	// DefaultMaxSilence is how long the heartbeat loop may go without starting a
	// round before the liveness probe fails. A round with the API down takes
	// about two minutes at most, so this leaves a wide margin.
	DefaultMaxSilence = 10 * time.Minute

	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// Health tracks whether the agent is alive and ready. Alive means the heartbeat
// loop is still running rounds, which catches a hung process that Kubernetes
// cannot see. Ready means the informer caches are synced.
type Health struct {
	maxSilence time.Duration
	now        func() time.Time
	lastBeat   atomic.Int64
	ready      atomic.Bool
}

// New creates a Health that counts as alive for maxSilence after the last beat.
func New(maxSilence time.Duration) *Health {
	h := &Health{maxSilence: maxSilence, now: time.Now}
	h.Beat()
	return h
}

// Beat records that the heartbeat loop is running.
func (h *Health) Beat() { h.lastBeat.Store(h.now().UnixNano()) }

// SetReady marks the caches as synced.
func (h *Health) SetReady() { h.ready.Store(true) }

func (h *Health) alive() bool {
	return h.now().Sub(time.Unix(0, h.lastBeat.Load())) <= h.maxSilence
}

// Handler answers GET and HEAD of /healthz and /readyz with a bare "ok". It
// reveals nothing else about the agent.
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", probe(h.alive))
	mux.HandleFunc("/readyz", probe(h.ready.Load))
	return mux
}

func probe(ok func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !ok() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}
}

// Serve answers on lis until ctx ends, then shuts down gracefully.
func Serve(ctx context.Context, lis net.Listener, handler http.Handler) error {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
