#!/usr/bin/env bash
# The `prepack` of both Oh My Pi plugins (packages/pi-envoy, packages/pi-legion), and the one place
# that says which of the repository's skills/ each ships. npm and bun run `prepack` with the package
# root as the working directory, so the manifest is $PWD/package.json: the script reads the plugin's
# name and its one extension entry from there, builds that entry into dist/, writes the notices of
# every package the bundle inlines, and stages the plugin's own skill directories into dist/skills,
# which the manifest's `omp.skills` names and `postpack` removes again.
#
#   scripts/pi-plugin-prepack.sh                                    as a plugin's prepack
#   scripts/pi-plugin-prepack.sh --stage-skills <package name> <dest dir>
#
# The contract with whoever packs: the release workflow, the worker image and the e2e pack step
# rewrite `omp.extensions` to the packed bundle, ["dist/<entry>.js"], before packing and restore the
# committed manifest afterwards. Packing with the source manifest, ["extensions/<entry>.ts"], would
# publish a package whose extension file is absent from the tarball (`files` ships dist/ only), so
# the script refuses any other list, naming the one it found and the one it requires.
#
# The partition lives here, keyed by package name, rather than in a manifest field the daemon's
# boot gate would then have to know: the Envoy plugin ships what every session uses, the Legion
# plugin what a Legion pane's prompts load. The two lists must partition skills/ exactly, which
# packages/pi-shared/test/skills-partition.test.ts holds them to; `--stage-skills` stages one list
# alone into a directory of the caller's and builds nothing, for that test and the guards.
set -euo pipefail

me=pi-plugin-prepack
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)

refuse() {
  echo "$me: $*" >&2
  exit 1
}

# Sets `entry` and `skills` for the named plugin; any other name is refused.
plugin() {
  case "$1" in
  @sjawhar/pi-envoy)
    entry=envoy
    skills=(dispatch dispatch-first dispatch-brainstorming envoy)
    ;;
  @sjawhar/pi-legion)
    entry=legion
    skills=(
      legion-architect legion-controller legion-oracle legion-retro legion-worker
      ce-simplify-code thermonuclear-code-quality thermonuclear-deep-review
    )
    ;;
  *)
    refuse "unknown plugin package '$1'; the plugins this script packs are @sjawhar/pi-envoy and @sjawhar/pi-legion"
    ;;
  esac
}

# Copies each of `skills` from the repository's skills/ into <dest>, which must be new or empty so
# that what it holds afterwards is the partition and nothing else.
stage_skills() {
  local dest=$1 skill
  mkdir -p "$dest"
  if [ -n "$(ls -A "$dest")" ]; then
    refuse "$dest is not empty; the staged skills must be the plugin's partition alone"
  fi
  for skill in "${skills[@]}"; do
    [ -d "$root/skills/$skill" ] || refuse "skills/$skill, which $name ships, is not in $root/skills"
    cp -R "$root/skills/$skill" "$dest/$skill"
  done
}

if [ "${1:-}" = --stage-skills ]; then
  [ $# -eq 3 ] || refuse "usage: $me --stage-skills <package name> <dest dir>"
  name=$2
  plugin "$name"
  stage_skills "$3"
  exit 0
fi
[ $# -eq 0 ] || refuse "usage: $me (as a plugin's prepack, from its root) | $me --stage-skills <package name> <dest dir>"

[ -f package.json ] || refuse "no package.json in $PWD; run from a plugin's root, as npm and bun run prepack"
command -v jq >/dev/null 2>&1 || refuse "jq is required"
name=$(jq -r '.name' package.json)
plugin "$name"
required="[\"dist/$entry.js\"]"
if ! jq -e --arg bundle "dist/$entry.js" '.omp.extensions == [$bundle]' package.json >/dev/null; then
  refuse "refusing to pack $name with omp.extensions=$(jq -c '.omp.extensions' package.json); rewrite it to $required first (the release, the worker image and the e2e pack step do, then restore the committed manifest)"
fi

# dist/THIRD_PARTY_NOTICES carries the license of every package the bundle inlines, read from the
# build's metafile; it fails the pack when one has none.
metafile=$(mktemp)
trap 'rm -f "$metafile"' EXIT
bun build "extensions/$entry.ts" --outdir dist --target bun --format esm \
  --external @oh-my-pi/pi-coding-agent --external @oh-my-pi/pi-tui --external @oh-my-pi/pi-utils \
  --metafile="$metafile"
bun "$root/scripts/third-party-notices.ts" "$metafile" dist/THIRD_PARTY_NOTICES
rm -rf dist/skills
stage_skills dist/skills
