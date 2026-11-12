# Runner images for the docker backend: one ephemeral GitHub Actions runner
# per container. Build a flavor with --target (base, node, go, python, media, full).
#
# Every tool version is pinned here; bump them through Renovate, never on a host.
FROM ubuntu:24.04 AS base

ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
# renovate: datasource=github-releases depName=actions/runner
ARG RUNNER_VERSION=2.338.2
ARG RUNNER_SHA256_AMD64=baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40
ARG RUNNER_SHA256_ARM64=670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git git-lfs gnupg jq unzip zip xz-utils zstd tar \
        openssh-client build-essential pkg-config iptables \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update && apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/*

# No sudo on purpose: the toolchain comes from the image, not from the job.
RUN useradd -m -s /bin/bash -u 1001 runner && usermod -aG docker runner

WORKDIR /home/runner
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
        amd64) arch=x64; sha="${RUNNER_SHA256_AMD64}" ;; \
        arm64) arch=arm64; sha="${RUNNER_SHA256_ARM64}" ;; \
        *) echo "unsupported arch ${TARGETARCH}"; exit 1 ;; \
    esac; \
    curl -fsSL -o runner.tar.gz \
        "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${arch}-${RUNNER_VERSION}.tar.gz"; \
    echo "${sha}  runner.tar.gz" | sha256sum -c -; \
    tar xzf runner.tar.gz; rm runner.tar.gz; \
    ./bin/installdependencies.sh; \
    rm -rf /var/lib/apt/lists/*; \
    chown -R runner:runner /home/runner

COPY srg-dind-start.sh /usr/local/bin/srg-dind-start
COPY runner-entrypoint.sh /usr/local/bin/srg-entrypoint
RUN chmod 0755 /usr/local/bin/srg-dind-start /usr/local/bin/srg-entrypoint

ENV RUNNER_MANUALLY_TRAP_SIG=1 \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1
# The entrypoint starts as root (to read the JIT config and start dockerd)
# and drops to the runner user before the runner starts.
ENTRYPOINT ["/usr/local/bin/srg-entrypoint"]

# ---- node: Node.js LTS + pnpm ---------------------------------------------
FROM base AS node
# renovate: datasource=node-version depName=node
ARG NODE_VERSION=22.21.1
# renovate: datasource=npm depName=pnpm
ARG PNPM_VERSION=10.20.0
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in amd64) a=x64 ;; arm64) a=arm64 ;; esac; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${a}.tar.xz" \
        | tar -xJ -C /usr/local --strip-components=1 --no-same-owner; \
    npm install -g "pnpm@${PNPM_VERSION}"; \
    node --version; pnpm --version

# ---- go --------------------------------------------------------------------
FROM base AS go
# renovate: datasource=golang-version depName=go
ARG GO_VERSION=1.27.3
RUN set -eux; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH:-amd64}.tar.gz" | tar -xz -C /usr/local; \
    /usr/local/go/bin/go version
ENV PATH=/usr/local/go/bin:/home/runner/go/bin:$PATH

# ---- python: system Python 3.12 + uv ---------------------------------------
FROM base AS python
# renovate: datasource=github-releases depName=astral-sh/uv
ARG UV_VERSION=0.9.7
RUN set -eux; \
    apt-get update; apt-get install -y --no-install-recommends python3 python3-venv python3-pip python3-dev; \
    rm -rf /var/lib/apt/lists/*; \
    case "${TARGETARCH:-amd64}" in amd64) a=x86_64 ;; arm64) a=aarch64 ;; esac; \
    curl -fsSL "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${a}-unknown-linux-gnu.tar.gz" \
        | tar -xz -C /usr/local/bin --strip-components=1; \
    uv --version

# ---- media: node + ffmpeg (home-pbx image builds) ----------------------------
FROM node AS media
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg sox \
    && rm -rf /var/lib/apt/lists/*

# ---- full: node + go + python + ffmpeg -------------------------------------
FROM media AS full
COPY --from=go /usr/local/go /usr/local/go
COPY --from=python /usr/local/bin/uv /usr/local/bin/uvx /usr/local/bin/
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-venv python3-pip python3-dev \
    && rm -rf /var/lib/apt/lists/*
ENV PATH=/usr/local/go/bin:/home/runner/go/bin:$PATH
