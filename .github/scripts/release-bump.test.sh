#!/usr/bin/env bash
# Tests for `release-bump.sh`: the version it computes for each kind of commit, and its refusal
# of a previous version that is not a plain X.Y.Z, each field a number without leading zeros.
# That version comes from a release tag's name, which anyone who can push a tag chooses, so each
# refusal case is a name an attacker could pick.
#
# One repository holds a linear history, one commit per kind, each touching pkg/:
#   seed (tag pkg-v1.2.3) -> fix -> feat -> feat! -> docs -> other (touches other/ only)
# and each case runs the script under test over a range of it. The refusal cases use the
# breaking-change range, where the script computes the major version with shell arithmetic,
# which runs a command in the subscript of an array the script has set (`bump[$(…)]`); without
# the refusal, that payload runs and comes back as the version. Each case checks nothing ran.
# The leading-zero case uses the feature range, where `1.010.0` would read its minor field as
# octal 8 and come back as `1.9.0`, lower than the version it bumps.
#
# Run from anywhere: .github/scripts/release-bump.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
# Hermetic: the developer's global/system git config (commit.gpgsign, core.hooksPath) must not
# reach the fixture repository.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
release_bump="$script_dir/release-bump.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

repo="$work/repo"
git init -q -b main "$repo"
git -C "$repo" config user.name "release test"
git -C "$repo" config user.email "release-test@example.com"
mkdir -p "$repo/pkg" "$repo/other"

# commit <dir> <message>: one commit changing a file in <dir>; echoes its hash.
commit() {
  printf '%s\n' "$2" >> "$repo/$1/file"
  git -C "$repo" add -A
  git -C "$repo" commit -q -m "$2"
  git -C "$repo" rev-parse HEAD
}
seed=$(commit pkg "chore: seed")
git -C "$repo" tag pkg-v1.2.3 "$seed"
fix=$(commit pkg "fix: one")
feat=$(commit pkg "feat: two")
breaking=$(commit pkg "feat!: three")
docs=$(commit pkg "docs: four")
other=$(commit other "fix: elsewhere")

# bump <prev-version> <range>: runs the script under test in the repository over pkg/; leaves
# its exit code in rc, its stdout in out and its stderr in err.
bump() {
  set +e
  out=$(cd "$repo" && "$release_bump" "$1" "$2" -- pkg/ 2>"$work/err")
  rc=$?
  set -e
  err=$(<"$work/err")
}

echo "Case 1: each kind of commit bumps as before"
bump 1.2.3 "pkg-v1.2.3..$fix"
check "fix: patch" "$(is "$rc $out" "0 1.2.4")"
bump 1.2.3 "$fix..$feat"
check "feat: minor" "$(is "$rc $out" "0 1.3.0")"
bump 1.2.3 "$feat..$breaking"
check "feat!: major" "$(is "$rc $out" "0 2.0.0")"
bump 1.2.3 "$breaking..$docs"
check "docs: none" "$(is "$rc $out" "0 none")"
bump 1.2.3 "$docs..$other"
check "no commit touching pkg/: empty" "$(is "$rc $out" "0 empty")"
bump 0.0.9 "pkg-v1.2.3..$fix"
check "a zero field and a field past 9 are plain: fix bumps 0.0.9 to 0.0.10" "$(is "$rc $out" "0 0.0.10")"

echo
echo "Case 2: a previous version that is not X.Y.Z is refused, and nothing in it runs"
refused() { # refused <description> <prev-version> [<range>, the breaking-change one by default]
  local range=${3:-$feat..$breaking}
  rm -f "$repo/pwned"
  bump "$2" "$range"
  check "$1: exits 2" "$(is "$rc" 2)"
  check "$1: prints no version" "$(is "$out" "")"
  check "$1: the error names the version and the range" "$(contains "$err" "^::error::release-bump.sh: previous version .* (range ${range//./\\.}) is not a plain X\.Y\.Z version; refusing it$")"
  check "$1: stderr is that one line" "$([ -n "$err" ] && [[ $err != *$'\n'* ]] && echo true || echo false)"
  check "$1: nothing in it ran" "$([ ! -e "$repo/pwned" ] && echo true || echo false)"
}
# shellcheck disable=SC2016 # literal $(…): the payload must reach the script unexpanded
refused 'a command substitution' '1.2.3$(id>pwned)'
# shellcheck disable=SC2016 # literal $(…): the payload must reach the script unexpanded
refused 'a command in an array subscript' 'bump[$(id>pwned)].0.0'
refused 'a space' '1.2.3 x'
refused 'a newline' $'1.2.3\nx'
refused 'a pre-release' '1.2.3-rc.1'
refused 'two fields' '1.2'
refused 'a leading zero, read as octal' '1.010.0' "$fix..$feat"
refused 'a leading zero in the major field' '01.2.3'

summary "release-bump.sh"
