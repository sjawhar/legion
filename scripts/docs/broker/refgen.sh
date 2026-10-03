#!/usr/bin/env bash
# scripts/docs/broker/refgen.sh <api|config|errors> <content dir>
#
# Writes <content dir>/broker/reference/<page>.md, a secrets broker reference page generated from
# the broker's Go source by refgen/ (see its main.go). docs/site/generators/broker-<page>.sh call
# it at site build. Needs Go; reads no network and nothing outside the repository.
set -euo pipefail

page="${1:?usage: refgen.sh <api|config|errors> <content dir>}"
content_dir="${2:?usage: refgen.sh <api|config|errors> <content dir>}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
out="$content_dir/broker/reference/$page.md"

mkdir -p "$(dirname "$out")"
trap 'rm -f "$out.tmp"' EXIT
GOWORK=off go -C "$repo_root/scripts/docs/broker/refgen" run . "$page" "$repo_root" >"$out.tmp"
mv "$out.tmp" "$out"
