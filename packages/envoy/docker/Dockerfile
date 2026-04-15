# Go version must match go.mod (tsnet requires >= 1.26.1).
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -o /out/envoy-listener ./cmd/listener

# Minimal runtime — static binary, no shell needed.
# curl is included for HEALTHCHECK and operational debugging.
FROM debian:bookworm-slim

LABEL org.opencontainers.image.source=https://github.com/sjawhar/legion

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/* \
    && groupadd -r envoy && useradd -r -g envoy envoy

COPY --from=build /out/envoy-listener /usr/local/bin/envoy-listener

# tsnet uses userspace networking (gVisor netstack) by default — no
# CAP_NET_ADMIN or /dev/net/tun required. Only needs outbound UDP
# (WireGuard) and outbound TCP 443 (control plane).

USER envoy

EXPOSE 9020

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -sf http://localhost:9020/healthz || exit 1

ENTRYPOINT ["envoy-listener"]
