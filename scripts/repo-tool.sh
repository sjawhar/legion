#!/bin/sh
# Runs one of this repository's own pinned Node tools: the `node_modules/.bin` entry, never
# whatever a name happens to reach. `bunx <bare name>` is not that command. With nothing
# installed it asks the npm registry, and `bunx biome` there is a squatted package (version
# 0.3.3, unrelated to @biomejs/biome) that checks nothing and exits 0 — a lint job that passes
# without reading a file. A bare `tsc` in a `bun run` recipe is the same defect through PATH: the
# worker image ships TypeScript 5.9.3 at /usr/local/bin/tsc, so the recipe silently typechecks
# with a compiler this repository does not pin. `bunx playwright` likewise fetches whatever the
# registry serves instead of the pinned @playwright/test.
#
# Resolving `node_modules/.bin/<tool>` explicitly is the only form that is both pinned and
# fail-closed: either it is the version the lockfile installed, or there is no binary and this
# script says so and exits non-zero. PATH is never consulted.
#
# Usage, from a package directory or the root: scripts/repo-tool.sh <tool> [args…]
# The caller's own `./node_modules/.bin` wins (a workspace member's install), then the root's
# (Bun hoists most binaries there). The tool is exec'd, so its exit status and signals are the
# command's own.
set -eu

root=$(cd -- "$(dirname -- "$0")/.." && pwd)

tool=${1-}
if [ -z "$tool" ]; then
  echo "repo-tool: no tool named; usage: scripts/repo-tool.sh <tool> [args...]" >&2
  exit 1
fi
case "$tool" in
  */*)
    echo "repo-tool: refusing tool name '$tool': it must be a node_modules/.bin entry, not a path" >&2
    exit 1
    ;;
esac
shift

local_bin="./node_modules/.bin/$tool"
root_bin="$root/node_modules/.bin/$tool"

if [ -x "$local_bin" ]; then
  exec "$local_bin" "$@"
fi
if [ -x "$root_bin" ]; then
  exec "$root_bin" "$@"
fi

echo "repo-tool: $tool is not installed" >&2
echo "  tried: $local_bin" >&2
echo "  tried: $root_bin" >&2
echo "  remedy: in a Legion role container, run 'sh scripts/legion-setup.sh' from the workspace" >&2
echo "  ($root): it installs these packages as Legion does, under the lock the roles share." >&2
echo "  Anywhere else, 'bun install' at the repository root." >&2
exit 1
