//go:build unix

package runner

import (
	"strconv"
	"syscall"
)

var signalNames = map[syscall.Signal]string{
	syscall.SIGABRT: "SIGABRT", syscall.SIGALRM: "SIGALRM", syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE",
	syscall.SIGHUP: "SIGHUP", syscall.SIGILL: "SIGILL", syscall.SIGINT: "SIGINT", syscall.SIGKILL: "SIGKILL",
	syscall.SIGPIPE: "SIGPIPE", syscall.SIGPROF: "SIGPROF", syscall.SIGQUIT: "SIGQUIT", syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGSYS: "SIGSYS", syscall.SIGTERM: "SIGTERM", syscall.SIGTRAP: "SIGTRAP", syscall.SIGUSR1: "SIGUSR1",
	syscall.SIGUSR2: "SIGUSR2", syscall.SIGVTALRM: "SIGVTALRM", syscall.SIGXCPU: "SIGXCPU", syscall.SIGXFSZ: "SIGXFSZ",
	syscall.SIGSTOP: "SIGSTOP", syscall.SIGTSTP: "SIGTSTP", syscall.SIGTTIN: "SIGTTIN", syscall.SIGTTOU: "SIGTTOU",
}

func signalName(sig syscall.Signal) string {
	if name, ok := signalNames[sig]; ok {
		return name
	}
	return "SIG" + strconv.Itoa(int(sig))
}
