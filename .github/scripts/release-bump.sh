#!/usr/bin/env bash
# Computes the next release version from conventional commits, shared by the
# release workflows.
#
# Usage: release-bump.sh <prev-version> <range> [--patch-path <path>]... [-- <paths...>]
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
#
# --patch-path <path> (repeatable) names a path whose commits bump at least the
# patch version whatever their type: a path the package ships as runtime rather
# than documentation, such as skills/, which the plugins copy into their
# tarballs, so a docs: commit there still releases. A feat: or breaking commit
# there bumps minor or major as anywhere else. Each --patch-path must also be one
# of <paths>, since only commits touching <paths> are read.
set -euo pipefail

usage='usage: release-bump.sh <prev-version> <range> [--patch-path <path>]... [-- <paths...>]'
prev_version=${1:?$usage}
range=${2:?commit range (e.g. tag..HEAD, or HEAD for full history)}
shift 2

patch_paths=()
while [ $# -gt 0 ] && [ "$1" != "--" ]; do
  case $1 in
    --patch-path) patch_paths+=("${2:?--patch-path needs a path}"); shift 2 ;;
    *) echo "release-bump.sh: unknown argument '$1'; $usage" >&2; exit 2 ;;
  esac
done
for patch_path in "${patch_paths[@]}"; do
  if [ $# -gt 1 ] && ! grep -qxF -- "$patch_path" <<< "$(printf '%s\n' "${@:2}")"; then
    echo "release-bump.sh: --patch-path $patch_path is not one of the paths after --, so no commit touching it is read" >&2
    exit 2
  fi
done

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

if [ "$bump" = none ] && [ ${#patch_paths[@]} -gt 0 ] \
  && [ -n "$(git log -1 "$range" --format=%H -- "${patch_paths[@]}")" ]; then
  bump="patch"
fi

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
