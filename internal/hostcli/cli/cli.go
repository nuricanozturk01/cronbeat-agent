//go:build unix

// Package cli dispatches the cronbeat host commands.
package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/check"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/client"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/config"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/configure"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/crontab"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/run"
	"cronbeat.dev/cronbeat-agent/internal/hostversion"
)

const (
	exitUsage    = 64
	exitInternal = 70
	etcKeyPath   = "/etc/cronbeat/key"
)

const usage = `cronbeat monitors cron jobs and other scheduled commands with CronBeat.

usage:
  cronbeat run --name JOB [--schedule "CRON"] [--timezone IANA] [--grace 5m|300] [--max-runtime 1h|3600]
               [--output failure|always|never] [--tail-bytes N] [--api-url URL] [--key-file FILE] [--verbose]
               -- COMMAND [ARGS...]
  cronbeat check [--api-url URL] [--key-file FILE]
  cronbeat configure [--key-file FILE] [--stdin]
  cronbeat crontab plan|apply|undo [--file FILE] [--cron-d] [--force]
  cronbeat version
`

// Main runs the command in args (without the program name) and returns the process exit code.
func Main(args []string) int { return dispatchTo(args, os.Stdout, os.Stderr) }

func dispatchTo(args []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			_, _ = fmt.Fprintln(stderr, "cronbeat: internal error")
			code = exitInternal
		}
	}()
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	}
	rest := args[1:]
	switch args[0] {
	case "run":
		return run.Main(runEnv(rest, stdout, stderr))
	case "check":
		return check.Main(checkEnv(rest, stdout, stderr))
	case "configure":
		return configure.Main(configureEnv(rest, stdout, stderr))
	case "crontab":
		return crontab.Main(crontabEnv(rest, stdout, stderr))
	case "version", "--version":
		_, _ = fmt.Fprintf(stdout, "cronbeat %s (%s/%s)\n", hostversion.Version, runtime.GOOS, runtime.GOARCH)
		return 0
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "cronbeat: unknown command %q\n%s", args[0], usage)
		return exitUsage
	}
}

func home() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return dir
}

func runEnv(args []string, stdout, stderr io.Writer) run.Env {
	return run.Env{
		Args: args, Getenv: os.Getenv, Stdin: os.Stdin, Stdout: stdout, Stderr: stderr, Home: home(),
		EtcKeyPath: etcKeyPath, Now: time.Now,
		NewPoster: func(apiURL string, key config.Secret) run.Poster { return client.New(apiURL, key) },
		NewRunID:  run.NewRunID,
		TimeZone: func() string {
			return config.DetectTimeZone(config.TimeZoneSources{Getenv: os.Getenv, ReadFile: os.ReadFile, Readlink: os.Readlink})
		},
	}
}

func checkEnv(args []string, stdout, stderr io.Writer) check.Env {
	return check.Env{
		Args: args, Getenv: os.Getenv, Stdout: stdout, Stderr: stderr, Home: home(), EtcKeyPath: etcKeyPath,
		NewChecker: func(apiURL string, key config.Secret) check.Checker { return client.New(apiURL, key) },
	}
}

func configureEnv(args []string, stdout, stderr io.Writer) configure.Env {
	return configure.Env{
		Args: args, Stdin: os.Stdin, Stdout: stdout, Stderr: stderr, Home: home(), EtcKeyPath: etcKeyPath,
		IsRoot: os.Geteuid() == 0, Terminal: configure.STTY{},
	}
}

func crontabEnv(args []string, stdout, stderr io.Writer) crontab.Env {
	return crontab.Env{
		Args: args, Stdout: stdout, Stderr: stderr, Getenv: os.Getenv, Home: home(),
		Executable: os.Executable, CrontabBin: "crontab", Now: time.Now,
	}
}
