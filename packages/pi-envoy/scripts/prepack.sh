#!/bin/bash
# Tarballs ship dist/ and agents/ (package.json `files`). The release
# workflow rewrites omp.extensions to the packed bundles before packing and
# restores the committed manifest afterwards (see .github/workflows/release.yaml).
# Packing with the committed source manifest would publish a package whose
# extension files are absent from the tarball, so fail fast instead.
set -euo pipefail
cd "$(dirname "$0")/.."
if ! jq -e '.omp.extensions == ["dist/envoy.js","dist/legion.js"]' package.json >/dev/null; then
  echo "pi-envoy: refusing to pack with omp.extensions=$(jq -c '.omp.extensions' package.json); rewrite it to [\"dist/envoy.js\",\"dist/legion.js\"] first" >&2
  exit 1
fi
# Both extensions ship: envoy.ts loads on every OMP session, legion.ts is
# inert without LEGION_TREE/LEGION_ROLE/LEGION_CONTROLLER in the environment.
# dist/THIRD_PARTY_NOTICES carries the license of every package the bundles
# inline, read from the build's metafile; it fails the pack when one has none.
metafile="$(mktemp)"
trap 'rm -f "$metafile"' EXIT
bun build extensions/envoy.ts extensions/legion.ts --outdir dist --target bun --format esm --external @oh-my-pi/pi-coding-agent --external @oh-my-pi/pi-tui --external @oh-my-pi/pi-utils --metafile="$metafile"
bun ../../scripts/third-party-notices.ts "$metafile" dist/THIRD_PARTY_NOTICES
rm -rf dist/skills
cp -r ../../skills dist/skills
