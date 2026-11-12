# ScaleRunnerGH: audit, runner inventory and rollout plan

Status: **proposal, waiting for go** (2026-11-12). No code changes yet.

Goal: one ScaleRunnerGH deployment per runner host (a container from the CI-built
image, auto-updated by DockWarden, never hand-run). It starts an ephemeral runner
per job from pinned toolchain images, and replaces the hand-made runner
registrations, the pc-09 mkdir lock and lock hooks, and the weekly prune crons.

---

## 1. Audit of the repo as it is (main @ 06e35cf)

### What works

| Check | Result |
|---|---|
| `go build ./...`, `go vet ./...` | pass (Go 1.27 locally, `go.mod` says 1.25.5) |
| `go test ./... -count=1` (plus `-race`) | pass, all 6 packages |
| `go test -tags integration ./tests/integration/` | pass (mock GitHub API only, never run against the real API) |
| Binary built in `golang:1.25-alpine` | runs, `--version` OK |

### Blocking defects

1. **The orchestrator image does not build.** `Dockerfile` uses `golang:1.23-alpine` while
   `go.mod` requires `go >= 1.25.5` (`GOTOOLCHAIN=local`). The CI `build-docker` job would
   fail; CI only runs on `v*` tags, and its last two runs (Feb/Mar) are red.
2. **Org-level mode is a stub.** `ListOrgQueuedJobs` returns `nil`, so a `repo: ""` config
   never launches a runner. Every runner we have today is org-level (see §2).
3. **Label matching is reversed in intent.** `MatchesLabels(job, runner)` only checks that
   the job's labels are a subset of ours. A config with `[self-hosted, pc09, Linux]`
   therefore grabs a job that asks only for `[self-hosted]` (a job meant for `vps`), and
   the job then runs on whichever registered runner GitHub picks, not necessarily ours.
4. **Polling cost.** For each repo and each poll, it lists queued runs and then makes one
   call per run. With about 60 repos on a 10 s poll, that is far over the 5 000 req/h PAT
   limit, and it is still blind to org-level queues.
5. **No per-host concurrency or resource enforcement.** `max_cpus`/`max_mem_mb` are counted
   but never requested (`CanAllocate(gpus, 0, 0)`), and containers start without `--cpus` or
   `--memory`. A small box (pc-09, 7.4 GB) cannot be held to one heavy job at a time.

### Security gaps

- **The token is visible.** The registration token goes in as `docker run -e RUNNER_TOKEN=…`,
  so it shows in `docker inspect` and `ps`. JIT configs are single-use and runner-scoped,
  which is better.
- **Root on the host.** The runner image installs `docker.io` and passwordless `sudo`, and the
  compose file mounts the host `/var/run/docker.sock` into the orchestrator. If a job also gets
  the socket (the usual `extra_args` route to make docker work), it is root on the host and can
  see every other job's containers.
- **Only PATs.** No GitHub App support (App tokens are short-lived, per-installation, and have a
  higher rate limit).
- **Nothing cleans up after a crash.** Dispatch state lives only in memory. If the orchestrator
  restarts mid-job, it leaks containers and leaves offline runner registrations behind.

### Missing for the way we deploy

- No image push to ghcr, no release-please, no DockWarden labels, no `go.sum`
  (zero deps today, fine).
- One generic runner image (`actions/runner` 2.321.0; latest is v2.338.2). There are no
  toolchain images. Runners started with `--disableupdate` must be rebuilt within about 30
  days of each runner release or GitHub refuses them, so image bumps need to be automatic
  (Renovate, then a CI image, then DockWarden).
- The repo is **public**: GitHub-hosted CI for it is free (no paid minutes), but per-host
  deploy config (org names, group ids, host names) belongs in the private infra repos, not here.

---

## 2. Inventory of today's runners (read-only, 2026-11-12)

Sources: `gh api orgs/<org>/actions/runners`, runner groups, the repo-level runners of all
248 non-archived repos (5 orgs plus emon5122), and code search for `runs-on` labels.

| Host | Runner (org) | OS | Labels | Runner group |
|---|---|---|---|---|
| VPS (srv2066370) | vps-slot1, vps-slot2, vps-slot3 (nexisltd) | Linux | self-hosted, Linux, X64, vps, docker | `vps-private` (id 3, 47 selected repos) |
| office PC 5 | office-pc5 (nexisltd) | Linux | self-hosted, Linux, X64, docker, office | `office-private` (id 4, 5 repos) |
| pc-09 WSL `titli-ci` | pc09-nexisltd-linux, -linux-2, -linux-3 | Linux | self-hosted, Linux, X64, pc09 | Default |
| pc-09 WSL `titli-ci` | pc09-{radiolens,jotno-care,medispa-trodad,trodad-intl}-linux | Linux | self-hosted, Linux, X64, pc09 | Default |
| pc-09 Windows | pc09-{nexisltd,radiolens,jotno-care,medispa-trodad,trodad-intl}-win | Windows | self-hosted, Windows, X64, pc09 | Default |

That is **20 runners**: 3 VPS, 1 office-pc5, and 12 on pc-09 (8 Linux, 4 Windows; the brief
said 10). All are online and idle at audit time. **No repo-level runners exist** in any org
or under emon5122. No runner is registered from R610 or the home PC.

Who uses them (code search on `.github/workflows`):
- `vps`: 47 nexisltd repos (the gh-hosted sweep of 2026-11-09 moved all private Linux jobs here).
- `pc09`: 15 repos. nexisltd: caddy-modules, home-cams, home-infra, home-pbx, HRM-Push,
  lepti-desktop, lepti-docs, lepti-extension, lepti-web, security-stack, shikor, titli-hub.
  Plus medispa-trodad/web, radiolens/backend and trodad-intl/trodad-api. Four of them already
  take `runs-on` from the `vars.CI_RUNS_ON` variable, so switching them is a variable change.
- `office`: nexisltd/wa-gateway.

Fork-PR safety today:
- Every org has `run_workflows_from_fork_pull_requests: false` for private repos.
- Every runner group has `allows_public_repositories: false`.
- Public repos exist in nexisltd (`.github`, celery-flower-docker, date2word, country) and
  trodad-intl (rich-text-editor). The group setting is what keeps them off the runners. The new
  setup must keep both guards and add its own (§3.4).

Known hand-made drift this replaces:
- On pc-09: the shared mkdir lock and lock hooks (orphan waiters), the weekly prune cron and
  `provision-wsl.sh` (titli `tools/ci-runner/`, `docs/ci-runner.md`).
- On the VPS: tools installed by hand on the runner (ffmpeg missing broke the home-pbx
  v1.10.0 image build). The VPS runners also drive a separate dind daemon, so jobs already
  assume `DOCKER_HOST` is not the host socket.

Not readable from this PC (to confirm on the hosts during the pilot): how the VPS and
office-pc5 runners are set up on the host (systemd units, the dind container), and the
exact pc-09 provisioning. None of this changes the plan.

---

## 3. Design decisions (proposed)

### 3.1 Use GitHub runner scale sets instead of polling jobs

Rebuild the control loop on **`github.com/actions/scaleset`** (MIT, v0.5.0, Oct 2026,
public preview, extracted from actions-runner-controller and usable without Kubernetes):

- **Outbound only.** A long-poll message session, no inbound webhook. This works behind NAT
  on pc-09 and office-pc5.
- **GitHub does the matching.** GitHub matches jobs by scale-set name and labels and by the
  runner-group policy, and returns `statistics.TotalAssignedJobs`. This fixes defects 2–4:
  org-level works natively, there is no per-run API fan-out, and no label guessing.
- **We tell GitHub our capacity.** We send `maxCapacity`, so GitHub does not assign more jobs
  than the host can take. This is what replaces the pc-09 mkdir lock.
- **JIT runner configs** (`GenerateJitRunnerConfig`): single-use, no registration token,
  no `config.sh`, and the runner removes itself after one job.

We keep the current repo's config, metrics, resource-manager and launcher packages where
they fit, and replace `internal/github` plus the orchestrator loop.

### 3.2 Auth

- **Recommended:** a GitHub App per org with the `Self-hosted runners: R/W` org permission.
  Tokens are short-lived and the rate limit is higher.
- **Fallback for the pilot:** the existing classic PAT with `admin:org`, read from Bitwarden.
- Credentials reach the container as a mounted file (`/run/secrets/…`), never as
  `-e` or a CLI argument, and the code never logs them. The JIT config also goes to the
  runner as a file or stdin, not as an env var.

### 3.3 One host, several orgs, one capacity pool

- One deployment per host with several scale sets: one per org, plus one per label set if
  needed.
- All scale sets on a host share one **host slot pool** (`max_runners`, plus
  `max_heavy` = 1 on pc-09), so pc-09 runs one heavy job across all five orgs at a time.
  Each scale set reports `maxCapacity = free slots`. Jobs beyond that stay queued at GitHub,
  with no orphaned waiters.
- Every job container gets hard limits (`--cpus`, `--memory`, `--pids-limit`, disk via a
  tmpfs or volume quota) from a per-scale-set profile. Proposed pc-09 profile:
  `--cpus=4 --memory=3g` (today's rule). VPS: to be decided after reading its size.

### 3.4 Private-repo-only safety (fork PRs must never reach these runners)

Defence in depth:
1. Org setting stays: no workflows from fork PRs.
2. Scale sets are registered into **private runner groups** with
   `allows_public_repositories: false` and a selected-repos list (not the Default group,
   where the pc-09 runners are today).
3. Before starting a runner for a `JobAssigned` message, the orchestrator checks the repo
   (cached `GET /repos/{o}/{r}`): if the repo is public, or the event is `pull_request` from
   a fork, it refuses and logs. This needs the job payload fields to be verified in the spike.
4. Runner containers have no host socket, no host mounts and no host network.

### 3.5 Docker inside jobs: a sidecar dind per job, no host socket

- Each job gets its own `docker:dind` sidecar on a private bridge network, and the runner
  container sets `DOCKER_HOST=tcp://dind:2376` with TLS. Both are destroyed with the job, which
  replaces the prune cron and stops jobs seeing each other's containers. This is the model the
  VPS lane already uses, so home-pbx's e2e jobs keep working.
- The sidecar is privileged. That is acceptable only because of §3.4. Option for later:
  the sysbox runtime, which runs unprivileged dind.
- Pull cost: a per-host registry pull-through cache container (`registry:2` as a mirror)
  that every dind sidecar uses as its mirror.

### 3.6 Toolchain images (pinned, built by CI, ghcr)

- `ghcr.io/nexisltd/srg-runner-base`: Ubuntu 24.04, actions/runner pinned, git, jq,
  docker CLI and buildx/compose plugins, no sudo.
- On top of base, add only what the inventory shows is used:
  - `-node`: node LTS, pnpm via corepack.
  - `-android`: JDK 17/21, Android SDK cmdline-tools (lepti-mobile).
  - `-go`
  - `-python`: uv.
  - `-media`: ffmpeg (home-pbx).
  - `-rust`: only if a repo needs it.
- The job image is picked per scale set, so each label set maps to an image.
- Renovate pins every tool version. CI builds the images on tag; the repo is public, so
  GitHub-hosted builds are free. DockWarden pulls the new tag on the hosts.

### 3.7 Deployment and lifecycle

- The orchestrator image `ghcr.io/emon5122/scalerunnergh:<semver>` is released through
  release-please and CI, and carries DockWarden labels.
- Per-host `compose.yml` and the non-secret config live in the private infra repo for that
  host (nexis-infra for the VPS, home-infra or titli for pc-09). Secrets come from Bitwarden.
- On startup the orchestrator removes every container labelled `scalerunner=true` that it
  does not own, and deletes offline runners of its own scale sets. That makes it safe to
  restart.
- It keeps the Prometheus `/metrics` endpoint. Optionally it pushes status to the hub.

### 3.8 Windows runners

Out of scope. The 4 pc-09 `*-win` runners stay native for now. Revisit only if a Windows
container route is wanted, since Docker Desktop on that box is the problem, not the
orchestrator.

---

## 4. Rollout plan

Each step lands on main via release-please, with reviewer MERGE-READY and the local CI
suite passing before every push.

| Step | What | Done when |
|---|---|---|
| 0. Repo fix | Go 1.25 builder, `go.sum`, release-please, ghcr push of the orchestrator image, CI on PR/tag only (hosted runners, free for a public repo) | image builds and is published |
| 1. Spike | Throwaway scale set `srg-spike` in a private nexisltd group bound to one scratch private repo. Confirm: JIT runner starts and finishes a job, multi-label works, the job payload gives repo and event for the fork guard, behaviour when capacity is 0 | an echo job and a docker-in-job run green on a local box |
| 2. Core | scaleset listener, host slot pool, limits per profile, dind sidecar, JIT config via file, startup cleanup, metrics, tests (mock scaleset server, `-race`) | unit and integration tests green; local e2e against the spike scale set |
| 3. Images | srg-runner-base, -node, -media (enough for home-pbx), then the others | images on ghcr, Renovate config |
| 4. **Pilot: VPS / home-pbx** | Deploy on the VPS next to vps-slot1..3 as scale set `srg-vps`. Point home-pbx's deploy lane at it via `vars.CI_RUNS_ON`. Run one week, including a release tag (the ffmpeg case) | home-pbx tag CI builds its images on srg, no manual installs |
| 5. VPS rest | Move the other 46 repos from `[self-hosted, vps]` to the srg labels (a scripted workflow edit, or reuse the `vps` label on the scale set if multi-label works), then remove vps-slot1..3 and their dind/systemd | `vps-private` group holds only srg |
| 6. pc-09 Linux | One deployment in WSL `titli-ci`, 5 org scale sets, `max_heavy=1`, `--cpus=4 --memory=3g`. Move the 15 pc09 repos, then remove the 8 `pc09-*-linux` runners, the lock hooks, the prune cron and `provision-wsl.sh`'s runner part | runners show only srg on pc-09; titli `docs/ci-runner.md` rewritten |
| 7. office-pc5 | Same pattern for wa-gateway | office-pc5 manual runner removed |
| 8. Windows | Leave native; decide later | n/a |

Rollback at every step: the old runners stay registered until the step's "done" check has
held for a week, and `vars.CI_RUNS_ON` or a one-line `runs-on` revert moves jobs back.

---

## 5. Decisions needed before coding

1. **Auth**: a GitHub App per org (recommended) or the existing classic PAT for the pilot,
   then the App?
2. **Labels**: give scale sets new names (`srg-vps`, `srg-pc09`) and edit `runs-on`, or reuse
   the old labels (`vps`, `pc09`) on the scale set if multi-label works, so 60+ workflows need
   no edit?
3. **dind**: privileged per-job dind sidecar for now (recommended), or install sysbox on the
   hosts first?
4. **Where per-host deploy config lives**: nexis-infra (VPS) and home-infra/titli (pc-09)?
5. **Base**: rebuild on `actions/scaleset` (recommended; it is in public preview, so pin a
   version), or keep the hand-rolled job polling in this repo?
