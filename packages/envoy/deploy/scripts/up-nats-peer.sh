#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
./scripts/render-nats-peer.sh compose/nats
docker compose -f compose/nats/peer.compose.yml up -d
