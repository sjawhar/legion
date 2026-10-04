#!/usr/bin/env bash
# Tests for `check-action-pins.sh`: every remote `uses:` in a workflow or composite action is
# pinned to a 40-character commit SHA.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs.
#
# Run from anywhere: .github/scripts/check-action-pins.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-action-pins.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

SHA=fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09

# fixture <name>: an empty tree with the script linked in. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows"
  ln -s "$check_script" "$root/.github/scripts/check-action-pins.sh"
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$(cd "$root" && "./.github/scripts/check-action-pins.sh" 2>&1)
  status=$?
  set -e
}

echo "case: every remote uses, in a workflow and a composite action, is pinned to a commit SHA"
root=$(fixture green)
cat > "$root/.github/workflows/ci.yaml" <<YAML
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@${SHA} # v5.1.0
      - uses: ./.github/actions/local
YAML
mkdir -p "$root/.github/actions/local"
cat > "$root/.github/actions/local/action.yml" <<YAML
name: Local
runs:
  using: composite
  steps:
    - uses: oven-sh/setup-bun@${SHA} # v2.2.0
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it covered" "$(contains "$out" 'every remote action across 2 files is pinned')"

echo "case: a floating tag fails"
root=$(fixture tag)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v5
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, line and ref" \
  "$(contains "$out" ".github/workflows/ci.yaml:7: 'actions/checkout@v5' is pinned to 'v5', not a 40-character commit SHA")"

echo "case: a short SHA fails"
root=$(fixture short-sha)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@fbc6f39
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" "'actions/checkout@fbc6f39' is pinned to 'fbc6f39'")"

echo "case: an upper-case SHA fails"
root=$(fixture upper-sha)
cat > "$root/.github/workflows/ci.yaml" <<YAML
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@${SHA^^}
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" ", not a 40-character commit SHA")"

echo "case: a ref-less uses fails"
root=$(fixture refless)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says there is no ref to pin" "$(contains "$out" 'names no ref')"

echo "case: a local path and a Docker reference are out of scope"
root=$(fixture local-and-docker)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: ./.github/actions/local
      - uses: docker://alpine:3.19
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a reusable-workflow call at job level is checked too"
root=$(fixture job-uses)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    uses: octo-org/octo-repo/.github/workflows/build.yaml@v1
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job-level call" "$(contains "$out" "octo-org/octo-repo/.github/workflows/build.yaml@v1' is pinned to 'v1'")"

echo "case: a file that cannot be parsed as YAML"
root=$(fixture bad-yaml)
printf 'name: CI\non: [push\n' > "$root/.github/workflows/ci.yaml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" 'is not valid YAML')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

summary "check-action-pins.sh"
