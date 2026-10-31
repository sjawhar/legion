# Legion worker image: every Legion agent process under `runtime: kubernetes` runs from this image.
# Build context: the repo root. Built by .github/workflows/worker-image.yaml on the GitHub-hosted runner
# (called from release.yaml beside its publish job, build only, then attested by a second call after
# publish; from ci.yaml's worker-image job on every head of a pull request that touches a file it
# builds from; or dispatched post-merge). Never build it on a workstation — no `docker build`,
# `docker buildx`, or `docker compose build` (Sami, 2026-09-12); the CI runner is not a workstation.
#
# Contents: pinned Bun; the Go `legion` (packages/daemon) compiled from this checkout at go.work's
# Go version, static, at /opt/legion/bin/legion (one binary: worker-shim, launcher, workspace-init,
# gh, git-credential, probe-image and the daemon's own commands), the first PATH entry and the
# ENTRYPOINT name, with the `agent-secrets` client and the `gh` that runs `legion gh` beside it; the
# pinned OMP fork build, resolved with
# mise's github backend exactly as a tmux host resolves an `omp_invocation` naming it;
# @sjawhar/pi-envoy and @sjawhar/pi-legion packed from this checkout's packages/pi-envoy and
# packages/pi-legion at /opt/legion/pi-envoy and /opt/legion/pi-legion, the two roots a pod names as
# Oh My Pi's explicit extensions with discovery on, linked into no profile (a plugin both linked and
# explicit loads twice; the pod lane's probe refuses an image whose pi-envoy does), and
# @bopstack/pi-codegraph (installed at /opt/omp-plugins from the committed lockfile at
# packages/daemon/docker/omp-plugins), the one plugin linked into the isolated OMP profile
# `legion`, where discovery loads it — backed by the CodeGraph CLI (@colbymchenry/codegraph,
# installed from the committed lockfile at packages/daemon/docker/codegraph) at
# /opt/codegraph/bin; @sjawhar/claude-envoy built from this checkout's packages/claude-envoy at
# /opt/legion/claude-envoy, the plugin root a phase worker on the `claude` backend loads (its
# dist/envoy-channel.js is that worker's one MCP server, its legion/ the generated Legion plugin
# Claude Code loads beside it), which the plugin stage builds from source; the pin of Claude Code
# and nothing else of it — packages/daemon/docker/claude-code's
# package.json and package-lock.json, which pin @anthropic-ai/claude-code exactly with the npm
# integrity of every tarball, at /opt/legion/claude-code. The image carries no Claude Code: its
# license reserves every right and grants no redistribution, and this image is public, so a pod
# whose role runs on the `claude` backend installs that pinned release from npm at every launch,
# verified against the lockfile's integrity and the pinned version, and fails the launch if it
# cannot (claudeCodeBlock, packages/daemon/internal/runtime/sandbox/plugins.go) — the pin is in
# every image, whatever a deployment's worker_backends names; jj; git at /usr/bin/git (>= 2.42, from the debian:trixie-slim runtime base — jj's
# git backend requires it); gh; trixie's socat, which carries a connection over `kubectl exec` to a
# loopback port inside the pod (the runtime-base stage says why); what the capability check
# (packages/daemon/internal/capabilities, which `legion probe-image` runs) requires of the image: the Go
# toolchain at go.work's version (go and gofmt, /opt/go) with trixie's gcc and libc6-dev, the C
# compiler and headers Go's cgo and race detector need (cgo stays off by default), gopls,
# typescript-language-server with TypeScript's tsc and tsserver, pyright with its pyright-langserver,
# and trixie's python3 with PyYAML (python3-yaml), curl, wget, jq, openssh-client (ssh-keygen),
# util-linux (flock), postgresql-client (psql), redis-tools (redis-cli), sqlite3,
# netcat-openbsd (nc), bind9-dnsutils (dig) and chromium (the browser Oh My Pi finds on PATH);
# the PostgreSQL 18 server with pgvector and /usr/local/bin/legion-testpg, the pod's own test
# database on a pod-lifetime emptyDir (the runtime-base stage says why);
# and a generic toolchain for the
# repositories the workers work, specific to none of them: uv and uvx, Node LTS with npm and
# corepack's pnpm and yarn, the AWS CLI v2 and kubectl —
# each of these on PATH at /usr/local/bin or /usr/bin; the warm Go module cache of the two modules
# go.work binds, at /home/legion/go/pkg/mod/cache/download, which /opt/go/go.env names first in
# GOPROXY, and which carries no version list, so a version query falls through to the public
# proxy (the go stage and the toolchain stage say why);
# and the license of every third-party piece of
# all that at /usr/share/doc/legion/THIRD_PARTY_NOTICES (the notices stage; its Debian list comes from
# the image's own dpkg database).
# The last three RUNs gate the publish, as the runtime user: the first checks every binary runs on
# the base, proves jj accepts the image's git with a network-free `jj git clone` of a scratch
# repository, and links the CodeGraph plugin into the profile, which fetches Oh My Pi's natives; the second
# runs every toolchain command, the language servers', Python's and the browser's included; the last
# runs `legion version` and `legion probe-image`, which runs the three launch probes (the daemon's two
# plus the session-storage probe), holds the Legion plugin to the daemon API contract and proves it
# loads with the Envoy plugin, holds the claude-envoy root the same way — its built channel server,
# its generated Legion plugin and the daemon API contract its manifest declares — and holds Claude
# Code's pin to one a pod can install and verify, checks every image-site capability and TLS client
# against this image, and prints their results before the OK line the daemon's probe Sandbox reads.
# A broken image never publishes.
#
# The `legion` profile carries no model route, and neither does Legion: an operator's pod supplies it
# (runtime.kubernetes.pod, docs/kubernetes.md). In a pod, the Go `legion` starts Oh My Pi on Legion's
# pod baseline (packages/daemon/internal/podsafety: the worker shim, and `legion probe-image`, each
# with --pod-safety), which names no model, provider or route.

# Pins not derived from daemon code. The OMP fork pin is deliberately NOT an ARG: it is the one line of
# the repository's .omp-pin, its only home, which the tools stage copies.
ARG BUN_VERSION=1.4.4
ARG MISE_VERSION=v2026.8.12
# SHA-256 of the tools stage's linux-x64 archive (the image builds linux/amd64 only; `install-jj`'s
# mise step reads this pair from here too). Take the hash from the output of
# `gpg --decrypt SHASUMS256.asc` — never by grepping the `.asc` file itself: a forged line added
# outside its signed block does not stop `gpg --verify` from reporting a good signature, but
# `gpg --decrypt`'s stdout is exactly the signed text, nothing else. Take the hash only once that
# command reports a good signature from release key 24853EC9F655CE80B48E6C3A8B81C9D17413A06D
# (published at https://mise.jdx.dev/gpg-key.pub, and at keys.openpgp.org by the same fingerprint).
ARG MISE_SHA256=0c782233b97745fd3ed317ba3acbfd7d256e6268470373757f0cc48d57bb87e6
# Sami's jj fork: what the dogfood daemon runs on the devbox; same 0.45 line as the jj-lib inside OMP.
ARG JJ_TOOL=github:sjawhar/jj@0.45.1-sami.20260910-043938
ARG GH_TOOL=gh@2.98.0
# go.work's `go` line: the Go stage builds in workspace mode, and the golang image's GOTOOLCHAIN=local
# fails the build if go.work moves past this. The toolchain stage ships the same release to the workers
# (/opt/go), checked against GO_SHA256: the SHA-256 https://go.dev/dl/ publishes for
# go${GO_VERSION}.linux-amd64.tar.gz (its ?mode=json&include=all listing), which a version bump reads
# from there again.
ARG GO_VERSION=1.27.1
ARG GO_SHA256=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
# gopls, which the go stage builds static from golang.org/x/tools/gopls at this tag (gopls/v<version>).
ARG GOPLS_VERSION=0.23.0
# The toolchain stage's pins: each is a release version and the SHA-256 of the linux/amd64 download the
# stage fetches for it (an archive, or kubectl's single binary), which the build checks before unpacking
# or installing anything.
ARG UV_VERSION=0.13.5
ARG UV_SHA256=f68d291c75783122dad58ab77def0e13f3233f358731db5d6d7e8715603b928f
# Node 24 LTS.
ARG NODE_VERSION=24.22.1
ARG NODE_SHA256=6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6
# Chromium for Testing is pinned separately from Debian's chromium package because it carries the
# Chrome Root Store revision that distrusts the compromised emSign root. Its SHA-256 is for
# chrome-linux64.zip from Chrome for Testing's public release archive.
ARG CHROMIUM_VERSION=158.0.8117.2
ARG CHROMIUM_SHA256=80d11972d4c483716685fed17ca95cf69eb9c849b1146c0da55c07a58617f627
ARG AWS_CLI_VERSION=2.38.4
# AWS publishes no SHA-256 for the CLI, only a PGP signature. This pin is the SHA-256 of the zip whose
# `.sig` gpg verified as a good signature from the AWS CLI Team key
# FB5DB77FD5C118B80511ADA8A6310ACC4672475C. A version bump repeats that check before taking its hash.
ARG AWS_CLI_SHA256=10ab32a95b27138d7cc1c1ac4ebb4d3acab60cd0a0df2fc37c4e2c440ed40bce
# kubectl, pinned to the minor version of the API server of the production cluster the Legion workers
# run on (v1.35.7 when this pin was set): Kubernetes' version skew policy supports kubectl one minor
# version either side of its server, so a bump follows that server's minor. A deployment whose cluster
# runs another minor layers its own kubectl on this image (docs/kubernetes.md, "Per-deployment
# toolchains layer on top"). The pin is the SHA-256 Kubernetes publishes beside the binary
# (https://dl.k8s.io/release/v<version>/bin/linux/amd64/kubectl.sha256), taken only once the binary's
# keyless Sigstore signature (kubectl.sig and kubectl.cert, beside it) verifies for the release identity
# krel-staging@k8s-releng-prod.iam.gserviceaccount.com issued by https://accounts.google.com. A version
# bump repeats both checks.
ARG KUBECTL_VERSION=1.35.9
ARG KUBECTL_SHA256=3cfeaf80be482b435b0aa214aff6e0b2c312ee23c0ff20810c75517b6004c6eb
# The language servers Oh My Pi's LSP tools start for TypeScript and Python install from the
# committed lockfile at packages/daemon/docker/language-servers (COPY'd into the toolchain stage),
# with scripts off (`npm ci --ignore-scripts`): a reproducible install off that lockfile, no ARG
# pin to keep in step with it. The pin rule itself — TypeScript is the repository's own major
# because typescript-language-server wraps tsserver, which TypeScript 7 no longer ships (its npm
# package's bin is tsc alone) — lives in that package.json's description.
# apt packages carry no version pin (hadolint DL3008, ignored at each `apt-get install`): Debian's
# archive serves only a suite's current version of a package, so a pinned version stops resolving at
# the suite's next update. The base image's suite is the pin.
# The PostgreSQL server and pgvector come from apt.postgresql.org, not trixie: trixie's own archive
# stops at PostgreSQL 17, and the pod's cluster has to be the major the deployment's databases and CI
# run, 18 (packages/daemon/internal/testpg/testpg.go). That suite is no pin either, and a version pin
# is no answer there: read on 2026-10-30, the trixie-pgdg index carries the last three point releases
# of a major and no more (postgresql-18 at 18.6, 18.4 and 18.3; postgresql-17 at 17.11, 17.10 and
# 17.9, with 17.0 through 17.8 gone), so `postgresql-18=18.6-1.pgdg13+2` would stop resolving at the
# next point release. What is pinned is the repository's signing key: the runtime-base RUN fetches its
# armored export from https://www.postgresql.org/media/keys/ACCC4CF8.asc — RSA-4096 key
# B97B 0AFC AA1A 47F0 44F2 44A0 7FCC 7D46 ACCC 4CF8, uid "PostgreSQL Debian Repository" — and checks
# it against this SHA-256 before apt reads one index signed by it, so a rotated or re-exported key
# fails the build and gets read by a person instead of being trusted.
ARG PGDG_KEY_SHA256=0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76
# The CodeGraph CLI and the Oh My Pi tool that wraps it are pinned to the versions the devbox runs
# (`codegraph --version`, `omp/plugins/package.json`). Each one installs from a committed lockfile
# in the plugin stage: the tool, @bopstack/pi-codegraph, from packages/daemon/docker/omp-plugins,
# and the CLI, @colbymchenry/codegraph, from packages/daemon/docker/codegraph. Each package.json
# pins its one package, and each bun.lock pins everything Bun installs beside it, so, as for the
# language servers, there is no ARG pin to keep in step with them. An ARG cannot be that one place
# for the CLI: a lockfile is committed, so it names the version too, and the ARG would be a second
# name to keep in step. The notices stage reads the CLI's version out of that package.json, the way
# it reads the language servers' versions out of theirs.

# ------------------------------------------------------------------------------------------------
# plugin: workspace install, the Oh My Pi plugins' frozen install, the CodeGraph CLI's frozen
# install, and the two packed plugins.
# ghcr.io/sjawhar/legion/ci, the copy of oven/bun:1.4.4-slim (.github/ci-images.yaml)
FROM ghcr.io/sjawhar/legion/ci@sha256:1846adf22496bd711036aae8be147e57bdebbaea2e5264e3e0b5af1db187d9a4 AS plugin
WORKDIR /repo
# jq: the same omp.extensions rewrite release.yaml's pi_envoy_build and pi_legion_build jobs run.
# python3/make/g++: native devDependencies in the workspace lockfile (mirrors
# packages/envoy/docker/Dockerfile).
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends jq python3 make g++ \
    && rm -rf /var/lib/apt/lists/*
COPY package.json bun.lock ./
COPY patches patches
COPY packages/contracts/package.json packages/contracts/package.json
COPY packages/envoy-client/package.json packages/envoy-client/package.json
COPY packages/pi-envoy/package.json packages/pi-envoy/package.json
COPY packages/pi-legion/package.json packages/pi-legion/package.json
COPY packages/pi-shared/package.json packages/pi-shared/package.json
COPY packages/envoy-plugin/package.json packages/envoy-plugin/package.json
COPY packages/claude-envoy/package.json packages/claude-envoy/package.json
COPY packages/proof-editor/package.json packages/proof-editor/package.json
COPY packages/dispatch/package.json packages/dispatch/package.json
COPY packages/envoy/internal/dispatch/pmdoc/gen/package.json packages/envoy/internal/dispatch/pmdoc/gen/package.json
COPY docs/site/package.json docs/site/package.json
RUN bun install --frozen-lockfile
# The Oh My Pi plugins the runtime stage links into the `legion` profile (its step 3), installed with
# `bun install --frozen-lockfile` from the committed lockfile at packages/daemon/docker/omp-plugins, a
# project of its own (no root workspace), into its own prefix, /opt/omp-plugins, which the runtime
# stage copies whole. The tree is the lockfile's whatever the registry has published since, and a
# package.json the lockfile does not match fails the install instead of resolving afresh; lifecycle
# scripts stay off, as for the language servers. It follows the workspace install because every
# frozen Bun install in this file comes after the root workspace manifests
# (.github/scripts/check-bun-workspace-manifests.sh).
WORKDIR /opt/omp-plugins
COPY packages/daemon/docker/omp-plugins/package.json packages/daemon/docker/omp-plugins/bun.lock ./
RUN bun install --frozen-lockfile --ignore-scripts
# CodeGraph CLI (@colbymchenry/codegraph), installed the same way, from the committed lockfile at
# packages/daemon/docker/codegraph, a project of its own. `bin.codegraph` in the main npm package
# is a thin `#!/usr/bin/env node` launcher shim that locates and execs the per-platform
# optionalDependency (`@colbymchenry/codegraph-linux-x64`) — but this stage's base image has no
# system `node` (only bun's own fallback shim, which the final runtime stage does not copy over),
# so the shim itself cannot start. The platform package's own `bin/codegraph` is a `#!/bin/sh`
# wrapper that execs a *bundled* Node 24 runtime sitting beside it — fully self-contained, no
# system node required anywhere — so this copies that platform package alone, as `/opt/codegraph`
# in the runtime stage. The lockfile pins that bundle too, which the unlocked global install this
# replaced resolved afresh at every build. The install precedes every application-source COPY, so
# a source change to the plugins this stage packs never invalidates its layer. The bundled Node's
# version goes to /out/codegraph-node-version, for the notices stage to fetch that release's
# LICENSE.
WORKDIR /opt/codegraph-cli
COPY packages/daemon/docker/codegraph/package.json packages/daemon/docker/codegraph/bun.lock ./
RUN mkdir -p /out \
    && bun install --frozen-lockfile --ignore-scripts \
    && cp -a node_modules/@colbymchenry/codegraph-linux-x64 /out/codegraph \
    && /out/codegraph/bin/codegraph --version \
    && /out/codegraph/node --version > /out/codegraph-node-version
WORKDIR /repo
COPY packages/contracts packages/contracts
COPY packages/envoy-client packages/envoy-client
COPY packages/pi-shared packages/pi-shared
COPY packages/pi-envoy packages/pi-envoy
COPY packages/pi-legion packages/pi-legion
COPY skills skills
# Each plugin's prepack (scripts/pi-plugin-prepack.sh) builds its one bundle, stages its skills from
# skills/ into dist/skills, and writes its dist/THIRD_PARTY_NOTICES with scripts/third-party-notices.ts.
COPY scripts/pi-plugin-prepack.sh scripts/pi-plugin-prepack.sh
COPY scripts/third-party-notices.ts scripts/third-party-notices.ts

# Each plugin ships from this checkout with the steps release.yaml's pi_envoy_build and
# pi_legion_build jobs run before `bun pm pack`: the manifest pointed at the one packed bundle (the
# prepack refuses to pack with the source manifest). Each tarball is unpacked into a directory: `omp
# plugin install` links a directory and rejects a tarball path (ENOTDIR).
WORKDIR /repo/packages/pi-envoy
RUN jq '.omp.extensions = ["dist/envoy.js"]' package.json > tmp.json \
    && mv tmp.json package.json \
    && rm -f ./*.tgz && bun pm pack \
    && mkdir -p /out/pi-envoy \
    && tar xzf ./*.tgz -C /out/pi-envoy --strip-components=1
WORKDIR /repo/packages/pi-legion
RUN jq '.omp.extensions = ["dist/legion.js"]' package.json > tmp.json \
    && mv tmp.json package.json \
    && rm -f ./*.tgz && bun pm pack \
    && mkdir -p /out/pi-legion \
    && tar xzf ./*.tgz -C /out/pi-legion --strip-components=1

# claude-envoy ships as a build of this checkout's source rather than a pack: no workflow releases
# it, its skills/ are relative symlinks into the repository's skills/ that its package.json `files`
# does not list, and a Claude Code marketplace install takes the git tree itself. That tree's dist/
# and legion/ are no input here: a pull request never commits them, and main's Rebuild Claude Envoy
# plugin workflow commits them only after each merge, so they lag the source they were built from.
# `scripts/build.ts --out` writes /out/claude-envoy from the source instead: the plugin's own files
# with its skills symlinks resolved, since the image carries no repository root for them to point
# into, and a fresh dist/ and legion/. It refuses any Bun but the one .bun-version pins, which this
# stage's base image carries (ARG BUN_VERSION; .github/scripts/check-bun-version.sh holds the ARG,
# the pin and that base's row in .github/ci-images.yaml equal).
# The copy drops group and other write, which a checkout's umask may have left, since the runtime
# stage's final step refuses a plugin file anyone but root can write.
COPY .bun-version /repo/.bun-version
COPY packages/claude-envoy /repo/packages/claude-envoy
WORKDIR /repo/packages/claude-envoy
RUN set -eu; \
    bun scripts/build.ts --out /out/claude-envoy; \
    chmod -R go-w /out/claude-envoy; \
    test -f /out/claude-envoy/dist/envoy-channel.js; \
    test -f /out/claude-envoy/legion/.claude-plugin/plugin.json

# ------------------------------------------------------------------------------------------------
# tools: mise resolves the pinned OMP fork build, jj, and gh — the same backend and pin string the
# daemon hands to `mise where` on a tmux host. mise itself is checked against MISE_SHA256 before
# anything extracts or runs it, the same shape the toolchain stage below uses for uv, Node and the AWS
# CLI; .github/actions/install-jj reads the same pair for the CI runner's own mise.
# ghcr.io/sjawhar/legion/ci, the copy of debian:bookworm-slim (.github/ci-images.yaml)
FROM ghcr.io/sjawhar/legion/ci@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3 AS tools
ARG MISE_VERSION
ARG MISE_SHA256
ARG JJ_TOOL
ARG GH_TOOL
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    t=/tmp/mise; mkdir -p "$t"; \
    curl -fsSLo "$t/mise.tar.gz" \
      "https://github.com/jdx/mise/releases/download/${MISE_VERSION}/mise-${MISE_VERSION}-linux-x64.tar.gz"; \
    printf '%s  %s\n' "$MISE_SHA256" "$t/mise.tar.gz" > "$t/SHA256SUMS"; \
    sha256sum --check --strict "$t/SHA256SUMS"; \
    tar -xzf "$t/mise.tar.gz" -C "$t" --no-same-owner; \
    install -m 0755 "$t/mise/bin/mise" /usr/local/bin/mise; \
    mise --version; \
    rm -rf "$t"
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
# packages/daemon under go.work, whose other module (packages/envoy) contributes its go.mod and go.sum
# to dependency selection and its listenfd package to `legion start` — static, so it runs on any
# base. LEGION_REVISION is the commit the workflow builds; it is linked in so `legion version` names
# it, and the build refuses without it.
# The same toolchain builds gopls (static too) for the runtime stage's /usr/local/bin/gopls.
# ghcr.io/sjawhar/legion/ci, the copy of golang:1.27.1-alpine (.github/ci-images.yaml)
FROM ghcr.io/sjawhar/legion/ci@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS go
WORKDIR /src

# A `|` between proxy entries makes a go command fall through to the next source after any error,
# where a `,` falls through only after a 404 or a 410 (Go's module reference, "GOPROXY protocol").
# go.sum holds each module's hash either way, so this is a second path, not a weaker check
# (sjawhar/legion#2512).
ENV GOPROXY=https://proxy.golang.org|direct
# The notices stage's Go part: go-licenses (pinned in go-third-party-notices.sh) is built in a layer of
# its own, before the module files, so neither a source change nor a dependency bump rebuilds it.
COPY scripts/go-third-party-notices.sh /usr/local/bin/
RUN go-third-party-notices.sh --install
# gopls, in a layer of its own before the module files for the same reason: `go install pkg@version`
# builds the module at that tag with its own go.mod, whatever this checkout's go.work says, so the
# binary changes only with GOPLS_VERSION (or GO_VERSION), never with a commit.
ARG GOPLS_VERSION
RUN set -eu; \
    mkdir -p /out; \
    CGO_ENABLED=0 GOBIN=/out go install "golang.org/x/tools/gopls@v${GOPLS_VERSION}"
# The module files and their download, in a layer above every source COPY, so a commit rebuilds
# neither. GOMODCACHE moves to /gomodcache here, after the gopls and go-licenses layers. That
# keeps the modules of those two tools out of this cache. The runtime stage ships
# /gomodcache/cache/download to the workers as their warm module cache. One download serves the
# build and the workers. A module-cache stage of its own would read the same 380 MiB from
# proxy.golang.org a second time on every build that rebuilds this layer. This image's change
# (sjawhar/legion#2512) counts every such read.
ENV GOMODCACHE=/gomodcache
COPY go.work go.work.sum ./
COPY packages/daemon/go.mod packages/daemon/go.sum packages/daemon/
COPY packages/envoy/go.mod packages/envoy/go.sum packages/envoy/
# The download writes an `@v/list` file for each module. That file names the versions this cache
# holds, and no other. A file proxy answers a list request from it with no error, and a `|` in
# GOPROXY falls through to the next source on an error alone, so proxy.golang.org would never be
# asked. In every repository a role opens, `go get X@latest`, `go get -u`, `go list -m -u all`,
# `go list -m -versions` and a `go mod tidy` that adds an import of a cached module would then
# stop at the versions this build pinned, and nothing would tell the role. The RUN therefore
# deletes every list file and every `@latest` file from the cache it ships. A version query gets
# a 404 from the file proxy and falls through to the public proxy, while the `.info`, `.mod` and
# `.zip` of a pinned version still come from the cache. The final stage's toolchain step refuses
# an image that ships one of these files again.
RUN go mod download \
    && find /gomodcache/cache/download \( -path '*/@v/list' -o -name '@latest' \) -delete
COPY packages/daemon packages/daemon
COPY packages/envoy packages/envoy
WORKDIR /src/packages/daemon
# The licenses of the Go modules `legion` and `agent-secrets` compile in, with the build's CGO_ENABLED
# (which decides them, with the GOARCH this stage builds for); fails the build when one's license
# cannot be determined.
RUN mkdir -p /out && CGO_ENABLED=0 go-third-party-notices.sh /out/go-notices \
    ./cmd/legion github.com/sjawhar/envoy/cmd/agent-secrets
# Declared here, after the dependency layers, so a new commit re-runs only the compile.
ARG LEGION_REVISION
ARG PI_LEGION_VERSION
ARG PI_ENVOY_VERSION
RUN test -n "$LEGION_REVISION" \
    && ldflags="-X main.revision=${LEGION_REVISION}" \
    && if [ -n "$PI_LEGION_VERSION" ] && [ -n "$PI_ENVOY_VERSION" ]; then ldflags="$ldflags -X main.piLegionVersion=${PI_LEGION_VERSION} -X main.piEnvoyVersion=${PI_ENVOY_VERSION}"; fi \
    && CGO_ENABLED=0 go build -ldflags "$ldflags" -o /out/legion ./cmd/legion \
    && CGO_ENABLED=0 go -C /src/packages/envoy build -o /out/agent-secrets ./cmd/agent-secrets

# ------------------------------------------------------------------------------------------------
# toolchain: what a worker needs to work a repository that is not Legion's, specific to none: uv (which
# installs each project's own Python from its `.python-version` or `requires-python`; the image's own
# python3 is trixie's, installed by the runtime-base stage for Oh My Pi's Python eval), Node with npm and
# corepack, the AWS CLI v2, kubectl, the Go toolchain at go.work's version (the release the go stage
# builds `legion` with, so a worker builds the daemon as CI does), and the TypeScript and Python language
# servers Oh My Pi's LSP tools start. All five downloads are checked against their pinned SHA-256
# before any is unpacked or installed. uv, uvx and kubectl are single binaries, copied to /usr/local/bin
# as gh and jj are; Node, Go and the AWS CLI keep their own trees under /opt, and /out/bin holds the
# symlinks into them that the runtime stage copies to /usr/local/bin (the AWS installer's `--bin-dir`
# writes its two). The pnpm, pnpx, yarn and yarnpkg links are the ones `corepack enable` would write
# beside node, which the runtime user cannot: each runs the version a project's `packageManager`
# names, or else the default this corepack ships (COREPACK_DEFAULT_TO_LATEST=0, the runtime stage's
# ENV), fetched on first use into the user's own corepack cache. The language servers install from
# packages/daemon/docker/language-servers's committed lockfile (COPY'd below) with
# `npm ci --ignore-scripts`, into their own prefix, /opt/language-servers, so they land in
# /opt/language-servers/node_modules with their launchers in node_modules/.bin, linked from
# /out/bin as node's are; each launcher is a `#!/usr/bin/env node` script, which finds the image's
# node on PATH as npm's own does. typescript-language-server ships no TypeScript: it resolves
# `typescript` by Node's module resolution from its own directory, which reaches the TypeScript
# installed beside it under /opt/language-servers/node_modules, so a worker's tsserver is the
# version that package.json pins unless the repository's own node_modules holds one, which the
# server prefers. go finds its GOROOT, /opt/go, by resolving the /usr/local/bin/go
# symlink (cmd/go's findGOROOT tries the executable's own path, then its symlink target), so the image
# sets no GOROOT; the toolchain step below checks it. Right after unpacking Go, the RUN appends
# `CGO_ENABLED=0` to /opt/go/go.env, the file Go reads for a distributor's defaults: the runtime-base
# stage installs gcc for the race detector, and stock Go turns cgo on whenever it finds a C compiler on PATH,
# so without the line gcc's presence alone would flip the default of every build in a pod. The final
# stage's `ENV CGO_ENABLED=0` sets the same value for every process that inherits the image's
# environment; this line holds where that environment is scrubbed — Oh My Pi's Python eval kernel
# starts with an allowlisted environment that drops CGO_ENABLED, and a test may build with go under an
# environment it constructs. It is only a default: a role's `CGO_ENABLED=1` overrides it, as the final
# stage's ENV comment says.
# The same RUN points /opt/go/go.env's GOPROXY at the warm module cache the runtime stage copies,
# before the public proxy:
# `file:///home/legion/go/pkg/mod/cache/download|https://proxy.golang.org|direct`.
# A `|` between entries makes a go command fall through to the next source after any error. A
# module the image's cache does not hold therefore still comes from proxy.golang.org, and an image
# whose cache was deleted still builds. A version query falls through for the same reason: the go
# stage deletes every `@v/list` and `@latest` file from the cache, so the file proxy answers
# `go get X@latest` or `go list -m -versions` with a 404 and proxy.golang.org answers with the
# releases it has (that stage says more). The line replaces the GOPROXY line stock go.env carries,
# and the RUN fails if that line is absent. go.env is the place for it, not an `ENV` of the final
# stage. Two facts decide that. First, a pod tells every role `GOMODCACHE=/legion/go/mod`, the
# issue volume (roleEnvironment, packages/daemon/internal/runtime/sandbox/manifest.go). That
# volume is empty in a fresh pod and dies with it (docs/kubernetes.md, "The disk dies with its
# pod"), so the image's cache reaches a role as a proxy and not as its GOMODCACHE. Second, a
# variable the final stage's ENV sets is one `runtime.kubernetes.pod.env` may not set
# (sandbox/operatorpod.go imageEnv). A go.env default leaves GOPROXY to an operator, a role or a
# command that names its own. The process environment outranks go.env, so each of those wins.
# ghcr.io/sjawhar/legion/ci, the copy of debian:trixie-slim (.github/ci-images.yaml)
FROM ghcr.io/sjawhar/legion/ci@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4 AS toolchain
ARG UV_VERSION
ARG UV_SHA256
ARG NODE_VERSION
ARG NODE_SHA256
ARG AWS_CLI_VERSION
ARG AWS_CLI_SHA256
ARG CHROMIUM_VERSION
ARG CHROMIUM_SHA256
ARG KUBECTL_VERSION
ARG KUBECTL_SHA256
ARG GO_VERSION
ARG GO_SHA256
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip xz-utils \
    && rm -rf /var/lib/apt/lists/*
COPY packages/daemon/docker/language-servers/package.json packages/daemon/docker/language-servers/package-lock.json /opt/language-servers/
RUN set -eu; \
    t=/tmp/toolchain; mkdir -p "$t" /out/bin /opt/node; \
    curl -fsSLo "$t/uv.tar.gz" \
      "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-gnu.tar.gz"; \
    curl -fsSLo "$t/node.tar.xz" "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz"; \
    curl -fsSLo "$t/awscli.zip" "https://awscli.amazonaws.com/awscli-exe-linux-x86_64-${AWS_CLI_VERSION}.zip"; \
    curl -fsSLo "$t/go.tar.gz" "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"; \
    curl -fsSLo "$t/kubectl" "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl"; \
    curl -fsSLo "$t/chromium.zip" \
      "https://storage.googleapis.com/chrome-for-testing-public/${CHROMIUM_VERSION}/linux64/chrome-linux64.zip"; \
    printf '%s  %s\n' "$UV_SHA256" "$t/uv.tar.gz" "$NODE_SHA256" "$t/node.tar.xz" \
      "$AWS_CLI_SHA256" "$t/awscli.zip" "$GO_SHA256" "$t/go.tar.gz" \
      "$KUBECTL_SHA256" "$t/kubectl" "$CHROMIUM_SHA256" "$t/chromium.zip" > "$t/SHA256SUMS"; \
    sha256sum --check --strict "$t/SHA256SUMS"; \
    tar -xzf "$t/uv.tar.gz" -C /out/bin --strip-components=1 --no-same-owner \
      uv-x86_64-unknown-linux-gnu/uv uv-x86_64-unknown-linux-gnu/uvx; \
    install -m 0755 "$t/kubectl" /out/bin/kubectl; \
    tar -xJf "$t/node.tar.xz" -C /opt/node --strip-components=1 --no-same-owner; \
    for tool in node npm npx corepack; do ln -s "/opt/node/bin/$tool" "/out/bin/$tool"; done; \
    for shim in pnpm pnpx yarn yarnpkg; do \
      ln -s "/opt/node/lib/node_modules/corepack/dist/$shim.js" "/out/bin/$shim"; \
    done; \
    (cd /opt/language-servers && PATH="/opt/node/bin:$PATH" npm ci --ignore-scripts --omit=dev --no-audit --no-fund); \
    for tool in typescript-language-server tsc tsserver pyright pyright-langserver; do \
      ln -s "/opt/language-servers/node_modules/.bin/$tool" "/out/bin/$tool"; \
    done; \
    tar -xzf "$t/go.tar.gz" -C /opt --no-same-owner; \
    printf 'CGO_ENABLED=0\n' >> /opt/go/go.env; \
    goproxy='GOPROXY=file:///home/legion/go/pkg/mod/cache/download|https://proxy.golang.org|direct'; \
    sed -i "s#^GOPROXY=.*#$goproxy#" /opt/go/go.env; \
    grep -qxF "$goproxy" /opt/go/go.env; \
    for tool in go gofmt; do ln -s "/opt/go/bin/$tool" "/out/bin/$tool"; done; \
    unzip -q "$t/awscli.zip" -d "$t"; \
    "$t/aws/install" --install-dir /opt/aws-cli --bin-dir /out/bin; \
    mkdir -p /out/licenses; \
    cp "$t/aws/THIRD_PARTY_LICENSES" /out/licenses/aws-cli-THIRD_PARTY_LICENSES; \
    unzip -q "$t/chromium.zip" -d "$t"; \
    mv "$t/chrome-linux64" /out/chromium; \
    rm -rf "$t"

# ------------------------------------------------------------------------------------------------
# runtime-base: debian:trixie-slim for its git (2.47; jj 0.45's git backend needs >= 2.42 — bookworm
# and bookworm-backports stop at 2.39.5). The dynamically linked binaries copied in below were built
# or fetched on bookworm; trixie's newer glibc runs them, and the probe RUN below proves it.
# No later layer may install a package: the image's Debian packages are the base's own and those this
# stage's one apt RUN adds, and the debian-packages stage lists them from this stage's dpkg database,
# so a package installed in a later layer would be missing from the notices file. ENV, USER and
# WORKDIR belong in the final stage, not here:
# TestImageEnvIsWhatTheWorkerImageSets (packages/daemon/internal/runtime/sandbox/operatorpod_test.go)
# reads the last stage's ENV only.
# ghcr.io/sjawhar/legion/ci, the copy of debian:trixie-slim (.github/ci-images.yaml)
FROM ghcr.io/sjawhar/legion/ci@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4 AS runtime-base
# git: jj's git backend and the workers' own git use. ca-certificates: GitHub, Dispatch, model APIs.
# chromium, python3, curl, wget, jq, openssh-client (for ssh-keygen), util-linux (for flock),
# postgresql-client (for psql), redis-tools (for redis-cli), sqlite3, netcat-openbsd (for nc) and
# bind9-dnsutils (for dig): what the capability check's browser, eval-python and toolchain rows look for
# (packages/daemon/internal/capabilities/image.go) — the last five join flock in the toolchain row —
# trixie's since the suite is the apt pin (the note above the ARGs); chromium brings its library
# closure as its Depends (a font with it: fontconfig-config requires one, fonts-dejavu-core first),
# and its Recommends stay out with everyone else's — the setuid sandbox (chromium-sandbox) and
# fonts-liberation among them; Oh My Pi never uses the sandbox, launching the browser with
# --no-sandbox --disable-setuid-sandbox. jq and ssh-keygen are real binaries, not the harness's: the
# harness bash's jq builtin reaches only the bash tool, while a worker's subprocesses (`bun test`, a
# script, a subagent's shell) need /usr/bin/jq. openssh-client is the one apt route to ssh-keygen and
# also brings ssh, scp, sftp, ssh-agent and ssh-add, which reach no host that curl, wget, git, python3
# and socat on this line could not already reach (git's ssh transport is what they make runnable; a
# pod's git speaks https with its role's App token through gh): what a pod may reach is Legion's
# ruling, the capability table's withheld `network` row
# (packages/daemon/internal/capabilities/capabilities.go, dispatch://LEGION-5: the pod is the
# boundary, except a workload a role starts where its deployment's cluster admits it and the
# Service fronting it, docs/kubernetes.md "Running a workload in the cluster from a role"), a
# declaration the report renders, not a network control this repository ships. The
# package's two set-id files lose their bits in the RUN below: /usr/lib/openssh/ssh-keysign, setuid
# root for host-based authentication, which nothing here uses, and /usr/bin/ssh-agent, setgid _ssh by
# its postinst. A pod would ignore both anyway (allowPrivilegeEscalation false and every capability
# dropped, packages/daemon/internal/runtime/sandbox/manifest.go), and the final step refuses an image
# where any file under /usr/lib/openssh or /usr/bin/ssh-agent carries one, so a rebuild of this layer
# (a new trixie-slim digest, a package update) cannot bring a bit back unnoticed.
# socat: a pipe that carries a connection over `kubectl exec` to a port the pod listens on at 127.0.0.1
# (a dev server an agent runs), which under gVisor `kubectl port-forward` cannot reach. python3 or
# bash's /dev/tcp over the same exec works too; a listener on 0.0.0.0 is also reachable on the pod IP
# inside the cluster, unless the deployment's NetworkPolicy denies that ingress.
# util-linux (flock): the install Legion renders for an installing launch serialises on the issue's
# shared bun install cache with /usr/bin/flock (installBlock,
# packages/daemon/internal/runtime/sandbox/plugins.go), so a pod whose image lacks it
# fails every installing launch. The entry buys nothing today — Debian's priority:required set
# already brings util-linux into the base — and buys a build failure instead of a fleet-wide launch
# failure the day a base drops it. Do not delete it as redundant.
# postgresql-client (psql): `bun run e2e`, the Dispatch browser suite this deployment's
# instructions require before a reader-facing Dispatch change merges, reaches psql through
# packages/dispatch/e2e/psql.ts, which seed.ts calls for every spec's database reset; and the local
# stack `bun run dispatch:local` (packages/dispatch/e2e/local-instance.ts) refuses to start without
# psql on PATH — so a role whose image lacks psql cannot run the suite its own change owes. The
# Debian psql is a Perl wrapper around the versioned client; perl-base is already in the image, so
# the wrapper resolves.
# postgresql-18 and postgresql-18-pgvector (the server and the vector extension): the pod's own test
# database, which /usr/local/bin/legion-testpg starts under LEGION_TESTPG_DIR
# (/var/run/legion/roles/testpg, the emptyDir the daemon mounts there) and LEGION_TEST_PG_DSN names.
# Without a server in the image, a role that needs Postgres either unpacks debs onto the issue volume
# for twenty minutes or points its test at the deployment's sessions database, whose login drops
# databases on the instance the whole fleet's sessions live in. 18 is the major the deployment's
# databases and CI run (packages/daemon/internal/testpg/testpg.go), so both come from
# apt.postgresql.org: trixie serves 17 alone, and a cluster of another major is a different planner,
# a different catalog and a different dump format from the one a test meets everywhere else. The ARG
# note above says what is pinned there and why a version pin is not. The source list and the key go
# in the same RUN, since this stage's single apt RUN is the only one the image allows (the paragraph
# at the top of the stage says why): it installs ca-certificates and curl first, writes the key and
# the list, updates again and then installs the whole set. postgresql-client follows the server to 18.
# The extension goes into template1 at start, so every database a test makes from it carries pgvector
# and the Dispatch suite needs no step of its own. The RUN below deletes the cluster
# `pg_createcluster` makes from the package's postinst (/var/lib/postgresql, /etc/postgresql): no pod
# runs it, since a pod's cluster is initdb's under LEGION_TESTPG_DIR, under the runtime user. The
# server programs stay off PATH — postgresql-common wraps the client programs alone — so
# legion-testpg names initdb, pg_ctl and postgres by path.
# redis-tools (redis-cli): no repository of the project speaks Redis today. It is carried anyway,
# at about a megabyte, because a second image roll costs more than a megabyte.
# sqlite3: a repository's own SQLite fixture or database has no other way to open from a shell, and
# Oh My Pi's own profile databases under ~/.omp/profiles/legion are SQLite too. A role's session is
# not one of them: it is results.jsonl, state.json and title under XDG_STATE_HOME, and the session
# store these pods otherwise use is Postgres.
# netcat-openbsd (nc): a reachability check otherwise has to be written as an inline Python loop.
# `nc` is not a file of the package: it ships /usr/bin/nc.openbsd, and the `nc` name comes from the
# alternatives system, whose script runs as root during this build — so the checks below ask for
# `nc`, never `nc.openbsd`.
# bind9-dnsutils (dig): a role whose connection to a service name fails cannot otherwise tell "the
# name does not resolve" from "the packets are dropped". The package is named rather than the
# virtual `dnsutils`, which trixie has no candidate for: apt resolves it to this single provider
# today, and a second provider the day one appears would fail the build on a name that was never
# the package.
# gcc and libc6-dev: the C compiler and the libc headers and startup files Go's cgo builds with. Go
# refuses -race without cgo, so these are what lets a role run the race detector — the
# `go test -race …` of CI's envoy-go-race lane — as `CGO_ENABLED=1 go test -race …`, with no CC or
# linker override; gcc joins go in the toolchain row. libc6-dev is named rather than left to gcc
# because it is only a Recommends of gcc-14, and Recommends stay out here as everywhere. cgo stays off
# by default all the same (the final stage's `ENV CGO_ENABLED=0` and the toolchain stage's go.env line
# say how).
# python3-yaml: Debian's PyYAML for the image's python3, so `python3 -c "import yaml"` answers in a
# pod. Six of this repository's own gates run python3 with `import yaml`:
# .github/scripts/check-action-pins.sh, check-bun-version.sh, check-ci-images.sh,
# check-image-trigger-paths.sh, check-release-credentials.sh, and check-ci-filters.sh through
# ci_filter_coverage.py. A Legion role runs those gates on its own change before it returns the
# work. apt is the one install route here. This stage's apt RUN is the only one the image allows
# (the paragraph at the top of the stage says why), and trixie marks its python3 externally
# managed (PEP 668), which refuses a pip install into it.
# /opt/legion and /opt/legion/bin are created here, root-owned, before any COPY into them: a COPY
# creates a missing parent with its own --chown, so a runtime-user copy into /opt/legion would
# otherwise leave the runtime user free to rename bin/ and plant its own `legion`. The final step
# refuses an image where either is not root's.
# /var/run/legion/{gh,private,state,agent-secrets} are the empty, root-owned mount points an issue
# pod's role-init binds each role's own directories back at (packages/daemon/internal/launcher,
# roleinit_linux.go): inside a role's namespaces the role is the runtime user, which can create
# nothing under the root-owned /var/run, so the image makes them. The controller's pod mounts its
# volumes over the same paths, as it always has. The final step refuses an image where any is not
# root's or is not empty.
# /etc/apt/preferences.d/pgdg holds apt.postgresql.org to the PostgreSQL packages. That suite
# rebuilds packages trixie also carries, and without a pin its versions would win on candidate
# order for anything the two archives share, so a server upgrade could quietly pull a different
# libssl or ca-certificates into the fleet's image. Everything from that origin is priority 100,
# below the 500 a Debian suite gets, and `postgresql*` and libpq5 are 600: the server, the client,
# postgresql-common and the pgvector extension come from pgdg, libpq5 with them because 18's client
# needs a newer one than trixie ships, and every other package stays Debian's.
# The RUN also deletes /etc/ssl/private/ssl-cert-snakeoil.key and its certificate. postgresql-common
# depends on ssl-cert, whose postinst generates that self-signed pair at install time; this image is
# public, so it must carry no private key, and nothing here serves TLS from one — the pod's cluster
# listens on loopback with ssl off. The `find … -xtype l -delete` clears the hash link the
# certificate left behind in the trust directory, which would otherwise dangle.
ARG PGDG_KEY_SHA256
# hadolint ignore=DL3008
RUN install -d -m 0755 /etc/apt/keyrings \
    && apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && curl -fsSL -o /etc/apt/keyrings/pgdg.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc \
    && printf '%s  %s\n' "$PGDG_KEY_SHA256" /etc/apt/keyrings/pgdg.asc > /tmp/pgdg-key.sha256 \
    && sha256sum --check --strict /tmp/pgdg-key.sha256 \
    && rm -f /tmp/pgdg-key.sha256 \
    && echo 'deb [signed-by=/etc/apt/keyrings/pgdg.asc] https://apt.postgresql.org/pub/repos/apt trixie-pgdg main' \
      > /etc/apt/sources.list.d/pgdg.list \
    && printf '%s\n' 'Package: *' 'Pin: origin apt.postgresql.org' 'Pin-Priority: 100' '' \
      'Package: postgresql* libpq5' 'Pin: origin apt.postgresql.org' 'Pin-Priority: 600' \
      > /etc/apt/preferences.d/pgdg \
    && apt-get update \
    && apt-get install -y --no-install-recommends bind9-dnsutils ca-certificates chromium curl gcc git jq libc6-dev netcat-openbsd openssh-client postgresql-18 postgresql-18-pgvector postgresql-client python3 python3-yaml redis-tools socat sqlite3 util-linux wget \
    && rm -rf /var/lib/apt/lists/* /var/lib/postgresql /etc/postgresql \
      /etc/ssl/private/ssl-cert-snakeoil.key /etc/ssl/certs/ssl-cert-snakeoil.pem \
    && find /etc/ssl/certs -xtype l -delete \
    && chmod u-s /usr/lib/openssh/ssh-keysign \
    && chmod g-s /usr/bin/ssh-agent \
    && groupadd --gid 1000 legion \
    && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash legion \
    && install -d -m 0755 -o root -g root /opt/legion /opt/legion/bin \
    && install -d -m 0755 -o root -g root /var/run/legion /var/run/legion/gh /var/run/legion/private \
      /var/run/legion/state /var/run/legion/agent-secrets

# ------------------------------------------------------------------------------------------------
# debian-packages (build only): trixie's own dpkg-query over runtime-base's own database, one
# installed package per line. ${Package}, not ${binary:Package}, which prints name:amd64 for a
# Multi-Arch: same package, a name with no /usr/share/doc directory. dpkg-query writes to a file
# first so its failure fails the RUN (dash has no pipefail); only `installed` rows are kept, and
# LC_ALL=C sort -u makes the order locale-independent. An empty list fails the build in the
# assemble script's own empty-file check.
FROM runtime-base AS debian-packages
RUN dpkg-query -W -f '${db:Status-Status} ${Package}\n' > /tmp/dpkg-packages \
    && sed -n 's/^installed //p' /tmp/dpkg-packages | LC_ALL=C sort -u > /debian-packages

# ------------------------------------------------------------------------------------------------
# notices: /out/THIRD_PARTY_NOTICES, the image's /usr/share/doc/legion/THIRD_PARTY_NOTICES — the
# license of every third-party piece the image ships. Its first section names the Debian packages,
# listed by the debian-packages stage from runtime-base's dpkg database, whose terms are in
# /usr/share/doc/<package>/copyright; the npm packages installed unmodified have their own license
# files beside them. What this checkout builds carries notices generated from what the build included
# (the go stage's, the packed plugin's); a prebuilt tool carries the license files its distribution
# ships (Node's LICENSE, the AWS CLI's THIRD_PARTY_LICENSES) and those its source repository holds at
# the pinned release, fetched here. jj and uv link Rust crates whose license texts neither release
# ships, so their sections name the Cargo.lock that lists those crates; gopls, which the go stage
# compiles, links Go modules the same way, and its section names the go.mod that lists them rather
# than a go-licenses report like `legion`'s, because go-licenses (go-third-party-notices.sh's
# v2.0.1, under Go 1.27.1) finds no license for one of them, github.com/segmentio/asm, and would fail
# the build. kubectl's section names the go.mod of the Kubernetes release its binary is built from,
# the same way. The k8s.io modules that go.mod replaces with ./staging/src, kubectl itself among them,
# are that release's own source, each with its license file under staging/src/k8s.io at the release's
# tag; the repository keeps the license text of every other module it links under LICENSES/vendor
# there. The language servers' own LICENSE files come from their installed trees in the toolchain
# stage; TypeScript's ThirdPartyNoticeText.txt and the typeshed LICENSE pyright bundles come with
# them. A missing or empty file fails the build. The fetches depend only on the pins, so they come
# first, in a layer a source change reuses; the assembly reads the other stages' outputs after them.
FROM tools AS notices
ARG BUN_VERSION
ARG JJ_TOOL
ARG GH_TOOL
ARG UV_VERSION
ARG AWS_CLI_VERSION
ARG KUBECTL_VERSION
ARG GO_VERSION
ARG GOPLS_VERSION
COPY --from=plugin /out/codegraph-node-version /in/codegraph-node-version
COPY packages/daemon/docker/language-servers/package.json /in/language-servers/package.json
COPY packages/daemon/docker/codegraph/package.json /in/codegraph/package.json
# The seventeen reads below (thirteen LICENSE/NOTICE fetches, four HEAD checks) are independent
# of each other and left sequential rather than backgrounded: this whole RUN is the cached layer
# the stage comment above describes (a source change, not a pin bump, reuses it, so it rebuilds
# rarely), and running unauthenticated GitHub raw-content requests in parallel risks tripping its
# secondary rate limit for a build-time saving that doesn't matter on a layer this cold.
RUN set -eu; \
    omp_pin="$(cat /omp-pin)"; omp_repo="${omp_pin#github:}"; omp_repo="${omp_repo%@*}"; \
    omp_tag="v${omp_pin##*@}"; \
    jj_repo="${JJ_TOOL#github:}"; jj_repo="${jj_repo%@*}"; jj_tag="v${JJ_TOOL##*@}"; \
    gh_tag="v${GH_TOOL#gh@}"; \
    codegraph_node_tag="$(cat /in/codegraph-node-version)"; \
    pin() { sed -n "s|^ *\"$2\": \"\([^\"]*\)\".*|\1|p" "/in/$1/package.json"; }; \
    typescript_version=$(pin language-servers typescript); tsls_version=$(pin language-servers typescript-language-server); pyright_version=$(pin language-servers pyright); \
    codegraph_version=$(pin codegraph @colbymchenry/codegraph); \
    : "${typescript_version:?typescript pin not read from /in/language-servers/package.json}"; : "${tsls_version:?typescript-language-server pin not read from /in/language-servers/package.json}"; : "${pyright_version:?pyright pin not read from /in/language-servers/package.json}"; : "${codegraph_version:?@colbymchenry/codegraph pin not read from /in/codegraph/package.json}"; \
    printf 'omp_repo=%s\nomp_tag=%s\njj_repo=%s\njj_tag=%s\ngh_tag=%s\ncodegraph_node_tag=%s\ncodegraph_version=%s\ntypescript_version=%s\ntsls_version=%s\npyright_version=%s\n' \
      "$omp_repo" "$omp_tag" "$jj_repo" "$jj_tag" "$gh_tag" "$codegraph_node_tag" "$codegraph_version" \
      "$typescript_version" "$tsls_version" "$pyright_version" > /in/tags.env; \
    gh_raw_url() { printf 'https://raw.githubusercontent.com/%s/%s/%s' "$1" "$2" "$3"; }; \
    fetch() { curl -fsSL "$(gh_raw_url "$1" "$2" "$3")" -o "/in/$4"; }; \
    fetch oven-sh/bun "bun-v${BUN_VERSION}" LICENSE.md bun-LICENSE.md; \
    fetch "$omp_repo" "$omp_tag" LICENSE omp-LICENSE; \
    fetch "$omp_repo" "$omp_tag" THIRD-PARTY-NOTICES.txt omp-THIRD-PARTY-NOTICES.txt; \
    fetch "$jj_repo" "$jj_tag" LICENSE jj-LICENSE; \
    fetch cli/cli "$gh_tag" LICENSE gh-LICENSE; \
    fetch astral-sh/uv "$UV_VERSION" LICENSE-APACHE uv-LICENSE-APACHE; \
    fetch astral-sh/uv "$UV_VERSION" LICENSE-MIT uv-LICENSE-MIT; \
    fetch aws/aws-cli "$AWS_CLI_VERSION" LICENSE.txt aws-cli-LICENSE.txt; \
    fetch kubernetes/kubernetes "v${KUBECTL_VERSION}" LICENSE kubectl-LICENSE; \
    fetch colbymchenry/codegraph "v${codegraph_version}" LICENSE codegraph-LICENSE; \
    fetch nodejs/node "$codegraph_node_tag" LICENSE codegraph-node-LICENSE; \
    fetch golang/go "go${GO_VERSION}" LICENSE go-LICENSE; \
    fetch golang/tools "gopls/v${GOPLS_VERSION}" LICENSE gopls-LICENSE; \
    crates() { \
      curl -fsSIo /dev/null "$(gh_raw_url "$1" "$2" Cargo.lock)"; \
      printf '%s links Rust crates whose license texts its release does not ship. They are the packages listed in\nhttps://github.com/%s/blob/%s/Cargo.lock, the lockfile of the release this image installs; the\nlicense of each is published with it on crates.io.\n' "$3" "$1" "$2" > "/in/$4"; \
    }; \
    crates "$jj_repo" "$jj_tag" jj jj-crates; \
    crates astral-sh/uv "$UV_VERSION" uv uv-crates; \
    curl -fsSIo /dev/null "$(gh_raw_url golang/tools "gopls/v${GOPLS_VERSION}" gopls/go.mod)"; \
    printf 'gopls links Go modules whose license texts its binary does not carry. They are the modules listed in\nhttps://github.com/golang/tools/blob/gopls/v%s/gopls/go.mod, the module file of the release this image builds; the\nlicense of each is published with its source and shown on pkg.go.dev.\n' "$GOPLS_VERSION" > /in/gopls-modules; \
    curl -fsSIo /dev/null "$(gh_raw_url kubernetes/kubernetes "v${KUBECTL_VERSION}" go.mod)"; \
    printf 'kubectl links Go modules whose license texts its binary does not carry. They are the modules listed in\nhttps://github.com/kubernetes/kubernetes/blob/v%s/go.mod, the module file of the release this image installs.\nThe k8s.io modules that file replaces with ./staging/src (kubectl itself, client-go, apimachinery, cli-runtime\nand component-base among them) are source of the Kubernetes release itself, each with its license file under\nhttps://github.com/kubernetes/kubernetes/tree/v%s/staging/src/k8s.io; the license of every other module is in\nhttps://github.com/kubernetes/kubernetes/tree/v%s/LICENSES/vendor.\n' "$KUBECTL_VERSION" "$KUBECTL_VERSION" "$KUBECTL_VERSION" > /in/kubectl-modules
COPY scripts/assemble-third-party-notices.sh /usr/local/bin/
COPY --from=debian-packages /debian-packages /in/debian-packages
COPY --from=go /out/go-notices /in/go-notices
COPY --from=plugin /out/pi-envoy/dist/THIRD_PARTY_NOTICES /in/pi-envoy-notices
COPY --from=plugin /out/pi-legion/dist/THIRD_PARTY_NOTICES /in/pi-legion-notices
COPY --from=plugin /out/claude-envoy/dist/THIRD_PARTY_NOTICES /in/claude-envoy-notices
COPY --from=toolchain /opt/node/LICENSE /in/node-LICENSE
COPY --from=toolchain /out/licenses/aws-cli-THIRD_PARTY_LICENSES /in/aws-cli-THIRD_PARTY_LICENSES
COPY --from=toolchain /opt/language-servers/node_modules/typescript/LICENSE.txt /in/typescript-LICENSE.txt
COPY --from=toolchain /opt/language-servers/node_modules/typescript/ThirdPartyNoticeText.txt /in/typescript-ThirdPartyNoticeText.txt
COPY --from=toolchain /opt/language-servers/node_modules/typescript-language-server/LICENSE /in/typescript-language-server-LICENSE
COPY --from=toolchain /opt/language-servers/node_modules/pyright/LICENSE.txt /in/pyright-LICENSE.txt
COPY --from=toolchain /opt/language-servers/node_modules/pyright/dist/typeshed-fallback/LICENSE /in/pyright-typeshed-LICENSE
RUN set -eu; mkdir -p /out; . /in/tags.env; \
    assemble-third-party-notices.sh /out/THIRD_PARTY_NOTICES \
      "Third-party software in the Legion worker image, with the license of each piece. Legion's own code is under the Apache License 2.0. The Debian packages the image installs, listed in the first section below, carry their terms in /usr/share/doc/<package>/copyright. npm packages installed unmodified carry their own license files beside them: the CodeGraph plugin and its dependencies under /opt/omp-plugins/node_modules, the CodeGraph CLI's dependencies under /opt/codegraph/lib/node_modules, and the language servers under /opt/language-servers/node_modules (their sections below)." \
      "Debian packages installed in the image, one per line, each with its terms in /usr/share/doc/<package>/copyright" /in/debian-packages \
      "Go modules compiled into /opt/legion/bin/legion and /opt/legion/bin/agent-secrets" /in/go-notices \
      "npm packages inlined into the pi-envoy plugin, /opt/legion/pi-envoy (also its dist/THIRD_PARTY_NOTICES)" /in/pi-envoy-notices \
      "npm packages inlined into the pi-legion plugin, /opt/legion/pi-legion (also its dist/THIRD_PARTY_NOTICES)" /in/pi-legion-notices \
      "npm packages inlined into the claude-envoy plugin, /opt/legion/claude-envoy (also its dist/THIRD_PARTY_NOTICES)" /in/claude-envoy-notices \
      "Bun ${BUN_VERSION}, /usr/local/bin/bun: LICENSE.md of github.com/oven-sh/bun at bun-v${BUN_VERSION}" /in/bun-LICENSE.md \
      "Oh My Pi, /opt/omp and the native module it extracted into /home/legion/.omp/natives: LICENSE of github.com/${omp_repo} at ${omp_tag}" /in/omp-LICENSE \
      "Oh My Pi: THIRD-PARTY-NOTICES.txt of github.com/${omp_repo} at ${omp_tag}" /in/omp-THIRD-PARTY-NOTICES.txt \
      "jj, /usr/local/bin/jj: LICENSE of github.com/${jj_repo} at ${jj_tag}" /in/jj-LICENSE \
      "jj: the Rust crates it links" /in/jj-crates \
      "GitHub CLI, /usr/local/bin/gh: LICENSE of github.com/cli/cli at ${gh_tag}" /in/gh-LICENSE \
      "uv and uvx, /usr/local/bin/uv and uvx: LICENSE-APACHE of github.com/astral-sh/uv at ${UV_VERSION} (uv is offered under Apache-2.0 or MIT)" /in/uv-LICENSE-APACHE \
      "uv: LICENSE-MIT of github.com/astral-sh/uv at ${UV_VERSION}" /in/uv-LICENSE-MIT \
      "uv: the Rust crates it links" /in/uv-crates \
      "Node.js, /opt/node (with npm and corepack): the LICENSE its release archive ships" /in/node-LICENSE \
      "Node.js ${codegraph_node_tag}, /opt/codegraph/node (the runtime the CodeGraph CLI bundles): LICENSE of github.com/nodejs/node at ${codegraph_node_tag}" /in/codegraph-node-LICENSE \
      "AWS CLI v2, /opt/aws-cli: LICENSE.txt of github.com/aws/aws-cli at ${AWS_CLI_VERSION}" /in/aws-cli-LICENSE.txt \
      "AWS CLI v2: the THIRD_PARTY_LICENSES its installer archive ships" /in/aws-cli-THIRD_PARTY_LICENSES \
      "kubectl v${KUBECTL_VERSION}, /usr/local/bin/kubectl: LICENSE of github.com/kubernetes/kubernetes at v${KUBECTL_VERSION}" /in/kubectl-LICENSE \
      "kubectl: the Go modules it links" /in/kubectl-modules \
      "CodeGraph CLI, /opt/codegraph: LICENSE of github.com/colbymchenry/codegraph at v${codegraph_version}" /in/codegraph-LICENSE \
      "Go ${GO_VERSION}, /opt/go (go and gofmt at /usr/local/bin): LICENSE of github.com/golang/go at go${GO_VERSION}" /in/go-LICENSE \
      "gopls v${GOPLS_VERSION}, /usr/local/bin/gopls: LICENSE of github.com/golang/tools at gopls/v${GOPLS_VERSION}" /in/gopls-LICENSE \
      "gopls: the Go modules it links" /in/gopls-modules \
      "TypeScript ${typescript_version}, /opt/language-servers/node_modules/typescript (tsc and tsserver at /usr/local/bin): the LICENSE.txt its npm package ships" /in/typescript-LICENSE.txt \
      "TypeScript: the ThirdPartyNoticeText.txt its npm package ships" /in/typescript-ThirdPartyNoticeText.txt \
      "typescript-language-server ${tsls_version}, /opt/language-servers/node_modules/typescript-language-server (/usr/local/bin/typescript-language-server): the LICENSE its npm package ships" /in/typescript-language-server-LICENSE \
      "pyright ${pyright_version}, /opt/language-servers/node_modules/pyright (pyright and pyright-langserver at /usr/local/bin): the LICENSE.txt its npm package ships" /in/pyright-LICENSE.txt \
      "pyright: the LICENSE of the typeshed stubs it bundles, /opt/language-servers/node_modules/pyright/dist/typeshed-fallback" /in/pyright-typeshed-LICENSE

# ------------------------------------------------------------------------------------------------
# runtime: the final stage, the image itself, on runtime-base.
FROM runtime-base AS runtime
LABEL org.opencontainers.image.source=https://github.com/sjawhar/legion
# Pinned Bun: the binary the plugin stage packed with (the copy of oven/bun:${BUN_VERSION}-slim).
COPY --from=plugin /usr/local/bin/bun /usr/local/bin/bun
RUN ln -s bun /usr/local/bin/bunx
COPY --from=tools /opt/omp /opt/omp
COPY --from=tools /opt/tools/jj /usr/local/bin/jj
COPY --from=tools /opt/tools/gh /usr/local/bin/gh
# The three plugins are root's like the rest of /opt/legion: every role of an issue pod that does not
# install the plugins loads them from here, one container's filesystem, so a role that could write
# them could change the plugin code its siblings load. A launch that installs the plugins loads them
# from the role's state home on the issue volume instead, which every role of the issue can write;
# nothing here covers that copy. The final step refuses an image where anything under them is not
# root's or is writable by anyone else.
COPY --from=plugin /out/pi-envoy /opt/legion/pi-envoy
COPY --from=plugin /out/pi-legion /opt/legion/pi-legion
# claude-envoy, the plugin root a phase worker on the `claude` backend loads: its package root
# beside the two Oh My Pi roots (names.go's claudePluginDir), holding the build of this checkout
# the plugin stage made — `dist/envoy-channel.js`, the worker's one MCP server, and `legion/`, the
# generated Legion plugin Claude Code loads as a second --plugin-dir (claudelaunch).
# Every Claude role of an issue pod loads this one copy, so it is root's too.
COPY --from=plugin /out/claude-envoy /opt/legion/claude-envoy
# CodeGraph CLI (@colbymchenry/codegraph): the self-contained per-platform bundle alone (bundled
# Node runtime + app), never the npm package's own launcher shim — see the plugin stage's comment.
COPY --from=plugin /out/codegraph /opt/codegraph
# The Oh My Pi plugins' frozen install (the plugin stage's), root's like the two plugins above, so no
# role can change the CodeGraph plugin code the profile's link (step 3 below) resolves to.
COPY --from=plugin /opt/omp-plugins /opt/omp-plugins
# OMP_PROFILE=legion: the isolated profile the CodeGraph plugin is linked into (plugins resolve to
# /home/legion/.omp/profiles/legion/plugins/node_modules); the Envoy and Legion plugins stay unlinked,
# named by path (step 3 below). LEGION_OMP_PATH: how `legion probe-image`
# — and a daemon pointed at this image — names the OMP executable without mise. HOME is explicit
# because OMP's DirResolver derives the profile root from it. DO_NOT_TRACK=1: CodeGraph's telemetry
# and update-check opt-out (ranked above CODEGRAPH_TELEMETRY, above stored config, above
# default-on) — every worker's own `codegraph` call, and the warm-up the tmux daemon runs outside
# this image, must never phone home for an automatic, non-opt-in tool. No PUPPETEER_EXECUTABLE_PATH:
# Oh My Pi picks its browser by that variable first, else the first of google-chrome-stable,
# google-chrome, chromium, chromium-browser and chrome on PATH (then /usr/bin/chromium and its
# neighbours), and downloads Chrome for Testing only with none of those — so the image's chromium on
# PATH is the one every worker gets with no download, and the capability check runs that same
# resolution (`browser`, image.go). The variable stays the operator's: every variable this ENV sets
# is one runtime.kubernetes.pod.env is refused (sandbox/operatorpod.go imageEnv), and an operator
# pod naming another browser is exactly what the probe should check, not refuse.
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
# 3. Link the CodeGraph plugin's frozen install, /opt/omp-plugins/node_modules/@bopstack/pi-codegraph,
#    into the legion profile: `omp plugin link` symlinks that directory into the profile's
#    plugins/node_modules and records it enabled in omp-plugins.lock.json, resolving nothing from a
#    registry (an npm spec given to `omp plugin install` runs `bun install` in the profile with no
#    lockfile, which is how a dependency published moments before a build once failed it). Link it
#    and nothing else: /opt/legion/pi-envoy and /opt/legion/pi-legion are the two roots a
#    pod names as Oh My Pi's explicit extensions (--extension, discovery on;
#    packages/daemon/internal/runtime/sandbox/manifest.go agentArgv), and must never be linked into
#    the profile too. Oh My Pi loads an installed plugin and an explicit one by their own paths, so a
#    plugin that is both loads twice — its factory runs twice in every role. The pod lane's probe in
#    the final step refuses an image whose pi-envoy loads twice (the Envoy interface counts its
#    publishers); a re-linked pi-legion alone it cannot see, since that plugin's load marker is one
#    object the second load overwrites — so the two links come and go together, here. Loading both
#    from the profile instead, as a pane does, would stop the image probe certifying the two roots by
#    path (bootgate.go's pod lane reads the explicit root's manifest for the contract and holds
#    pi-envoy to its root): explicit roots and an unlinked profile are one decision. This is OMP's
#    first run in the image, so the CodeGraph link also extracts OMP's native module from the
#    binary (no network) into /home/legion/.omp/natives/<version>/ — the build's success is the
#    check. From 18.11.0 OMP writes only the variant the CPU selects, so this is the build runner's
#    (ubuntu-24.04, AVX2), 111 MiB; this layer ships it, so a pod whose node selects the same
#    variant extracts nothing, while a node that selects the other has each role write that one into
#    its own home overlay's upper layer, on the pod's role-homes volume, at its first OMP run: up to
#    six copies in an issue pod. The probes in the final step never wait on it.
# Any failure fails the build: a broken image never publishes.
RUN set -eu; \
    bun --version; omp --version; jj --version; gh --version; git --version; codegraph --version; \
    scratch="$(mktemp -d)"; \
    git init --quiet --bare "$scratch/origin.git"; \
    jj git clone "$scratch/origin.git" "$scratch/clone"; \
    rm -rf "$scratch"; \
    omp plugin link /opt/omp-plugins/node_modules/@bopstack/pi-codegraph; \
    rm -rf /home/legion/.omp/profiles/legion/logs
# The toolchain goes in after the probe layer, so a new toolchain pin never rebuilds that layer and its
# natives, and before `legion`, which changes on every commit. It lands outside HOME, in /opt and
# /usr/local/bin, so no volume a pod mounts under HOME shadows it, and /usr/local/bin is on the image
# PATH and on every pod's (imagePath, packages/daemon/internal/runtime/sandbox/names.go). /opt/go
# is the Go toolchain, /opt/language-servers carries the language servers (node_modules), and
# /out/bin holds every launcher's link; gopls comes from the go stage, whose gopls layer precedes any
# source, so its binary — and this layer — changes only with a pin.
COPY --from=toolchain /opt/node /opt/node
COPY --from=toolchain /opt/language-servers /opt/language-servers
COPY --from=toolchain /opt/go /opt/go
COPY --from=toolchain /opt/aws-cli /opt/aws-cli
COPY --from=toolchain /out/bin/ /usr/local/bin/
COPY --from=go /out/gopls /usr/local/bin/gopls
# The warm Go module cache: the download cache the go stage filled from go.work's module files
# (its zips, .mod and .info files and their hashes). It lands at the module cache a go with no
# GOMODCACHE in its environment reads, /home/legion/go/pkg/mod: HOME is /home/legion, and GOPATH is
# its default, $HOME/go. The extracted module trees stay out of the image. go extracts what a build
# needs from these zips, and a role's GOMODCACHE is the issue volume anyway (the toolchain stage's
# go.env paragraph), so the image carries the one form both uses read: 380 MiB of download cache,
# 419 module zips with their .mod, .info and hash files, rather than the 2.2 GiB of the whole
# module cache, which is that download cache and the extracted trees together.
# --chown makes the cache the runtime user's. Its files are mode 0644 in mode 0755 directories,
# which is what lets a role add a module the cache does not hold. The read-only 0444 files and 0555
# directories of a module cache are in the extracted trees, which this is not. The COPY sits in the
# toolchain group for the toolchain group's reason: it changes only with go.work's module files.
COPY --from=go --chown=1000:1000 /gomodcache/cache/download /home/legion/go/pkg/mod/cache/download
USER root
COPY --from=toolchain /out/chromium /opt/chromium
COPY packages/daemon/docker/tls-roots /usr/share/legion/tls-roots
# legion-testpg: root-owned and mode 0555, like every other program on the image's PATH, so a role
# cannot rewrite the script its own next start runs as the pod's setup step.
COPY --chmod=555 packages/daemon/docker/legion-testpg /usr/local/bin/legion-testpg
# Keep Debian's chromium package for its shared-library dependency closure, but make its vulnerable
# executable unreachable and expose the pinned Chrome for Testing binary under the usual name.
RUN rm -f /opt/codegraph/node /usr/bin/chromium /usr/lib/chromium/chromium \
    && ln -s /opt/node/bin/node /opt/codegraph/node \
    && ln -s /opt/chromium/chrome /usr/local/bin/chromium
USER 1000:1000
# Corepack resolves a project with no `packageManager` to the pnpm and yarn it ships as defaults,
# never to npm's newest release, so every place the image runs (a pod, `docker run`, the check below)
# gets one version until the Node pin moves. It is image ENV, not a pod variable, for that reason.
# AWS_CA_BUNDLE makes the AWS CLI use Debian's current system roots. Its vendored bundle has no
# trust path for example.com's current chain after the compromised AAA root is removed, while the
# system bundle validates that control and rejects both compromised test chains.
# CGO_ENABLED=0 keeps cgo off by default now that gcc is on PATH (the apt line above says why gcc is
# there), for every process that inherits the image's environment: the bash tool, subagents, and go
# test's test binaries. The toolchain stage's go.env line alone would not reach two of them: a go/build
# reader such as go/types' source importer (packages/daemon/internal/supervise/table_test.go) reads only
# the process environment, never go.env, so this ENV is what makes its cgo setting match the go
# command's; and a toolchain Go switches to for GOTOOLCHAIN or a go.mod `toolchain` line has its own
# go.env, which lacks the line. A role opts in per command, `CGO_ENABLED=1 go test -race …`;
# `go env -w CGO_ENABLED=1` does not override it, since Go ranks the environment above its go env file;
# and as image ENV a deployment's runtime.kubernetes.pod.env may not set it (sandbox/operatorpod.go
# imageEnv).
ENV AWS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt COREPACK_DEFAULT_TO_LATEST=0 CGO_ENABLED=0
# The toolchain step: every toolchain command runs as the runtime user from the image PATH, the
# corepack shims fetching their shipped default pnpm and yarn, with TMPDIR, COREPACK_HOME and
# XDG_CONFIG_HOME in a scratch directory the step removes, so the layer keeps nothing (go, gofmt and
# gopls write Go telemetry counters under the user config directory on every run, local mode being
# Go's default; XDG_CONFIG_HOME is where they look for it). The image's own Python is checked here,
# python3 --version, and so is the PyYAML the gates import, `python3 -c 'import yaml'`;
# uv still installs each project's own at run time. `go version` alone would pass
# with a GOROOT go cannot find, and every `go build` would then fail, so the step requires GOROOT to
# resolve to /opt/go through the /usr/local/bin/go symlink. gcc answers --version and is checked beside
# go, whose cgo (and so whose race detector) needs it. The step then reads cgo's default with
# CGO_ENABLED removed from the environment (XDG_CONFIG_HOME in scratch, so no `go env -w` file is
# read), where only /opt/go/go.env's line holds it at 0, so the build fails if that line, the default
# a scrubbed environment depends on, is ever lost. It reads GOPROXY the same way, with GOPROXY
# removed from the environment, and requires the line the toolchain stage wrote into the same
# go.env: the warm module cache reaches a role through that line alone. It then requires the cache
# to carry no `@v/list` and no `@latest` file. One of those files would answer every version query
# of every repository a role opens with the versions this image pinned, and the fall-through to
# proxy.golang.org happens on an error alone (the go stage says more). It then holds the cache
# itself to the runtime user and to a writable download directory. A role that cannot write there
# cannot add a module the cache does not hold. The step prints how many modules the cache carries.
# gofmt takes no --version; formatting a
# line from stdin is its check. pyright-langserver takes none either (it opens its LSP connection
# first and refuses without --stdio), so `pyright --version`, the same package's CLI, answers for it.
# Being its own layer above `legion`, it reruns only when the toolchain or a layer before it changes,
# so a commit that only rebuilds `legion` fetches nothing from a registry. pnpx is `pnpm dlx`, which
# takes no --version; its --help, whose first line names the pnpm version, is the check. `kubectl
# version --client` reports the client alone: it reads no kubeconfig and contacts no API server. socat
# answers -V, not --version, with its compiled-in features after the version line; the step prints
# that line alone. ssh-keygen takes no --version (its -V is the certificate-validity option), so it
# generates a throwaway ed25519 key in the scratch directory, which -l fingerprints and the step
# removes. flock answers --version; it is on this line because an installing launch's rendered
# install serialises on the issue's shared bun install cache with it.
# psql, redis-cli and sqlite3 answer --version, psql's being the Debian Perl wrapper, which resolves
# because perl-base is in the image; nc takes no --version, its -h printing usage and exiting 0, and
# it is checked under `nc`, the name the alternatives system installs, never nc.openbsd; and dig -v
# prints its version on stdout, measured in a pod of this image (`dig -v 2>/dev/null` prints
# `DiG 9.20.30-1~deb13u1-Debian`, `dig -v 2>&1 >/dev/null` prints nothing), so the step takes it as
# it comes.
# legion-testpg runs here for real, not as a version print: it initdbs a cluster in the scratch
# directory as the runtime user, starts it, makes a database from template1 and reads pgvector back
# out of it, then stops it with `legion-testpg stop`, a fast shutdown it waits for — a postmaster
# killed instead would leave its shutdown checkpoint running and the `rm -rf` below would race it.
# An issue pod's launcher runs the same command, so a build whose server package, extension or
# script no longer works fails here rather than at every role's start in every pod. The DSN is the
# one the runtime sets for every role: a loopback host, no password, and legion_test, the
# non-superuser role `ensure` creates, not the cluster's own superuser.
RUN set -eu; \
    scratch="$(mktemp -d)"; \
    export TMPDIR="$scratch" COREPACK_HOME="$scratch/corepack" XDG_CONFIG_HOME="$scratch/config"; \
    uv --version; uvx --version; node --version; npm --version; npx --version; corepack --version; \
    aws --version; kubectl version --client; \
    for shim in pnpm yarn yarnpkg; do "$shim" --version; done; \
    pnpx --help > "$scratch/pnpx-help"; sed -n 1p "$scratch/pnpx-help"; \
    go version; goroot="$(go env GOROOT)"; echo "GOROOT: $goroot"; test "$goroot" = /opt/go; gcc --version; \
    test "$(env -u CGO_ENABLED go env CGO_ENABLED)" = 0; \
    test "$(env -u GOPROXY go env GOPROXY)" = 'file:///home/legion/go/pkg/mod/cache/download|https://proxy.golang.org|direct'; \
    modcache=/home/legion/go/pkg/mod/cache/download; \
    test -z "$(find "$modcache" \( -path '*/@v/list' -o -name '@latest' \) -print -quit)"; \
    test "$(stat -c '%U' "$modcache")" = legion; test -w "$modcache"; \
    printf 'go module cache: %s modules\n' "$(find "$modcache" -name '*.zip' | wc -l)"; \
    printf 'package main\n' | gofmt; gopls version; \
    typescript-language-server --version; tsc --version; pyright --version; \
    python3 --version; python3 -c 'import yaml'; curl --version; wget --version; jq --version; chromium --version; \
    socat -V > "$scratch/socat-V"; sed -n '/^socat version/p' "$scratch/socat-V"; \
    ssh-keygen -q -t ed25519 -N '' -f "$scratch/probe-key"; ssh-keygen -l -f "$scratch/probe-key.pub"; \
    flock --version; \
    psql --version; redis-cli --version; sqlite3 --version; nc -h; dig -v; \
    legion-testpg check; \
    probe_dsn='postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable'; \
    LEGION_TESTPG_DIR="$scratch/testpg" LEGION_TEST_PG_DSN="$probe_dsn" legion-testpg ensure; \
    psql -X -q "$probe_dsn" -c 'create database probe'; \
    psql -X -tAq 'postgres://legion_test@127.0.0.1:5433/probe?sslmode=disable' -c "select extname from pg_extension where extname = 'vector'" | grep -qx vector; \
    psql -X -q "$probe_dsn" -c 'drop database probe with (force)'; \
    LEGION_TESTPG_DIR="$scratch/testpg" legion-testpg stop; \
    rm -rf "$scratch"
# Claude Code's pin, packages/daemon/docker/claude-code's package.json and package-lock.json, at
# /opt/legion/claude-code (claudeCodePin, packages/daemon/internal/runtime/sandbox/names.go):
# root's, under the root-owned /opt/legion the final step checks, and all of Claude Code this
# image carries. A Claude role's start script copies both into its generation's directory on the
# issue's volume and runs `npm ci --ignore-scripts --omit=dev` from them there, then holds the
# installed binary's --version to this package.json's pin (claudeCodeBlock,
# packages/daemon/internal/runtime/sandbox/plugins.go). After the toolchain step, so a pin bump
# reruns no layer above it.
COPY packages/daemon/docker/claude-code/package.json packages/daemon/docker/claude-code/package-lock.json /opt/legion/claude-code/
# The notices stage's file, after the toolchain step so a notices change never reruns it, and before
# `legion`, which changes on every commit while the notices change only with a dependency, a pin, or
# runtime-base's package set.
COPY --from=notices /out/THIRD_PARTY_NOTICES /usr/share/doc/legion/THIRD_PARTY_NOTICES
# `legion` goes in after the probe layer and the toolchain: its binary differs on every commit (it
# links the commit), so a new commit rebuilds only the layers from here down, never the probe layer and
# its natives.
COPY --from=go /out/legion /opt/legion/bin/legion
# agent-secrets (packages/envoy/cmd/agent-secrets): the pod's secrets client — the shim
# runs `keygen` before its hello and `renew` after its enrollment, and the agent's tools call it
# from PATH, which /opt/legion/bin leads in the image and in every worker container (the PATH
# mainEnvironment sets in packages/daemon/internal/runtime/sandbox/manifest.go). The daemon's
# Tools.AgentSecrets names this path.
COPY --from=go /out/agent-secrets /opt/legion/bin/agent-secrets
# gh in a pod is `legion gh`, first on the image PATH, which runs the real gh at /usr/local/bin/gh
# (LEGION_GH_PATH, which every agent container is told) with GH_CONFIG_DIR at the gh directory of
# the owner the command names: packages/daemon/docker/gh, the same script a tmux pane's launcher
# directory holds (packages/daemon/internal/runtime/workerbin).
COPY --chmod=0755 packages/daemon/docker/gh /opt/legion/bin/gh
# The final step: /opt/legion and /opt/legion/bin are root's with mode 0755, so the runtime user can
# neither rename nor replace what they hold; everything under /opt/legion/pi-envoy,
# /opt/legion/pi-legion and /opt/legion/claude-envoy is root's and writable by no one else, so no
# role changes the image's copy
# of the plugin code (a launch that installs the plugins loads them instead from the role's state
# home on the issue volume, which every role of the issue can write); the role mount points under
# /var/run/legion are root's, mode 0755 and empty;
# and no file under /usr/lib/openssh or /usr/bin/ssh-agent
# carries a setuid or setgid bit (the apt RUN drops the two openssh-client installs, ssh-keysign's and
# ssh-agent's), so a rebuild of that layer that brings one back fails here.
# Then the `legion` on the image PATH is this one, it runs on this base, and it names the
# commit the workflow built. git resolves to /usr/bin/git on the image PATH and the step refuses any
# other path, so git's absolute path is as fixed as jj's and the real gh's (/usr/local/bin, copied
# above) and a pod environment can name all three; gh resolves to the /opt/legion/bin/gh script and
# the real gh behind it runs. flock is pinned the same way, and for a reason the other gates do not
# cover: the installs Legion renders call it as /usr/bin/flock — the plugin pair's for an
# installing launch (installBlock) and Claude Code's for every Claude launch (claudeCodeBlock), both
# packages/daemon/internal/runtime/sandbox/plugins.go — while the apt line, the toolchain check
# above and the toolchain capability row all ask only that `flock` be on PATH, so a base that kept
# flock and moved it would pass every one of those and fail every such launch. The declaration and
# the use have to agree on the path, and this is where they are held to it. npm is pinned at
# /usr/local/bin/npm for the same reason: claudeCodeBlock calls it by that path, while the
# toolchain check asks only that `npm` be on PATH.
# nc is pinned for the neighbouring reason: it is the one name here the alternatives system
# supplies rather than its own package (netcat-openbsd ships /usr/bin/nc.openbsd), so the
# alternatives link is what the name resolves through and a base that changed it would pass every
# other gate. The Envoy plugin's `dispatch` shim runs its bundled CLI under
# the image's bun and prints its help, so a pi-envoy packed without bin/ or dist/dispatch.js, or an
# image without bun, fails here; that the extension puts bin/ first on an agent's PATH is pi-envoy's
# own test. Then `legion probe-image` runs the three launch probes through
# the daemon's own code, loading the plugins the way a Sandbox pod does (--envoy-plugin-root then
# --plugin-root: the two explicit extensions, discovery on, so the profile's CodeGraph plugin and a
# repository's own extensions load beside the two roots; a pi-envoy that loaded twice, a profile
# link beside the explicit root, is refused), holds the Legion plugin to the daemon
# API contract this binary speaks and to the Envoy plugin's interface, and resolves by name every
# task agent and skill Legion's prompts name (shipped in the Legion plugin's agents/ and the two
# plugins' dist/skills directories). It leaves those agents' models unresolved
# (--skip-agent-models): the build has none of the operator's model configuration, which the pod
# brings. --claude-root and --claude-code-pin name the Claude lane's two paths — the image's own,
# which they default to, named here so this line says what it checks: the claude-envoy root's
# built channel server (dist/envoy-channel.js) and generated Legion plugin
# (legion/.claude-plugin/plugin.json), each a regular file, and that root's manifest held to the
# same daemon API contract the Legion plugin is; and Claude Code's pin at /opt/legion/claude-code,
# which must pin one exact version and lock it and its linux-x64 package with the sha512 integrity
# `npm ci` holds each tarball to — so an image whose Claude worker could not start fails here
# rather than at a pod's first claim. The image carries no Claude Code, and this step fetches none:
# what it proves is that the image is self-consistent, and the fetch itself is proved where it
# happens, by every Claude launch's start script, which fails that launch when the fetch fails or
# does not verify (verifyClaude, packages/daemon/internal/daemon/bootgate.go, says why the probe
# reads the pin rather than installing from it). Then it checks every image-site capability as a
# worker's Oh My Pi would find each:
# `omp setup python --check`; the chromium on PATH (or the browser a PUPPETEER_EXECUTABLE_PATH names)
# with --version; gopls, typescript-language-server and pyright-langserver on PATH; the CodeGraph CLI
# on PATH with its plugin enabled in the profile's lock; and go, gcc, curl, wget, python3, node, bun,
# uv, kubectl, jq, ssh-keygen, flock, psql, redis-cli, sqlite3, nc and dig on PATH with `go version`
# running, and the claude-code row's claude-envoy root and Claude Code pin as the lane held them
# with npm and node on PATH (packages/daemon/internal/capabilities/image.go). It then has node, bun,
# omp's embedded bun,
# CodeGraph's Node, uv, the AWS CLI, curl, Python, gh and Chromium refuse each compromised chain while
# reaching the control. It
# prints the table, one `probe-image: capability <name>: <status> (<detail>)` line per row, every TLS
# decision, then `probe-image: OK (/opt/omp/bin/omp) session-storage=probed extensions=discovered
# agent-models=skipped capabilities=checked model-fallback=on daemon-api-version=<N>` — `on` here is
# Oh My Pi's own default for retry.modelFallback, since the build runs under no operator overlay; a
# probe pod reads the operator's value — and a build whose image lacks a capability or trusts a
# compromised chain fails here, the probe naming every failure. The daemon's probe Sandbox runs it
# again with its own contract, on the pod baseline and under the operator's pod, resolving every
# agent's model, and refuses a skipped result, before any claim runs on the image
# (packages/daemon/internal/runtime/sandbox/probe.go).
# It needs the natives step 3 extracted, which the cached probe layer above carries. The step leaves no
# residue: Oh My Pi's logs, and the Go telemetry counters the capability check's `go version` writes
# under HOME (the toolchain step's XDG_CONFIG_HOME does not reach a probe that runs as a pod would).
ARG LEGION_REVISION
ARG PI_LEGION_VERSION
ARG PI_ENVOY_VERSION
RUN set -eu; \
    for dir in /opt/legion /opt/legion/bin /var/run/legion /var/run/legion/gh /var/run/legion/private \
      /var/run/legion/state /var/run/legion/agent-secrets; do \
      owner="$(stat -c '%u:%g %a' "$dir")"; echo "$dir: $owner"; test "$owner" = "0:0 755"; \
    done; \
    for dir in /var/run/legion/gh /var/run/legion/private /var/run/legion/state /var/run/legion/agent-secrets; do \
      test -z "$(ls -A "$dir")"; \
    done; \
    plugins="$(find /opt/legion/pi-envoy /opt/legion/pi-legion /opt/legion/claude-envoy \( ! -user 0 -o ! -group 0 -o -perm /022 \) -print)"; \
    echo "plugin files not root's alone: ${plugins:-none}"; test -z "$plugins"; \
    stat -c '%a %n' /usr/lib/openssh/ssh-keysign /usr/bin/ssh-agent; \
    setid="$(find /usr/lib/openssh /usr/bin/ssh-agent -perm /6000)"; echo "set-id bits: ${setid:-none}"; test -z "$setid"; \
    git="$(command -v git)"; echo "git: $git"; test "$git" = /usr/bin/git; \
    flock="$(command -v flock)"; echo "flock: $flock"; test "$flock" = /usr/bin/flock; \
    nc="$(command -v nc)"; echo "nc: $nc"; test "$nc" = /usr/bin/nc; \
    legion="$(command -v legion)"; echo "legion: $legion"; test "$legion" = /opt/legion/bin/legion; \
    gh="$(command -v gh)"; echo "gh: $gh"; test "$gh" = /opt/legion/bin/gh; /usr/local/bin/gh --version; \
    npm="$(command -v npm)"; echo "npm: $npm"; test "$npm" = /usr/local/bin/npm; \
    DISPATCH_HOST=omp DISPATCH_SESSION_ID=image-check HOME=/tmp \
      /opt/legion/pi-envoy/bin/dispatch --help > /tmp/dispatch-help; \
    echo "dispatch: $(sed -n 1p /tmp/dispatch-help)"; grep -q '^Usage: dispatch ' /tmp/dispatch-help; \
    rm /tmp/dispatch-help; \
    agent_secrets="$(command -v agent-secrets)"; test "$agent_secrets" = /opt/legion/bin/agent-secrets; \
    version="$(legion version)"; echo "$version"; \
    expected="legion (devel) commit ${LEGION_REVISION}"; \
    if [ -n "$PI_LEGION_VERSION" ] && [ -n "$PI_ENVOY_VERSION" ]; then \
      expected="$(printf '%s\nplugins @sjawhar/pi-legion %s @sjawhar/pi-envoy %s' "$expected" "$PI_LEGION_VERSION" "$PI_ENVOY_VERSION")"; \
    fi; \
    test "$version" = "$expected"; \
    legion probe-image --plugin-root /opt/legion/pi-legion --envoy-plugin-root /opt/legion/pi-envoy --claude-root /opt/legion/claude-envoy --claude-code-pin /opt/legion/claude-code --skip-agent-models; \
    agent-secrets --help >/dev/null; \
    rm -rf /home/legion/.omp/profiles/legion/logs /home/legion/.config/go
# The Kubernetes runtime sets every container's command explicitly
# (packages/daemon/internal/runtime/sandbox/manifest.go): the init containers run `legion
# workspace-init fetch …` and `legion workspace-init provision …`, and the main container runs `legion
# worker-shim --connect tcp://<daemon>:<worker_stream_port> --boot-token-file … -- omp --mode rpc …`.
# This ENTRYPOINT therefore only makes `docker run <image> version` and `docker run <image>
# probe-image --plugin-root /opt/legion/pi-legion --envoy-plugin-root /opt/legion/pi-envoy
# --claude-root /opt/legion/claude-envoy --claude-code-pin /opt/legion/claude-code
# --skip-agent-models` work.
ENTRYPOINT ["legion"]
