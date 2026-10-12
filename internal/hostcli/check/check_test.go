//go:build unix

package check

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/client"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/config"
)

const canary = "cb_agent_CANARYSECRETVALUE123456"

type fakeChecker struct {
	resp client.CheckResponse
	err  error
	url  string
	key  config.Secret
}

func (f *fakeChecker) Check(context.Context, client.Policy) (client.CheckResponse, error) {
	return f.resp, f.err
}

func newEnv(t *testing.T, args ...string) (Env, *fakeChecker, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	fake := &fakeChecker{resp: client.CheckResponse{Name: "prod-db-1", Kind: "host", Version: 1}}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	home := t.TempDir()
	vars := map[string]string{"CRONBEAT_API_KEY": canary, "CRONBEAT_API_URL": "https://api.example.test"}
	return Env{
		Args:       args,
		Getenv:     func(k string) string { return vars[k] },
		Stdout:     out,
		Stderr:     errOut,
		Home:       home,
		EtcKeyPath: filepath.Join(home, "etc-key"),
		NewChecker: func(url string, key config.Secret) Checker {
			fake.url, fake.key = url, key
			return fake
		},
	}, fake, out, errOut
}

func TestValidKeyPrintsTheHostAndSucceeds(t *testing.T) {
	env, fake, out, errOut := newEnv(t)
	if code := Main(env); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	for _, want := range []string{"prod-db-1", "https://api.example.test", "CRONBEAT_API_KEY"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if fake.key.Reveal() != canary || fake.url != "https://api.example.test" {
		t.Fatalf("checker got url=%q", fake.url)
	}
	if strings.Contains(out.String()+errOut.String(), canary) {
		t.Fatal("key printed")
	}
}

func TestMissingKeyFails(t *testing.T) {
	env, fake, _, errOut := newEnv(t)
	env.Getenv = func(string) string { return "" }
	if code := Main(env); code != 1 || fake.key != "" {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "cronbeat configure") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestFailuresAreExplained(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"unauthorized", &client.StatusError{Status: 401}, "rejected"},
		{"forbidden", &client.StatusError{Status: 403, Detail: "wrong key kind"}, "wrong key kind"},
		{"network", errors.New("connection refused"), "connection refused"},
		{"server", &client.StatusError{Status: 503}, "503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, fake, out, errOut := newEnv(t)
			fake.err = tt.err
			if code := Main(env); code != 1 {
				t.Fatalf("exit = %d", code)
			}
			if !strings.Contains(errOut.String(), tt.want) || out.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
			}
			if strings.Contains(errOut.String(), canary) {
				t.Fatal("key printed")
			}
		})
	}
}

func TestKubernetesKeyIsNotAHostKey(t *testing.T) {
	env, fake, _, errOut := newEnv(t)
	fake.resp.Kind = "kubernetes"
	if code := Main(env); code != 1 || !strings.Contains(errOut.String(), "kubernetes") {
		t.Fatalf("exit %d stderr %q", code, errOut.String())
	}
}

func TestInsecureURLIsRefusedBeforeAnythingIsSent(t *testing.T) {
	env, fake, _, errOut := newEnv(t, "--api-url", "http://api.example.test")
	if code := Main(env); code != 1 || fake.key != "" || !strings.Contains(errOut.String(), "https") {
		t.Fatalf("exit %d key=%v stderr=%q", code, fake.key != "", errOut.String())
	}
}

func TestKeyFileFlagAndPermissionCheck(t *testing.T) {
	env, fake, _, errOut := newEnv(t)
	env.Getenv = func(string) string { return "" }
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	env.Args = []string{"--key-file", path}
	if code := Main(env); code != 0 || fake.key.Reveal() != canary {
		t.Fatalf("exit %d stderr %q", code, errOut.String())
	}
	if err := os.Chmod(path, 0o640); err != nil { //nolint:gosec // the test needs a key file that is too open
		t.Fatal(err)
	}
	fake.key = ""
	if code := Main(env); code != 1 || fake.key != "" || !strings.Contains(errOut.String(), "0600") {
		t.Fatalf("exit %d stderr %q", code, errOut.String())
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	env, _, _, errOut := newEnv(t, "--bogus")
	if code := Main(env); code != 64 || errOut.Len() == 0 {
		t.Fatalf("exit %d", code)
	}
}
