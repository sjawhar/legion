#!/usr/bin/env bash
# prepare-dist.sh — populate gen/proof-editor-dist/, the target of
# gen/package.json's "file:./proof-editor-dist" dependency on
# @sjawhar/proof-editor. Temporary until that package is published to npm;
# see the "Temporary: building @sjawhar/proof-editor from source" section of
# ../doc.go.
#
# Local dev: set PROOF_EDITOR_DIST to an already-built checkout (a directory
# containing package.json and dist/, e.g. from `npm run build:lib` in a
# proof-sdk clone).
#
# CI (PROOF_EDITOR_DIST unset): clones sjawhar/proof-sdk at a pinned revision
# into a scratch directory and builds it there.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dest="$here/proof-editor-dist"
PROOF_EDITOR_REF=54432f569275

if [[ -n "${PROOF_EDITOR_DIST:-}" ]]; then
  src="$PROOF_EDITOR_DIST"
else
  src="$(mktemp -d)"
  trap 'rm -rf "$src"' EXIT
  git clone https://github.com/sjawhar/proof-sdk.git "$src"
  (cd "$src" && git checkout --detach "$PROOF_EDITOR_REF" && npm install && npm run build:lib)
fi

if [[ ! -f "$src/package.json" || ! -d "$src/dist" ]]; then
  echo "prepare-dist.sh: $src does not look like a built @sjawhar/proof-editor checkout (missing package.json or dist/)" >&2
  exit 1
fi

rm -rf "$dest"
mkdir -p "$dest"
cp "$src/package.json" "$dest/"
cp -r "$src/dist" "$dest/"
[[ -f "$src/LICENSE" ]] && cp "$src/LICENSE" "$dest/"
[[ -f "$src/README.md" ]] && cp "$src/README.md" "$dest/"

echo "prepared $dest from ${PROOF_EDITOR_DIST:-proof-sdk@$PROOF_EDITOR_REF}"
