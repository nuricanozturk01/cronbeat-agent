//go:build unix

package runner

import "runtime"

func goroutineCount() int { return runtime.NumGoroutine() }
