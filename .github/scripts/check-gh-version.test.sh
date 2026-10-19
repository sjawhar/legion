#!/usr/bin/env bash
# Tests for `check-gh-version.sh`: the saved gh help names the gh the worker image pins.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs.
#
# Run from anywhere: .github/scripts/check-gh-version.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-gh-version.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes — the image pinning gh 2.98.0 and help saved from it. Echoes
# its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/daemon/docker" "$root/packages/daemon/cmd/legion/testdata/gh-help"
  ln -s "$check_script" "$root/.github/scripts/check-gh-version.sh"
  printf 'FROM scratch\nARG JJ_TOOL=github:example/jj@0.45.1\nARG GH_TOOL=gh@2.98.0\nARG GH_TOOL\n' \
    > "$root/packages/daemon/docker/worker.Dockerfile"
  echo "gh version 2.98.0 (2026-08-20)" > "$root/packages/daemon/cmd/legion/testdata/gh-help/VERSION"
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-gh-version.sh" 2>&1)
  status=$?
  set -e
}

echo "case: the saved help is the pinned gh's"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it compared" "$(contains "$out" "the saved gh help is gh 2.98.0's, the worker image's pin")"

echo "case: the pin moved and the help did not"
root=$(fixture moved)
sed -i 's/GH_TOOL=gh@2.98.0/GH_TOOL=gh@2.99.1/' "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both and the fix" \
  "$(contains "$out" "saves gh 2.98.0's help, and packages/daemon/docker/worker.Dockerfile:3 pins gh 2.99.1: run packages/daemon/cmd/legion/testdata/regenerate-gh-help.sh with gh 2.99.1")"

echo "case: a pin that is no exact release"
root=$(fixture floating)
sed -i 's/GH_TOOL=gh@2.98.0/GH_TOOL=gh@latest/' "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'ARG GH_TOOL=gh@latest is no exact gh release')"

echo "case: no pin, or two"
root=$(fixture unpinned)
sed -i '/GH_TOOL=gh/d' "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 1 with none" "$(is "$status" 1)"
check "says how many" "$(contains "$out" 'must default ARG GH_TOOL exactly once, as gh@X.Y.Z; it does 0 times')"
root=$(fixture twice)
echo "ARG GH_TOOL=gh@2.98.0" >> "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 1 with two" "$(is "$status" 1)"
check "says how many" "$(contains "$out" 'it does 2 times')"

echo "case: a VERSION file that is no gh --version line, or none"
root=$(fixture garbled)
echo "2.98.0" > "$root/packages/daemon/cmd/legion/testdata/gh-help/VERSION"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" "holds '2.98.0', not the first line of \`gh --version\`")"
root=$(fixture missing)
rm "$root/packages/daemon/cmd/legion/testdata/gh-help/VERSION"
run_check "$root"
check "exits 1 when it is missing" "$(is "$status" 1)"
check "names it, without a traceback" "$(contains "$out" 'gh-help/VERSION is missing')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

summary "check-gh-version.sh"
