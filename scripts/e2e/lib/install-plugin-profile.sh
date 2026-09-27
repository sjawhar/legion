#!/usr/bin/env bash
# Installs this checkout's @sjawhar/pi-legion-envoy into a named OMP profile, packed exactly as the
# release packs it, so a live proof or a boot-gate test runs the branch-built plugin while the
# user's own profiles stay untouched.
#
#   scripts/e2e/lib/install-plugin-profile.sh --profile <name> --home <dir> --dest <dir>
#
# --home is the HOME Oh My Pi runs under for the profile, made by make_omp_home (lib/omp-home.sh): the
# profile is <home>/.omp/profiles/<name>, and the caller's own HOME is refused.
#
# Stdout is one line, the installed manifest's path as `omp plugin list --json` reports the plugin
# (so a caller can write `manifest=$(…)`); every step's own output goes to stderr.
#
# The sequence runs in the checkout this script lives in, because a copy of packages/pi-envoy cannot
# build: prepack.sh copies ../../skills, and the bundle resolves @legion/* through the workspace
# root's node_modules (`bun install --frozen-lockfile` at the root first).
#   1. save package.json, arm the EXIT trap    release.yaml pi_envoy "Point extensions at the packed bundles"
#   2. omp.extensions -> the packed bundles    that step's jq; worker.Dockerfile's `jq '.omp.extensions = …'`
#   3. bun pm pack; its prepack builds dist/   release.yaml "Pack extension"; prepack.sh's `bun build`
#   4. put package.json back                   release.yaml "Restore committed manifest"
#   5. unpack the tarball into <dir>           worker.Dockerfile's `tar xzf ./*.tgz -C /out/pi-legion-envoy`
#   6. OMP_PROFILE=<name> omp plugin install   worker.Dockerfile's `omp plugin install /opt/legion/pi-legion-envoy`
#   7. verify with OMP_PROFILE=<name> omp plugin list
# Steps 6 and 7 run the Oh My Pi the daemons pin (omp-pin.ts) under HOME=<home>.
# Each source is cited by what it runs, never by line number: the lines move with every edit
# above them. release.yaml's "Set release version" is not a step here: the profile gets the
# checkout's own version. The packed manifest and the tarball are written under this run's temp
# directory rather than beside package.json, so an interrupted run strands neither a tmp.json nor
# a .tgz in the checkout (the release's `rm -f ./*.tgz` guards the same stale-glob case).
set -euo pipefail

me=install-plugin-profile
refuse() {
  echo "$me: $*" >&2
  echo "usage: $0 --profile <name> --home <dir> --dest <dir>" >&2
  exit 2
}

profile=
home=
dest=
have_profile=
have_home=
have_dest=
while [ $# -gt 0 ]; do
  case "$1" in
  --profile)
    [ $# -ge 2 ] || refuse "--profile needs a value: the OMP profile to install into"
    profile=$2 have_profile=1
    shift 2
    ;;
  --home)
    [ $# -ge 2 ] || refuse "--home needs a value: the HOME Oh My Pi runs under for the profile"
    home=$2 have_home=1
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
[ -n "$have_home" ] || refuse "--home is required: the HOME Oh My Pi runs under for the profile"
[ -n "$have_dest" ] || refuse "--dest is required: the directory the plugin is unpacked into"

# OMP trims the profile name and reads an empty one or "default" as its default profile
# (normalizeProfileName, @oh-my-pi/pi-utils dirs.ts), the one every plain `omp` uses. Any other
# name goes to OMP as given, and OMP refuses one it cannot use.
trimmed=${profile#"${profile%%[![:space:]]*}"}
trimmed=${trimmed%"${trimmed##*[![:space:]]}"}
if [ -z "$trimmed" ] || [ "$trimmed" = default ]; then
  refuse "--profile '$profile' is OMP's default profile, the one plain \`omp\` uses; name a dedicated profile"
fi

# The profile goes under the run's own home, never the caller's: a proof's profile, and a crashed
# proof's leftovers, then live in its work directory (lib/omp-home.sh, make_omp_home).
home=$(realpath -m -- "$home")
[ "$home" != "$(realpath -m -- "$HOME")" ] || refuse "--home $home is your own HOME; give the profile a home of its own (make_omp_home, lib/omp-home.sh)"
[ -d "$home/.omp" ] || refuse "--home $home has no .omp directory; make it with make_omp_home (lib/omp-home.sh)"

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

# The Oh My Pi both daemons run (omp-pin.ts), under the run's home and the operator's mise tool
# store, from the unpacked plugin's directory, where no mise config is: an operator's `omp` wrapper
# reads its own files from HOME, which is the run's here.
pin=$(bun "$root/packages/daemon/src/daemon/omp-pin.ts")
mise where "$pin" >/dev/null 2>&1 || mise install "$pin" >&2
mise_data=${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}
run_omp() { (cd "$dest" && HOME=$home MISE_DATA_DIR=$mise_data OMP_PROFILE=$profile mise x "$pin" -- omp "$@"); }

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
run_omp plugin install "$dest" >&2

listing=$(run_omp plugin list --json)
plugin=$(jq -c '[.npm[]? | select(.name == "@sjawhar/pi-legion-envoy")]' <<<"$listing")
if ! jq -e --arg v "$version" 'length == 1 and .[0].version == $v and .[0].enabled == true' \
  <<<"$plugin" >/dev/null; then
  echo "$me: omp $pin plugin list under HOME=$home OMP_PROFILE=$profile does not show @sjawhar/pi-legion-envoy@$version enabled: $plugin" >&2
  exit 1
fi
installed=$(jq -r '.[0].path' <<<"$plugin")
resolved=$(realpath -m -- "$installed")
if [ "$resolved" != "$dest" ]; then
  echo "$me: profile $profile resolves @sjawhar/pi-legion-envoy to $resolved, not the unpacked $dest" >&2
  exit 1
fi
echo "$me: @sjawhar/pi-legion-envoy@$version enabled in OMP profile $profile under $home ($installed -> $dest)" >&2
printf '%s\n' "$installed/package.json"
