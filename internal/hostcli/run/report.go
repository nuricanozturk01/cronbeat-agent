//go:build unix

package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/client"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/mask"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/runner"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/tail"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/warn"
	"cronbeat.dev/cronbeat-agent/internal/hostversion"
)

const maxWarningLength = 300

type outcome struct {
	resp client.Response
	err  error
}

type reporter struct {
	env     Env
	p       prepared
	diag    *diagnostics
	poster  Poster
	runID   string
	started chan outcome
	cancel  context.CancelFunc
	ctx     context.Context
}

func newReporter(env Env, p prepared, diag *diagnostics) *reporter {
	r := &reporter{env: env, p: p, diag: diag, started: make(chan outcome, 1)}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	defer func() {
		if recover() != nil {
			r.poster = nil
			diag.add("internal", "internal error while preparing the report; the command runs without monitoring")
		}
	}()
	id, err := env.NewRunID()
	if err != nil || id == "" {
		diag.add("internal", "cannot create a run id; the command runs without monitoring")
		return r
	}
	r.runID = id
	r.poster = env.NewPoster(p.apiURL, p.key)
	return r
}

func (r *reporter) enabled() bool { return r.poster != nil && r.runID != "" }

func (r *reporter) request(runType string) client.Request {
	return client.Request{AgentVersion: hostversion.Version, Job: r.p.job, Run: client.Run{ID: r.runID, Type: runType}}
}

func (r *reporter) start() {
	if !r.enabled() {
		close(r.started)
		return
	}
	req := r.request(client.RunStarted)
	go func() {
		defer func() {
			if recover() != nil {
				r.started <- outcome{err: errors.New("internal error")}
			}
		}()
		resp, err := r.poster.PostRun(r.ctx, req, client.StartedPolicy)
		r.started <- outcome{resp, err}
	}()
}

func (r *reporter) finish(res runner.Result, output string) {
	if !r.enabled() {
		return
	}
	defer r.cancel()
	defer func() {
		if recover() != nil {
			r.diag.add("internal", "internal error while reporting the result")
		}
	}()
	r.collect("started", r.waitStarted())

	runType, exit, durationMs := client.RunSucceeded, res.ExitCode, res.Duration.Milliseconds()
	req := r.request(runType)
	if res.ExitCode != 0 {
		req.Run.Type = client.RunFailed
	}
	req.Run.ExitCode, req.Run.DurationMs = &exit, &durationMs
	req.Run.Signal, req.Run.Output = res.Signal, output

	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- outcome{err: errors.New("internal error")}
			}
		}()
		resp, err := r.poster.PostRun(r.ctx, req, client.FinishPolicy)
		done <- outcome{resp, err}
	}()
	wait := r.env.FinishWait
	if wait <= 0 {
		wait = client.FinishPolicy.TotalBudget + finishWaitSlop
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case o := <-done:
		r.collect(req.Run.Type, o)
	case <-timer.C:
		r.collect(req.Run.Type, outcome{err: errors.New("timed out")})
	}
}

func (r *reporter) waitStarted() outcome {
	wait := r.env.StartedWait
	if wait <= 0 {
		wait = client.StartedPolicy.TotalBudget + startedWaitSlop
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case o, ok := <-r.started:
		if !ok {
			return outcome{}
		}
		return o
	case <-timer.C:
		return outcome{err: errors.New("timed out")}
	}
}

func (r *reporter) collect(event string, o outcome) {
	if o.err == nil {
		r.diag.verbosef("%s event sent (monitor %s, created %v)", event, o.resp.MonitorID, o.resp.Created)
		for _, w := range o.resp.Warnings {
			r.diag.add("server-warning", "CronBeat says: "+sanitize(w))
		}
		return
	}
	r.diag.verbosef("%s event failed: %s", event, sanitize(o.err.Error()))
	r.diag.add(client.Kind(o.err), describe(o.err))
}

func describe(err error) string {
	var se *client.StatusError
	if !errors.As(err, &se) {
		return fmt.Sprintf("could not reach CronBeat (%s); the result of this run was not reported", sanitize(err.Error()))
	}
	detail := ""
	if se.Detail != "" {
		detail = ": " + sanitize(se.Detail)
	}
	switch client.Kind(err) {
	case "auth":
		return "CronBeat rejected the API key (HTTP 401); check it with `cronbeat check`"
	case "forbidden":
		return fmt.Sprintf("CronBeat refused this job (HTTP 403)%s", detail)
	case "bad-request":
		return fmt.Sprintf("CronBeat rejected the report (HTTP %d)%s", se.Status, detail)
	case "rate-limited":
		return "CronBeat is rate limiting this host (HTTP 429), the result was not reported"
	case "server":
		return fmt.Sprintf("CronBeat had a server error (HTTP %d), the result was not reported", se.Status)
	default:
		return fmt.Sprintf("unexpected answer from CronBeat (HTTP %d)%s", se.Status, detail)
	}
}

func sanitize(text string) string {
	var b strings.Builder
	for _, r := range mask.Text(text) {
		if unicode.IsControl(r) {
			r = ' '
		}
		b.WriteRune(r)
	}
	return tail.Last(strings.Join(strings.Fields(b.String()), " "), maxWarningLength)
}

type diagnostics struct {
	env      Env
	key      string
	verbose  bool
	mu       sync.Mutex
	problems []problem
	lines    []string
}

func (d *diagnostics) add(kind, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.problems {
		if p.kind == kind && p.msg == msg {
			return
		}
	}
	d.problems = append(d.problems, problem{kind, msg})
}

func (d *diagnostics) verbosef(format string, args ...any) {
	if !d.verbose {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lines = append(d.lines, fmt.Sprintf(format, args...))
}

func (d *diagnostics) flush(limiter *warn.Limiter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out io.Writer = &keyScrubber{w: d.env.Stderr, key: d.key}
	for _, line := range d.lines {
		_, _ = fmt.Fprintf(out, "cronbeat: %s\n", line)
	}
	shownKinds := map[string]bool{}
	for _, p := range d.problems {
		if shownKinds[p.kind] && !d.verbose {
			continue
		}
		if !d.verbose && !limiter.Allow(p.kind) {
			shownKinds[p.kind] = true
			continue
		}
		shownKinds[p.kind] = true
		_, _ = fmt.Fprintf(out, "cronbeat: warning: %s\n", p.msg)
	}
}

type keyScrubber struct {
	w   io.Writer
	key string
}

func (k *keyScrubber) Write(p []byte) (int, error) {
	if k.key != "" {
		if _, err := k.w.Write([]byte(strings.ReplaceAll(string(p), k.key, "***"))); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return k.w.Write(p)
}
