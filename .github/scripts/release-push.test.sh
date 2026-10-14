#!/usr/bin/env bash
# Tests for `release-push.sh push-main`: the version-bump push and its
# same-package race resolution (LEGION-75), and its re-run cases; and for
# `release-push.sh tagged`, which finds the tag an earlier attempt pushed.
#
# Each case builds a bare origin holding one seed commit (packages/pkg at
# 1.0.0 plus a README) and two clones, a and b. Clone a plays the release run
# whose bump commit already landed on main; clone b plays the next run of the
# same package, whose checkout predates that bump — so b's push is rejected
# and its rebase meets a's change on the same "version" line. In the re-run
# cases, b plays a later attempt of a's run or of an older run. A pre-receive
# hook that always rejects forces the exhaustion case. The `tagged` case builds
# the commits and tags release runs leave in clone a and reads them from
# several checkouts.
#
# Run from anywhere: .github/scripts/release-push.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
# Hermetic: the developer's global/system git config (commit.gpgsign,
# core.hooksPath, rerere, rebase.*) must not reach the seed repos or the script
# under test. Needs git >= 2.32.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
release_push="$script_dir/release-push.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

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

# tagged <clone> <package-dir> <tag-prefix>: runs the script under test's
# `tagged` from <clone>; leaves its exit code in rc and its output in out.
tagged() {
  local clone=$1 package_dir=$2 prefix=$3
  set +e
  out=$(cd "$clone" && "$release_push" tagged "$package_dir" "$prefix" 2>&1)
  rc=$?
  set -e
}

origin_version() { git -C "$1/origin.git" show main:packages/pkg/package.json | jq -r .version; }
origin_tip_subject() { git -C "$1/origin.git" log -1 --format=%s main; }
release_commits() { git -C "$1/origin.git" log --format=%s main | { grep -c '^chore: release' || true; }; }
not_rebasing() { [ ! -d "$1/.git/rebase-merge" ] && [ ! -d "$1/.git/rebase-apply" ] && echo true || echo false; }

echo "Case 1: two releases of one package race on the version line"
setup "$work/c1"
# a's bump also changes another key, so the resolution must keep main's copy of
# the file (HEAD at the rebase stop), not just replay b's version line.
jq '.description = "set on main by a"' "$work/c1/a/packages/pkg/package.json" > "$work/c1/a/tmp.json" \
  && mv "$work/c1/a/tmp.json" "$work/c1/a/packages/pkg/package.json"
bump "$work/c1/a" 1.0.1
git -C "$work/c1/a" push -q origin main
bump "$work/c1/b" 1.0.2
push_main "$work/c1/b" 1.0.2
check "push-main exits 0" "$(is "$rc" 0)"
check "origin/main holds the higher version" "$(is "$(origin_version "$work/c1")" 1.0.2)"
check "origin/main keeps main's other change to the manifest" "$(is "$(git -C "$work/c1/origin.git" show main:packages/pkg/package.json | jq -r .description)" "set on main by a")"
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
echo "Case 5: an unreachable origin fails through the same message"
setup "$work/c5"
bump "$work/c5/b" 1.0.1
git -C "$work/c5/b" remote set-url origin "$work/c5/nowhere.git"
push_main "$work/c5/b" 1.0.1 "$work/c5/summary.md"
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error names the fetch" "$(contains "$out" '^::error::Fetching main from origin failed on attempt 1\.')"
check "error says the release itself succeeded" "$(contains "$out" '^::error::.*The release itself succeeded')"
check "job summary says the same" "$(contains "$(cat "$work/c5/summary.md" 2>/dev/null)" 'The release itself succeeded.*only this version-bump push to main is outstanding')"
check "clone is not left mid-rebase" "$(not_rebasing "$work/c5/b")"

echo
echo "Case 6: the manifest deleted on main is a real divergence, not resolved"
setup "$work/c6"
git -C "$work/c6/a" rm -q -r packages/pkg
git -C "$work/c6/a" commit -q -m "a: remove pkg"
git -C "$work/c6/a" push -q origin main
bump "$work/c6/b" 1.0.1
push_main "$work/c6/b" 1.0.1
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error names the manifest as the conflict" "$(contains "$out" '^::error::.*conflicts in: packages/pkg/package\.json\.')"
check "rebase was aborted" "$(not_rebasing "$work/c6/b")"
check "clone's manifest is intact, not truncated" "$(is "$(jq -r .version "$work/c6/b/packages/pkg/package.json")" 1.0.1)"
check "origin/main is untouched" "$(is "$(origin_tip_subject "$work/c6")" "a: remove pkg")"

echo
echo "Case 7: a re-run whose first attempt already landed its bump pushes nothing"
setup "$work/c7"
bump "$work/c7/a" 1.0.1
git -C "$work/c7/a" push -q origin main
# The re-run recommits the same bump at a later time, so it is a commit of its own.
GIT_AUTHOR_DATE="2001-01-01T00:00:00Z" GIT_COMMITTER_DATE="2001-01-01T00:00:00Z" bump "$work/c7/b" 1.0.1
check "the re-run's bump is a different commit from main's" "$([ "$(git -C "$work/c7/b" rev-parse HEAD)" != "$(git -C "$work/c7/origin.git" rev-parse main)" ] && echo true || echo false)"
push_main "$work/c7/b" 1.0.1 "$work/c7/summary.md"
check "push-main exits 0" "$(is "$rc" 0)"
check "notice says main already holds the version" "$(contains "$out" "^::notice title=Version bump not pushed::main's packages/pkg/package.json already holds v1\.0\.1")"
check "origin/main holds the version" "$(is "$(origin_version "$work/c7")" 1.0.1)"
check "origin/main has one bump commit, not two" "$(is "$(release_commits "$work/c7")" 1)"
check "clone is not left mid-rebase" "$(not_rebasing "$work/c7/b")"

echo
echo "Case 8: a re-run after a later release of the package landed never lowers main's version"
setup "$work/c8"
bump "$work/c8/a" 1.0.2
git -C "$work/c8/a" push -q origin main
bump "$work/c8/b" 1.0.1
push_main "$work/c8/b" 1.0.1 "$work/c8/summary.md"
check "push-main exits 0" "$(is "$rc" 0)"
check "notice names the later release" "$(contains "$out" "^::notice title=Version bump not pushed::main's packages/pkg/package.json holds v1\.0\.2, a later release than v1\.0\.1")"
check "job summary says the bump was not pushed" "$(contains "$(cat "$work/c8/summary.md" 2>/dev/null)" 'Version bump for packages/pkg v1.0.1 not pushed')"
check "origin/main keeps the later version" "$(is "$(origin_version "$work/c8")" 1.0.2)"
check "origin/main has only the later release's bump commit" "$(is "$(release_commits "$work/c8")" 1)"
check "clone is not left mid-rebase" "$(not_rebasing "$work/c8/b")"

echo
echo "Case 9: tagged finds the tag an earlier attempt pushed for this checkout, and only that"
setup "$work/c9"
clone="$work/c9/a"
seed=$(git -C "$clone" rev-parse HEAD)
tagged "$clone" packages/pkg pkg-v
check "no tag: exits 0" "$(is "$rc" 0)"
check "no tag: prints nothing" "$(is "$out" "")"
# What one release run leaves: its bump commit, a child of the checkout, tagged.
bump "$clone" 1.0.1
git -C "$clone" tag pkg-v1.0.1
# Another package's release from the same checkout: a sibling bump, tagged with its own prefix.
git -C "$clone" checkout -q --detach "$seed"
mkdir -p "$clone/packages/other"
printf '{\n  "name": "other",\n  "version": "2.0.1"\n}\n' > "$clone/packages/other/package.json"
git -C "$clone" add -A
git -C "$clone" commit -q -m "chore: release other v2.0.1 [skip ci]"
git -C "$clone" tag other-v2.0.1
git -C "$clone" checkout -q --detach "$seed"
tagged "$clone" packages/pkg pkg-v
check "the tag on HEAD's bump commit: printed" "$(is "$out" pkg-v1.0.1)"
tagged "$clone" packages/other other-v
check "each prefix finds its own package's tag" "$(is "$out" other-v2.0.1)"
# A later release: main moves past the first bump, and the next run tags its own bump.
git -C "$clone" checkout -q --detach pkg-v1.0.1
printf 'later\n' > "$clone/README.md"
git -C "$clone" commit -q -am "fix: later change"
later=$(git -C "$clone" rev-parse HEAD)
bump "$clone" 1.0.2
git -C "$clone" tag pkg-v1.0.2
git -C "$clone" checkout -q --detach "$seed"
tagged "$clone" packages/pkg pkg-v
check "a later release on a descendant is not this checkout's" "$(is "$out" pkg-v1.0.1)"
git -C "$clone" checkout -q --detach "$later"
tagged "$clone" packages/pkg pkg-v
check "the later run's checkout finds its own tag" "$(is "$out" pkg-v1.0.2)"
# A tag on a child of HEAD that changes more than the manifest is not a bump commit.
git -C "$clone" checkout -q --detach "$seed"
printf 'not a bump\n' > "$clone/README.md"
jq '.version = "1.0.9"' "$clone/packages/pkg/package.json" > "$clone/tmp.json" \
  && mv "$clone/tmp.json" "$clone/packages/pkg/package.json"
git -C "$clone" commit -q -am "feat: a child that is not a bump"
git -C "$clone" tag pkg-v1.0.9
git -C "$clone" checkout -q --detach "$seed"
tagged "$clone" packages/pkg pkg-v
check "a tagged child changing another file is skipped" "$(is "$out" pkg-v1.0.1)"
# The manifest already held the version, so `tag` tagged HEAD itself.
git -C "$work/c9/b" tag pkg-v1.0.0
tagged "$work/c9/b" packages/pkg pkg-v
check "a tag on HEAD itself: printed" "$(is "$out" pkg-v1.0.0)"

summary "release-push.sh push-main and tagged"

