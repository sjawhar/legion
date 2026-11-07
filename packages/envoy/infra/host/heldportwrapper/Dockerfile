# The image the envoy-infra live tests run (host/live_test.go, pulumitest.HeldPortImage): the smoke
# image BASE names, with this directory's wrapper as its envoy-listener and the real listener moved
# to /usr/local/libexec/envoy-listener, where the wrapper executes it on the socket the test holds.
# The user, entrypoint, healthcheck and curl stay the smoke image's. Build context is the repository
# root, read through Dockerfile.dockerignore beside this file:
#
#   docker buildx build --load --build-arg BASE=legion-envoy-smoke:local \
#     -t legion-envoy-smoke-heldport:local -f packages/envoy/infra/host/heldportwrapper/Dockerfile .
ARG BASE

FROM ${BASE} AS base

# Go version must match packages/envoy/infra/go.mod. The smoke image builds on the same image, so
# the envoy-smoke job has pulled it already.
# ghcr.io/cybermonklabs/legion/ci, the copy of golang:1.27.3-alpine (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS wrapper

WORKDIR /src/packages/envoy/infra

# A `|` between proxy entries makes a go command fall through to the next source after any error,
# where a `,` falls through only after a 404 or a 410 (Go's module reference, "GOPROXY protocol").
# go.sum holds each module's hash either way, so this is a second path, not a weaker check
# (sjawhar/legion#2512).
ENV GOPROXY=https://proxy.golang.org|direct

# The module builds in the workspace of packages/envoy/infra/go.work, as the envoy-infra-go job
# builds it, which settles the two modules' requirements.
COPY packages/envoy/go.mod packages/envoy/go.sum /src/packages/envoy/
COPY packages/envoy/listenfd /src/packages/envoy/listenfd
COPY packages/envoy/infra/go.mod packages/envoy/infra/go.sum packages/envoy/infra/go.work packages/envoy/infra/go.work.sum ./
COPY packages/envoy/infra/host/heldportwrapper ./host/heldportwrapper
# The build's modules come down first, under the retry: five attempts, the fifth starting 150 s
# after the first, each attempt bounded at 600 s, which is what the two modules' zips cost from the
# proxy. The `go build` that follows reads the cache the download filled and fetches nothing, so it
# stays bare. Both stay in this one layer: the wrapper's sources are already copied in above, so a
# layer of its own would be rebuilt by every source change anyway.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 600 "go mod download" go mod download \
    && CGO_ENABLED=0 go build -o /out/envoy-listener-heldport ./host/heldportwrapper

FROM ${BASE}

COPY --from=base /usr/local/bin/envoy-listener /usr/local/libexec/envoy-listener
COPY --from=wrapper --chmod=0755 /out/envoy-listener-heldport /usr/local/bin/envoy-listener
