//go:build unix

package crontab

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cmdEnv struct {
	Env
	stdout, stderr *bytes.Buffer
	home           string
}

func newCmdEnv(t *testing.T, args ...string) *cmdEnv {
	t.Helper()
	home := t.TempDir()
	c := &cmdEnv{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, home: home}
	c.Env = Env{
		Args:       args,
		Stdout:     c.stdout,
		Stderr:     c.stderr,
		Getenv:     func(string) string { return "" },
		Home:       home,
		Executable: func() (string, error) { return bin, nil },
		Now:        func() time.Time { return time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC) },
	}
	return c
}

func writeCrontab(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crontab")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	return path
}

func TestPlanPrintsTheReportAndChangesNothing(t *testing.T) {
	path := writeCrontab(t, original)
	c := newCmdEnv(t, "plan", "--file", path)
	if code := Main(c.Env); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, c.stderr.String())
	}
	out := c.stdout.String()
	for _, want := range []string{`line 2: wrap as "backup"`, "1 wrapped", "cronbeat crontab apply", "cronbeat configure", "CRONBEAT_API_KEY"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output lacks %q:\n%s", want, out)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != original { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal("plan modified the file")
	}
	if _, err := os.Stat(filepath.Join(c.home, ".local")); err == nil {
		t.Fatal("plan created state")
	}
}

func TestPlanNeverPrintsAnAPIKeyFromTheEnvironment(t *testing.T) {
	path := writeCrontab(t, original)
	c := newCmdEnv(t, "plan", "--file", path)
	c.Getenv = func(k string) string {
		if k == "CRONBEAT_API_KEY" {
			return "cb_agent_CANARYSECRETVALUE123456"
		}
		return ""
	}
	Main(c.Env)
	if strings.Contains(c.stdout.String()+c.stderr.String(), "CANARY") {
		t.Fatal("key printed")
	}
}

func TestApplyThenUndoRoundTrip(t *testing.T) {
	path := writeCrontab(t, original)
	apply := newCmdEnv(t, "apply", "--file", path)
	if code := Main(apply.Env); code != 0 {
		t.Fatalf("apply exit %d: %s", code, apply.stderr.String())
	}
	data, _ := os.ReadFile(path) //nolint:gosec // test fixture needs this file mode or path
	if !strings.Contains(string(data), bin+" run --name backup") {
		t.Fatalf("file = %q", data)
	}
	if !strings.Contains(apply.stdout.String(), "Backup:") || !strings.Contains(apply.stdout.String(), "undo") {
		t.Fatalf("stdout = %q", apply.stdout.String())
	}
	undo := newCmdEnv(t, "undo", "--file", path)
	undo.Home = apply.home
	undo.Home = apply.home
	if code := Main(undo.Env); code != 0 {
		t.Fatalf("undo exit %d: %s", code, undo.stderr.String())
	}
	if data, _ := os.ReadFile(path); string(data) != original { //nolint:gosec // test fixture needs this file mode or path
		t.Fatalf("file after undo = %q", data)
	}
}

func TestUndoNeedsForceAfterManualEdits(t *testing.T) {
	path := writeCrontab(t, original)
	apply := newCmdEnv(t, "apply", "--file", path)
	Main(apply.Env)
	data, _ := os.ReadFile(path)                                                                          //nolint:gosec // test fixture needs this file mode or path
	if err := os.WriteFile(path, append(data, []byte("1 1 * * * /opt/new.sh\n")...), 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	undo := newCmdEnv(t, "undo", "--file", path)
	undo.Home = apply.home
	if code := Main(undo.Env); code != 1 || !strings.Contains(undo.stderr.String(), "--force") {
		t.Fatalf("exit %d stderr %q", code, undo.stderr.String())
	}
	forced := newCmdEnv(t, "undo", "--file", path, "--force")
	forced.Home = apply.home
	if code := Main(forced.Env); code != 0 {
		t.Fatalf("forced undo exit %d: %s", code, forced.stderr.String())
	}
}

func TestCronDFlagRules(t *testing.T) {
	cronD := writeCrontab(t, "30 4 * * * root /opt/clean.sh\n")
	c := newCmdEnv(t, "plan", "--file", cronD, "--cron-d")
	if code := Main(c.Env); code != 0 || !strings.Contains(c.stdout.String(), "root") {
		t.Fatalf("exit %d out %q err %q", code, c.stdout.String(), c.stderr.String())
	}
	c = newCmdEnv(t, "plan", "--cron-d")
	if code := Main(c.Env); code != 64 || !strings.Contains(c.stderr.String(), "--file") {
		t.Fatalf("--cron-d without --file: exit %d err %q", code, c.stderr.String())
	}
	for _, system := range []string{"/etc/cron.d/app", "/etc/crontab"} {
		c = newCmdEnv(t, "plan", "--file", system)
		if code := Main(c.Env); code != 64 || !strings.Contains(c.stderr.String(), "--cron-d") {
			t.Fatalf("%s without --cron-d: exit %d err %q", system, code, c.stderr.String())
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"plan", "--nope"}, {"plan", "extra"}, {"plan", "--force"}} {
		c := newCmdEnv(t, args...)
		if code := Main(c.Env); code != 64 || c.stderr.Len() == 0 {
			t.Errorf("%v: exit %d stderr %q", args, code, c.stderr.String())
		}
	}
}

func TestMissingFileIsAFailureNotAUsageError(t *testing.T) {
	c := newCmdEnv(t, "plan", "--file", filepath.Join(t.TempDir(), "absent"))
	if code := Main(c.Env); code != 1 || c.stderr.Len() == 0 {
		t.Fatalf("exit %d stderr %q", code, c.stderr.String())
	}
}

func TestUserCrontabThroughTheCrontabCommand(t *testing.T) {
	store, data := fakeCrontab(t)
	if err := os.WriteFile(data, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCmdEnv(t, "apply")
	c.CrontabBin = store.Bin
	if code := Main(c.Env); code != 0 {
		t.Fatalf("exit %d: %s", code, c.stderr.String())
	}
	stored, _ := os.ReadFile(data) //nolint:gosec // test fixture needs this file mode or path
	if !strings.Contains(string(stored), "run --name backup") {
		t.Fatalf("stored = %q", stored)
	}
	undo := newCmdEnv(t, "undo")
	undo.Home = c.home
	undo.CrontabBin = store.Bin
	if code := Main(undo.Env); code != 0 {
		t.Fatalf("undo exit %d: %s", code, undo.stderr.String())
	}
	if stored, _ := os.ReadFile(data); string(stored) != original { //nolint:gosec // test fixture needs this file mode or path
		t.Fatalf("stored after undo = %q", stored)
	}
}

func TestBackupsGoToTheStateDirectory(t *testing.T) {
	path := writeCrontab(t, original)
	c := newCmdEnv(t, "apply", "--file", path)
	state := t.TempDir()
	c.Getenv = func(k string) string {
		if k == "XDG_STATE_HOME" {
			return state
		}
		return ""
	}
	Main(c.Env)
	entries, _ := os.ReadDir(filepath.Join(state, "cronbeat", "crontab-backups"))
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
}
