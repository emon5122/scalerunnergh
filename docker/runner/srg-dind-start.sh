#!/bin/bash
# Start a private dockerd inside this (privileged) job container and wait
# until it answers. Its data lives in an anonymous volume that is removed
# with the container, so nothing survives the job.
set -euo pipefail

# cgroup v2: move our processes into a child group so dockerd can delegate
# controllers to the containers it starts (same approach as docker:dind).
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    mkdir -p /sys/fs/cgroup/init
    xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
    sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers \
        > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
fi

args=(--host=unix:///var/run/docker.sock --group=docker)
if [ -n "${SRG_REGISTRY_MIRROR:-}" ]; then
    args+=(--registry-mirror="${SRG_REGISTRY_MIRROR}")
fi

dockerd "${args[@]}" > /var/log/dockerd.log 2>&1 &

for _ in $(seq 1 120); do
    if docker info > /dev/null 2>&1; then
        exit 0
    fi
    sleep 0.5
done

echo "srg: dockerd did not start; last log lines:" >&2
tail -n 30 /var/log/dockerd.log >&2 || true
exit 65
