//go:build unix

package cli

import (
	"bytes"
	"strings"
	"testing"

	"cronbeat.dev/cronbeat-agent/internal/hostversion"
)

func dispatch(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := dispatchTo(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersionCommand(t *testing.T) {
	code, out, _ := dispatch(t, "version")
	if code != 0 || !strings.HasPrefix(out, "cronbeat "+hostversion.Version+" ") {
		t.Fatalf("exit %d out %q", code, out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("version must be one line: %q", out)
	}
}

func TestHelpGoesToStdoutAndSucceeds(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		code, out, _ := dispatch(t, arg)
		if code != 0 || !strings.Contains(out, "cronbeat run") || !strings.Contains(out, "crontab") {
			t.Fatalf("%s: exit %d out %q", arg, code, out)
		}
	}
}

func TestMissingOrUnknownCommandIsAUsageError(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"--nope"}} {
		code, out, errOut := dispatch(t, args...)
		if code != 64 || out != "" || !strings.Contains(errOut, "usage") {
			t.Fatalf("%v: exit %d out %q err %q", args, code, out, errOut)
		}
	}
}
