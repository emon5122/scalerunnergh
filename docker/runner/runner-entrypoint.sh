#!/bin/bash
set -euo pipefail

# Entrypoint for ephemeral GitHub Actions runner container.
# This script configures the runner using environment variables,
# runs the job, and then exits (ephemeral mode).

RUNNER_NAME="${RUNNER_NAME:-$(hostname)}"
RUNNER_WORK_FOLDER="${RUNNER_WORK_FOLDER:-/_work}"
RUNNER_LABELS="${RUNNER_LABELS:-self-hosted}"
RUNNER_TOKEN="${RUNNER_TOKEN:?RUNNER_TOKEN is required}"

echo "==============================="
echo " ScaleRunnerGH Ephemeral Runner"
echo "==============================="
echo "Runner Name: ${RUNNER_NAME}"
echo "Labels: ${RUNNER_LABELS}"

# Determine registration URL
if [ -n "${RUNNER_REPOSITORY:-}" ]; then
    REGISTRATION_URL="${RUNNER_REPOSITORY}"
    echo "Repository: ${REGISTRATION_URL}"
elif [ -n "${RUNNER_ORGANIZATION:-}" ]; then
    REGISTRATION_URL="https://github.com/${RUNNER_ORGANIZATION}"
    echo "Organization: ${RUNNER_ORGANIZATION}"
else
    echo "ERROR: Either RUNNER_REPOSITORY or RUNNER_ORGANIZATION must be set"
    exit 1
fi

# Configure the runner
./config.sh \
    --url "${REGISTRATION_URL}" \
    --token "${RUNNER_TOKEN}" \
    --name "${RUNNER_NAME}" \
    --labels "${RUNNER_LABELS}" \
    --work "${RUNNER_WORK_FOLDER}" \
    --ephemeral \
    --unattended \
    --replace \
    --disableupdate

echo "Runner configured. Starting..."

# Run the runner (will exit after one job due to --ephemeral)
./run.sh

echo "Runner finished. Container will self-destruct."
