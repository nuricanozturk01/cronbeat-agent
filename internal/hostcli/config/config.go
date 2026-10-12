// Package config resolves the API key and API URL of the host CLI.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/keyfile"
)

// DefaultAPIURL is used when no API URL is configured.
const DefaultAPIURL = "https://api.cronbeat.dev"

const masked = "***"

// Secret is an API key that never prints itself.
type Secret string

// String returns a mask.
func (Secret) String() string { return masked }

// GoString returns a mask.
func (Secret) GoString() string { return masked }

// Format prints a mask for every verb.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(masked)) }

// LogValue keeps the key out of structured logs.
func (Secret) LogValue() slog.Value { return slog.StringValue(masked) }

// MarshalText keeps the key out of encoded output.
func (Secret) MarshalText() ([]byte, error) { return []byte(masked), nil }

// Reveal returns the key for the Authorization header.
func (s Secret) Reveal() string { return string(s) }

// KeySources lists where the API key may come from, in order of precedence.
type KeySources struct {
	Getenv   func(string) string
	FlagFile string
	EtcPath  string
	HomePath string
}

// KeyResult is the outcome of ResolveKey.
type KeyResult struct {
	Key      Secret
	Source   string
	Rejected []error
}

// ResolveKey finds the API key: CRONBEAT_API_KEY, --key-file or CRONBEAT_KEY_FILE, /etc, home.
// An explicitly named source that fails is final: another identity is never used silently.
func ResolveKey(s KeySources) KeyResult {
	if v := strings.TrimSpace(s.Getenv("CRONBEAT_API_KEY")); v != "" {
		if !keyfile.ValidKey(v) {
			return KeyResult{Rejected: []error{errors.New("CRONBEAT_API_KEY is not a valid API key (whitespace or control characters)")}}
		}
		return KeyResult{Key: Secret(v), Source: "CRONBEAT_API_KEY"}
	}
	explicit := s.FlagFile
	if explicit == "" {
		explicit = strings.TrimSpace(s.Getenv("CRONBEAT_KEY_FILE"))
	}
	if explicit != "" {
		key, err := keyfile.Read(explicit)
		if err != nil {
			return KeyResult{Rejected: []error{err}}
		}
		return KeyResult{Key: Secret(key), Source: "key file " + explicit}
	}
	var rejected []error
	for _, path := range []string{s.EtcPath, s.HomePath} {
		if path == "" {
			continue
		}
		key, err := keyfile.Read(path)
		if errors.Is(err, keyfile.ErrNotExist) {
			continue
		}
		if err != nil {
			rejected = append(rejected, err)
			continue
		}
		return KeyResult{Key: Secret(key), Source: "key file " + path, Rejected: rejected}
	}
	return KeyResult{Rejected: rejected}
}

// ValidateAPIURL checks the URL and returns it without a trailing slash.
// Plain http is only accepted for loopback hosts.
func ValidateAPIURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return "", errors.New("the API URL is not valid, use an absolute https URL")
	}
	if u.User != nil {
		return "", errors.New("the API URL must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", errors.New("the API URL must not contain a query or a fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackName(u.Hostname()) {
			return "", errors.New("the API URL must use https (http is only allowed for localhost, 127.0.0.1 and ::1)")
		}
	default:
		return "", errors.New("the API URL must use https")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/"), nil
}

func isLoopbackName(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// APIURL picks the flag, then CRONBEAT_API_URL, then the default, and validates it.
func APIURL(flagValue string, getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(flagValue)
	if raw == "" {
		raw = strings.TrimSpace(getenv("CRONBEAT_API_URL"))
	}
	if raw == "" {
		raw = DefaultAPIURL
	}
	return ValidateAPIURL(raw)
}
