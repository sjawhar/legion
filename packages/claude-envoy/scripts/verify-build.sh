#!/usr/bin/env bash
# verify-build.sh <tar> — build the plugin in place and check the build is one main's Rebuild
# Claude Envoy plugin workflow lands, then pack it into <tar>.
#
# This is the one statement of what main's rebuild must accept. The rebuild's build job
# (.github/workflows/claude-envoy-rebuild.yaml) runs it and hands <tar> to its push job, whose
# `release-push.sh push-built` checks the same tar again before it commits it; pr-and-main.yaml's
# required test job and envoy-and-contracts.yaml's claude-envoy job run the very same command on a
# pull request, so a pull request fails a check for whatever main's push job would refuse instead
# of merging and then failing every rebuild on main.
#
# In order, from the package directory and the repository root whatever the caller's directory:
#   1. `bun scripts/build.ts --in-place` rewrites the package's dist/ and legion/;
#   2. `bun scripts/build.ts --check` builds again and fails when the second build differs from
#      the first, so what lands is what the source builds to, not one run's accident;
#   3. .github/scripts/check-private-names.sh over the two directories;
#   4. no file under them is one git ignores (below);
#   5. `tar -cf <tar> -C packages/claude-envoy dist legion`, the tar the push job receives;
#   6. `.github/scripts/release-push.sh check-built <tar>`, push-built's own member checks: no
#      symlink, no executable file, no name outside letters, digits, '.', '_' and '-', and every
#      bundle and file a build holds (a skill pi-legion ships is staged into legion/ with `cp -R`,
#      which keeps its modes and symlinks).
# The in-place build stays in the checkout, as the rebuild's next step reads it with git status;
# <tar> is written outside the two directories, wherever the caller names. Any failure exits
# non-zero.
#
# Run from packages/claude-envoy: bun run verify-build <tar>
set -euo pipefail

[ $# -eq 1 ] || {
  echo "::error::verify-build.sh: usage: verify-build.sh <tar>" >&2
  exit 2
}
tar_file=$1
# Absolute before any cd, so a path relative to the caller's directory still names its file.
case $tar_file in /*) ;; *) tar_file=$PWD/$tar_file ;; esac

package_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
repo_root=$(cd "$package_dir/../.." && pwd -P)
built=(packages/claude-envoy/dist packages/claude-envoy/legion)

cd "$package_dir"
bun scripts/build.ts --in-place
bun scripts/build.ts --check

cd "$repo_root"
.github/scripts/check-private-names.sh "${built[@]}"

# A generated file at a path the repository's .gitignore matches (a skill's `logs/`, `out/`,
# `*.log` or `coverage`, force-added under skills/ and staged into legion/) is skipped by `git
# status` and by push-built's `git add -A`, so main would silently lack it while the tar holds it.
mapfile -d '' -t ignored < <(git ls-files -z -o -i --exclude-standard -- "${built[@]}")
wait "$!" || {
  echo "::error::verify-build.sh: listing the ignored files under ${built[*]} failed" >&2
  exit 1
}
if [ "${#ignored[@]}" -gt 0 ]; then
  for path in "${ignored[@]}"; do
    printf '::error::verify-build.sh: %q is a generated file git ignores, so the rebuild would never commit it to main; rename its source, or make .gitignore stop matching it\n' "$path" >&2
  done
  exit 1
fi

tar -cf "$tar_file" -C packages/claude-envoy dist legion
.github/scripts/release-push.sh check-built "$tar_file"
echo "verify-build.sh: $tar_file holds a build main's rebuild accepts"
