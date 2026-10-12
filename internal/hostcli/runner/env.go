//go:build unix

package runner

import (
	"os"
	"strings"
)

const strippedVariablePrefix = "CRONBEAT_API_KEY="

// childEnv drops the API key so the job cannot print it and /proc/<pid>/environ does not expose it.
func childEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, strippedVariablePrefix) {
			out = append(out, kv)
		}
	}
	return out
}

func inheritedEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	return childEnv(env)
}
