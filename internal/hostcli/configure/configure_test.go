//go:build unix

package configure

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canary = "cb_agent_CANARYSECRETVALUE123456"

type fakeTerminal struct {
	tty    bool
	secret string
	err    error
	asked  int
}

func (f *fakeTerminal) IsTerminal(*os.File) bool { return f.tty }

func (f *fakeTerminal) ReadSecret(_ *os.File, prompt io.Writer, label string) (string, error) {
	f.asked++
	_, _ = io.WriteString(prompt, label)
	return f.secret, f.err
}

type setup struct {
	env    Env
	term   *fakeTerminal
	out    *bytes.Buffer
	errOut *bytes.Buffer
	home   string
}

func newSetup(t *testing.T, stdin string, args ...string) *setup {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	go func() { _, _ = io.WriteString(w, stdin); _ = w.Close() }()
	home := t.TempDir()
	s := &setup{term: &fakeTerminal{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, home: home}
	s.env = Env{Args: args, Stdin: r, Stdout: s.out, Stderr: s.errOut, Home: home, EtcKeyPath: filepath.Join(home, "etc", "cronbeat", "key"), Terminal: s.term}
	return s
}

func (s *setup) homeKey() string { return filepath.Join(s.home, ".config", "cronbeat", "key") }

func readKeyFile(t *testing.T, path string) (string, os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test fixture needs this file mode or path
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	return string(data), info.Mode().Perm()
}

func TestStdinKeyIsWrittenToTheHomeKeyFileWith0600(t *testing.T) {
	s := newSetup(t, canary+"\n", "--stdin")
	if code := Main(s.env); code != 0 {
		t.Fatalf("exit %d: %s", code, s.errOut.String())
	}
	content, mode := readKeyFile(t, s.homeKey())
	if content != canary+"\n" || mode != 0o600 {
		t.Fatalf("content=%q mode=%04o", content, mode)
	}
	if !strings.Contains(s.out.String(), s.homeKey()) || !strings.Contains(s.out.String(), "cronbeat check") {
		t.Fatalf("stdout = %q", s.out.String())
	}
	if strings.Contains(s.out.String()+s.errOut.String(), canary) {
		t.Fatal("the key was echoed")
	}
}

func TestRootWritesTheSystemKeyFile(t *testing.T) {
	s := newSetup(t, canary, "--stdin")
	s.env.IsRoot = true
	if code := Main(s.env); code != 0 {
		t.Fatalf("exit %d: %s", code, s.errOut.String())
	}
	if content, mode := readKeyFile(t, s.env.EtcKeyPath); content != canary+"\n" || mode != 0o600 {
		t.Fatalf("content=%q mode=%04o", content, mode)
	}
}

func TestKeyFileFlagOverridesTheDefaultPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom", "key")
	s := newSetup(t, canary+"\r\n", "--stdin", "--key-file", path)
	if code := Main(s.env); code != 0 {
		t.Fatalf("exit %d: %s", code, s.errOut.String())
	}
	if content, _ := readKeyFile(t, path); content != canary+"\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestExistingFileIsReplacedAtomicallyAndFixed(t *testing.T) {
	s := newSetup(t, canary, "--stdin")
	if err := os.MkdirAll(filepath.Dir(s.homeKey()), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.homeKey(), []byte("old"), 0o644); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	if code := Main(s.env); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if content, mode := readKeyFile(t, s.homeKey()); content != canary+"\n" || mode != 0o600 {
		t.Fatalf("content=%q mode=%04o", content, mode)
	}
}

func TestWithoutStdinFlagANonTerminalIsRefused(t *testing.T) {
	s := newSetup(t, canary+"\n")
	if code := Main(s.env); code == 0 {
		t.Fatal("must not read a key from a non terminal without --stdin")
	}
	if _, err := os.Stat(s.homeKey()); err == nil {
		t.Fatal("a file was written")
	}
	if !strings.Contains(s.errOut.String(), "--stdin") {
		t.Fatalf("stderr = %q", s.errOut.String())
	}
}

func TestTerminalPromptReadsTheKeyWithoutEcho(t *testing.T) {
	s := newSetup(t, "")
	s.term.tty, s.term.secret = true, "  "+canary+" "
	if code := Main(s.env); code != 0 {
		t.Fatalf("exit %d: %s", code, s.errOut.String())
	}
	if s.term.asked != 1 {
		t.Fatalf("asked %d times", s.term.asked)
	}
	if content, _ := readKeyFile(t, s.homeKey()); content != canary+"\n" {
		t.Fatalf("content = %q", content)
	}
	if strings.Contains(s.out.String()+s.errOut.String(), canary) {
		t.Fatal("the key was echoed")
	}
	if !strings.Contains(s.errOut.String(), "API key") {
		t.Fatalf("the prompt must go to stderr: %q", s.errOut.String())
	}
}

func TestTerminalReadErrorWritesNothing(t *testing.T) {
	s := newSetup(t, "")
	s.term.tty, s.term.err = true, errors.New("interrupted")
	if code := Main(s.env); code == 0 {
		t.Fatal("expected a failure")
	}
	if _, err := os.Stat(s.homeKey()); err == nil {
		t.Fatal("a file was written")
	}
}

func TestInvalidKeysAreRejected(t *testing.T) {
	for name, input := range map[string]string{
		"empty":       "",
		"blank":       "  \n",
		"inner space": "cb_agent_abc def\n",
		"non ascii":   "cb_agent_é\n",
		"control":     "cb_agent_\x01abc\n",
		"too long":    strings.Repeat("a", 5000) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := newSetup(t, input, "--stdin")
			if code := Main(s.env); code == 0 {
				t.Fatal("accepted")
			}
			if _, err := os.Stat(s.homeKey()); err == nil {
				t.Fatal("a file was written")
			}
			if strings.Contains(s.errOut.String(), "cb_agent_abc") {
				t.Fatalf("error leaks the key: %q", s.errOut.String())
			}
		})
	}
}

func TestOnlyTheFirstLineOfStdinIsUsed(t *testing.T) {
	s := newSetup(t, canary+"\nsecond line that is ignored\n", "--stdin")
	if code := Main(s.env); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if content, _ := readKeyFile(t, s.homeKey()); content != canary+"\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestUnknownFlagsAndArgumentsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"--bogus"}, {"extra"}} {
		s := newSetup(t, canary, args...)
		if code := Main(s.env); code != 64 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestWriteFailureDoesNotLeakTheKey(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSetup(t, canary, "--stdin", "--key-file", filepath.Join(blocker, "sub", "key"))
	if code := Main(s.env); code != 1 || strings.Contains(s.errOut.String(), canary) {
		t.Fatalf("exit %d stderr %q", code, s.errOut.String())
	}
}
