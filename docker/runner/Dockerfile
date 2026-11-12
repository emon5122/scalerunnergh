# Runner images for the docker backend: one ephemeral GitHub Actions runner
# per container. Build a flavor with --target (base, node, go, python, media, full, rust).
#
# The runner and the language toolchains are pinned by version and sha256 here;
# bump them through Renovate, never on a host. Ubuntu and Docker packages come
# from apt at build time (the image tag pins the result).
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

# No root for jobs: the toolchain comes from the image, not from the job (the rust
# flavor adds a sudo shim that grants nothing, see srg-sudo).
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
ARG NODE_SHA256_AMD64=680d3f30b24a7ff24b98db5e96f294c0070f8f9078df658da1bce1b9c9873c88
ARG NODE_SHA256_ARM64=e660365729b434af422bcd2e8e14228637ecf24a1de2cd7c916ad48f2a0521e1
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
        amd64) a=x64; sha="${NODE_SHA256_AMD64}" ;; \
        arm64) a=arm64; sha="${NODE_SHA256_ARM64}" ;; \
    esac; \
    curl -fsSL -o /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${a}.tar.xz"; \
    echo "${sha}  /tmp/node.tar.xz" | sha256sum -c -; \
    tar -xJ -f /tmp/node.tar.xz -C /usr/local --strip-components=1 --no-same-owner; \
    rm /tmp/node.tar.xz; \
    npm install -g "pnpm@${PNPM_VERSION}"; \
    node --version; pnpm --version

# ---- go --------------------------------------------------------------------
FROM base AS go
# renovate: datasource=golang-version depName=go
ARG GO_VERSION=1.27.3
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
        amd64) sha="${GO_SHA256_AMD64}" ;; \
        arm64) sha="${GO_SHA256_ARM64}" ;; \
    esac; \
    curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH:-amd64}.tar.gz"; \
    echo "${sha}  /tmp/go.tar.gz" | sha256sum -c -; \
    tar -xz -f /tmp/go.tar.gz -C /usr/local; \
    rm /tmp/go.tar.gz; \
    /usr/local/go/bin/go version
ENV PATH=/usr/local/go/bin:/home/runner/go/bin:$PATH

# ---- python: system Python 3.12 + uv ---------------------------------------
FROM base AS python
# renovate: datasource=github-releases depName=astral-sh/uv
ARG UV_VERSION=0.9.7
ARG UV_SHA256_AMD64=b26fcc8dfa1c39b5a5613445af3be3eefda45d9a39359bee271eafe34913583e
ARG UV_SHA256_ARM64=8b3d31a154673c6d357727d2083a33525b515589d153fa5b5455e1db9e9e6363
RUN set -eux; \
    apt-get update; apt-get install -y --no-install-recommends python3 python3-venv python3-pip python3-dev; \
    rm -rf /var/lib/apt/lists/*; \
    case "${TARGETARCH:-amd64}" in \
        amd64) a=x86_64; sha="${UV_SHA256_AMD64}" ;; \
        arm64) a=aarch64; sha="${UV_SHA256_ARM64}" ;; \
    esac; \
    curl -fsSL -o /tmp/uv.tar.gz "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${a}-unknown-linux-gnu.tar.gz"; \
    echo "${sha}  /tmp/uv.tar.gz" | sha256sum -c -; \
    tar -xz -f /tmp/uv.tar.gz -C /usr/local/bin --strip-components=1; \
    rm /tmp/uv.tar.gz; \
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

# ---- rust: full + rustup + clang/lld 21, cmake, glib headers (wa-gateway), ----
# ---- and the Tauri v2 Linux build libraries (radiolens viewer/router) -------
# Jobs get no sudo, so everything a Rust job used to apt-get at run time is here.
# The toolchain itself follows each repo's rust-toolchain.toml (rustup installs it
# on first use inside the job).
FROM full AS rust
ARG LLVM_VERSION=21
# apt.llvm.org signing key (Sylvestre Ledru): only this fingerprint is trusted
ARG LLVM_SIGNER_FPR=6084F3CF814B57C1CF12EFD515CF4D18AF4F7421
# renovate: datasource=github-tags depName=rust-lang/rustup
ARG RUSTUP_VERSION=1.28.2
ARG RUSTUP_SHA256_AMD64=20a06e644b0d9bd2fbdbfd52d42540bdde820ea7df86e92e533c073da0cdd43c
ARG RUSTUP_SHA256_ARM64=e3853c5a252fca15252d07cb23a1bdd9377a8c6f3efa01531109281ae47f841c
RUN set -eux; \
    curl -fsSL -o /tmp/llvm.asc https://apt.llvm.org/llvm-snapshot.gpg.key; \
    export GNUPGHOME="$(mktemp -d)"; \
    gpg --batch --import /tmp/llvm.asc; \
    gpg --batch --export "${LLVM_SIGNER_FPR}" > /etc/apt/keyrings/llvm.gpg; \
    [ -s /etc/apt/keyrings/llvm.gpg ]; \
    rm -rf "${GNUPGHOME}" /tmp/llvm.asc; unset GNUPGHOME; \
    echo "deb [signed-by=/etc/apt/keyrings/llvm.gpg] http://apt.llvm.org/noble/ llvm-toolchain-noble-${LLVM_VERSION} main" \
        > /etc/apt/sources.list.d/llvm.list; \
    apt-get update; apt-get install -y --no-install-recommends \
        clang-${LLVM_VERSION} lld-${LLVM_VERSION} cmake libglib2.0-dev sqlite3 shellcheck lsb-release wget \
        libudev-dev libwebkit2gtk-4.1-dev libappindicator3-dev librsvg2-dev patchelf libsoup-3.0-dev \
        libjavascriptcoregtk-4.1-dev libssl-dev file \
        imagemagick webp rpm ruby ruby-dev maven libpq-dev; \
    rm -rf /var/lib/apt/lists/*; \
    clang-${LLVM_VERSION} --version
# Release-upload and packaging tools CI jobs used to install with sudo.
# renovate: datasource=github-tags depName=aws/aws-cli
ARG AWSCLI_VERSION=2.40.1
ARG AWSCLI_SHA256_AMD64=fddb097a8ea197dc9beed9e4426ef99aded612a52df64581e079b6e8817e1d24
# renovate: datasource=github-releases depName=AppImage/appimagetool
ARG APPIMAGETOOL_VERSION=1.9.1
ARG APPIMAGETOOL_SHA256_AMD64=ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0
# mc has no binary downloads any more: built from the tagged module (verified by the Go checksum database).
# renovate: datasource=github-tags depName=minio/mc
ARG MC_VERSION=RELEASE.2025-08-13T08-35-41Z
# renovate: datasource=rubygems depName=fpm
ARG FPM_VERSION=1.18.0
RUN set -eux; \
    curl -fsSL -o /tmp/awscli.zip "https://awscli.amazonaws.com/awscli-exe-linux-x86_64-${AWSCLI_VERSION}.zip"; \
    echo "${AWSCLI_SHA256_AMD64}  /tmp/awscli.zip" | sha256sum -c -; \
    unzip -q /tmp/awscli.zip -d /tmp/awscli; /tmp/awscli/aws/install; \
    curl -fsSL -o /usr/local/bin/appimagetool "https://github.com/AppImage/appimagetool/releases/download/${APPIMAGETOOL_VERSION}/appimagetool-x86_64.AppImage"; \
    echo "${APPIMAGETOOL_SHA256_AMD64}  /usr/local/bin/appimagetool" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/appimagetool; \
    GOPATH=/tmp/gopath GOCACHE=/tmp/gocache GOBIN=/usr/local/bin GOFLAGS=-trimpath GOTOOLCHAIN=local \
        go install "github.com/minio/mc@${MC_VERSION}"; \
    gem install --no-document fpm -v "${FPM_VERSION}"; \
    chmod -R u+w /tmp/gopath; rm -rf /tmp/*; \
    aws --version; mc --version; fpm --version; mvn -v | head -1
# No root for jobs: sudo is a shim that only checks apt packages are present
# and otherwise runs the command unprivileged (srg-sudo). /usr/local/bin is the
# job user's, for workflows that drop a downloaded tool there.
COPY srg-sudo /usr/local/bin/sudo
RUN chmod 0755 /usr/local/bin/sudo && chown runner:runner /usr/local/bin
# AppImages cannot mount FUSE inside a job container.
ENV APPIMAGE_EXTRACT_AND_RUN=1
USER runner
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
        amd64) a=x86_64; sha="${RUSTUP_SHA256_AMD64}" ;; \
        arm64) a=aarch64; sha="${RUSTUP_SHA256_ARM64}" ;; \
    esac; \
    curl -fsSL -o /tmp/rustup-init "https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/${a}-unknown-linux-gnu/rustup-init"; \
    echo "${sha}  /tmp/rustup-init" | sha256sum -c -; \
    chmod +x /tmp/rustup-init; \
    /tmp/rustup-init -y --profile minimal --default-toolchain none --no-modify-path; \
    rm /tmp/rustup-init; \
    /home/runner/.cargo/bin/rustup --version
USER root
ENV PATH=/home/runner/.cargo/bin:$PATH
