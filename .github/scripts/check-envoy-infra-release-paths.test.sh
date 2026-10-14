#!/usr/bin/env bash
# Tests for `check-envoy-infra-release-paths.sh`: release-envoy-infra.yaml's push paths and
# .github/envoy-infra-release-paths, the paths its version step cuts a version from, must cover
# every package of the repository's own Go modules the plugin compiles in, for each target the
# target list names, the module and workspace files that set its versions, and the paths file
# itself.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script and the path model it imports (workflow_paths.py) into it, so the code under test is the
# file CI runs. The miniature holds a Go workspace shaped like the real one (packages/envoy and its
# nested packages/envoy/infra module, bound by infra's go.work) whose packages import only the
# standard library, so `go list` reads it with no download.
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

workflow=.github/workflows/release-envoy-infra.yaml
release_paths=.github/envoy-infra-release-paths

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
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/scripts" "$envoy/internal/bus" \
    "$envoy/internal/contracts" "$envoy/internal/other" "$infra/cmd/pulumi-resource-envoy-infra" \
    "$infra/core"
  ln -s "$check_script" "$root/.github/scripts/check-envoy-infra-release-paths.sh"
  ln -s "$script_dir/workflow_paths.py" "$root/.github/scripts/workflow_paths.py"
  printf '{\n  "envoy-infra": ["linux/amd64", "darwin/arm64"]\n}\n' > "$root/.github/envoy-infra-targets.json"
  printf '#!/usr/bin/env bash\n' > "$root/scripts/go-third-party-notices.sh"
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
  cat > "$root/$release_paths" <<'PATHS'
packages/envoy/infra/
packages/envoy/internal/bus/
packages/envoy/internal/contracts/
packages/envoy/go.mod
packages/envoy/go.sum
scripts/go-third-party-notices.sh
.github/envoy-infra-targets.json
.github/envoy-infra-release-paths
PATHS
  cat > "$root/$workflow" <<'YAML'
name: Release envoy-infra
on:
  push:
    branches: [main]
    paths:
      - "packages/envoy/infra/**"
      - "packages/envoy/internal/bus/**"
      - "packages/envoy/internal/contracts/**"
      - "packages/envoy/go.mod"
      - "packages/envoy/go.sum"
      - "scripts/go-third-party-notices.sh"
      - ".github/envoy-infra-targets.json"
      - ".github/envoy-infra-release-paths"
  workflow_dispatch:
jobs:
  release:
    runs-on: ubuntu-24.04
    steps:
      - name: Determine next version
        id: version
        run: |
          # A comment that names release-bump.sh (as this one does) is no call.
          mapfile -t paths < .github/envoy-infra-release-paths
          VERSION=$(.github/scripts/release-bump.sh "$PREV_VERSION" "${PREV_TAG}..HEAD" \
            --patch-path .github/envoy-infra-targets.json \
            -- "${paths[@]}")
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

# edit <file> <old> <new>: replaces the one occurrence of <old> in a fixture's file, and fails the
# harness when it is not there exactly once, so a case never runs on an unchanged tree.
edit() {
  python3 - "$1" "$2" "$3" <<'PY'
import sys
from pathlib import Path

path, old, new = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
text = path.read_text()
if text.count(old) != 1:
    sys.exit(f"edit: {old!r} occurs {text.count(old)} times in {path}")
path.write_text(text.replace(old, new))
PY
}

push_miss="is missing from $workflow's push paths"
bump_miss="is missing from $release_paths$"

echo "case: both lists cover what the plugin builds from"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it covered" \
  "$(contains "$out" 'cover the 14 packages and files above, which the plugin builds from across 2 targets')"
check "lists a package of packages/envoy the plugin compiles in" \
  "$(contains "$out" '  packages/envoy/internal/bus/ (example.com/envoy/internal/bus)')"
check "lists the workspace file" "$(contains "$out" '  packages/envoy/infra/go.work (the workspace file)')"
check "lists a module's checksums" "$(contains "$out" "  packages/envoy/go.sum (example.com/envoy's go.sum)")"
check "lists the paths file itself" \
  "$(contains "$out" "  $release_paths (the paths a version is cut from)")"
check "leaves out a package nothing imports" "$(is "$(contains "$out" 'internal/other')" false)"
check "lists a line of the paths file the build does not name" \
  "$(contains "$out" "  scripts/go-third-party-notices.sh (a line of $release_paths)")"

echo "case: a line of the paths file, missing from the push paths"
root=$(fixture paths-line-unpushed)
edit "$root/$workflow" '      - "scripts/go-third-party-notices.sh"'$'\n' ""
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the line and the push paths" \
  "$(contains "$out" "scripts/go-third-party-notices.sh (a line of $release_paths) $push_miss")"
check "names it alone" "$(is "$(grep -c -- "$push_miss" <<<"$out" || true)" 1)"

echo "case: a line of the paths file that names nothing"
root=$(fixture paths-line-stale)
edit "$root/$release_paths" "scripts/go-third-party-notices.sh" "scripts/no-such-script.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the line" \
  "$(contains "$out" "$release_paths: the line 'scripts/no-such-script.sh' names no file or directory")"

echo "case: a package the plugin compiles in, missing from the push paths"
root=$(fixture push-missing)
edit "$root/$workflow" '      - "packages/envoy/internal/bus/**"'$'\n' ""
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package, the push paths and a file they miss" \
  "$(contains "$out" "packages/envoy/internal/bus/ (example.com/envoy/internal/bus) $push_miss: packages/envoy/internal/bus/bus.go is not covered")"
check "blames not the paths file" "$(is "$(contains "$out" "$bump_miss")" false)"

echo "case: a package the plugin compiles in, missing from the paths file"
root=$(fixture bump-missing)
edit "$root/$release_paths" "packages/envoy/internal/contracts/"$'\n' ""
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package and the paths file" \
  "$(contains "$out" "packages/envoy/internal/contracts/ (example.com/envoy/internal/contracts) $bump_miss")"
check "blames not the push paths" "$(is "$(contains "$out" "$push_miss")" false)"

echo "case: the push paths narrowed to the plugin's command"
root=$(fixture push-narrowed)
edit "$root/$workflow" '      - "packages/envoy/infra/**"' '      - "packages/envoy/infra/cmd/**"'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names a package" "$(contains "$out" "packages/envoy/infra/core/ (example.com/envoy/infra/core) $push_miss")"
check "names a module file" "$(contains "$out" "packages/envoy/infra/go.mod (example.com/envoy/infra's go.mod) $push_miss")"
check "names the directory line the glob narrows" \
  "$(contains "$out" "packages/envoy/infra/ (a line of $release_paths) $push_miss")"
check "names each of the 6 the command's glob leaves out" "$(is "$(grep -c -- "$push_miss" <<<"$out" || true)" 6)"

echo "case: a negation in the push paths, read as GitHub reads it"
root=$(fixture push-negated)
edit "$root/$workflow" '      - "packages/envoy/infra/**"' \
  '      - "packages/envoy/infra/**"'$'\n''      - "!packages/envoy/infra/core/**"'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package the negation drops" \
  "$(contains "$out" "packages/envoy/infra/core/ (example.com/envoy/infra/core) $push_miss: packages/envoy/infra/core/core.go is not covered")"
check "names the directory line, every file under which it must cover" \
  "$(contains "$out" "packages/envoy/infra/ (a line of $release_paths) $push_miss: packages/envoy/infra/core/core.go is not covered")"
check "names those two alone" "$(is "$(grep -c -- "$push_miss" <<<"$out" || true)" 2)"

echo "case: a file in a package's directory the push paths' glob leaves out"
root=$(fixture push-glob)
edit "$root/$workflow" '      - "packages/envoy/internal/bus/**"' '      - "packages/envoy/internal/bus/*.go"'
printf 'The bus.\n' > "$root/packages/envoy/internal/bus/README.md"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package and the file" \
  "$(contains "$out" "packages/envoy/internal/bus/ (example.com/envoy/internal/bus) $push_miss: packages/envoy/internal/bus/README.md is not covered")"

echo "case: a module file, missing from the push paths"
root=$(fixture gomod-missing)
edit "$root/$workflow" '      - "packages/envoy/go.mod"'$'\n' ""
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the module's go.mod" "$(contains "$out" "packages/envoy/go.mod (example.com/envoy's go.mod) $push_miss")"

echo "case: the paths file does not list itself"
root=$(fixture paths-file-unlisted)
edit "$root/$release_paths" ".github/envoy-infra-release-paths"$'\n' ""
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the paths file" \
  "$(contains "$out" "$release_paths (the paths a version is cut from) $bump_miss")"

echo "case: a new import on one target's platform only"
root=$(fixture darwin-import)
printf '//go:build darwin\n\npackage core\n\nimport _ "example.com/envoy/internal/other"\n' \
  > "$root/packages/envoy/infra/core/other_darwin.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package missing from the push paths" \
  "$(contains "$out" "packages/envoy/internal/other/ (example.com/envoy/internal/other) $push_miss")"
check "names it missing from the paths file" \
  "$(contains "$out" "packages/envoy/internal/other/ (example.com/envoy/internal/other) $bump_miss")"

echo "case: a new import on a platform no target names"
root=$(fixture windows-import)
printf '//go:build windows\n\npackage core\n\nimport _ "example.com/envoy/internal/other"\n' \
  > "$root/packages/envoy/infra/core/other_windows.go"
run_check "$root"
check "exits 0: the targets decide which files are compiled in" "$(is "$status" 0)"

echo "case: a package whose path extends a listed one"
root=$(fixture extended-path)
mkdir -p "$root/packages/envoy/internal/busx"
printf 'package busx\n\nconst Subject = "busx"\n' > "$root/packages/envoy/internal/busx/busx.go"
printf 'package core\n\nimport _ "example.com/envoy/internal/busx"\n' \
  > "$root/packages/envoy/infra/core/busx.go"
run_check "$root"
check "exits 1: internal/bus/** and internal/bus/ cover only that directory" "$(is "$status" 1)"
check "names it missing from the push paths" \
  "$(contains "$out" "packages/envoy/internal/busx/ (example.com/envoy/internal/busx) $push_miss")"
check "names it missing from the paths file" \
  "$(contains "$out" "packages/envoy/internal/busx/ (example.com/envoy/internal/busx) $bump_miss")"

echo "case: an entry the check cannot read"
root=$(fixture push-plus)
edit "$root/$workflow" '      - "packages/envoy/go.mod"' '      - "packages/envoy+/go.mod"'
run_check "$root"
check "a push path with + fails" "$(is "$status" 1)"
check "names the entry" "$(contains "$out" "the push paths hold \['packages/envoy+/go.mod'\]")"

root=$(fixture paths-pathspec)
edit "$root/$release_paths" "packages/envoy/infra/" ":(exclude)packages/envoy/infra/testdata"$'\n'"packages/envoy/infra/"
run_check "$root"
check "a pathspec magic in the paths file fails" "$(is "$status" 1)"
check "names the line" "$(contains "$out" "cannot read the line ':(exclude)packages/envoy/infra/testdata'")"

root=$(fixture paths-glob)
edit "$root/$release_paths" "packages/envoy/go.mod" "packages/envoy/*/go.mod"
run_check "$root"
check "a glob in the paths file fails" "$(is "$status" 1)"
check "names the line" "$(contains "$out" "cannot read the line 'packages/envoy/\*/go.mod'")"

root=$(fixture paths-blank)
edit "$root/$release_paths" "packages/envoy/go.mod"$'\n' "packages/envoy/go.mod"$'\n\n'
run_check "$root"
check "a blank line in the paths file fails" "$(is "$status" 1)"
check "names the line" "$(contains "$out" "cannot read the line ''")"

echo "case: a workflow the check cannot find its lists in"
root=$(fixture no-push-paths)
edit "$root/$workflow" "    paths:" "    paths-ignore:"
run_check "$root"
check "push paths missing fails" "$(is "$status" 1)"
check "says so" "$(contains "$out" 'on.push names no paths')"

root=$(fixture inline-paths)
edit "$root/$workflow" "          mapfile -t paths < .github/envoy-infra-release-paths" \
  "          paths=(packages/envoy/infra/)"
run_check "$root"
check "a version step that does not read the paths file fails" "$(is "$status" 1)"
check "says so" "$(contains "$out" "the version step does not read $release_paths")"

root=$(fixture extra-path)
# shellcheck disable=SC2016 # literal workflow text, no expansion wanted
edit "$root/$workflow" '-- "${paths[@]}")' '-- "${paths[@]}" packages/envoy/cmd/)'
run_check "$root"
check "a version step passing a path of its own fails" "$(is "$status" 1)"
check "says what it must pass" "$(contains "$out" 'the lines of .github/envoy-infra-release-paths, and no other path')"

root=$(fixture two-calls)
# shellcheck disable=SC2016 # literal workflow text, no expansion wanted
edit "$root/$workflow" '-- "${paths[@]}")' \
  '-- "${paths[@]}")'$'\n'"          OTHER=\$(.github/scripts/release-bump.sh 0.0.0 HEAD -- packages/envoy/)"
run_check "$root"
check "a second release-bump.sh call fails" "$(is "$status" 1)"
check "counts them" "$(contains "$out" 'expected one release-bump.sh call in the version step, found 2')"

root=$(fixture no-version-step)
edit "$root/$workflow" "        id: version" "        id: versions"
run_check "$root"
check "a version step under another id fails" "$(is "$status" 1)"
check "names the step it wants" "$(contains "$out" 'expected one step with id version, found 0')"

summary "check-envoy-infra-release-paths.sh"
