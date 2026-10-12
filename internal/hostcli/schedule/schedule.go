// Package schedule normalises cron schedules to the five field form the CronBeat host API accepts.
package schedule

import (
	"errors"
	"fmt"
	"strings"
)

const (
	maxLength = 256
	reboot    = "@reboot"
)

var macros = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

// Macro returns the five field expression of a cron macro such as @daily.
func Macro(spec string) (string, bool) {
	expr, ok := macros[strings.ToLower(strings.TrimSpace(spec))]
	return expr, ok
}

// IsReboot reports whether spec is the @reboot macro.
func IsReboot(spec string) bool {
	return strings.EqualFold(strings.TrimSpace(spec), reboot)
}

// Normalize returns the five field cron expression for spec.
func Normalize(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", errors.New("schedule is empty")
	}
	if len(spec) > maxLength {
		return "", errors.New("schedule is too long")
	}
	if strings.ContainsAny(spec, "\r\n\x00") {
		return "", errors.New("schedule must be a single line")
	}
	if strings.HasPrefix(spec, "@") {
		if IsReboot(spec) {
			return "", errors.New("@reboot has no schedule")
		}
		expr, ok := Macro(spec)
		if !ok {
			return "", fmt.Errorf("unknown schedule macro %q", spec)
		}
		return expr, nil
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return "", fmt.Errorf("schedule must have 5 fields, got %d", len(fields))
	}
	for i, f := range fields {
		if !ValidField(i, f) {
			return "", fmt.Errorf("schedule field %q has unsupported characters", f)
		}
	}
	return strings.Join(fields, " "), nil
}

// ValidField reports whether f only uses characters the field at index (0 minute ... 4 weekday) allows.
// Names are only valid for the month and weekday fields.
func ValidField(index int, f string) bool {
	if f == "" {
		return false
	}
	names := index >= 3
	for _, r := range f {
		switch {
		case r >= '0' && r <= '9', r == '*', r == '/', r == ',', r == '-', r == '?':
		case names && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
		default:
			return false
		}
	}
	return true
}
