# Contributing

Bug reports, fixes and small features are welcome. For a larger change, open an issue
first so we can agree on the approach. Security problems go through
[SECURITY.md](SECURITY.md), never a public issue.

## Setup

- Go: the version in [`go.mod`](go.mod) (the `go` line; CI uses `go-version-file: go.mod`).
- Docker for the integration and image lanes of the CI script (optional).
- `jq` for the deploy file checks.

## Before you open a pull request

Run the same suite CI runs:

```bash
./scripts/ci.sh                 # everything (needs Docker)
SKIP_DOCKER=1 ./scripts/ci.sh   # without the Docker lanes
```

[`scripts/ci.sh`](scripts/ci.sh) runs, in order:

- `gofmt -l cmd internal tests` (must print nothing)
- `go mod tidy` (must leave `go.mod` and `go.sum` unchanged)
- `go build ./...` for linux/amd64, linux/arm64 and windows/amd64
- `go vet ./...`, `GOOS=windows go vet ./...` and `go vet -tags integration ./...`
- `go test ./... -race -count=1`
- `scalerunnergh --config <file> --check` for every Linux example in `examples/`
- `jq` and `bash -n` over `deploy/github-app/`
- with Docker: the integration tests in `tests/integration`, the orchestrator image build,
  and the base runner image build

The quick loop while you work:

```bash
go test -race ./...
GOOS=windows go vet ./...       # Windows build check
```

Windows-specific code (the service mode and the native backend's process handling) also
runs on a Windows CI job; say in the pull request if you tested it on a real Windows host.

## Commits and releases

- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages and
  pull request titles (`feat: ...`, `fix(native): ...`, `docs: ...`). They decide the next
  version.
- Update the README and the examples when you change behaviour or config.
- Releases are made by release-please from `main`: it opens the release pull request,
  and merging it tags the version.
- Binaries and container images are built only by CI on a release tag. Do not attach or
  push images or binaries by hand.

By contributing you agree that your contribution is licensed under the [MIT License](LICENSE).
