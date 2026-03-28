#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
docker compose -f compose/nats/client.compose.yml up -d
