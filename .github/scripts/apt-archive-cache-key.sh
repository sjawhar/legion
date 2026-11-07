#!/usr/bin/env bash
# Prints the Actions cache key the ~/apt-archives family is read and written under:
# apt-playwright-<the playwright-core version bun.lock pins>-<ImageOS>-<ImageVersion>.
#
# The key names the Playwright version because that version decides the apt list the entry holds
# (.github/playwright-apt-packages.txt), and the runner image because the image decides which
# versions apt resolves for those names, so an image roll misses the entry rather than restoring
# files the new image's indexes no longer name. The image is read from the runner's own
# environment rather than from a workflow expression, which cannot reach it.
#
# Two jobs run it, and that is why it is one script: ci.yaml's dispatch job, whose restore reads
# the entry, and .github/workflows/ci-caches.yaml's apt-debs job, whose save writes it on a push
# to main. A key that drifted between the two would make every restore miss and nothing would
# fail - the dispatch job would just fetch the whole 114 MB set again on every run.
#
# Its errors go to stderr, since its caller reads stdout for the key alone:
#   key=$(.github/scripts/apt-archive-cache-key.sh)
# Run from anywhere: it reads bun.lock from the repository root, whatever directory it is in.
set -euo pipefail
shopt -s inherit_errexit

cd "$(dirname "$0")/../.."

# The entry line in bun.lock reads `"playwright-core": ["playwright-core@1.64.1", ...`.
version=$(sed -n 's/^ *"playwright-core": \["playwright-core@\([^"]*\)".*/\1/p' bun.lock)
[ -n "$version" ] || {
  echo "::error::no playwright-core version in bun.lock, so the apt archive cache key" \
    "cannot name the Playwright version" >&2
  exit 1
}
# A GitHub hosted runner sets both, for example ubuntu24 and 20251027.1.0. Expanded with a
# default, since this script runs under `set -u` and an unset name must reach the message below
# rather than bash's own.
for name in ImageOS ImageVersion; do
  [ -n "${!name:-}" ] || {
    echo "::error::the runner set no $name, so the apt archive cache key cannot name" \
      "the image" >&2
    exit 1
  }
done
echo "apt-playwright-$version-$ImageOS-$ImageVersion"
