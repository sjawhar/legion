#!/usr/bin/env bash
# Computes the next release version from conventional commits, shared by the
# release workflows.
#
# Usage: release-bump.sh <prev-version> <range> [-- <paths...>]
#
# Prints exactly one token:
#   X.Y.Z  next version, when a version-bumping commit exists in the range
#   none   commits exist in the range but none bump the version
#   empty  no commits touch the given paths in the range
#
# Both commit subjects and bodies are classified. Squash merges carry the
# original commit subjects as "* type: summary" bullet lines in the squash
# body, so a squash-merged PR with a non-conventional title still releases;
# a "BREAKING CHANGE:" footer in a body bumps the major version, per the
# conventional-commits spec.
set -euo pipefail

prev_version=${1:?usage: release-bump.sh <prev-version> <range> [-- <paths...>]}
range=${2:?commit range (e.g. tag..HEAD, or HEAD for full history)}
shift 2

messages=$(git log "$range" --format='%s%n%b' "$@" | sed -E 's/^[*-] +//')
if [ -z "$(printf '%s' "$messages" | tr -d '[:space:]')" ]; then
  echo "empty"
  exit 0
fi

bump=none
while IFS= read -r msg; do
  if printf '%s\n' "$msg" | grep -qE '^[a-z]+(\(.+\))?!:|^BREAKING[- ]CHANGE:'; then
    bump=major
    break
  elif printf '%s\n' "$msg" | grep -qE '^feat(\(.+\))?:'; then
    bump=minor
  elif printf '%s\n' "$msg" | grep -qE '^(fix|refactor|perf)(\(.+\))?:'; then
    [ "$bump" = none ] && bump="patch"
  fi
done <<< "$messages"

if [ "$bump" = none ]; then
  echo "none"
  exit 0
fi

IFS='.' read -r major minor patch <<< "$prev_version"
case "$bump" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
echo "${major}.${minor}.${patch}"
