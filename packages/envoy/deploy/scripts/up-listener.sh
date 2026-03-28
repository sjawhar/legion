#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
docker compose -f compose/listener.compose.yml up -d --build
