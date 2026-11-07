# Build context is the repo root (bun workspace + Go module in packages/envoy).

# Dispatch SPA build + Go contract codegen. Both come from the bun workspace,
# so they share one stage keyed off the root lockfile.
# Bun is pinned to the version the repo locks, like packages/daemon/docker/worker.Dockerfile:
# bundler output differs across Bun releases, so a published SPA must not be built by whatever
# 1.x is newest that day. The web stage runs the copy of that Bun, named by digest;
# .github/scripts/check-bun-version.sh holds that row's upstream tag to .bun-version.
# ghcr.io/cybermonklabs/legion/ci, the copy of oven/bun:1.4.4 (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:1d22548f177f4ff38e0c15baa3c2c46c6c2060ba2d1bc573bb4720b407548f1d AS web

WORKDIR /repo

# The row above names a Bun in its text, and an oven/bun image declares its version in no part
# of its config, so no registry read can hold that label to the bytes it points at
# (.github/scripts/mirror-ci-images.sh says so where it holds a library/golang row to its own
# GOLANG_VERSION). This build is where the two meet: it refuses any Bun but the one
# .bun-version pins, before it installs or builds anything, as the plugin stage of
# packages/daemon/docker/worker.Dockerfile does through its build script.
COPY .bun-version .bun-version
RUN set -eu; \
    pinned="$(tr -d '[:space:]' < .bun-version)"; \
    running="$(bun --version)"; \
    if [ "$running" != "$pinned" ]; then \
      echo "This stage runs Bun $running, and .bun-version pins $pinned. The row this stage's FROM names is labelled with one Bun and carries another, so that row's label and its bytes disagree. Read the oven/bun row of .github/ci-images.yaml, then write the digest of the Bun the pin names." >&2; \
      exit 1; \
    fi

# Both fetches of this RUN retry: five attempts, the fifth starting 150 s after the first, each
# attempt under a bound of its own. 120 s for the index read, a few megabytes from the Debian
# mirror; 300 s for the package set, which fetches and unpacks python3, make and g++. `rm -rf`
# fetches nothing and stays bare. Every apt bound in this file is that pair, for that reason.
# Each `apt-get update` in this file also carries `-o APT::Update::Error-Mode=any`, which makes a
# failed index read fail the command instead of warning and exiting 0: without it the retry has
# nothing to retry. .github/scripts/check-download-retry.sh's rule 5 is the whole reason.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends python3 make g++ \
    && rm -rf /var/lib/apt/lists/*

COPY package.json bun.lock ./
COPY patches patches
COPY packages/contracts/package.json packages/contracts/package.json
COPY packages/broker-client/package.json packages/broker-client/package.json
COPY packages/envoy-client/package.json packages/envoy-client/package.json
COPY packages/pi-envoy/package.json packages/pi-envoy/package.json
COPY packages/pi-legion/package.json packages/pi-legion/package.json
COPY packages/pi-shared/package.json packages/pi-shared/package.json
COPY packages/claude-envoy/package.json packages/claude-envoy/package.json
COPY packages/dispatch/package.json packages/dispatch/package.json
COPY packages/proof-editor/package.json packages/proof-editor/package.json
# A dependency of that manifest, not a registry fetch: `file:./proof-sdk-<commit>.tgz` is the
# upstream editor sources, so the install below reads them from the context rather than the network.
COPY packages/proof-editor/proof-sdk-dc6a852b2ab45463dc9d3d17762235450c8ff0d6.tgz packages/proof-editor/
COPY packages/envoy/internal/dispatch/pmdoc/gen/package.json packages/envoy/internal/dispatch/pmdoc/gen/package.json
COPY docs/site/package.json docs/site/package.json
# The one fetch of this layer, and the largest in the file: Bun reads every dependency the root
# lockfile pins from the npm registry, tens of megabytes of tarballs, so an attempt gets 600 s.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 600 "bun install" bun install --frozen-lockfile

COPY packages/contracts packages/contracts
COPY packages/dispatch packages/dispatch
COPY packages/proof-editor packages/proof-editor
# The SPA build writes dist/THIRD_PARTY_NOTICES.txt with this (packages/dispatch/web/vite.config.ts).
COPY scripts/third-party-notices.ts scripts/third-party-notices.ts
RUN cd packages/contracts && bun run gen:go
RUN cd packages/dispatch && bun run build:web

# Go version must match go.mod.
# ghcr.io/cybermonklabs/legion/ci, the copy of golang:1.27.3-alpine (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build

WORKDIR /src

# A `|` between proxy entries makes a go command fall through to the next source after any error,
# where a `,` falls through only after a 404 or a 410 (Go's module reference, "GOPROXY protocol").
# go.sum holds each module's hash either way, so this is a second path, not a weaker check
# (sjawhar/legion#2512).
ENV GOPROXY=https://proxy.golang.org|direct

# The image's third-party notices: go-licenses (pinned in go-third-party-notices.sh) is built in a
# layer of its own, before the module files, so neither a source change nor a dependency bump
# rebuilds it.
COPY scripts/go-third-party-notices.sh scripts/assemble-third-party-notices.sh /usr/local/bin/
# go-licenses comes down and is built in one act (`go install …@v2.0.1`, inside the script), with no
# download to separate out, so the retry wraps the script: 600 s an attempt, one module tree
# fetched from the proxy and compiled.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 600 "go-licenses install" go-third-party-notices.sh --install
COPY packages/envoy/go.mod packages/envoy/go.sum ./
# The module cache of the four binaries below, this layer's only fetch and a few hundred megabytes
# of module zips from the proxy: 600 s an attempt. The builds that follow read this cache and
# download nothing, so they stay bare.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 600 "go mod download" go mod download

COPY packages/envoy/cmd ./cmd
COPY packages/envoy/internal ./internal
# listenfd: the inherited listening socket envoy-dispatch and envoy-listener serve on
# (DISPATCH_LISTEN_FD, ENVOY_LISTEN_FD).
COPY packages/envoy/listenfd ./listenfd
# going: the chat client, the answer filter and the run the going routes answer with, a package
# outside internal/ so the daemon can speak the same stream (going/events.go). envoy-dispatch
# compiles it in, so the build needs it here beside cmd and internal.
COPY packages/envoy/going ./going
# migrationid: the rule a store's migration identifiers follow, which internal/pgmigrate reads to
# name the remedy a collision takes in the set that refused it.
COPY packages/envoy/migrationid ./migrationid
# roleholder: the no-holder reasons envoy-listener's role publish refusal names.
COPY packages/envoy/roleholder ./roleholder
# The Go contracts file is generated from packages/contracts/schemas; never
# trust the copy checked into the tree over a fresh regeneration.
COPY --from=web /repo/packages/envoy/internal/contracts/generated.go ./internal/contracts/generated.go

# /out/THIRD_PARTY_NOTICES, the image's /usr/share/doc/legion/THIRD_PARTY_NOTICES: the licenses of
# the Go modules the four binaries below compile in (with the build's CGO_ENABLED and the image's
# own GOARCH, which decide them) and of the npm packages the SPA inlines. Fails the build when a
# module's or a package's license cannot be determined.
COPY --from=web /repo/packages/dispatch/web/dist/THIRD_PARTY_NOTICES.txt /tmp/dispatch-web-notices
RUN mkdir -p /out \
    && CGO_ENABLED=0 go-third-party-notices.sh /tmp/go-notices \
      ./cmd/listener ./cmd/dispatch ./cmd/broker ./cmd/agent-secrets \
    && assemble-third-party-notices.sh /out/THIRD_PARTY_NOTICES \
      "Third-party software in the Legion Envoy image, with the license of each piece. Legion's own code is under the Apache License 2.0. The Debian packages the image installs carry their terms in /usr/share/doc/<package>/copyright." \
      "Go modules compiled into /usr/local/bin/envoy-listener, envoy-dispatch, envoy-broker and agent-secrets" /tmp/go-notices \
      "npm packages inlined into the Dispatch web app, /srv/dispatch-web (served as /THIRD_PARTY_NOTICES.txt)" /tmp/dispatch-web-notices

# The legion commit this image is built from, which envoy-dispatch's /healthz reports as
# `commit`. The release workflow passes its own github.sha, for the image it publishes and for
# the smoke image it tests first. Left empty — a local compose or hand build, and the
# pull-request lane's smoke image, which publishes nothing to match — the binary reports null;
# anything else must be a full commit sha, so no placeholder is ever reported as a commit.
ARG LEGION_COMMIT=
RUN if [ -n "$LEGION_COMMIT" ] && ! printf '%s' "$LEGION_COMMIT" | grep -Eqx '[0-9a-f]{40}'; then \
      echo "LEGION_COMMIT must be a full 40-character commit sha or empty, got '$LEGION_COMMIT'" >&2; exit 1; \
    fi \
    && CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -o /out/envoy-listener ./cmd/listener \
    && CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -ldflags "-X main.buildCommit=${LEGION_COMMIT}" -o /out/envoy-dispatch ./cmd/dispatch \
    && CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -o /out/envoy-broker ./cmd/broker \
    && CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -o /out/agent-secrets ./cmd/agent-secrets

# Minimal runtime — static binary, no shell needed.
# curl is included for HEALTHCHECK and operational debugging.
# ghcr.io/cybermonklabs/legion/ci, the copy of debian:bookworm-slim (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3

LABEL org.opencontainers.image.source=https://github.com/cybermonklabs/legion

# The two apt fetches retry on the bounds the web stage's RUN sets, 120 s for the index and 300 s
# for the package set, and the index read carries that RUN's error-mode option; groupadd,
# useradd, mkdir and chown fetch nothing and stay bare.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r envoy && useradd -r -g envoy envoy \
    && mkdir -p /home/envoy/.local/share/dispatch \
    && chown -R envoy:envoy /home/envoy

COPY --from=build /out/envoy-listener /usr/local/bin/envoy-listener
COPY --from=build /out/envoy-dispatch /usr/local/bin/envoy-dispatch
COPY --from=build /out/envoy-broker /usr/local/bin/envoy-broker
COPY --from=build /out/agent-secrets /usr/local/bin/agent-secrets
COPY --from=build /out/THIRD_PARTY_NOTICES /usr/share/doc/legion/THIRD_PARTY_NOTICES
COPY --from=web --chown=envoy:envoy /repo/packages/dispatch/web/dist /srv/dispatch-web
# The Amazon RDS CA bundle, for a secrets broker that signs in to an RDS database by IAM token to
# name as BROKER_DATABASE_URL's sslrootcert. It sits outside every file and directory Go's
# crypto/x509 reads the system roots from (/etc/ssl/certs among them), so no binary here trusts an
# RDS CA for any other connection; the smoke test (internal/smoke/rdsbundle_test.go) checks both.
COPY packages/envoy/docker/rds-global-bundle.pem /etc/ssl/rds/global-bundle.pem

# The same commit envoy-dispatch reports on /healthz, as the image's own label. Its presence is how
# a deploy check knows the pinned image serves /healthz's commit and schema_version (the
# deployment repository's deploy verification). Declared this late so a new commit reuses every
# layer above.
ARG LEGION_COMMIT=
LABEL org.opencontainers.image.revision=${LEGION_COMMIT}

ENV DISPATCH_WEB_DIST=/srv/dispatch-web

USER envoy

EXPOSE 9020

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -sf http://localhost:9020/healthz || exit 1

ENTRYPOINT ["envoy-listener"]
