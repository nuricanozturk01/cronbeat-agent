//go:build unix

package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/client"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/config"
	"cronbeat.dev/cronbeat-agent/internal/hostversion"
)

const canary = "cb_agent_CANARYSECRETVALUE123456"

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

type call struct {
	req    client.Request
	policy client.Policy
}

type fakePoster struct {
	mu       sync.Mutex
	calls    []call
	onStart  func(ctx context.Context, req client.Request) (client.Response, error)
	onFinish func(ctx context.Context, req client.Request) (client.Response, error)
	key      config.Secret
	url      string
}

func (f *fakePoster) PostRun(ctx context.Context, req client.Request, p client.Policy) (client.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{req, p})
	hook := f.onFinish
	if req.Run.Type == client.RunStarted {
		hook = f.onStart
	}
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, req)
	}
	return client.Response{MonitorID: "m-1"}, nil
}

func (f *fakePoster) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

type harness struct {
	env    Env
	poster *fakePoster
	stdout *syncBuffer
	stderr *syncBuffer
	vars   map[string]string
	home   string
}

func newHarness(t *testing.T, args ...string) *harness {
	t.Helper()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	home := t.TempDir()
	h := &harness{poster: &fakePoster{}, stdout: &syncBuffer{}, stderr: &syncBuffer{}, home: home,
		vars: map[string]string{
			"CRONBEAT_API_KEY": canary,
			"CRONBEAT_API_URL": "https://api.example.test",
			"XDG_CACHE_HOME":   filepath.Join(home, "cache"),
		}}
	runIDs := 0
	h.env = Env{
		Args:       args,
		Getenv:     func(k string) string { return h.vars[k] },
		Stdin:      devNull,
		Stdout:     h.stdout,
		Stderr:     h.stderr,
		Home:       home,
		EtcKeyPath: filepath.Join(home, "etc-key"),
		Now:        func() time.Time { return time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC) },
		NewPoster: func(url string, key config.Secret) Poster {
			h.poster.url, h.poster.key = url, key
			return h.poster
		},
		NewRunID: func() (string, error) {
			runIDs++
			return fmt.Sprintf("00000000-0000-4000-8000-%012d", runIDs), nil
		},
		TimeZone:    func() string { return "" },
		DrainGrace:  time.Second,
		StartedWait: 2 * time.Second,
		FinishWait:  2 * time.Second,
	}
	return h
}

func (h *harness) run() int { return Main(h.env) }

func sh(script string) []string { return []string{"--", "/bin/sh", "-c", script} }

func join(parts ...[]string) []string {
	var all []string
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

func flags(kv ...string) []string { return kv }

func TestSuccessfulJobReportsStartedThenSucceededWithTheSameRunID(t *testing.T) {
	h := newHarness(t, join(flags("--name", "nightly", "--schedule", "0 2 * * *"), sh("echo hello; exit 0"))...)
	if code := h.run(); code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, h.stderr.String())
	}
	calls := h.poster.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	started, finished := calls[0], calls[1]
	if started.req.Run.Type != client.RunStarted || finished.req.Run.Type != client.RunSucceeded {
		t.Fatalf("types = %s, %s", started.req.Run.Type, finished.req.Run.Type)
	}
	if started.req.Run.ID != finished.req.Run.ID || started.req.Run.ID == "" {
		t.Fatalf("run ids = %q %q", started.req.Run.ID, finished.req.Run.ID)
	}
	if finished.req.Job.Name != "nightly" || finished.req.Job.Schedule != "0 2 * * *" || finished.req.AgentVersion != hostversion.Version {
		t.Fatalf("job = %+v version %q", finished.req.Job, finished.req.AgentVersion)
	}
	if finished.req.Run.ExitCode == nil || *finished.req.Run.ExitCode != 0 || finished.req.Run.DurationMs == nil {
		t.Fatalf("run = %+v", finished.req.Run)
	}
	if finished.req.Run.Output != "" || finished.req.Run.Signal != "" {
		t.Fatalf("a successful run sends no output or signal: %+v", finished.req.Run)
	}
	if started.req.Run.DurationMs != nil || started.req.Run.ExitCode != nil {
		t.Fatalf("started carries finish fields: %+v", started.req.Run)
	}
	if started.policy.MaxAttempts != 1 || finished.policy.MaxAttempts != 3 {
		t.Fatalf("policies = %+v / %+v", started.policy, finished.policy)
	}
	if h.stdout.String() != "hello\n" {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
	if h.poster.url != "https://api.example.test" || h.poster.key.Reveal() != canary {
		t.Fatalf("poster url=%q", h.poster.url)
	}
}

func TestFailedJobSendsExitCodeAndMaskedTailAndPassesOutputThrough(t *testing.T) {
	script := "echo starting; sleep 0.1; echo 'connecting with password=hunter2 to db' >&2; sleep 0.1; echo user admin@example.com; exit 7"
	h := newHarness(t, join(flags("--name", "backup"), sh(script))...)
	if code := h.run(); code != 7 {
		t.Fatalf("exit = %d", code)
	}
	finished := h.poster.snapshot()[1].req.Run
	if finished.Type != client.RunFailed || *finished.ExitCode != 7 {
		t.Fatalf("run = %+v", finished)
	}
	for _, leak := range []string{"hunter2", "admin@example.com"} {
		if strings.Contains(finished.Output, leak) {
			t.Fatalf("tail leaks %q: %q", leak, finished.Output)
		}
	}
	if !strings.Contains(finished.Output, "password=***") || !strings.HasPrefix(finished.Output, "starting\n") {
		t.Fatalf("tail = %q", finished.Output)
	}
	if !strings.Contains(h.stdout.String(), "admin@example.com") || !strings.Contains(h.stderr.String(), "hunter2") {
		t.Fatalf("the job output must pass through unmasked: stdout=%q stderr=%q", h.stdout.String(), h.stderr.String())
	}
}

func TestOutputModes(t *testing.T) {
	tests := []struct {
		mode       string
		script     string
		wantOutput bool
	}{
		{"failure", "echo out; exit 0", false},
		{"failure", "echo out; exit 1", true},
		{"always", "echo out; exit 0", true},
		{"always", "echo out; exit 1", true},
		{"never", "echo out; exit 1", false},
		{"never", "echo out; exit 0", false},
	}
	for _, tt := range tests {
		t.Run(tt.mode+" "+tt.script, func(t *testing.T) {
			h := newHarness(t, join(flags("--name", "j", "--output", tt.mode), sh(tt.script))...)
			h.run()
			got := h.poster.snapshot()[1].req.Run.Output
			if (got != "") != tt.wantOutput || (tt.wantOutput && got != "out\n") {
				t.Fatalf("output = %q", got)
			}
		})
	}
}

func TestTailBytesLimitsTheOutput(t *testing.T) {
	script := "i=0; while [ $i -lt 2000 ]; do echo line-$i; i=$((i+1)); done; exit 1"
	for _, tt := range []struct {
		flag string
		want int
	}{{"100", 100}, {"1", 1}, {"", 4096}, {"8192", 8192}} {
		args := flags("--name", "j")
		if tt.flag != "" {
			args = append(args, "--tail-bytes", tt.flag)
		}
		h := newHarness(t, join(args, sh(script))...)
		h.run()
		out := h.poster.snapshot()[1].req.Run.Output
		if len(out) > tt.want || len(out) < tt.want-10 {
			t.Errorf("--tail-bytes %q: len = %d, want about %d", tt.flag, len(out), tt.want)
		}
		if tt.want >= 100 && !strings.HasSuffix(out, "line-1999\n") {
			t.Errorf("--tail-bytes %q: the tail must end with the last line: %q", tt.flag, out[max(0, len(out)-30):])
		}
	}
}

func TestTailBytesOutOfRangeIsClampedWithAWarning(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j", "--tail-bytes", "99999"), sh("yes 'ab cd' | head -c 20000; exit 1"))...)
	h.run()
	if got := len(h.poster.snapshot()[1].req.Run.Output); got != 8192 {
		t.Fatalf("len = %d", got)
	}
	if !strings.Contains(h.stderr.String(), "tail-bytes") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
}

func TestTailNeverSplitsACharacterAndIsMaskedAcrossTheCut(t *testing.T) {
	script := "printf '%s' 'token=abcdef0123456789abcdef0123456789abcdef '; printf '\\303\\244\\303\\244\\303\\244 end'; exit 1"
	h := newHarness(t, join(flags("--name", "j", "--tail-bytes", "40"), sh(script))...)
	h.run()
	out := h.poster.snapshot()[1].req.Run.Output
	if len(out) > 40 || strings.Contains(out, "abcdef0123") {
		t.Fatalf("output = %q", out)
	}
	if !strings.HasSuffix(out, "äää end") {
		t.Fatalf("output = %q", out)
	}
}

func TestSignalDeathIsReportedWithTheSignalName(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j"), sh("kill -KILL $$"))...)
	if code := h.run(); code != 137 {
		t.Fatalf("exit = %d", code)
	}
	run := h.poster.snapshot()[1].req.Run
	if run.Type != client.RunFailed || run.Signal != "SIGKILL" || *run.ExitCode != 137 {
		t.Fatalf("run = %+v", run)
	}
}

func TestCommandNotFoundIs127AndTheCommandTextIsNeverSent(t *testing.T) {
	h := newHarness(t, "--name", "j", "--", "secret-tool-name-xyz", "--token=topsecretvalue")
	if code := h.run(); code != 127 {
		t.Fatalf("exit = %d", code)
	}
	calls := h.poster.snapshot()
	if len(calls) != 2 || calls[1].req.Run.Type != client.RunFailed || *calls[1].req.Run.ExitCode != 127 {
		t.Fatalf("calls = %+v", calls)
	}
	for _, c := range calls {
		if body := fmt.Sprintf("%+v", c.req); strings.Contains(body, "secret-tool-name-xyz") || strings.Contains(body, "topsecretvalue") {
			t.Fatalf("the command leaked into the request: %s", body)
		}
	}
	if !strings.Contains(h.stderr.String(), "not found") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
}

func TestNetworkFailureNeverChangesTheExitCodeAndWarnsOncePerSixHours(t *testing.T) {
	failing := func(context.Context, client.Request) (client.Response, error) {
		return client.Response{}, errors.New("dial tcp: connection refused")
	}
	sharedCache := filepath.Join(t.TempDir(), "cache")
	var warnings []int
	for range 3 {
		h := newHarness(t, join(flags("--name", "j"), sh("exit 5"))...)
		h.vars["XDG_CACHE_HOME"] = sharedCache
		h.poster.onStart, h.poster.onFinish = failing, failing
		if code := h.run(); code != 5 {
			t.Fatalf("exit = %d", code)
		}
		warnings = append(warnings, strings.Count(h.stderr.String(), "cronbeat: warning"))
	}
	if warnings[0] != 1 || warnings[1] != 0 || warnings[2] != 0 {
		t.Fatalf("warning counts per run = %v, want [1 0 0]", warnings)
	}
}

func TestVerboseShowsDiagnosticsEveryTime(t *testing.T) {
	sharedCache := filepath.Join(t.TempDir(), "cache")
	for range 2 {
		h := newHarness(t, join(flags("--name", "j", "--verbose"), sh("exit 0"))...)
		h.vars["XDG_CACHE_HOME"] = sharedCache
		h.poster.onStart = func(context.Context, client.Request) (client.Response, error) {
			return client.Response{}, errors.New("connection refused")
		}
		h.run()
		out := h.stderr.String()
		for _, want := range []string{"api.example.test", "CRONBEAT_API_KEY", "started", "succeeded", "connection refused"} {
			if !strings.Contains(out, want) {
				t.Fatalf("verbose output lacks %q:\n%s", want, out)
			}
		}
	}
}

func TestStatusErrorsProduceHelpfulWarnings(t *testing.T) {
	tests := []struct {
		status int
		detail string
		want   string
	}{
		{401, "", "cronbeat check"},
		{403, "plan_limit_exceeded: monitor limit reached", "plan_limit_exceeded"},
		{400, "schedule is invalid", "schedule is invalid"},
		{429, "", "rate"},
		{503, "", "503"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			h := newHarness(t, join(flags("--name", "j"), sh("exit 3"))...)
			fail := func(context.Context, client.Request) (client.Response, error) {
				return client.Response{}, &client.StatusError{Status: tt.status, Detail: tt.detail}
			}
			h.poster.onStart, h.poster.onFinish = fail, fail
			if code := h.run(); code != 3 {
				t.Fatalf("exit = %d", code)
			}
			if !strings.Contains(h.stderr.String(), tt.want) || strings.Count(h.stderr.String(), "cronbeat: warning") != 1 {
				t.Fatalf("stderr = %q", h.stderr.String())
			}
		})
	}
}

func TestServerWarningsAreShown(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j"), sh("exit 0"))...)
	h.poster.onFinish = func(context.Context, client.Request) (client.Response, error) {
		return client.Response{Warnings: []string{"grace is capped at 3600"}}, nil
	}
	h.run()
	if !strings.Contains(h.stderr.String(), "grace is capped at 3600") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
}

func TestMissingKeyRunsTheJobWithoutReporting(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j"), sh("echo ran; exit 4"))...)
	delete(h.vars, "CRONBEAT_API_KEY")
	if code := h.run(); code != 4 {
		t.Fatalf("exit = %d", code)
	}
	if len(h.poster.snapshot()) != 0 || h.stdout.String() != "ran\n" {
		t.Fatalf("calls=%d stdout=%q", len(h.poster.snapshot()), h.stdout.String())
	}
	if !strings.Contains(h.stderr.String(), "cronbeat configure") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
}

func TestKeyFilesAreUsedAndLooseOnesAreRefused(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, join(flags("--name", "j", "--key-file", keyPath), sh("exit 0"))...)
	delete(h.vars, "CRONBEAT_API_KEY")
	if code := h.run(); code != 0 || h.poster.key.Reveal() != canary {
		t.Fatalf("exit=%d key set=%v stderr=%q", code, h.poster.key != "", h.stderr.String())
	}

	if err := os.Chmod(keyPath, 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	h2 := newHarness(t, join(flags("--name", "j", "--key-file", keyPath), sh("exit 2"))...)
	delete(h2.vars, "CRONBEAT_API_KEY")
	if code := h2.run(); code != 2 || len(h2.poster.snapshot()) != 0 {
		t.Fatalf("exit=%d calls=%d", code, len(h2.poster.snapshot()))
	}
	if !strings.Contains(h2.stderr.String(), "0600") || strings.Contains(h2.stderr.String(), canary) {
		t.Fatalf("stderr = %q", h2.stderr.String())
	}
}

func TestInsecureOrInvalidAPIURLRunsTheJobWithoutReporting(t *testing.T) {
	for _, url := range []string{"http://api.example.test", "ftp://x", "not a url", "https://user:pw@api.example.test"} {
		h := newHarness(t, join(flags("--name", "j", "--api-url", url), sh("exit 6"))...)
		if code := h.run(); code != 6 || len(h.poster.snapshot()) != 0 {
			t.Fatalf("%s: exit=%d calls=%d", url, code, len(h.poster.snapshot()))
		}
		if !strings.Contains(h.stderr.String(), "API URL") || strings.Contains(h.stderr.String(), "pw") {
			t.Fatalf("%s: stderr = %q", url, h.stderr.String())
		}
	}
}

func TestBadFlagsStillRunTheCommand(t *testing.T) {
	h := newHarness(t, join(flags("--no-such-flag", "--name", "j"), sh("echo ran; exit 9"))...)
	if code := h.run(); code != 9 || h.stdout.String() != "ran\n" || len(h.poster.snapshot()) != 0 {
		t.Fatalf("exit=%d stdout=%q calls=%d", code, h.stdout.String(), len(h.poster.snapshot()))
	}
	if !strings.Contains(h.stderr.String(), "no-such-flag") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
}

func TestMissingOrInvalidNameRunsTheJobWithoutReporting(t *testing.T) {
	for _, name := range [][]string{nil, {"--name", ""}, {"--name", "has space"}, {"--name", "semi;colon"}, {"--name", strings.Repeat("a", 65)}} {
		h := newHarness(t, join(name, sh("exit 8"))...)
		if code := h.run(); code != 8 || len(h.poster.snapshot()) != 0 {
			t.Fatalf("%v: exit=%d calls=%d", name, code, len(h.poster.snapshot()))
		}
		if !strings.Contains(h.stderr.String(), "--name") {
			t.Fatalf("%v: stderr = %q", name, h.stderr.String())
		}
	}
}

func TestMissingCommandIsAUsageError(t *testing.T) {
	for _, args := range [][]string{{"--name", "j"}, {"--name", "j", "--"}, nil} {
		h := newHarness(t, args...)
		if code := h.run(); code != 64 || len(h.poster.snapshot()) != 0 || h.stderr.Len() == 0 {
			t.Fatalf("%v: exit=%d", args, code)
		}
	}
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func TestFlagsAfterTheSeparatorBelongToTheCommand(t *testing.T) {
	h := newHarness(t, "--name", "j", "--", "/bin/sh", "-c", `printf '%s|' "$@"`, "sh", "--name", "other", "--verbose")
	if code := h.run(); code != 0 || h.stdout.String() != "--name|other|--verbose|" {
		t.Fatalf("exit=%d stdout=%q", code, h.stdout.String())
	}
	if h.poster.snapshot()[0].req.Job.Name != "j" {
		t.Fatalf("job = %+v", h.poster.snapshot()[0].req.Job)
	}
}

func TestSeparatorIsOptionalWhenTheCommandStartsWithANonFlag(t *testing.T) {
	h := newHarness(t, "--name", "j", "/bin/sh", "-c", "exit 3")
	if code := h.run(); code != 3 || len(h.poster.snapshot()) != 2 {
		t.Fatalf("exit=%d calls=%d", code, len(h.poster.snapshot()))
	}
}

func TestJobDefinitionFlags(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j", "--schedule", "@daily", "--timezone", "Europe/Istanbul",
		"--grace", "5m", "--max-runtime", "3600"), sh("exit 0"))...)
	h.run()
	job := h.poster.snapshot()[0].req.Job
	if job.Schedule != "0 0 * * *" || job.TimeZone != "Europe/Istanbul" || job.GraceSeconds == nil || *job.GraceSeconds != 300 ||
		job.MaxRuntimeSeconds == nil || *job.MaxRuntimeSeconds != 3600 {
		t.Fatalf("job = %+v", job)
	}
}

func TestDurationFlagForms(t *testing.T) {
	for in, want := range map[string]int{"300": 300, "5m": 300, "1h": 3600, "90s": 90, "1h30m": 5400, "1500ms": 2} {
		h := newHarness(t, join(flags("--name", "j", "--grace", in), sh("exit 0"))...)
		h.run()
		got := h.poster.snapshot()[0].req.Job.GraceSeconds
		if got == nil || *got != want {
			t.Errorf("--grace %s = %v, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"0", "-5", "abc", "1x", "99999999999999999999"} {
		h := newHarness(t, join(flags("--name", "j", "--grace", bad), sh("exit 0"))...)
		if code := h.run(); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		calls := h.poster.snapshot()
		if len(calls) != 2 || calls[0].req.Job.GraceSeconds != nil || !strings.Contains(h.stderr.String(), "--grace") {
			t.Errorf("--grace %s: grace=%v stderr=%q", bad, calls[0].req.Job.GraceSeconds, h.stderr.String())
		}
	}
}

func TestInvalidScheduleIsDroppedWithAWarningButReportingContinues(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j", "--schedule", "every day"), sh("exit 0"))...)
	h.run()
	calls := h.poster.snapshot()
	if len(calls) != 2 || calls[0].req.Job.Schedule != "" || !strings.Contains(h.stderr.String(), "schedule") {
		t.Fatalf("calls=%d schedule=%q stderr=%q", len(calls), calls[0].req.Job.Schedule, h.stderr.String())
	}
}

func TestTimeZoneIsDetectedOnlyWithASchedule(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j", "--schedule", "0 2 * * *"), sh("exit 0"))...)
	h.env.TimeZone = func() string { return "Europe/Berlin" }
	h.run()
	if got := h.poster.snapshot()[0].req.Job.TimeZone; got != "Europe/Berlin" {
		t.Fatalf("detected zone = %q", got)
	}
	h = newHarness(t, join(flags("--name", "j"), sh("exit 0"))...)
	h.env.TimeZone = func() string { return "Europe/Berlin" }
	h.run()
	if got := h.poster.snapshot()[0].req.Job.TimeZone; got != "" {
		t.Fatalf("zone without schedule = %q", got)
	}
	h = newHarness(t, join(flags("--name", "j", "--schedule", "0 2 * * *", "--timezone", "UTC"), sh("exit 0"))...)
	h.env.TimeZone = func() string { return "Europe/Berlin" }
	h.run()
	if got := h.poster.snapshot()[0].req.Job.TimeZone; got != "UTC" {
		t.Fatalf("explicit zone = %q", got)
	}
}

func TestSlowStartedEventDoesNotDelayTheJobAndFinishWaitsForIt(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j"), sh("echo ready; exit 0"))...)
	release := make(chan struct{})
	var startedDone, finishSawStartedDone bool
	var mu sync.Mutex
	h.poster.onStart = func(ctx context.Context, _ client.Request) (client.Response, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		startedDone = true
		mu.Unlock()
		return client.Response{}, nil
	}
	h.poster.onFinish = func(context.Context, client.Request) (client.Response, error) {
		mu.Lock()
		finishSawStartedDone = startedDone
		mu.Unlock()
		return client.Response{}, nil
	}
	stdout := &arrivalBuffer{}
	h.env.Stdout = stdout
	begin := time.Now()
	done := make(chan int, 1)
	go func() { done <- h.run() }()
	deadline := time.After(3 * time.Second)
	for stdout.firstWrite().IsZero() {
		select {
		case <-deadline:
			t.Fatal("the job never produced output while the started event was pending")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if delay := stdout.firstWrite().Sub(begin); delay > 500*time.Millisecond {
		t.Fatalf("the job started %v after cronbeat, the started event must not delay it", delay)
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("cronbeat exited before the started event finished")
	default:
	}
	close(release)
	if code := <-done; code != 0 {
		t.Fatalf("exit = %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if !finishSawStartedDone {
		t.Fatal("the finish event was sent before the started event completed")
	}
}

type arrivalBuffer struct {
	mu    sync.Mutex
	first time.Time
}

func (a *arrivalBuffer) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.first.IsZero() {
		a.first = time.Now()
	}
	return len(p), nil
}

func (a *arrivalBuffer) firstWrite() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.first
}

func TestAHungPosterCannotHoldTheJobBeyondTheWaitBounds(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j"), sh("exit 2"))...)
	hang := func(context.Context, client.Request) (client.Response, error) {
		select {}
	}
	h.poster.onStart, h.poster.onFinish = hang, hang
	h.env.StartedWait, h.env.FinishWait = 200*time.Millisecond, 200*time.Millisecond
	begin := time.Now()
	if code := h.run(); code != 2 {
		t.Fatalf("exit = %d", code)
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

func TestAPanicInTheClientNeverAffectsTheJob(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j"), sh("echo ran; exit 11"))...)
	boom := func(context.Context, client.Request) (client.Response, error) { panic("boom") }
	h.poster.onStart, h.poster.onFinish = boom, boom
	if code := h.run(); code != 11 || h.stdout.String() != "ran\n" {
		t.Fatalf("exit=%d stdout=%q", code, h.stdout.String())
	}
}

func TestAPanicWhileSettingUpStillRunsTheCommand(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j"), sh("echo ran; exit 12"))...)
	h.env.NewPoster = func(string, config.Secret) Poster { panic("setup boom") }
	if code := h.run(); code != 12 || h.stdout.String() != "ran\n" {
		t.Fatalf("exit=%d stdout=%q", code, h.stdout.String())
	}
}

func TestRunIDFailureOnlyDisablesReporting(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j"), sh("exit 13"))...)
	h.env.NewRunID = func() (string, error) { return "", errors.New("no entropy") }
	if code := h.run(); code != 13 || len(h.poster.snapshot()) != 0 {
		t.Fatalf("exit=%d calls=%d", code, len(h.poster.snapshot()))
	}
}

func TestTheKeyNeverAppearsInAnyOutput(t *testing.T) {
	echo := func(context.Context, client.Request) (client.Response, error) {
		return client.Response{Warnings: []string{"key " + canary + " is old"}},
			&client.StatusError{Status: 403, Detail: "key " + canary + " refused"}
	}
	for _, verbose := range []bool{false, true} {
		args := flags("--name", "j")
		if verbose {
			args = append(args, "--verbose")
		}
		h := newHarness(t, join(args, sh("echo $CRONBEAT_API_KEY >/dev/null; echo job-output; exit 1"))...)
		h.poster.onStart, h.poster.onFinish = echo, echo
		h.run()
		if strings.Contains(h.stdout.String()+h.stderr.String(), canary) {
			t.Fatalf("verbose=%v: key printed:\nstdout=%q\nstderr=%q", verbose, h.stdout.String(), h.stderr.String())
		}
		for _, c := range h.poster.snapshot() {
			if strings.Contains(fmt.Sprintf("%+v", c.req), canary) {
				t.Fatalf("key in request body")
			}
		}
	}
}

func TestTheKeyIsMaskedWhenTheJobPrintsIt(t *testing.T) {
	h := newHarness(t, join(flags("--name", "j"), sh(`printf 'using %s now\n' "$CRONBEAT_API_KEY"; exit 1`))...)
	h.vars["CRONBEAT_API_KEY"] = "short-key-9"
	h.run()
	if out := h.poster.snapshot()[1].req.Run.Output; strings.Contains(out, "short-key-9") {
		t.Fatalf("tail contains the literal key: %q", out)
	}
}

func TestStdinPassesThroughToTheCommand(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	go func() { _, _ = w.WriteString("from stdin\n"); _ = w.Close() }()
	h := newHarness(t, "--name", "j", "--", "cat")
	h.env.Stdin = r
	if code := h.run(); code != 0 || h.stdout.String() != "from stdin\n" {
		t.Fatalf("exit=%d stdout=%q", code, h.stdout.String())
	}
}

func TestEnvironmentOfTheCommandIsNotModified(t *testing.T) {
	t.Setenv("CRONBEAT_RUN_TEST", "visible")
	h := newHarness(t, join(flags("--name", "j"), sh(`printf %s "$CRONBEAT_RUN_TEST"`))...)
	h.run()
	if h.stdout.String() != "visible" {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewRunIDIsARandomUUIDv4(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id, err := NewRunID()
		if err != nil || !uuidV4.MatchString(id) {
			t.Fatalf("id = %q, %v", id, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestHelpFlagPrintsUsageAndDoesNotRun(t *testing.T) {
	h := newHarness(t, "--help")
	if code := h.run(); code != 0 || !strings.Contains(h.stdout.String()+h.stderr.String(), "usage") {
		t.Fatalf("exit=%d out=%q err=%q", code, h.stdout.String(), h.stderr.String())
	}
}

var _ io.Writer = (*syncBuffer)(nil)

func TestTheAPIKeyIsNotPassedOnToTheJobNorSentInTheTail(t *testing.T) {
	t.Setenv("CRONBEAT_API_KEY", canary)
	h := newHarness(t, join(flags("--name", "j"), sh("env; exit 1"))...)
	h.run()
	if strings.Contains(h.stdout.String(), canary) || strings.Contains(h.stdout.String(), "CRONBEAT_API_KEY") {
		t.Fatal("the job saw the API key")
	}
	for _, c := range h.poster.snapshot() {
		if strings.Contains(fmt.Sprintf("%+v", c.req), canary) {
			t.Fatal("the API key was sent in a request")
		}
	}
	if strings.Contains(h.stderr.String(), canary) {
		t.Fatal("the API key appeared in a warning")
	}
}
