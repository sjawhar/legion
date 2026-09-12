# syntax=docker/dockerfile:1.7
# Legion worker image: every Legion agent process under `runtime: kubernetes` runs from this image.
# Build context: the repo root. Built on Depot by .github/workflows/worker-image.yaml (called from
# release.yaml after `cli`, on every head of a pull request against main that touches the image files, or
# dispatched post-merge). Never build it on a workstation — no `docker build`, `docker buildx`, or
# `docker compose build` (Sami, 2026-09-12).
#
# Contents: pinned Bun; the `legion` CLI compiled from this checkout (one binary: legion, worker-shim,
# credential, gh, handoff, probe-image); the pinned OMP fork build the daemon's default
# `omp_invocation` names, resolved with mise's github backend exactly as the daemon resolves it;
# @sjawhar/pi-legion-envoy packed from this checkout's packages/pi-envoy and linked into the isolated OMP
# profile `legion`; jj; git; gh. The last RUN executes the daemon's two boot probes (`legion probe-image`)
# as the runtime user, so a broken image never publishes.

# Pins not derived from daemon code. The OMP fork pin is deliberately NOT an ARG: it is printed from
# packages/daemon/src/daemon/omp-pin.ts (the single source config.ts's DEFAULT_OMP_INVOCATION uses).
ARG BUN_VERSION=1.3.14
ARG MISE_VERSION=v2026.8.12
# Sami's jj fork: what the dogfood daemon runs on sami-agents; same 0.45 line as the jj-lib inside OMP.
ARG JJ_TOOL=github:sjawhar/jj@0.45.1-sami.20260910-043938
ARG GH_TOOL=gh@2.98.0

# ------------------------------------------------------------------------------------------------
# cli: workspace install, the compiled legion CLI, the OMP pin, and the packed plugin.
FROM oven/bun:${BUN_VERSION}-slim AS cli
WORKDIR /repo
# jq: the same omp.extensions rewrite release.yaml's pi_envoy job runs. python3/make/g++: native
# devDependencies in the workspace lockfile (mirrors packages/envoy/docker/Dockerfile).
RUN apt-get update && apt-get install -y --no-install-recommends jq python3 make g++ \
    && rm -rf /var/lib/apt/lists/*
COPY package.json bun.lock ./
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
# runtime
FROM debian:bookworm-slim
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
# OMP_PROFILE=legion: the isolated profile the plugin is linked into (plugins resolve to
# /home/legion/.omp/profiles/legion/plugins/node_modules). LEGION_OMP_PATH: how `legion probe-image`
# — and a daemon pointed at this image — names the OMP executable without mise. HOME is explicit
# because OMP's DirResolver derives the profile root from it.
ENV OMP_PROFILE=legion \
    LEGION_OMP_PATH=/opt/omp/bin/omp \
    HOME=/home/legion \
    PATH=/opt/legion/bin:/opt/omp/bin:/usr/local/bin:/usr/bin:/bin
# Numeric uid:gid (user `legion`, created above) so Kubernetes `runAsNonRoot` can verify it from the
# image alone.
USER 1000:1000
WORKDIR /home/legion
# 1. Link the packed plugin into the legion profile (omp-plugins.lock.json records it enabled).
# 2. Run the daemon's two boot probes; OMP's first run also downloads its native modules into
#    /home/legion/.omp/natives/<version>/, so this layer ships them and a pod never fetches them.
# Any failure fails the build: a broken image never publishes.
RUN omp plugin install /opt/legion/pi-legion-envoy \
    && legion probe-image \
    && rm -rf /home/legion/.omp/profiles/legion/logs
# Placeholder only: the real entrypoint (provider *_FILE export into the OMP child, shim dial-out) is the
# Kubernetes runtime's (LEGION-24), which sets the pod command. Override with --entrypoint to run anything else.
ENTRYPOINT ["legion", "worker-shim"]
