#!/usr/bin/env bash
# Installs this checkout's @sjawhar/pi-legion-envoy into a named OMP profile, packed exactly as the
# release packs it, so a live proof or a boot-gate test runs the branch-built plugin while the
# user's default profile stays untouched.
#
#   scripts/e2e/lib/install-plugin-profile.sh --profile <name> --dest <dir>
#
# Stdout is one line, the installed manifest's path as `omp plugin list --json` reports the plugin
# (so a caller can write `manifest=$(…)`); every step's own output goes to stderr.
#
# The sequence runs in the checkout this script lives in, because a copy of packages/pi-envoy cannot
# build: prepack.sh copies ../../skills, and the bundle resolves @legion/* through the workspace
# root's node_modules (`bun install --frozen-lockfile` at the root first).
#   1. save package.json, arm the EXIT trap    release.yaml:345
#   2. omp.extensions -> the packed bundles    release.yaml:346-348; worker.Dockerfile:66-67
#   3. bun pm pack; its prepack builds dist/   release.yaml:350-353; prepack.sh:9-17
#   4. put package.json back                   release.yaml:365-370
#   5. unpack the tarball into <dir>           worker.Dockerfile:62-64, :69-70
#   6. OMP_PROFILE=<name> omp plugin install   worker.Dockerfile:184
#   7. verify with OMP_PROFILE=<name> omp plugin list
# release.yaml:328-333 (set the release version) is not a step here: the profile gets the
# checkout's own version. The packed manifest and the tarball are written under this run's temp
# directory rather than beside package.json, so an interrupted run strands neither a tmp.json nor
# a .tgz in the checkout (the release's `rm -f ./*.tgz` guards the same stale-glob case).
set -euo pipefail

me=install-plugin-profile
refuse() {
  echo "$me: $*" >&2
  echo "usage: $0 --profile <name> --dest <dir>" >&2
  exit 2
}

profile=
dest=
have_profile=
have_dest=
while [ $# -gt 0 ]; do
  case "$1" in
  --profile)
    [ $# -ge 2 ] || refuse "--profile needs a value: the OMP profile to install into"
    profile=$2 have_profile=1
    shift 2
    ;;
  --dest)
    [ $# -ge 2 ] || refuse "--dest needs a value: the directory the plugin is unpacked into"
    dest=$2 have_dest=1
    shift 2
    ;;
  *) refuse "unknown argument: $1" ;;
  esac
done
[ -n "$have_profile" ] || refuse "--profile is required: the OMP profile to install into"
[ -n "$have_dest" ] || refuse "--dest is required: the directory the plugin is unpacked into"

# OMP trims the profile name and reads an empty one or "default" as its default profile
# (normalizeProfileName, @oh-my-pi/pi-utils dirs.ts), the one every plain `omp` uses. Any other
# name goes to OMP as given, and OMP refuses one it cannot use.
trimmed=${profile#"${profile%%[![:space:]]*}"}
trimmed=${trimmed%"${trimmed##*[![:space:]]}"}
if [ -z "$trimmed" ] || [ "$trimmed" = default ]; then
  refuse "--profile '$profile' is OMP's default profile, the one plain \`omp\` uses; name a dedicated profile"
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)
dest=$(realpath -m -- "$dest")
case "$dest/" in
"$root"/*) refuse "--dest $dest is inside the checkout $root, where jj would snapshot the unpacked plugin" ;;
esac
# `omp plugin install` links <dir> rather than copying it, so <dir> is the installed plugin for as
# long as the profile uses it. Unpacking over an earlier build would leave its stale files behind.
if [ -e "$dest" ] && { [ ! -d "$dest" ] || [ -n "$(ls -A -- "$dest")" ]; }; then
  refuse "--dest $dest exists and is not an empty directory"
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
version=$(jq -r .version "$saved")
jq '.omp.extensions = ["dist/envoy.js","dist/legion.js"]' "$saved" >"$work/packed.json"
cp "$work/packed.json" "$manifest"

mkdir "$work/pack"
(cd "$root/packages/pi-envoy" && bun pm pack --destination "$work/pack") >&2
put_back
exec 9<&-
tarballs=("$work"/pack/*.tgz)
if [ "${#tarballs[@]}" != 1 ] || [ ! -f "${tarballs[0]}" ]; then
  echo "$me: bun pm pack left ${#tarballs[@]} entries matching $work/pack/*.tgz; expected one tarball" >&2
  exit 1
fi

mkdir -p "$dest"
tar xzf "${tarballs[0]}" -C "$dest" --strip-components=1
OMP_PROFILE=$profile omp plugin install "$dest" >&2

listing=$(OMP_PROFILE=$profile omp plugin list --json)
plugin=$(jq -c '[.npm[]? | select(.name == "@sjawhar/pi-legion-envoy")]' <<<"$listing")
if ! jq -e --arg v "$version" 'length == 1 and .[0].version == $v and .[0].enabled == true' \
  <<<"$plugin" >/dev/null; then
  echo "$me: OMP_PROFILE=$profile omp plugin list does not show @sjawhar/pi-legion-envoy@$version enabled: $plugin" >&2
  exit 1
fi
installed=$(jq -r '.[0].path' <<<"$plugin")
resolved=$(realpath -m -- "$installed")
if [ "$resolved" != "$dest" ]; then
  echo "$me: profile $profile resolves @sjawhar/pi-legion-envoy to $resolved, not the unpacked $dest" >&2
  exit 1
fi
echo "$me: @sjawhar/pi-legion-envoy@$version enabled in OMP profile $profile ($installed -> $dest)" >&2
printf '%s\n' "$installed/package.json"
