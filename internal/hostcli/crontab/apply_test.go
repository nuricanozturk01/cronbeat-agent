//go:build unix

package crontab

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type env struct {
	dir     string
	file    string
	store   FileStore
	backups *Backups
	now     time.Time
}

func newEnv(t *testing.T, content string, mode os.FileMode) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{dir: dir, file: filepath.Join(dir, "crontab"), now: time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)}
	if err := os.WriteFile(e.file, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.file, mode); err != nil {
		t.Fatal(err)
	}
	e.store = FileStore{Path: e.file}
	e.backups = &Backups{Dir: filepath.Join(dir, "backups"), Now: func() time.Time { return e.now }}
	return e
}

func (e *env) content(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var opts = Options{BinaryPath: bin}

const original = "MAILTO=ops@example.com\n0 2 * * * /opt/backup.sh\n"

func TestApplyWritesTheWrappedCrontabAndKeepsAPrivateBackup(t *testing.T) {
	e := newEnv(t, original, 0o644)
	res, err := Apply(e.store, e.backups, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := build(t, original, false).Output
	if !res.Changed || e.content(t) != want {
		t.Fatalf("changed=%v content=%q", res.Changed, e.content(t))
	}
	backup, err := os.ReadFile(res.BackupPath)
	if err != nil || string(backup) != original {
		t.Fatalf("backup = %q, %v", backup, err)
	}
	info, _ := os.Stat(res.BackupPath)
	dirInfo, _ := os.Stat(e.backups.Dir)
	if info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("backup mode %04o dir mode %04o", info.Mode().Perm(), dirInfo.Mode().Perm())
	}
}

func TestApplyPreservesTheFileMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o600, 0o640} {
		e := newEnv(t, original, mode)
		if _, err := Apply(e.store, e.backups, opts); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(e.file)
		if info.Mode().Perm() != mode {
			t.Fatalf("mode = %04o, want %04o", info.Mode().Perm(), mode)
		}
	}
}

func TestApplyWithNothingToWrapChangesNothingAndMakesNoBackup(t *testing.T) {
	e := newEnv(t, "# nothing\nMAILTO=a\n@reboot /opt/boot.sh\n", 0o644)
	res, err := Apply(e.store, e.backups, opts)
	if err != nil || res.Changed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(e.backups.Dir); !os.IsNotExist(err) {
		t.Fatalf("a backup directory was created: %v", err)
	}
}

func TestApplyTwiceIsIdempotent(t *testing.T) {
	e := newEnv(t, original, 0o644)
	if _, err := Apply(e.store, e.backups, opts); err != nil {
		t.Fatal(err)
	}
	after := e.content(t)
	res, err := Apply(e.store, e.backups, opts)
	if err != nil || res.Changed || e.content(t) != after {
		t.Fatalf("second apply: res=%+v err=%v", res, err)
	}
	entries, _ := os.ReadDir(e.backups.Dir)
	if len(entries) != 2 {
		t.Fatalf("expected exactly one backup pair, got %d files", len(entries))
	}
}

func TestUndoRestoresTheOriginal(t *testing.T) {
	e := newEnv(t, original, 0o640)
	if _, err := Apply(e.store, e.backups, opts); err != nil {
		t.Fatal(err)
	}
	res, err := Undo(e.store, e.backups, false)
	if err != nil || !res.Changed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	info, _ := os.Stat(e.file)
	if e.content(t) != original || info.Mode().Perm() != 0o640 {
		t.Fatalf("content=%q mode=%04o", e.content(t), info.Mode().Perm())
	}
}

func TestUndoRefusesWhenTheCrontabChangedUnlessForced(t *testing.T) {
	e := newEnv(t, original, 0o644)
	if _, err := Apply(e.store, e.backups, opts); err != nil {
		t.Fatal(err)
	}
	edited := e.content(t) + "30 5 * * * /opt/added-later.sh\n"
	if err := os.WriteFile(e.file, []byte(edited), 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	if _, err := Undo(e.store, e.backups, false); !errors.Is(err, ErrModified) {
		t.Fatalf("err = %v", err)
	}
	if e.content(t) != edited {
		t.Fatal("a refused undo must not touch the crontab")
	}
	if _, err := Undo(e.store, e.backups, true); err != nil {
		t.Fatal(err)
	}
	if e.content(t) != original {
		t.Fatalf("forced undo content = %q", e.content(t))
	}
}

func TestUndoWithoutBackupFails(t *testing.T) {
	e := newEnv(t, original, 0o644)
	if _, err := Undo(e.store, e.backups, false); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("err = %v", err)
	}
}

func TestUndoCannotBeRepeated(t *testing.T) {
	e := newEnv(t, original, 0o644)
	if _, err := Apply(e.store, e.backups, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := Undo(e.store, e.backups, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Undo(e.store, e.backups, false); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("second undo: %v", err)
	}
}

func TestUndoUsesTheLatestApplyOfTheSameTarget(t *testing.T) {
	e := newEnv(t, original, 0o644)
	if _, err := Apply(e.store, e.backups, opts); err != nil {
		t.Fatal(err)
	}
	withNewJob := e.content(t) + "15 4 * * * /opt/second.sh\n"
	if err := os.WriteFile(e.file, []byte(withNewJob), 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Hour)
	if _, err := Apply(e.store, e.backups, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := Undo(e.store, e.backups, false); err != nil {
		t.Fatal(err)
	}
	if e.content(t) != withNewJob {
		t.Fatalf("undo went back too far: %q", e.content(t))
	}
}

func TestBackupsOfOtherTargetsAreIgnored(t *testing.T) {
	a := newEnv(t, original, 0o644)
	other := filepath.Join(a.dir, "other")
	if err := os.WriteFile(other, []byte("1 1 * * * /opt/o.sh\n"), 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	if _, err := Apply(FileStore{Path: other}, a.backups, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := Undo(a.store, a.backups, false); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("err = %v", err)
	}
}

func TestBackupsInTheSameInstantDoNotOverwriteEachOther(t *testing.T) {
	e := newEnv(t, original, 0o644)
	first, err := e.backups.Save(e.store.Target(), "one", "x")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.backups.Save(e.store.Target(), "two", "y")
	if err != nil || first.BackupPath == second.BackupPath {
		t.Fatalf("paths %q %q err=%v", first.BackupPath, second.BackupPath, err)
	}
	latest, err := e.backups.Latest(e.store.Target())
	if err != nil || latest.Original != "two" {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
}

type lossyStore struct {
	content string
	writes  []string
	drop    bool
}

func (s *lossyStore) Read() (string, error) { return s.content, nil }
func (s *lossyStore) Target() string        { return "user" }
func (s *lossyStore) Write(c string) error {
	s.writes = append(s.writes, c)
	if !s.drop || len(s.writes) > 1 {
		s.content = c
	}
	return nil
}

func TestApplyVerifiesByReadingBackAndRollsBack(t *testing.T) {
	e := newEnv(t, "", 0o644)
	store := &lossyStore{content: original, drop: true}
	_, err := Apply(store, e.backups, opts)
	if err == nil || !strings.Contains(err.Error(), "verif") {
		t.Fatalf("err = %v", err)
	}
	if store.content != original || len(store.writes) != 2 || store.writes[1] != original {
		t.Fatalf("content=%q writes=%d", store.content, len(store.writes))
	}
}

type failingStore struct{ lossyStore }

func (s *failingStore) Write(string) error {
	return errors.New("crontab: errors in crontab file, can't install")
}

func TestFailedWriteLeavesTheCrontabAndReportsTheCause(t *testing.T) {
	e := newEnv(t, "", 0o644)
	store := &failingStore{lossyStore{content: original}}
	_, err := Apply(store, e.backups, opts)
	if err == nil || !strings.Contains(err.Error(), "can't install") || store.content != original {
		t.Fatalf("err=%v content=%q", err, store.content)
	}
}

func TestReadErrorIsReported(t *testing.T) {
	e := newEnv(t, original, 0o644)
	if _, err := Apply(FileStore{Path: filepath.Join(e.dir, "missing")}, e.backups, opts); err == nil {
		t.Fatal("expected an error")
	}
}

func TestInvalidOptionsFailBeforeAnythingIsWritten(t *testing.T) {
	e := newEnv(t, original, 0o644)
	if _, err := Apply(e.store, e.backups, Options{BinaryPath: "relative"}); err == nil {
		t.Fatal("expected an error")
	}
	if e.content(t) != original {
		t.Fatal("crontab was modified")
	}
}

func TestCronDApplyKeepsTheUserColumn(t *testing.T) {
	e := newEnv(t, "30 4 * * * root /opt/clean.sh\n", 0o644)
	if _, err := Apply(e.store, e.backups, Options{BinaryPath: bin, CronD: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.content(t), "30 4 * * * root "+bin+" run --name clean") {
		t.Fatalf("content = %q", e.content(t))
	}
}

func TestFileStoreTargetIsTheAbsolutePath(t *testing.T) {
	e := newEnv(t, original, 0o644)
	if !filepath.IsAbs(e.store.Target()) || e.store.Target() != e.file {
		t.Fatalf("Target = %q", e.store.Target())
	}
	rel := FileStore{Path: "relative/file"}
	if !filepath.IsAbs(rel.Target()) {
		t.Fatalf("relative path target = %q", rel.Target())
	}
}

func TestFileStoreRefusesHugeFiles(t *testing.T) {
	e := newEnv(t, strings.Repeat("# filler line\n", 400_000), 0o644)
	if _, err := e.store.Read(); err == nil {
		t.Fatal("a multi megabyte crontab must be refused")
	}
}

func fakeCrontab(t *testing.T) (UserStore, string) {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "stored")
	script := filepath.Join(dir, "crontab")
	body := `#!/bin/sh
case "$1" in
  -l) if [ -f "` + data + `" ]; then cat "` + data + `"; else echo "no crontab for tester" >&2; exit 1; fi ;;
  -) if [ -n "$FAKE_CRONTAB_FAIL" ]; then echo "errors in crontab file, can't install" >&2; exit 1; fi; cat > "` + data + `" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	return UserStore{Bin: script}, data
}

func TestUserStoreReadsAnAbsentCrontabAsEmpty(t *testing.T) {
	store, _ := fakeCrontab(t)
	content, err := store.Read()
	if err != nil || content != "" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if store.Target() != "user" {
		t.Fatalf("Target = %q", store.Target())
	}
}

func TestUserStoreRoundTripAndApplyAndUndo(t *testing.T) {
	store, data := fakeCrontab(t)
	if err := store.Write(original); err != nil {
		t.Fatal(err)
	}
	backups := &Backups{Dir: filepath.Join(t.TempDir(), "b"), Now: time.Now}
	if _, err := Apply(store, backups, opts); err != nil {
		t.Fatal(err)
	}
	stored, _ := os.ReadFile(data) //nolint:gosec // test fixture needs this file mode or path
	if !strings.Contains(string(stored), bin+" run --name backup") {
		t.Fatalf("stored = %q", stored)
	}
	if _, err := Undo(store, backups, false); err != nil {
		t.Fatal(err)
	}
	stored, _ = os.ReadFile(data) //nolint:gosec // test fixture needs this file mode or path
	if string(stored) != original {
		t.Fatalf("stored after undo = %q", stored)
	}
}

func TestUserStoreReportsCrontabErrors(t *testing.T) {
	store, data := fakeCrontab(t)
	if err := os.WriteFile(data, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CRONTAB_FAIL", "1")
	err := store.Write("garbage")
	if err == nil || !strings.Contains(err.Error(), "can't install") {
		t.Fatalf("err = %v", err)
	}
	if stored, _ := os.ReadFile(data); string(stored) != original { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal("failed install changed the stored crontab")
	}
}

func TestUserStoreMissingBinary(t *testing.T) {
	if _, err := (UserStore{Bin: "/nonexistent/crontab"}).Read(); err == nil {
		t.Fatal("expected an error")
	}
}
