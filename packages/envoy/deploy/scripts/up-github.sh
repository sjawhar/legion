#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
export ENVOY_GITHUB_WEBHOOK_SECRET="$($(dirname "$0")/read-secret.sh ENVOY_GITHUB_WEBHOOK_SECRET)"
docker compose -f compose/github.compose.yml up -d --build
