//go:build unix

// Package tty detects terminals.
package tty

import "os"

// IsTerminal reports whether f is an interactive terminal. A character device is
// a terminal unless it is /dev/null, the one character device cron jobs redirect to.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(info, null)
}
