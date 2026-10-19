#!/usr/bin/env bash
# Saves the real gh's help of each command the `legion gh` tests read into gh-help/, one file per
# command path with its words joined by "_" (gh.txt for gh's own, `gh --help`), its reference of
# every command (`gh help reference`) as reference.txt, and the gh version into gh-help/VERSION.
# Run it with the gh the worker image pins on PATH, or LEGION_GH_PATH naming it, when that pin
# moves. gh runs with an empty configuration directory and no extensions, as in a fresh pane, so
# its own help lists gh's one default alias (co) and nothing of this machine's.
set -euo pipefail
gh=${LEGION_GH_PATH:-$(command -v gh)}
out=$(cd "$(dirname "$0")" && pwd)/gh-help
clean=$(mktemp -d)
trap 'rm -rf "$clean"' EXIT
mkdir -p "$clean/config" "$clean/data"
run() { env GH_CONFIG_DIR="$clean/config" XDG_DATA_HOME="$clean/data" GH_NO_UPDATE_NOTIFIER=1 NO_COLOR=1 "$gh" "$@"; }
rm -rf "$out"
mkdir -p "$out"
run --version | head -1 >"$out/VERSION"
run --help >"$out/gh.txt"
run help reference >"$out/reference.txt"
for path in "pr" "pr view" "pr create" "pr list" "pr ls" "pr comment" "api" "repo" "repo view" "repo clone" \
  "repo create" "repo edit" "repo fork" "repo set-default" "repo sync" "repo list" "repo deploy-key" "repo deploy-key add" "issue" \
  "issue close" "issue create" "issue develop" "issue edit" "issue transfer" "label" "label clone" "project" \
  "project link" "project unlink" "search" "search issues" "auth" "auth setup-git" "auth status" "co" "secret" \
  "secret set" "browse" "extension" "agent-task" "agent-task view"; do
  # shellcheck disable=SC2086
  run $path --help >"$out/${path// /_}.txt"
done
echo "saved $(find "$out" -name '*.txt' ! -name reference.txt | wc -l) help files and gh's reference from $(cat "$out/VERSION")"
