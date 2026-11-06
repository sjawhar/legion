#!/usr/bin/env bash
# Tests for `release-push.sh version`, the one version step every build job
# calls: the tag an earlier attempt pushed, the bump release-bump.sh computes
# since the newest tag, the npm fallback for a first release, --expect and the
# refusals; for `release-push.sh tagged`, which finds the tag an earlier
# attempt pushed and refuses one whose name is not <prefix>X.Y.Z; for
# `release-push.sh tag`, which makes the release tag on $GITHUB_SHA, commits
# nothing and pushes nothing; for `release-push.sh pair-version`, which prints
# a plugin's released version or its newest tag's and refuses a released
# version that is not X.Y.Z; for
# `release-push.sh release`, which makes a GitHub release carry the run's
# assets; for `release-push.sh release-section`, which writes one section into
# a release's body, replacing the one that heading already holds and editing
# nothing when the body already holds it; for `release-push.sh restart`, which
# leaves a lost version's commit to a later unfinished run or starts a run for
# it, bounded, against a stub gh;
# for `release-push.sh push-tag`, which pushes a tag with the deploy
# key;
# for `release-push.sh check-built`, which refuses a build no release may
# publish, without a checkout or a credential; for
# `release-push.sh extract-built`, which replaces a checkout's two built
# directories with a checked build and refuses a bad one untouched; for
# `release-push.sh handed`, which checks the version and the files a build job
# hands a publish job and prints the release tag; for `release-push.sh
# no-credential`, the build job's tripwire; for `release-push.sh fold-notes`,
# `fold-check` and `fold-orphans`, the changelog fragments; and for
# release.yaml's worker_image_release step, which needs the checkout this
# change gives its job.
#
# The `tagged`, `version` and `tag` cases each build a bare origin holding one
# seed commit (packages/pkg at 1.0.0, a bun.lock whose workspace entries hold
# packages/pkg at 1.0.0 and packages/other at 2.0.0, plus a README) and two
# clones, a and b. The `tagged` cases put the tags release runs leave on that
# commit, on a descendant of it and on a child that changes the manifest and
# the lockfile alone, and read them from several checkouts; only a tag on the
# checkout itself is its release. The `version` cases run the version step
# from each checkout, with a stub `npm` first on PATH for the first-release
# fallback, and read what `tag` leaves for a re-run and for the next commit.
# The `tag` cases run it with $GITHUB_SHA naming the clone's HEAD and check
# that the tag names that commit and nothing else moves: no commit is added,
# the manifests and the lockfile keep their blobs, the work tree stays clean
# and origin ends without the tag. A tag already on that commit, one another
# clone pushed for it since the checkout, one on another commit, a version
# that is not X.Y.Z and a $GITHUB_SHA that is not 40 lower-case hex are each
# checked too. The publish-job case runs `tag` and `push-tag` for two packages
# from one clone, as release.yaml's publish job does, each from the run's
# commit, then a re-run of both, with no commit added to either clone and
# origin's main left where it was. The pair-version cases run over a
# repository whose manifest holds an older version than its newest tag: a
# released version wins, a released version that is not X.Y.Z is refused
# rather than falling through, a package that released nothing takes its
# newest tag, and a newest tag that is no release prints nothing.
# The `release` cases run against a stub `gh` first on PATH, which answers `gh
# release view` for a release that does not exist, is a draft or is published,
# can fail an upload, an edit or a create, and logs every call it gets. The
# `release-section` cases drive that stub too: it answers `gh release view
# --json body` with the body each case leaves it and keeps what an edit's
# --notes-file held, so each case reads the body release-section would write,
# byte for byte, over bodies holding that section nowhere, in the middle, at
# the end, before another heading and already as it would be written, and over
# a content or a heading it must refuse. The
# `push-tag` cases push to a local bare origin through a URL rewrite, and two
# more reach github.com itself: one with the pinned host keys, one with a copy
# of the script whose pinned key GitHub does not hold. Those, and a comparison
# of the pinned keys with `gh api meta`, print a SKIP line and count nothing
# when the network or a gh sign-in is missing.
# The `check-built` cases pack a tar as a build job packs it (`tar -cf … -C
# <tree> dist legion`) from a tree each one may doctor, and run it from an
# empty directory outside any repository with an empty TMPDIR, checking it
# writes nothing there or beside the tars. Each refusal — a symlink, a hard
# link, a `..`, an absolute path, a member outside dist/ and legion/ or with
# another character, an executable member (a skill script among them), a
# missing bundle, manifest or agent, a tar that is not there and a second
# argument — is named. The `extract-built` case runs it from inside a checkout
# whose two directories, which nothing tracks, carry a file the build lacks:
# on a refused tar nothing changes, and on a good one the directories become
# exactly the tar's files. The `handed` cases fill a directory with the
# archives a release ships, change one thing as a compromised build job could,
# and run `handed` on it. The `no-credential`
# cases run it under `env -i` with a throwaway HOME, plant one credential at a
# time, and check it is named but never shown. The `fold-notes` cases build one
# repository holding a released package, with fragments added before, between
# and after hand-made release tags of both shapes (one on a child of the built
# commit, one on the built commit itself, and a name off <prefix>X.Y.Z), and
# check it out at each release's own commit, as that release's build job has
# it: each release renders the entries its own commits added and no others, a
# release whose commits a lower tag already holds renders no file, and a
# fragment renamed with git mv or named with a glob character keeps the release
# that first shipped it. Stdout holds the `notes=` line alone, which each build
# job appends to $GITHUB_OUTPUT. An entry that is no .yaml file is refused by
# name, by fold-notes and `fold-check` alike, and `fold-check` with no changie
# on PATH refuses once, naming the tool. The `fold-check` cases render the
# shipped packages/pi-legion/.changie.yaml's own fragments and a kind it does
# not list, which it names with what changie said. The cases that render print
# a SKIP line where changie is not on PATH; the refusals above need none, and
# run with a stub changie that fails if anything reaches it.
#
# Run from anywhere: .github/scripts/release-push.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Hermetic: the developer's global/system git config (commit.gpgsign,
# core.hooksPath, rerere, rebase.*) must not reach the seed repos or the script
# under test. Needs git >= 2.32.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
# Nor may the Oh My Pi session it runs in, if any: CI runs it in no session,
# and a session's tools act on the variables that name one (a commit made with
# OMP_SESSION_ID set can carry a session trailer, which a test reading a
# commit's exact subject would read as part of the message).
unset OMP_SESSION_ID PI_SESSION_FILE

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

# seed_lock <file>: a bun.lock as `bun install` writes one, with a workspace
# entry for packages/other at 2.0.0 and packages/pkg at 1.0.0.
seed_lock() {
  cat > "$1" <<'LOCK'
{
  "lockfileVersion": 1,
  "configVersion": 1,
  "workspaces": {
    "": {
      "name": "seed",
    },
    "packages/other": {
      "name": "other",
      "version": "2.0.0",
    },
    "packages/pkg": {
      "name": "pkg",
      "version": "1.0.0",
    },
  }
}
LOCK
}

# setup <dir>: bare origin at <dir>/origin.git holding the seed commit, and
# clones <dir>/a and <dir>/b of it, both still at the seed commit.
setup() {
  local dir=$1
  mkdir -p "$dir/seed/packages/pkg"
  git init -q -b main "$dir/seed"
  identity "$dir/seed"
  printf '{\n  "name": "pkg",\n  "version": "1.0.0"\n}\n' > "$dir/seed/packages/pkg/package.json"
  seed_lock "$dir/seed/bun.lock"
  printf 'seed line\n' > "$dir/seed/README.md"
  git -C "$dir/seed" add -A
  git -C "$dir/seed" commit -q -m "seed"
  git clone -q --bare "$dir/seed" "$dir/origin.git"
  git clone -q "$dir/origin.git" "$dir/a"
  git clone -q "$dir/origin.git" "$dir/b"
  identity "$dir/a"
  identity "$dir/b"
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

# The deploy key push-tag pushes with. The script under test writes it under
# TMPDIR; each case gives it a fresh one and checks it is left empty.
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

# tagged <clone> <tag-prefix>: runs the script under test's `tagged` from
# <clone>; leaves its exit code in rc, its stdout (the tag the version step
# reads) in out and its stderr in err.
tagged() {
  local clone=$1
  shift
  set +e
  out=$(cd "$clone" && "$release_push" tagged "$@" 2>"$work/tagged.err")
  rc=$?
  set -e
  err=$(<"$work/tagged.err")
}

echo "Case 9: tagged finds the tag an earlier attempt pushed for this checkout, and only that"
setup "$work/c9"
clone="$work/c9/a"
seed=$(git -C "$clone" rev-parse HEAD)
tagged "$clone" pkg-v
check "no tag: exits 0" "$(is "$rc" 0)"
check "no tag: prints nothing" "$(is "$out" "")"
# What one release run leaves: its tag on the commit it released, which is the commit the run
# checked out, since `tag` tags $GITHUB_SHA and commits nothing.
git -C "$clone" tag pkg-v1.0.1
tagged "$clone" pkg-v
check "the tag on HEAD: printed" "$(is "$rc $out" "0 pkg-v1.0.1")"
# Another package's release from the same commit: its own prefix's tag, on that same commit.
git -C "$clone" tag other-v2.0.1
tagged "$clone" other-v
check "each prefix finds its own package's tag" "$(is "$out" other-v2.0.1)"
tagged "$clone" pkg-v
check "another package's tag on the same commit changes nothing" "$(is "$out" pkg-v1.0.1)"
# Two releases of one package on one commit: the newer version is this checkout's.
git -C "$clone" tag pkg-v1.0.2
tagged "$clone" pkg-v
check "two tags of the prefix on HEAD: the newer version" "$(is "$out" pkg-v1.0.2)"
git -C "$clone" tag -d pkg-v1.0.2 >/dev/null
# A later release: the next run tags its own commit, which is no tag of this one.
change "$clone" packages/pkg/index.js "fix: later change"
later=$(git -C "$clone" rev-parse HEAD)
git -C "$clone" tag pkg-v1.1.0
git -C "$clone" checkout -q --detach "$seed"
tagged "$clone" pkg-v
check "a later release on a descendant is not this checkout's" "$(is "$out" pkg-v1.0.1)"
git -C "$clone" checkout -q --detach "$later"
tagged "$clone" pkg-v
check "the later run's checkout finds its own tag" "$(is "$out" pkg-v1.1.0)"
# No release commits anything any more, so a tagged child of HEAD is another commit's release,
# whatever it changes: the manifest and the lockfile alone, the shape the bump commit had, counts
# no more than any other child.
git -C "$clone" checkout -q --detach "$seed"
jq '.version = "1.0.3"' "$clone/packages/pkg/package.json" > "$clone/tmp.json" \
  && mv "$clone/tmp.json" "$clone/packages/pkg/package.json"
sed -i '/^    "packages\/pkg": {$/,/^    },$/ s/^      "version": "[^"]*",$/      "version": "1.0.3",/' "$clone/bun.lock"
git -C "$clone" commit -q -am "chore: release pkg v1.0.3 [skip ci]"
git -C "$clone" tag pkg-v1.0.3
git -C "$clone" checkout -q --detach "$seed"
tagged "$clone" pkg-v
check "a tag on a child of HEAD that changes the manifest and the lockfile alone is skipped" "$(is "$out" pkg-v1.0.1)"

echo
echo "Case 10: tagged refuses a release tag whose name is not <prefix>X.Y.Z"
setup "$work/c10"
clone="$work/c10/a"
seed=$(git -C "$clone" rev-parse HEAD)
# shellcheck disable=SC2016 # literal $(…): git accepts it in a tag name
git -C "$clone" tag 'pkg-v1.0.1$(id>pwned)'
tagged "$clone" pkg-v
check "a command substitution on HEAD: exits 1" "$(is "$rc" 1)"
check "a command substitution on HEAD: prints no tag" "$(is "$out" "")"
check "a command substitution on HEAD: the error names the tag" "$(contains "$err" '^::error::release-push.sh tagged: pkg-v1\.0\.1.*id.*pwned.*, the release tag for this checkout, is not pkg-vX\.Y\.Z; refusing it$')"
check "a command substitution on HEAD: nothing in it ran" "$([ ! -e "$clone/pwned" ] && echo true || echo false)"
# shellcheck disable=SC2016 # literal $(…), the tag's name
git -C "$clone" tag -d 'pkg-v1.0.1$(id>pwned)' >/dev/null
git -C "$clone" tag pkg-v1.0.0-rc.1
tagged "$clone" pkg-v
check "a pre-release on HEAD: exits 1" "$(is "$rc" 1)"
check "a pre-release on HEAD: prints no tag" "$(is "$out" "")"
check "a pre-release on HEAD: the error names the tag" "$(contains "$err" '^::error::release-push.sh tagged: pkg-v1\.0\.0-rc\.1, the release tag for this checkout, is not pkg-vX\.Y\.Z; refusing it$')"
git -C "$clone" tag -d pkg-v1.0.0-rc.1 >/dev/null
# A leading zero, which release-bump.sh's arithmetic would read as octal.
for name in pkg-v1.010.0 pkg-v01.0.0; do
  git -C "$clone" tag "$name"
  tagged "$clone" pkg-v
  check "a leading zero on HEAD ($name): exits 1" "$(is "$rc" 1)"
  check "a leading zero on HEAD ($name): prints no tag" "$(is "$out" "")"
  check "a leading zero on HEAD ($name): the error names the tag" "$(contains "$err" "^::error::release-push.sh tagged: ${name//./\\.}, the release tag for this checkout, is not pkg-vX\.Y\.Z; refusing it$")"
  git -C "$clone" tag -d "$name" >/dev/null
done
git -C "$clone" tag pkg-v1.0.0
tagged "$clone" pkg-v
check "a zero field is plain: pkg-v1.0.0 on HEAD, printed" "$(is "$rc $out" "0 pkg-v1.0.0")"
git -C "$clone" tag -d pkg-v1.0.0 >/dev/null
# Git refuses a tag name holding a space or a newline, and ignores a ref file
# forged with one, so `tagged` is never handed either.
for name in 'pkg-v1.0.2 x' $'pkg-v1.0.2\nx'; do
  printf -v shown '%q' "$name"
  check "git refuses to create the tag $shown" "$(git -C "$clone" tag "$name" 2>/dev/null && echo false || echo true)"
  printf '%s\n' "$seed" > "$clone/.git/refs/tags/$name"
done
tagged "$clone" pkg-v
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
# What `tag` leaves a re-run: the release tag on the commit the run released, made by the script
# under test with $GITHUB_SHA naming that commit.
vb="$work/v/b"
vb_sha=$(git -C "$vb" rev-parse HEAD)
set +e
(cd "$vb" && GITHUB_SHA="$vb_sha" "$release_push" tag 1.0.1 pkg-v1.0.1) >/dev/null 2>&1
rc=$?
set -e
check "tag on this checkout: exits 0" "$(is "$rc" 0)"
check "tag on this checkout: committed nothing, leaving the work tree as it was" \
  "$(is "$(git -C "$vb" rev-parse HEAD)|$(git -C "$vb" status --porcelain)" "$vb_sha|")"
version_run "$vb" pkg-v -- packages/pkg/
check "the tag tag made on HEAD: its version, the tag and skip=false" "$(is "$rc $out" "0 $(outputs 1.0.1 pkg-v1.0.1 false)")"
check "the tag tag made on HEAD: says it was already pushed" "$(contains "$err" '^Releasing pkg-v1\.0\.1, already pushed for this checkout$')"
# A commit after that release: the next version comes from the tag alone, since no commit of the
# release itself sits on the branch to read a version from.
change "$vb" packages/pkg/index.js "fix: a bug after the release"
version_run "$vb" pkg-v -- packages/pkg/
check "a commit after the tag: the next version from the tag alone" "$(is "$rc $out" "0 $(outputs 1.0.2 pkg-v1.0.2 false)")"
check "a commit after the tag: logs the bump from that tag" "$(contains "$err" '^Bumping pkg-v1\.0\.1 -> pkg-v1\.0\.2$')"
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
# @sjawhar/claude-envoy's own case, which release.yaml's claude_envoy_build runs: npm carries a
# 0.0.0 placeholder of the package, so npm answers and the fallback above is never reached, and
# no claude-envoy-v tag exists yet. The first release comes from the whole history of the paths,
# not from the newest commit alone: the fix: above and the feat: below are both in the range, so
# it bumps the minor rather than failing for the missing tag or skipping for an empty range.
change "$work/v/c" packages/pkg/index.js "feat: an export before the first release"
NPM_TEST_VERSION=0.0.0 version_run "$work/v/c" pkg-v --first-npm @example/pkg -- packages/pkg/
check "no tag, npm's 0.0.0 placeholder: exits 0" "$(is "$rc" 0)"
check "no tag, npm's 0.0.0 placeholder: the first release reads the whole history" \
  "$(is "$out" "$(outputs 0.1.0 pkg-v0.1.0 false)")"
check "no tag, npm's 0.0.0 placeholder: asked npm for the package's version" "$(is "$npm_calls" '[view][@example/pkg][version]')"
check "no tag, npm's 0.0.0 placeholder: logs the bump from the placeholder" "$(contains "$err" '^Bumping v0\.0\.0 -> pkg-v0\.1\.0$')"
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
echo "Case 10c: tag makes the release tag on \$GITHUB_SHA, committing nothing and pushing nothing"
# tag_run <clone> <version> <tag> [<sha>]: runs `release-push.sh tag <version> <tag>` from <clone>
# with $GITHUB_SHA naming <clone>'s HEAD, as the publish job runs it from the run's commit, or
# naming <sha> when one is given (the empty string for a $GITHUB_SHA set to nothing). Leaves its
# exit code in rc and its combined output in out.
tag_run() {
  local clone=$1 v=$2 tag=$3 sha=${4-}
  [ $# -ge 4 ] || sha=$(git -C "$clone" rev-parse HEAD)
  set +e
  out=$(cd "$clone" && env GITHUB_SHA="$sha" "$release_push" tag "$v" "$tag" 2>&1)
  rc=$?
  set -e
}
# state <clone>: what `tag` leaves as it was — HEAD, how many commits the branch holds, the blob
# of every file under packages/ and of bun.lock, and whatever the work tree and index carry.
state() {
  git -C "$1" rev-parse HEAD
  git -C "$1" rev-list --count HEAD
  git -C "$1" ls-tree -r HEAD -- packages bun.lock
  git -C "$1" status --porcelain
}
setup "$work/t"
clone="$work/t/a"
mkdir -p "$clone/packages/other"
printf '{\n  "name": "other",\n  "version": "2.0.0"\n}\n' > "$clone/packages/other/package.json"
git -C "$clone" add -A
git -C "$clone" commit -q -m "feat: add other"
git -C "$clone" push -q origin main
run=$(git -C "$clone" rev-parse HEAD)
before=$(state "$clone")
tag_run "$clone" 1.0.1 pkg-v1.0.1
check "tags: exits 0" "$(is "$rc" 0)"
check "tags: the tag names \$GITHUB_SHA" "$(is "$(git -C "$clone" rev-list -n 1 pkg-v1.0.1)" "$run")"
check "tags: no commit is added and the manifests and the lockfile are untouched" "$(is "$(state "$clone")" "$before")"
check "tags: origin has no tag" "$(is "$(git -C "$work/t/origin.git" tag -l)" "")"
check "tags: origin/main is untouched" "$(is "$(git -C "$work/t/origin.git" rev-parse main)" "$run")"
tag_run "$clone" 1.0.1 pkg-v1.0.1
check "the tag already on this commit: exits 0, reusing it" \
  "$(is "$rc|$(contains "$out" '^Tag pkg-v1\.0\.1 already exists for version 1\.0\.1; reusing it$')" '0|true')"
check "the tag already on this commit: still no commit, and still nothing on origin" \
  "$(is "$(state "$clone")|$(git -C "$work/t/origin.git" tag -l)" "$before|")"
# The tag on another commit: the run releases a commit the tag does not name.
change "$clone" packages/pkg/index.js "fix: a later commit"
later=$(git -C "$clone" rev-parse HEAD)
before=$(state "$clone")
tag_run "$clone" 1.0.1 pkg-v1.0.1
check "the tag on another commit: exits 1" "$(is "$rc" 1)"
check "the tag on another commit: the error names both commits" \
  "$(contains "$out" "^::error::release-push\.sh tag: tag pkg-v1\.0\.1 already exists on $run, not $later, the commit this run releases$")"
check "the tag on another commit: the tag still names the commit it named" \
  "$(is "$(git -C "$clone" rev-list -n 1 pkg-v1.0.1)" "$run")"
check "the tag on another commit: nothing else is tagged and nothing is committed" \
  "$(is "$(git -C "$clone" tag -l)|$(state "$clone")" "pkg-v1.0.1|$before")"
# A version that is not X.Y.Z, each refused before anything is tagged.
for v in 1.0 1.0.1-rc.1 01.0.0; do
  tag_run "$clone" "$v" "pkg-v$v"
  check "a version that is not X.Y.Z ($v): exits 1" "$(is "$rc" 1)"
  check "a version that is not X.Y.Z ($v): the error names it" \
    "$(contains "$out" "^::error::release-push.sh tag: the release version ${v//./\\.} is not X\.Y\.Z")"
  check "a version that is not X.Y.Z ($v): nothing is tagged" "$(is "$(git -C "$clone" tag -l)" pkg-v1.0.1)"
done
# A $GITHUB_SHA that is not a commit sha: empty, not hex, short, or upper-case hex.
for bad in '' not-a-sha "${run:0:12}" "${run^^}"; do
  printf -v shown '%q' "$bad"
  tag_run "$clone" 1.0.2 pkg-v1.0.2 "$bad"
  check "a \$GITHUB_SHA that is not 40 lower-case hex ($shown): exits 1" "$(is "$rc" 1)"
  check "a \$GITHUB_SHA that is not 40 lower-case hex ($shown): the error names it" \
    "$(contains "$out" '^::error::release-push.sh tag: GITHUB_SHA names the commit this run releases; ')"
  check "a \$GITHUB_SHA that is not 40 lower-case hex ($shown): nothing is tagged" "$(is "$(git -C "$clone" tag -l)" pkg-v1.0.1)"
done
set +e
out=$(cd "$clone" && env -u GITHUB_SHA "$release_push" tag 1.0.2 pkg-v1.0.2 2>&1)
rc=$?
set -e
check "an unset \$GITHUB_SHA: exits 1 naming it, with nothing tagged" \
  "$(is "$rc|$(contains "$out" '^::error::release-push.sh tag: GITHUB_SHA names the commit this run releases; ')|$(git -C "$clone" tag -l)" '1|true|pkg-v1.0.1')"
# The tags are fetched, so a tag another attempt pushed since the checkout is seen: one on this
# run's commit is reused, one on another commit is refused.
setup "$work/t2"
run=$(git -C "$work/t2/a" rev-parse HEAD)
git -C "$work/t2/b" tag pkg-v1.2.0
git -C "$work/t2/b" push -q origin pkg-v1.2.0
before=$(state "$work/t2/a")
tag_run "$work/t2/a" 1.2.0 pkg-v1.2.0
check "a tag another attempt pushed for this commit: fetched and reused" \
  "$(is "$rc|$(contains "$out" '^Tag pkg-v1\.2\.0 already exists for version 1\.2\.0; reusing it$')|$(state "$work/t2/a")" "0|true|$before")"
change "$work/t2/b" README.md "fix: another commit"
other=$(git -C "$work/t2/b" rev-parse HEAD)
git -C "$work/t2/b" tag pkg-v1.3.0
git -C "$work/t2/b" push -q origin pkg-v1.3.0
tag_run "$work/t2/a" 1.3.0 pkg-v1.3.0
check "a tag another run pushed for another commit: refused, naming both" \
  "$(is "$rc|$(contains "$out" "^::error::release-push\.sh tag: tag pkg-v1\.3\.0 already exists on $other, not $run, the commit this run releases$")" '1|true')"
check "a tag another run pushed for another commit: the clone made no tag of its own" \
  "$(is "$(git -C "$work/t2/a" tag -l)" "pkg-v1.2.0"$'\n'"pkg-v1.3.0")"
check "tag pushes nothing: origin holds only the tags the other clone pushed" \
  "$(is "$(git -C "$work/t2/origin.git" tag -l)" "pkg-v1.2.0"$'\n'"pkg-v1.3.0")"

echo
echo "Case: one publish job releases two packages from the run's commit"
# release.yaml's publish job releases its packages one after another from one checkout, each from
# the run's commit: `git checkout --force --detach "$GITHUB_SHA"` and `git clean -ffdx`, then
# `tag` and `push-tag`, here each through the script under test against a bare origin standing in
# for GitHub. The run's commit R holds a fix for each of two packages, pkg and other: each tag
# names R itself and no commit is made, so main's tip stays R, and a re-run finds each tag where
# the first attempt left it, adds no commit of its own and changes nothing on origin.
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
# publish_package <clone> <version> <tag>: one package's steps after its hand-over check, from the
# run's commit: `tag`, then `push-tag`, stopping at the first that fails. Leaves the failed step's
# name in failed (empty when every step succeeded), the output of `tag` in tag_out, rc and out
# from the last step run, and in pushed how many of its pushes reached origin.
publish_package() {
  local clone=$1 v=$2 tag=$3
  failed='' pushed=0
  tag_run "$clone" "$v" "$tag" "$run"
  tag_out=$out
  [ "$rc" = 0 ] || { failed=tag; return 0; }
  deploy_run "$clone" -- push-tag "$tag"
  [ -z "$seen" ] || pushed=$((pushed + 1))
  [ "$rc" = 0 ] || failed=push-tag
}
# publish_refs: every ref the publish-job case's origin holds, with the object it names.
publish_refs() { git -C "$pj/origin.git" for-each-ref --format='%(refname) %(objectname)'; }
o="$pj/origin.git"

# The run: its build jobs read the version from the run's commit, then publish releases both.
git clone -q "$o" "$pj/job"
version_run "$pj/job" pkg-v -- packages/pkg/
check "the build of pkg: releases 1.0.1" "$(is "$rc $out" "0 $(outputs 1.0.1 pkg-v1.0.1 false)")"
version_run "$pj/job" other-v -- packages/other/
check "the build of other: releases 2.0.1" "$(is "$rc $out" "0 $(outputs 2.0.1 other-v2.0.1 false)")"
from_run "$pj/job"
before=$(state "$pj/job")
publish_package "$pj/job" 1.0.1 pkg-v1.0.1
check "first attempt, pkg: every step succeeds, its tag pushed" "$(is "$failed|$pushed" "|1")"
from_run "$pj/job"
publish_package "$pj/job" 2.0.1 other-v2.0.1
check "first attempt, other: every step succeeds, its tag pushed" "$(is "$failed|$pushed" "|1")"
check "first attempt: each tag on origin names the run's commit" \
  "$(is "$(git -C "$o" rev-list -n 1 pkg-v1.0.1) $(git -C "$o" rev-list -n 1 other-v2.0.1)" "$run $run")"
check "first attempt: origin's main is still the run's commit" "$(is "$(git -C "$o" rev-parse main)" "$run")"
check "first attempt: the clone added no commit, and left the manifests and the lockfile as they were" \
  "$(is "$(state "$pj/job")" "$before")"
check "first attempt: the pushes went through the deploy key" "$(is "$seen" "600 key"$'\n'"600 known_hosts")"
refs=$(publish_refs)

# A re-run: the same commit, with every tag the first attempt pushed.
git clone -q "$o" "$pj/rerun"
git -C "$pj/rerun" checkout -q --detach "$run"
git -C "$pj/rerun" fetch -q --tags origin
version_run "$pj/rerun" pkg-v -- packages/pkg/
check "re-run, the build of pkg: releases pkg-v1.0.1 again" \
  "$(is "$rc|$out|$(contains "$err" '^Releasing pkg-v1\.0\.1, already pushed for this checkout$')" "0|$(outputs 1.0.1 pkg-v1.0.1 false)|true")"
version_run "$pj/rerun" other-v -- packages/other/
check "re-run, the build of other: releases other-v2.0.1 again" \
  "$(is "$rc|$out|$(contains "$err" '^Releasing other-v2\.0\.1, already pushed for this checkout$')" "0|$(outputs 2.0.1 other-v2.0.1 false)|true")"
from_run "$pj/rerun"
before=$(state "$pj/rerun")
publish_package "$pj/rerun" 1.0.1 pkg-v1.0.1
check "re-run, pkg: every step succeeds" "$(is "$failed" "")"
check "re-run, pkg: tag reuses its own tag" "$(contains "$tag_out" '^Tag pkg-v1\.0\.1 already exists for version 1\.0\.1; reusing it$')"
from_run "$pj/rerun"
publish_package "$pj/rerun" 2.0.1 other-v2.0.1
check "re-run, other: every step succeeds" "$(is "$failed" "")"
check "re-run, other: tag reuses its own tag" "$(contains "$tag_out" '^Tag other-v2\.0\.1 already exists for version 2\.0\.1; reusing it$')"
check "re-run: no commit is added to the clone" "$(is "$(state "$pj/rerun")" "$before")"
check "re-run: origin's main and tags are unchanged" "$(is "$(publish_refs)" "$refs")"

echo
echo "Case: pair-version prints a plugin's released version, else its newest tag's, and refuses a released version that is not X.Y.Z"
# A release commits nothing, so a package's committed manifest keeps whatever version was last
# written to it and the tags alone say what npm carries. release.yaml's two steps —
# legion_build's "Resolve plugin versions for the smoke test and worker image" and publish's
# "Check the plugin pair npm carries" — each work out what their own run released and hand that
# over; this command answers from the tags when they hand over nothing.
pm="$work/pair"
mkdir -p "$pm/packages/pi-envoy"
git init -q -b main "$pm"
identity "$pm"
printf '{\n  "name": "@sjawhar/pi-envoy",\n  "version": "0.9.0"\n}\n' > "$pm/packages/pi-envoy/package.json"
git -C "$pm" add -A
git -C "$pm" commit -q -m "seed"
git -C "$pm" tag pi-envoy-v1.9.0
git -C "$pm" tag pi-envoy-v1.10.0
# pair_version <arg>...: runs `release-push.sh pair-version <arg>...` in that repository; leaves
# its exit code in rc, its stdout in out and its stderr in err.
pair_version() {
  set +e
  out=$(cd "$pm" && "$release_push" pair-version "$@" 2>"$work/pair.err")
  rc=$?
  set -e
  err=$(<"$work/pair.err")
}
pair_version pi-envoy-v 1.11.0
check "a released version: that version, not the newest tag's" "$(is "$rc|$out" "0|1.11.0")"
pair_version pi-envoy-v ''
check "a package that released nothing: the newest tag's version, not the manifest's older one" \
  "$(is "$rc|$out" "0|1.10.0")"
pair_version pi-envoy-v
check "no released version at all: the same newest tag's version" "$(is "$rc|$out" "0|1.10.0")"
# A released version that is not X.Y.Z is refused rather than falling through to the newest tag:
# the caller means a release it just made, and the fall-through would link the previous plugin
# with no word said. Only an empty value falls through, which is the two steps' "this run
# released nothing".
for bad in 1.11 v1.11.0 1.11.0-rc.1 01.11.0 ' '; do
  pair_version pi-envoy-v "$bad"
  printf -v shown '%q' "$bad"
  check "a released version that is not X.Y.Z ($shown): refused, printing nothing" \
    "$(is "$rc|$out|$(contains "$err" "^::error::release-push\.sh pair-version: the released version .* is not X\.Y\.Z, each field a number without leading zeros; refusing it\$")" "1||true")"
done
# A newest tag whose name is no release, and no tag at all: each prints nothing, which the two
# steps' ::error::Could not resolve guards refuse.
git -C "$pm" tag -d pi-envoy-v1.10.0 >/dev/null
git -C "$pm" tag -d pi-envoy-v1.9.0 >/dev/null
git -C "$pm" tag pi-envoy-v1.9.0-rc.1
pair_version pi-envoy-v ''
check "the newest tag is no release: prints nothing" "$(is "$rc|$out" "0|")"
pair_version pi-envoy-v 1.11.0
check "the newest tag is no release, with a released version: that version" "$(is "$rc|$out" "0|1.11.0")"
git -C "$pm" tag -d pi-envoy-v1.9.0-rc.1 >/dev/null
pair_version pi-envoy-v ''
check "no tag of the prefix at all: prints nothing" "$(is "$rc|$out" "0|")"
pair_version
check "no tag prefix: refused, naming the usage" \
  "$(is "$rc|$out|$(contains "$err" '^::error::release-push\.sh pair-version: usage: release-push\.sh pair-version <tag-prefix> \[<released version>\]$')" "1||true")"
pair_version pi-envoy-v 1.11.0 extra
check "an argument past the released version: refused, naming the usage" \
  "$(is "$rc|$out|$(contains "$err" '^::error::release-push\.sh pair-version: usage')" "1||true")"

echo
echo "Case 11: release creates a missing release, publishes a draft, and reuses a published one"
# The stub gh, first on PATH. It logs each call to $GH_TEST_DIR/calls as one
# line, each argument in brackets, so an argument the script splits or joins
# shows up as a different log line. `gh release view <tag> ... --json isDraft
# -q .isDraft` answers for $GH_TEST_RELEASE: none (exit 1, as gh does for a
# missing release), draft or published (true or false), or odd (a value gh
# never prints); `--json body -q .body` prints $GH_TEST_DIR/body.md, the
# release's body as the case left it, for the release-section cases below. An
# upload, edit or create fails when $GH_TEST_FAIL names it, and the file an
# edit's --notes-file names is copied to $GH_TEST_DIR/edited.md, the body that
# edit would write. Any other call fails as unexpected, so a call the command
# should not make shows up as an error.
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
{ printf '[%s]' "$@"; printf '\n'; } >> "$GH_TEST_DIR/calls"
case "$1 $2" in
  "release view")
    if [[ " $* " == *" --json body -q .body "* ]]; then
      cat "$GH_TEST_DIR/body.md"
    elif [[ " $* " == *" --json isDraft -q .isDraft "* ]]; then
      case $GH_TEST_RELEASE in
        none) echo "release not found" >&2; exit 1 ;;
        draft) echo true ;;
        published) echo false ;;
        odd) echo null ;;
        *) echo "unknown GH_TEST_RELEASE $GH_TEST_RELEASE" >&2; exit 3 ;;
      esac
    else
      echo "view without --json isDraft or --json body: $*" >&2
      exit 3
    fi ;;
  "release upload" | "release edit" | "release create")
    if [ "$GH_TEST_FAIL" = "$2" ]; then
      echo "HTTP 502: the test failed this $2" >&2
      exit 1
    fi
    while [ $# -gt 1 ]; do
      [ "$1" != --notes-file ] || cp "$2" "$GH_TEST_DIR/edited.md"
      shift
    done ;;
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
echo "Case 11a: release-section writes one section into the release's body, replacing the one it holds"
# The content a changelog step hands over: markdown of its own, with "###" headings and list
# items, which must reach the body unchanged.
entries="$work/section-entries.md"
printf '### Added\n\n- an entry\n- another entry\n\n### Fixed\n\n- a bug\n' > "$entries"
: > "$work/section-stdin.md"
# section_run <arg>...: runs `release-push.sh release-section pkg-v1.0.1 <arg>...` against the
# stub, which answers the body view with $work/body.md as the case left it, and with standard
# input from $work/section-stdin.md, empty unless the case wrote it; leaves its exit code in rc,
# its combined output in out, the stub's call log in calls and the body the edit was handed in
# edited, emptied first so a case can tell that no edit came.
section_run() {
  rm -f "$work/edited.md"
  : > "$work/calls"
  set +e
  out=$(cd "$work" && GITHUB_REPOSITORY=example/repo GH_TEST_DIR="$work" GH_TEST_RELEASE=published \
    GH_TEST_FAIL='' PATH="$work/bin:$PATH" "$release_push" release-section pkg-v1.0.1 "$@" \
    < "$work/section-stdin.md" 2>&1)
  rc=$?
  set -e
  calls=$(<"$work/calls")
  edited=''
  [ ! -e "$work/edited.md" ] || edited=$(<"$work/edited.md")
}
# edited_is <kept> <heading> <content-file>: true when the body the edit was handed holds, byte
# for byte, <kept> — the body's other lines as release-section keeps them, empty for none — then
# the heading, a blank line and the content file.
edited_is() {
  { [ -z "$1" ] || printf '%s\n\n' "$1"; printf '%s\n\n' "$2"; cat "$3"; } > "$work/expected-body.md"
  cmp -s "$work/edited.md" "$work/expected-body.md" && echo true || echo false
}
body_view=$(argv release view pkg-v1.0.1 --repo example/repo --json body -q .body)
# The edit's --notes-file names a file under the command's own temporary directory, so the log
# line is read by its shape; one_edit: the body view and one edit, and nothing else.
notes_edit='^\[release\]\[edit\]\[pkg-v1\.0\.1\]\[--repo\]\[example/repo\]\[--notes-file\]\[/'
one_edit() { is "$(grep -c . <<<"$calls")|$(contains "$calls" "$notes_edit")" '2|true'; }
# A body holding no such section: the section is appended under its heading, and every other line
# is kept as it was.
printf 'Pkg v1.0.1, built from the tag.\n\n## Worker image\n\n`ghcr.io/example/worker@sha256:a1`\n' > "$work/body.md"
kept=$(<"$work/body.md")
section_run '## Changelog' "$entries"
check "no such section: exits 0, viewing the body and editing it once" "$(is "$rc|$(one_edit)" '0|true')"
check "no such section: the section is appended, the body's other lines kept byte for byte" \
  "$(edited_is "$kept" '## Changelog' "$entries")"
check "no such section: says it wrote the section" \
  "$(contains "$out" '^Wrote the ## Changelog section into the body of release pkg-v1\.0\.1$')"
check "no such section: the content's own markdown goes through unchanged" \
  "$(is "$(contains "$edited" '^### Added$')|$(contains "$edited" '^- another entry$')|$(contains "$edited" '^### Fixed$')" 'true|true|true')"
# The same body, now holding that section with other content, between two others.
printf 'Pkg v1.0.1, built from the tag.\n\n## Changelog\n\n### Added\n\n- an older entry\n\n## Worker image\n\n`ghcr.io/example/worker@sha256:a1`\n' > "$work/body.md"
section_run '## Changelog' "$entries"
check "a section already there: exits 0, viewing the body and editing it once" "$(is "$rc|$(one_edit)" '0|true')"
check "a section already there: replaced, the body's other lines in their order and the next heading kept" \
  "$(edited_is "$kept" '## Changelog' "$entries")"
check "a section already there: nothing of what it held is left" "$(is "$(contains "$edited" 'an older entry')" false)"
# The body already holding exactly what would be written, which a re-run of the step hands over
# again: nothing is edited, a difference in trailing newlines alone included.
cp "$work/expected-body.md" "$work/body.md"
section_run '## Changelog' "$entries"
check "a body already holding it: exits 0, viewing the body and editing nothing" "$(is "$rc|$calls|$edited" "0|$body_view|")"
check "a body already holding it: says the release already holds it" \
  "$(contains "$out" '^Release pkg-v1\.0\.1 already holds this ## Changelog section$')"
printf '\n\n' >> "$work/body.md"
section_run '## Changelog' "$entries"
check "a body differing in trailing newlines alone: still edits nothing" "$(is "$rc|$calls" "0|$body_view")"
# The section at the end of the body, and one followed by a heading of level one.
printf 'Lead sentence.\n\n## Changelog\n\n- an older entry\n' > "$work/body.md"
section_run '## Changelog' "$entries"
check "a section at the end of the body: replaced, the lead kept" \
  "$(is "$rc|$(edited_is 'Lead sentence.' '## Changelog' "$entries")" '0|true')"
printf 'Lead sentence.\n\n## Changelog\n\n- an older entry\n\n# Notes\n\nTail sentence.\n' > "$work/body.md"
section_run '## Changelog' "$entries"
check "a section followed by a heading of level one: that heading and what follows it survive" \
  "$(is "$rc|$(edited_is "$(printf 'Lead sentence.\n\n# Notes\n\nTail sentence.')" '## Changelog' "$entries")" '0|true')"
# The worker-image caller's shape: one line of content on standard input, under its own heading.
printf 'Pkg v1.0.1, built from the tag.\n' > "$work/body.md"
# shellcheck disable=SC2016 # literal Markdown backticks, no expansion wanted
printf '`%s`\n' 'ghcr.io/example/worker@sha256:b2' > "$work/section-stdin.md"
section_run '## Worker image' -
check "content on standard input: written under that heading" \
  "$(is "$rc|$(edited_is 'Pkg v1.0.1, built from the tag.' '## Worker image' "$work/section-stdin.md")" '0|true')"
# A content that is empty or missing, on either route: refused before the body is even read, so
# the caller's step fails instead of leaving a heading over nothing.
: > "$work/section-empty.md"
: > "$work/section-stdin.md"
section_run '## Changelog' "$work/section-empty.md"
check "an empty content file: exits 1, calling no gh" "$(is "$rc|$calls|$edited" '1||')"
check "an empty content file: the error names the file" \
  "$([[ $out == *"::error::release-push.sh release-section: nothing to write into the ## Changelog section of release pkg-v1.0.1: $work/section-empty.md holds no content"* ]] && echo true || echo false)"
section_run '## Changelog' "$work/section-missing.md"
check "a content file that is not there: exits 1, calling no gh, naming the file" \
  "$(is "$rc|$calls|$([[ $out == *"$work/section-missing.md holds no content"* ]] && echo true || echo false)" '1||true')"
section_run '## Worker image' -
check "empty standard input: exits 1, calling no gh, naming it" \
  "$(is "$rc|$calls|$([[ $out == *"release pkg-v1.0.1: standard input holds no content"* ]] && echo true || echo false)" '1||true')"
# A heading that is no level-two heading line: refused, since the awk that finds the section again
# would find nothing and a re-run would add a second copy.
for heading in '### Changelog' '# Changelog' 'Changelog' '## ' '## Changelog ' '##Changelog'; do
  printf -v shown '%q' "$heading"
  section_run "$heading" "$entries"
  check "a heading that is no level-two heading line ($shown): exits 2, calling no gh" "$(is "$rc|$calls|$edited" '2||')"
  check "a heading that is no level-two heading line ($shown): the error names it" \
    "$([[ $out == *"the heading $shown is not a level-two heading line ('## ' and a title); refusing it"* ]] && echo true || echo false)"
done
section_run '## Changelog'
check "a missing argument: exits 2, calling no gh, naming what is needed" \
  "$(is "$rc|$calls|$(contains "$out" 'a tag, a heading and a content file are needed; usage: release-push\.sh release-section <tag> <heading> <content-file>$')" '2||true')"

echo
echo "Case: release.yaml's worker_image_release names the image on the release from a checkout"
# That job's last step runs release-push.sh out of the checkout, and the job made none until this
# change, so the step exited 127 on every legion release and the release body never got its
# "## Worker image" section. The step's `run:` is read out of the workflow by name and run twice
# against the stub gh above: in a directory holding no checkout, as the job was, and in one
# holding the script where the step names it, as the job's checkout leaves it. That the job has
# the checkout is check-release-credentials.sh's rule 6, not a text match here.
workflow="$script_dir/../workflows/release.yaml"
# workflow_step_run <job> <step name>: the body of that step's `run:` block in the workflow,
# dedented, so the case runs the workflow's own code.
workflow_step_run() {
  awk -v job="  $1:" -v name="      - name: $2" '
    $0 == job { in_job = 1; next }
    in_job && /^  [A-Za-z_]+:/ { in_job = 0 }
    in_job && $0 == name { step = 1; next }
    step && /^      - / { exit }
    step && $0 == "        run: |" { body = 1; next }
    body && $0 != "" && $0 !~ /^          / { exit }
    body { sub(/^          /, ""); print }
  ' "$workflow"
}
name_step=$(workflow_step_run worker_image_release "Name the worker image on the legion release")
check "that job's last step runs release-section over the worker image reference" \
  "$(contains "$name_step" 'release-push\.sh release-section "\$RELEASE_TAG" .## Worker image. -')"
ref='ghcr.io/example/repo/worker@sha256:1111111111111111111111111111111111111111111111111111111111111111'
printf 'Generated notes\n' > "$work/body.md"
# run_name_step <dir>: that step's body in <dir>, under the shell GitHub runs a `run:` with and
# with the environment worker_image_release gives it; leaves rc, out (combined) and the stub's
# call log in calls.
run_name_step() {
  : > "$work/calls"
  rm -f "$work/edited.md"
  set +e
  out=$(cd "$1" && env GITHUB_REPOSITORY=example/repo GH_TEST_DIR="$work" GH_TEST_RELEASE=published \
    GH_TEST_FAIL='' GH_TOKEN=token REF="$ref" RELEASE_TAG=legion-v1.2.3 PATH="$work/bin:$PATH" \
    bash -e -o pipefail -c "$name_step" 2>&1)
  rc=$?
  set -e
  calls=$(<"$work/calls")
}
mkdir -p "$work/wir-bare" "$work/wir-checkout/.github/scripts"
ln -sf "$release_push" "$work/wir-checkout/.github/scripts/release-push.sh"
run_name_step "$work/wir-bare"
check "with no checkout: the step exits 127, having called no gh, as every legion release did" \
  "$(is "$rc|$calls|$(contains "$out" 'release-push\.sh: No such file or directory')" "127||true")"
run_name_step "$work/wir-checkout"
# The notes file release-section hands gh is its own temporary one, so the last argument of the
# edit call is cut away before the two calls are compared.
check "with the checkout: the step exits 0, viewing the body and editing it once" \
  "$(is "$rc|$(wc -l <<<"$calls")|$(sed -n 1p <<<"$calls")|$(sed -n '2s/\[[^]]*\]$//p' <<<"$calls")" \
    "0|2|$(argv release view legion-v1.2.3 --repo example/repo --json body -q .body)|$(argv release edit legion-v1.2.3 --repo example/repo --notes-file)")"
check "with the checkout: the body keeps its notes and gets the digest under ## Worker image" \
  "$(is "$(<"$work/edited.md")" "$(printf 'Generated notes\n\n## Worker image\n\n`%s`' "$ref")")"

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
echo "Case: check-built accepts a build of the Claude Envoy plugin and refuses what no build holds, writing nothing"
# The tars are made as a build job makes them (`tar -cf … -C <tree> dist legion`), from a tree
# each case fills and may doctor. Each run is from an empty directory outside any repository,
# with an empty TMPDIR, and the tars' directory is compared before and after: check-built needs
# no checkout, no credential and no network, and writes nothing.
pb="$work/built"
tars="$pb/trees"
mkdir -p "$tars"
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
# built_tar <name> <marker> [<mutation> [<tar argument>...]]: $tars/<name>.tar, from a tree
# plugin_tree fills with <marker>, the shell text <mutation> then run in it, packed with the tar
# arguments (default: dist legion) after `-C <tree>`.
built_tar() {
  local name=$1 marker=$2 mutation=${3:-} dir="$tars/$1"
  shift 2
  [ $# -eq 0 ] || shift
  [ $# -gt 0 ] || set -- dist legion
  mkdir "$dir"
  plugin_tree "$dir" "$marker"
  [ -z "$mutation" ] || (cd "$dir" && eval "$mutation")
  tar -cf "$dir.tar" -C "$dir" "$@" 2>/dev/null
}
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
built_tar good rebuilt
check_built "$tars/good.tar"
check "check-built, a good build: exits 0, printing nothing and writing nothing" "$(is "$rc|$out|$written" "0||0|true")"
built_tar symlink rebuilt 'ln -s envoy-channel.js dist/link.js'
check_built "$tars/symlink.tar"
check_refused "a symlink member" "member dist/link\.js of .*symlink\.tar is a symlink"
built_tar hardlink rebuilt 'ln dist/dispatch.js dist/dispatch-copy.js'
check_built "$tars/hardlink.tar"
check_refused "a hard link member" "member dist/dispatch[-a-z]*\.js of .*hardlink\.tar is a hard link"
built_tar dotdot rebuilt '' --transform 's,^dist/dispatch\.js$,../dispatch.js,' dist legion
check "the ../ tar holds ../dispatch.js" "$(contains "$(tar -tf "$tars/dotdot.tar" 2>/dev/null)" '^\.\./dispatch\.js$')"
check_built "$tars/dotdot.tar"
check_refused "a ../ member" "member \.\./dispatch\.js of .* has a '\.\.' component"
built_tar dotdot-inner rebuilt '' --transform 's,^dist/dispatch\.js$,dist/../../dispatch.js,' dist legion
check_built "$tars/dotdot-inner.tar"
check_refused "a member climbing out through dist/../.." "member dist/\.\./\.\./dispatch\.js of .* has a '\.\.' component"
built_tar absolute rebuilt '' --transform 's,^dist/dispatch\.js$,/dispatch.js,' -P dist legion
check "the absolute tar holds /dispatch.js" "$(contains "$(tar -tf "$tars/absolute.tar" 2>/dev/null)" '^/dispatch\.js$')"
check_built "$tars/absolute.tar"
check_refused "an absolute member" "member /dispatch\.js of .* is an absolute path"
built_tar readme rebuilt 'printf "x\n" > README.md' dist legion README.md
check_built "$tars/readme.tar"
check_refused "a member outside the two directories, README.md" "member README\.md of .* is not dist, legion or a path under them"
built_tar src rebuilt 'mkdir src && printf "x\n" > src/x' dist legion src
check_built "$tars/src.tar"
check_refused "a member outside the two directories, src/x" "member src/ of .* is not dist, legion or a path under them"
built_tar space rebuilt 'printf "x\n" > "dist/a b.js"'
check_built "$tars/space.tar"
# %q quotes the space as 'dist/a b.js' or dist/a\ b.js, by bash version.
check_refused "a member whose name holds another character, a space" 'member .\{0,1\}dist/a\\\{0,1\} b\.js.\{0,1\} of .* is not dist, legion or a path under them'
built_tar executable rebuilt 'chmod +x dist/envoy-channel.js'
check_built "$tars/executable.tar"
check_refused "an executable member" "member dist/envoy-channel\.js of .*executable\.tar has the mode -rwx[-rwx]*, with an execute"
built_tar skill-script rebuilt 'mkdir legion/skills/legion-worker/scripts && printf "x\n" > legion/skills/legion-worker/scripts/x.sh && chmod 755 legion/skills/legion-worker/scripts/x.sh'
check_built "$tars/skill-script.tar"
check_refused "an executable skill script" "member legion/skills/legion-worker/scripts/x\.sh of .* has the mode -rwxr-xr-x, with an execute"
built_tar no-channel rebuilt 'rm dist/envoy-channel.js'
check_built "$tars/no-channel.tar"
check_refused "a tar missing dist/envoy-channel.js" ".*no-channel\.tar lacks dist/envoy-channel\.js, a file every build of the plugin holds"
built_tar no-dispatch rebuilt 'rm dist/dispatch.js'
check_built "$tars/no-dispatch.tar"
check_refused "a tar missing dist/dispatch.js" ".*no-dispatch\.tar lacks dist/dispatch\.js, a file every build of the plugin holds"
built_tar no-manifest rebuilt 'rm legion/.claude-plugin/plugin.json'
check_built "$tars/no-manifest.tar"
check_refused "a tar missing legion/.claude-plugin/plugin.json" ".*no-manifest\.tar lacks legion/\.claude-plugin/plugin\.json"
built_tar no-agents rebuilt 'rm -r legion/agents'
check_built "$tars/no-agents.tar"
check_refused "a tar with no file under legion/agents/" ".*no-agents\.tar holds no file under legion/agents/"
check_built "$tars/no-such.tar"
check_refused "a tar that does not exist" "listing .*no-such\.tar failed"
check_built "$tars/good.tar" extra
check_refused "a second argument" "usage: release-push\.sh check-built <tar>$"

echo
echo "Case: extract-built replaces the two directories with exactly a checked build, and leaves them untouched when it refuses one"
# A checkout whose two directories hold the seed build and an untracked file of their own, as a
# job's checkout holds them once verify-build.sh has run. Neither directory is tracked: nothing
# commits them. extract-built runs from a subdirectory of the checkout with the tar's path
# relative to it, with no credential in its environment, as release.yaml's claude_envoy_build and
# ci.yaml's claude-envoy job run it.
xa="$pb/x"
mkdir -p "$xa/packages/claude-envoy"
git init -q -b main "$xa"
identity "$xa"
printf 'seed line\n' > "$xa/README.md"
git -C "$xa" add -A
git -C "$xa" commit -q -m "seed"
xbase=$(git -C "$xa" rev-parse HEAD)
plugin_tree "$xa/packages/claude-envoy" seed
printf 'stale\n' > "$xa/packages/claude-envoy/dist/stale.js"
built_tar land rebuilt 'rm legion/agents/deep-worker.md && mkdir legion/skills/new-skill && printf "new\n" > legion/skills/new-skill/SKILL.md'
# extract_built <tar> [<arg>...]: runs `release-push.sh extract-built <tar> [<arg>...]` from
# $xa/packages; leaves its exit code in rc and its combined output in out.
extract_built() {
  set +e
  out=$(cd "$xa/packages" && env -u DEPLOY_KEY "$release_push" extract-built "$@" 2>&1)
  rc=$?
  set -e
}
# x_tree: every entry under the checkout's two directories, with its type and mode, then every
# file's checksum.
x_tree() {
  (cd "$xa/packages/claude-envoy" && find dist legion -printf '%y %m %p\n' | LC_ALL=C sort &&
    find dist legion -type f -exec sha256sum {} + | LC_ALL=C sort)
}
x_before=$(x_tree)
extract_built ../../trees/executable.tar
check "extract-built, an executable member: exits 1, naming it" \
  "$(is "$rc|$(contains "$out" "^::error::release-push\.sh extract-built: member dist/envoy-channel\.js of .*executable\.tar has the mode -rwx[-rwx]*, with an execute")" "1|true")"
check "extract-built, an executable member: the two directories are untouched" "$(is "$(x_tree)" "$x_before")"
extract_built ../../trees/land.tar extra
check "extract-built, a second argument: exits 1 with its usage, the directories untouched" \
  "$(is "$rc|$(contains "$out" '^::error::release-push\.sh extract-built: usage: release-push\.sh extract-built <tar>$')|$(is "$(x_tree)" "$x_before")" "1|true|true")"
extract_built ../../trees/land.tar
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
echo "Case: fold-notes refuses a version, a package, a prefix or an entry no release can render"
# The template: clone a, whose main holds packages/rel, released under rel-v, with fragments
# added before, between and after release tags of both shapes; packages/empty, which has a
# changie configuration and no fragment; and packages/nolog, which has a fragment and no
# configuration. The rendering cases below check it out at each release's own commit, detached,
# as that release's build job has it.
u=.changes/unreleased
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
# changie_config <package dir>: this file's own minimal changie configuration, which renders a
# fragment of a kind and a body and nothing else. The shipped configuration gets a case of its
# own below.
changie_config() {
  cat > "$1/.changie.yaml" <<'CHANGIE'
changesDir: .changes
unreleasedDir: unreleased
changelogPath: CHANGELOG.md
versionExt: md
versionFormat: ''
kindFormat: '### {{.Kind}}'
changeFormat: '- {{.Body}}'
newlines:
  beforeKind: 1
  afterKind: 1
kinds:
  - label: Added
  - label: Changed
  - label: Fixed
CHANGIE
}
mkdir -p "$ft/a/packages/rel" "$ft/a/packages/empty" "$ft/a/packages/nolog"
printf '{"name": "rel", "version": "1.5.0"}\n' > "$ft/a/packages/rel/package.json"
printf 'no changie configuration\n' > "$ft/a/packages/nolog/README.md"
changie_config "$ft/a/packages/rel"
changie_config "$ft/a/packages/empty"
commit_at 2026-02-01T12:00:00+00:00 "chore: seed"
fragment rel INFRA-1-first Added "the first entry"
fragment rel INFRA-1-another Added "another entry"
commit_at 2026-02-10T12:00:00+00:00 "feat: first"
c1=$(git -C "$ft/a" rev-parse HEAD)
# The bump-shaped release tag: on a child of the built commit that main never holds, which
# contains every commit up to the one the release built.
git -C "$ft/a" checkout -q --detach
printf '{"name": "rel", "version": "1.9.0"}\n' > "$ft/a/packages/rel/package.json"
commit_at 2026-03-01T23:30:00-05:00 "chore: release rel v1.9.0 [skip ci]"
git -C "$ft/a" tag rel-v1.9.0
git -C "$ft/a" checkout -q main
fragment rel INFRA-2-second Fixed "the second entry"
# The listener-shaped tag, on the built commit itself.
commit_at 2026-03-05T01:00:00+09:00 "feat: second"
c2=$(git -C "$ft/a" rev-parse HEAD)
git -C "$ft/a" tag rel-v1.10.0
fragment rel INFRA-3-third Added "the third entry"
fragment nolog INFRA-3-nolog Added "an entry of a package with no configuration"
commit_at 2026-04-10T22:15:00-07:00 "feat: third"
c3=$(git -C "$ft/a" rev-parse HEAD)
# A name under the prefix that is no release: it bounds no group and is rendered nowhere.
git -C "$ft/a" tag rel-v1.11.0-rc.1

# notes_run <commit> <version> <package-dir> <tag-prefix>: checks the template's clone out at
# <commit>, detached, and runs fold-notes there into one notes file; leaves rc, out (stdout, the
# `notes=` line), err, wrote (whether the file is there) and notes (its content, empty when none
# was written).
notes_file="$work/fold-notes.md"
notes_run() {
  rm -f "$notes_file"
  git -C "$ft/a" checkout -q --detach "$1"
  set +e
  out=$(cd "$ft/a" && "$release_push" fold-notes "$2" "$notes_file" "$3" "$4" 2>"$work/fold-notes.err")
  rc=$?
  set -e
  err=$(<"$work/fold-notes.err")
  notes='' wrote=false
  [ ! -f "$notes_file" ] || { notes=$(<"$notes_file"); wrote=true; }
}
notes_run "$c3" 1.9 packages/rel rel-v
check "fold-notes: a version that is not X.Y.Z is refused, writing nothing" \
  "$(is "$rc|$wrote|$(contains "$err" '^::error::release-push\.sh fold-notes: the release version 1\.9 is not X\.Y\.Z')" "1|false|true")"
for dir in ../rel /packages/rel 'packages/rel=rel-v'; do
  notes_run "$c3" 1.11.0 "$dir" rel-v
  printf -v shown '%q' "$dir"
  check "fold-notes: the package directory $shown is refused" \
    "$(is "$rc|$wrote|$(contains "$err" '^::error::release-push\.sh fold-notes: package directory .* is not a relative path')" "1|false|true")"
done
for prefix in '' 'rel v' rel/v; do
  notes_run "$c3" 1.11.0 packages/rel "$prefix"
  printf -v shown '%q' "$prefix"
  check "fold-notes: the tag prefix $shown is refused: a release line is what bounds a release" \
    "$(is "$rc|$wrote|$(contains "$err" '^::error::release-push\.sh fold-notes: tag prefix .* holds more than letters, digits')" "1|false|true")"
done
# usage_run <arg>...: runs fold-notes with those arguments in the template's clone, as it stands;
# leaves rc and out (combined).
usage_run() {
  set +e
  out=$(cd "$ft/a" && "$release_push" fold-notes "$@" 2>&1)
  rc=$?
  set -e
}
usage='usage: release-push\.sh fold-notes <version> <notes-file> <package-dir> <tag-prefix>'
usage_run 1.11.0 "$notes_file" packages/rel
check "fold-notes: no tag prefix at all is refused, naming the usage" \
  "$(is "$rc|$(contains "$out" "^::error::release-push\.sh fold-notes: $usage\$")" "1|true")"
usage_run 1.11.0 "$notes_file" packages/rel rel-v packages/empty
check "fold-notes: a second package is refused: one release of one package goes into one file" \
  "$(is "$rc|$(contains "$out" "^::error::release-push\.sh fold-notes: $usage\$")" "1|true")"
notes_run "$c3" 1.11.0 packages/nolog rel-v
check "fold-notes: a package whose entries are in the release but holds no .changie.yaml is refused" \
  "$(is "$rc|$wrote|$(contains "$err" '^::error::release-push\.sh fold-notes: packages/nolog holds no \.changie\.yaml at HEAD to render its fragments with$')" "1|false|true")"

# Anything directly in an unreleased directory but a regular file whose name ends in .yaml is
# refused, by name, with nothing rendered: no release would ever render it. Each package of this
# repository holds one fragment and one such entry — a subdirectory, a .yml file, a README.md, a
# symlink — and packages/two holds two of them. fold-check refuses them the same way, from the
# same listing, and neither command reaches changie to do it.
rt="$work/fold-refuse"
git init -q -b main "$rt"
identity "$rt"
for package in sub yml readme link two; do
  mkdir -p "$rt/packages/$package/$u"
  printf 'kind: Added\nbody: fine\n' > "$rt/packages/$package/$u/INFRA-1-fine.yaml"
done
mkdir -p "$rt/packages/sub/$u/sjawhar"
printf 'kind: Added\nbody: nested\n' > "$rt/packages/sub/$u/sjawhar/fix-thing.yaml"
printf 'kind: Added\nbody: yml\n' > "$rt/packages/yml/$u/INFRA-1-a.yml"
printf 'not a fragment\n' > "$rt/packages/readme/$u/README.md"
ln -s INFRA-1-fine.yaml "$rt/packages/link/$u/link.yaml"
printf 'kind: Added\nbody: yml\n' > "$rt/packages/two/$u/INFRA-1-a.yml"
printf 'not a fragment\n' > "$rt/packages/two/$u/README.md"
git -C "$rt" add -A
git -C "$rt" commit -q -m "feat: odd entries"
refuse_notes="$work/fold-refuse.md"
# A changie that fails if it is ever run, first on PATH for the refusals below: neither command
# reaches changie to make them, and fold-check looks for one before it reads anything, so the
# cases say the same thing whether or not the real changie is installed.
mkdir -p "$work/stub-bin"
cat > "$work/stub-bin/changie" <<'STUB'
#!/usr/bin/env bash
echo "the stub changie was run" >&2
exit 1
STUB
chmod +x "$work/stub-bin/changie"
# refuse_run <command> <arg>...: runs that command of the script under test in the refusal
# repository, with the stub changie on PATH; leaves rc, out and err.
refuse_run() {
  rm -f "$refuse_notes"
  set +e
  out=$(cd "$rt" && PATH="$work/stub-bin:$PATH" "$release_push" "$@" 2>"$work/fold-refuse.err")
  rc=$?
  set -e
  err=$(<"$work/fold-refuse.err")
}
refused='at HEAD: each entry in a package.s \.changes/unreleased/ must be a regular file whose name ends in \.yaml'
for entry in sub/sjawhar yml/INFRA-1-a.yml readme/README.md link/link.yaml; do
  refuse_run fold-notes 1.0.0 "$refuse_notes" "packages/${entry%%/*}" odd-v
  check "fold-notes: packages/${entry%%/*}/$u/${entry#*/} is refused by name, writing no file" \
    "$(is "$rc|$([ -e "$refuse_notes" ] && echo true || echo false)|$(contains "$err" "^::error::release-push\.sh fold-notes: packages/${entry%%/*}/$u/${entry#*/} $refused")" "1|false|true")"
  refuse_run fold-check "packages/${entry%%/*}"
  check "fold-check: packages/${entry%%/*}/$u/${entry#*/} is refused the same way" \
    "$(is "$rc|$(contains "$err" "^::error::release-push\.sh fold-check: packages/${entry%%/*}/$u/${entry#*/} $refused")" "1|true")"
done
refuse_run fold-notes 1.0.0 "$refuse_notes" packages/two odd-v
check "fold-notes: one error names every refused entry of the package, sorted" \
  "$(is "$rc|$(grep -c '^::error::' <<<"$err")|$(contains "$err" "^::error::release-push\.sh fold-notes: packages/two/$u/INFRA-1-a\.yml packages/two/$u/README\.md $refused")" "1|1|true")"
check "fold-notes: that refusal says to move or rename a fragment in one commit with git mv" \
  "$(contains "$err" 'in one commit, with git mv')"
refuse_run fold-check
check "fold-check: no package directory is refused" \
  "$(is "$rc|$out|$(contains "$err" '^::error::release-push\.sh fold-check: no package directory given')" "1||true")"

# fold-check without changie: one refusal naming the tool, where rendering each fragment to find
# the refusals would report every one of them refused with `changie: command not found` inside
# the message. The command refuses before it runs anything, so an empty PATH costs it nothing;
# $BASH runs the script, which its `/usr/bin/env bash` could not find there.
mkdir -p "$work/no-tools"
set +e
out=$(cd "$rt" && env PATH="$work/no-tools" "$BASH" "$release_push" fold-check packages/sub 2>&1)
rc=$?
set -e
check "fold-check: with no changie on PATH it refuses once, naming the tool and how to install it" \
  "$(is "$rc|$(grep -c '^::error::' <<<"$out")|$(contains "$out" '^::error::release-push\.sh fold-check: changie is not on PATH,.*install-changie')" "1|1|true")"
check "fold-check: that refusal names no fragment" "$(is "$(contains "$out" 'INFRA-1-fine')" false)"

# The rendering cases need the real changie, which the release build jobs install with
# .github/actions/install-changie. Without it on PATH they are skipped, not counted.
echo
echo "Case: fold-notes renders the changelog entries a release's own commits added"
if command -v changie >/dev/null; then
  notes_run "$c1" 1.9.0 packages/rel rel-v
  check "fold-notes: the first release of a line, with no tag below it, renders the entries it ships" \
    "$(is "$rc|$out|$(contains "$err" "^Wrote $notes_file from 2 fragments\$")" "0|notes=true|true")"
  check "fold-notes: those notes hold that release's entries under their kind, and no later release's" \
    "$(is "$(contains "$notes" '^### Added$')|$(contains "$notes" '^- the first entry$')|$(contains "$notes" '^- another entry$')|$(contains "$notes" 'the second entry')" "true|true|true|false")"
  notes_run "$c2" 1.10.0 packages/rel rel-v
  check "fold-notes: the next release holds its own entry alone, the bump-shaped tag below it bounding the rest" \
    "$(is "$rc|$out|$(contains "$err" "from 1 fragments\$")|$(contains "$notes" '^- the second entry$')|$(contains "$notes" 'the first entry')" "0|notes=true|true|true|false")"
  notes_run "$c3" 1.11.0 packages/rel rel-v
  check "fold-notes: a release with no tag of its own yet holds the entries added since the newest release" \
    "$(is "$rc|$(contains "$notes" '^- the third entry$')|$(contains "$notes" 'the second entry')" "0|true|false")"
  check "fold-notes: rel-v1.11.0-rc.1, a name under the prefix that is no release, bounded nothing" \
    "$(is "$(contains "$notes" '^- the third entry$')" true)"
  notes_run "$c1" 1.10.0 packages/rel rel-v
  check "fold-notes: a release whose commits a lower tag already holds writes no file, prints notes=false and exits 0" \
    "$(is "$rc|$wrote|$out|$(contains "$err" '^No changelog fragment of packages/rel is new in 1\.10\.0; wrote no notes file$')" "0|false|notes=false|true")"
  notes_run "$c3" 1.11.0 packages/empty rel-v
  check "fold-notes: a package holding no fragment at all writes no file, prints notes=false and exits 0" \
    "$(is "$rc|$wrote|$out|$(contains "$err" '^No changelog fragment of packages/empty is new in 1\.11\.0; wrote no notes file$')" "0|false|notes=false|true")"
  # Only HEAD's tree counts: a file the working tree alone holds is in no release.
  printf 'kind: Added\nbody: untracked\n' > "$ft/a/packages/rel/$u/INFRA-9-untracked.yaml"
  notes_run "$c3" 1.11.0 packages/rel rel-v
  rm -f "$ft/a/packages/rel/$u/INFRA-9-untracked.yaml"
  check "fold-notes: a fragment the working tree alone holds is rendered nowhere" \
    "$(is "$rc|$(contains "$notes" 'untracked')" "0|false")"

  # A fragment name holding a glob character is still one file: the commit that added that very
  # file decides which release ships it, never one that added another file the name matches as a
  # pattern. Here INFRA-7-[a].yaml ships in glob-v1.0.0, and INFRA-7-a.yaml, which `[a]` matches
  # as a glob, is added after it and ships in glob-v1.1.0.
  gt="$work/fold-glob"
  mkdir -p "$gt/packages/glob/$u"
  git init -q -b main "$gt"
  identity "$gt"
  changie_config "$gt/packages/glob"
  printf 'kind: Added\nbody: bracketed\n' > "$gt/packages/glob/$u/INFRA-7-[a].yaml"
  git -C "$gt" add -A
  GIT_COMMITTER_DATE=2026-05-01T12:00:00+00:00 git -C "$gt" commit -q -m "feat: bracketed"
  git -C "$gt" tag glob-v1.0.0
  printf 'kind: Added\nbody: plain\n' > "$gt/packages/glob/$u/INFRA-7-a.yaml"
  git -C "$gt" add -A
  GIT_COMMITTER_DATE=2026-05-02T12:00:00+00:00 git -C "$gt" commit -q -m "feat: plain"
  git -C "$gt" tag glob-v1.1.0
  set +e
  out=$(cd "$gt" && "$release_push" fold-notes 1.1.0 "$work/fold-glob.md" packages/glob glob-v 2>&1)
  rc=$?
  set -e
  glob_notes=$(<"$work/fold-glob.md")
  check "fold-notes: a fragment whose name holds a glob character ships in the release of the commit that added that very file" \
    "$(is "$rc|$(contains "$glob_notes" '^- plain$')|$(contains "$glob_notes" 'bracketed')" "0|true|false")"

  # A fragment renamed with git mv keeps the release that first shipped it. Here sjawhar/fix.yaml
  # ships in ren-v1.0.0; a later commit moves it to sjawhar-fix.yaml, and INFRA-8-later.yaml,
  # alike enough to read to git as a copy of it, is added after that and ships in ren-v1.1.0
  # (`git log --follow` would follow that copy and give it the older release).
  nt="$work/fold-rename"
  mkdir -p "$nt/packages/ren/$u/sjawhar"
  git init -q -b main "$nt"
  identity "$nt"
  changie_config "$nt/packages/ren"
  printf 'kind: Fixed\ntime: 2026-06-01T12:00:00Z\nbody: a nested fix\n' > "$nt/packages/ren/$u/sjawhar/fix.yaml"
  git -C "$nt" add -A
  GIT_COMMITTER_DATE=2026-06-01T12:00:00+00:00 git -C "$nt" commit -q -m "fix: nested"
  git -C "$nt" tag ren-v1.0.0
  git -C "$nt" mv "packages/ren/$u/sjawhar/fix.yaml" "packages/ren/$u/sjawhar-fix.yaml"
  GIT_COMMITTER_DATE=2026-06-02T12:00:00+00:00 git -C "$nt" commit -q -m "chore: move the fragment"
  printf 'kind: Fixed\ntime: 2026-06-03T12:00:00Z\nbody: a later fix\n' > "$nt/packages/ren/$u/INFRA-8-later.yaml"
  git -C "$nt" add -A
  GIT_COMMITTER_DATE=2026-06-03T12:00:00+00:00 git -C "$nt" commit -q -m "fix: later"
  git -C "$nt" tag ren-v1.1.0
  set +e
  out=$(cd "$nt" && "$release_push" fold-notes 1.1.0 "$work/fold-rename.md" packages/ren ren-v 2>&1)
  rc=$?
  set -e
  ren_notes=$(<"$work/fold-rename.md")
  check "fold-notes: a fragment renamed with git mv keeps the release that first shipped it, and one alike enough to read as its copy gets its own" \
    "$(is "$rc|$(contains "$ren_notes" '^- a later fix$')|$(contains "$ren_notes" 'a nested fix')" "0|true|false")"

  echo
  echo "Case: fold-notes renders a package with the shipped packages/pi-legion/.changie.yaml"
  # The configurations above are this file's own, minimal ones; this one is the shipped file
  # itself, copied in beside fragments carrying the custom Issue field it requires, so a bad edit
  # to a real .changie.yaml fails here rather than in a release's build job. ci.yaml's test filter
  # and .github/scripts/ci_job_reads.py name that file for this copy.
  sc="$work/fold-shipped-config"
  mkdir -p "$sc/packages/shipped/$u"
  git init -q -b main "$sc"
  identity "$sc"
  cp "$script_dir/../../packages/pi-legion/.changie.yaml" "$sc/packages/shipped/.changie.yaml"
  cat > "$sc/packages/shipped/$u/INFRA-20-Added-20260101-120000.yaml" <<'YAML'
kind: Added
body: a plain addition
time: 2026-01-01T12:00:00Z
custom:
  Issue: INFRA-20
YAML
  cat > "$sc/packages/shipped/$u/INFRA-21-Changed-20260101-130000.yaml" <<'YAML'
kind: Changed
body: |-
  a change whose entry runs
  onto a second line

  and a second paragraph
time: 2026-01-01T13:00:00Z
custom:
  Issue: INFRA-21
YAML
  git -C "$sc" add -A
  GIT_COMMITTER_DATE=2026-01-02T12:00:00+00:00 git -C "$sc" commit -q -m "feat: two entries"
  git -C "$sc" tag shipped-v3.1.0
  shipped_notes="$work/notes-shipped.md"
  set +e
  out=$(cd "$sc" && "$release_push" fold-notes 3.1.0 "$shipped_notes" packages/shipped shipped-v 2>&1)
  rc=$?
  set -e
  check "the shipped config: fold-notes exits 0, writing the release's notes file" \
    "$(is "$rc|$([ -f "$shipped_notes" ] && echo true || echo false)" "0|true")"
  # What that config's kindFormat, changeFormat and newlines render, byte for byte: a heading per
  # kind in the config's order, one bullet per entry, and the second and later lines of a body two
  # spaces under the bullet, with its blank line left empty.
  printf '\n\n### Added\n\n- a plain addition\n\n### Changed\n\n- a change whose entry runs\n  onto a second line\n\n  and a second paragraph' \
    > "$work/shipped-expected.md"
  check "the shipped config: the entries render under their kind, each body line after the first two spaces under the bullet" \
    "$(cmp -s "$work/shipped-expected.md" "$shipped_notes" && echo true || echo false)"

  echo
  echo "Case: fold-check renders every fragment of a pull request's checkout and names what changie refuses"
  # The pull request's gate, over the same repository and the same shipped configuration: after
  # the merge nothing renders a fragment before the release that first ships it, so a fragment
  # changie refuses would fail that release's build job on every run until a fix merges.
  # check_run <package-dir>...: runs fold-check in that repository; leaves rc and out (combined).
  check_run() {
    set +e
    out=$(cd "$sc" && "$release_push" fold-check "$@" 2>&1)
    rc=$?
    set -e
  }
  check_run packages/shipped
  check "fold-check: a checkout whose every fragment renders exits 0, saying nothing" "$(is "$rc|$out" "0|")"
  check_run packages/shipped packages/none
  check "fold-check: a package holding no fragment renders nothing and exits 0" "$(is "$rc|$out" "0|")"
  check_run packages/shipped=shipped-v
  check "fold-check: a tag prefix on the directory is refused: fold-check reads no release line" \
    "$(is "$rc|$(contains "$out" '^::error::release-push\.sh fold-check: package directory packages/shipped=shipped-v is not a relative path')" "1|true")"
  bad="$sc/packages/shipped/$u/INFRA-22-Fixd-20260101-140000.yaml"
  cat > "$bad" <<'YAML'
kind: Fixd
body: a kind the configuration does not list
time: 2026-01-01T14:00:00Z
custom:
  Issue: INFRA-22
YAML
  git -C "$sc" add -A
  GIT_COMMITTER_DATE=2026-01-03T12:00:00+00:00 git -C "$sc" commit -q -m "feat: a kind changie refuses"
  check_run packages/shipped
  check "fold-check: a kind the configuration does not list fails it, with one ::error:: naming that fragment and what changie said" \
    "$(is "$rc|$(grep -c '^::error::' <<<"$out")|$(contains "$out" "packages/shipped/$u/INFRA-22-Fixd-20260101-140000\.yaml, where changie said: .*Fixd")" "1|1|true")"
  check "fold-check: the error names no fragment changie renders" \
    "$(is "$(contains "$out" 'INFRA-20')|$(contains "$out" 'INFRA-21')" "false|false")"
  cat > "$sc/packages/shipped/$u/INFRA-23-Chnged-20260101-150000.yaml" <<'YAML'
kind: Chnged
body: another kind the configuration does not list
time: 2026-01-01T15:00:00Z
custom:
  Issue: INFRA-23
YAML
  git -C "$sc" add -A
  GIT_COMMITTER_DATE=2026-01-04T12:00:00+00:00 git -C "$sc" commit -q -m "feat: a second kind changie refuses"
  check_run packages/shipped
  check "fold-check: both refused fragments are named, each with what changie said, in the one error" \
    "$(is "$rc|$(grep -c '^::error::' <<<"$out")|$(contains "$out" "INFRA-22-Fixd-20260101-140000\.yaml, where changie said: .*Fixd")|$(contains "$out" "INFRA-23-Chnged-20260101-150000\.yaml, where changie said: .*Chnged")" "1|1|true|true")"
  # What fold-check is there to stop: the release that first ships such a fragment fails its own
  # build job, naming it the same way, on every run until a fix merges.
  set +e
  out=$(cd "$sc" && "$release_push" fold-notes 3.2.0 "$work/notes-refused.md" packages/shipped shipped-v 2>&1)
  rc=$?
  set -e
  check "fold-notes: the release that first ships a fragment changie refuses fails, naming each one" \
    "$(is "$rc|$(contains "$out" "^::error::release-push\.sh fold-notes: changie could not render packages/shipped's notes for 3\.2\.0; refused alone: .*INFRA-22-Fixd.*INFRA-23-Chnged.*; changie said: ")" "1|true")"
else
  echo "  SKIP: the cases that render with changie (changie is not on PATH)"
fi

echo
echo "Case: fold-orphans refuses a fragment of a package it was not given"
# ci.yaml's lint job runs it over .github/changelog-packages on every pull request, inside a
# required check, so a fragment no release renders is caught before it reaches main.
# packages/envoy is released under envoy-v; packages/daemon, which has no .changie.yaml, is not.
ot="$work/fold-orphan"
git init -q -b main "$ot"
identity "$ot"
mkdir -p "$ot/packages/envoy/$u" "$ot/packages/daemon/$u"
printf 'kind: Added\nbody: an envoy entry\n' > "$ot/packages/envoy/$u/pr-1-envoy.yaml"
printf 'kind: Added\nbody: a daemon entry\n' > "$ot/packages/daemon/$u/pr-1-daemon.yaml"
git -C "$ot" add -A
GIT_COMMITTER_DATE=2026-07-01T12:00:00+00:00 git -C "$ot" commit -q -m "feat: two fragments"
git -C "$ot" tag envoy-v1.0.0
# orphans_run <package-dir>...: runs fold-orphans in that repository; leaves rc, out and err.
orphans_run() {
  set +e
  out=$(cd "$ot" && "$release_push" fold-orphans "$@" 2>"$work/fold-orphans.err")
  rc=$?
  set -e
  err=$(<"$work/fold-orphans.err")
}
orphans_run packages/envoy
check "fold-orphans: the daemon's fragment is refused, with one error naming it and nothing on stdout" \
  "$(is "$rc|$out|$(grep -c "^::error::release-push\.sh fold-orphans: packages/daemon/$u/pr-1-daemon\.yaml at HEAD: " <<<"$err")" "1||1")"
check "fold-orphans: that refusal names the directories it was given and says what to do about the file" \
  "$(is "$(contains "$err" 'here packages/envoy,')|$(contains "$err" 'remove it, or give its package a \.changie\.yaml and a CHANGELOG\.md and add it to \.github/changelog-packages$')" "true|true")"
git -C "$ot" rm -q "packages/daemon/$u/pr-1-daemon.yaml"
GIT_COMMITTER_DATE=2026-07-02T12:00:00+00:00 git -C "$ot" commit -q -m "chore: remove the daemon fragment"
orphans_run packages/envoy
check "fold-orphans: with the file removed in a commit, it exits 0 saying nothing" "$(is "$rc|$out|$err" "0||")"
# Only HEAD's tree counts: a file the working tree alone holds is no fragment of this repository.
mkdir -p "$ot/packages/daemon/$u"
printf 'kind: Added\nbody: uncommitted\n' > "$ot/packages/daemon/$u/pr-2-daemon.yaml"
orphans_run packages/envoy
check "fold-orphans: a file under .changes/ in the working tree alone is not refused" "$(is "$rc|$out|$err" "0||")"
rm -r "$ot/packages/daemon/.changes"
# Anywhere under .changes/, not only in unreleased/: no release reads anything of that package.
mkdir -p "$ot/packages/daemon/.changes"
printf 'A header\n' > "$ot/packages/daemon/.changes/header.tpl.md"
git -C "$ot" add -A
GIT_COMMITTER_DATE=2026-07-03T12:00:00+00:00 git -C "$ot" commit -q -m "chore: a daemon .changes file"
orphans_run packages/envoy
check "fold-orphans: a file under .changes/ outside unreleased/ is refused too" \
  "$(is "$rc|$out|$(contains "$err" "^::error::release-push\.sh fold-orphans: packages/daemon/\.changes/header\.tpl\.md at HEAD: ")" "1||true")"
orphans_run
check "fold-orphans: no package directory is refused" \
  "$(is "$rc|$out|$(contains "$err" '^::error::release-push\.sh fold-orphans: no package directory given')" "1||true")"

summary "release-push.sh version, tagged, tag, pair-version, release, release-section, push-tag, check-built, extract-built, handed, no-credential, fold-notes, fold-check and fold-orphans, and release.yaml's worker_image_release step"
