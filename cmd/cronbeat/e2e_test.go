//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostversion"
)

const canary = "cb_agent_CANARYSECRETVALUE123456"

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cronbeat-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "cronbeat")
	goTool, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "the go tool is needed to build the cronbeat binary:", err)
		os.Exit(1)
	}
	build := exec.CommandContext(context.Background(), goTool, "build", "-trimpath", "-o", binary, ".") //nolint:gosec // builds the package under test
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the cronbeat binary failed: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type received struct {
	path   string
	auth   string
	ctype  string
	body   []byte
	parsed map[string]any
}

type fakeAPI struct {
	srv       *httptest.Server
	mu        sync.Mutex
	requests  []received
	runStatus int
	runBody   string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	return newFakeAPIAnswering(t, http.StatusAccepted, `{"monitorId":"m-1","created":true,"warnings":[]}`)
}

func newFakeAPIAnswering(t *testing.T, status int, body string) *fakeAPI {
	t.Helper()
	api := &fakeAPI{runStatus: status, runBody: body}
	api.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rec := received{path: r.URL.Path, auth: r.Header.Get("Authorization"), ctype: r.Header.Get("Content-Type"), body: body}
		_ = json.Unmarshal(body, &rec.parsed)
		api.mu.Lock()
		api.requests = append(api.requests, rec)
		status, resp := api.runStatus, api.runBody
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/agent/v1/host/check" && rec.auth == "Bearer "+canary:
			_, _ = io.WriteString(w, `{"name":"prod-db-1","kind":"host","version":1}`)
		case r.URL.Path == "/agent/v1/host/check":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"title":"Unauthorized","detail":"invalid key"}`)
		case r.URL.Path == "/agent/v1/host/runs":
			w.WriteHeader(status)
			_, _ = io.WriteString(w, resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.srv.Close)
	return api
}

func (a *fakeAPI) runs() []received {
	a.mu.Lock()
	defer a.mu.Unlock()
	var runs []received
	for _, r := range a.requests {
		if r.path == "/agent/v1/host/runs" {
			runs = append(runs, r)
		}
	}
	return runs
}

type outcome struct {
	code    int
	stdout  string
	stderr  string
	elapsed time.Duration
}

func baseEnv(t *testing.T, apiURL string) []string {
	t.Helper()
	home := t.TempDir()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"CRONBEAT_API_URL=" + apiURL,
		"CRONBEAT_API_KEY=" + canary,
	}
}

func without(env []string, names ...string) []string {
	var kept []string
next:
	for _, kv := range env {
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				continue next
			}
		}
		kept = append(kept, kv)
	}
	return kept
}

func cronbeatCommand(env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(context.Background(), binary, args...) //nolint:gosec // runs the binary the tests just built
	cmd.Env = env
	return cmd
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func runCronbeat(t *testing.T, env []string, stdin io.Reader, args ...string) outcome {
	t.Helper()
	cmd := cronbeatCommand(env, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	out := outcome{stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start)}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		out.code = exit.ExitCode()
	default:
		t.Fatalf("running cronbeat: %v", err)
	}
	if strings.Contains(out.stdout+out.stderr, canary) {
		t.Fatalf("the API key appeared in the output:\nstdout=%q\nstderr=%q", out.stdout, out.stderr)
	}
	return out
}

func sh(script string) []string { return []string{"--", "/bin/sh", "-c", script} }

func args(parts ...[]string) []string {
	var all []string
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

func f(v ...string) []string { return v }

func TestExitCodesAndOutputPassThroughUnchanged(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	tests := []struct {
		name   string
		script string
		code   int
		stdout string
		stderr string
	}{
		{"success", "echo out; echo err >&2", 0, "out\n", "err\n"},
		{"seven", "echo partial; exit 7", 7, "partial\n", ""},
		{"max", "exit 255", 255, "", ""},
		{"killed", "kill -KILL $$", 137, "", ""},
		{"terminated", "kill -TERM $$", 143, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runCronbeat(t, baseEnv(t, api.srv.URL), nil, args(f("run", "--name", "e2e-"+tt.name), sh(tt.script))...)
			if out.code != tt.code || out.stdout != tt.stdout || out.stderr != tt.stderr {
				t.Fatalf("code=%d stdout=%q stderr=%q", out.code, out.stdout, out.stderr)
			}
		})
	}
}

func TestExecFailuresAre127And126(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	notExecutable := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(notExecutable, []byte("x"), 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	out := runCronbeat(t, baseEnv(t, api.srv.URL), nil, "run", "--name", "missing", "--", "no-such-command-anywhere")
	if out.code != 127 || !strings.Contains(out.stderr, "not found") {
		t.Fatalf("not found: code=%d stderr=%q", out.code, out.stderr)
	}
	out = runCronbeat(t, baseEnv(t, api.srv.URL), nil, "run", "--name", "noexec", "--", notExecutable)
	if out.code != 126 {
		t.Fatalf("not executable: code=%d stderr=%q", out.code, out.stderr)
	}
	for _, r := range api.runs() {
		if strings.Contains(string(r.body), "no-such-command-anywhere") || strings.Contains(string(r.body), notExecutable) {
			t.Fatalf("the command was sent: %s", r.body)
		}
	}
}

func TestStdinReachesTheCommand(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	out := runCronbeat(t, baseEnv(t, api.srv.URL), strings.NewReader("piped input\n"), "run", "--name", "stdin", "--", "cat")
	if out.code != 0 || out.stdout != "piped input\n" {
		t.Fatalf("code=%d stdout=%q", out.code, out.stdout)
	}
}

func TestWireRequestsMatchTheContractGoldenFile(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "host_runs_contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	golden := map[string]map[string]any{}
	for name, entry := range raw {
		if name == "_comment" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(entry, &body); err != nil {
			t.Fatal(err)
		}
		golden[name] = body
	}
	job := f("--name", "nightly-backup", "--schedule", "0 2 * * *", "--timezone", "Europe/Istanbul", "--grace", "5m", "--max-runtime", "1h")
	scripts := map[string]string{
		"succeeded":        "exit 0",
		"failed":           "echo 'backup failed: disk full'; exit 2",
		"failed_by_signal": "kill -KILL $$",
	}
	for name, script := range scripts {
		before := len(api.runs())
		out := runCronbeat(t, baseEnv(t, api.srv.URL), nil, args(f("run"), job, sh(script))...)
		if out.stderr != "" && name == "succeeded" {
			t.Fatalf("%s: stderr = %q", name, out.stderr)
		}
		runs := api.runs()[before:]
		if len(runs) != 2 {
			t.Fatalf("%s: %d requests", name, len(runs))
		}
		for i, wantType := range []string{"started", name} {
			if wantType == "failed_by_signal" {
				wantType = "failed"
			}
			got := normalise(t, runs[i].parsed)
			want := golden[wantType]
			if i == 1 {
				want = golden[name]
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s request %d differs from the golden file\n got  %v\n want %v", name, i, got, want)
			}
			if runs[i].auth != "Bearer "+canary || runs[i].ctype != "application/json" {
				t.Fatalf("headers: auth set=%v type=%q", runs[i].auth != "", runs[i].ctype)
			}
		}
		if runs[0].parsed["run"].(map[string]any)["id"] != runs[1].parsed["run"].(map[string]any)["id"] {
			t.Fatalf("%s: run ids differ between started and the result", name)
		}
	}
}

func normalise(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	clone := map[string]any{}
	for k, v := range body {
		clone[k] = v
	}
	run := map[string]any{}
	for k, v := range clone["run"].(map[string]any) {
		run[k] = v
	}
	if id, _ := run["id"].(string); len(id) != 36 || id[14] != '4' {
		t.Fatalf("run id %q is not a UUID v4", id)
	}
	run["id"] = "3f2b8c1e-5a4d-4e6f-8b7a-1c2d3e4f5a6b"
	if _, ok := run["durationMs"]; ok {
		run["durationMs"] = float64(1234)
	}
	clone["run"] = run
	if clone["agentVersion"] != hostversion.Version {
		t.Fatalf("agentVersion = %v", clone["agentVersion"])
	}
	clone["agentVersion"] = "0.1.0"
	return clone
}

func TestFailureTailIsMaskedAndBounded(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	script := `echo "login with password=hunter2 as ops@example.com"; i=0; while [ $i -lt 3000 ]; do echo "line $i"; i=$((i+1)); done; exit 1`
	out := runCronbeat(t, baseEnv(t, api.srv.URL), nil, args(f("run", "--name", "tail", "--tail-bytes", "512"), sh(script))...)
	if out.code != 1 || !strings.Contains(out.stdout, "hunter2") {
		t.Fatalf("code=%d: the job output must pass through unmasked", out.code)
	}
	runs := api.runs()
	output, _ := runs[1].parsed["run"].(map[string]any)["output"].(string)
	if len(output) == 0 || len(output) > 512 || !strings.HasSuffix(output, "line 2999\n") {
		t.Fatalf("output len=%d tail=%q", len(output), output[max(0, len(output)-20):])
	}
	if strings.Contains(string(runs[1].body), "hunter2") {
		t.Fatal("secret in the request")
	}
}

func TestUnreachableServerDoesNotDelayTheJobAndKeepsTheExitCode(t *testing.T) {
	t.Parallel()
	l := listen(t)
	url := "http://" + l.Addr().String()
	_ = l.Close()
	out := runCronbeat(t, baseEnv(t, url), nil, args(f("run", "--name", "offline"), sh("echo done; exit 3"))...)
	if out.code != 3 || out.stdout != "done\n" {
		t.Fatalf("code=%d stdout=%q", out.code, out.stdout)
	}
	if out.elapsed > 6*time.Second {
		t.Fatalf("took %v with CronBeat unreachable", out.elapsed)
	}
	if !strings.Contains(out.stderr, "could not reach CronBeat") {
		t.Fatalf("stderr = %q", out.stderr)
	}
	again := runCronbeat(t, append(without(baseEnv(t, url), "XDG_CACHE_HOME", "HOME"), "HOME="+t.TempDir()), nil, args(f("run", "--name", "offline"), sh("exit 3"))...)
	if again.code != 3 {
		t.Fatalf("code = %d", again.code)
	}
}

func TestWarningIsShownOnceThenSuppressed(t *testing.T) {
	t.Parallel()
	l := listen(t)
	url := "http://" + l.Addr().String()
	_ = l.Close()
	env := baseEnv(t, url)
	first := runCronbeat(t, env, nil, args(f("run", "--name", "offline"), sh("exit 0"))...)
	second := runCronbeat(t, env, nil, args(f("run", "--name", "offline"), sh("exit 0"))...)
	verbose := runCronbeat(t, env, nil, args(f("run", "--name", "offline", "--verbose"), sh("exit 0"))...)
	if !strings.Contains(first.stderr, "warning") || second.stderr != "" || !strings.Contains(verbose.stderr, "started event failed") {
		t.Fatalf("first=%q second=%q verbose=%q", first.stderr, second.stderr, verbose.stderr)
	}
}

func TestHangingServerNeverDelaysTheJobAndStaysWithinTheBudgets(t *testing.T) {
	if testing.Short() {
		t.Skip("takes up to 20 seconds")
	}
	t.Parallel()
	l := listen(t)
	t.Cleanup(func() { _ = l.Close() })
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	cmd := cronbeatCommand(baseEnv(t, "http://"+l.Addr().String()), args(f("run", "--name", "hang"), sh("echo started-job; sleep 0.3; exit 4"))...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if firstOutput := time.Since(begin); firstOutput > time.Second {
		t.Fatalf("the job's first output came after %v, the hanging server delayed the job", firstOutput)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 4 {
		t.Fatalf("exit = %v", err)
	}
	if total := time.Since(begin); total > 20*time.Second {
		t.Fatalf("total %v exceeds started (4s) + finish (12s) budgets", total)
	}
}

func TestMissingKeyRunsTheJobAndExplains(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	env := without(baseEnv(t, api.srv.URL), "CRONBEAT_API_KEY")
	out := runCronbeat(t, env, nil, args(f("run", "--name", "nokey"), sh("echo ran; exit 9"))...)
	if out.code != 9 || out.stdout != "ran\n" || !strings.Contains(out.stderr, "cronbeat configure") {
		t.Fatalf("code=%d stdout=%q stderr=%q", out.code, out.stdout, out.stderr)
	}
	if len(api.runs()) != 0 {
		t.Fatal("requests were sent without a key")
	}
}

func TestKeyFilePermissionsAreEnforced(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	env := without(baseEnv(t, api.srv.URL), "CRONBEAT_API_KEY")
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runCronbeat(t, env, nil, args(f("run", "--name", "kf", "--key-file", keyFile), sh("exit 0"))...)
	if out.code != 0 || len(api.runs()) != 2 {
		t.Fatalf("code=%d requests=%d stderr=%q", out.code, len(api.runs()), out.stderr)
	}
	if err := os.Chmod(keyFile, 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	before := len(api.runs())
	out = runCronbeat(t, env, nil, args(f("run", "--name", "kf", "--key-file", keyFile), sh("exit 5"))...)
	if out.code != 5 || len(api.runs()) != before || !strings.Contains(out.stderr, "0600") {
		t.Fatalf("code=%d requests=%d stderr=%q", out.code, len(api.runs())-before, out.stderr)
	}
}

func TestPlainHTTPToARemoteHostIsRefused(t *testing.T) {
	t.Parallel()
	out := runCronbeat(t, baseEnv(t, "http://example.com"), nil, args(f("run", "--name", "x"), sh("exit 2"))...)
	if out.code != 2 || !strings.Contains(out.stderr, "https") {
		t.Fatalf("code=%d stderr=%q", out.code, out.stderr)
	}
}

func TestServerErrorsNeverChangeTheExitCode(t *testing.T) {
	t.Parallel()
	for _, status := range []int{400, 401, 403, 413, 429, 500, 503} {
		api := newFakeAPIAnswering(t, status, `{"title":"x","detail":"key `+canary+` is not allowed"}`)
		out := runCronbeat(t, baseEnv(t, api.srv.URL), nil, args(f("run", "--name", "errs"), sh("exit 6"))...)
		if out.code != 6 {
			t.Fatalf("status %d: code = %d", status, out.code)
		}
		if out.elapsed > 8*time.Second {
			t.Fatalf("status %d: took %v", status, out.elapsed)
		}
	}
}

func TestSIGTERMReachesTheJobAndItsChildren(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := fmt.Sprintf("sleep 60 & echo $! > %s; echo ready; wait", pidFile)
	cmd := cronbeatCommand(baseEnv(t, api.srv.URL), args(f("run", "--name", "sig"), sh(script))...)
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 143 {
		t.Fatalf("exit = %v", err)
	}
	raw, _ := os.ReadFile(pidFile) //nolint:gosec // test fixture needs this file mode or path
	var pid int
	if _, err := fmt.Sscan(string(raw), &pid); err != nil {
		t.Fatalf("pid file %q: %v", raw, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatal("the grandchild survived SIGTERM")
		}
		time.Sleep(20 * time.Millisecond)
	}
	runs := api.runs()
	last := runs[len(runs)-1].parsed["run"].(map[string]any)
	if last["type"] != "failed" || last["signal"] != "SIGTERM" {
		t.Fatalf("last run = %v", last)
	}
}

func TestAClosedStdoutDoesNotKillTheJobOrChangeItsExitCode(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	cmd := cronbeatCommand(baseEnv(t, api.srv.URL), args(f("run", "--name", "pipe"), sh("i=0; while [ $i -lt 50 ]; do echo line$i; sleep 0.01; i=$((i+1)); done; exit 7"))...)
	cmd.Stdout = w
	err = cmd.Run()
	_ = w.Close()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("exit = %v, want code 7", err)
	}
}

func TestVersionCheckAndConfigureWorkTogether(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root stores the key in /etc/cronbeat/key instead of the home directory")
	}
	t.Parallel()
	api := newFakeAPI(t)
	home := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CACHE_HOME=" + home, "CRONBEAT_API_URL=" + api.srv.URL}

	if out := runCronbeat(t, env, nil, "version"); out.code != 0 || !strings.HasPrefix(out.stdout, "cronbeat "+hostversion.Version) {
		t.Fatalf("version: %+v", out)
	}
	if out := runCronbeat(t, env, nil, "check"); out.code != 1 || !strings.Contains(out.stderr, "cronbeat configure") {
		t.Fatalf("check without key: %+v", out)
	}
	if out := runCronbeat(t, env, strings.NewReader(canary+"\n"), "configure", "--stdin"); out.code != 0 {
		t.Fatalf("configure: %+v", out)
	}
	info, err := os.Stat(filepath.Join(home, ".config", "cronbeat", "key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}
	out := runCronbeat(t, env, nil, "check")
	if out.code != 0 || !strings.Contains(out.stdout, "prod-db-1") {
		t.Fatalf("check: %+v", out)
	}
	runOut := runCronbeat(t, env, nil, args(f("run", "--name", "from-file"), sh("exit 0"))...)
	if runOut.code != 0 || len(api.runs()) != 2 {
		t.Fatalf("run with the configured key: %+v requests=%d", runOut, len(api.runs()))
	}
	bad := append(without(env, "HOME"), "HOME="+t.TempDir(), "CRONBEAT_API_KEY=cb_agent_wrong")
	if out := runCronbeat(t, bad, nil, "check"); out.code != 1 || !strings.Contains(out.stderr, "rejected") {
		t.Fatalf("check with a wrong key: %+v", out)
	}
}

func TestConfigureRefusesToReadAKeyFromANonTerminalWithoutTheFlag(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	out := runCronbeat(t, env, strings.NewReader(canary+"\n"), "configure")
	if out.code == 0 {
		t.Fatalf("configure succeeded: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "cronbeat", "key")); err == nil {
		t.Fatal("a key file was written")
	}
}

func TestCrontabPlanApplyUndoOnAFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state")}
	file := filepath.Join(home, "crontab")
	original := "MAILTO=ops@example.com\n0 2 * * * /opt/backup.sh --full\n@reboot /opt/boot.sh\n0 3 * * * date +%F\n"
	if err := os.WriteFile(file, []byte(original), 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	plan := runCronbeat(t, env, nil, "crontab", "plan", "--file", file)
	if plan.code != 0 || !strings.Contains(plan.stdout, `wrap as "backup"`) || !strings.Contains(plan.stdout, "@reboot") {
		t.Fatalf("plan: %+v", plan)
	}
	if data, _ := os.ReadFile(file); string(data) != original { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal("plan changed the file")
	}
	if out := runCronbeat(t, env, nil, "crontab", "apply", "--file", file); out.code != 0 {
		t.Fatalf("apply: %+v", out)
	}
	applied, _ := os.ReadFile(file) //nolint:gosec // test fixture needs this file mode or path
	if !strings.Contains(string(applied), " run --name backup --schedule '0 2 * * *' -- /bin/sh -c '/opt/backup.sh --full'") ||
		!strings.Contains(string(applied), "@reboot /opt/boot.sh\n") || !strings.Contains(string(applied), "date +%F\n") {
		t.Fatalf("applied = %q", applied)
	}
	if !strings.Contains(string(applied), binary) {
		t.Fatalf("the absolute path of the binary is missing: %q", applied)
	}
	if out := runCronbeat(t, env, nil, "crontab", "undo", "--file", file); out.code != 0 {
		t.Fatalf("undo: %+v", out)
	}
	if data, _ := os.ReadFile(file); string(data) != original { //nolint:gosec // test fixture needs this file mode or path
		t.Fatalf("after undo = %q", data)
	}
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	for _, a := range [][]string{nil, {"nonsense"}, {"run"}, {"run", "--name", "x"}, {"run", "--name", "x", "--"}} {
		if out := runCronbeat(t, env, nil, a...); out.code != 64 {
			t.Errorf("%v: code = %d, stderr %q", a, out.code, out.stderr)
		}
	}
}

func TestTheJobDoesNotSeeTheAPIKey(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	env := append(baseEnv(t, api.srv.URL), "CRONBEAT_KEY_FILE=/nonexistent/key")
	out := runCronbeat(t, env, nil, args(f("run", "--name", "envdump"), sh("env; exit 1"))...)
	if strings.Contains(out.stdout, "CRONBEAT_API_KEY") || !strings.Contains(out.stdout, "CRONBEAT_API_URL=") {
		t.Fatal("the job environment must lack the key and keep the other variables")
	}
	for _, r := range api.runs() {
		if strings.Contains(string(r.body), canary) || strings.Contains(string(r.body), "CRONBEAT_API_KEY") {
			t.Fatal("the key reached the server in the captured tail")
		}
	}
}
