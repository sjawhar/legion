#!/bin/bash
set -euo pipefail

dir="$(cd "$(dirname "$0")" && pwd)"
cd "$dir/.."
export ENVOY_SLACK_SIGNING_SECRET="$($dir/read-secret.sh ENVOY_SLACK_SIGNING_SECRET)"
docker compose -f compose/slack.compose.yml up -d --build
