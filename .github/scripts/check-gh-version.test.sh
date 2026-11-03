#!/usr/bin/env bash
# Tests for `check-gh-version.sh`: the saved gh help and the self-hosted pool's gh name the gh the
# worker image pins.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs.
#
# Run from anywhere: .github/scripts/check-gh-version.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-gh-version.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes — the image pinning gh 2.98.0, help saved from it and the
# pool's install of it. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/actions/self-hosted-prereqs" "$root/packages/daemon/docker" "$root/packages/daemon/cmd/legion/testdata/gh-help"
  ln -s "$check_script" "$root/.github/scripts/check-gh-version.sh"
  printf 'FROM scratch\nARG JJ_TOOL=github:example/jj@0.45.1\nARG GH_TOOL=gh@2.98.0\nARG GH_TOOL\n' \
    > "$root/packages/daemon/docker/worker.Dockerfile"
  echo "gh version 2.98.0 (2026-08-20)" > "$root/packages/daemon/cmd/legion/testdata/gh-help/VERSION"
  printf 'runs:\n  steps:\n    - run: |\n        gh_version=2.98.0\n        gh_sha256=0000\n' \
    > "$root/.github/actions/self-hosted-prereqs/action.yml"
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
check "says what it compared" "$(contains "$out" "the saved gh help and the self-hosted pool's gh are gh 2.98.0, the worker image's pin")"

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

echo "case: the pool's gh is another release"
root=$(fixture pool-moved)
sed -i 's/gh_version=2.98.0/gh_version=2.45.0/' "$root/.github/actions/self-hosted-prereqs/action.yml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both and the fix" \
  "$(contains "$out" "action.yml:4: gh_version=2.45.0, and packages/daemon/docker/worker.Dockerfile:3 pins gh 2.98.0: set gh_version=2.98.0 and gh_sha256")"

echo "case: the pool's install names no gh, or two"
root=$(fixture pool-unpinned)
sed -i '/gh_version=/d' "$root/.github/actions/self-hosted-prereqs/action.yml"
run_check "$root"
check "exits 1 with none" "$(is "$status" 1)"
check "says how many" "$(contains "$out" 'must set gh_version exactly once, as gh_version=X.Y.Z; it does 0 times')"
root=$(fixture pool-twice)
echo "        gh_version=2.98.0" >> "$root/.github/actions/self-hosted-prereqs/action.yml"
run_check "$root"
check "exits 1 with two" "$(is "$status" 1)"
check "says how many" "$(contains "$out" 'it does 2 times')"

summary "check-gh-version.sh"
