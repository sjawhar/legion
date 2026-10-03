#!/usr/bin/env bash
# Builds the Go binaries the reference generators run, into the directory named as the only
# argument. scripts/generate.ts calls this before any generator and puts that directory first on
# each generator's PATH, so a generator runs `legion`, `envoy-dispatch`, `envoy-broker`,
# `agent-secrets` or `agent-secrets-helper` by name and documents the code at this commit.
#
# This file is the one place the docs build names a Go module path or command package: when a
# module moves, change it here and nowhere else.
#
# Usage: docs/site/scripts/build-binaries.sh <bin dir>
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <bin dir>" >&2
  exit 2
fi
bin=$(realpath -m "$1")
root=$(cd "$(dirname "$0")/../../.." && pwd)

# The Go coordinator.
legion_module=packages/daemon-go
# Dispatch, the secrets broker, its client and the client's host helper.
envoy_module=packages/envoy

mkdir -p "$bin"
go -C "$root/$legion_module" build -o "$bin/legion" ./cmd/legion
go -C "$root/$envoy_module" build -o "$bin/envoy-dispatch" ./cmd/dispatch
go -C "$root/$envoy_module" build -o "$bin/envoy-broker" ./cmd/broker
go -C "$root/$envoy_module" build -o "$bin/" ./cmd/agent-secrets ./cmd/agent-secrets-helper
echo "built legion, envoy-dispatch, envoy-broker, agent-secrets and agent-secrets-helper into $bin"
