#!/bin/bash
set -euo pipefail

host="${1:?usage: sync-host.sh user@host}"
root="$(cd "$(dirname "$0")/../../../.." && pwd)"
# The Dockerfile's build context is the repo root (it bakes the Dispatch SPA
# and generates Go contracts from the bun workspace), so a remote host cannot
# build the image from a partial sync. Remote hosts run a pinned
# ghcr.io/sjawhar/legion/envoy tag (ENVOY_IMAGE_TAG); sync only the
# compose files and scripts they need to pull and run it.
rsync -az --delete "$root/packages/envoy/deploy/" "$host:~/legion/default/packages/envoy/deploy/"
