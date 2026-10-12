//go:build unix

// Package run implements "cronbeat run": it runs the command, reports it to CronBeat and
// never lets a CronBeat problem change what the command does.
package run

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/client"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/config"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/runner"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/tail"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/warn"
)

const (
	exitUsage       = 64
	exitInternal    = 70
	maskContext     = 512
	startedWaitSlop = 500 * time.Millisecond
	finishWaitSlop  = time.Second
)

// Poster sends run events to CronBeat.
type Poster interface {
	PostRun(ctx context.Context, req client.Request, p client.Policy) (client.Response, error)
}

// Env is what "cronbeat run" needs from its surroundings; tests replace the parts that touch the outside world.
type Env struct {
	Args       []string
	Getenv     func(string) string
	Stdin      *os.File
	Stdout     io.Writer
	Stderr     io.Writer
	Home       string
	EtcKeyPath string
	Now        func() time.Time
	NewPoster  func(apiURL string, key config.Secret) Poster
	NewRunID   func() (string, error)
	// TimeZone returns the IANA zone of this host, or "" when unknown.
	TimeZone func() string
	Signals  <-chan os.Signal
	// DrainGrace, StartedWait and FinishWait default to the production budgets when zero.
	DrainGrace  time.Duration
	StartedWait time.Duration
	FinishWait  time.Duration
}

// NewRunID returns a random UUID version 4.
func NewRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Main runs the command given in env.Args and returns the exit code of cronbeat.
// Whatever goes wrong inside cronbeat, a command that has not run yet still runs.
func Main(env Env) (code int) {
	command := commandOf(env.Args)
	jobStarted := false
	var result *runner.Result
	defer func() {
		if recover() == nil {
			return
		}
		switch {
		case result != nil:
			code = result.ExitCode
		case !jobStarted && len(command) > 0:
			code = runBare(env, command)
		default:
			code = exitInternal
		}
	}()

	p := prepare(env)
	if p.exit >= 0 {
		return p.exit
	}
	diag := &diagnostics{env: env, verbose: p.opts.verbose, key: p.key.Reveal()}
	diag.problems = p.problems
	if p.report {
		diag.verbosef("API %s, key from %s", p.apiURL, p.keySource)
	}

	var rep *reporter
	if p.report {
		rep = newReporter(env, p, diag)
		rep.start()
	}

	var capture io.Writer
	ring := tail.New(p.opts.tailBytes + maskContext)
	if p.report && p.opts.output != outputNever {
		capture = ring
	}
	jobStarted = true
	res := runner.Run(runner.Options{
		Argv: p.command, Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr, Capture: capture,
		Signals: env.Signals, DrainGrace: env.DrainGrace,
	})
	result = &res
	if res.Err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "cronbeat: %v\n", res.Err)
	}
	diag.verbosef("command ended: exit code %d, signal %q, after %v", res.ExitCode, res.Signal, res.Duration.Round(time.Millisecond))

	if rep != nil {
		rep.finish(res, outputOf(ring, p, res))
	}
	diag.flush(warnLimiter(env))
	return res.ExitCode
}

func runBare(env Env, command []string) int {
	return runner.Run(runner.Options{Argv: command, Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr, Signals: env.Signals}).ExitCode
}

func warnLimiter(env Env) *warn.Limiter {
	return warn.NewLimiter(warn.StatePath(env.Getenv, env.Home), env.Now)
}

func outputOf(ring *tail.Ring, p prepared, res runner.Result) string {
	failed := res.ExitCode != 0
	switch p.opts.output {
	case outputNever:
		return ""
	case outputFailure:
		if !failed {
			return ""
		}
	}
	text := ring.Snapshot()
	if key := p.key.Reveal(); key != "" {
		text = strings.ReplaceAll(text, key, "***")
	}
	return tail.Last(maskText(text), p.opts.tailBytes)
}
