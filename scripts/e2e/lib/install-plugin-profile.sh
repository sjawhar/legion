#!/usr/bin/env bash
# Installs this checkout's @sjawhar/pi-legion-envoy into a named OMP profile, packed exactly as the
# release packs it, so a stage proof runs the branch-built plugin while the
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
# The plugin is packed by lib/pack-plugin.sh, the one pack step every branch-built install shares
# (its header maps each step to the release's), then:
#   1. unpack the tarball into <dir>           worker.Dockerfile's `tar xzf ./*.tgz -C /out/pi-legion-envoy`
#   2. OMP_PROFILE=<name> omp plugin install   worker.Dockerfile's `omp plugin install /opt/legion/pi-legion-envoy`
#   3. verify with OMP_PROFILE=<name> omp plugin list
# Steps 2 and 3 run the Oh My Pi the daemons pin (omp-pin.ts) under HOME=<home>.
# Each source is cited by what it runs, never by line number: the lines move with every edit
# above them. The tarball is written under this run's temp directory, so an interrupted run strands
# no .tgz in the checkout (the release's `rm -f ./*.tgz` guards the same stale-glob case).
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
# An unset or empty HOME is refused first: realpath of it fails inside the substitution, which
# yields an empty string and would pass the comparison below for any --home.
[ -n "${HOME:-}" ] || refuse "HOME is unset or empty, so --home cannot be checked against it"
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

work=$(mktemp -d "${TMPDIR:-/tmp}/$me.XXXXXXXX")
trap 'rm -rf "$work"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" "$work/pack")
version=$(tar xzOf "$tarball" package/package.json | jq -r .version)

mkdir -p "$dest"
tar xzf "$tarball" -C "$dest" --strip-components=1
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
