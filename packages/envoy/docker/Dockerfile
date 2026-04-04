FROM golang:1.24-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -o /out/envoy-listener ./cmd/listener && \
    go build -o /out/envoy-github ./cmd/github && \
    go build -o /out/envoy-slack ./cmd/slack

FROM debian:bookworm-slim

LABEL org.opencontainers.image.source=https://github.com/sjawhar/legion

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/* \
    && groupadd -r envoy && useradd -r -g envoy envoy

COPY --from=build /out/envoy-listener /usr/local/bin/envoy-listener
COPY --from=build /out/envoy-github /usr/local/bin/envoy-github
COPY --from=build /out/envoy-slack /usr/local/bin/envoy-slack

USER envoy
