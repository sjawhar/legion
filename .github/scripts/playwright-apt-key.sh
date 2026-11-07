#!/usr/bin/env bash
# The key the apt archive cache of the Dispatch browser suite is read and written under, printed
# on stdout:
#   apt-playwright-<playwright-core version>-<ImageOS>-<ImageVersion>
# for example apt-playwright-1.64.1-ubuntu24-20261101.360.1, the entry run 42349037034's shard 1
# restored (`Cache hit for: apt-playwright-1.64.1-ubuntu24-20261101.360.1`).
#
# Who runs it, and why it is a script: the "Compute the apt archive cache key" step of
# .github/workflows/dispatch-browser.yaml's shard job and the step of that name in its alone
# job. The two jobs held this body word for word, so a fix to the key landed in one copy only;
# written once here, both steps call it. It is a script of its own and not a mode of
# .github/scripts/playwright-install.sh: that one installs packages, reached through
# $GITHUB_WORKSPACE from packages/dispatch at install time, while this one only computes a
# string, at the checkout root and before the restore step those packages come out of.
#
# It is computed in a step at all, rather than in the `key:` expression of the cache step,
# because a step expression cannot read the runner's own environment: ImageOS and ImageVersion
# are environment variables the hosted runner sets, not `runner.*` context.
#
# Why the key names both:
#   * the Playwright version, read from bun.lock, because the apt list the install step installs
#     is Playwright's own ubuntu24.04-x64 list for that version, so a new version is a new set
#     of .deb files;
#   * the runner image, ImageOS and ImageVersion, because a .deb fetched for one image is the
#     version of that image's apt indexes, which the next image bumps.
# The two cache steps that use it pass no `restore-keys`, for that same reason: a partial match
# would hand the install the .deb files of another Playwright version or another image, which
# apt would fetch again anyway, so the entry would cost a download and save nothing.
#
# It prints the key rather than writing $GITHUB_OUTPUT itself, so that every case of its test
# beside it reads one string off stdout with no fake output file, and so that a person can run
# it on a checkout and see the key. Each caller writes the line, in two lines of its own:
#   key=$(.github/scripts/playwright-apt-key.sh)
#   echo "key=$key" >> "$GITHUB_OUTPUT"
# The assignment is its own line because `echo "key=$(...)" >> ...` is a command whose status is
# echo's, so under GitHub's `bash -e {0}` a refusal here would write an empty key and the job
# would carry on.
#
# Refusals go to stderr, not stdout, for the same reason: the caller reads stdout, so an
# `::error::` written there would be captured into the key instead of reaching the log.
#
# Run from the checkout root, as both steps do - neither sets a `working-directory` - since
# bun.lock is read by that name.
set -euo pipefail

if [ "$#" -ne 0 ]; then
  echo "::error::playwright-apt-key.sh takes no argument and was given $#:" \
    ".github/scripts/playwright-apt-key.sh, from the checkout root" >&2
  exit 1
fi

# The entry line in bun.lock reads `"playwright-core": ["playwright-core@1.64.1", ...`.
version=$(sed -n 's/^ *"playwright-core": \["playwright-core@\([^"]*\)".*/\1/p' bun.lock)
[ -n "$version" ] || {
  echo "::error::no playwright-core version in bun.lock, so the apt archive cache key" \
    "cannot name the Playwright version" >&2
  exit 1
}
# A GitHub hosted runner sets both, for example ubuntu24 and 20251027.1.0. `${!name:-}` and not
# `${!name}`: under this script's `set -u` the bare form aborts on the unset one with bash's own
# message instead of the line below naming it.
for name in ImageOS ImageVersion; do
  [ -n "${!name:-}" ] || {
    echo "::error::the runner set no $name, so the apt archive cache key cannot name" \
      "the image" >&2
    exit 1
  }
done
echo "apt-playwright-$version-$ImageOS-$ImageVersion"
