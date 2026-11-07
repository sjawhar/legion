#!/usr/bin/env bash
# run-series.sh: the load series, 0.25x to 10x, each step a fresh reset (README.md).
#
#   run-series.sh [step flags]
#
# Runs six steps with run-step.sh, in order, into $LOADTEST_ROOT/runs/series-<UTC time>/step-<f>x/,
# then one TABLES.md across them in the series directory:
#
#   step        events/s  sessions  requests/s  live sockets
#   step-0.25x         1       100          15             1
#   step-0.5x          2       200          30             1
#   step-1x            4       400          60             2
#   step-2x            8       467         120             4
#   step-5x           20       667         300            10
#   step-10x          40     1,000         600            20
#
# Events are 4/s and requests 60/s at 1x, live sockets max(1, round(2·f)); sessions come from
# `loadgen series-sessions <f>`. Every other flag is passed to every step as given, and run-step.sh
# refuses one it does not know: --duration, --warmup, --grace, --event-mix, --route-mix, --human,
# --rng-seed, --schedule and --recovery-ref-p99. With --schedule each step's factors multiply that
# step's events and requests; its live sockets stay the series' max(1, round(2·f)).
#
# The series holds LOADTEST_ROOT for its whole run (common.sh); its own run-step.sh calls pass, and
# keep the lock if the series is killed. It sets up (builds this checkout), starts and seeds the
# stack and runs the migration check once, before its first step, and passes that build's commit
# and dirty flag to every step in LOADTEST_SERIES_BUILD; its steps skip setup, so every step of a
# series runs one build, even when the checkout changes during the series. A failed step stops the
# series: the series TABLES.md covers the steps that ran, the failed one included when it wrote
# summary.json, and the script exits 1 naming the step.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
stack=$here/stack.sh
# shellcheck source=packages/envoy/loadtest/common.sh
source "$here/common.sh"
resolve_paths

for arg in "$@"; do
  case $arg in
    --events | --sessions | --requests | --live | --out) die "$arg is set by the series for each step; it cannot be passed through" ;;
    -h | --help) usage ;;
  esac
done

lock_root "$ROOT"

# Setup and seed once, before the series directory exists, so a failed setup leaves no empty series.
prepare_stack
export LOADTEST_SERIES_BUILD=$BUILD

series=$ROOT/runs/series-$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p -- "$series"
dirs=()
failed=""
#       factor events requests live
steps=("0.25 1 15 1" "0.5 2 30 1" "1 4 60 2" "2 8 120 4" "5 20 300 10" "10 40 600 20")
for step in "${steps[@]}"; do
  read -r f events requests live <<<"$step"
  sessions=$("$stack" loadgen series-sessions "$f")
  dir=$series/step-${f}x
  if ! "$here/run-step.sh" "step-${f}x" --events "$events" --sessions "$sessions" --requests "$requests" \
    --live "$live" --out "$dir" "$@"; then
    failed=step-${f}x
    [ -f "$dir/summary.json" ] && dirs+=("$dir")
    break
  fi
  dirs+=("$dir")
done
# Each step's run-step.sh already wrote its pprof tops and goroutine summary.
if [ ${#dirs[@]} -gt 0 ]; then
  "$stack" loadgen tables -tops=false -out "$series/TABLES.md" "${dirs[@]}"
fi
if [ -n "$failed" ]; then
  [ ${#dirs[@]} -gt 0 ] || die "step $failed failed (run-step.sh above names why); the series stopped there, before any step wrote summary.json"
  die "step $failed failed (run-step.sh above names why); the series stopped there; $series/TABLES.md holds the steps that ran"
fi
note "series done: $series"
