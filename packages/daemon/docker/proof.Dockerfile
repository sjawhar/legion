# syntax=docker/dockerfile:1.7
# Legion proof image: the pod a stage proof runs from (scripts/e2e/README.md, "From a pod"). Build
# context: the repo root, read through proof.Dockerfile.dockerignore beside this file, which keeps the
# whole checkout. Built by .github/workflows/proof-image.yaml on the GitHub-hosted runner for every
# head of a pull request against main and every main commit, and published as
# ghcr.io/sjawhar/legion-proof:sha-<12>. Never build it on a workstation, as worker.Dockerfile says of
# itself.
#
# Contents: the commit's checkout at /src/legion, its .git included, owned by the runtime user (uid
# 1000), so a stage proof builds and stamps its binaries from it as it does from CI's checkout
# (scripts/e2e/lib/built-from.sh reads HEAD and a clean tree; Go stamps vcs.revision); the Go modules
# both of go.work's modules need, downloaded into the runtime user's module cache; Go at go.work's
# version; kubectl; the AWS CLI v2; gh; Bun; jq; psql; git and curl; openssl; ss (iproute2); flock
# and setpriv (util-linux); ps, pgrep and pkill (procps). The last RUN gates the publish: the checkout
# is the commit the workflow built and is clean, and every tool runs.

# Pins. GO_VERSION is go.work's `go` line: the golang image's GOTOOLCHAIN=local fails the build if
# go.work moves past it. BUN_VERSION is .bun-version's (.github/scripts/check-bun-version.sh), and
# GH_VERSION and AWS_CLI_VERSION are worker.Dockerfile's, so a proof drives what a worker runs. Each
# download is checked against its SHA-256 before anything unpacks it: kubectl's is the one dl.k8s.io
# publishes beside the binary, gh's the line for this archive in the release's checksums file, and
# the AWS CLI's is worker.Dockerfile's, taken as that file says.
ARG BUN_VERSION=1.3.14
ARG GO_VERSION=1.26.8
# Within one minor of the clusters the proofs drive (EKS 1.35).
ARG KUBECTL_VERSION=v1.35.9
ARG KUBECTL_SHA256=3cfeaf80be482b435b0aa214aff6e0b2c312ee23c0ff20810c75517b6004c6eb
ARG GH_VERSION=2.98.0
ARG GH_SHA256=3b8ac6b30336802fc1a858d7c084e11cdf24ac1a761ca90b68022d7d729208de
ARG AWS_CLI_VERSION=2.37.6
ARG AWS_CLI_SHA256=cd40c7d1f41b3a4964e77a65377e480d71fe6ebc96bbbb64eb2239d69af6fbb2

# ------------------------------------------------------------------------------------------------
# bun: the pinned Bun binary, copied below as worker.Dockerfile copies it.
FROM oven/bun:${BUN_VERSION}-slim AS bun

# ------------------------------------------------------------------------------------------------
# tools: kubectl, gh and the AWS CLI, each checked against its pin. /out/bin holds the binaries and
# the AWS installer's two links into /opt/aws-cli.
FROM debian:trixie-slim AS tools
ARG KUBECTL_VERSION
ARG KUBECTL_SHA256
ARG GH_VERSION
ARG GH_SHA256
ARG AWS_CLI_VERSION
ARG AWS_CLI_SHA256
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    t=/tmp/tools; mkdir -p "$t" /out/bin; \
    curl -fsSLo "$t/kubectl" "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"; \
    curl -fsSLo "$t/gh.tar.gz" \
      "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz"; \
    curl -fsSLo "$t/awscli.zip" "https://awscli.amazonaws.com/awscli-exe-linux-x86_64-${AWS_CLI_VERSION}.zip"; \
    printf '%s  %s\n' "$KUBECTL_SHA256" "$t/kubectl" "$GH_SHA256" "$t/gh.tar.gz" \
      "$AWS_CLI_SHA256" "$t/awscli.zip" > "$t/SHA256SUMS"; \
    sha256sum --check --strict "$t/SHA256SUMS"; \
    install -m 0755 "$t/kubectl" /out/bin/kubectl; \
    tar -xzf "$t/gh.tar.gz" -C "$t" --no-same-owner; \
    install -m 0755 "$t/gh_${GH_VERSION}_linux_amd64/bin/gh" /out/bin/gh; \
    unzip -q "$t/awscli.zip" -d "$t"; \
    "$t/aws/install" --install-dir /opt/aws-cli --bin-dir /out/bin; \
    rm -rf "$t"

# ------------------------------------------------------------------------------------------------
# runtime: the golang image, which brings Go, git and curl on Debian trixie.
FROM golang:${GO_VERSION}-trixie
LABEL org.opencontainers.image.source=https://github.com/sjawhar/legion
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends \
      jq postgresql-client iproute2 util-linux procps openssl diffutils \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 legion \
    && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash legion \
    && install -d -m 0755 -o 1000 -g 1000 /src/legion
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
RUN ln -s bun /usr/local/bin/bunx
COPY --from=tools /opt/aws-cli /opt/aws-cli
COPY --from=tools /out/bin/ /usr/local/bin/
# GOPATH under HOME: the module cache below and every build cache belong to the runtime user.
ENV HOME=/home/legion \
    GOPATH=/home/legion/go \
    PATH=/home/legion/go/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
# Numeric uid:gid, the checkout's owner: git refuses a repository another user owns, and Go's vcs
# stamp runs git there. A pod runs the image as this user.
USER 1000:1000
WORKDIR /src/legion
# The modules first, from the module files alone, so a commit that changes no go.mod or go.sum reuses
# this layer and a run downloads nothing.
COPY --chown=1000:1000 go.work go.work.sum ./
COPY --chown=1000:1000 packages/daemon/go.mod packages/daemon/go.sum packages/daemon/
COPY --chown=1000:1000 packages/envoy/go.mod packages/envoy/go.sum packages/envoy/
RUN go mod download
COPY --chown=1000:1000 . .
# The publish gate: the checkout is the commit the workflow built (LEGION_REVISION, refused when
# empty), and git reports it clean, so a stage proof's built-from line names that commit alone;
# every tool a proof calls is on PATH and runs.
ARG LEGION_REVISION
RUN set -eu; \
    test -n "$LEGION_REVISION"; \
    head="$(git rev-parse HEAD)"; echo "checkout: $head"; test "$head" = "$LEGION_REVISION"; \
    dirty="$(git status --porcelain)"; \
    if [ -n "$dirty" ]; then printf 'the checkout is not clean:\n%s\n' "$dirty"; exit 1; fi; \
    for tool in go git curl kubectl aws gh bun bunx jq psql openssl ss flock setpriv ps pgrep pkill diff timeout; do \
      command -v "$tool"; \
    done; \
    go version; kubectl version --client; aws --version; gh --version; bun --version; jq --version; \
    psql --version; openssl version; ss -V; \
    bash scripts/e2e/lib/built-from.sh /src/legion
