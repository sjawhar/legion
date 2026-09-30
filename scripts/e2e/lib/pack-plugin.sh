#!/usr/bin/env bash
# Packs this checkout's @sjawhar/pi-legion-envoy exactly as the release packs it, into a tarball
# holding what `npm pack` ships (package.json `files`: dist/ with the bundles and prepack.sh's
# dist/skills, agents/, the packed manifest). Every caller that installs a branch-built plugin
# packs through here, so the plugin they run is the one the release publishes.
#
#   scripts/e2e/lib/pack-plugin.sh <out dir>
#
# Stdout is one line, the tarball's path; every step's own output goes to stderr. <out dir> is
# created if missing and must hold no tarball already.
#
# The sequence runs in the checkout this script lives in, because a copy of packages/pi-envoy cannot
# build: prepack.sh copies ../../skills, and the bundle resolves @legion/* through the workspace
# root's node_modules (`bun install --frozen-lockfile` at the root first).
#   1. save package.json, arm the EXIT trap    release.yaml pi_envoy "Point extensions at the packed bundles"
#   2. omp.extensions -> the packed bundles    that step's jq; worker.Dockerfile's `jq '.omp.extensions = …'`
#   3. bun pm pack; its prepack builds dist/   release.yaml "Pack extension"; prepack.sh's `bun build`
#   4. put package.json back                   release.yaml "Restore committed manifest"
# Each source is cited by what it runs, never by line number: the lines move with every edit
# above them. release.yaml's "Set release version" is not a step here: the tarball carries the
# checkout's own version. The bundles inline package.json, so they are built while it names the
# packed bundles, as the release builds them. The saved manifest is written under this run's temp
# directory rather than beside package.json, so an interrupted run strands no tmp.json in the
# checkout.
set -euo pipefail

me=pack-plugin
refuse() {
  echo "$me: $*" >&2
  echo "usage: $0 <out dir>" >&2
  exit 2
}

[ $# -eq 1 ] || refuse "expected one argument, the directory the tarball is written to"
out=$(realpath -m -- "$1")
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)
case "$out/" in
"$root"/*) refuse "<out dir> $out is inside the checkout $root, where jj would snapshot the tarball" ;;
esac
mkdir -p "$out"
if compgen -G "$out/*.tgz" >/dev/null; then
  refuse "<out dir> $out already holds a tarball, so the one this run packs could not be told apart"
fi

manifest=$root/packages/pi-envoy/package.json
work=$(mktemp -d "${TMPDIR:-/tmp}/$me.XXXXXXXX")
saved=$work/package.json
restore=
# put_back copies the saved bytes over the manifest and checks them. It runs right after the pack,
# and the EXIT trap runs it on every other way out — a pack that dies halfway, an interrupt — so no
# jj snapshot taken after the run sees the rewrite. The trap keeps the run's status unless put_back
# itself failed, and then the saved bytes stay where they are and the status is non-zero.
put_back() {
  cp -p "$saved" "$manifest" && cmp -s "$saved" "$manifest" && restore=
}
cleanup() {
  local st=$?
  if [ -n "$restore" ] && ! put_back; then
    echo "$me: could not restore $manifest; the bytes it had before this run are in $saved — copy them back before jj snapshots the rewrite" >&2
    [ "$st" != 0 ] || st=1
  else
    rm -rf "$work" || true
  fi
  exit "$st"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Runs in one checkout take turns at the manifest: a run that started while another had it
# rewritten would save that rewrite and put it back. The lock is on the manifest itself, which is
# rewritten and restored in place (cp keeps the inode), and is held from the save to put_back.
exec 9<"$manifest"
if ! flock -n 9; then
  echo "$me: waiting for another run to finish packing in $root" >&2
  flock 9
fi
cp -p "$manifest" "$saved"
restore=1
jq '.omp.extensions = ["dist/envoy.js","dist/legion.js"]' "$saved" >"$work/packed.json"
cp "$work/packed.json" "$manifest"

(cd "$root/packages/pi-envoy" && bun pm pack --destination "$out") >&2
put_back
exec 9<&-
tarballs=("$out"/*.tgz)
if [ "${#tarballs[@]}" != 1 ] || [ ! -f "${tarballs[0]}" ]; then
  echo "$me: bun pm pack left ${#tarballs[@]} entries matching $out/*.tgz; expected one tarball" >&2
  exit 1
fi
printf '%s\n' "${tarballs[0]}"
