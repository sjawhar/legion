#!/bin/bash
# Tarballs ship dist/ (the bundled server), src/ and skills/ (package.json `files`).
# dist/THIRD_PARTY_NOTICES carries the license of every package the bundle
# inlines, read from the build's metafile; it fails the pack when one has none.
# The repository's skills/ is copied in, where OpenCode loads it as the plugin's
# skills; postpack removes the copy.
set -euo pipefail
cd "$(dirname "$0")/.."
metafile="$(mktemp)"
trap 'rm -f "$metafile"' EXIT
bun run build --metafile="$metafile"
bun ../../scripts/third-party-notices.ts "$metafile" dist/THIRD_PARTY_NOTICES
rm -rf skills
cp -r ../../skills skills
