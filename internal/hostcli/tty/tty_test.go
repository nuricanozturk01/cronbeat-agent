//go:build unix

package tty

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsTerminalIsFalseForPipesFilesAndDevNull(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	for name, f := range map[string]*os.File{"pipe read": r, "pipe write": w, "regular file": file, "/dev/null": null} {
		if IsTerminal(f) {
			t.Errorf("%s reported as a terminal", name)
		}
	}
}

func TestIsTerminalHandlesNilAndClosedFiles(t *testing.T) {
	if IsTerminal(nil) {
		t.Error("nil file")
	}
	f, _ := os.Create(filepath.Join(t.TempDir(), "x"))
	_ = f.Close()
	if IsTerminal(f) {
		t.Error("closed file")
	}
}
