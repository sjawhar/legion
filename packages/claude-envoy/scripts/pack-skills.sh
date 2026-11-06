#!/usr/bin/env bash
# The `prepack` and `postpack` of @sjawhar/claude-envoy, and the one place that makes the packed
# tarball carry the plugin's skills.
#
# skills/<name> is a symlink into the repository's skills/, and those eight links are the
# statement of which skills this plugin ships. `bun pm pack` packs a link as nothing at all —
# measured on bun 1.4.4: with `skills/**` in `files` and the links in place, the tarball held no
# package/skills member whatever — so an install got no skill, and the SessionStart and
# SubagentStart hook hooks/hooks.json runs on every session, `session-hook.js dispatch-first`,
# threw on the SKILL.md it reads under the plugin root. prepack replaces each link with the
# directory it points at, which `files` then packs as real files; postpack puts the links back, so
# the checkout is as the pack found it.
#
# This is what the two Oh My Pi plugins do (scripts/pi-plugin-prepack.sh, their prepack, stages
# each one's skills into dist/skills and their postpack removes them again). The difference is
# that this package's links are tracked files, not a staging directory, so postpack restores them
# rather than deleting them, and the list of skills is read off the links rather than written out
# a second time here.
#
# npm and bun both run these with the package root as the working directory, as that script's
# header says, so the package is $PWD and the repository $PWD/../..:
#
#   scripts/pack-skills.sh            as prepack: each skills/<name> link -> the skill's files
#   scripts/pack-skills.sh --restore  as postpack: each skills/<name> directory -> the link
set -euo pipefail

me=pack-skills

refuse() {
  echo "$me: $*" >&2
  exit 1
}

restore=false
if [ "${1:-}" = --restore ]; then
  restore=true
  shift
fi
[ $# -eq 0 ] || refuse "usage: $me [--restore], from the package root, as its prepack and postpack"

[ -f package.json ] && [ -d skills ] || refuse \
  "no package.json or no skills/ in $PWD; run from the package root, as npm and bun run prepack"
repo_skills=$(cd ../../skills && pwd -P)

shopt -s nullglob
entries=(skills/*)
[ ${#entries[@]} -gt 0 ] || refuse "skills/ is empty; the links in it are what this package ships"

for entry in "${entries[@]}"; do
  name=${entry#skills/}
  if $restore; then
    # Rebuilt from the name alone, so postpack needs nothing prepack left behind and a postpack
    # that runs without a prepack writes the same link the checkout already holds.
    rm -rf "$entry"
    ln -s "../../../skills/$name" "$entry"
    continue
  fi
  [ -L "$entry" ] || refuse \
    "skills/$name is not a link; an earlier pack staged it and its postpack never put the link back"
  target=$(readlink "$entry")
  [ "$target" = "../../../skills/$name" ] || refuse \
    "skills/$name points at $target, not ../../../skills/$name: this package ships the repository's skills"
  [ -d "$repo_skills/$name" ] || refuse "skills/$name names no directory under $repo_skills"
  rm "$entry"
  cp -R "$repo_skills/$name" "$entry"
done
