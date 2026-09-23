# syntax=docker/dockerfile:1.7
# Legion worker image: every Legion agent process under `runtime: kubernetes` runs from this image.
# Build context: the repo root. Built by .github/workflows/worker-image.yaml on the GitHub-hosted runner
# (called from release.yaml after `cli`, on every head of a pull request against main that touches the
# image files, or dispatched post-merge). Never build it on a workstation — no `docker build`,
# `docker buildx`, or `docker compose build` (Sami, 2026-09-12); the CI runner is not a workstation.
#
# Contents: pinned Bun; the TypeScript `legion` CLI compiled from this checkout (one binary: legion,
# worker-shim, credential, gh, handoff, workspace-init, probe-image) at /opt/legion/bin/legion, the one
# PATH and the ENTRYPOINT name; the Go coordinator's `legion` (packages/daemon-go) compiled from the same
# checkout at /opt/legion/go/bin/legion, off PATH, until the Go daemon replaces the TypeScript one; the
# pinned OMP fork build the daemon's default `omp_invocation` names, resolved with mise's github backend
# exactly as the daemon resolves it; @sjawhar/pi-legion-envoy packed from this checkout's packages/pi-envoy
# and linked into the isolated OMP profile `legion`; the role prompts (packages/pi-envoy/roles) at
# /opt/legion/roles for the in-cluster daemon; jj; git at /usr/bin/git (>= 2.42, from the
# debian:trixie-slim runtime base — jj's git backend requires it); gh. The last two RUNs gate the publish,
# as the runtime user: the first checks every binary runs on the base, proves jj accepts the image's git
# with a network-free `jj git clone` of a scratch repository, and executes the three launch probes (the
# daemon's two plus the session-storage probe) through `legion probe-image`; the last runs the Go
# `legion version` and the Go `legion probe-image`, which runs the same three probes, holds the plugin
# to the Go daemon API contract, and prints the OK line the Go daemon's probe Sandbox reads. A broken
# image never publishes.

# Pins not derived from daemon code. The OMP fork pin is deliberately NOT an ARG: it is printed from
# packages/daemon/src/daemon/omp-pin.ts (the single source config.ts's DEFAULT_OMP_INVOCATION uses).
ARG BUN_VERSION=1.3.14
ARG MISE_VERSION=v2026.8.12
# Sami's jj fork: what the dogfood daemon runs on sami-agents; same 0.45 line as the jj-lib inside OMP.
ARG JJ_TOOL=github:sjawhar/jj@0.45.1-sami.20260910-043938
ARG GH_TOOL=gh@2.98.0
# go.work's `go` line: the Go stage builds in workspace mode, and the golang image's GOTOOLCHAIN=local
# fails the build if go.work moves past this.
ARG GO_VERSION=1.26.1

# ------------------------------------------------------------------------------------------------
# cli: workspace install, the compiled legion CLI, the OMP pin, and the packed plugin.
FROM oven/bun:${BUN_VERSION}-slim AS cli
WORKDIR /repo
# jq: the same omp.extensions rewrite release.yaml's pi_envoy job runs. python3/make/g++: native
# devDependencies in the workspace lockfile (mirrors packages/envoy/docker/Dockerfile).
RUN apt-get update && apt-get install -y --no-install-recommends jq python3 make g++ \
    && rm -rf /var/lib/apt/lists/*
COPY package.json bun.lock ./
COPY patches patches
COPY packages/contracts/package.json packages/contracts/package.json
COPY packages/envoy-client/package.json packages/envoy-client/package.json
COPY packages/pi-envoy/package.json packages/pi-envoy/package.json
COPY packages/daemon/package.json packages/daemon/package.json
COPY packages/envoy-plugin/package.json packages/envoy-plugin/package.json
COPY packages/claude-envoy-bridge/package.json packages/claude-envoy-bridge/package.json
COPY packages/dispatch/package.json packages/dispatch/package.json
COPY packages/workspace/package.json packages/workspace/package.json
RUN bun install --frozen-lockfile
COPY packages/contracts packages/contracts
COPY packages/envoy-client packages/envoy-client
COPY packages/workspace packages/workspace
COPY packages/daemon packages/daemon
COPY packages/pi-envoy packages/pi-envoy
COPY skills skills
RUN mkdir -p /out \
    && bun build --compile --target=bun-linux-x64 packages/daemon/src/cli/index.ts --outfile /out/legion \
    && bun packages/daemon/src/daemon/omp-pin.ts > /out/omp-pin \
    && test -s /out/omp-pin
# The plugin ships from this checkout with the steps release.yaml's pi_envoy job runs before
# `bun pm pack` (prepack.sh refuses to pack with the source manifest). The tarball is unpacked into a
# directory: `omp plugin install` links a directory and rejects a tarball path (ENOTDIR).
RUN cd packages/pi-envoy \
    && jq '.omp.extensions = ["dist/envoy.js","dist/legion.js"]' package.json > tmp.json \
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
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://mise.run -o /tmp/mise-install.sh \
    && MISE_VERSION="${MISE_VERSION}" MISE_INSTALL_PATH=/usr/local/bin/mise sh /tmp/mise-install.sh \
    && rm /tmp/mise-install.sh
COPY --from=cli /out/omp-pin /omp-pin
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
# packages/daemon-go under go.work, whose other module (packages/envoy) contributes only its go.mod and
# go.sum to dependency selection — static, so it runs on any base. LEGION_REVISION is the commit the
# workflow builds; it is linked in so `legion version` names it, and the build refuses without it.
FROM golang:${GO_VERSION}-alpine AS go
WORKDIR /src
COPY go.work go.work.sum ./
COPY packages/daemon-go/go.mod packages/daemon-go/go.sum packages/daemon-go/
COPY packages/envoy/go.mod packages/envoy/go.sum packages/envoy/
RUN go mod download
COPY packages/daemon-go packages/daemon-go
WORKDIR /src/packages/daemon-go
# Declared here, after the dependency layers, so a new commit re-runs only the compile.
ARG LEGION_REVISION
RUN test -n "$LEGION_REVISION" \
    && CGO_ENABLED=0 go build -ldflags "-X main.revision=${LEGION_REVISION}" -o /out/legion ./cmd/legion

# ------------------------------------------------------------------------------------------------
# runtime: debian:trixie-slim for its git (2.47; jj 0.45's git backend needs >= 2.42 — bookworm and
# bookworm-backports stop at 2.39.5). The dynamically linked binaries copied in below were built or
# fetched on bookworm; trixie's newer glibc runs them, and the probe RUN below proves it.
FROM debian:trixie-slim
LABEL org.opencontainers.image.source=https://github.com/sjawhar/legion
# git: jj's git backend and the workers' own git use. ca-certificates: GitHub, Dispatch, model APIs.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 legion \
    && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash legion
# Pinned Bun: the binary the cli stage built with (oven/bun:${BUN_VERSION}-slim).
COPY --from=cli /usr/local/bin/bun /usr/local/bin/bun
RUN ln -s bun /usr/local/bin/bunx
COPY --from=tools /opt/omp /opt/omp
COPY --from=tools /opt/tools/jj /usr/local/bin/jj
COPY --from=tools /opt/tools/gh /usr/local/bin/gh
COPY --from=cli /out/legion /opt/legion/bin/legion
COPY --from=cli --chown=legion:legion /out/pi-legion-envoy /opt/legion/pi-legion-envoy
# The role prompt parts (`packages/pi-envoy/roles/core/*.md`, `mechanics/*.md`, and per-role
# residues — not part of the packed plugin, whose `files` is `dist`): the in-cluster daemon reads the
# configured parts for each process and concatenates them into its pod command. A daemon run from source
# finds them beside its own sources; the compiled binary's `import.meta.dir` is Bun's virtual
# /$bunfs/root (its relative path lands on a nonexistent /pi-envoy/roles), so
# LEGION_ROLE_PROMPTS_DIR names this copy instead (`resolveRolePromptsDir`, environment.ts — boot
# refuses if any prompt part is missing here).
COPY --from=cli /repo/packages/pi-envoy/roles /opt/legion/roles
# OMP_PROFILE=legion: the isolated profile the plugin is linked into (plugins resolve to
# /home/legion/.omp/profiles/legion/plugins/node_modules). LEGION_OMP_PATH: how `legion probe-image`
# — and a daemon pointed at this image — names the OMP executable without mise. HOME is explicit
# because OMP's DirResolver derives the profile root from it.
ENV OMP_PROFILE=legion \
    LEGION_OMP_PATH=/opt/omp/bin/omp \
    LEGION_ROLE_PROMPTS_DIR=/opt/legion/roles \
    HOME=/home/legion \
    PATH=/opt/legion/bin:/opt/omp/bin:/usr/local/bin:/usr/bin:/bin
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
# 3. Link the packed plugin into the legion profile (omp-plugins.lock.json records it enabled). This is
#    OMP's first run in the image, so it also downloads OMP's native modules (~345 MB) into
#    /home/legion/.omp/natives/<version>/; this layer ships them and a pod never fetches them.
# 4. Run the three launch probes: the daemon's two (pi.agents, the plugin load) plus the session-storage
#    setting probe, which only the image runs — so no image ships an OMP that would silently keep a `sql`
#    deployment's sessions on files. The order is load-bearing: `defaultRunner` (state/fetch.ts) kills any
#    single omp invocation after 30 s, so a natives download inside the first probe would read as a
#    definitive "does not expose pi.agents" failure. Step 3 must have already fetched them.
# Any failure fails the build: a broken image never publishes. The in-cluster TypeScript daemon
# (deploy/kubernetes/daemon, runtime: kubernetes) re-runs the same command with
# `--daemon-api-version <N>` in a one-shot pod of this image before it serves — the image's own CLI is
# the only thing that can read the image plugin's daemon API contract (worker-image-probe.ts).
RUN set -eu; \
    bun --version; omp --version; jj --version; gh --version; git --version; \
    scratch="$(mktemp -d)"; \
    git init --quiet --bare "$scratch/origin.git"; \
    jj git clone "$scratch/origin.git" "$scratch/clone"; \
    rm -rf "$scratch"; \
    omp plugin install /opt/legion/pi-legion-envoy; \
    legion probe-image; \
    rm -rf /home/legion/.omp/profiles/legion/logs
# The Go `legion` goes in after the probe layer: its binary differs on every commit (it links the
# commit), so a new commit rebuilds only the last two layers, never the probe layer and its natives.
COPY --from=go /out/legion /opt/legion/go/bin/legion
# The final step: the Go `legion` runs on this base and names the commit the workflow built. git resolves
# to /usr/bin/git on the image PATH and the step refuses any other path, so git's absolute path is as fixed
# as gh's and jj's (/usr/local/bin, copied above) and a pod environment can name all three. Then the Go
# `legion probe-image` runs the three launch probes through the Go daemon's own code and holds the
# plugin to the Go daemon API contract this binary speaks, printing
# `probe-image: OK (/opt/omp/bin/omp) session-storage=probed go-daemon-api-version=<N>`; the Go daemon's
# probe Sandbox runs it again with its own contract before any claim runs on the image
# (packages/daemon-go/internal/runtime/sandbox/probe.go). It needs the natives step 3 fetched, which the
# cached probe layer above carries.
ARG LEGION_REVISION
RUN set -eu; \
    git="$(command -v git)"; echo "git: $git"; test "$git" = /usr/bin/git; \
    version="$(/opt/legion/go/bin/legion version)"; echo "$version"; \
    test "$version" = "legion (devel) commit ${LEGION_REVISION}"; \
    /opt/legion/go/bin/legion probe-image; \
    rm -rf /home/legion/.omp/profiles/legion/logs
# The Kubernetes runtime (packages/daemon/src/daemon/runtime-kubernetes.ts) sets every container's
# command explicitly: the init container runs `legion workspace-init …` and the main container runs
# `legion worker-shim --connect tcp://<daemon>:<worker_stream_port> --boot-token-file … --provider-env-dir
# /var/run/legion/providers -- omp --mode rpc …` (k8s-manifests.ts); the daemon Deployment runs
# `legion start <project> --config /etc/legion/legion.yaml` from this same image. This ENTRYPOINT
# therefore only makes `docker run <image> probe-image` and `docker run <image> --help` work; the Go
# `legion` runs with `--entrypoint /opt/legion/go/bin/legion`.
ENTRYPOINT ["legion"]
