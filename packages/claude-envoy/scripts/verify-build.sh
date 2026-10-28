#!/usr/bin/env bash
# verify-build.sh <tar> — build the plugin from this tree, check the build is one main's Rebuild
# Claude Envoy plugin workflow lands, and pack it into <tar>, leaving the checkout as it found it.
#
# This is the one statement of what main's rebuild must accept. The rebuild's build job
# (.github/workflows/claude-envoy-rebuild.yaml) runs it and hands <tar> to its push job, whose
# `release-push.sh push-built` checks the same tar again before it commits it; pr-and-main.yaml's
# required test job and envoy-and-contracts.yaml's claude-envoy job run the very same command on a
# pull request, so a pull request fails a check for whatever main's push job would refuse instead
# of merging and then failing every rebuild on main.
#
# It writes no file of the checkout: a pull request never commits dist/ or legion/, and jj records
# whatever a command writes into the working copy. Every build goes into a scratch directory
# outside the repository, and <tar> must be outside it too. Only the rebuild workflow puts the
# build into its own checkout, by extracting the checked <tar> (its build job's next step).
#
# In order, whatever the caller's directory:
#   1. `bun scripts/build.ts --out` twice, into two scratch directories, and the two builds'
#      dist/ and legion/ compared byte for byte, so what lands is what the source builds to, not
#      one run's accident;
#   2. .github/scripts/check-private-names.sh over the first build, laid out at its repository
#      path (packages/claude-envoy/dist and legion) in a scratch git repository, since the check
#      reads through git and its vendored-NATS exception is tied to that path;
#   3. no built file is one this repository's .gitignore matches (below);
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

# The check finds the repository it scans from its own path, so a link to it sits in the scratch
# repository around the first build, and it scans that build's two directories alone.
mkdir -p "$scratch/first/.github/scripts"
ln -s "$repo_root/.github/scripts/check-private-names.sh" "$scratch/first/.github/scripts/"
git -c init.defaultBranch=main init -q "$scratch/first"
(cd "$scratch/first" && .github/scripts/check-private-names.sh packages/claude-envoy/dist packages/claude-envoy/legion)

# A generated file at a path this repository's .gitignore matches (a skill's `logs/`, `out/`,
# `*.log` or `coverage`, force-added under skills/ and staged into legion/) is skipped by push-built's
# `git add -A`, so main would silently lack it while the tar holds it. check-ignore answers for the
# path as git add would see it, a tracked path never being ignored: 0 when it printed one, 1 when
# none matched.
status=0
ignored=$(cd "$first/.." && find claude-envoy/dist claude-envoy/legion -type f -printf 'packages/%p\n' |
  git -C "$repo_root" check-ignore --stdin) || status=$?
case $status in
  0)
    while IFS= read -r path; do
      printf '::error::verify-build.sh: %q is a generated file git ignores, so the rebuild would never commit it to main; rename its source, or make .gitignore stop matching it\n' "$path" >&2
    done <<<"$ignored"
    exit 1
    ;;
  1) ;;
  *)
    echo "::error::verify-build.sh: git check-ignore exited $status, so the ignored files of the build could not be listed" >&2
    exit 1
    ;;
esac

tar -cf "$tar_file" -C "$first" dist legion
"$repo_root/.github/scripts/release-push.sh" check-built "$tar_file"
echo "verify-build.sh: $tar_file holds a build main's rebuild accepts"
