#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
# The SPA lives in packages/dispatch, outside this package's Docker build
# context. Build it here on a full checkout; a sync-host.sh target receives
# the prebuilt dist instead of the package sources.
if [ -f ../../dispatch/package.json ]; then
  (cd ../../dispatch && bun run build:web)
elif [ ! -f ../../dispatch/web/dist/index.html ]; then
  echo "dispatch SPA dist missing: build packages/dispatch (bun run build:web) on a full checkout or sync it with sync-host.sh" >&2
  exit 1
fi
docker compose -f compose/dispatch.compose.yml up -d --build
