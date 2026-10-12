package k8s

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
)

// Annotations a CronJob can carry to configure its monitor from the manifest.
const (
	annotationPrefix     = "cronbeat.dev/"
	annotationGrace      = annotationPrefix + "grace"
	annotationMaxRuntime = annotationPrefix + "max-runtime"
	annotationIgnore     = annotationPrefix + "ignore"

	secondsPerDay = 24 * 60 * 60
	maxGrace      = 7 * secondsPerDay
	maxRuntime    = secondsPerDay
)

// annotationSettings are the values read from a CronJob's annotations. A value
// that is not valid is left out and described in problem; the others still count.
type annotationSettings struct {
	grace      int
	maxRuntime int
	ignored    bool
	problem    string
}

func readAnnotations(cj *batchv1.CronJob) annotationSettings {
	var s annotationSettings
	var problems []string
	if raw, ok := cj.Annotations[annotationGrace]; ok {
		if v, err := parseSeconds(raw, 1, maxGrace); err != nil {
			problems = append(problems, fmt.Sprintf("%s %q: %s, between 1 second and 7 days", annotationGrace, raw, err))
		} else {
			s.grace = v
		}
	}
	if raw, ok := cj.Annotations[annotationMaxRuntime]; ok {
		if v, err := parseSeconds(raw, 1, maxRuntime); err != nil {
			problems = append(problems, fmt.Sprintf("%s %q: %s, between 1 second and 24 hours", annotationMaxRuntime, raw, err))
		} else {
			s.maxRuntime = v
		}
	}
	if raw, ok := cj.Annotations[annotationIgnore]; ok {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true":
			s.ignored = true
		case "false", "":
		default:
			problems = append(problems, fmt.Sprintf("%s %q: use true or false", annotationIgnore, raw))
		}
	}
	s.problem = strings.Join(problems, "; ")
	return s
}

// parseSeconds reads "90" (seconds), "10m", "1h30m" (Go durations) or "2d".
func parseSeconds(raw string, min, max int) (int, error) {
	text := strings.TrimSpace(raw)
	var seconds int64
	switch {
	case text == "":
		return 0, fmt.Errorf("empty value")
	case isDigits(text):
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("not a number")
		}
		seconds = v
	case strings.HasSuffix(text, "d") && isDigits(strings.TrimSuffix(text, "d")):
		days, err := strconv.ParseInt(strings.TrimSuffix(text, "d"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("not a number")
		}
		seconds = days * secondsPerDay
	default:
		d, err := time.ParseDuration(text)
		if err != nil {
			return 0, fmt.Errorf("not a duration (use for example 90, 10m, 2h or 1d)")
		}
		seconds = int64(d / time.Second)
	}
	if seconds < int64(min) || seconds > int64(max) {
		return 0, fmt.Errorf("out of range")
	}
	return int(seconds), nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func ignoredByAnnotation(cj *batchv1.CronJob) bool {
	return strings.EqualFold(strings.TrimSpace(cj.Annotations[annotationIgnore]), "true")
}
