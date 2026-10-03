#!/usr/bin/env bash
# docs/site/media/broker/walkthrough.sh
#
# Records the broker walkthrough's raw footage: boots the rig (rig.sh), runs
# walkthrough.record.ts against it, which writes one cast or browser recording per section into
# docs/site/media/broker/walkthrough/raw/, then stops the rig. Takes rig.sh's input, DATABASE_URL.
# Needs asciinema and tmux (the terminal sections) and ffmpeg and ffprobe (each browser section's
# check of its own recording) beside rig.sh's tools, checked before the rig starts. The cut, the
# narration and the final video are that directory's build.py and narrate.py. Like screenshots.sh,
# it links packages/dispatch's node_modules here so the spec resolves @playwright/test.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

for tool in asciinema tmux ffmpeg ffprobe; do
  command -v "$tool" >/dev/null || { echo "walkthrough: $tool is required on PATH" >&2; exit 1; }
done

ln -sfn ../../../../packages/dispatch/node_modules "$SCRIPT_DIR/node_modules"
exec bash "$SCRIPT_DIR/rig.sh" -- \
  bash -c 'cd "$1/packages/dispatch" && bunx playwright test --config "$2/playwright.config.ts" walkthrough.record.ts' \
  walkthrough "$ROOT" "$SCRIPT_DIR"
