#!/usr/bin/env bash
# Tests for the whole changelog fold with the real changie: `release-push.sh fold-plan`, then
# `fold-notes`, which renders the fragments with changie, then `push-fold`, which writes the notes
# into CHANGELOG.md and lands the fold on main. release-push.test.sh tests each command's rules
# with notes it writes itself; this proves that the packages' .changie.yaml and changie v1.26.0
# render what push-fold expects, in the layout of the existing CHANGELOG.md files.
#
# The repository holds copies of this checkout's packages/pi-legion/.changie.yaml and
# packages/pi-legion/CHANGELOG.md, taken when the test runs, and fragments written by hand (one
# per kind, a body written as a plain `|-` block whose second paragraph follows a blank line, one
# without `time:`) and one written by `changie new`, all shipped by a bump-shaped release tag (on
# a child of the built commit, as the npm releases tag it); the fold indents that body's
# continuation lines two spaces under the bullet and leaves its blank line empty. The release's
# major is one above that of the copied CHANGELOG.md's first `## [x.y.z]` heading, its newest
# release since the fold keeps the file newest first, so the fold, which places a release's block
# among the others by version, puts it on top whatever pi-legion has released since. push-fold
# reaches a bare origin as release-push.test.sh's push-main cases do: through a deploy key, with
# the GitHub URL rewritten to the local origin by GIT_CONFIG_*, and a stub `gh` that logs each
# call. A last case gives fold-notes a fragment of an unknown kind, in a later release of the same
# major, which changie refuses without naming the file, and checks fold-notes names it.
#
# Needs changie v1.26.0 on PATH (.github/actions/install-changie installs it) and exits 1 without
# it: the test never passes by skipping.
#
# Run from anywhere: .github/scripts/changelog-fold.test.sh
set -euo pipefail
# Hermetic: the developer's global/system git config must not reach the repos or the script under
# test. Needs git >= 2.32.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

found=$(changie --version 2>/dev/null) || found=''
if [ "${found##* }" != v1.26.0 ]; then
  echo "changelog-fold.test.sh: needs changie v1.26.0 on PATH (.github/actions/install-changie installs it); changie --version printed: ${found:-nothing, changie is not on PATH}" >&2
  exit 1
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
release_push="$script_dir/release-push.sh"
package_src="$script_dir/../../packages/pi-legion"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The repository: clone a, whose main holds the fragments; the release tag pi-legion-v<version> on
# a child of main's tip, committed at 20:00 on June 30 in UTC-6, which is July 1 in UTC; and a bare
# origin whose main is that tip. The version is <major>.2.3 and the one before it <major>.2.2.
a="$work/a"
pkg="$a/packages/pi-legion"
unreleased="$pkg/.changes/unreleased"
mkdir -p "$unreleased"
git init -q -b main "$a"
git -C "$a" config user.name "fold test"
git -C "$a" config user.email "fold-test@example.com"
cp "$package_src/.changie.yaml" "$package_src/CHANGELOG.md" "$pkg/"
cp "$pkg/CHANGELOG.md" "$work/old-CHANGELOG.md"
# The major: one above the copy's first `## [x.y.z]` heading's, read from the copy the fold is
# compared with; 1 when it names no release.
newest=$(sed -nE '/^## \[([0-9]+)\.[0-9]+\.[0-9]+\].*/{s//\1/p;q;}' "$work/old-CHANGELOG.md")
major=$((10#${newest:-0} + 1))
version="$major.2.3"
printf '{"name": "@sjawhar/pi-legion", "version": "%s"}\n' "$major.2.2" > "$pkg/package.json"
git -C "$a" add -A
git -C "$a" commit -q -m "chore: seed"

cat > "$unreleased/INFRA-10-added.yaml" <<'YAML'
kind: Added
body: a hand-written addition
time: 2026-05-01T10:00:00Z
YAML
cat > "$unreleased/INFRA-10-no-time.yaml" <<'YAML'
kind: Added
body: an addition without a time
YAML
cat > "$unreleased/INFRA-11-changed.yaml" <<'YAML'
kind: Changed
body: |-
  a change whose entry runs
  onto a second line

  and a second paragraph
time: 2026-05-02T10:00:00Z
YAML
cat > "$unreleased/INFRA-12-removed.yaml" <<'YAML'
kind: Removed
body: a removal
time: 2026-05-03T10:00:00Z
YAML
cat > "$unreleased/INFRA-13-fixed.yaml" <<'YAML'
kind: Fixed
body: a fix
time: 2026-05-04T10:00:00Z
YAML
(cd "$pkg" && CI=true changie new --kind Added --body "an entry changie new wrote" --custom Issue=INFRA-1)
written=$(find "$unreleased" -name 'INFRA-1-*.yaml')
git -C "$a" add -A
git -C "$a" commit -q -m "feat: the entries"
base=$(git -C "$a" rev-parse HEAD)
git -C "$a" checkout -q --detach
printf '{"name": "@sjawhar/pi-legion", "version": "%s"}\n' "$version" > "$pkg/package.json"
git -C "$a" add -A
GIT_COMMITTER_DATE=2026-06-30T20:00:00-06:00 git -C "$a" commit -q -m "chore: release pi-legion v$version [skip ci]"
git -C "$a" tag "pi-legion-v$version"
git -C "$a" checkout -q main
git clone -q --bare "$a" "$work/origin.git"
git -C "$a" checkout -q --detach

ssh-keygen -q -t ed25519 -N '' -C changelog-fold-test -f "$work/deploy"
# The stub gh, first on PATH: it logs each call to $work/calls, one line, each argument in
# brackets.
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
{ printf '[%s]' "$@"; printf '\n'; } >> "$GH_TEST_DIR/calls"
GH
chmod +x "$work/bin/gh"
: > "$work/calls"
spec=packages/pi-legion=pi-legion-v

# run <command> <arg>...: runs `release-push.sh <command> <arg>... $spec` in clone a with the deploy
# key, the GitHub URL rewritten to the local origin and the stub gh; leaves rc and out (combined).
run() {
  set +e
  out=$(cd "$a" && env GITHUB_REPOSITORY=example/repo DEPLOY_KEY="$(<"$work/deploy")" GH_TEST_DIR="$work" \
    PATH="$work/bin:$PATH" GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.$work/origin.git.insteadOf" \
    GIT_CONFIG_VALUE_0="git@github.com:example/repo.git" "$release_push" "$@" "$spec" 2>&1)
  rc=$?
  set -e
}

echo "Case: the fold of a release's fragments, from the plan to origin's CHANGELOG.md"
check "changie new writes a fragment named for its issue" "$([ -n "$written" ] && echo true || echo false)"
run fold-plan
check "fold-plan: exits 0" "$(is "$rc" 0)"
check "fold-plan: plans the six fragments under $version, dated in UTC" \
  "$(is "$(cut -f1-3 <<<"$out" | sort -u)|$(wc -l <<<"$out")" "packages/pi-legion	$version	2026-07-01|6")"
run fold-notes "$work/notes"
check "fold-notes: exits 0" "$(is "$rc" 0)"
check "fold-notes: writes the one notes file of the release" "$(is "$(ls "$work/notes")" "pi-legion_$version.md")"
run push-fold "$base" "$work/notes"
check "push-fold: exits 0, origin's main one commit over the base" \
  "$(is "$rc|$(git -C "$work/origin.git" rev-list --count "$base..main")|$(git -C "$work/origin.git" log -1 --format=%P main)" "0|1|$base")"
check "push-fold: calls no gh" "$(is "$(<"$work/calls")" "")"
git -C "$work/origin.git" show main:packages/pi-legion/CHANGELOG.md > "$work/new-CHANGELOG.md"
first=$(grep -n -m 1 '^## ' "$work/old-CHANGELOG.md" | cut -d: -f1)
block="## [$version] - 2026-07-01"'

### Added

- an addition without a time
- a hand-written addition
- an entry changie new wrote

### Changed

- a change whose entry runs
  onto a second line

  and a second paragraph

### Removed

- a removal

### Fixed

- a fix
'
{
  head -n "$((first - 1))" "$work/old-CHANGELOG.md"
  printf '%s\n' "$block"
  tail -n "+$first" "$work/old-CHANGELOG.md"
} > "$work/expected-CHANGELOG.md"
check "push-fold: the release's block, kinds in order and bodies as bullets, stands right above the old first ## heading" \
  "$(cmp -s "$work/expected-CHANGELOG.md" "$work/new-CHANGELOG.md" && echo true || echo false)"
check "push-fold: every old line stays, in order" \
  "$(is "$(contains "$(diff "$work/old-CHANGELOG.md" "$work/new-CHANGELOG.md" || true)" '^<')" false)"
check "push-fold: no fragment is left on main" \
  "$(is "$(git -C "$work/origin.git" ls-tree --name-only main -- packages/pi-legion/.changes/unreleased/)" "")"

echo
echo "Case: fold-notes names a fragment changie refuses"
# A later listener-shaped release, on the commit itself, ships a good fragment and one of an
# unknown kind; changie's own error names the kind only.
git -C "$a" checkout -q --detach "$base"
printf 'kind: Fixed\nbody: a good fix\n' > "$unreleased/INFRA-14-good.yaml"
printf 'kind: Deprecated\nbody: no such kind\n' > "$unreleased/INFRA-14-bogus.yaml"
git -C "$a" add -A
git -C "$a" commit -q -m "feat: more entries"
git -C "$a" tag "pi-legion-v$major.2.4"
run fold-notes "$work/notes-bogus"
check "an unknown kind: fold-notes exits 1 with an ::error:: naming the package and the label" \
  "$(is "$rc|$(contains "$out" "^::error::release-push.sh fold-notes: changie could not render packages/pi-legion's notes for $major\.2\.4")" "1|true")"
check "an unknown kind: the error names the fragment, and only that one" \
  "$(contains "$out" "refused alone: packages/pi-legion/\.changes/unreleased/INFRA-14-bogus\.yaml; changie said: .*Deprecated")"

summary "the changelog fold with changie v1.26.0: fold-plan, fold-notes and push-fold"
