// Package version holds the agent version, overridden at build time with
// -ldflags "-X cronbeat.dev/cronbeat-agent/internal/version.Version=x.y.z".
package version

// Version is the agent version reported to the CronBeat API.
var Version = "0.4.0"
