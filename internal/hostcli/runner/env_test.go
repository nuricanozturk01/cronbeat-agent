//go:build unix

package runner

import (
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestChildEnvRemovesOnlyTheAPIKey(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HOME=/home/u", "TZ=Europe/Istanbul", "CRONBEAT_API_KEY=secret", "CRONBEAT_KEY_FILE=/k",
		"CRONBEAT_API_URL=https://x", "cronbeat_api_key=lower", "CRONBEAT_API_KEY_OTHER=keep", "XCRONBEAT_API_KEY=keep",
		"CRONBEAT_API_KEY=duplicate", "EMPTY=",
	}
	want := []string{
		"PATH=/usr/bin", "HOME=/home/u", "TZ=Europe/Istanbul", "CRONBEAT_KEY_FILE=/k", "CRONBEAT_API_URL=https://x",
		"cronbeat_api_key=lower", "CRONBEAT_API_KEY_OTHER=keep", "XCRONBEAT_API_KEY=keep", "EMPTY=",
	}
	if got := childEnv(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("childEnv =\n%v\nwant\n%v", got, want)
	}
}

func TestChildEnvDoesNotModifyTheInput(t *testing.T) {
	in := []string{"A=1", "CRONBEAT_API_KEY=s", "B=2"}
	childEnv(in)
	if in[1] != "CRONBEAT_API_KEY=s" {
		t.Fatalf("input modified: %v", in)
	}
}

func TestRunStripsTheAPIKeyFromAnInheritedEnvironment(t *testing.T) {
	t.Setenv("CRONBEAT_API_KEY", "cb_agent_CANARYSECRETVALUE123456")
	t.Setenv("CRONBEAT_KEY_FILE", "/some/key")
	out := &syncBuffer{}
	res := Run(Options{Argv: shell("env"), Stdin: devNull(t), Stdout: out, Stderr: io.Discard})
	if res.ExitCode != 0 || strings.Contains(out.String(), "CANARYSECRET") || strings.Contains(out.String(), "CRONBEAT_API_KEY") {
		t.Fatal("the API key reached the job")
	}
	if !strings.Contains(out.String(), "CRONBEAT_KEY_FILE=/some/key") {
		t.Fatal("CRONBEAT_KEY_FILE must be kept")
	}
}

func TestRunStripsTheAPIKeyFromAnExplicitEnvironment(t *testing.T) {
	out := &syncBuffer{}
	Run(Options{Argv: shell("env"), Env: []string{"PATH=/usr/bin:/bin", "CRONBEAT_API_KEY=cb_agent_CANARY"}, Stdin: devNull(t), Stdout: out, Stderr: io.Discard})
	if strings.Contains(out.String(), "CANARY") {
		t.Fatal("the API key reached the job")
	}
}
