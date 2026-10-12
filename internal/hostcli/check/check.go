//go:build unix

// Package check implements "cronbeat check": it verifies the API key against CronBeat.
package check

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/client"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/config"
)

const (
	exitFailure = 1
	exitUsage   = 64
)

// Checker validates an API key.
type Checker interface {
	Check(ctx context.Context, p client.Policy) (client.CheckResponse, error)
}

// Env is what "cronbeat check" needs from its surroundings.
type Env struct {
	Args       []string
	Getenv     func(string) string
	Stdout     io.Writer
	Stderr     io.Writer
	Home       string
	EtcKeyPath string
	NewChecker func(apiURL string, key config.Secret) Checker
}

// Main returns the process exit code.
func Main(env Env) int {
	var apiURLFlag, keyFile string
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&apiURLFlag, "api-url", "", "")
	fs.StringVar(&keyFile, "key-file", "", "")
	if err := fs.Parse(env.Args); err != nil || fs.NArg() > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "cronbeat check: invalid arguments\nusage: cronbeat check [--api-url URL] [--key-file FILE]\n")
		return exitUsage
	}
	apiURL, err := config.APIURL(apiURLFlag, env.Getenv)
	if err != nil {
		return failf(env, "%v", err)
	}
	home := ""
	if env.Home != "" {
		home = filepath.Join(env.Home, ".config", "cronbeat", "key")
	}
	res := config.ResolveKey(config.KeySources{Getenv: env.Getenv, FlagFile: keyFile, EtcPath: env.EtcKeyPath, HomePath: home})
	for _, rejected := range res.Rejected {
		_, _ = fmt.Fprintf(env.Stderr, "cronbeat check: %v\n", rejected)
	}
	if res.Key == "" {
		return failf(env, "no API key found: run `cronbeat configure`, set CRONBEAT_API_KEY or pass --key-file")
	}

	resp, err := env.NewChecker(apiURL, res.Key).Check(context.Background(), client.CheckPolicy)
	if err != nil {
		return failf(env, "%s", describe(err))
	}
	if resp.Kind != "host" {
		return failf(env, "this API key belongs to a %s agent, create a Linux host agent in CronBeat", resp.Kind)
	}
	_, _ = fmt.Fprintf(env.Stdout, "OK: the API key is valid\nhost agent: %s\nAPI:        %s\nkey from:   %s\n", resp.Name, apiURL, res.Source)
	return 0
}

func failf(env Env, format string, args ...any) int {
	_, _ = fmt.Fprintf(env.Stderr, "cronbeat check: "+format+"\n", args...)
	return exitFailure
}

func describe(err error) string {
	var se *client.StatusError
	if !errors.As(err, &se) {
		return fmt.Sprintf("could not reach CronBeat: %v", err)
	}
	detail := ""
	if se.Detail != "" {
		detail = ": " + se.Detail
	}
	switch client.Kind(err) {
	case "auth":
		return "CronBeat rejected the API key (HTTP 401), it is wrong or revoked"
	case "forbidden":
		return "CronBeat refused the API key (HTTP 403)" + detail
	default:
		return fmt.Sprintf("CronBeat answered HTTP %d%s", se.Status, detail)
	}
}
