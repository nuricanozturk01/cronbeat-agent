// Package hostversion holds the host CLI version, overridden at build time with
// -ldflags "-X cronbeat.dev/cronbeat-agent/internal/hostversion.Version=x.y.z".
package hostversion

// Version is the cronbeat host CLI version reported to the CronBeat API.
var Version = "0.1.0"
