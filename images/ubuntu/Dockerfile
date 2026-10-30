# syntax=docker/dockerfile:1

# latem-ubuntu — a base image for latem's docker executor (RUNNER_SPEC §13.1)
# that stands in for GitHub's ubuntu-latest closely enough to run workflows
# written against it. README.md says what is in it, what is not, and why.
#
# Build with ./build.sh, check with ./check.sh. Every download outside apt is
# pinned by an ARG below and verified against SHA256SUMS; ./pin.sh rewrites
# that file after a version bump. The awk in each check fails when the file has
# no line for the download: some sha256sum builds accept empty input as a pass. Each ARG sits just above the one RUN that
# uses it, so bumping one rebuilds that layer and those after it, not all.

FROM ubuntu:24.04

ARG TARGETARCH

# pipefail matters here as it does in the repository's scripts (amendment A37):
# a verification pipeline that can fail silently is not a verification.
SHELL ["/bin/bash", "-o", "errexit", "-o", "nounset", "-o", "pipefail", "-c"]

# DEBIAN_FRONTEND stays set at run time on purpose. Jobs call apt (through
# `playwright install --with-deps`, through sudo) and a tzdata or keyboard
# prompt in a job with no terminal hangs until the step times out.
#
# The toolcache is where actions/setup-node and actions/setup-python look
# before downloading anything, laid out the way they expect, so that once
# latem runs JavaScript actions the versions baked in here cost nothing.
#
# /usr/local/node is a link to the default Node in the toolcache. PATH names
# the link rather than the toolcache because the toolcache path carries the
# architecture (x64 or arm64) and ENV cannot compute it.
ENV LANG=C.UTF-8 \
    DEBIAN_FRONTEND=noninteractive \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
    AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache \
    PATH=/usr/local/node/bin:$PATH

# lsb-release is not decoration: setup-python identifies the OS by running it.
# python3 is the system 3.12, as on ubuntu-latest, and it also pulls in the
# shared libraries the toolcache Pythons below link against.
RUN apt-get update; \
    apt-get install -y --no-install-recommends \
      bash ca-certificates curl wget gnupg lsb-release sudo tzdata procps \
      git git-lfs openssh-client \
      jq zip unzip tar xz-utils zstd bzip2 gzip file rsync \
      build-essential pkg-config \
      python3 python3-pip python3-venv python-is-python3; \
    rm -rf /var/lib/apt/lists/*

# The Docker CLI and buildx are for `wrangler containers build` and the like,
# against a daemon socket the job is given. There is no daemon in the image.
RUN . /etc/os-release; \
    dpkg_arch=$(dpkg --print-architecture); \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL --retry 3 -o /etc/apt/keyrings/docker.asc \
      https://download.docker.com/linux/ubuntu/gpg; \
    curl -fsSL --retry 3 -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
      https://cli.github.com/packages/githubcli-archive-keyring.gpg; \
    chmod a+r /etc/apt/keyrings/*; \
    echo "deb [arch=$dpkg_arch signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $VERSION_CODENAME stable" \
      > /etc/apt/sources.list.d/docker.list; \
    echo "deb [arch=$dpkg_arch signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      docker-ce-cli docker-buildx-plugin docker-compose-plugin gh; \
    rm -rf /var/lib/apt/lists/*

# The §13.1 contract: user `runner`, uid and gid 1000, with a home it owns.
# ubuntu:24.04 already has a uid 1000, named `ubuntu`, which `latem doctor`
# rejects by name, so it goes first.
#
# Passwordless sudo because ubuntu-latest has it and workflows assume it:
# `playwright install --with-deps` runs apt through sudo when it is not root.
# secure_path gains the default Node so `sudo npx …` resolves as it does on
# ubuntu-latest, where Node is in /usr/local/bin.
#
# safe.directory covers the workspace on Docker Desktop, where the bind mount's
# ownership is virtualised and need not match the step's uid.
RUN if getent passwd ubuntu >/dev/null; then userdel --remove ubuntu; fi; \
    groupadd --gid 1000 runner; \
    useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash runner; \
    printf '%s\n' \
      'runner ALL=(ALL) NOPASSWD:ALL' \
      'Defaults env_keep += "DEBIAN_FRONTEND"' \
      'Defaults secure_path="/usr/local/node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"' \
      > /etc/sudoers.d/runner; \
    chmod 0440 /etc/sudoers.d/runner; \
    visudo --check --quiet --file=/etc/sudoers.d/runner; \
    install -d -o 1000 -g 1000 "$RUNNER_TOOL_CACHE"; \
    git config --system --add safe.directory /workspace; \
    git lfs install --system --skip-repo

# Node from nodejs.org into the toolcache. The toolcache is owned by uid 1000,
# as it is on ubuntu-latest, which is what makes `npm install -g npm@11` work
# without sudo: npm's global prefix is the directory node runs from.
ARG NODE22_VERSION=22.23.4
ARG NODE24_VERSION=24.22.2
ARG NODE_DEFAULT=24
RUN --mount=type=bind,source=SHA256SUMS,target=/tmp/SHA256SUMS \
    case $TARGETARCH in \
      amd64) arch=x64 ;; \
      arm64) arch=arm64 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    for v in "$NODE22_VERSION" "$NODE24_VERSION"; do \
      f=node-v$v-linux-$arch.tar.xz; \
      curl -fsSL --retry 3 -o "/tmp/$f" "https://nodejs.org/dist/v$v/$f"; \
      (cd /tmp && awk -v f="$f" '$2 == f { print; found = 1 } END { exit !found }' SHA256SUMS | sha256sum --check --strict -); \
      dest=$RUNNER_TOOL_CACHE/node/$v/$arch; \
      mkdir -p "$dest"; \
      tar -xJf "/tmp/$f" -C "$dest" --strip-components=1; \
      touch "$dest.complete"; \
      rm "/tmp/$f"; \
    done; \
    case $NODE_DEFAULT in \
      22) default=$NODE22_VERSION ;; \
      24) default=$NODE24_VERSION ;; \
      *) echo "NODE_DEFAULT must be 22 or 24, not $NODE_DEFAULT" >&2; exit 1 ;; \
    esac; \
    ln -s "$RUNNER_TOOL_CACHE/node/$default/$arch" /usr/local/node; \
    chown -R 1000:1000 "$RUNNER_TOOL_CACHE/node"; \
    node --version; \
    npm --version

# Chromium's system packages, from Playwright's own list for this OS, so that a
# job's `playwright install --with-deps chromium` finds them present and only
# downloads the browser. The list includes xvfb and the fonts Playwright wants.
# A project pinning a different Playwright gets the same list or one that
# differs by a package or two, which --with-deps then installs.
ARG PLAYWRIGHT_VERSION=1.64.1
RUN npx --yes "playwright@$PLAYWRIGHT_VERSION" install-deps chromium; \
    rm -rf /var/lib/apt/lists/* /root/.npm /root/.cache

# CPython from actions/python-versions — the builds setup-python installs on
# ubuntu-latest — through their own installer, which puts them in the
# toolcache. They link against an rpath under /opt/hostedtoolcache, so they
# work only there. python3 stays the system 3.12; these are python3.11 and
# python3.13.
ARG PYTHON311_RELEASE=3.11.18-40260280589
ARG PYTHON313_RELEASE=3.13.18-40336726450
RUN --mount=type=bind,source=SHA256SUMS,target=/tmp/SHA256SUMS \
    case $TARGETARCH in \
      amd64) arch=x64 ;; \
      arm64) arch=arm64 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    for rel in "$PYTHON311_RELEASE" "$PYTHON313_RELEASE"; do \
      v=${rel%-*}; \
      mm=${v%.*}; \
      f=python-$v-linux-24.04-$arch.tar.gz; \
      curl -fsSL --retry 3 -o "/tmp/$f" \
        "https://github.com/actions/python-versions/releases/download/$rel/$f"; \
      (cd /tmp && awk -v f="$f" '$2 == f { print; found = 1 } END { exit !found }' SHA256SUMS | sha256sum --check --strict -); \
      mkdir /tmp/python; \
      tar -xzf "/tmp/$f" -C /tmp/python; \
      (cd /tmp/python && bash ./setup.sh); \
      rm -rf /tmp/python "/tmp/$f"; \
      bin=$RUNNER_TOOL_CACHE/Python/$v/$arch/bin; \
      ln -s "$bin/python$mm" "/usr/local/bin/python$mm"; \
      ln -s "$bin/pip$mm" "/usr/local/bin/pip$mm"; \
      "python$mm" -c 'import bz2, ctypes, lzma, readline, sqlite3, ssl, uuid, venv, zlib'; \
    done; \
    chown -R 1000:1000 "$RUNNER_TOOL_CACHE/Python"; \
    rm -rf /root/.cache

# AWS CLI v2. AWS publishes a PGP signature and no checksum, so the checksum in
# SHA256SUMS is one pin.sh computed from the pinned version's zip.
ARG AWSCLI_VERSION=2.39.1
RUN --mount=type=bind,source=SHA256SUMS,target=/tmp/SHA256SUMS \
    case $TARGETARCH in \
      amd64) arch=x86_64 ;; \
      arm64) arch=aarch64 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    f=awscli-exe-linux-$arch-$AWSCLI_VERSION.zip; \
    curl -fsSL --retry 3 -o "/tmp/$f" "https://awscli.amazonaws.com/$f"; \
    (cd /tmp && awk -v f="$f" '$2 == f { print; found = 1 } END { exit !found }' SHA256SUMS | sha256sum --check --strict -); \
    unzip -q "/tmp/$f" -d /tmp/awscli; \
    /tmp/awscli/aws/install --install-dir /usr/local/aws-cli --bin-dir /usr/local/bin; \
    rm -rf /tmp/awscli "/tmp/$f"; \
    aws --version

# No USER, deliberately: the image's default user stays root.
#
# latem chooses the step user itself. On Linux it runs every step as the
# agent's own uid:gid (§13.1) — runner, on a host whose user is uid 1000. Before
# that it chowns the run's freshly created volumes in a probe container that
# runs as the image's default user and has to be root to do it; `USER runner`
# here would fail every Linux run in Prepare. On Docker Desktop latem passes no
# user, so there steps run as root. README.md has the consequences.
#
# No ENTRYPOINT either. latem replaces the run container's, but `latem doctor`
# probes the image with a command alone, which an entrypoint would swallow.

LABEL org.opencontainers.image.title="latem-ubuntu" \
      org.opencontainers.image.description="Ubuntu 24.04 base image for latem's docker executor, close to GitHub's ubuntu-latest" \
      org.opencontainers.image.source="https://github.com/Abyssinia-Labs/latem"
