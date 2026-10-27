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
FROM mirror.gcr.io/library/golang:1.27.1-alpine AS wrapper

WORKDIR /src/packages/envoy/infra

# The module builds in the workspace of packages/envoy/infra/go.work, as the envoy-infra-go job
# builds it, which settles the two modules' requirements.
COPY packages/envoy/go.mod packages/envoy/go.sum /src/packages/envoy/
COPY packages/envoy/listenfd /src/packages/envoy/listenfd
COPY packages/envoy/infra/go.mod packages/envoy/infra/go.sum packages/envoy/infra/go.work packages/envoy/infra/go.work.sum ./
COPY packages/envoy/infra/host/heldportwrapper ./host/heldportwrapper
RUN CGO_ENABLED=0 go build -o /out/envoy-listener-heldport ./host/heldportwrapper

FROM ${BASE}

COPY --from=base /usr/local/bin/envoy-listener /usr/local/libexec/envoy-listener
COPY --from=wrapper --chmod=0755 /out/envoy-listener-heldport /usr/local/bin/envoy-listener
