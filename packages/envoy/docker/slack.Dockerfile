FROM golang:1.24-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) go build -o /out/envoy-slack ./cmd/slack

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/* \
    && groupadd -r envoy && useradd -r -g envoy envoy

COPY --from=build /out/envoy-slack /usr/local/bin/envoy-slack

USER envoy

EXPOSE 9011

ENTRYPOINT ["/usr/local/bin/envoy-slack"]
