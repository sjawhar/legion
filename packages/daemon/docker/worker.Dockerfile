# syntax=docker/dockerfile:1.7
# Legion worker image: every Legion agent process under `runtime: kubernetes` runs from this image.
# Build context: the repo root. Built by .github/workflows/worker-image.yaml on the GitHub-hosted runner
# (called from release.yaml after `legion`, on every head of a pull request against main that touches a file
# it builds from, or dispatched post-merge). Never build it on a workstation — no `docker build`,
# `docker buildx`, or `docker compose build` (Sami, 2026-09-12); the CI runner is not a workstation.
#
# Contents: pinned Bun; the Go `legion` (packages/daemon) compiled from this checkout at go.work's
# Go version, static, at /opt/legion/bin/legion (one binary: worker-shim, workspace-init, credential,
# gh, handoff, push, probe-image and the daemon's own commands), the first PATH entry and the
# ENTRYPOINT name, with the `agent-secrets` client beside it; the pinned OMP fork build, resolved with
# mise's github backend exactly as a tmux host resolves an `omp_invocation` naming it;
# @sjawhar/pi-legion-envoy packed from this checkout's packages/pi-envoy and @bopstack/pi-codegraph
# (from npm, pinned) linked into the isolated OMP profile `legion`, backed by the CodeGraph CLI
# (@colbymchenry/codegraph, pinned) at /opt/codegraph/bin; jj; git at /usr/bin/git (>= 2.42, from the
# debian:trixie-slim runtime base — jj's git backend requires it); gh; and a generic toolchain for the
# repositories the workers work, specific to none of them: uv and uvx, Node LTS with npm and corepack's
# pnpm and yarn, and the AWS CLI v2, each on PATH at /usr/local/bin. The last three RUNs gate the
# publish, as the runtime user: the first checks every binary runs on the base, proves jj accepts the
# image's git with a network-free `jj git clone` of a scratch repository, and links the plugins into
# the profile, which fetches Oh My Pi's natives; the second runs every toolchain command; the last
# runs `legion version` and `legion probe-image`, which runs the three launch probes (the daemon's two
# plus the session-storage probe), holds the plugin to the daemon API contract, and prints the OK line
# the daemon's probe Sandbox reads. A broken image never publishes.
#
# The `legion` profile carries no model route, and neither does Legion: an operator's pod supplies it
# (runtime.kubernetes.pod, docs/kubernetes.md). In a pod, the Go `legion` starts Oh My Pi on Legion's
# pod baseline (packages/daemon/internal/podsafety: the worker shim, and `legion probe-image`, each
# with --pod-safety), which names no model, provider or route.

# Pins not derived from daemon code. The OMP fork pin is deliberately NOT an ARG: it is the one line of
# the repository's .omp-pin, its only home, which the tools stage copies.
ARG BUN_VERSION=1.3.14
ARG MISE_VERSION=v2026.8.12
# Sami's jj fork: what the dogfood daemon runs on the devbox; same 0.45 line as the jj-lib inside OMP.
ARG JJ_TOOL=github:sjawhar/jj@0.45.1-sami.20260910-043938
ARG GH_TOOL=gh@2.98.0
# go.work's `go` line: the Go stage builds in workspace mode, and the golang image's GOTOOLCHAIN=local
# fails the build if go.work moves past this.
ARG GO_VERSION=1.26.1
# The toolchain stage's pins: each is a release version and the SHA-256 of the linux/amd64 archive the
# stage downloads for it, which the build checks before unpacking anything.
ARG UV_VERSION=0.12.21
ARG UV_SHA256=23f02075b652bb1df64178cfae41b5caf160822e720e2663568f3f5d63bc52c0
# Node 24 LTS.
ARG NODE_VERSION=24.21.0
ARG NODE_SHA256=fd8e59d5a511510f6a298afb548f18c7d2b1be404d8b4a27d94fbe49f56cb2d6
ARG AWS_CLI_VERSION=2.37.6
# AWS publishes no SHA-256 for the CLI, only a PGP signature. This pin is the SHA-256 of the zip whose
# `.sig` gpg verified as a good signature from the AWS CLI Team key
# FB5DB77FD5C118B80511ADA8A6310ACC4672475C. A version bump repeats that check before taking its hash.
ARG AWS_CLI_SHA256=cd40c7d1f41b3a4964e77a65377e480d71fe6ebc96bbbb64eb2239d69af6fbb2
# apt packages carry no version pin (hadolint DL3008, ignored at each `apt-get install`): Debian's
# archive serves only a suite's current version of a package, so a pinned version stops resolving at
# the suite's next update. The base image's suite is the pin.
# CodeGraph CLI (research report AGENTC-1305 §7) and the Oh My Pi tool that wraps it, pinned to the
# versions the devbox runs (`@colbymchenry/codegraph --version`, `omp/plugins/package.json`).
ARG CODEGRAPH_VERSION=1.5.0
ARG PI_CODEGRAPH_VERSION=0.1.1

# ------------------------------------------------------------------------------------------------
# plugin: workspace install, the CodeGraph CLI, and the packed plugin.
FROM oven/bun:${BUN_VERSION}-slim AS plugin
WORKDIR /repo
# jq: the same omp.extensions rewrite release.yaml's pi_envoy job runs. python3/make/g++: native
# devDependencies in the workspace lockfile (mirrors packages/envoy/docker/Dockerfile).
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends jq python3 make g++ \
    && rm -rf /var/lib/apt/lists/*
ARG CODEGRAPH_VERSION
# CodeGraph CLI (@colbymchenry/codegraph): `bin.codegraph` in the main npm package is a thin
# `#!/usr/bin/env node` launcher shim that locates and execs the per-platform optionalDependency
# (`@colbymchenry/codegraph-linux-x64`) — but this stage's base image has no system `node` (only
# bun's own fallback shim, which the final runtime stage does not copy over), so the shim itself
# cannot start. The platform package's own `bin/codegraph` is a `#!/bin/sh` wrapper that execs a
# *bundled* Node 24 runtime sitting beside it — fully self-contained, no system node required
# anywhere — so this copies that platform package alone, as `/opt/codegraph` in the runtime stage.
# Installed before any application-source COPY: it depends on nothing this checkout builds, so a
# source change to the plugin this stage packs never invalidates this layer.
RUN mkdir -p /out \
    && bun add -g "@colbymchenry/codegraph@${CODEGRAPH_VERSION}" \
    && cp -a /root/.bun/install/global/node_modules/@colbymchenry/codegraph-linux-x64 /out/codegraph \
    && /out/codegraph/bin/codegraph --version
COPY package.json bun.lock ./
COPY patches patches
COPY packages/contracts/package.json packages/contracts/package.json
COPY packages/envoy-client/package.json packages/envoy-client/package.json
COPY packages/pi-envoy/package.json packages/pi-envoy/package.json
COPY packages/envoy-plugin/package.json packages/envoy-plugin/package.json
COPY packages/claude-envoy/package.json packages/claude-envoy/package.json
COPY packages/proof-editor/package.json packages/proof-editor/package.json
COPY packages/dispatch/package.json packages/dispatch/package.json
COPY packages/envoy/internal/dispatch/pmdoc/gen/package.json packages/envoy/internal/dispatch/pmdoc/gen/package.json
COPY docs/site/package.json docs/site/package.json
RUN bun install --frozen-lockfile
COPY packages/contracts packages/contracts
COPY packages/envoy-client packages/envoy-client
COPY packages/pi-envoy packages/pi-envoy
COPY skills skills

# The plugin ships from this checkout with the steps release.yaml's pi_envoy job runs before
# `bun pm pack` (prepack.sh refuses to pack with the source manifest). The tarball is unpacked into a
# directory: `omp plugin install` links a directory and rejects a tarball path (ENOTDIR).
WORKDIR /repo/packages/pi-envoy
RUN jq '.omp.extensions = ["dist/envoy.js","dist/legion.js"]' package.json > tmp.json \
    && mv tmp.json package.json \
    && rm -f ./*.tgz && bun pm pack \
    && mkdir -p /out/pi-legion-envoy \
    && tar xzf ./*.tgz -C /out/pi-legion-envoy --strip-components=1

# ------------------------------------------------------------------------------------------------
# tools: mise resolves the pinned OMP fork build, jj, and gh — the same backend and pin string the
# daemon hands to `mise where` on a tmux host.
FROM debian:bookworm-slim AS tools
ARG MISE_VERSION
ARG JJ_TOOL
ARG GH_TOOL
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://mise.run -o /tmp/mise-install.sh \
    && MISE_VERSION="${MISE_VERSION}" MISE_INSTALL_PATH=/usr/local/bin/mise sh /tmp/mise-install.sh \
    && rm /tmp/mise-install.sh
COPY .omp-pin /omp-pin
# github_token (optional BuildKit secret): mise's github backend reads MISE_GITHUB_TOKEN; a shared
# builder IP without it can hit GitHub's unauthenticated API limit (403). CI passes secrets.GITHUB_TOKEN;
# a build without the secret still runs, unauthenticated.
RUN --mount=type=secret,id=github_token \
    set -eu; \
    if [ -s /run/secrets/github_token ]; then \
      MISE_GITHUB_TOKEN="$(cat /run/secrets/github_token)"; export MISE_GITHUB_TOKEN; \
    fi; \
    pin="$(cat /omp-pin)"; \
    mise x "$pin" -- omp --version; \
    cp -r "$(mise where "$pin")" /opt/omp; \
    mkdir -p /opt/tools; \
    install -m 0755 "$(mise x "$JJ_TOOL" -- sh -c 'command -v jj')" /opt/tools/jj; \
    install -m 0755 "$(mise x "$GH_TOOL" -- sh -c 'command -v gh')" /opt/tools/gh; \
    /opt/omp/bin/omp --version && /opt/tools/jj --version && /opt/tools/gh --version

# ------------------------------------------------------------------------------------------------
# go: the Go coordinator's `legion`, built as the repository builds it — `go build ./cmd/legion` in
# packages/daemon under go.work, whose other module (packages/envoy) contributes only its go.mod and
# go.sum to dependency selection — static, so it runs on any base. LEGION_REVISION is the commit the
# workflow builds; it is linked in so `legion version` names it, and the build refuses without it.
FROM golang:${GO_VERSION}-alpine AS go
WORKDIR /src
COPY go.work go.work.sum ./
COPY packages/daemon/go.mod packages/daemon/go.sum packages/daemon/
COPY packages/envoy/go.mod packages/envoy/go.sum packages/envoy/
RUN go mod download
COPY packages/daemon packages/daemon
COPY packages/envoy packages/envoy
WORKDIR /src/packages/daemon
# Declared here, after the dependency layers, so a new commit re-runs only the compile.
ARG LEGION_REVISION
RUN test -n "$LEGION_REVISION" \
    && CGO_ENABLED=0 go build -ldflags "-X main.revision=${LEGION_REVISION}" -o /out/legion ./cmd/legion \
    && CGO_ENABLED=0 go -C /src/packages/envoy build -o /out/agent-secrets ./cmd/agent-secrets

# ------------------------------------------------------------------------------------------------
# toolchain: what a worker needs to work a repository that is not Legion's, specific to none: uv (which
# installs each project's own Python from its `.python-version` or `requires-python`, so the image bakes
# no Python), Node with npm and corepack, and the AWS CLI v2. All three archives are checked against
# their pinned SHA-256 before any is unpacked. uv and uvx are single binaries, copied to /usr/local/bin
# as gh and jj are; Node and the AWS CLI keep their own trees under /opt, and /out/bin holds the
# symlinks into them that the runtime stage copies to /usr/local/bin (the AWS installer's `--bin-dir`
# writes its two). The pnpm, pnpx, yarn and yarnpkg links are the ones `corepack enable` would write
# beside node, which the runtime user cannot: each runs the version a project's `packageManager` names,
# or else the default this corepack ships (COREPACK_DEFAULT_TO_LATEST=0, the runtime stage's ENV),
# fetched on first use into the user's own corepack cache.
FROM debian:trixie-slim AS toolchain
ARG UV_VERSION
ARG UV_SHA256
ARG NODE_VERSION
ARG NODE_SHA256
ARG AWS_CLI_VERSION
ARG AWS_CLI_SHA256
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip xz-utils \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    t=/tmp/toolchain; mkdir -p "$t" /out/bin /opt/node; \
    curl -fsSLo "$t/uv.tar.gz" \
      "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-gnu.tar.gz"; \
    curl -fsSLo "$t/node.tar.xz" "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz"; \
    curl -fsSLo "$t/awscli.zip" "https://awscli.amazonaws.com/awscli-exe-linux-x86_64-${AWS_CLI_VERSION}.zip"; \
    printf '%s  %s\n' "$UV_SHA256" "$t/uv.tar.gz" "$NODE_SHA256" "$t/node.tar.xz" \
      "$AWS_CLI_SHA256" "$t/awscli.zip" > "$t/SHA256SUMS"; \
    sha256sum --check --strict "$t/SHA256SUMS"; \
    tar -xzf "$t/uv.tar.gz" -C /out/bin --strip-components=1 --no-same-owner \
      uv-x86_64-unknown-linux-gnu/uv uv-x86_64-unknown-linux-gnu/uvx; \
    tar -xJf "$t/node.tar.xz" -C /opt/node --strip-components=1 --no-same-owner; \
    for tool in node npm npx corepack; do ln -s "/opt/node/bin/$tool" "/out/bin/$tool"; done; \
    for shim in pnpm pnpx yarn yarnpkg; do \
      ln -s "/opt/node/lib/node_modules/corepack/dist/$shim.js" "/out/bin/$shim"; \
    done; \
    unzip -q "$t/awscli.zip" -d "$t"; \
    "$t/aws/install" --install-dir /opt/aws-cli --bin-dir /out/bin; \
    rm -rf "$t"

# ------------------------------------------------------------------------------------------------
# runtime: debian:trixie-slim for its git (2.47; jj 0.45's git backend needs >= 2.42 — bookworm and
# bookworm-backports stop at 2.39.5). The dynamically linked binaries copied in below were built or
# fetched on bookworm; trixie's newer glibc runs them, and the probe RUN below proves it.
FROM debian:trixie-slim
LABEL org.opencontainers.image.source=https://github.com/sjawhar/legion
ARG PI_CODEGRAPH_VERSION
# git: jj's git backend and the workers' own git use. ca-certificates: GitHub, Dispatch, model APIs.
# /opt/legion and /opt/legion/bin are created here, root-owned, before any COPY into them: a COPY
# creates a missing parent with its own --chown, so the plugin's legion:legion copy below would
# otherwise leave the runtime user free to rename bin/ and plant its own `legion`. The final step
# refuses an image where either is not root's.
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 legion \
    && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash legion \
    && install -d -m 0755 -o root -g root /opt/legion /opt/legion/bin
# Pinned Bun: the binary the plugin stage packed with (oven/bun:${BUN_VERSION}-slim).
COPY --from=plugin /usr/local/bin/bun /usr/local/bin/bun
RUN ln -s bun /usr/local/bin/bunx
COPY --from=tools /opt/omp /opt/omp
COPY --from=tools /opt/tools/jj /usr/local/bin/jj
COPY --from=tools /opt/tools/gh /usr/local/bin/gh
COPY --from=plugin --chown=legion:legion /out/pi-legion-envoy /opt/legion/pi-legion-envoy
# CodeGraph CLI (@colbymchenry/codegraph): the self-contained per-platform bundle alone (bundled
# Node runtime + app), never the npm package's own launcher shim — see the plugin stage's comment.
COPY --from=plugin /out/codegraph /opt/codegraph
# OMP_PROFILE=legion: the isolated profile the plugin is linked into (plugins resolve to
# /home/legion/.omp/profiles/legion/plugins/node_modules). LEGION_OMP_PATH: how `legion probe-image`
# — and a daemon pointed at this image — names the OMP executable without mise. HOME is explicit
# because OMP's DirResolver derives the profile root from it. DO_NOT_TRACK=1: CodeGraph's telemetry
# and update-check opt-out (ranked above CODEGRAPH_TELEMETRY, above stored config, above
# default-on) — every worker's own `codegraph` call, and the warm-up the tmux daemon runs outside
# this image, must never phone home for an automatic, non-opt-in tool.
ENV OMP_PROFILE=legion \
    LEGION_OMP_PATH=/opt/omp/bin/omp \
    HOME=/home/legion \
    DO_NOT_TRACK=1 \
    PATH=/opt/legion/bin:/opt/omp/bin:/opt/codegraph/bin:/usr/local/bin:/usr/bin:/bin
# Numeric uid:gid (user `legion`, created above) so Kubernetes `runAsNonRoot` can verify it from the
# image alone.
USER 1000:1000
WORKDIR /home/legion
# 1. Every copied binary runs on this base (they were built or fetched on bookworm; trixie's glibc is
#    newer, so they do — proven here, not assumed), and git is new enough for jj.
# 2. jj's git backend must accept the git on this image: `jj git clone` of a scratch bare repository
#    runs jj's `git fetch --porcelain` subprocess exactly as `legion workspace-init` does in the init
#    container, with no network. A git older than 2.42 fails it (`Git does not recognize required
#    option: porcelain`), which is how bookworm's 2.39.5 shipped in an image that passed every probe:
#    `legion probe-image` never runs jj, and the daemon host's own git is newer.
# 3. Link the packed plugin, and the CodeGraph plugin from npm, into the legion profile
#    (omp-plugins.lock.json records both enabled). This is OMP's first run in the image, so it
#    also downloads OMP's native modules (~345 MB) into /home/legion/.omp/natives/<version>/;
#    this layer ships them, a pod never fetches them, and the probes in the final step never wait on
#    the download.
# Any failure fails the build: a broken image never publishes.
RUN set -eu; \
    bun --version; omp --version; jj --version; gh --version; git --version; codegraph --version; \
    scratch="$(mktemp -d)"; \
    git init --quiet --bare "$scratch/origin.git"; \
    jj git clone "$scratch/origin.git" "$scratch/clone"; \
    rm -rf "$scratch"; \
    omp plugin install /opt/legion/pi-legion-envoy; \
    omp plugin install "@bopstack/pi-codegraph@${PI_CODEGRAPH_VERSION}"; \
    rm -rf /home/legion/.omp/profiles/legion/logs
# The toolchain goes in after the probe layer, so a new toolchain pin never rebuilds that layer and its
# natives, and before `legion`, which changes on every commit. It lands outside HOME, in /opt and
# /usr/local/bin, so no volume a pod mounts under HOME shadows it, and /usr/local/bin is on the image
# PATH and on every pod's (imagePath, packages/daemon/internal/runtime/sandbox/names.go).
COPY --from=toolchain /opt/node /opt/node
COPY --from=toolchain /opt/aws-cli /opt/aws-cli
COPY --from=toolchain /out/bin/ /usr/local/bin/
# Corepack resolves a project with no `packageManager` to the pnpm and yarn it ships as defaults,
# never to npm's newest release, so every place the image runs (a pod, `docker run`, the check below)
# gets one version until the Node pin moves. It is image ENV, not a pod variable, for that reason.
ENV COREPACK_DEFAULT_TO_LATEST=0
# The toolchain step: every toolchain command runs as the runtime user from the image PATH, the
# corepack shims fetching their shipped default pnpm and yarn, with TMPDIR and COREPACK_HOME in a
# scratch directory the step removes, so the layer keeps nothing. No Python is checked: uv installs
# each project's own at run time. Being its own layer above `legion`, it reruns only when the
# toolchain or a layer before it changes, so a commit that only rebuilds `legion` fetches
# nothing from a registry. pnpx is `pnpm dlx`, which takes no --version; its --help, whose first line
# names the pnpm version, is the check.
RUN set -eu; \
    scratch="$(mktemp -d)"; export TMPDIR="$scratch" COREPACK_HOME="$scratch/corepack"; \
    uv --version; uvx --version; node --version; npm --version; npx --version; corepack --version; \
    aws --version; \
    for shim in pnpm yarn yarnpkg; do "$shim" --version; done; \
    pnpx --help > "$scratch/pnpx-help"; sed -n 1p "$scratch/pnpx-help"; \
    rm -rf "$scratch"
# `legion` goes in after the probe layer and the toolchain: its binary differs on every commit (it
# links the commit), so a new commit rebuilds only the layers from here down, never the probe layer and
# its natives.
COPY --from=go /out/legion /opt/legion/bin/legion
# agent-secrets (packages/envoy/cmd/agent-secrets, AGENTC-393): the pod's secrets client — the shim
# runs `keygen` before its hello and `renew` after its enrollment, and the agent's tools call it
# from PATH, which /opt/legion/bin leads in the image and in every worker container (the PATH
# mainEnvironment sets in packages/daemon/internal/runtime/sandbox/manifest.go). The daemon's
# Tools.AgentSecrets names this path.
COPY --from=go /out/agent-secrets /opt/legion/bin/agent-secrets
# The final step: /opt/legion and /opt/legion/bin are root's with mode 0755, so the runtime user can
# neither rename nor replace what they hold.
# Then the `legion` on the image PATH is this one, it runs on this base, and it names the
# commit the workflow built. git resolves to /usr/bin/git on the image PATH and the step refuses any
# other path, so git's absolute path is as fixed as gh's and jj's (/usr/local/bin, copied above) and a
# pod environment can name all three. Then `legion probe-image` runs the three launch probes through
# the daemon's own code, loading the plugin the way a Sandbox pod does (--plugin-root: the one
# explicit extension, discovery off), holds the plugin to the daemon API contract this binary speaks,
# and resolves by name every task agent and skill Legion's prompts name (shipped in its agents/ and
# dist/skills directories). It leaves those agents' models unresolved (--skip-agent-models): the build
# has none of the operator's model configuration, which the pod brings. It prints `probe-image: OK
# (/opt/omp/bin/omp) session-storage=probed agent-models=skipped daemon-api-version=<N>`; the daemon's
# probe Sandbox runs it again with its own contract, on the pod baseline and under the operator's pod,
# resolving every agent's model, and refuses a skipped result, before any claim runs on the image
# (packages/daemon/internal/runtime/sandbox/probe.go). It needs the natives step 3 fetched, which
# the cached probe layer above carries.
ARG LEGION_REVISION
RUN set -eu; \
    for dir in /opt/legion /opt/legion/bin; do \
      owner="$(stat -c '%u:%g %a' "$dir")"; echo "$dir: $owner"; test "$owner" = "0:0 755"; \
    done; \
    git="$(command -v git)"; echo "git: $git"; test "$git" = /usr/bin/git; \
    legion="$(command -v legion)"; echo "legion: $legion"; test "$legion" = /opt/legion/bin/legion; \
    agent_secrets="$(command -v agent-secrets)"; test "$agent_secrets" = /opt/legion/bin/agent-secrets; \
    version="$(legion version)"; echo "$version"; \
    test "$version" = "legion (devel) commit ${LEGION_REVISION}"; \
    legion probe-image --plugin-root /opt/legion/pi-legion-envoy --skip-agent-models; \
    agent-secrets --help >/dev/null; \
    rm -rf /home/legion/.omp/profiles/legion/logs
# The Kubernetes runtime sets every container's command explicitly
# (packages/daemon/internal/runtime/sandbox/manifest.go): the init containers run `legion
# workspace-init fetch …` and `legion workspace-init provision …`, and the main container runs `legion
# worker-shim --connect tcp://<daemon>:<worker_stream_port> --boot-token-file … -- omp --mode rpc …`.
# This ENTRYPOINT therefore only makes `docker run <image> version` and `docker run <image>
# probe-image --plugin-root /opt/legion/pi-legion-envoy --skip-agent-models` work.
ENTRYPOINT ["legion"]
