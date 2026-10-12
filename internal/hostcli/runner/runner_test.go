//go:build unix

package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func shell(script string) []string { return []string{"/bin/sh", "-c", script} }

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func runShell(t *testing.T, script string) (Result, *syncBuffer, *syncBuffer) {
	t.Helper()
	out, errOut := &syncBuffer{}, &syncBuffer{}
	res := Run(Options{Argv: shell(script), Stdin: devNull(t), Stdout: out, Stderr: errOut})
	return res, out, errOut
}

func TestExitCodeIsPassedThrough(t *testing.T) {
	for _, code := range []int{0, 1, 2, 7, 64, 126, 127, 255} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			res, _, _ := runShell(t, fmt.Sprintf("exit %d", code))
			if res.ExitCode != code || res.Signal != "" || res.Err != nil {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

func TestDeathBySignalIsReportedAs128PlusN(t *testing.T) {
	tests := []struct {
		signal string
		code   int
	}{{"KILL", 137}, {"TERM", 143}, {"SEGV", 139}, {"HUP", 129}, {"INT", 130}}
	for _, tt := range tests {
		t.Run(tt.signal, func(t *testing.T) {
			res, _, _ := runShell(t, "kill -"+tt.signal+" $$")
			if res.ExitCode != tt.code || res.Signal != "SIG"+tt.signal {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

func TestStdoutAndStderrPassThroughSeparately(t *testing.T) {
	res, out, errOut := runShell(t, "echo to-out; echo to-err >&2")
	if res.ExitCode != 0 || out.String() != "to-out\n" || errOut.String() != "to-err\n" {
		t.Fatalf("res=%+v out=%q err=%q", res, out.String(), errOut.String())
	}
}

func TestCaptureSeesBothStreams(t *testing.T) {
	out, errOut, capture := &syncBuffer{}, &syncBuffer{}, &syncBuffer{}
	res := Run(Options{Argv: shell("echo one; sleep 0.1; echo two >&2; sleep 0.1; echo three"),
		Stdin: devNull(t), Stdout: out, Stderr: errOut, Capture: capture})
	if res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if capture.String() != "one\ntwo\nthree\n" {
		t.Fatalf("capture = %q", capture.String())
	}
	if out.String() != "one\nthree\n" || errOut.String() != "two\n" {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
}

func TestSharedDestinationKeepsExactOrder(t *testing.T) {
	both, capture := &syncBuffer{}, &syncBuffer{}
	script := "i=0; while [ $i -lt 200 ]; do echo out$i; echo err$i >&2; i=$((i+1)); done"
	res := Run(Options{Argv: shell(script), Stdin: devNull(t), Stdout: both, Stderr: both, Capture: capture})
	if res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	var want strings.Builder
	for i := range 200 {
		fmt.Fprintf(&want, "out%d\nerr%d\n", i, i)
	}
	if capture.String() != want.String() || both.String() != want.String() {
		t.Fatalf("order not preserved: capture has %d bytes, want %d", capture.Len(), want.Len())
	}
}

func TestSameFileForStdoutAndStderrKeepsExactOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "both.log")
	f, err := os.Create(path) //nolint:gosec // the path is inside t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	g, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // the path is inside t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	capture := &syncBuffer{}
	script := "i=0; while [ $i -lt 100 ]; do echo out$i; echo err$i >&2; i=$((i+1)); done"
	res := Run(Options{Argv: shell(script), Stdin: devNull(t), Stdout: f, Stderr: g, Capture: capture})
	if res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	var want strings.Builder
	for i := range 100 {
		fmt.Fprintf(&want, "out%d\nerr%d\n", i, i)
	}
	if capture.String() != want.String() {
		t.Fatalf("capture order differs (%d bytes vs %d)", capture.Len(), want.Len())
	}
	written, _ := os.ReadFile(path) //nolint:gosec // the path is inside t.TempDir()
	if string(written) != want.String() {
		t.Fatalf("file content differs (%d bytes vs %d)", len(written), want.Len())
	}
}

func TestFilesAreHandedToTheChildDirectlyWithoutCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.log")
	f, err := os.Create(path) //nolint:gosec // test fixture needs this file mode or path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	res := Run(Options{Argv: shell("echo direct; exit 3"), Stdin: devNull(t), Stdout: f, Stderr: f})
	data, _ := os.ReadFile(path) //nolint:gosec // test fixture needs this file mode or path
	if res.ExitCode != 3 || string(data) != "direct\n" {
		t.Fatalf("res=%+v data=%q", res, data)
	}
}

func TestStdinPassesThrough(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	go func() {
		_, _ = w.WriteString("line one\nline two\n")
		_ = w.Close()
	}()
	out := &syncBuffer{}
	res := Run(Options{Argv: []string{"cat"}, Stdin: r, Stdout: out, Stderr: &syncBuffer{}, Capture: &syncBuffer{}})
	if res.ExitCode != 0 || out.String() != "line one\nline two\n" {
		t.Fatalf("res=%+v out=%q", res, out.String())
	}
}

func TestLargeOutputOnBothStreamsDoesNotDeadlock(t *testing.T) {
	const size = 5_000_000
	out, errOut, capture := &syncBuffer{}, &syncBuffer{}, &syncBuffer{}
	script := fmt.Sprintf(`head -c %d /dev/zero | tr '\0' a & head -c %d /dev/zero | tr '\0' b >&2 & wait`, size, size)
	done := make(chan Result, 1)
	go func() {
		done <- Run(Options{Argv: shell(script), Stdin: devNull(t), Stdout: out, Stderr: errOut, Capture: capture})
	}()
	select {
	case res := <-done:
		if res.ExitCode != 0 {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock on large output")
	}
	if out.Len() != size || errOut.Len() != size || capture.Len() != 2*size {
		t.Fatalf("lengths out=%d err=%d capture=%d", out.Len(), errOut.Len(), capture.Len())
	}
}

type failingWriter struct{ calls int }

func (w *failingWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("closed pipe")
}

func TestBrokenDestinationDoesNotBreakTheJob(t *testing.T) {
	capture := &syncBuffer{}
	res := Run(Options{Argv: shell("i=0; while [ $i -lt 300 ]; do echo line$i; i=$((i+1)); done; exit 9"),
		Stdin: devNull(t), Stdout: &failingWriter{}, Stderr: &failingWriter{}, Capture: capture})
	if res.ExitCode != 9 || !strings.HasSuffix(capture.String(), "line299\n") {
		t.Fatalf("res=%+v capture tail=%q", res, capture.String()[max(0, capture.Len()-20):])
	}
}

func TestCommandNotFoundIs127(t *testing.T) {
	for _, argv := range [][]string{{"definitely-not-a-command-xyz"}, {"/nonexistent/dir/cmd"}, {"./missing-relative"}, {""}} {
		res := Run(Options{Argv: argv, Stdin: devNull(t), Stdout: io.Discard, Stderr: io.Discard})
		if res.ExitCode != 127 || !errors.Is(res.Err, ErrNotFound) {
			t.Errorf("%q: result = %+v", argv, res)
		}
	}
}

func TestNotExecutableIs126(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(plain, []byte("echo hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Run(Options{Argv: []string{plain}, Stdin: devNull(t), Stdout: io.Discard, Stderr: io.Discard})
	if res.ExitCode != 126 || !errors.Is(res.Err, ErrNotExecutable) {
		t.Fatalf("file: result = %+v", res)
	}
	res = Run(Options{Argv: []string{dir}, Stdin: devNull(t), Stdout: io.Discard, Stderr: io.Discard})
	if res.ExitCode != 126 || !errors.Is(res.Err, ErrNotExecutable) {
		t.Fatalf("directory: result = %+v", res)
	}
}

func TestNonExecutableFoundOnPathIs126NotFoundIs127(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + dir}
	res := Run(Options{Argv: []string{"tool"}, Env: env, Stdin: devNull(t), Stdout: io.Discard, Stderr: io.Discard})
	if res.ExitCode != 126 {
		t.Fatalf("result = %+v", res)
	}
	res = Run(Options{Argv: []string{"other"}, Env: env, Stdin: devNull(t), Stdout: io.Discard, Stderr: io.Discard})
	if res.ExitCode != 127 {
		t.Fatalf("result = %+v", res)
	}
}

func TestExecutableOnPathRunsAndSkipsNonExecutableEarlierEntries(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "tool"), []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "tool"), []byte("#!/bin/sh\nexit 5\n"), 0o755); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	res := Run(Options{Argv: []string{"tool"}, Env: []string{"PATH=" + first + ":" + second},
		Stdin: devNull(t), Stdout: io.Discard, Stderr: io.Discard})
	if res.ExitCode != 5 {
		t.Fatalf("result = %+v", res)
	}
}

func TestScriptWithoutShebangRunsLikeInAShell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte("echo from script\nexit 4\n"), 0o755); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	out := &syncBuffer{}
	res := Run(Options{Argv: []string{path, "arg"}, Stdin: devNull(t), Stdout: out, Stderr: io.Discard})
	if res.ExitCode != 4 || out.String() != "from script\n" {
		t.Fatalf("res=%+v out=%q", res, out.String())
	}
}

func TestEmptyArgvIsAUsageError(t *testing.T) {
	res := Run(Options{Stdin: devNull(t), Stdout: io.Discard, Stderr: io.Discard})
	if res.ExitCode != 64 || !errors.Is(res.Err, ErrNoCommand) {
		t.Fatalf("result = %+v", res)
	}
}

func TestArgumentsAreNotInterpretedByCronbeat(t *testing.T) {
	out := &syncBuffer{}
	res := Run(Options{Argv: []string{"printf", "%s|", "a b", "--name", "$HOME", "*"}, Stdin: devNull(t), Stdout: out, Stderr: io.Discard})
	if res.ExitCode != 0 || out.String() != "a b|--name|$HOME|*|" {
		t.Fatalf("res=%+v out=%q", res, out.String())
	}
}

func TestEnvironmentIsInheritedOrReplaced(t *testing.T) {
	t.Setenv("CRONBEAT_RUNNER_TEST", "inherited")
	out := &syncBuffer{}
	Run(Options{Argv: shell("printf %s \"$CRONBEAT_RUNNER_TEST\""), Stdin: devNull(t), Stdout: out, Stderr: io.Discard})
	if out.String() != "inherited" {
		t.Fatalf("inherit: %q", out.String())
	}
	out = &syncBuffer{}
	Run(Options{Argv: shell("printf %s \"$ONLY\""), Env: []string{"ONLY=custom", "PATH=/usr/bin:/bin"}, Stdin: devNull(t), Stdout: out, Stderr: io.Discard})
	if out.String() != "custom" {
		t.Fatalf("replace: %q", out.String())
	}
}

func TestDurationIsMeasured(t *testing.T) {
	res, _, _ := runShell(t, "sleep 0.3")
	if res.Duration < 250*time.Millisecond || res.Duration > 5*time.Second {
		t.Fatalf("duration = %v", res.Duration)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runWithSignal(t *testing.T, script string, sig syscall.Signal, ready string) (Result, *syncBuffer) {
	t.Helper()
	signals := make(chan os.Signal, 4)
	out := &syncBuffer{}
	done := make(chan Result, 1)
	go func() {
		done <- Run(Options{Argv: shell(script), Stdin: devNull(t), Stdout: out, Stderr: io.Discard, Signals: signals})
	}()
	waitFor(t, "child readiness", func() bool { return strings.Contains(out.String(), ready) })
	signals <- sig
	select {
	case res := <-done:
		return res, out
	case <-time.After(15 * time.Second):
		t.Fatal("child did not end after the signal")
		return Result{}, nil
	}
}

func TestSignalsAreForwardedToTheChild(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			res, _ := runWithSignal(t, "trap 'exit 42' TERM INT HUP; echo ready; while :; do sleep 0.05; done", sig, "ready")
			if res.ExitCode != 42 {
				t.Fatalf("handler did not see the signal: %+v", res)
			}
		})
	}
}

func TestForwardedSignalKillsAChildWithoutHandler(t *testing.T) {
	res, _ := runWithSignal(t, "echo ready; exec sleep 60", syscall.SIGTERM, "ready")
	if res.ExitCode != 143 || res.Signal != "SIGTERM" {
		t.Fatalf("result = %+v", res)
	}
}

func TestSignalsReachGrandchildrenThroughTheProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := fmt.Sprintf("sleep 60 & echo $! > %s; echo ready; wait", pidFile)
	res, _ := runWithSignal(t, script, syscall.SIGTERM, "ready")
	if res.ExitCode != 143 {
		t.Fatalf("result = %+v", res)
	}
	raw, err := os.ReadFile(pidFile) //nolint:gosec // test fixture needs this file mode or path
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "grandchild to die", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
}

func TestChildRunsInItsOwnProcessGroup(t *testing.T) {
	_, out, _ := runShell(t, "echo $$; ps -o pgid= -p $$")
	fields := strings.Fields(out.String())
	if len(fields) != 2 {
		t.Fatalf("output = %q", out.String())
	}
	if fields[0] != fields[1] {
		t.Fatalf("child pid %s is not the leader of its group %s", fields[0], fields[1])
	}
	if strconv.Itoa(syscall.Getpgrp()) == fields[1] {
		t.Fatal("child shares the process group of cronbeat")
	}
}

func TestSignalQueuedBeforeStartIsDelivered(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	res := Run(Options{Argv: []string{"sleep", "60"}, Stdin: devNull(t), Stdout: io.Discard, Stderr: io.Discard, Signals: signals})
	if res.ExitCode != 143 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRealSignalsSentToTheProcessAreForwarded(t *testing.T) {
	out := &syncBuffer{}
	done := make(chan Result, 1)
	go func() {
		done <- Run(Options{Argv: shell("trap 'exit 33' TERM; echo ready; while :; do sleep 0.05; done"),
			Stdin: devNull(t), Stdout: out, Stderr: io.Discard})
	}()
	waitFor(t, "child readiness", func() bool { return strings.Contains(out.String(), "ready") })
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.ExitCode != 33 {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("real SIGTERM was not forwarded")
	}
}

func TestReturnsPromptlyWhenABackgroundProcessKeepsTheOutputOpen(t *testing.T) {
	capture := &syncBuffer{}
	start := time.Now()
	res := Run(Options{Argv: shell("sleep 3 & echo launched"), Stdin: devNull(t), Stdout: &syncBuffer{}, Stderr: &syncBuffer{},
		Capture: capture, DrainGrace: 300 * time.Millisecond})
	if res.ExitCode != 0 || capture.String() != "launched\n" {
		t.Fatalf("res=%+v capture=%q", res, capture.String())
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("waited %v for the background process", elapsed)
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("cannot list /dev/fd")
	}
	return len(entries)
}

func TestNoFileDescriptorOrGoroutineLeaks(t *testing.T) {
	stdin := devNull(t)
	runShell(t, "echo warmup")
	before := openFDs(t)
	goroutines := goroutineCount()
	for range 30 {
		capture := &syncBuffer{}
		Run(Options{Argv: shell("echo out; echo err >&2; exit 1"), Stdin: stdin, Stdout: &syncBuffer{}, Stderr: &syncBuffer{}, Capture: capture})
		Run(Options{Argv: []string{"definitely-not-a-command-xyz"}, Stdin: stdin, Stdout: io.Discard, Stderr: io.Discard, Capture: capture})
	}
	waitFor(t, "descriptors to be released", func() bool { return openFDs(t) <= before })
	waitFor(t, "goroutines to finish", func() bool { return goroutineCount() <= goroutines })
}
