//go:build unix

// Package configure implements "cronbeat configure": it stores the API key in a private file.
package configure

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/keyfile"
)

const (
	exitFailure   = 1
	exitUsage     = 64
	maxInputBytes = 4096
)

// Terminal reads secrets from an interactive terminal without echoing them.
type Terminal interface {
	IsTerminal(f *os.File) bool
	ReadSecret(f *os.File, prompt io.Writer, label string) (string, error)
}

// Env is what "cronbeat configure" needs from its surroundings.
type Env struct {
	Args       []string
	Stdin      *os.File
	Stdout     io.Writer
	Stderr     io.Writer
	Home       string
	EtcKeyPath string
	IsRoot     bool
	Terminal   Terminal
}

// Main returns the process exit code.
func Main(env Env) int {
	var keyPath string
	var fromStdin bool
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&keyPath, "key-file", "", "")
	fs.BoolVar(&fromStdin, "stdin", false, "")
	if err := fs.Parse(env.Args); err != nil || fs.NArg() > 0 {
		_, _ = fmt.Fprintln(env.Stderr, "cronbeat configure: invalid arguments\nusage: cronbeat configure [--key-file FILE] [--stdin]")
		return exitUsage
	}
	path, err := target(env, keyPath)
	if err != nil {
		return failf(env, "%v", err)
	}

	var key string
	switch {
	case fromStdin:
		key, err = readLine(env.Stdin)
	case env.Terminal.IsTerminal(env.Stdin):
		key, err = env.Terminal.ReadSecret(env.Stdin, env.Stderr, "CronBeat API key (input is hidden): ")
	default:
		return failf(env, "standard input is not a terminal, pass --stdin to read the key from it")
	}
	if err != nil {
		return failf(env, "cannot read the API key: %v", err)
	}
	key = strings.TrimSpace(key)
	if !keyfile.ValidKey(key) {
		return failf(env, "that is not a valid API key (empty, too long, or with spaces or special characters)")
	}
	if err := keyfile.Write(path, key); err != nil {
		return failf(env, "%v", err)
	}
	_, _ = fmt.Fprintf(env.Stdout, "Saved the API key to %s (mode 0600).\nNext: run `cronbeat check` to verify it.\n", path)
	return 0
}

func target(env Env, flagValue string) (string, error) {
	switch {
	case flagValue != "":
		return flagValue, nil
	case env.IsRoot:
		return env.EtcKeyPath, nil
	case env.Home == "":
		return "", errors.New("cannot find the home directory, pass --key-file")
	default:
		return filepath.Join(env.Home, ".config", "cronbeat", "key"), nil
	}
}

func readLine(f *os.File) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(f, maxInputBytes)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}

func failf(env Env, format string, args ...any) int {
	_, _ = fmt.Fprintf(env.Stderr, "cronbeat configure: "+format+"\n", args...)
	return exitFailure
}
