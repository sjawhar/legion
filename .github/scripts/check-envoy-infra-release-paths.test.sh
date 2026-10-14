#!/usr/bin/env bash
# Tests for `check-envoy-infra-release-paths.sh`: release.yaml's envoy_infra push paths, filter and
# bump paths must cover every package of the repository's own Go modules the plugin compiles in,
# for each target the target list names, and the module and workspace files that set its versions.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs. The miniature holds a Go workspace
# shaped like the real one (packages/envoy and its nested packages/envoy/infra module, bound by
# infra's go.work) whose packages import only the standard library, so `go list` reads it with no
# download.
#
# Run from anywhere: .github/scripts/check-envoy-infra-release-paths.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test), after its Go install.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-envoy-infra-release-paths.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes. The plugin's command imports core, which imports two
# packages of packages/envoy (bus and contracts); a third (other) is imported by nothing. The
# targets are linux/amd64 and darwin/arm64. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  local envoy="$root/packages/envoy" infra="$root/packages/envoy/infra"
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$envoy/internal/bus" \
    "$envoy/internal/contracts" "$envoy/internal/other" "$infra/cmd/pulumi-resource-envoy-infra" \
    "$infra/core"
  ln -s "$check_script" "$root/.github/scripts/check-envoy-infra-release-paths.sh"
  printf '{\n  "envoy-infra": ["linux/amd64", "darwin/arm64"]\n}\n' > "$root/.github/envoy-infra-targets.json"
  printf 'module example.com/envoy\n\ngo 1.22\n' > "$envoy/go.mod"
  : > "$envoy/go.sum"
  printf 'package bus\n\nconst Subject = "bus"\n' > "$envoy/internal/bus/bus.go"
  printf 'package contracts\n\nconst Version = 1\n' > "$envoy/internal/contracts/contracts.go"
  printf 'package other\n\nconst Name = "other"\n' > "$envoy/internal/other/other.go"
  printf 'module example.com/envoy/infra\n\ngo 1.22\n' > "$infra/go.mod"
  : > "$infra/go.sum"
  printf 'go 1.22\n\nuse (\n\t.\n\t..\n)\n' > "$infra/go.work"
  : > "$infra/go.work.sum"
  cat > "$infra/core/core.go" <<'GO'
package core

import (
	"example.com/envoy/internal/bus"
	"example.com/envoy/internal/contracts"
)

func Describe() string { return bus.Subject + string(rune('0'+contracts.Version)) }
GO
  cat > "$infra/cmd/pulumi-resource-envoy-infra/main.go" <<'GO'
package main

import "example.com/envoy/infra/core"

func main() { println(core.Describe()) }
GO
  cat > "$root/.github/workflows/release.yaml" <<'YAML'
name: Release Packages
on:
  push:
    branches: [main]
    paths:
      - "packages/envoy/**"
      - ".github/envoy-infra-targets.json"
jobs:
  changes:
    runs-on: ubuntu-24.04
    steps:
      - uses: dorny/paths-filter@v4
        id: filter
        with:
          filters: |
            envoy_infra:
              - 'packages/envoy/infra/**'
              - 'packages/envoy/internal/bus/**'
              - 'packages/envoy/internal/contracts/**'
              - 'packages/envoy/go.mod'
              - 'packages/envoy/go.sum'
              - '.github/envoy-infra-targets.json'
  envoy_infra:
    runs-on: ubuntu-24.04
    steps:
      - name: Determine next version
        id: version
        run: |
          # A comment that names release-bump.sh (as this one does) is no call.
          VERSION=$(.github/scripts/release-bump.sh "$PREV_VERSION" "${PREV_TAG}..HEAD" \
            --patch-path .github/envoy-infra-targets.json \
            -- packages/envoy/infra/ packages/envoy/internal/bus/ packages/envoy/internal/contracts/ \
            packages/envoy/go.mod packages/envoy/go.sum .github/envoy-infra-targets.json)
YAML
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-envoy-infra-release-paths.sh" 2>&1)
  status=$?
  set -e
}

# edit <root> <old> <new>: replaces the one occurrence of <old> in the fixture's release.yaml, and
# fails the harness when it is not there exactly once, so a case never runs on an unchanged tree.
edit() {
  python3 - "$1/.github/workflows/release.yaml" "$2" "$3" <<'PY'
import sys
from pathlib import Path

path, old, new = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
text = path.read_text()
if text.count(old) != 1:
    sys.exit(f"edit: {old!r} occurs {text.count(old)} times in {path}")
path.write_text(text.replace(old, new))
PY
}

filter_miss="is missing from the changes job's envoy_infra filter"
bump_miss="is missing from the paths after -- of envoy_infra's release-bump.sh call"
push_miss="is missing from the workflow's push paths"

echo "case: every list covers what the plugin builds from"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it covered" \
  "$(contains "$out" 'cover the 10 packages and module files above, which the plugin builds from across 2 targets')"
check "lists a package of packages/envoy the plugin compiles in" \
  "$(contains "$out" '  packages/envoy/internal/bus/ (example.com/envoy/internal/bus)')"
check "lists the workspace file" "$(contains "$out" '  packages/envoy/infra/go.work (the workspace file)')"
check "lists a module's checksums" "$(contains "$out" "  packages/envoy/go.sum (example.com/envoy's go.sum)")"
check "leaves out a package nothing imports" "$(is "$(contains "$out" 'internal/other')" false)"

echo "case: a package the plugin compiles in, missing from the filter"
root=$(fixture filter-missing)
edit "$root" "              - 'packages/envoy/internal/bus/**'"$'\n' ""
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package and the filter" \
  "$(contains "$out" "packages/envoy/internal/bus/ (example.com/envoy/internal/bus) $filter_miss")"
check "blames no other list" "$(is "$(contains "$out" "$bump_miss\|$push_miss")" false)"

echo "case: a package the plugin compiles in, missing from the bump paths"
root=$(fixture bump-missing)
edit "$root" " packages/envoy/internal/contracts/ \\" " \\"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package and the bump paths" \
  "$(contains "$out" "packages/envoy/internal/contracts/ (example.com/envoy/internal/contracts) $bump_miss")"
check "blames no other list" "$(is "$(contains "$out" "$filter_miss\|$push_miss")" false)"

echo "case: the push paths miss the module"
root=$(fixture push-missing)
edit "$root" '      - "packages/envoy/**"' '      - "packages/envoy/cmd/**"'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names a package" \
  "$(contains "$out" "packages/envoy/infra/core/ (example.com/envoy/infra/core) $push_miss")"
check "names a module file" "$(contains "$out" "packages/envoy/infra/go.mod (example.com/envoy/infra's go.mod) $push_miss")"
check "names every one of the 10" "$(is "$(grep -c -- "$push_miss" <<<"$out" || true)" 10)"

echo "case: a module file, missing from the filter"
root=$(fixture gomod-missing)
edit "$root" "              - 'packages/envoy/go.mod'"$'\n' ""
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the module's go.mod" \
  "$(contains "$out" "packages/envoy/go.mod (example.com/envoy's go.mod) $filter_miss")"

echo "case: a new import on one target's platform only"
root=$(fixture darwin-import)
printf '//go:build darwin\n\npackage core\n\nimport _ "example.com/envoy/internal/other"\n' \
  > "$root/packages/envoy/infra/core/other_darwin.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package missing from the filter" \
  "$(contains "$out" "packages/envoy/internal/other/ (example.com/envoy/internal/other) $filter_miss")"
check "names it missing from the bump paths" \
  "$(contains "$out" "packages/envoy/internal/other/ (example.com/envoy/internal/other) $bump_miss")"
check "the push paths' packages/envoy/** already covers it" "$(is "$(contains "$out" "$push_miss")" false)"

echo "case: a new import on a platform no target names"
root=$(fixture windows-import)
printf '//go:build windows\n\npackage core\n\nimport _ "example.com/envoy/internal/other"\n' \
  > "$root/packages/envoy/infra/core/other_windows.go"
run_check "$root"
check "exits 0: the targets decide which files are compiled in" "$(is "$status" 0)"

echo "case: an entry the check cannot read"
root=$(fixture negated-filter)
edit "$root" "              - 'packages/envoy/go.mod'" \
  "              - 'packages/envoy/go.mod'"$'\n'"              - '!packages/envoy/infra/testdata/**'"
run_check "$root"
check "a negation in the filter fails" "$(is "$status" 1)"
check "names the entry" \
  "$(contains "$out" "the changes job's envoy_infra filter holds '!packages/envoy/infra/testdata/\*\*', which this check cannot read")"

root=$(fixture pathspec-bump)
edit "$root" "-- packages/envoy/infra/ " "-- ':(exclude)packages/envoy/infra/testdata' packages/envoy/infra/ "
run_check "$root"
check "a pathspec magic in the bump paths fails" "$(is "$status" 1)"
check "names the entry" "$(contains "$out" "holds ':(exclude)packages/envoy/infra/testdata', which this check cannot read")"

root=$(fixture glob-push)
edit "$root" '      - "packages/envoy/**"' '      - "packages/envoy/*/go.mod"'
run_check "$root"
check "a glob inside a push path fails" "$(is "$status" 1)"
check "names the entry" "$(contains "$out" "the workflow's push paths holds 'packages/envoy/\*/go.mod'")"

echo "case: a workflow the check cannot find its lists in"
root=$(fixture no-filter)
edit "$root" "            envoy_infra:" "            envoy_infra_renamed:"
run_check "$root"
check "a missing filter fails" "$(is "$status" 1)"
check "says so" "$(contains "$out" 'has no envoy_infra filter')"

root=$(fixture two-calls)
edit "$root" "packages/envoy/go.mod packages/envoy/go.sum .github/envoy-infra-targets.json)" \
  "packages/envoy/go.mod packages/envoy/go.sum .github/envoy-infra-targets.json)"$'\n'"          OTHER=\$(.github/scripts/release-bump.sh 0.0.0 HEAD -- packages/envoy/)"
run_check "$root"
check "a second release-bump.sh call fails" "$(is "$status" 1)"
check "counts them" "$(contains "$out" 'expected one release-bump.sh call in envoy_infra')"

root=$(fixture no-version-step)
edit "$root" "        id: version" "        id: versions"
run_check "$root"
check "a version step under another id fails" "$(is "$status" 1)"
check "names the step it wants" "$(contains "$out" 'expected one step with id version in job envoy_infra, found 0')"

summary "check-envoy-infra-release-paths.sh"
