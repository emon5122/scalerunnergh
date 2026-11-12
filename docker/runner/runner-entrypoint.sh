#!/bin/bash
# Entrypoint of an ephemeral ScaleRunnerGH runner container.
#
# The orchestrator copies the single-use JIT runner config to /srg/jitconfig
# before the container starts. This script reads and deletes it, optionally
# starts a private dockerd (SRG_DIND=1), drops to the runner user and runs
# exactly one job. The JIT config is never written to a command line or a log.
set -euo pipefail

JIT_FILE=/srg/jitconfig

if [ ! -s "${JIT_FILE}" ]; then
    echo "srg: no JIT config at ${JIT_FILE}; this image is started by scalerunnergh" >&2
    exit 64
fi
ACTIONS_RUNNER_INPUT_JITCONFIG="$(cat "${JIT_FILE}")"
rm -f "${JIT_FILE}"
export ACTIONS_RUNNER_INPUT_JITCONFIG

if [ "${SRG_DIND:-}" = "1" ]; then
    /usr/local/bin/srg-dind-start
fi
unset SRG_DIND SRG_REGISTRY_MIRROR

cd /home/runner
export HOME=/home/runner USER=runner LOGNAME=runner
exec setpriv --reuid=runner --regid=runner --init-groups ./run.sh
