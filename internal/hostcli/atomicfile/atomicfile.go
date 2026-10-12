//go:build unix

// Package atomicfile replaces files so readers see either the old or the new content.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write replaces path with data and the given mode. Missing parent directories are
// created with dirMode when it is not zero.
func Write(path string, data []byte, mode, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if dirMode != 0 {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	if err := fill(tmp, data, mode); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func fill(f *os.File, data []byte, mode os.FileMode) error {
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("set mode: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}
