#!/usr/bin/env bash
# docs/site/media/broker/screenshots.sh
#
# Retakes the broker's screenshots into docs/site/public/media/broker/: boots the rig (rig.sh) and
# runs screenshots.spec.ts against it, then stops the rig. Takes rig.sh's inputs (DATABASE_URL,
# the harness ports). The spec runs under packages/dispatch's Playwright, the one installed copy:
# a node_modules link in this directory (gitignored) is how its files resolve @playwright/test.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

ln -sfn ../../../../packages/dispatch/node_modules "$SCRIPT_DIR/node_modules"
exec bash "$SCRIPT_DIR/rig.sh" -- \
  bash -c 'cd "$1/packages/dispatch" && bunx playwright test --config "$2/playwright.config.ts" screenshots.spec.ts' \
  screenshots "$ROOT" "$SCRIPT_DIR"
