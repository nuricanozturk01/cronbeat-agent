//go:build unix

package keyfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canary = "cb_agent_CANARYSECRETVALUE123456"

func writeFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadReturnsTheTrimmedKey(t *testing.T) {
	path := writeFile(t, "  "+canary+" \n\n", 0o600)
	got, err := Read(path)
	if err != nil || got != canary {
		t.Fatalf("Read = %q, %v", got, err)
	}
}

func TestReadAcceptsOwnerReadOnly(t *testing.T) {
	path := writeFile(t, canary, 0o400)
	if got, err := Read(path); err != nil || got != canary {
		t.Fatalf("Read = %q, %v", got, err)
	}
}

func TestReadRejectsGroupOrOtherAccessibleFiles(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666, 0o610, 0o601} {
		path := writeFile(t, canary, mode)
		_, err := Read(path)
		if err == nil {
			t.Fatalf("mode %04o was accepted", mode)
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("error leaks the key: %v", err)
		}
		if !strings.Contains(err.Error(), "0600") {
			t.Fatalf("error %q should tell the expected mode", err)
		}
	}
}

func TestReadMissingFileIsNotExist(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, ErrNotExist) {
		t.Fatalf("error = %v, want ErrNotExist", err)
	}
}

func TestReadRejectsDirectories(t *testing.T) {
	if _, err := Read(t.TempDir()); err == nil || errors.Is(err, ErrNotExist) {
		t.Fatalf("error = %v", err)
	}
}

func TestReadRejectsEmptyAndMalformedKeys(t *testing.T) {
	for name, content := range map[string]string{
		"empty":       "",
		"blank":       " \n\t\n",
		"inner space": "cb_agent_abc def",
		"control":     "cb_agent_abc\x01def",
		"non ascii":   "cb_agent_é",
		"huge":        strings.Repeat("a", 5000),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, content, 0o600)
			if _, err := Read(path); err == nil {
				t.Fatal("accepted")
			} else if strings.Contains(err.Error(), "cb_agent_abc") {
				t.Fatalf("error leaks key material: %v", err)
			}
		})
	}
}

func TestReadUsesTheFirstLineOnly(t *testing.T) {
	path := writeFile(t, canary+"\nsecond line\n", 0o600)
	if got, err := Read(path); err != nil || got != canary {
		t.Fatalf("Read = %q, %v", got, err)
	}
}

func TestWriteCreatesTheDirectoryAndA0600File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "cronbeat", "key")
	if err := Write(path, canary); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o", info.Mode().Perm())
	}
	if got, err := Read(path); err != nil || got != canary {
		t.Fatalf("Read = %q, %v", got, err)
	}
}

func TestWriteReplacesAnExistingFileAndFixesItsMode(t *testing.T) {
	path := writeFile(t, "old", 0o644)
	if err := Write(path, canary); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o", info.Mode().Perm())
	}
	if got, err := Read(path); err != nil || got != canary {
		t.Fatalf("Read = %q, %v", got, err)
	}
}

func TestWriteLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "key"), canary); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "key" {
		t.Fatalf("directory has %v", entries)
	}
}

func TestWriteRejectsInvalidKeyAndKeepsTheOldFile(t *testing.T) {
	path := writeFile(t, canary, 0o600)
	if err := Write(path, "bad key\nwith newline"); err == nil {
		t.Fatal("accepted an invalid key")
	}
	if got, _ := Read(path); got != canary {
		t.Fatalf("old key was lost: %q", got)
	}
}

func TestWriteFailureDoesNotLeakTheKey(t *testing.T) {
	blocker := writeFile(t, "x", 0o600)
	err := Write(filepath.Join(blocker, "sub", "key"), canary)
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidKey(t *testing.T) {
	for key, want := range map[string]bool{
		canary: true, "a": true, "": false, "a b": false, "a\n": false, "é": false, strings.Repeat("a", 256): true, strings.Repeat("a", 257): false,
	} {
		if got := ValidKey(key); got != want {
			t.Errorf("ValidKey(%q) = %v, want %v", key, got, want)
		}
	}
}
