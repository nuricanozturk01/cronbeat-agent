//go:build unix

package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func searchPath(env []string) string {
	if env == nil {
		if p, ok := os.LookupEnv("PATH"); ok {
			return p
		}
		return defaultSearchPath
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return defaultSearchPath
}

// resolve finds the executable like a shell does and tells "not found" (127)
// apart from "found but not executable" (126).
func resolve(name, pathList string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty command name", ErrNotFound)
	}
	if strings.Contains(name, "/") {
		return checkExecutable(name, name)
	}
	denied := false
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		path, err := checkExecutable(candidate, name)
		switch {
		case err == nil:
			return path, nil
		case errors.Is(err, ErrNotExecutable):
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
				denied = true
			}
		}
	}
	if denied {
		return "", fmt.Errorf("%w: %s: permission denied", ErrNotExecutable, name)
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, name)
}

func checkExecutable(path, name string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return "", fmt.Errorf("%w: %s: %w", ErrNotExecutable, name, errors.Unwrap(err))
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: %s: is a directory", ErrNotExecutable, name)
	}
	if err := syscall.Access(path, 1); err != nil {
		return "", fmt.Errorf("%w: %s: permission denied", ErrNotExecutable, name)
	}
	return path, nil
}

func startExitCode(err error) int {
	if errors.Is(err, ErrNotFound) {
		return exitNotFound
	}
	return exitNotExecutable
}
