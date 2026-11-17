# Private image pulls with the scale set's GitHub App (titli-hub card #1064)

Status: plan v3, DONE (2026-11-18): goldfish round 3 had no P0/P1.

## Need
rl9's `srg-rl9-nexis` scale set should run the private `ghcr.io/nexisltd/titli-ci-runner:main` image. rl9 has no ghcr
credentials. Emon chose GitHub App auth: the ScaleRunnerGH App (nexisltd installation) now has Packages: Read-only.

## Already exists (inventory)
1. Per-scale-set pull policy `pull: missing | always | interval:<d>` (#8, v1.4.0): `internal/backend/pull.go`
   (shared, serialized pulls per image, prune of dangling images), `app.pullIntervals` for background pulls.
2. App auth per scale set (`auth.app`: client_id, installation_id, private_key_file; `EffectiveAuth(ss)`), and
   `ghapi.AppToken`, which mints and caches an installation token (refreshes 5 min early).
3. `Docker.run(ctx, stdin, args...)` runs the docker CLI (tests replace it). `docker create` pulls a missing image
   itself, without credentials.
4. Nothing configures docker registry credentials today (the host's ~/.docker has only registry.trodad.com).

## Design
- New scale set option `registry_auth`: `""` (default, today's behaviour) or `"github-app"`. Valid only with the docker
  backend, an image on `ghcr.io/`, and an effective `auth.app` (config validation refuses anything else).
- **Credential:** a separate `ghapi.AppToken` per App installation used for pulls, whose `access_tokens` request
  asks for `{"permissions":{"packages":"read"}}` only (a new optional `Permissions` field; the guard's token keeps
  today's request), cached like today.
- **An authed pull:** a private docker config dir (`os.MkdirTemp`, 0700) with `config.json` = `{"auths":{"ghcr.io":
  {"auth": base64("x-access-token:"+token)}}}` (0600), created under the service's own state dir (not a shared /tmp);
  `docker --config <dir> -H <endpoint> pull -q <image>`; the dir is removed at once (defer). `<endpoint>` keeps the
  daemon the same as for every other docker call: it is resolved once at backend start with the user's normal config
  (`DOCKER_HOST` if set, else `docker context inspect --format '{{.Endpoints.docker.Host}}'` of the current context,
  else the default socket), since a temp `--config` has no context store. A test covers a non-default context. The token is never on argv, in the environment, in a log or in the host's ~/.docker. Error labels name
  the subcommand (`docker pull`), not `--config`.
- **Before every create** of a `github-app` scale set, whatever the policy: if the image is not on the host (`image
  inspect` fails), pull it with the credential through `pullShared` (the same per-image lock, so a first `interval`
  pull in flight is waited for, not raced; timeout `backgroundPullTimeout`, a cold CI image is large; skipped when an
  `always` pull just ran); `CreateArgs` gets `--pull never` for these scale sets, last, and config validation refuses
  a `--pull` in `docker.extra_args`/`extra_args` together with registry_auth, so create can never make an anonymous
  pull (needs docker >= 20.10; checked in rollout step 0). **always / interval** use the same authed pull on their existing paths.
- `app.go` keeps a map image -> credential func (from the scale sets with `github-app`), used by the per-image
  interval puller and by `Start` through `RunnerSpec.RegistryCred func(ctx) (string, error)` (nil = anonymous, as
  today). An image that several scale sets on the host share is pulled with the credential of one that has
  `github-app`; once local, every scale set on that host can use it (same host, same trust: documented). Two
  `github-app` scale sets with the same image but different installations are refused by config validation.
- `AppToken` with `Permissions` sends a JSON body (`Content-Type: application/json`); without, the request is
  unchanged.
- A token or pull failure logs one warning (image + error, never the token); the runner then starts from the local
  image, or, when the host has none, create fails (`--pull never`) and the start is retried as today.

## Tests
Config: registry_auth needs docker + ghcr.io + app (each refusal). Backend (fake run): an authed pull runs `docker
--config <dir> pull -q img`, the dir held a config.json with the right base64 while the pull ran and is gone after;
the token appears in no argv; create args carry `--pull never` with registry_auth only; an absent image is pulled
with the credential before create for `missing` AND `interval`; a present image is not pulled for `missing`; a token
error = no pull and create with `--pull never`. ghapi: an AppToken with Permissions sends them in the request body.
App: the cred map uses the scale set's pull token source.

## Rollout (rl9; Titli runs it, Emon's sudo for the binary)
0. **Proof first** (decides the design; also check `docker version` >= 20.10): on rl9, with a throwaway `--config` dir, log in to ghcr.io with an
   installation token minted by hand for installation 181347286 (App key from Bitwarden
   `scalerunnergh-app-nexisltd`, permissions packages:read) and `docker pull ghcr.io/nexisltd/titli-ci-runner:main`.
   If ghcr refuses: give the package read access for a repo the installation covers (package settings -> "Manage
   Actions access"/repository access), retry; if still refused, fall back to the PAT path (a `registry_auth:
   "file:<path>"` variant reading a read:packages PAT) before building further.
1. Release (release-please) -> `scalerunnergh-linux-amd64` vX.Y.0.
2. Emon at a terminal: `sudo install -m 750 -o root -g srg scalerunnergh-linux-amd64 /srv/srg/bin/scalerunnergh`.
3. Back up `/srv/srg/config.json` into `work/<date>-1064/`; in the `srg-rl9-nexis` scale set set
   `"image": "ghcr.io/nexisltd/titli-ci-runner:main"`, `"pull": "interval:30m"`, `"registry_auth": "github-app"`
   (its auth must be the nexisltd App installation 181347286).
4. `sudo systemctl restart scalerunnergh` (NOPASSWD rule) while the scale set is idle; check the log for "pulled a new
   image" and one CI run on that scale set.

## Review history
- **v1 goldfish round 1: 0 P0 + 2 P1**, folded into v2: the authed check-then-pull runs before every create (any
  policy) under the shared image lock and create gets `--pull never`; the ghcr-accepts-an-installation-token proof is
  rollout step 0 with a fallback. P2 folded: packages:read-only token, keep the docker context, error labels, the
  shared-image note, the image -> credential map.
- **v2 goldfish round 2: 0 P0 + 1 P1**, folded into v3: no `currentContext` copy (a temp config has no context
  store); the daemon endpoint is resolved once and passed with `-H`. P2 folded: JSON body for permissions, `--pull`
  collision refused, no double pull, backgroundPullTimeout, same image + two installations refused, temp dir under
  the state dir, docker >= 20.10 check.
- **v3 goldfish round 3: No P0/P1 -> plan done.** P2 notes for the build: the credential reaches `Start` through
  `scaler.Options.RegistryCred` -> `RunnerSpec.RegistryCred` (scaler.go builds the spec); `--pull never` and the
  pre-create pull hang on `RegistryCred != nil` (so a scale set sharing the image never pulls anonymously); the pull
  token refreshes when under `backgroundPullTimeout` remains; a TLS docker endpoint is refused (a temp `--config` has no
  certs); the temp dir is `os.MkdirTemp("")` with 0700 (no named state dir exists); the `--pull=...` form is refused too;
  the PAT fallback gets its own short design if step 0 fails; the pull holds the pool slot as today.
