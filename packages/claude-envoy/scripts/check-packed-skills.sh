#!/usr/bin/env bash
# check-packed-skills.sh <tgz> — the packed plugin carries its skills as real files.
#
# release.yaml's claude_envoy_build runs this over the tarball it hands the publish job, beside
# the manifest checks of the same step, and tests/packed-skills.test.ts runs it over tarballs it
# packs itself. What it refuses is what the first tarball of this package held: no
# package/skills member at all, because skills/<name> is a link into the repository's skills/ and
# `bun pm pack` packs a link as nothing (scripts/pack-skills.sh, the prepack that resolves them).
# An install of such a tarball loses every skill, and the dispatch-first hook hooks/hooks.json
# runs on every session and subagent throws on the SKILL.md it reads under the plugin root.
#
# Three rules over the tarball's own listing, and nothing read off the checkout:
#   1. package/skills/dispatch-first/SKILL.md is there, the file that hook reads;
#   2. no member under package/skills is a link, which an install would leave dangling;
#   3. every package/skills/<name> holds a SKILL.md, so a half-staged pack is refused too.
set -euo pipefail

[ $# -eq 1 ] || {
  echo "::error::check-packed-skills.sh: usage: check-packed-skills.sh <tgz>" >&2
  exit 2
}
tar_file=$1

refuse() {
  echo "::error::$*" >&2
  exit 1
}

# `tar -tvzf` so the listing carries each member's type: a link line starts with 'l' (symbolic) or
# 'h' (hard), where a regular file's starts with '-'.
listing=$(tar -tvzf "$tar_file")
members=$(tar -tzf "$tar_file")

if ! printf '%s\n' "$members" | grep -qxF package/skills/dispatch-first/SKILL.md; then
  refuse "$tar_file holds no package/skills/dispatch-first/SKILL.md, which the dispatch-first hook reads on every session: pack it with the prepack that resolves skills/<name> (scripts/pack-skills.sh)"
fi

links=$(printf '%s\n' "$listing" | grep -E '^[lh].* package/skills/' || true)
if [ -n "$links" ]; then
  refuse "$tar_file packs a link under package/skills, which an install would leave dangling: ${links//$'\n'/; }"
fi

names=$(printf '%s\n' "$members" | sed -n 's|^package/skills/\([^/]*\)/.*|\1|p' | sort -u)
while IFS= read -r name; do
  if ! printf '%s\n' "$members" | grep -qxF "package/skills/$name/SKILL.md"; then
    refuse "$tar_file packs package/skills/$name with no SKILL.md, so the skill it ships is incomplete"
  fi
done <<< "$names"

echo "check-packed-skills.sh: $tar_file carries $(printf '%s\n' "$names" | wc -l | tr -d ' ') skills as real files"
