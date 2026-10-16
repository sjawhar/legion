#!/usr/bin/env bash
# Saves the real gh's help of each command the `legion gh` tests read into gh-help/, one file per
# command path with its words joined by "_", and the gh version into gh-help/VERSION. Run it with
# the gh the worker image pins on PATH, or LEGION_GH_PATH naming it, when that pin moves.
set -euo pipefail
gh=${LEGION_GH_PATH:-$(command -v gh)}
out=$(cd "$(dirname "$0")" && pwd)/gh-help
rm -rf "$out"
mkdir -p "$out"
"$gh" --version | head -1 >"$out/VERSION"
for path in "pr" "pr view" "pr create" "pr list" "api" "repo" "repo view" "repo clone" "repo create" \
  "repo list" "repo deploy-key" "repo deploy-key add" "issue" "issue transfer" "label" "label clone" \
  "search" "search issues" "auth" "auth status" "co" "secret" "secret set" "browse"; do
  # shellcheck disable=SC2086
  NO_COLOR=1 "$gh" $path --help >"$out/${path// /_}.txt"
done
echo "saved $(find "$out" -name '*.txt' | wc -l) help files from $(cat "$out/VERSION")"
