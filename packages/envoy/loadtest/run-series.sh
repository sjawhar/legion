#!/usr/bin/env bash
# run-series.sh: the load series, 0.25x to 10x, each step a fresh reset (README.md).
#
#   run-series.sh [step flags]
#
# Runs six steps with run-step.sh, in order, into $LOADTEST_ROOT/runs/series-<UTC time>/step-<f>x/,
# then one TABLES.md across them in the series directory:
#
#   step        events/s  sessions  requests/s  SSE
#   step-0.25x         1       100          15    1
#   step-0.5x          2       200          30    1
#   step-1x            4       400          60    2
#   step-2x            8       467         120    4
#   step-5x           20       667         300   10
#   step-10x          40     1,000         600   20
#
# Events are 4/s and requests 60/s at 1x, SSE max(1, round(2·f)); sessions come from
# `loadgen series-sessions <f>`. Any other run-step.sh flag (--duration, --warmup, --grace,
# --event-mix, --route-mix, --human, --rng-seed) is passed to every step.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
stack=$here/stack.sh
ROOT=${LOADTEST_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest}
mkdir -p -- "$ROOT"
ROOT=$(cd -- "$ROOT" && pwd -P)
export LOADTEST_ROOT=$ROOT

die() {
  printf 'run-series.sh: %s\n' "$*" >&2
  exit 1
}

for arg in "$@"; do
  case $arg in
    --events | --sessions | --requests | --sse | --out) die "$arg is set by the series for each step; it cannot be passed through" ;;
    -h | --help)
      sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//' >&2
      exit 2
      ;;
  esac
done

# Setup and seed once, before the series directory exists, so a failed setup leaves no empty series.
"$stack" setup
"$stack" start
"$stack" seed

series=$ROOT/runs/series-$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p -- "$series"
dirs=()
#       factor events requests sse
steps=("0.25 1 15 1" "0.5 2 30 1" "1 4 60 2" "2 8 120 4" "5 20 300 10" "10 40 600 20")
for step in "${steps[@]}"; do
  read -r f events requests sse <<<"$step"
  sessions=$("$stack" loadgen series-sessions "$f")
  dir=$series/step-${f}x
  "$here/run-step.sh" "step-${f}x" --events "$events" --sessions "$sessions" --requests "$requests" \
    --sse "$sse" --out "$dir" "$@"
  dirs+=("$dir")
done
"$stack" loadgen tables -out "$series/TABLES.md" "${dirs[@]}"
printf 'run-series.sh: series done: %s\n' "$series" >&2
