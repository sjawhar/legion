#!/usr/bin/env bash
# Every path this repository's workflows and docs build derive from the repository's own
# owner/name - the image it pushes, the release download it points Pulumi at, the URL it tells npm
# it publishes from, the site GitHub Pages serves it at - computed here from $GITHUB_REPOSITORY,
# so that moving the repository to another owner or name is one environment value and not a sweep
# of literals through workflows, Dockerfiles and the docs config. A caller asks for one key
# (`repo-paths.sh docs_base`) or for all of them (`repo-paths.sh`, one `key=value` per line).
#
# $GITHUB_REPOSITORY is set in every GitHub Actions job. Off a runner - a local `bun run
# docs:build`, a shell - it is unset, and the default below is today's name, so the local build
# gives today's site. A value that is not exactly <owner>/<name> is refused rather than pasted
# into a URL or an image reference.
#
# The four rules a reader would otherwise have to guess at, and what each is for:
#
#   * plugin_download_url ends in the nine literal characters ${VERSION}. That is Pulumi's own
#     placeholder in a plugin download URL, which Pulumi substitutes with the plugin version at
#     install time, so this script must emit it uninterpolated - hence the single quotes around it
#     below, and hence a caller must not pass it through a second round of shell expansion.
#   * worker_image and envoy_image lowercase nothing. ghcr.io rejects an upper-case path, but the
#     owner and name here are already lowercase, and $GITHUB_REPOSITORY carries the owner as
#     GitHub spells it, so lowercasing would only hide the day an owner is not.
#   * repository_url spells the owner exactly as $GITHUB_REPOSITORY does, because npm matches a
#     package's repository.url against the repository publishing it case-sensitively (npm docs,
#     "Generating provenance statements", Prerequisites): a case-folded owner there fails the
#     publish with provenance, which is how this repository publishes.
#   * docs_site lowercases the owner, because GitHub Pages serves a site from a lowercased owner
#     (https://cybermonklabs.github.io, never https://CyberMonkLabs.github.io).
#
# Run from anywhere: .github/scripts/repo-paths.sh [key]
# CI runs its tests in ci.yaml's test job; the workflows and the docs build read it for the paths
# above instead of carrying them as literals.
set -euo pipefail

cd "$(dirname "$0")/../.."

repository=${GITHUB_REPOSITORY:-sjawhar/legion}
# One slash, both halves non-empty, and no whitespace anywhere: everything below pastes this into
# a URL or an image reference, where a second slash or a space is a different path, not an error.
if [[ ! $repository =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]]; then
  echo "repo-paths.sh: GITHUB_REPOSITORY is '$repository', which is not <owner>/<name>" >&2
  exit 1
fi
owner=${repository%%/*}
name=${repository#*/}

keys=(
  worker_image
  envoy_image
  plugin_download_url
  repository_url
  docs_site
  docs_base
  docs_edit_base
)
# shellcheck disable=SC2016 # the literal ${VERSION} is Pulumi's placeholder, substituted by Pulumi
values=(
  "ghcr.io/${repository}-worker"
  "ghcr.io/${repository}/envoy"
  "https://github.com/${repository}/releases/download/envoy-infra-v"'${VERSION}'
  "https://github.com/${repository}"
  "https://${owner,,}.github.io"
  "/${name}"
  "https://github.com/${repository}/edit/main/docs/site/"
)

if [ $# -gt 1 ]; then
  echo "repo-paths.sh: takes one key or none, got $#: $*" >&2
  echo "repo-paths.sh: known keys: ${keys[*]}" >&2
  exit 1
fi

if [ $# -eq 1 ]; then
  for index in "${!keys[@]}"; do
    if [ "${keys[index]}" = "$1" ]; then
      printf '%s\n' "${values[index]}"
      exit 0
    fi
  done
  echo "repo-paths.sh: unknown key '$1'" >&2
  echo "repo-paths.sh: known keys: ${keys[*]}" >&2
  exit 1
fi

for index in "${!keys[@]}"; do
  printf '%s=%s\n' "${keys[index]}" "${values[index]}"
done
