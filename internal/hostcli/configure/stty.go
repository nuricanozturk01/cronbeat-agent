//go:build unix

package configure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/tty"
)

const sttyTimeout = 5 * time.Second

// STTY reads a secret from a terminal by switching echo off with the stty command.
type STTY struct{}

// IsTerminal implements Terminal.
func (STTY) IsTerminal(f *os.File) bool { return tty.IsTerminal(f) }

// ReadSecret implements Terminal. It refuses to read when echo cannot be switched off.
func (STTY) ReadSecret(f *os.File, prompt io.Writer, label string) (string, error) {
	saved, err := stty(f, "-g")
	if err != nil {
		return "", fmt.Errorf("cannot control the terminal: %w", err)
	}
	if _, err := stty(f, "-echo"); err != nil {
		return "", fmt.Errorf("cannot switch off echo: %w", err)
	}
	restore := func() { _, _ = stty(f, strings.TrimSpace(saved)) }
	defer restore()

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupts)

	_, _ = io.WriteString(prompt, label)
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readUnbuffered(f)
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		_, _ = io.WriteString(prompt, "\n")
		return r.line, r.err
	case <-interrupts:
		_, _ = io.WriteString(prompt, "\n")
		return "", errors.New("interrupted")
	}
}

func readUnbuffered(f *os.File) (string, error) {
	var line []byte
	buf := make([]byte, 1)
	for len(line) < maxInputBytes {
		n, err := f.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				break
			}
			line = append(line, buf[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
	}
	return string(line), nil
}

func stty(f *os.File, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sttyTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "stty", args...) //nolint:gosec // fixed program, arguments come from this file
	cmd.Stdin, cmd.Stdout = f, &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}
