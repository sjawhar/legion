#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
export ENVOY_SLACK_SIGNING_SECRET="$($(dirname "$0")/read-secret.sh ENVOY_SLACK_SIGNING_SECRET)"
docker compose -f compose/slack.compose.yml up -d --build
