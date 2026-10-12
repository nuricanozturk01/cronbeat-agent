package config

import (
	"regexp"
	"strings"
)

var zoneName = regexp.MustCompile(`^(?:UTC|[A-Za-z]+(?:/[A-Za-z0-9_+-]+)+)$`)

// TimeZoneSources are the lookups DetectTimeZone uses; tests replace them.
type TimeZoneSources struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	Readlink func(string) (string, error)
}

// DetectTimeZone returns the IANA zone of this host (TZ, /etc/timezone, /etc/localtime) or "".
func DetectTimeZone(s TimeZoneSources) string {
	if zone := cleanZone(strings.TrimPrefix(strings.TrimSpace(s.Getenv("TZ")), ":")); zone != "" {
		return zone
	}
	if data, err := s.ReadFile("/etc/timezone"); err == nil {
		if zone := cleanZone(strings.TrimSpace(string(data))); zone != "" {
			return zone
		}
	}
	if target, err := s.Readlink("/etc/localtime"); err == nil {
		if _, after, ok := strings.Cut(target, "zoneinfo/"); ok {
			after = strings.TrimPrefix(strings.TrimPrefix(after, "posix/"), "right/")
			return cleanZone(after)
		}
	}
	return ""
}

func cleanZone(zone string) string {
	if zoneName.MatchString(zone) {
		return zone
	}
	return ""
}
