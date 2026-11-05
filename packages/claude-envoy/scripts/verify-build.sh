#!/usr/bin/env bash
# verify-build.sh <tar> — build the plugin from this tree, check the build is one main's Rebuild
# Claude Envoy plugin workflow lands, and pack it into <tar>, leaving the checkout as it found it.
#
# This is the one statement of what main's rebuild must accept. The rebuild's build job
# (.github/workflows/claude-envoy-rebuild.yaml) runs it and hands <tar> to its push job, whose
# `release-push.sh push-built` checks the same tar again before it commits it; ci.yaml's required
# test job and its claude-envoy job run the very same command on a pull request, so a pull request
# fails a check for whatever main's push job would refuse instead of merging and then failing every
# rebuild on main.
#
# It writes no file of the checkout: a pull request never commits dist/ or legion/, and jj records
# whatever a command writes into the working copy. Every build goes into a scratch directory
# outside the repository, and <tar> must be outside it too. The jobs that then put <tar> into a
# checkout are named at `release-push.sh extract-built`.
#
# Every file this script and scripts/build.ts read is a build input, so scripts/build-inputs.txt
# must name it: main's push job lands a build on a moved main and decides from that list alone
# whether the move changed the build. tests/build-inputs.test.ts checks the list against what the
# bundles and the Legion plugin read, but not against what this script, build.ts or the workflow
# read by path, which the list's header names as listed by hand (check-private-names.sh, the
# .gitignore files, release-push.sh, .bun-version, the scripts build.ts imports or runs, the
# workflow and setup-bun): a new file read here goes into the list by hand.
#
# In order, whatever the caller's directory:
#   1. `bun scripts/build.ts --out` twice, into two scratch directories, and the two builds'
#      dist/ and legion/ compared byte for byte, so what lands is what the source builds to, not
#      one run's accident;
#   2. .github/scripts/check-private-names.sh over the first build, laid out at its repository
#      path in a scratch git repository (below);
#   3. no built file is one git ignores there (below);
#   4. `tar -cf <tar>` of the first build's dist/ and legion/, the tar the push job receives;
#   5. `.github/scripts/release-push.sh check-built <tar>`, push-built's own member checks: no
#      symlink, no executable file, no name outside letters, digits, '.', '_' and '-', and every
#      bundle and file a build holds (a skill pi-legion ships is staged into legion/ with `cp -R`,
#      which keeps its modes and symlinks).
# Any failure exits non-zero.
#
# Run from packages/claude-envoy: bun run verify-build <tar outside the repository>
set -euo pipefail

[ $# -eq 1 ] || {
  echo "::error::verify-build.sh: usage: verify-build.sh <tar outside the repository>" >&2
  exit 2
}
tar_file=$1

package_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
repo_root=$(cd "$package_dir/../.." && pwd -P)

# <tar>, made absolute from the caller's directory, must sit outside the repository, where jj
# would record it.
tar_dir=$(cd "$(dirname -- "$tar_file")" 2>/dev/null && pwd -P) || {
  echo "::error::verify-build.sh: the directory of $tar_file does not exist" >&2
  exit 2
}
tar_file=$tar_dir/$(basename -- "$tar_file")
case $tar_dir/ in
  "$repo_root"/*)
    echo "::error::verify-build.sh: $tar_file is inside the repository ($repo_root); name a tar outside it, such as under \$TMPDIR" >&2
    exit 2
    ;;
esac

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
first=$scratch/first/packages/claude-envoy
second=$scratch/second

cd "$package_dir"
bun scripts/build.ts --out "$first"
bun scripts/build.ts --out "$second"
if ! differ=$(diff -r -q "$first/dist" "$second/dist" && diff -r -q "$first/legion" "$second/legion"); then
  echo "::error::verify-build.sh: two builds of this tree differ, so its build is not reproducible and the rebuild would land one run's output: ${differ//$'\n'/; }" >&2
  exit 1
fi

# The scratch git repository around the first build is there for the ignore check below. The
# private-name check runs from it only so the build sits at its repository path
# (packages/claude-envoy/dist/envoy-channel.js, the vendored-NATS exception's path): given paths,
# the check reads them with find and grep, not git. It finds the repository it scans from its own
# path, so a link to it sits in the scratch repository, and it scans the build's two directories
# alone.
mkdir -p "$scratch/first/.github/scripts"
ln -s "$repo_root/.github/scripts/check-private-names.sh" "$scratch/first/.github/scripts/"
git -c init.defaultBranch=main init -q "$scratch/first"
(cd "$scratch/first" && .github/scripts/check-private-names.sh packages/claude-envoy/dist packages/claude-envoy/legion)

# A generated file at a path a .gitignore matches (a skill's `logs/`, `out/`, `*.log` or
# `coverage`, force-added under skills/ and staged into legion/, or a file named by a .gitignore a
# skill ships inside legion/) is skipped by push-built's `git add -A`, so main would silently lack it
# while the tar holds it. The scratch repository gets this repository's .gitignore files on the path
# to the build, and git applies them and any .gitignore the build carries, as `git add -A` would.
# Nothing there is tracked, so git lists every ignored file of the two directories; a failing git
# stops the script.
for gitignore in .gitignore packages/.gitignore packages/claude-envoy/.gitignore; do
  if [ -f "$repo_root/$gitignore" ]; then
    cp "$repo_root/$gitignore" "$scratch/first/$gitignore"
  fi
done
ignored=$(git -C "$scratch/first" ls-files -o -i --exclude-standard -- packages/claude-envoy/dist packages/claude-envoy/legion) || {
  echo "::error::verify-build.sh: git ls-files could not list the ignored files of the build, so the build is not checked" >&2
  exit 1
}
if [ -n "$ignored" ]; then
  while IFS= read -r path; do
    printf '::error::verify-build.sh: %q is a generated file git ignores, so the rebuild would never commit it to main; rename its source, or make .gitignore stop matching it\n' "$path" >&2
  done <<<"$ignored"
  exit 1
fi

tar -cf "$tar_file" -C "$first" dist legion
"$repo_root/.github/scripts/release-push.sh" check-built "$tar_file"
echo "verify-build.sh: $tar_file holds a build main's rebuild accepts"
