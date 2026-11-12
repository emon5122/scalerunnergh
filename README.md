# ScaleRunnerGH

Ephemeral, auto-scaling GitHub Actions runners for your own hosts, built with Go.

One ScaleRunnerGH runs per host. It serves one or more GitHub **runner scale sets**
(org-level or repo-level), starts a fresh runner for every job and removes it afterwards.
A runner is either a container (Docker backend) or a process on the host (native backend,
Linux and Windows). All scale sets on a host share one slot pool, so a small box never runs
more than it can take.

## How it works

```
GitHub ──(long poll, outbound only)──▶ ScaleRunnerGH ──▶ backend ──▶ one runner per job
          jobs assigned to the           slot pool        docker: container (+ private dockerd)
          scale set, by label            guard            native: process in a clean directory
```

1. **Register.** Each scale set is registered in a runner group at GitHub, with its name and
   extra labels as `runs-on` labels. GitHub matches jobs to it; nothing is polled per repo.
2. **Listen.** A long-poll message session reports how many jobs are assigned. It is
   outbound only, so it works behind NAT. ScaleRunnerGH tells GitHub its current capacity,
   so GitHub never assigns more than the host can run.
3. **Guard.** Jobs from public repositories and pull requests from forks are never acquired.
4. **Start.** For each assigned job a slot is taken from the host pool and a runner starts
   with a single-use just-in-time (JIT) config. No registration token is involved.
5. **Clean up.** The runner exits after one job. Its container (with volumes) or its job
   directory is removed and the slot is returned.

It is built on [`actions/scaleset`](https://github.com/actions/scaleset), GitHub's Go client
for the runner scale set API.

## Features

- **One runner per job**, in a container or as a native process; nothing carries over.
- **Org-level and repo-level** scale sets, several orgs per host.
- **Host slot pool**: `max_runners` for the host, `max_heavy` for heavy scale sets
  (for example one heavy job at a time on a small box).
- **Hard limits** per job container: CPUs, memory (no swap on top), pids, CPU shares, I/O
  weight, OOM score, optional disk size and GPUs.
- **Production-host protection**: a load and memory gate, low priority, and a kill-switch
  file that stops new jobs at once (and running ones, if configured).
- **Private repos only**: the runner group must refuse public repositories, and a guard
  re-checks every job. Every check fails closed.
- **Docker inside jobs without the host socket**: an optional private dockerd inside the job
  container, destroyed with it.
- **Pinned toolchains**: runner images with fixed tool versions (container mode), and a
  runner release pinned by version and sha256 (native mode).
- **Safe restarts**: leftovers of an earlier process are removed at start; on shutdown
  running jobs may finish (`drain_timeout`).
- **PAT or GitHub App** credentials, read from files, never from the config or the
  environment.
- **Prometheus metrics** and a Grafana dashboard.

## Quick start (Docker host)

```bash
mkdir -p /srv/scalerunnergh/{secrets,control} && cd /srv/scalerunnergh
cp <repo>/deploy/compose.yml .
cp <repo>/examples/vps.json config.json      # edit: org URL, runner group, image tag
install -m 600 /dev/stdin secrets/github_pat # paste the token, Ctrl-D
docker compose up -d
```

Then use the scale set in a workflow:

```yaml
jobs:
  build:
    runs-on: srg-vps        # the scale set name, or [srg-vps, vps] with extra labels
```

The compose file carries DockWarden labels, so the orchestrator image is updated without
hand work. `stop_grace_period` must be longer than `host.drain_timeout`.

### Native mode (no Docker)

- **Linux**: put the released binary in `/usr/local/bin`, a config like
  [`examples/native-linux.json`](examples/native-linux.json) in `/etc/scalerunnergh/`, and
  enable [`deploy/systemd/scalerunnergh.service`](deploy/systemd/scalerunnergh.service).
  Run it as a dedicated non-root user.
- **Windows**: `scalerunnergh.exe` runs as a Windows service. Use
  [`deploy/windows/install.ps1`](deploy/windows/install.ps1) with a config like
  [`examples/native-windows.json`](examples/native-windows.json).

In native mode the job uses the tools installed on the host. Every job still gets its own
copy of the pinned runner release and its own work directory, both deleted afterwards.

## Configuration

One JSON file per host. Unknown fields are errors. Validate with
`scalerunnergh --config config.json --check`. Full examples:
[`vps.json`](examples/vps.json), [`production-host.json`](examples/production-host.json),
[`native-linux.json`](examples/native-linux.json),
[`native-windows.json`](examples/native-windows.json).

### `host`

| Field | Meaning | Default |
|---|---|---|
| `name` | Host name used in runner and container names | OS hostname |
| `max_runners` | Concurrent runners on this host, all scale sets together | required |
| `max_heavy` | Concurrent runners of scale sets marked `heavy` | `0` (no extra cap) |
| `max_load_per_core` | No new runner while `load1 / cores` is above this (Linux) | off |
| `min_mem_available_mb` | No new runner while `MemAvailable` is below this (Linux) | off |
| `kill_switch_file` | While this file exists, no runner starts and idle runners are removed | off |
| `kill_switch_stops_running` | The kill switch also stops running jobs | `false` |
| `idle_timeout` | Surplus idle runners are removed after this long | `3m` |
| `max_job_duration` | A runner is force-stopped after this long | `6h30m` |
| `drain_timeout` | On shutdown, let running jobs finish for up to this long | `0` |

### `auth` (host default, or per scale set)

```json
{ "pat_file": "/run/secrets/github_pat" }
```

```json
{ "app": { "client_id": "Iv23...", "installation_id": 12345678, "private_key_file": "/run/secrets/app_key" } }
```

- **PAT (classic)**: `repo` (the guard reads repositories and runs), plus `admin:org` for org-level scale sets.
- **GitHub App** (recommended): organization permission *Self-hosted runners: read and
  write*, repository permissions *Metadata: read* and *Actions: read*. An App installation
  belongs to one org, so set `auth` per scale set when a host serves several orgs.
  On a user account (personal repos: repo-level scale sets, `runner_group: "default"`)
  the App needs repository permission *Administration: read and write* instead.
  Manifests and a create script: [`deploy/github-app`](deploy/github-app/README.md).

### `scale_sets[]`

| Field | Meaning |
|---|---|
| `name` | Scale set name; also a `runs-on` label |
| `url` | `https://github.com/<org>` or `https://github.com/<org>/<repo>` |
| `runner_group` | Runner group to register in. It must not allow public repositories |
| `labels` | Extra `runs-on` labels |
| `backend` | `docker` or `native` |
| `min_runners` / `max_runners` | Idle runners kept ready / cap for this scale set |
| `heavy` | Counts against `host.max_heavy` |
| `image`, `dind` | Docker backend: runner image, private dockerd inside the job container |
| `limits` | `cpus`, `memory_mb`, `pids`, `cpu_shares`, `blkio_weight`, `oom_score_adj`, `storage_opt_size`, `gpus` (docker); `nice` (native) |
| `allow_repos` | Optional list of repos allowed to use this scale set |
| `auth` | Overrides the host-level `auth` |

### `docker` and `native`

| Field | Meaning |
|---|---|
| `docker.network`, `docker.runtime` | Network and runtime (for example `sysbox-runc`) for job containers |
| `docker.registry_mirror` | Pull-through cache passed to the dockerd inside dind jobs |
| `docker.extra_args` | Extra `docker create` flags (escape hatch) |
| `native.root` | Directory for the runner release, job directories and logs |
| `native.runner_version`, `native.runner_sha256` | Pinned `actions/runner` release for this OS and architecture |
| `native.env` | Extra environment for every native runner (toolchain paths such as `JAVA_HOME`, runner hooks such as `ACTIONS_RUNNER_HOOK_JOB_STARTED`). The inherited environment drops `ACTIONS_*`, `GITHUB_*`, `SRG_*` and credential-looking names, so set those here. Never secrets |
| `native.run_as` | Windows only: run every job as `NT AUTHORITY\NETWORK SERVICE` (or `LOCAL SERVICE`) while the service runs as LocalSystem. `native.root` must be absolute; it is reset to SYSTEM and Administrators only, the jobs dir gets modify for that account, and each directory above it read/list for that directory only. Keep the credential file readable by SYSTEM and Administrators only. Windows PowerShell steps then need `"PSExecutionPolicyPreference": "Bypass"` in `native.env` unless the machine policy allows scripts |

## Safety model

- **Fork pull requests and public repos never reach a runner.** Three layers: the org
  setting that disables fork-PR workflows, a runner group with *allow public repositories*
  off (checked at start; ScaleRunnerGH refuses to register otherwise), and the per-job guard.
- **No host Docker socket in jobs.** Only the orchestrator talks to the host daemon. With
  `dind: true` the job container is privileged; that is acceptable only for trusted,
  private repos. Set `docker.runtime` to `sysbox-runc` to avoid privileged containers.
- **Secrets**: credentials are read from files. The JIT config is copied into the
  container as a root-only file (docker) or passed to that one child process (native); it
  is single-use and never appears on a command line, in `docker inspect` or in logs.
- **Native mode shares the host and the service account** with the job: a workflow can
  read whatever that account can, including the credential file and the unpacked runner
  release. Use native mode only for repos you fully trust, under a dedicated unprivileged
  account, with a credential scoped to that one org. On Windows, `native.run_as` runs the
  jobs under a service account with a restricted token (all privileges but
  SeChangeNotify removed, so no SeImpersonate route to SYSTEM) and resets `native.root`
  to SYSTEM and Administrators only, so a credential readable by SYSTEM and
  Administrators only is out of a job's reach. Residual risk: a hostile job that leaves a
  process outside the job's Job Object could race the job-dir deletion.
- **`pull_request_target`, `workflow_run`, `issue_comment`** run the base repository's
  workflow, so the guard treats them as runs of the repository itself. Do not check out and
  run fork code in such workflows.
- **Known limit**: a runner is known to be busy from GitHub's job-started message. If that
  message is delayed past `idle_timeout` (or the 20 s settle time of a drain), a runner
  that just took a job can be stopped; GitHub then fails that job.

## Production hosts

See [`examples/production-host.json`](examples/production-host.json): per-job caps,
`cpu_shares: 128` and `oom_score_adj: 1000` so CI always loses against production
workloads, the load and memory gate, and the kill switch:

```bash
touch /srv/scalerunnergh/control/STOP   # no new jobs (and stop running ones, if configured)
rm /srv/scalerunnergh/control/STOP      # resume
```

## Runner images

`ghcr.io/emon5122/scalerunnergh-runner:<flavor>-<version>`, built from
[`docker/runner/Dockerfile`](docker/runner/Dockerfile):

| Flavor | Contents |
|---|---|
| `base` | Ubuntu 24.04, actions/runner, git, build tools, Docker engine + buildx + compose |
| `node` | base + Node.js, pnpm |
| `go` | base + Go |
| `python` | base + Python 3, uv |
| `media` | node + ffmpeg, sox |
| `full` | node + Go + Python + ffmpeg |
| `rust` | full + rustup (toolchain from the repo's `rust-toolchain.toml`), clang/lld 21, cmake, glib headers, sqlite3, shellcheck, Tauri v2 Linux build libraries (webkit2gtk 4.1, libsoup 3, appindicator, rsvg, udev, patchelf) |

There is no `sudo` in the images: tools come from the image, pinned in the Dockerfile.
The images only start through ScaleRunnerGH (the entrypoint needs the JIT config file).

## Metrics

`/metrics` (Prometheus), `/health`, `/status` (JSON) on `metrics.address`.

| Metric | Type | Meaning |
|---|---|---|
| `scalerunner_runners_active{scale_set}` | gauge | Runners alive |
| `scalerunner_runners_busy{scale_set}` | gauge | Runners running a job |
| `scalerunner_jobs_assigned{scale_set}` | gauge | Jobs GitHub assigned to the scale set |
| `scalerunner_capacity_blocked{scale_set}` | gauge | 1 while jobs wait for a slot, the gate or the kill switch |
| `scalerunner_runners_started_total{scale_set}` | counter | Runners started |
| `scalerunner_runners_completed_total{scale_set}` | counter | Runners that exited cleanly |
| `scalerunner_runners_failed_total{scale_set}` | counter | Runners that failed or were force-stopped |
| `scalerunner_start_errors_total{scale_set}` | counter | Runner start errors |
| `scalerunner_jobs_refused_total{scale_set}` | counter | Jobs refused by the guard |
| `scalerunner_listener_errors_total{scale_set}` | counter | Message session errors |
| `scalerunner_host_slots_used`, `scalerunner_host_slots_max` | gauge | Host slot pool |
| `scalerunner_host_paused` | gauge | 1 while the kill switch is on |

A Prometheus scrape config and a Grafana dashboard are in [`deploy/`](deploy/).

## Development

```bash
./scripts/ci.sh                 # the whole CI suite (needs Docker)
SKIP_DOCKER=1 ./scripts/ci.sh   # without the Docker lanes
go test ./... -race
```

```
cmd/scalerunnergh/     entrypoint, Windows service mode
internal/app/          wiring: backends, pool, kill switch, one listener per scale set
internal/scaler/       scale set messages -> runners
internal/backend/      docker and native runner backends
internal/pool/         host slot pool
internal/hostgate/     load/memory gate, kill switch
internal/guard/        public-repo and fork-PR guard
internal/ghapi/        small GitHub REST client (PAT, App)
internal/runnerdist/   pinned runner release for native mode
internal/config/       host config
docker/runner/         runner images
deploy/, examples/     compose, systemd, Windows service, example configs
docs/rollout-plan.md   audit, design decisions and rollout plan
```

Releases are cut by release-please on the built-in `GITHUB_TOKEN` (the repo setting "Allow
GitHub Actions to create and approve pull requests" must be on). It dispatches CI on the
release PR branch and, after the merge, the release build on the new tag, which publishes
the images to ghcr and attaches the binaries to the GitHub release.

## License

MIT
