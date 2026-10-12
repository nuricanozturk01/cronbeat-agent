//go:build unix

// Command cronbeat runs a command and reports it to CronBeat. See "cronbeat help".
package main

import (
	"os"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
