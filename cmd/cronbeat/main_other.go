//go:build !unix

// Command cronbeat runs a command and reports it to CronBeat. It is only available on Unix systems.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cronbeat is only available on Linux and other Unix systems")
	os.Exit(1)
}
