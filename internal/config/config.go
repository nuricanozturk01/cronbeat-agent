// Package config reads the agent configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// DefaultHealthAddr is used when HEALTH_ADDR is not set.
const DefaultHealthAddr = ":8081"

// DefaultAPIURL is used when CRONBEAT_API_URL is not set.
const DefaultAPIURL = "https://api.cronbeat.dev"

// Config is the validated agent configuration.
type Config struct {
	APIURL          string
	APIKey          string
	ClusterName     string
	WatchNamespaces []string
	LogLevel        slog.Level
	// HealthAddr is where the probes listen, empty means no probe server.
	HealthAddr string
}

// String never includes the API key.
func (c Config) String() string {
	return fmt.Sprintf("Config{APIURL:%s APIKey:*** ClusterName:%s WatchNamespaces:%v LogLevel:%s HealthAddr:%q}",
		c.APIURL, c.ClusterName, c.WatchNamespaces, c.LogLevel, c.HealthAddr)
}

// GoString never includes the API key.
func (c Config) GoString() string { return c.String() }

// Load reads and validates the configuration. getenv is os.Getenv in production.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	cfg := Config{
		APIURL:      strings.TrimSpace(getenv("CRONBEAT_API_URL")),
		APIKey:      strings.TrimSpace(getenv("CRONBEAT_API_KEY")),
		ClusterName: strings.TrimSpace(getenv("CLUSTER_NAME")),
	}
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}
	if u, err := url.Parse(cfg.APIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		errs = append(errs, errors.New("CRONBEAT_API_URL must be an absolute http(s) URL"))
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	if cfg.APIKey == "" {
		errs = append(errs, errors.New("CRONBEAT_API_KEY is required"))
	}
	if cfg.ClusterName == "" {
		errs = append(errs, errors.New("CLUSTER_NAME is required"))
	}
	cfg.WatchNamespaces = parseNamespaces(getenv("WATCH_NAMESPACES"))

	addr, err := parseHealthAddr(getenv("HEALTH_ADDR"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.HealthAddr = addr

	level, err := parseLevel(getenv("LOG_LEVEL"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.LogLevel = level
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

func parseNamespaces(v string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func parseHealthAddr(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "":
		return DefaultHealthAddr, nil
	case "off":
		return "", nil
	}
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", fmt.Errorf("HEALTH_ADDR %q must look like :8081 or off", v)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("HEALTH_ADDR %q has an invalid port", v)
	}
	return v, nil
}

func parseLevel(v string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("LOG_LEVEL %q is invalid (debug, info, warn, error)", v)
	}
}
