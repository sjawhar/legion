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
# The links are tracked, so a prepack that stops halfway is a working copy with deleted links and
# copied directories in their place, which the next pack refuses: every entry is checked before
# any of them is swapped, and a trap puts back every link this run swapped on any exit. The trap
# is disarmed where the swap loop finishes, since the pack that follows a prepack needs the real
# directories; from there the postpack is what puts the links back, and where no postpack runs —
# bun runs none when the pack itself fails — `--restore` puts them back by hand.
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

# link_back <name>...: make each skills/<name> the tracked link again, whatever is there now.
# Each link is rebuilt from the name alone, so a restore needs nothing a prepack left behind and
# a postpack that runs without a prepack writes the same links the checkout already holds.
link_back() {
  local name
  for name in "$@"; do
    rm -rf "skills/$name"
    ln -s "../../../skills/$name" "skills/$name"
  done
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
names=()
for entry in "${entries[@]}"; do
  names+=("${entry#skills/}")
done

if $restore; then
  link_back "${names[@]}"
  exit 0
fi

# Every entry's three conditions, all of them before the first swap below: a refusal on a later
# entry would otherwise leave the entries before it staged as copied directories.
for name in "${names[@]}"; do
  [ -L "skills/$name" ] || refuse \
    "skills/$name is not a link; an earlier pack staged it and its postpack never put the link back: bash scripts/pack-skills.sh --restore puts the links back"
  target=$(readlink "skills/$name")
  [ "$target" = "../../../skills/$name" ] || refuse \
    "skills/$name points at $target, not ../../../skills/$name: this package ships the repository's skills"
  [ -d "$repo_skills/$name" ] || refuse "skills/$name names no directory under $repo_skills"
done

# The swap, under a trap that puts back every link it has replaced. What is left to fail here is
# the filesystem rather than an entry, and a failure between the first swap and the last would
# leave tracked links as copied directories. The trap is disarmed the moment the loop finishes,
# and nowhere earlier: the pack this prepack runs for needs the real directories, and the
# postpack puts the links back after it.
swapped=()
trap '[ ${#swapped[@]} -eq 0 ] || link_back "${swapped[@]}"' EXIT
for name in "${names[@]}"; do
  rm "skills/$name"
  swapped+=("$name")
  cp -R "$repo_skills/$name" "skills/$name"
done
trap - EXIT
