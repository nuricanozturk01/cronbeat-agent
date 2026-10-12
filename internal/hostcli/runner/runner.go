//go:build unix

// Package runner runs the wrapped command and reports how it ended. The command
// must never be harmed by CronBeat: its exit code, signals, stdin and output pass through.
package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/tty"
)

const (
	exitUsage          = 64
	exitNotExecutable  = 126
	exitNotFound       = 127
	signalExitBase     = 128
	defaultDrainGrace  = 2 * time.Second
	copyBufferSize     = 32 * 1024
	defaultSearchPath  = "/usr/local/bin:/usr/bin:/bin"
	fallbackShellPath  = "/bin/sh"
	signalChannelDepth = 8
)

// Errors reported in Result.Err when the command could not be started.
var (
	ErrNoCommand     = errors.New("no command given")
	ErrNotFound      = errors.New("command not found")
	ErrNotExecutable = errors.New("command cannot be executed")
)

// Options describes one command run.
type Options struct {
	// Argv is the command and its arguments; Argv[0] is looked up in PATH.
	Argv []string
	// Env replaces the environment of the command; nil inherits it.
	Env    []string
	Stdin  *os.File
	Stdout io.Writer
	Stderr io.Writer
	// Capture receives a copy of stdout and stderr in arrival order. Without it,
	// files are handed to the command directly.
	Capture io.Writer
	// Signals replaces the process signals that are forwarded; used by tests.
	Signals <-chan os.Signal
	// DrainGrace bounds the wait for output still held open by background processes.
	DrainGrace time.Duration
}

// Result describes how the command ended.
type Result struct {
	ExitCode int
	Signal   string
	Duration time.Duration
	Err      error
}

// Run starts the command and waits for it.
func Run(o Options) Result {
	if len(o.Argv) == 0 {
		return Result{ExitCode: exitUsage, Err: ErrNoCommand}
	}
	signals, stopSignals := signalSource(o.Signals)
	defer stopSignals()

	path, err := resolve(o.Argv[0], searchPath(o.Env))
	if err != nil {
		return Result{ExitCode: startExitCode(err), Err: err}
	}

	out, err := attach(o)
	if err != nil {
		return Result{ExitCode: exitNotExecutable, Err: fmt.Errorf("%w: %s: %w", ErrNotExecutable, o.Argv[0], err)}
	}
	newGroup := !tty.IsTerminal(o.Stdin)
	cmd, startedAt, err := start(o, path, out, newGroup)
	out.closeWriteEnds()
	if err != nil {
		out.abandon()
		return Result{ExitCode: exitNotExecutable, Err: fmt.Errorf("%w: %s: %w", ErrNotExecutable, o.Argv[0], err)}
	}
	out.startCopying(o.Capture)

	fw := newForwarder(cmd.Process.Pid, newGroup, signals)
	_ = cmd.Wait()
	elapsed := time.Since(startedAt)
	fw.stop()

	grace := o.DrainGrace
	if grace <= 0 {
		grace = defaultDrainGrace
	}
	out.drain(grace)

	res := Result{Duration: elapsed}
	res.ExitCode, res.Signal = exitStatus(cmd.ProcessState)
	return res
}

func signalSource(injected <-chan os.Signal) (<-chan os.Signal, func()) {
	// A caught SIGPIPE makes writes to a closed stdout or stderr fail with EPIPE
	// instead of killing cronbeat; the command still gets the default SIGPIPE after exec.
	pipe := make(chan os.Signal, 1)
	signal.Notify(pipe, syscall.SIGPIPE)
	if injected != nil {
		return injected, func() { signal.Stop(pipe) }
	}
	ch := make(chan os.Signal, signalChannelDepth)
	var registered []os.Signal
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		// A signal ignored at startup (nohup) must stay ignored for the command too.
		if !signal.Ignored(sig) {
			registered = append(registered, sig)
		}
	}
	if len(registered) > 0 {
		signal.Notify(ch, registered...)
	}
	return ch, func() { signal.Stop(ch); signal.Stop(pipe) }
}

func start(o Options, path string, out *outputs, newGroup bool) (*exec.Cmd, time.Time, error) {
	build := func(execPath string, args []string) *exec.Cmd {
		cmd := &exec.Cmd{Path: execPath, Args: args, Env: inheritedEnv(o.Env)}
		if o.Stdin != nil {
			cmd.Stdin = o.Stdin
		}
		cmd.Stdout, cmd.Stderr = out.stdout, out.stderr
		if newGroup {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		return cmd
	}
	startedAt := time.Now()
	cmd := build(path, o.Argv)
	err := cmd.Start()
	if errors.Is(err, syscall.ENOEXEC) {
		cmd = build(fallbackShellPath, append([]string{o.Argv[0], path}, o.Argv[1:]...))
		startedAt = time.Now()
		err = cmd.Start()
	}
	return cmd, startedAt, err
}

func exitStatus(state *os.ProcessState) (int, string) {
	if state == nil {
		return exitNotExecutable, ""
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return signalExitBase + int(ws.Signal()), signalName(ws.Signal())
	}
	if code := state.ExitCode(); code >= 0 {
		return code, ""
	}
	return exitNotExecutable, ""
}

type forwarder struct {
	mu      sync.Mutex
	pid     int
	group   bool
	stopped bool
	quit    chan struct{}
	done    chan struct{}
}

func newForwarder(pid int, group bool, signals <-chan os.Signal) *forwarder {
	f := &forwarder{pid: pid, group: group, quit: make(chan struct{}), done: make(chan struct{})}
	go f.loop(signals)
	return f
}

func (f *forwarder) loop(signals <-chan os.Signal) {
	defer close(f.done)
	for {
		select {
		case <-f.quit:
			return
		case sig := <-signals:
			if s, ok := sig.(syscall.Signal); ok {
				f.send(s)
			}
		}
	}
}

func (f *forwarder) send(sig syscall.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	// On a terminal the command shares our process group and the terminal driver
	// already delivered Ctrl-C and Ctrl-\ to it.
	if !f.group && (sig == syscall.SIGINT || sig == syscall.SIGQUIT) {
		return
	}
	target := f.pid
	if f.group {
		target = -f.pid
	}
	_ = syscall.Kill(target, sig)
}

// stop must be called right after the command has been reaped: its pid can be reused.
func (f *forwarder) stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	close(f.quit)
	<-f.done
}
