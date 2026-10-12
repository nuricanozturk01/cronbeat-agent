//go:build unix

package crontab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/atomicfile"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/tail"
)

const (
	maxCrontabBytes = 1 << 20
	commandTimeout  = 15 * time.Second
	maxStderrBytes  = 300
	userTarget      = "user"
)

// Store is a crontab that can be read and replaced.
type Store interface {
	Read() (string, error)
	Write(content string) error
	Target() string
}

// ErrNoBackup is returned by Undo when no apply was recorded for the target.
var ErrNoBackup = errors.New("no backup found")

// ErrModified is returned by Undo when the crontab changed after the apply.
var ErrModified = errors.New("crontab changed since it was applied")

var absentCrontab = regexp.MustCompile(`(?i)no crontab for|can't open|no such file`)

// UserStore is the crontab of the current user, read and written through the crontab command.
type UserStore struct{ Bin string }

func (s UserStore) bin() string {
	if s.Bin == "" {
		return "crontab"
	}
	return s.Bin
}

// Read implements Store. A user without a crontab has an empty one.
func (s UserStore) Read() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.bin(), "-l") //nolint:gosec // the crontab binary is fixed or set by tests
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && stdout.Len() == 0 && absentCrontab.MatchString(stderr.String()) {
			return "", nil
		}
		return "", commandError("crontab -l", err, stderr.String())
	}
	if stdout.Len() > maxCrontabBytes {
		return "", errors.New("the crontab is larger than 1 MiB")
	}
	return stdout.String(), nil
}

// Write implements Store.
func (s UserStore) Write(content string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.bin(), "-") //nolint:gosec // the crontab binary is fixed or set by tests
	cmd.Stdin = strings.NewReader(content)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return commandError("crontab -", err, stderr.String())
	}
	return nil
}

// Target implements Store.
func (UserStore) Target() string { return userTarget }

func commandError(what string, err error, stderr string) error {
	msg := strings.Join(strings.Fields(tail.Last(stderr, maxStderrBytes)), " ")
	if msg == "" {
		return fmt.Errorf("%s failed: %w", what, err)
	}
	return fmt.Errorf("%s failed: %s", what, msg)
}

// FileStore is a crontab file such as /etc/cron.d/app, replaced atomically with its mode kept.
type FileStore struct{ Path string }

// Read implements Store.
func (s FileStore) Read() (string, error) {
	info, err := os.Stat(s.Path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", s.Path, errors.Unwrap(err))
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", s.Path)
	}
	if info.Size() > maxCrontabBytes {
		return "", fmt.Errorf("%s is larger than 1 MiB", s.Path)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", s.Path, errors.Unwrap(err))
	}
	return string(data), nil
}

// Write implements Store.
func (s FileStore) Write(content string) error {
	resolved, err := filepath.EvalSymlinks(s.Path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", s.Path, errors.Unwrap(err))
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat %s: %w", resolved, errors.Unwrap(err))
	}
	if err := atomicfile.Write(resolved, []byte(content), info.Mode().Perm(), 0); err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		_ = os.Chown(resolved, int(st.Uid), int(st.Gid))
	}
	return nil
}

// Target implements Store.
func (s FileStore) Target() string {
	abs, err := filepath.Abs(s.Path)
	if err != nil {
		return s.Path
	}
	return abs
}
