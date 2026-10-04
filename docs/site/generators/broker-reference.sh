#!/usr/bin/env bash
# docs/site/generators/broker-reference.sh <content dir>
#
# Writes the secrets broker's three reference pages generated from its Go source into
# <content dir>/broker/reference/: api.md (the HTTP API, from its route table and handlers),
# config.md (every environment variable it reads) and errors.md (every refusal code the broker, its
# host helper and the agent-secrets CLI answer). broker-refgen, on PATH from
# docs/site/scripts/build-binaries.sh, reads the source of this checkout and refuses an
# undocumented route, variable or code (packages/envoy/cmd/broker-refgen/main.go).
set -euo pipefail

content_dir="${1:?usage: broker-reference.sh <content dir>}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
broker-refgen "$repo_root" "$content_dir/broker/reference"
