#!/usr/bin/env bash
# Tests for `check-plugin-names.sh`: a clean tree passes; a line or a path naming the pre-split
# plugin @sjawhar/pi-legion-envoy fails, naming the file and line; a file git has not been told
# about counts; each class of the allowlist passes through one representative, and a path that
# merely begins like an allowlisted directory does not; and the real repository passes.
#
# Each case builds a miniature git repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs: it resolves its root from its own
# path, which the symlink puts at the fixture root.
#
# Run from anywhere: .github/scripts/check-plugin-names.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-plugin-names.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }

old_package='@sjawhar/pi-legion-envoy'

# fixture <name>: a clean repository with one tracked file. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/docs"
  ln -s "$check_script" "$root/.github/scripts/check-plugin-names.sh"
  printf '# Notes\n\nInstall @sjawhar/pi-envoy, and @sjawhar/pi-legion for a Legion pane.\n' > "$root/docs/notes.md"
  git -C "$root" init -q
  git -C "$root" add docs/notes.md
  echo "$root"
}

# run_check <root>: sets `out` to the combined output and `status` to the exit code.
run_check() {
  local root=$1
  set +e
  out=$(cd "$root" && .github/scripts/check-plugin-names.sh 2>&1)
  status=$?
  set -e
}

echo "case: a clean repository"
root=$(fixture clean)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it scanned" "$(contains "$out" 'no text file or path names the pre-split plugin')"

echo "case: a documentation line naming the old package fails, naming the file and line"
root=$(fixture doc-line)
printf 'line one\nInstall %s into the profile.\n' "$old_package" >> "$root/docs/notes.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "annotates the file and line for GitHub" \
  "$(contains "$out" '^::error file=docs/notes.md,line=5::docs/notes.md:5: names the pre-split plugin pi-legion-envoy$')"
check "counts the findings" "$(contains "$out" '^check-plugin-names: 1 finding(s)')"
check "names the two packages that replace it" "$(contains "$out" '@sjawhar/pi-envoy (packages/pi-envoy)')"

echo "case: the bare name in any case is a hit too"
root=$(fixture bare-name)
printf 'The PI-LEGION-ENVOY plugin.\n' > "$root/docs/notes.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the line" "$(contains "$out" 'docs/notes.md:1: names the pre-split plugin')"

echo "case: a line is reported once, however many times it names the package"
root=$(fixture once)
printf '%s and %s again\n' "$old_package" "$old_package" > "$root/docs/notes.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "one finding counted" "$(contains "$out" '^check-plugin-names: 1 finding(s)')"

echo "case: a path naming the old package fails"
root=$(fixture path-name)
mkdir -p "$root/docs/pi-legion-envoy"
printf 'clean text\n' > "$root/docs/pi-legion-envoy/install.md"
git -C "$root" add docs
run_check "$root"
check "fails" "$(is "$status" 1)"
check "annotates the file for GitHub" \
  "$(contains "$out" '^::error file=docs/pi-legion-envoy/install.md::docs/pi-legion-envoy/install.md: names the pre-split plugin pi-legion-envoy in its path$')"

echo "case: a file git has not been told about still counts; an ignored one does not"
root=$(fixture untracked)
printf 'omp plugin install %s\n' "$old_package" > "$root/docs/new.md"
run_check "$root"
check "an untracked file fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'docs/new.md:1:')"

root=$(fixture ignored)
printf 'dist/\n' > "$root/.gitignore"
git -C "$root" add .gitignore
mkdir -p "$root/dist"
printf '%s\n' "$old_package" > "$root/dist/envoy.js"
run_check "$root"
check "an ignored file passes" "$(is "$status" 0)"

echo "case: a file git tracks under an ignored path still counts"
root=$(fixture tracked-ignored)
printf 'notes/\n' > "$root/.gitignore"
git -C "$root" add .gitignore
mkdir -p "$root/notes"
printf '%s\n' "$old_package" > "$root/notes/report.md"
git -C "$root" add -f notes/report.md
run_check "$root"
check "a tracked file under an ignored path fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'notes/report.md:1:')"

echo "case: binary files are skipped"
root=$(fixture binary)
printf '%s\0binary\n' "$old_package" > "$root/docs/image.bin"
git -C "$root" add docs/image.bin
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: each class of the allowlist passes, through one representative"
representatives=(
  docs/plans/2026-09-04-design.md
  docs/plans/nested/2026-09-04-design.md
  docs/solutions/legion/a-record.md
  docs/superpowers/specs/a-spec.md
  packages/pi-envoy/CHANGELOG.md
  packages/pi-legion/CHANGELOG.md
  packages/claude-envoy/CHANGELOG.md
  packages/envoy/CHANGELOG.md
  packages/contracts/src/dispatch-api.ts
  packages/contracts/AGENTS.md
  packages/envoy/internal/dispatch/model/model.go
  packages/dispatch/web/src/features/agent-view/AgentConversationPage.tsx
  packages/daemon/internal/daemon/bootgate.go
  packages/daemon/internal/daemon/probe.mjs
  packages/pi-shared/src/interface.ts
  packages/pi-shared/AGENTS.md
  packages/pi-legion/extensions/legion.ts
  packages/pi-legion/extensions/legion.test.ts
  packages/daemon/internal/daemon/bootgate_test.go
  packages/daemon/internal/daemon/imageprobe_test.go
  packages/daemon/cmd/legion/controller_test.go
  .github/workflows/release.yaml
  .legion/plan.json
)
for index in "${!representatives[@]}"; do
  file=${representatives[index]}
  root=$(fixture "allowed-$index")
  mkdir -p "$root/$(dirname "$file")"
  printf 'history: %s\n' "$old_package" > "$root/$file"
  git -C "$root" add "$file"
  run_check "$root"
  check "allowed-$index ($file) passes" "$(is "$status" 0)"
done

echo "case: a path that merely begins like an allowlisted directory is not allowed"
root=$(fixture near-miss)
mkdir -p "$root/docs/plans-archive"
printf '%s\n' "$old_package" > "$root/docs/plans-archive/old.md"
git -C "$root" add docs
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'docs/plans-archive/old.md:1:')"

echo "case: an allowlisted file beside a hit does not hide the hit"
root=$(fixture mixed)
printf 'history: %s\n' "$old_package" > "$root/docs/plans-note.md"
mkdir -p "$root/docs/plans"
printf 'history: %s\n' "$old_package" > "$root/docs/plans/note.md"
git -C "$root" add docs
run_check "$root"
check "fails" "$(is "$status" 1)"
check "one finding counted" "$(contains "$out" '^check-plugin-names: 1 finding(s)')"
check "names the file outside the allowlist" "$(contains "$out" 'docs/plans-note.md:1:')"

echo "case: the real repository passes"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "reports the clean scan" "$(contains "$out" '^check-plugin-names: no text file or path names the pre-split plugin')"

summary "check-plugin-names.sh"
