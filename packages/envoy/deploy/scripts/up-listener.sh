#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
# ENVOY_IMAGE_TAG is required by the compose file; a local build doesn't care
# about its value, so default it here.
export ENVOY_IMAGE_TAG="${ENVOY_IMAGE_TAG:-local}"
docker compose -f compose/listener.compose.yml up -d --build
