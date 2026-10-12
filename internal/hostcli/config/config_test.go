//go:build unix

package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/keyfile"
)

const canary = "cb_agent_CANARYSECRETVALUE123456"

func TestSecretNeverPrintsItself(t *testing.T) {
	s := Secret(canary)
	type holder struct {
		Key Secret
		Ptr *Secret
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "key", s, "holder", holder{s, &s})
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "key", s)
	jsonOut, _ := json.Marshal(holder{s, &s})
	outputs := []string{
		fmt.Sprint(s), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s),
		fmt.Sprintf("%q", s), fmt.Sprintf("%x", s), fmt.Sprintf("%d", s), fmt.Sprintf("%v", holder{s, &s}),
		fmt.Sprintf("%+v", holder{s, &s}), fmt.Sprintf("%#v", holder{s, &s}), fmt.Sprintf("%v", []Secret{s}),
		fmt.Sprintf("%v", map[string]Secret{"k": s}), fmt.Errorf("wrapped: %v", s).Error(), logs.String(), string(jsonOut),
	}
	for _, out := range outputs {
		if strings.Contains(out, canary) || strings.Contains(out, fmt.Sprintf("%x", canary)) {
			t.Fatalf("secret leaked in %q", out)
		}
	}
	if s.Reveal() != canary {
		t.Fatal("Reveal must return the key")
	}
	if fmt.Sprint(s) != "***" {
		t.Fatalf("String = %q", fmt.Sprint(s))
	}
}

type fixture struct {
	dir  string
	etc  string
	home string
	env  map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	return &fixture{dir: dir, etc: filepath.Join(dir, "etc", "key"), home: filepath.Join(dir, "home", "key"), env: map[string]string{}}
}

func (f *fixture) sources(flagFile string) KeySources {
	return KeySources{
		Getenv:   func(k string) string { return f.env[k] },
		FlagFile: flagFile,
		EtcPath:  f.etc,
		HomePath: f.home,
	}
}

func (f *fixture) put(t *testing.T, path, key string) {
	t.Helper()
	if err := keyfile.Write(path, key); err != nil {
		t.Fatal(err)
	}
}

func TestKeyPrecedenceEnvThenFlagThenEtcThenHome(t *testing.T) {
	f := newFixture(t)
	flagPath := filepath.Join(f.dir, "flag-key")
	f.put(t, f.home, "home-key")
	if got := ResolveKey(f.sources("")); got.Key != "home-key" || !strings.Contains(got.Source, f.home) {
		t.Fatalf("home: %+v", got)
	}
	f.put(t, f.etc, "etc-key")
	if got := ResolveKey(f.sources("")); got.Key != "etc-key" || !strings.Contains(got.Source, f.etc) {
		t.Fatalf("etc: %+v", got)
	}
	f.put(t, flagPath, "flag-key")
	if got := ResolveKey(f.sources(flagPath)); got.Key != "flag-key" {
		t.Fatalf("flag: %+v", got)
	}
	f.env["CRONBEAT_KEY_FILE"] = flagPath
	if got := ResolveKey(f.sources("")); got.Key != "flag-key" {
		t.Fatalf("CRONBEAT_KEY_FILE: %+v", got)
	}
	envPath := filepath.Join(f.dir, "env-key")
	f.put(t, envPath, "envfile-key")
	if got := ResolveKey(f.sources(envPath)); got.Key != "envfile-key" {
		t.Fatalf("flag must beat CRONBEAT_KEY_FILE: %+v", got)
	}
	f.env["CRONBEAT_API_KEY"] = "  env-key \n"
	got := ResolveKey(f.sources(flagPath))
	if got.Key != "env-key" || got.Source != "CRONBEAT_API_KEY" {
		t.Fatalf("env: %+v", got)
	}
}

func TestNoKeyAnywhere(t *testing.T) {
	f := newFixture(t)
	got := ResolveKey(f.sources(""))
	if got.Key != "" || len(got.Rejected) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestLooseEtcFileIsRejectedAndHomeIsUsed(t *testing.T) {
	f := newFixture(t)
	f.put(t, f.etc, "etc-key")
	if err := os.Chmod(f.etc, 0o644); err != nil { //nolint:gosec // the test needs a key file that is too open
		t.Fatal(err)
	}
	f.put(t, f.home, "home-key")
	got := ResolveKey(f.sources(""))
	if got.Key != "home-key" || len(got.Rejected) != 1 || !strings.Contains(got.Rejected[0].Error(), "0600") {
		t.Fatalf("%+v", got)
	}
}

func TestExplicitKeyFileFailureDoesNotFallThrough(t *testing.T) {
	f := newFixture(t)
	f.put(t, f.home, "home-key")
	got := ResolveKey(f.sources(filepath.Join(f.dir, "missing")))
	if got.Key != "" || len(got.Rejected) != 1 {
		t.Fatalf("explicit file must not fall back to another identity: %+v", got)
	}
}

func TestInvalidEnvKeyIsRejected(t *testing.T) {
	f := newFixture(t)
	f.env["CRONBEAT_API_KEY"] = "bad key\nX-Evil: 1"
	got := ResolveKey(f.sources(""))
	if got.Key != "" || len(got.Rejected) != 1 {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(got.Rejected[0].Error(), "Evil") {
		t.Fatalf("error leaks the value: %v", got.Rejected[0])
	}
}

func TestRejectionNeverContainsTheKey(t *testing.T) {
	f := newFixture(t)
	f.put(t, f.etc, canary)
	if err := os.Chmod(f.etc, 0o666); err != nil { //nolint:gosec // the test needs a key file that is too open
		t.Fatal(err)
	}
	for _, err := range ResolveKey(f.sources("")).Rejected {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("leak: %v", err)
		}
	}
}

func TestValidateAPIURL(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://api.cronbeat.dev", "https://api.cronbeat.dev", false},
		{"https://api.cronbeat.dev/", "https://api.cronbeat.dev", false},
		{"https://example.com:8443/base/", "https://example.com:8443/base", false},
		{"http://localhost:8080", "http://localhost:8080", false},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080", false},
		{"http://[::1]:8080", "http://[::1]:8080", false},
		{"http://localhost", "http://localhost", false},
		{"http://example.com", "", true},
		{"http://127.0.0.2:8080", "", true},
		{"http://localhost.evil.com", "", true},
		{"http://10.0.0.5", "", true},
		{"ftp://example.com", "", true},
		{"https://", "", true},
		{"example.com", "", true},
		{"", "", true},
		{"https://user:pass@example.com", "", true},
		{"https://example.com?x=1", "", true},
		{"https://example.com#frag", "", true},
		{"https://exa mple.com", "", true},
		{"HTTPS://API.CRONBEAT.DEV", "https://api.cronbeat.dev", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ValidateAPIURL(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("ValidateAPIURL(%q) = %q, %v", tt.in, got, err)
			}
		})
	}
}

func TestValidateAPIURLErrorDoesNotRepeatCredentials(t *testing.T) {
	_, err := ValidateAPIURL("https://user:topsecret@example.com")
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIURLPrecedence(t *testing.T) {
	env := func(k string) string {
		if k == "CRONBEAT_API_URL" {
			return "https://env.example.com"
		}
		return ""
	}
	if got, err := APIURL("https://flag.example.com", env); err != nil || got != "https://flag.example.com" {
		t.Fatalf("flag: %q %v", got, err)
	}
	if got, err := APIURL("", env); err != nil || got != "https://env.example.com" {
		t.Fatalf("env: %q %v", got, err)
	}
	if got, err := APIURL("", func(string) string { return "" }); err != nil || got != DefaultAPIURL {
		t.Fatalf("default: %q %v", got, err)
	}
	if _, err := APIURL("http://example.com", env); err == nil {
		t.Fatal("plain http to a remote host must fail")
	}
}
