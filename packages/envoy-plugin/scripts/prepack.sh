#!/bin/bash
# Tarballs ship dist/ (the bundled server and the `dispatch` CLI), src/, skills/ and bin/
# (package.json `files`). dist/dispatch.js is the CLI that bin/dispatch runs, a bundle of its
# own so the server's output path stays put. dist/THIRD_PARTY_NOTICES carries the license of
# every package the bundles inline, read from each build's own metafile (one `--metafile` per
# `bun build`: `bun run build --metafile=…` would hand it to the last command of the chain
# only); it fails the pack when one has none. The repository's skills/ is copied in, where
# OpenCode loads it as the plugin's skills; postpack removes the copy.
set -euo pipefail
cd "$(dirname "$0")/.."
metafiles="$(mktemp -d)"
trap 'rm -rf "$metafiles"' EXIT
bun build src/server.ts --root . --outdir dist --target bun --format esm --external '@opencode-ai/*' --metafile="$metafiles/server.json"
bun build ../envoy-client/bin/dispatch.ts --outfile dist/dispatch.js --target bun --format esm --metafile="$metafiles/dispatch.json"
bun ../../scripts/third-party-notices.ts "$metafiles/server.json" "$metafiles/dispatch.json" dist/THIRD_PARTY_NOTICES
rm -rf skills
cp -r ../../skills skills
