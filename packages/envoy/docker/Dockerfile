# Build context is the repo root (bun workspace + Go module in packages/envoy).

# Dispatch SPA build + Go contract codegen. Both come from the bun workspace,
# so they share one stage keyed off the root lockfile.
FROM oven/bun:1 AS web

WORKDIR /repo

COPY package.json bun.lock ./
COPY packages/contracts/package.json packages/contracts/package.json
COPY packages/envoy-client/package.json packages/envoy-client/package.json
COPY packages/pi-envoy/package.json packages/pi-envoy/package.json
COPY packages/daemon/package.json packages/daemon/package.json
COPY packages/envoy-plugin/package.json packages/envoy-plugin/package.json
COPY packages/envoy-claude-plugin/package.json packages/envoy-claude-plugin/package.json
COPY packages/dispatch/package.json packages/dispatch/package.json
COPY packages/workspace/package.json packages/workspace/package.json
RUN bun install --frozen-lockfile

COPY packages/contracts packages/contracts
COPY packages/dispatch packages/dispatch
RUN cd packages/contracts && bun run gen:go
RUN cd packages/dispatch && bun run build:web

# Go version must match go.mod.
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY packages/envoy/go.mod packages/envoy/go.sum ./
RUN go mod download

COPY packages/envoy/cmd ./cmd
COPY packages/envoy/internal ./internal
# The Go contracts file is generated from packages/contracts/schemas; never
# trust the copy checked into the tree over a fresh regeneration.
COPY --from=web /repo/packages/envoy/internal/contracts/generated.go ./internal/contracts/generated.go

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -o /out/envoy-listener ./cmd/listener \
    && CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -o /out/envoy-dispatch ./cmd/dispatch

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
COPY --from=web --chown=envoy:envoy /repo/packages/dispatch/web/dist /srv/dispatch-web

ENV DISPATCH_WEB_DIST=/srv/dispatch-web

USER envoy

EXPOSE 9020

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -sf http://localhost:9020/healthz || exit 1

ENTRYPOINT ["envoy-listener"]
