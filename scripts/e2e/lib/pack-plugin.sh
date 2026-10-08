#!/usr/bin/env bash
# Packs one of this checkout's two Oh My Pi plugins, @sjawhar/pi-envoy (packages/pi-envoy) or
# @sjawhar/pi-legion (packages/pi-legion), exactly as the release packs it, into a tarball holding
# what `npm pack` ships (package.json `files`: dist/ with the one bundle and the prepack's
# dist/skills, the Legion plugin's agents/, the packed manifest). Every script that installs a
# branch-built plugin packs through here (lib/install-plugin-profile.sh and the grant rig's branch
# mode), so the plugins they run are the ones the release publishes. The worker image packs on its
# own, as release.yaml does, with the same jq rewrite and `bun pm pack`; the prepack
# (scripts/pi-plugin-prepack.sh) refuses any other omp.extensions.
#
#   scripts/e2e/lib/pack-plugin.sh <pi-envoy|pi-legion> <out dir>
#
# The first argument is the package's directory under packages/, and nothing else is taken: the
# other workspace packages are not plugins. Stdout is one line, the tarball's path; every step's own
# output goes to stderr. <out dir> is created if missing and must hold no tarball already.
#
# The sequence runs in the checkout this script lives in, because a copy of the package cannot
# build: the prepack copies the repository's skills/, and the bundle resolves @legion/* through the
# workspace root's node_modules (`bun install --frozen-lockfile` at the root first).
#   1. save package.json, arm the EXIT trap    release.yaml pi_envoy/pi_legion "Point extensions at the packed bundle"
#   2. omp.extensions -> the packed bundle     that step's jq; worker.Dockerfile's `jq '.omp.extensions = …'`
#   3. bun pm pack; its prepack builds dist/   release.yaml "Pack extension"; the prepack's `bun build`
#   4. put package.json back                   release.yaml "Restore committed manifest"
# Each source is cited by what it runs, never by line number: the lines move with every edit
# above them. release.yaml's "Set release version" is not a step here: the tarball carries the
# checkout's own version. The bundle is the committed manifest's one extension entry,
# extensions/<entry>.ts, as dist/<entry>.js: the list the prepack requires. The bundle inlines
# package.json, so it is built while the manifest names the packed bundle, as the release builds it.
# The saved manifest is written under this run's temp directory rather than beside package.json, so
# an interrupted run strands no tmp.json in the checkout.
set -euo pipefail

me=pack-plugin
refuse() {
  echo "$me: $*" >&2
  echo "usage: $0 <pi-envoy|pi-legion> <out dir>" >&2
  exit 2
}

[ $# -eq 2 ] || refuse "expected two arguments, the package to pack (pi-envoy or pi-legion) and the directory the tarball is written to"
package=$1
case "$package" in
pi-envoy | pi-legion) ;;
*) refuse "'$package' is not a plugin this checkout packs; the first argument is pi-envoy or pi-legion" ;;
esac
out=$(realpath -m -- "$2")
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)
case "$out/" in
"$root"/*) refuse "<out dir> $out is inside the checkout $root, where jj would snapshot the tarball" ;;
esac
mkdir -p "$out"
if compgen -G "$out/*.tgz" >/dev/null; then
  refuse "<out dir> $out already holds a tarball, so the one this run packs could not be told apart"
fi

manifest=$root/packages/$package/package.json
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
# A TERM once whoever reads stderr has gone would otherwise end this script with the manifest still
# rewritten: bash writes its Terminated notice for the interrupted pack to the dead stderr, and dies
# of SIGPIPE before the EXIT trap puts the manifest back. The grant rig calls this with no
# transcript tee to keep its stderr open.
trap '' PIPE

# Runs in one checkout take turns at the manifest: a run that started while another had it
# rewritten would save that rewrite and put it back. The lock is on the manifest itself, which is
# rewritten and restored in place (cp keeps the inode), and is held from the save to put_back.
exec 9<"$manifest"
if ! flock -n 9; then
  echo "$me: waiting for another run to finish packing in $root" >&2
  flock 9
fi
cp -p "$manifest" "$saved"
# The committed manifest names one source entry, extensions/<entry>.ts; the packed one names that
# entry's bundle alone, ["dist/<entry>.js"], the only list the prepack packs.
entry=$(jq -r 'if (.omp.extensions | length) == 1 then .omp.extensions[0] else empty end' "$saved")
case "$entry" in
extensions/*.ts) entry=${entry#extensions/} && entry=${entry%.ts} ;;
*) refuse "$manifest names omp.extensions $(jq -c '.omp.extensions' "$saved"); the committed list is one source entry, extensions/<entry>.ts" ;;
esac
restore=1
jq --arg bundle "dist/$entry.js" '.omp.extensions = [$bundle]' "$saved" >"$work/packed.json"
cp "$work/packed.json" "$manifest"

(cd "$root/packages/$package" && bun pm pack --destination "$out") >&2
put_back
exec 9<&-
tarballs=("$out"/*.tgz)
if [ "${#tarballs[@]}" != 1 ] || [ ! -f "${tarballs[0]}" ]; then
  echo "$me: bun pm pack left ${#tarballs[@]} entries matching $out/*.tgz; expected one tarball" >&2
  exit 1
fi
printf '%s\n' "${tarballs[0]}"
