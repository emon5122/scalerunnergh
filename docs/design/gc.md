# scalerunnergh cleans up after itself: the `gc` block (titli-hub card #1124)

Status: plan v3 (goldfish round 1 applied: see "Review round 1" at the end), implemented in internal/gc,
internal/backend/gc.go and internal/config/gc.go.

## Need
Live finding (2026-11-19, trodad-pc5's docker-gha daemon): 0 containers, 0 volumes (JIT runners clean up), but 17
images = 24.7 GB (98% reclaimable) and 12.2 GB of build cache. Nothing prunes them. The service must do its own
cleanup, on its own timer, and say in its own `/status` when cleanup fails or the disk fills up. No hub schedule, no
agent loop.

## Already exists (inventory)
1. Per-runner cleanup: docker `rm -f -v` after each job; native removes the whole job dir `<root>/jobs/<runner>`
   (the runner's `_work` lives inside it) in `Native.Stop`, with retries, and startup `Cleanup` removes leftovers.
2. `Pull` prunes the dangling images of the *same repository* after a pull brings a new image (pull.go `pruneOld`).
   Images of other repos, old tags, and images nobody pulls any more are never removed. That is the 24.7 GB.
3. `/status` JSON and Prometheus `/metrics` (hub reads `/status`).

## Design
Top-level optional config block (absent = today's behaviour, nothing new runs):

```json
"gc": {
  "interval": "1h",              // default 1h, minimum 5m
  "image_max_age": "168h",       // unused images older than this are removed (0/absent = no age rule)
  "images_max_bytes": "30GB",    // after the age rule, unused images are removed oldest first until under the cap (absent = no cap)
  "keep_images": ["ghcr.io/o/base:1"], // kept even when unused, on top of every scale set's image
  "build_cache_keep_storage": "10GB",  // optional: docker builder prune -f --keep-storage <v> each run
  "native_max_job_dir_age": "1h", // a job dir no live runner owns, older than this, is swept (default 1h)
  "disk_paths": ["/var/lib/docker"],   // statfs'd each run (default: native.root when native is used)
  "min_free_disk_percent": 10          // below this a path is "under pressure" (default 10)
}
```

Sizes accept `B`, `KB/MB/GB/TB` (10^n) and `KiB/MiB/GiB/TiB` (2^n), or a plain byte number.

### Image policy (docker backend; pure function `planImageGC`, unit tested)
Input: every image on the daemon (`docker image ls -q --no-trunc` then one `docker image inspect -f` for
id / created / size / RepoTags / RepoDigests), the image ids any container uses (running or stopped, existing
`imagesInUse`), the keep set (each docker scale set's `image` + `keep_images`), now.
- **Protected**: used by any container; or a RepoTag equal to a kept ref (refs normalised: a ref with no tag and no
  digest gets `:latest`; Docker Hub short names compare as docker shows them, e.g. `ubuntu:22.04`); or a RepoDigest
  equal to a kept `repo@sha256:` ref. Also protected: images created less than 1h ago (a pull or build in flight
  must not lose its fresh image before a container uses it).
- **Age rule**: every unprotected image created before now - image_max_age is removed.
- **Cap rule**: total = sum of `.Size` of all images left. While total > images_max_bytes, remove the oldest
  unprotected image left. Sizes overcount shared layers, so the cap is conservative (it may remove a bit more,
  never less). When only protected images remain and total is still over the cap, status says `images_over_cap`.
- Removal is `docker rmi <tag>...` for tagged images (removing every tag deletes the image) and `docker rmi <id>` for
  untagged ones, never `-f`: docker itself refuses an image a container started using after the listing (race safe).
  A refused removal is counted in `last_errors` for that run and does not abort the others.
- Scope: every image on the daemon the service talks to. That daemon is meant to be the CI daemon; README says so
  and says to leave `gc` off (or use `keep_images`) on a daemon shared with other services.
- Runs serialised with pulls: a gc run takes no per-image lock, but protected-if-young covers an in-flight pull,
  and a kept ref is never removed.

### Build cache
`build_cache_keep_storage` set: `docker builder prune -f --keep-storage <bytes>` each run (the host daemon's
BuildKit cache; dind daemons die with their container). The daemon.json BuildKit GC (below) is the main fix; this is
for hosts where daemon.json cannot be changed.

### Native job dirs (`_work`)
Every job dir is already removed after its job. GC adds the safety net: each run sweeps `<root>/jobs/*` that no live
runner owns and no runner is being started into, and whose mtime is older than `native_max_job_dir_age` (Windows
file locks can make the after-job removal fail). Start registers its dir as "starting" before it creates it, so the
sweep can never take a dir mid-copy. A failed removal (after the job or in the sweep) is counted and the paths are in
status (`native_leftover_dirs`).

### Disk pressure
Each run statfs's each `disk_paths` entry (Linux `syscall.Statfs`, Windows `GetDiskFreeSpaceExW`): free/total bytes,
`pressure: true` when free% < min_free_disk_percent. A path that cannot be read is an error in status.

### Reporting (no agent)
- `/status` gets `"gc": {last_run, last_duration, ok, errors[], images_bytes, images_count, images_removed_total,
  bytes_freed_total, images_over_cap, build_cache_pruned, native_leftover_dirs, disks[{path, free_bytes, total_bytes,
  free_percent, pressure}], disk_pressure}`; absent when gc is off.
- `/health` stays HTTP 200 (a container health check must not restart CI for a full disk) but its body is
  `degraded: <reasons>` instead of `ok` when the last gc run had errors, the images are over the cap, or a disk is
  under pressure.
- Prometheus: `scalerunner_gc_runs_total`, `scalerunner_gc_errors_total`, `scalerunner_gc_images_removed_total`,
  `scalerunner_gc_bytes_freed_total`, `scalerunner_gc_images_bytes`, `scalerunner_disk_free_bytes{path}` is not
  label-compatible with the scale_set label; instead `scalerunner_disk_pressure` (1/0, any path).
- One log line per run (info: removed n images, freed x; warn on each error).

### Loop
`app.Run` starts one goroutine when `gc` is set: first run 1 minute after start (after crash cleanup), then every
interval; each run has a timeout of min(interval, 30m); stops with ctx.

## IaC: BuildKit GC in each CI daemon's daemon.json (no code)
`deploy/docker/daemon.json` + `deploy/docker/README.md`: `{"builder": {"gc": {"enabled": true,
"defaultKeepStorage": "10GB"}}}`, merge it with jq into the existing /etc/docker/daemon.json on each CI daemon (VPS,
trodad-pc5's docker-gha daemon, rl9), `dockerd --validate`, then a daemon restart (runners drain first: set the kill
switch). Titli/Emon roll it out; this job deploys nothing.

## Tests
config: gc parse/defaults/size units/refusals (interval < 5m, negative, bad size). planImageGC: in-use kept,
scale-set image kept (tag and digest refs, `:latest` normalisation), keep_images kept, young kept, age rule, cap rule
oldest first, over-cap with only protected left. Docker gc run (fake CLI): rmi by tags then by id, no `-f`, a refused
rmi is an error and the rest go on, builder prune args. Native sweep: old orphan removed, live / starting / young
kept. Disk: pressure threshold. App: status/health degraded text.

## Review round 1 (goldfish) and what changed (v3)
- P0 `Created` is the build time, not the pull time: the age, young and cap ordering use `.Metadata.LastTagTime`
  (when the image got its newest tag on this daemon), falling back to `Created`. The young grace is
  `image_min_age` (default 6h). An up-to-date re-pull does not refresh LastTagTime; kept refs are protected anyway.
- P1 `rmi <tag>` acts on what the tag points to now: an image with at most one tag is removed by id (`rmi <id>`,
  tag independent). A multi-tag image is untagged tag by tag, each after a re-inspect proves the tag still resolves
  to the planned id.
- P1 kept refs by string: each kept ref is also resolved by the daemon (`image inspect -f {{.Id}} <ref>`) and
  protected by id; string matching on RepoTags/RepoDigests stays as a second check.
- P1 expected races ("No such image", "conflict", "being used", "dependent child images") are skips, logged, not
  errors; only other failures go to `errors[]`.
- P1 build cache: `docker builder prune -f -a --max-used-space <v>` (buildx era), falling back to `--keep-storage`
  when the CLI says the flag is unknown. Config key `build_cache_max_bytes`.
- P1 native sweep race: the backend keeps a `claimed` set (under the same mutex as `procs`): Start claims its dir
  before the existing-dir check and releases it after `procs` holds it; Stop claims for the kill and removal. The
  sweep skips live and claimed dirs, symlinks and non-dirs, claims the dir while it removes it, and kills a leftover
  `.srg-pid` process like Cleanup.
- P1 real-daemon test: `tests/integration` gets a gc test (tagged, untagged, multi-tag, kept, in-use by a stopped
  container) that CI runs against a real dockerd.
- P1 disk default: when `disk_paths` is unset, native.root (native) and the daemon's `DockerRootDir` (docker, only
  when that path exists locally) are checked; an unreadable default path is skipped, an unreadable configured path
  is an error. Free = `Bavail`.
- P2 sizes overcount shared layers: documented, `bytes_freed_total` is named `bytes_freed_estimate_total`.
- P2 locking: no pull locks; kept refs are protected by id and the young grace covers other images' pulls.
- P2 status: `ok` is null before the first run; `stale` when the last run is older than 2x interval (+ the run
  timeout); state behind a mutex, copied on read.
- P2 `dry_run`: plan and report only (first rollout on a host). Config refuses image/build-cache settings without a
  docker scale set and a native-only setting without a native one.
- P2 per-image inspect (one vanished image does not fail the run).
