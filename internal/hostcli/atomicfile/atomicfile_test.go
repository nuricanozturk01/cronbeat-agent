//go:build unix

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesFileWithModeAndParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "file")
	if err := Write(path, []byte("hello"), 0o640, 0o750); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("stat = %v, %v", info, err)
	}
	dir, _ := os.Stat(filepath.Dir(path))
	if dir.Mode().Perm() != 0o750 {
		t.Fatalf("dir mode = %04o", dir.Mode().Perm())
	}
	if data, _ := os.ReadFile(path); string(data) != "hello" { //nolint:gosec // test fixture needs this file mode or path
		t.Fatalf("content = %q", data)
	}
}

func TestWriteReplacesContentAndModeAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("old content that is longer"), 0o666); err != nil { //nolint:gosec // test fixture needs this file mode or path
		t.Fatal(err)
	}
	if err := Write(path, []byte("new"), 0o600, 0); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	data, _ := os.ReadFile(path) //nolint:gosec // test fixture needs this file mode or path
	entries, _ := os.ReadDir(dir)
	if string(data) != "new" || info.Mode().Perm() != 0o600 || len(entries) != 1 {
		t.Fatalf("data=%q mode=%04o entries=%d", data, info.Mode().Perm(), len(entries))
	}
}

func TestWriteFailsWithoutParentWhenDirModeIsZero(t *testing.T) {
	if err := Write(filepath.Join(t.TempDir(), "missing", "file"), []byte("x"), 0o600, 0); err == nil {
		t.Fatal("expected an error")
	}
}

func TestWriteDoesNotLeaveTemporaryFilesOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(target, []byte("x"), 0o600, 0); err == nil {
		t.Fatal("renaming a file over a directory must fail")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftovers: %v", entries)
	}
}
