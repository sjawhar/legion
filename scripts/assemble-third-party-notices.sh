#!/bin/sh
# Writes <out>: one notices file made of several, so an image that ships many artifacts carries the
# license of every third-party piece in one documented file. <intro> opens it; each <title> heads
# its <file>, copied in whole. A missing or empty file fails it, writing nothing.
#
#   scripts/assemble-third-party-notices.sh <out> <intro> <title> <file> [<title> <file>]...
#
# POSIX sh, so it runs in the Debian and Alpine build stages alike.
set -eu

if [ "$#" -lt 4 ] || [ $((($# - 2) % 2)) -ne 0 ]; then
  echo "usage: scripts/assemble-third-party-notices.sh <out> <intro> <title> <file> [<title> <file>]..." >&2
  exit 2
fi
out=$1
intro=$2
shift 2

rule="$(printf '%080d' 0 | tr 0 '#')"
# A directory, not `mktemp`'s file: that is created 0600, and the result is read by an image's
# unprivileged user.
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
{
  printf '%s\n' "$intro"
  while [ "$#" -gt 0 ]; do
    if [ ! -s "$2" ]; then
      echo "assemble-third-party-notices: $2 ($1) is missing or empty" >&2
      exit 1
    fi
    printf '\n%s\n%s\n%s\n\n' "$rule" "$1" "$rule"
    cat "$2"
    shift 2
  done
} >"$scratch/notices"
mv "$scratch/notices" "$out"
