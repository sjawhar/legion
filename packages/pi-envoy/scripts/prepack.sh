#!/bin/bash
# Tarballs ship only dist/: the release workflow rewrites omp.extensions to
# the packed bundles before packing and restores the committed manifest
# afterwards (see .github/workflows/release.yaml). Packing with the committed
# source manifest would publish a package whose extension files are absent
# from the tarball, so fail fast instead.
set -euo pipefail
cd "$(dirname "$0")/.."
if ! jq -e '.omp.extensions == ["dist/envoy.js","dist/legion.js"]' package.json >/dev/null; then
  echo "pi-envoy: refusing to pack with omp.extensions=$(jq -c '.omp.extensions' package.json); rewrite it to [\"dist/envoy.js\",\"dist/legion.js\"] first" >&2
  exit 1
fi
# The Envoy and Legion extensions load in ordinary sessions; the packaged prompt-dependency
# extension runs only when `legion probe-image` names it explicitly.
bun build extensions/envoy.ts extensions/legion.ts extensions/prompt-dependencies-probe.ts --outdir dist --target bun --format esm --external @oh-my-pi/pi-coding-agent --external @oh-my-pi/pi-tui --external @oh-my-pi/pi-utils
rm -rf dist/skills
cp -r ../../skills dist/skills
