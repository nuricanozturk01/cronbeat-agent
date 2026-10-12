package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaultsAndParsing(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"CRONBEAT_API_KEY": "cb_agent_secret",
		"CLUSTER_NAME":     "prod-eu",
		"WATCH_NAMESPACES": " b, a ,,b",
		"LOG_LEVEL":        "DEBUG",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIURL != DefaultAPIURL {
		t.Errorf("APIURL = %q", cfg.APIURL)
	}
	if got := strings.Join(cfg.WatchNamespaces, ","); got != "a,b" {
		t.Errorf("namespaces = %q", got)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("level = %v", cfg.LogLevel)
	}
}

func TestLoadValidation(t *testing.T) {
	cases := map[string]map[string]string{
		"missing key":     {"CLUSTER_NAME": "x"},
		"missing cluster": {"CRONBEAT_API_KEY": "k"},
		"bad url":         {"CRONBEAT_API_KEY": "k", "CLUSTER_NAME": "x", "CRONBEAT_API_URL": "ftp://x"},
		"bad level":       {"CRONBEAT_API_KEY": "k", "CLUSTER_NAME": "x", "LOG_LEVEL": "loud"},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(env(m)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestConfigNeverPrintsKey(t *testing.T) {
	cfg, err := Load(env(map[string]string{"CRONBEAT_API_KEY": "cb_agent_topsecret", "CLUSTER_NAME": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(s, "topsecret") {
			t.Fatalf("key leaked: %s", s)
		}
	}
}

func TestHealthAddressDefaultsCanBeChangedAndSwitchedOff(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			switch k {
			case "CRONBEAT_API_KEY":
				return "cb_agent_x"
			case "CLUSTER_NAME":
				return "prod"
			case "HEALTH_ADDR":
				return v
			}
			return ""
		}
	}
	tests := map[string]string{"": ":8081", ":9090": ":9090", "0.0.0.0:8080": "0.0.0.0:8080", "off": ""}
	for in, want := range tests {
		cfg, err := Load(env(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if cfg.HealthAddr != want {
			t.Errorf("%q -> %q, want %q", in, cfg.HealthAddr, want)
		}
	}
	for _, bad := range []string{"8081", "localhost", ":notaport", ":99999", "a:b:c"} {
		if _, err := Load(env(bad)); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
