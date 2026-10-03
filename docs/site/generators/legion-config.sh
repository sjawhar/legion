#!/usr/bin/env bash
# Writes Legion's configuration reference, <content dir>/legion/reference/config.md, from the
# annotated example files under deploy/kubernetes: the operator-side controller.yaml, the operator's
# model route (pod.yml, models.yml, overlay.yml), and any other example the directory ships, a
# legion.yaml example among them once one exists. Each file is rendered whole, comments included,
# since the comments are the documentation; the files themselves are the only source.
#
#   docs/site/generators/legion-config.sh <content dir>      # run from the repository root
#
# A deploy/kubernetes holding no example fails the build rather than writing an empty page.
set -euo pipefail

me=legion-config.sh
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

files=()
while IFS= read -r file; do
  files+=("$file")
done < <(find "$source_dir" -type f \( -name '*.example' -o -name '*.yml' -o -name '*.yaml' \) | LC_ALL=C sort)
[ "${#files[@]}" -gt 0 ] || fail "$source_dir holds no example file"

legion_yaml=
for file in "${files[@]}"; do
  case $(basename "$file") in legion.yaml* | legion.yml*) legion_yaml=$file ;; esac
done

out=$content/legion/reference
mkdir -p "$out"
page=$(mktemp "${TMPDIR:-/tmp}/legion-config-reference.XXXXXXXX")
trap 'rm -f "$page"' EXIT

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
  if [ -z "$legion_yaml" ]; then
    printf 'The repository ships no `legion.yaml` example; [Running Legion](/legion/legion/running-legion/#legionyaml) carries a minimal one with every key a Kubernetes deployment requires.\n\n'
  fi
  for file in "${files[@]}"; do
    printf '## `%s`\n\n```yaml\n' "$file"
    cat "$file"
    # A file whose last line has no newline still closes its fence on a line of its own.
    [ -z "$(tail -c1 "$file")" ] || printf '\n'
    printf '```\n\n'
  done
} >"$page"

chmod 0644 "$page"
mv "$page" "$out/config.md"
trap - EXIT
echo "$me: wrote $out/config.md (${#files[@]} files from $source_dir)"
