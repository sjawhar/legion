#!/usr/bin/env bash
# @sjawhar/pi-legion-envoy, the one Oh My Pi plugin that carried both the Envoy tools and the Legion
# entry, was split into @sjawhar/pi-envoy (every session's Envoy and Dispatch tools) and
# @sjawhar/pi-legion (the Legion entry a Legion pane loads beside it) and gets no further release
# (LEGION-247). A document, workflow, script, prompt, test or file name that still names the old
# package sends a reader to a package that gets no fixes, and the daemon's boot gate refuses a
# Legion pane that still loads it. This check fails on any text line or file path naming
# `pi-legion-envoy`, in any case, outside the allowlist below.
#
# The allowlist holds history and the remedies that name the old package as the thing to remove:
# each entry is an exact path or a directory prefix (ending in "/"), never a pattern that would
# hide a hit in a file nobody listed, and each says why that file legitimately keeps the name. A
# new file that records history (a changelog entry, a dated plan or solution under those
# directories) is covered by its directory; anything else that needs the name is added here, with
# its reason.
#
# It scans every file git tracks, a file under an ignored path included, plus every untracked file
# git does not ignore, so a new file is caught before it is added; the ignored `.jj/`, `.git/`,
# `node_modules/` and each untracked `dist/` are never listed, so they need no entry. Binary files
# are skipped. Every hit is printed as a GitHub annotation naming the file and line; the only
# non-zero exits are hits (1) and a search that failed (2).
#
# Run from anywhere: .github/scripts/check-plugin-names.sh
# CI runs it over the repository in the lint job of pr-and-main.yaml, beside check-private-names.sh,
# and its tests (check-plugin-names.test.sh) in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

old_package='pi-legion-envoy'
label="names the pre-split plugin $old_package"

allowlist=(
  # Dated design documents and plans, written when the one package existed.
  docs/plans/
  # Dated solution records, each about a bug of its time.
  docs/solutions/
  # Dated superpowers specs, written when the one package existed.
  docs/superpowers/
  # The Envoy plugin's changelog: its history under the old name, and the entry that renames it.
  packages/pi-envoy/CHANGELOG.md
  # The Legion plugin's changelog: its first entry says which package it was split from.
  packages/pi-legion/CHANGELOG.md
  # The Claude Code plugin's changelog: entries of its time name the omp plugin as it was then.
  packages/claude-envoy/CHANGELOG.md
  # The Envoy listener's changelog: entries of its time name the omp plugin as it was then.
  packages/envoy/CHANGELOG.md
  # The legacy Dispatch client comment: `SearchResult.issue` stays until no installed client built
  # before the owner-only shape remains, and the old package is one of those clients.
  packages/contracts/src/dispatch-api.ts
  packages/contracts/AGENTS.md
  packages/envoy/internal/dispatch/model/model.go
  # The live-view message names the plugin releases that do not stream ("before 5.8.0").
  packages/dispatch/web/src/features/agent-view/AgentConversationPage.tsx
  # The daemon's boot gate: its `legacyPackage` constant names the old package in the refusal that
  # tells an operator to uninstall it, and its load probe reads the old package's marker.
  packages/daemon/internal/daemon/bootgate.go
  packages/daemon/internal/daemon/probe.mjs
  # The shared interface: `LEGACY_LEGION_LOADED_KEY` is the old package's marker, and the symbol
  # table in the package's AGENTS.md says what that marker means.
  packages/pi-shared/src/interface.ts
  packages/pi-shared/AGENTS.md
  # The Legion entry's refusal names the old package as the thing to uninstall, its test asserts
  # that sentence, and the package's AGENTS.md quotes it.
  packages/pi-legion/extensions/legion.ts
  packages/pi-legion/extensions/legion.test.ts
  packages/pi-legion/AGENTS.md
  # The Go tests that plant the old package's marker and assert each lane's refusal.
  packages/daemon/internal/daemon/bootgate_test.go
  packages/daemon/internal/daemon/imageprobe_test.go
  packages/daemon/cmd/legion/controller_test.go
  # The release workflow's first-release seed reads the old name's npm version, so the two new
  # packages continue its version line instead of restarting at 0.x.
  .github/workflows/release.yaml
  # Committed handoffs quote the plan that names the split.
  .legion/
  # This check and its test spell the name they hunt.
  .github/scripts/check-plugin-names.sh
  .github/scripts/check-plugin-names.test.sh
)

# allowed <path>: whether an allowlist entry is the path or a directory above it.
allowed() {
  local entry
  for entry in "${allowlist[@]}"; do
    case "$entry" in
    */) [[ $1 == "$entry"* ]] && return 0 ;;
    *) [[ $1 == "$entry" ]] && return 0 ;;
    esac
  done
  return 1
}

matches=$(mktemp)
hits=$(mktemp)
paths=$(mktemp)
trap 'rm -f "$matches" "$hits" "$paths"' EXIT

# run <command...>: appends the command's output to $matches; a failed search (exit 2 or more,
# where grep's 1 means no match) stops the check.
run() {
  local status=0
  "$@" >> "$matches" || status=$?
  if [ "$status" -gt 1 ]; then
    echo "check-plugin-names: the search with $1 failed (exit $status)" >&2
    exit 2
  fi
}

git -c core.quotePath=false ls-files --cached --others --exclude-standard | sort -u > "$paths"
scanned="$(wc -l < "$paths") files in the repository"

# Text lines. --untracked adds the files git has not been told about, but skips a tracked file
# under an ignored path, which the plain pass reads; a tracked file appears in both, so the rows
# are deduplicated below. core.quotePath=false prints a non-ASCII path as it is, so the annotation
# names a real file.
run git -c core.quotePath=false grep -n -I -i -F --untracked -e "$old_package" -- .
run git -c core.quotePath=false grep -n -I -i -F -e "$old_package" -- .
cut -d: -f1,2 "$matches" | sort -u | while IFS=: read -r path line; do
  allowed "$path" || printf '%s:%s: %s\n' "$path" "$line" "$label"
done > "$hits"

# File and directory names.
: > "$matches"
run grep -i -F -e "$old_package" "$paths"
while IFS= read -r path; do
  allowed "$path" || printf '%s:0: %s in its path\n' "$path" "$label"
done < "$matches" >> "$hits"

if [ -s "$hits" ]; then
  sort -t: -k1,1 -k2,2n -o "$hits" "$hits"
  while IFS= read -r hit; do
    path=${hit%%:*}
    rest=${hit#*:}
    line=${rest%%:*}
    if [ "$line" = 0 ]; then
      echo "::error file=$path::$path: ${rest#*: }"
    else
      echo "::error file=$path,line=$line::$hit"
    fi
  done < "$hits"
  echo "check-plugin-names: $(wc -l < "$hits") finding(s) ($scanned). @sjawhar/$old_package was split and gets no further release, so:"
  echo "  - for the Envoy and Dispatch tools every session loads write @sjawhar/pi-envoy (packages/pi-envoy);"
  echo "  - for the Legion entry a Legion pane loads beside it write @sjawhar/pi-legion (packages/pi-legion);"
  echo "  - a worker image carries both, at /opt/legion/pi-envoy and /opt/legion/pi-legion;"
  echo "  - a file that records history keeps the name only under an allowlist entry of this script, with its reason;"
  echo "  - for a file or directory whose path is reported, rename it."
  exit 1
fi

echo "check-plugin-names: no text file or path names the pre-split plugin $old_package outside its history ($scanned)"
