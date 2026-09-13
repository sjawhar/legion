#!/usr/bin/env bash
# Tests for `release-push.sh push-main`: the version-bump push and its
# same-package race resolution (LEGION-75).
#
# Each case builds a bare origin holding one seed commit (packages/pkg at
# 1.0.0 plus a README) and two clones, a and b. Clone a plays the release run
# whose bump commit already landed on main; clone b plays the next run of the
# same package, whose checkout predates that bump — so b's push is rejected
# and its rebase meets a's change on the same "version" line. A pre-receive
# hook that always rejects forces the exhaustion case.
#
# Run from anywhere: .github/scripts/release-push.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
release_push="$script_dir/release-push.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

PASS=0
FAIL=0

check() {
  local desc="$1" result="$2"
  if [ "$result" = "true" ]; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc"
    FAIL=$((FAIL + 1))
  fi
}

# is <actual> <expected>: true/false for check.
is() { [ "$1" = "$2" ] && echo true || echo false; }
# contains <text> <regex>: true/false for check; greps the whole text.
contains() { printf '%s\n' "$1" | grep -q -- "$2" && echo true || echo false; }

identity() {
  git -C "$1" config user.name "release test"
  git -C "$1" config user.email "release-test@example.com"
}

# setup <dir>: bare origin at <dir>/origin.git holding the seed commit, and
# clones <dir>/a and <dir>/b of it, both still at the seed commit.
setup() {
  local dir=$1
  mkdir -p "$dir/seed/packages/pkg"
  git init -q -b main "$dir/seed"
  identity "$dir/seed"
  printf '{\n  "name": "pkg",\n  "version": "1.0.0"\n}\n' > "$dir/seed/packages/pkg/package.json"
  printf 'seed line\n' > "$dir/seed/README.md"
  git -C "$dir/seed" add -A
  git -C "$dir/seed" commit -q -m "seed"
  git clone -q --bare "$dir/seed" "$dir/origin.git"
  git clone -q "$dir/origin.git" "$dir/a"
  git clone -q "$dir/origin.git" "$dir/b"
  identity "$dir/a"
  identity "$dir/b"
}

# bump <clone> <version>: what the workflow's "Set release version" step and
# `release-push.sh tag` do together — rewrite .version with jq and commit it
# (together with anything the case already staged).
bump() {
  local clone=$1 v=$2
  jq --arg v "$v" '.version = $v' "$clone/packages/pkg/package.json" > "$clone/tmp.json" \
    && mv "$clone/tmp.json" "$clone/packages/pkg/package.json"
  git -C "$clone" add packages/pkg/package.json
  git -C "$clone" commit -q -m "chore: release pkg v$v [skip ci]"
}

# push_main <clone> <version> [summary-file]: runs the script under test from
# <clone>; leaves its exit code in rc and its combined output in out.
push_main() {
  local clone=$1 v=$2 summary=${3:-}
  set +e
  out=$(cd "$clone" && GITHUB_STEP_SUMMARY="$summary" "$release_push" push-main packages/pkg "$v" 2>&1)
  rc=$?
  set -e
}

origin_version() { git -C "$1/origin.git" show main:packages/pkg/package.json | jq -r .version; }
origin_tip_subject() { git -C "$1/origin.git" log -1 --format=%s main; }
release_commits() { git -C "$1/origin.git" log --format=%s main | { grep -c '^chore: release' || true; }; }
not_rebasing() { [ ! -d "$1/.git/rebase-merge" ] && [ ! -d "$1/.git/rebase-apply" ] && echo true || echo false; }

echo "Case 1: two releases of one package race on the version line"
setup "$work/c1"
bump "$work/c1/a" 1.0.1
git -C "$work/c1/a" push -q origin main
bump "$work/c1/b" 1.0.2
push_main "$work/c1/b" 1.0.2
check "push-main exits 0" "$(is "$rc" 0)"
check "origin/main holds the higher version" "$(is "$(origin_version "$work/c1")" 1.0.2)"
check "origin/main has one bump commit per release" "$(is "$(release_commits "$work/c1")" 2)"
check "the rebased bump commit keeps its message" "$(is "$(origin_tip_subject "$work/c1")" "chore: release pkg v1.0.2 [skip ci]")"
check "clone is not left mid-rebase" "$(not_rebasing "$work/c1/b")"

echo
echo "Case 2: a conflict in another file fails naming it"
setup "$work/c2"
printf 'from a\n' > "$work/c2/a/README.md"
git -C "$work/c2/a" commit -q -am "a: readme"
git -C "$work/c2/a" push -q origin main
printf 'from b\n' > "$work/c2/b/README.md"
git -C "$work/c2/b" commit -q -am "b: readme"
bump "$work/c2/b" 1.0.1
push_main "$work/c2/b" 1.0.1
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error names README.md as the conflict" "$(contains "$out" '^::error::.*conflicts in: README\.md\.')"
check "origin/main manifest is untouched" "$(is "$(origin_version "$work/c2")" 1.0.0)"
check "rebase was aborted" "$(not_rebasing "$work/c2/b")"

echo
echo "Case 2b: the manifest conflicting alongside another file is not resolved"
setup "$work/c2b"
printf 'from a\n' > "$work/c2b/a/README.md"
git -C "$work/c2b/a" add README.md
bump "$work/c2b/a" 1.0.1
git -C "$work/c2b/a" push -q origin main
printf 'from b\n' > "$work/c2b/b/README.md"
git -C "$work/c2b/b" add README.md
bump "$work/c2b/b" 1.0.2
push_main "$work/c2b/b" 1.0.2
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error names both conflicted paths" "$(contains "$out" '^::error::.*conflicts in: README\.md packages/pkg/package\.json\.')"
check "origin/main keeps the landed version" "$(is "$(origin_version "$work/c2b")" 1.0.1)"
check "rebase was aborted" "$(not_rebasing "$work/c2b/b")"

echo
echo "Case 3: five rejections fail, saying the release succeeded and only the push is outstanding"
setup "$work/c3"
printf '#!/bin/sh\necho "rejected by test hook" >&2\nexit 1\n' > "$work/c3/origin.git/hooks/pre-receive"
chmod +x "$work/c3/origin.git/hooks/pre-receive"
bump "$work/c3/b" 1.0.1
push_main "$work/c3/b" 1.0.1 "$work/c3/summary.md"
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "push was attempted exactly five times" "$(is "$(printf '%s\n' "$out" | grep -c 'rejected by test hook')" 5)"
check "error says the release itself succeeded" "$(contains "$out" '^::error::.*The release itself succeeded')"
check "error says only the version-bump push is outstanding" "$(contains "$out" 'only this version-bump push to main is outstanding')"
check "job summary says the same" "$(contains "$(cat "$work/c3/summary.md" 2>/dev/null)" 'The release itself succeeded.*only this version-bump push to main is outstanding')"
check "origin/main manifest is untouched" "$(is "$(origin_version "$work/c3")" 1.0.0)"

echo
echo "Case 4: a release of another package racing in still rebases cleanly"
setup "$work/c4"
mkdir -p "$work/c4/a/packages/other"
printf '{\n  "name": "other",\n  "version": "2.0.1"\n}\n' > "$work/c4/a/packages/other/package.json"
git -C "$work/c4/a" add -A
git -C "$work/c4/a" commit -q -m "chore: release other v2.0.1 [skip ci]"
git -C "$work/c4/a" push -q origin main
bump "$work/c4/b" 1.0.1
push_main "$work/c4/b" 1.0.1
check "push-main exits 0" "$(is "$rc" 0)"
check "origin/main holds the bump" "$(is "$(origin_version "$work/c4")" 1.0.1)"
check "origin/main has both release commits" "$(is "$(release_commits "$work/c4")" 2)"

echo
echo "---"
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
echo "PASS: release-push.sh push-main"

