// Package keyfile reads and writes the file that stores the CronBeat host API key.
package keyfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/atomicfile"
)

const (
	maxFileBytes = 4096
	maxKeyLength = 256
	fileMode     = 0o600
	dirMode      = 0o700
)

// ErrNotExist is returned (wrapped) when the key file does not exist.
var ErrNotExist = errors.New("key file does not exist")

// ValidKey reports whether key can safely be used in an HTTP header.
func ValidKey(key string) bool {
	if key == "" || len(key) > maxKeyLength {
		return false
	}
	for i := range len(key) {
		if key[i] <= ' ' || key[i] >= 0x7f {
			return false
		}
	}
	return true
}

// Read returns the key stored in path. Files readable by group or others are rejected.
func Read(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // reading the key file the user named is the purpose
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s: %w", path, ErrNotExist)
	}
	if err != nil {
		return "", fmt.Errorf("open key file %s: %w", path, errors.Unwrap(err))
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat key file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("key file %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("key file %s has mode %04o, expected 0600 (not accessible by group or others): chmod 600 %s",
			path, info.Mode().Perm(), path)
	}
	line, err := bufio.NewReader(io.LimitReader(f, maxFileBytes)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read key file %s: %w", path, err)
	}
	key := strings.TrimSpace(line)
	if !ValidKey(key) {
		return "", fmt.Errorf("key file %s does not contain a valid API key", path)
	}
	return key, nil
}

// Write stores key in path with mode 0600, replacing the file atomically.
func Write(path, key string) error {
	if !ValidKey(key) {
		return errors.New("the API key is empty or contains whitespace or control characters")
	}
	if err := atomicfile.Write(path, []byte(key+"\n"), fileMode, dirMode); err != nil {
		return fmt.Errorf("write key file: %w", err)
	}
	return nil
}
