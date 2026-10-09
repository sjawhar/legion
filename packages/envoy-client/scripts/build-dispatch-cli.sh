#!/usr/bin/env bash
# Bundles the `dispatch` command (bin/dispatch.ts) into <out dir>/dispatch.js: the one statement of
# the CLI bundle's entry, target and format, which every host plugin that ships the command builds
# through (packages/pi-envoy's build and scripts/pi-plugin-prepack.sh, packages/envoy-plugin's build
# and prepack). claude-envoy commits its bundles and builds them with Bun.build in scripts/build.ts.
#
#   packages/envoy-client/scripts/build-dispatch-cli.sh <out dir> [<metafile>]
#
# <out dir> is relative to the caller's directory. With <metafile>, Bun writes the bundle's inputs
# there, for the caller's dist/THIRD_PARTY_NOTICES.
set -euo pipefail
[ $# -ge 1 ] && [ $# -le 2 ] || {
  echo "build-dispatch-cli: usage: build-dispatch-cli.sh <out dir> [<metafile>]" >&2
  exit 2
}
entry=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)/bin/dispatch.ts
bun build "$entry" --outfile "$1/dispatch.js" --target bun --format esm ${2:+"--metafile=$2"}
