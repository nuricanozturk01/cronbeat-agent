# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [host 0.1.0]

The Linux host CLI `cronbeat` (releases are tagged `host-v<version>` and versioned separately from the
Kubernetes agent; the Kubernetes agent and its chart are unchanged).

### Added
- **`cronbeat run`** wraps a cron job or systemd timer command: exit code, signals (forwarded to the process group),
  stdin and output pass through; the run is reported as `started` and `succeeded` / `failed` with duration,
  exit code or signal and, on failure only, a masked tail of the output (`--output`, `--tail-bytes`).
  CronBeat being unreachable or the key missing never changes what the command does.
  The API key is not passed on to your job: `CRONBEAT_API_KEY` is removed from the command's environment.
- **`cronbeat configure`, `check`, `version`** to store the key (mode 0600, hidden input), verify it and print the version.
- **`cronbeat crontab plan|apply|undo`** wraps the jobs of a crontab or cron.d file, with a backup, read-back
  verification and undo.
- `scripts/install.sh` (checksum verified, Linux amd64 and arm64), `scripts/build-host.sh` and the `host-agent`
  workflow that publishes the binaries, `SHA256SUMS` and a build provenance attestation.

## [0.4.0]

### Added
- **Why a run did not come:** the sync payload carries what the CronJob says about its own scheduling:
  `concurrencyPolicy`, `startingDeadlineSeconds`, `lastScheduleTime`, `lastSuccessfulTime` and `activeJobs`. CronBeat uses
  them to explain a missed run (a Job still running with `Forbid`, no Job created at the expected time, a Job created that
  never started). No new permission is needed: the agent already reads CronJobs.

### Changed
- A run starting or finishing changes `lastScheduleTime` and `activeJobs` but does not start an extra sync; the values
  travel with the next periodic sync.

## [0.3.0]

### Added
- **CronJob annotations** configure the monitor from the manifest: `cronbeat.dev/grace`, `cronbeat.dev/max-runtime` and
  `cronbeat.dev/ignore`. A value outside the limits is ignored and logged, the sync is not affected.
- **Richer failure messages:** the memory limit of an `OOMKilled` container, what common exit codes mean (126, 127, 137, 139,
  143), the kubelet message of a waiting container (for example a missing Secret), and pod level problems
  (`Unschedulable` with the scheduler's message, `Evicted`).
- The Helm install command in the app now names the release after the cluster (`cronbeat-agent-<cluster>`), so several
  agents (for example one per namespace) can be installed side by side.

### Changed
- The sync payload carries the optional `graceSeconds` and `maxRuntimeSeconds`. CronJobs marked `cronbeat.dev/ignore` are
  left out of the sync and of the heartbeat count.
- Of the pod spec the agent now also reads the memory limit of each container (nothing else: no image, command or env).

## [0.2.3]

### Changed
- Default image is `repo.repsy.io/nuricanozturk/cronbeat-agent-bin/agent`.

## [0.2.2]

### Changed
- Default image is `repo.repsy.io/nuricanozturk/cronbeat-public/agent` (public repository).

## [0.2.1]

### Changed
- Default image is `repo.repsy.io/nuricanozturk/cronbeat-cloud/agent`.

## [0.2.0]

### Added
- Liveness (`/healthz`) and readiness (`/readyz`) probes on `HEALTH_ADDR`
  (default `:8081`, `off` disables). Chart: `healthPort`, probes.
- A new or changed CronJob is synced within seconds instead of at the next
  5 minute sync.
- Chart: namespaced `Role`/`RoleBinding` per entry of `watchNamespaces` (no
  cluster-wide access then), `GOMEMLIMIT` (`goMemLimit`) and `GOMAXPROCS`.
- `User-Agent: cronbeat-agent/<version>`.

### Changed
- Much lower memory: informer objects are trimmed to the used fields and pods
  are watched only when they belong to a Job.
- Events wait until the API knows the CronJob, then are replayed after the
  sync, so the first run of a new CronJob is no longer lost.
- Failed syncs retry with backoff (capped at the sync interval, with jitter).
  Entries the API cannot accept are left out; more than 5000 CronJobs sends
  nothing (a partial list would pause the rest).
- Heartbeat counts CronJobs from the cache without copying them; idle HTTP
  connections are limited.
- RBAC needs only `list` and `watch`.

## [0.1.0]

### Added
- Initial release: CronJob sync (on start and every 5 minutes), job events
  (`job_started`, `job_succeeded`, `job_failed`), failure reason inference from
  the Job condition and pod container states, 60 second heartbeat.
- Helm chart `cronbeat-agent` with read-only RBAC.
