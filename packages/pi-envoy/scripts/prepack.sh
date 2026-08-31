#!/bin/bash
# Tarballs ship only dist/: the release workflow rewrites omp.extensions to
# the bundle before packing and restores the committed manifest afterwards
# (see .github/workflows/release-pi-envoy.yaml). Packing with the committed
# source manifest would publish a package whose extension files are absent
# from the tarball, so fail fast instead.
set -euo pipefail
cd "$(dirname "$0")/.."
if ! jq -e '.omp.extensions == ["dist/envoy.js"]' package.json >/dev/null; then
  echo "pi-envoy: refusing to pack with omp.extensions=$(jq -c '.omp.extensions' package.json); rewrite it to [\"dist/envoy.js\"] first" >&2
  exit 1
fi
# The omp-plugins provider rebases a path-like command against the package
# root while the session cwd is preserved (project envoy.json + cwd-scoped gh
# identity stay correct). Tarballs must point it at the packed bundle, not
# sources (release workflow rewrites).
if ! jq -e '.mcpServers.dispatch.command == "./dist/bin/dispatch-mcp-shim.js"' .mcp.json >/dev/null; then
  echo "pi-envoy: refusing to pack with .mcp.json command=$(jq -c '.mcpServers.dispatch.command' .mcp.json); rewrite to the dist bundle first" >&2
  exit 1
fi
# Only the envoy extension ships: the legion extension is daemon
# infrastructure, loaded from repo checkouts.
bun build extensions/envoy.ts --outdir dist --target bun --format esm --external @oh-my-pi/pi-coding-agent
# Self-contained dispatch shim for the package-root .mcp.json mount.
bun build bin/dispatch-mcp-shim.ts --outdir dist/bin --target bun --format esm
chmod +x dist/bin/dispatch-mcp-shim.js
rm -f dist/legion.js
rm -rf dist/skills
cp -r ../../skills dist/skills
