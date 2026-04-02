#!/bin/bash
set -euo pipefail

dir="$(cd "$(dirname "$0")" && pwd)"
cd "$dir/.."
export ENVOY_GITHUB_WEBHOOK_SECRET="$($dir/read-secret.sh ENVOY_GITHUB_WEBHOOK_SECRET)"
docker compose -f compose/github.compose.yml up -d --build
