# cronbeat-agent

The CronBeat Kubernetes agent. It watches your CronJobs, Jobs and Pods and
reports to [CronBeat](https://cronbeat.dev) when a job started, succeeded or
failed (and why), so you get notified when a CronJob does not run, fails or
runs too long.

This repository also contains the [host CLI](#host-cli-linux-servers-and-vms) `cronbeat`
for cron jobs and systemd timers on Linux servers and VMs.

## What it does

- On start and every 5 minutes it sends the full CronJob list
  (`POST /agent/v1/sync`: namespace, name, uid, schedule, timeZone, suspend).
- For every Job owned by a CronJob (`ownerReferences` kind `CronJob`; other Jobs
  are ignored) it sends `job_started` when the Job is created, `job_succeeded`
  when the condition `Complete=True` appears (with `durationMs`) and
  `job_failed` when `Failed=True` appears (with `reason` and `message`).
- Every 60 seconds it sends a heartbeat so CronBeat knows the agent is alive.
- Optional [annotations](#configure-a-cronjob-with-annotations) on a CronJob set its grace period and maximum runtime,
  or leave it out of monitoring.

## Security model

- **Read-only RBAC.** The chart creates a ClusterRole with `list` and
  `watch` on `batch/cronjobs`, `batch/jobs` and core `pods` (with
  `watchNamespaces` set: a Role per namespace instead, no cluster-wide access).
  Nothing else: no secrets, no pod logs, no write verbs.
- **Outbound only.** The agent only makes HTTPS requests to the CronBeat API.
  It listens on one port (`healthPort`, default 8081) for the liveness and
  readiness probes only and has no Service.
- Runs as non-root (uid 65532), read-only root filesystem, no privilege
  escalation, all capabilities dropped, `RuntimeDefault` seccomp profile,
  distroless static image.
- The API key is read from a Secret through `secretKeyRef`, sent only in the
  `Authorization` header and never logged. Container logs, environment variables, images and commands are never
  read or sent. Only CronJob metadata (namespace, name, uid, schedule, time
  zone, suspend) and job event data (job uid, timestamps, failure reason, a
  short message such as `container backup terminated: OOMKilled (exit code 137)`)
  leave the cluster.
- With `watchNamespaces` set the agent only watches those namespaces and the
  chart creates a Role per namespace instead of a ClusterRole.

## Install

```sh
helm install cronbeat-agent-prod-eu cronbeat-agent --repo https://repo.repsy.io/nuricanozturk/cronbeat-agent \
  --namespace cronbeat-agent --create-namespace \
  --set apiKey=cb_agent_... \
  --set clusterName=prod-eu
```

To watch only some namespaces add `--set 'watchNamespaces={team-a,team-b}'`. The chart
then creates a Role per namespace instead of a ClusterRole. The sync is a complete
list for the cluster name, so monitors of CronJobs outside the watched namespaces are
paused, and two agents must never share one `clusterName`.

To keep the key in a Secret you manage:

```sh
kubectl create namespace cronbeat-agent
kubectl -n cronbeat-agent create secret generic cronbeat-agent-key --from-literal=api-key=cb_agent_...
helm install cronbeat-agent-prod-eu cronbeat-agent --repo https://repo.repsy.io/nuricanozturk/cronbeat-agent -n cronbeat-agent \
  --set existingSecret=cronbeat-agent-key --set clusterName=prod-eu
```

Requires Kubernetes 1.27+ (`spec.timeZone` of CronJob).

## Values

| Key | Default | Description |
|---|---|---|
| `clusterName` | (required) | Cluster name. The server uses the cluster name registered with the key. |
| `apiKey` | `""` | Agent API key, stored in a chart-managed Secret. |
| `existingSecret` | `""` | Name of your own Secret holding the key (instead of `apiKey`). |
| `existingSecretKey` | `api-key` | Key inside `existingSecret`. |
| `apiUrl` | `https://api.cronbeat.dev` | CronBeat API base URL. |
| `watchNamespaces` | `[]` | Namespaces to watch, empty means all. |
| `logLevel` | `info` | `debug`, `info`, `warn`, `error`. |
| `image.repository` | `repo.repsy.io/nuricanozturk/cronbeat-agent-bin/agent` | Override it to use your own mirror. |
| `image.tag` | chart `appVersion` | Image tag. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `goMemLimit` | `200MiB` | Go soft memory limit, keep about 20% below the memory limit. |
| `resources` | 20m/64Mi requests, 200m/256Mi limits | Raise the memory limit for clusters with many thousands of pods. |
| `nodeSelector`, `tolerations`, `affinity` | empty | Scheduling. |
| `podAnnotations`, `podLabels` | empty | |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, release name, empty | |
| `nameOverride`, `fullnameOverride` | empty | |

`clusterName` is required and one of `apiKey` / `existingSecret` must be set
(enforced by `values.schema.json`).

### Agent environment variables

| Variable | Default | Description |
|---|---|---|
| `CRONBEAT_API_URL` | `https://api.cronbeat.dev` | |
| `CRONBEAT_API_KEY` | (required) | Mounted from the Secret. |
| `CLUSTER_NAME` | (required) | |
| `WATCH_NAMESPACES` | empty (all) | Comma separated. |
| `LOG_LEVEL` | `info` | JSON logs on stdout. |
| `KUBECONFIG` | unset | Local development only; in a cluster the in-cluster config is used. |

## Update

```sh
helm list --all-namespaces | grep cronbeat-agent
helm upgrade RELEASE cronbeat-agent --repo https://repo.repsy.io/nuricanozturk/cronbeat-agent \
  --namespace NAMESPACE --reset-then-reuse-values
```

`--reset-then-reuse-values` keeps the values you installed with and takes new chart defaults. The CronBeat app shows the
agent version and offers this command when a newer one is published.

## Configure a CronJob with annotations

Put these on the CronJob (`metadata.annotations`) and keep the settings next to the manifest:

| Annotation | Value | Effect |
|---|---|---|
| `cronbeat.dev/grace` | `90` (seconds), `10m`, `1h30m` or `2d`; 1 second to 7 days | Grace period of the monitor (default 10 minutes) |
| `cronbeat.dev/max-runtime` | same formats; 1 second to 24 hours | A run longer than this is reported as too long |
| `cronbeat.dev/ignore` | `true` | The CronJob is not monitored. An existing monitor is paused |

```yaml
metadata:
  annotations:
    cronbeat.dev/grace: "15m"
    cronbeat.dev/max-runtime: "2h"
```

An invalid value is ignored and logged (`annotations with an invalid value were ignored`), the other settings and the
sync still work. Removing an annotation leaves the monitor as it is, change it in the app if you want another value.
While an annotation is present the agent sets the value on every sync, so a change made in the app is replaced.

## How failure reasons are inferred

When a Job gets `Failed=True`:

1. The `reason` of the Job's `Failed` condition (`DeadlineExceeded`, ...) is used.
2. The container states of the Job's pods (label `batch.kubernetes.io/job-name`,
   falling back to `job-name` and the Job owner reference) are inspected:
   `lastState.terminated` / `state.terminated` reason and exit code (`OOMKilled`,
   `Error`), and `state.waiting.reason` (`ImagePullBackOff`, `ErrImagePull`,
   `CrashLoopBackOff`, ...). Among several candidates `OOMKilled` wins, then
   other terminated states, then waiting states; the most recently updated pod
   comes first.
3. The generic condition reason `BackoffLimitExceeded` carries no information
   about the cause, so when container state is known the more specific
   container reason is reported as `reason` instead. Any other condition reason
   (for example `DeadlineExceeded`) stays the `reason`, and the container
   detail is added to the `message`.

Message formats, followed by the Job condition message:

- `container <name> terminated: <reason> (exit code <code>)`. An `OOMKilled` container adds its memory limit
  (`exit code 137, memory limit 256Mi`); the exit codes 126, 127, 137, 139 and 143 add what they usually mean
  (`command not found`, `killed with SIGKILL`, ...). Other codes belong to your program and are not guessed.
- `container <name> waiting: <reason> (<kubelet message>)`, for example
  `CreateContainerConfigError (secret "db-credentials" not found)`. The message is cut to 200 characters.
- `pod unschedulable: <scheduler message>` (reason `Unschedulable`) and `pod evicted: <node message>` (reason `Evicted`)
  when the pod itself had a problem. A container problem is more specific and wins over a pod problem.

Of the pod spec the agent reads only the memory limit of each container. The kubelet message can contain a Secret or
image name, never a value.

Pods may be gone when the Job fails (`ttlSecondsAfterFinished`), so the agent
remembers the last known abnormal container states per Job from the pod watch
and reads them when it emits `job_failed`. This memory is bounded (at most 5000
jobs and 20 pods per job, entries are dropped when the Job is deleted).

## Delivery guarantees and limitations

- Events go through a bounded in-memory queue (1000 events) and a single
  worker, so informer callbacks never block. Each `(jobUid, type)` is sent at
  most once per process. The backend de-duplicates by `jobUid:type` as well.
- Events are **not persisted**. They live only as long as the process: an event
  still queued when the pod dies is lost, and an event is dropped (with an error
  log) when the queue is full. After a restart the informers replay the Jobs that
  still exist and the agent re-sends their events; events older than one hour are
  skipped so a restart does not replay the cluster history.
- Delivery is retried with exponential backoff and jitter on network errors,
  5xx and 429 (`Retry-After` honoured). Other 4xx responses are never retried.
  On 401 the agent logs an error and keeps retrying slowly (30 s) so a rotated
  key recovers without a restart.
- One replica, no leader election. Two replicas would not corrupt data (events
  are idempotent) but would double the traffic.
- The failure reason of a pod that was never observed (for example it was created
  and deleted while the agent was down) cannot be recovered: only the Job
  condition is available.
- Pods that are already terminating are ignored for reason tracking so that kill
  signals sent during cleanup (exit 137/143) do not hide the real cause.

## Host CLI (Linux servers and VMs)

`cronbeat` is one static binary of about 6 MB for Linux (amd64 and arm64). There is no daemon:
cron, a systemd timer or you start `cronbeat run -- your-command`, it runs the command,
passes its exit code through and tells CronBeat that the run started and how it ended,
so CronBeat can notify you when a job does not run, fails or runs too long.

The command is never harmed by CronBeat. If the API key is missing, the network is down
or CronBeat answers with an error, the command still runs, with the same exit code,
and the only trace is a short warning on stderr.

### Install

```sh
curl -fsSL https://docs.cronbeat.dev/install.sh | sh
cronbeat configure     # asks for the API key of a Linux host agent, input is hidden
cronbeat check         # verifies the key
```

The script supports Linux on amd64 and arm64 only. It downloads
`cronbeat_<version>_linux_<arch>.tar.gz` and `SHA256SUMS` from the GitHub release
`host-v<version>`, verifies the checksum, extracts only the `cronbeat` file and installs
it to `/usr/local/bin` (or `~/.local/bin` when that is not writable). Nothing is
installed when anything does not match. The only downloaded program it runs is
`cronbeat version`, after the checksum was verified.

`CRONBEAT_VERSION` pins another version, `CRONBEAT_INSTALL_DIR` chooses the directory
and `CRONBEAT_DOWNLOAD_BASE` points to a mirror. Every release asset also has a build
provenance attestation: `gh attestation verify cronbeat_0.1.0_linux_amd64.tar.gz --repo nuricanozturk01/cronbeat-agent`.

### Run a job

```sh
cronbeat run --name nightly-backup --schedule "0 2 * * *" --grace 10m -- /opt/backup.sh --full
```

In a crontab (the key comes from `cronbeat configure`, or from a `CRONBEAT_API_KEY=` line
at the top of the crontab):

```cron
0 2 * * * /usr/local/bin/cronbeat run --name nightly-backup --schedule '0 2 * * *' -- /opt/backup.sh --full
```

| Flag | Meaning |
| --- | --- |
| `--name` | Job name, 1 to 64 characters of `A-Za-z0-9._-`. Required to monitor the job. |
| `--schedule` | Five field cron expression or a macro such as `@daily`. Needed the first time, when CronBeat creates the monitor. Schedule, name and time zone belong to the CLI: changing them here updates the monitor. |
| `--timezone` | IANA zone of the schedule. Default: `TZ`, `/etc/timezone` or `/etc/localtime` of the host, otherwise UTC. |
| `--grace`, `--max-runtime` | Seconds (`300`) or a duration (`5m`, `1h`). Only used when the monitor is created, edit them in the app afterwards. |
| `--output failure\|always\|never` | When the output tail is sent. Default `failure`. |
| `--tail-bytes N` | Size of the output tail, 1 to 8192, default 4096. |
| `--api-url`, `--key-file` | Override `CRONBEAT_API_URL` (default `https://api.cronbeat.dev`) and `CRONBEAT_KEY_FILE`. |
| `--verbose` | Prints what cronbeat does (never the key) and shows warnings every time. |

Everything after `--` belongs to your command, including words that look like flags.
Without `--`, the command starts at the first word that is not a flag of `cronbeat`.
If the flags of `cronbeat` itself are wrong but a command is given, the command still
runs (without monitoring) after a warning.

### Exit codes

`cronbeat run` exits with the exit code of your command. If the command is killed by a
signal it exits with 128 plus the signal number (137 for `SIGKILL`) and CronBeat is told
the signal name. If it cannot be started, the codes are the shell's: 127 when the command
is not found, 126 when it exists but cannot be executed. Only a missing command after `--`
is a usage error (64). CronBeat problems never change the exit code.

`SIGINT`, `SIGTERM`, `SIGHUP` and `SIGQUIT` sent to `cronbeat` are forwarded to the
command's process group, so `systemctl stop` or `kill` reach the grandchildren too.
Standard input is passed through and standard output and error are passed through
unchanged, in order. When they are terminals the command is attached to them directly.

A background process that your job leaves running and that keeps stdout or stderr open
would keep cronbeat waiting for the end of the output. cronbeat waits two seconds after
the command ended and then stops reading. That process receives SIGPIPE when it writes
afterwards. Redirect its output (`> /dev/null 2>&1`) or use `--output never`.

### Warnings

Problems with CronBeat itself (no key, wrong key, network, plan limit, server errors) are
printed to stderr as `cronbeat: warning: ...`, at most once per kind every 6 hours
(state in `~/.cache/cronbeat/warned.json`, best effort), so a broken connection does not
fill the cron mail. `--verbose` shows them every time.

### Crontab helper

```sh
cronbeat crontab plan                 # show what would change, writes nothing
cronbeat crontab apply                # backup, write, read back and verify
cronbeat crontab undo                 # restore the backup of the last apply
cronbeat crontab plan --file /etc/cron.d/myapp --cron-d    # system crontab files have a user column
```

`apply` wraps every job line of your crontab with `cronbeat run` and uses the schedule of
the line. Comments, blank lines and `NAME=value` lines are never touched. A job that
already uses `cronbeat` is skipped, so running it twice changes nothing. Skipped, with a
reason in the plan: `@reboot` (no schedule), lines with an unescaped `%` (cron turns it into a
newline, quoting cannot fix that), lines that are not valid crontab lines, and lines that
would become longer than 1000 bytes (some cron implementations drop those).

The original command becomes a single quoted argument: `/bin/sh -c '<your command>'` (or the
`SHELL=` of the crontab), so pipes, `&&` and redirections behave as before. The job name is
the base name of the command (`/opt/backup.sh` gives `backup`); on a collision the later
job gets a stable suffix from a hash. The path of the `cronbeat` binary is absolute because cron's PATH is
short. The API key is never written to the crontab. Backups are kept in
`~/.local/state/cronbeat/crontab-backups` with mode 0600. `undo` restores the latest backup
only while the crontab is still exactly what `apply` wrote, otherwise it needs `--force`.

### systemd timer

```ini
# /etc/systemd/system/nightly-backup.service
[Unit]
Description=Nightly backup

[Service]
Type=oneshot
User=backup
ExecStart=/usr/local/bin/cronbeat run --name nightly-backup --schedule "0 2 * * *" -- /opt/backup.sh
# the key file must be readable by User= and have mode 0600
Environment=CRONBEAT_KEY_FILE=/etc/cronbeat/backup.key
```

```ini
# /etc/systemd/system/nightly-backup.timer
[Unit]
Description=Nightly backup

[Timer]
OnCalendar=*-*-* 02:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

Create the key file with `sudo -u backup cronbeat configure --key-file /etc/cronbeat/backup.key`.
In unit files `%` must be written `%%`. In a Docker image, use `ENTRYPOINT ["cronbeat", "run", "--name", "job", "--"]`
and mount the key file or pass `CRONBEAT_API_KEY`.

### What leaves the host

| Sent | When |
| --- | --- |
| Job name, schedule, time zone, grace and maximum runtime | Every run (the definition travels with the event) |
| Run id (random UUID v4), event type (`started`, `succeeded`, `failed`), duration, exit code, signal name | Every run |
| Agent version (in the body, and as `cronbeat-host/<version>` User-Agent) | Every run |
| The last `--tail-bytes` (default 4096) of stdout and stderr, masked | Only when the command failed (`--output always` sends it for every run, `never` disables it) |

Never sent: the command or its arguments (only the name you give with `--name`), environment
variables, file contents, user names or the host name (the API key identifies the host agent).

The output tail is masked on the host before it is sent, with the same rules CronBeat applies
on the server: credentials in URLs, `Authorization` and `Bearer` values, values of keys such as
`password`, `secret`, `token`, `api_key` and `access_key`, e-mail addresses and tokens of
32 or more characters. Masking is a safety net, not a guarantee: use `--output never` for jobs
whose output may contain other secrets. When stdout or stderr is a terminal nothing is captured, and a job that
redirects its own output to a file (`> job.log`) leaves nothing to capture.

The API key is not passed on to your job: `CRONBEAT_API_KEY` is removed from the environment of the command,
so a job that prints its environment, or another tool reading `/proc/<pid>/environ`, never sees it.
(`CRONBEAT_KEY_FILE` and `CRONBEAT_API_URL` are not secrets and are passed on.)

The API key is read from `CRONBEAT_API_KEY`, `--key-file` / `CRONBEAT_KEY_FILE`, `/etc/cronbeat/key`
or `~/.config/cronbeat/key`, in this order. It is never accepted as an argument (it would show in `ps`),
key files must not be readable by group or others (mode 0600, otherwise they are ignored with a
warning), it is only sent in the `Authorization` header over HTTPS (plain `http` is accepted for
`localhost`, `127.0.0.1` and `::1` only) and never printed or logged.

### Network behaviour

`started` is sent in the background and never delays the command (2 second connect timeout,
4 seconds in total, one attempt). Before the result is sent, cronbeat waits for it to finish, so
the order is kept. The result is tried up to 3 times (4 seconds per attempt, waits of 0.3 and 0.9
seconds, `Retry-After` honoured up to 3 seconds), within 12 seconds in total. Errors such as 400,
401 and 403 are not retried. Events lost while CronBeat is unreachable are not queued for later.
Proxies from `HTTPS_PROXY` are used.

### Troubleshooting

- `cronbeat check` validates the key and shows where it comes from. `--verbose` on `run` shows the steps.
- `key file ... has mode 0644, expected 0600`: run `chmod 600` on it.
- `no API key configured`: cron has a minimal environment, so `export` in your shell does not help.
  Run `cronbeat configure` as the user that owns the crontab, or put `CRONBEAT_API_KEY=` on the first line of the crontab.
- `CronBeat refused this job (HTTP 403)` with `plan_limit_exceeded`: the plan allows no more monitors or
  host agents. The job keeps running.
- `command not found` (127) under cron: cron's `PATH` is `/usr/bin:/bin`. Use absolute paths.
- `could not reach CronBeat (... certificate ...)` on a minimal image: the CA certificates (`ca-certificates` package) are missing.
- Nothing arrives in CronBeat: run the job by hand with `--verbose`, check the clock of the host and that
  `api.cronbeat.dev` is reachable over HTTPS.

## Development

```sh
go vet ./...
golangci-lint run
go test ./... -race
helm lint charts/cronbeat-agent --set clusterName=x --set apiKey=cb_agent_x
docker build -t cronbeat-agent:dev .
```

Layout: `cmd/agent` (main), `internal/config`, `internal/api` (API client with
retry/backoff), `internal/k8s` (informers, reason inference, event creation),
`internal/sync` (event queue, periodic sync and heartbeat).

The host CLI lives in `cmd/cronbeat` and `internal/hostcli/*` (one small package per concern:
`runner`, `tail`, `mask`, `client`, `config`, `keyfile`, `warn`, `crontab`, `run`, `check`,
`configure`), with the version in `internal/hostversion`. It uses the standard library only.
`testdata/mask_vectors.json` (shared with the backend masker) and `testdata/host_runs_contract.json`
(the request bodies of `POST /agent/v1/host/runs`) are the contract fixtures.

```sh
go test ./... -race
sh scripts/install_test.sh      # needs python3; the installer against a local fake release
shellcheck scripts/*.sh
sh scripts/build-host.sh        # dist/: tarballs for linux amd64 and arm64 and SHA256SUMS
```

Release of the host CLI: bump `internal/hostversion` and `DEFAULT_VERSION` in `scripts/install.sh`
to the same version, update `CHANGELOG.md`, then push the tag `host-v<version>`. The workflow
`host-agent.yml` tests, builds, attests and publishes the GitHub release.

## License

Apache-2.0, see [LICENSE](LICENSE).
