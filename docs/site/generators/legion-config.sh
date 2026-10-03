#!/usr/bin/env bash
# Writes Legion's configuration reference, <content dir>/legion/reference/config.md, from the
# annotated example files the repository tracks under deploy/kubernetes: the daemon's legion.yaml,
# the operator-side controller.yaml, the operator's model route (pod.yml, models.yml, overlay.yml),
# and any other example the directory ships. Each file is rendered whole, comments included, since
# the comments are the documentation; the files themselves are the only source. Only tracked files
# are read (`git ls-files`), so nothing an operator keeps untracked in a checkout reaches the page.
#
#   docs/site/generators/legion-config.sh <content dir>      # run from the repository root
#
# A deploy/kubernetes holding no example fails the build rather than writing an empty page.
set -euo pipefail

me=${0##*/}
fail() {
  echo "$me: $*" >&2
  exit 1
}

[ $# -eq 1 ] || {
  echo "usage: $me <content dir>" >&2
  exit 2
}
content=$1
[ -d "$content" ] || fail "$content is not a directory"
source_dir=deploy/kubernetes
[ -d "$source_dir" ] || fail "no $source_dir here; run $me from the repository root"

# Git's pathspec `*` matches across `/`, so each pattern reaches every directory below.
tracked=$(git ls-files -- "$source_dir/*.example" "$source_dir/*.yml" "$source_dir/*.yaml")
[ -n "$tracked" ] || fail "$source_dir holds no tracked example file"
mapfile -t files <<<"$tracked"

# fence prints the code fence for the text on stdin: one backtick more than its longest run of
# backticks, and at least three, so no line of the text can close the block early.
fence() {
  local longest
  longest=$(awk '{ while (match($0, /`+/)) { if (RLENGTH > n) n = RLENGTH; $0 = substr($0, RSTART + RLENGTH) } } END { print n + 0 }')
  [ "$longest" -ge 3 ] || longest=2
  printf '%*s' $((longest + 1)) '' | tr ' ' '`'
}

out=$content/legion/reference
mkdir -p "$out"
page=$out/config.md

{
  cat <<'EOF'
---
title: Configuration reference
description: The annotated example files Legion ships for an operator, rendered from the repository when the site was built.
---

Each file below is rendered whole from the repository when the site was built; its comments are
its documentation. [Running Legion](/legion/legion/running-legion/) says which file goes where and in what
order an operator sets them up.

EOF
  for file in "${files[@]}"; do
    ticks=$(fence <"$file")
    # shellcheck disable=SC2016 # the backticks are a Markdown code span, not a command substitution
    printf '## `%s`\n\n%syaml\n' "$file" "$ticks"
    cat "$file"
    # A file whose last line has no newline still closes its fence on a line of its own.
    [ -z "$(tail -c1 "$file")" ] || printf '\n'
    printf '%s\n\n' "$ticks"
  done
} >"$page"

echo "$me: wrote $page (${#files[@]} files from $source_dir)"
