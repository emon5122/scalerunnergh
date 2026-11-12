#!/usr/bin/env bash
# The whole CI suite. CI runs exactly this; run it locally before every push.
#
#   ./scripts/ci.sh            # everything (needs Docker for the integration lane)
#   SKIP_DOCKER=1 ./scripts/ci.sh
#   DOCKER_BUILD_ARGS="--network host" ./scripts/ci.sh
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n== %s\n' "$*"; }

# Extra docker build flags for this machine, e.g. DOCKER_BUILD_ARGS="--network host".
read -r -a build_args <<< "${DOCKER_BUILD_ARGS:-}"

step "gofmt"
unformatted="$(gofmt -l cmd internal tests)"
if [ -n "${unformatted}" ]; then
    echo "not gofmt-ed:"; echo "${unformatted}"; exit 1
fi

step "go mod tidy is clean"
cp go.mod go.mod.ci-bak; cp go.sum go.sum.ci-bak
go mod tidy
tidy_ok=0
if diff -q go.mod go.mod.ci-bak > /dev/null && diff -q go.sum go.sum.ci-bak > /dev/null; then tidy_ok=1; fi
mv go.mod.ci-bak go.mod; mv go.sum.ci-bak go.sum
[ "${tidy_ok}" = 1 ] || { echo "go.mod/go.sum are not tidy"; exit 1; }

step "build (linux, linux/arm64, windows)"
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...

step "vet (linux, windows, integration tag)"
go vet ./...
GOOS=windows go vet ./...
go vet -tags integration ./...

step "unit tests with the race detector"
go test ./... -race -count=1

step "example configs"
for f in examples/*.json; do
    case "$f" in
        *windows*) ;; # paths are Windows-only; parsed by the unit test
        *) go run ./cmd/scalerunnergh --config "$f" --check ;;
    esac
done

step "deploy files"
for f in deploy/github-app/*.json; do jq empty "$f"; done
bash -n deploy/github-app/convert.sh

if [ "${SKIP_DOCKER:-}" = "1" ]; then
    echo; echo "SKIP_DOCKER=1: integration and image lanes skipped"; exit 0
fi

step "integration: docker backend against a real daemon"
(cd tests/integration && go test -tags integration . -count=1)

step "orchestrator image builds and runs"
docker build "${build_args[@]}" -q --build-arg VERSION=ci -t scalerunnergh:ci .
docker run --rm scalerunnergh:ci --version
docker rmi scalerunnergh:ci > /dev/null

step "runner image (base) builds and its entrypoint refuses to run without a JIT config"
docker build "${build_args[@]}" -q --target base -t scalerunnergh-runner:ci docker/runner
set +e
docker run --rm scalerunnergh-runner:ci > /dev/null 2>&1
code=$?
set -e
[ "${code}" = 64 ] || { echo "expected exit 64 without a JIT config, got ${code}"; exit 1; }
docker rmi scalerunnergh-runner:ci > /dev/null

echo; echo "CI suite passed"
