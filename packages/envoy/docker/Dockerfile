# Build context is the repo root (bun workspace + Go module in packages/envoy).

# Dispatch SPA build + Go contract codegen. Both come from the bun workspace,
# so they share one stage keyed off the root lockfile.
# Bun is pinned to the version the repo locks, like packages/daemon/docker/worker.Dockerfile:
# bundler output differs across Bun releases, so a published SPA must not be built by whatever
# 1.x is newest that day. .github/scripts/check-bun-version.sh keeps this equal to .bun-version.
ARG BUN_VERSION=1.3.14
FROM oven/bun:${BUN_VERSION} AS web

WORKDIR /repo

RUN apt-get update && apt-get install -y --no-install-recommends python3 make g++ \
    && rm -rf /var/lib/apt/lists/*

COPY package.json bun.lock ./
COPY patches patches
COPY packages/contracts/package.json packages/contracts/package.json
COPY packages/envoy-client/package.json packages/envoy-client/package.json
COPY packages/pi-envoy/package.json packages/pi-envoy/package.json
COPY packages/envoy-plugin/package.json packages/envoy-plugin/package.json
COPY packages/claude-envoy/package.json packages/claude-envoy/package.json
COPY packages/dispatch/package.json packages/dispatch/package.json
COPY packages/proof-editor/package.json packages/proof-editor/package.json
COPY packages/envoy/internal/dispatch/pmdoc/gen/package.json packages/envoy/internal/dispatch/pmdoc/gen/package.json
COPY docs/site/package.json docs/site/package.json
RUN bun install --frozen-lockfile

COPY packages/contracts packages/contracts
COPY packages/dispatch packages/dispatch
COPY packages/proof-editor packages/proof-editor
# The SPA build writes dist/THIRD_PARTY_NOTICES.txt with this (packages/dispatch/web/vite.config.ts).
COPY scripts/third-party-notices.ts scripts/third-party-notices.ts
RUN cd packages/contracts && bun run gen:go
RUN cd packages/dispatch && bun run build:web

# Go version must match go.mod.
FROM golang:1.26.8-alpine AS build

WORKDIR /src

# The image's third-party notices: go-licenses (pinned in go-third-party-notices.sh) is built in a
# layer of its own, before the module files, so neither a source change nor a dependency bump
# rebuilds it.
COPY scripts/go-third-party-notices.sh scripts/assemble-third-party-notices.sh /usr/local/bin/
RUN go-third-party-notices.sh --install
COPY packages/envoy/go.mod packages/envoy/go.sum ./
RUN go mod download

COPY packages/envoy/cmd ./cmd
COPY packages/envoy/internal ./internal
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
FROM debian:bookworm-slim

LABEL org.opencontainers.image.source=https://github.com/sjawhar/legion

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/* \
    && groupadd -r envoy && useradd -r -g envoy envoy \
    && mkdir -p /home/envoy/.local/share/dispatch \
    && chown -R envoy:envoy /home/envoy

COPY --from=build /out/envoy-listener /usr/local/bin/envoy-listener
COPY --from=build /out/envoy-dispatch /usr/local/bin/envoy-dispatch
COPY --from=build /out/envoy-broker /usr/local/bin/envoy-broker
COPY --from=build /out/agent-secrets /usr/local/bin/agent-secrets
COPY --from=build /out/THIRD_PARTY_NOTICES /usr/share/doc/legion/THIRD_PARTY_NOTICES
COPY --from=web --chown=envoy:envoy /repo/packages/dispatch/web/dist /srv/dispatch-web

# The same commit envoy-dispatch reports on /healthz, as the image's own label. Its presence is how
# a deploy check knows the pinned image serves /healthz's commit and schema_version (agent-c's
# scripts/verify_dispatch_deploy.py). Declared this late so a new commit reuses every layer above.
ARG LEGION_COMMIT=
LABEL org.opencontainers.image.revision=${LEGION_COMMIT}

ENV DISPATCH_WEB_DIST=/srv/dispatch-web

USER envoy

EXPOSE 9020

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -sf http://localhost:9020/healthz || exit 1

ENTRYPOINT ["envoy-listener"]
