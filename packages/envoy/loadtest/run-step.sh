#!/usr/bin/env bash
# run-step.sh: the one command for a named load step (README.md).
#
#   run-step.sh <name> --events <per s> --sessions <n> --requests <per s>
#     [--duration 180s] [--warmup 30s] [--grace 20s] [--event-mix <preset or kind=w,...>]
#     [--route-mix <file>] [--human 0.1] [--live <n>] [--rng-seed <n>] [--out <dir>]
#
# From a fresh checkout it sets up, starts and seeds the stack (stack.sh). Then, every step that is
# not part of a series: it rebuilds the binaries from this checkout (stack.sh setup, incremental) and
# refuses a build whose highest migration is older than the seed's. A step of run-series.sh skips
# that: the series did it once, before its first step, and passes its build's commit and dirty flag
# in LOADTEST_SERIES_BUILD, which a run-step.sh that does not hold the series' lock (common.sh)
# ignores. Every step then resets the stack to the seed (stack.sh reset: database recreated from
# dispatch_seed, NATS on a fresh store, listener and Dispatch restarted), runs `loadgen run` and
# renders `loadgen tables` in the step directory, $LOADTEST_ROOT/runs/<name>/ unless --out names
# another (it must not exist or be empty). Dispatch's log is the step's dispatch.log.
#
# Dispatch is stopped when the step ends, pass or fail, so the box is idle between steps; Postgres,
# the proxy, NATS and the listener keep running (stack.sh stop stops them).
#
# One run at a time per LOADTEST_ROOT (common.sh): a run-step.sh or run-series.sh on a root that
# another run holds is refused before it touches the stack. A step in which a stack process exited,
# or whose commit rows (loadtest.event_commits) could not be read, fails: loadgen run exits non-zero
# naming why, and so does this script; it still renders TABLES.md, where the row carries the mark.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
stack=$here/stack.sh
# shellcheck source=packages/envoy/loadtest/common.sh
source "$here/common.sh"
resolve_paths

[ $# -ge 1 ] || usage
name=$1
shift
case $name in
  -h | --help) usage ;;
  -* | "") die "the step name comes first, before the flags, not '$name'" ;;
esac
[[ $name =~ ^[A-Za-z0-9._-]+$ ]] || die "step name '$name' may hold only letters, digits, '.', '_' and '-'"

out=$ROOT/runs/$name
args=()
have_events=false have_sessions=false have_requests=false
while [ $# -gt 0 ]; do
  flag=$1
  [ $# -ge 2 ] || die "$flag needs a value"
  value=$2
  shift 2
  case $flag in
    --events) have_events=true ;;
    --sessions) have_sessions=true ;;
    --requests) have_requests=true ;;
    --duration | --warmup | --grace | --event-mix | --human | --live | --rng-seed) ;;
    --route-mix)
      [ -f "$value" ] || die "--route-mix names $value, which is not a file"
      value=$(cd -- "$(dirname -- "$value")" && pwd -P)/$(basename -- "$value")
      ;;
    --out)
      out=$value
      continue
      ;;
    *) die "unknown flag '$flag'" ;;
  esac
  args+=("-${flag#--}" "$value")
done
$have_events || die "--events is required (0 sends no writes)"
$have_sessions || die "--sessions is required"
$have_requests || die "--requests is required (0 sends no requests)"

lock_root "$ROOT"

out=$(realpath -m -- "$out")
if [ -e "$out" ] && [ -n "$(ls -A -- "$out")" ]; then
  die "$out already holds a step; remove it or name another step"
fi

started=$SECONDS
if $LOCK_INHERITED && [ -n "${LOADTEST_SERIES_BUILD:-}" ]; then
  # A step of run-series.sh, which holds the lock this script inherited: the series set up, started
  # and seeded the stack once, and every step runs that build.
  BUILD=$LOADTEST_SERIES_BUILD
else
  unset LOADTEST_SERIES_BUILD
  prepare_stack
fi
read -r commit dirty <<<"$BUILD"

mkdir -p -- "$out"
# The box's busy share before this step starts anything: Dispatch is stopped (the last step stopped
# it) and the rest of the stack is idle, so what it shows is other work on the machine. Read after
# the reset, it would show Dispatch's own start-up work instead.
box_busy=$("$stack" loadgen box-busy)
trap '"$stack" stop dispatch' EXIT
"$stack" reset "$out/dispatch.log"
rc=0
"$stack" loadgen run -name "$name" -out "$out" -build-commit "$commit" -build-dirty="$dirty" -box-busy-before "$box_busy" "${args[@]}" || rc=$?
"$stack" stop dispatch
trap - EXIT
cp -- "$ROOT/logs/listener.out" "$out/listener.log"
cp -- "$ROOT/logs/nats.out" "$out/nats.log"
if ((rc != 0)); then
  # loadgen run printed why above (a stack process that exited during the step, commit rows it
  # could not read, or its error).
  [ -f "$out/summary.json" ] || die "step $name failed: loadgen run exited $rc before it wrote summary.json (its message above says why); see $out/run.log"
  "$stack" loadgen tables "$out" || true
  die "step $name failed: loadgen run exited $rc (its message above says why); $out/TABLES.md holds the step's row for diagnosis; see $out/run.log"
fi
"$stack" loadgen tables "$out"
note "step $name done in $((SECONDS - started)) s: $out"
