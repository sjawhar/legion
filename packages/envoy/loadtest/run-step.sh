#!/usr/bin/env bash
# run-step.sh: the one command for a named load step (README.md).
#
#   run-step.sh <name> --events <per s> --sessions <n> --requests <per s>
#     [--duration 180s] [--warmup 30s] [--grace 20s] [--event-mix <preset or kind=w,...>]
#     [--route-mix <file>] [--human 0.1] [--sse <n>] [--rng-seed <n>] [--out <dir>]
#
# From a fresh checkout it sets up, starts and seeds the stack (stack.sh). Then, every step: it
# rebuilds the binaries from this checkout (incremental), refuses a build whose highest migration is
# older than the seed's, resets the stack to the seed (stack.sh reset: database recreated from
# dispatch_seed, NATS on a fresh store, listener and Dispatch restarted), runs `loadgen run` and
# renders `loadgen tables` in the step directory, $LOADTEST_ROOT/runs/<name>/ unless --out names
# another (it must not exist or be empty). Dispatch's log is the step's dispatch.log.
#
# Dispatch is stopped when the step ends, pass or fail, so the box is idle between steps; Postgres,
# the proxy, NATS and the listener keep running (stack.sh stop stops them).
#
# One run at a time per LOADTEST_ROOT (lock.sh): a run-step.sh or run-series.sh on a root that
# another run holds is refused before it touches the stack. A step in which a stack process exited
# fails: loadgen run exits non-zero, and so does this script, naming the process; it still renders
# TABLES.md, where the row carries the mark.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
stack=$here/stack.sh
# shellcheck source=packages/envoy/loadtest/lock.sh
source "$here/lock.sh"
SRC=${LOADTEST_SRC:-$(cd -- "$here/../../.." && pwd -P)}
ROOT=${LOADTEST_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest}
mkdir -p -- "$ROOT"
ROOT=$(cd -- "$ROOT" && pwd -P)
export LOADTEST_ROOT=$ROOT LOADTEST_SRC=$SRC

die() {
  printf 'run-step.sh: %s\n' "$*" >&2
  exit 1
}
usage() {
  sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//' >&2
  exit 2
}

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
    --duration | --warmup | --grace | --event-mix | --human | --sse | --rng-seed) ;;
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

lock_root "$ROOT" run-step.sh

out=$(realpath -m -- "$out")
if [ -e "$out" ] && [ -n "$(ls -A -- "$out")" ]; then
  die "$out already holds a step; remove it or name another step"
fi

started=$SECONDS
"$stack" setup
"$stack" start
"$stack" seed

# A build cannot start on a seed a newer build migrated: compare the highest migration file of this
# checkout with the seed's level.
migrations=$SRC/packages/envoy/internal/dispatch/store/migrations
build_file=$(cd -- "$migrations" && printf '%s\n' [0-9]*.up.sql | sort -n | tail -n1)
build_level=$((10#${build_file%%_*}))
seed_level=$(jq -er .migration "$ROOT/seed.meta.json")
if ((build_level < seed_level)); then
  die "this build's highest migration is $build_level ($migrations/$build_file), older than the seed's $seed_level (seed made by $(jq -r .build_commit "$ROOT/seed.meta.json")); reseed with this build (stack.sh seed --force) or run a newer one"
fi
if ((build_level > seed_level)); then
  printf 'run-step.sh: note: the seed is at migration %s and this build at %s. Dispatch migrates the step'"'"'s copy forward at start, and any backfill those migrations start runs inside the step; reseed with this build (stack.sh seed --force) to measure it alone\n' \
    "$seed_level" "$build_level" >&2
fi

build_info=$("$stack" build-info)
read -r commit dirty <<<"$build_info"

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
  # loadgen writes summary.json once the step has run, also when a stack process exited in it.
  [ -f "$out/summary.json" ] || die "step $name failed: loadgen run exited $rc before it wrote summary.json; see $out/run.log"
  "$stack" loadgen tables "$out" || true
  exits=$(jq -r '[(.stack_exits // [])[] | "\(.name) (pid \(.pid))"] | join(", ")' "$out/summary.json")
  [ -n "$exits" ] || die "step $name failed: loadgen run exited $rc; see $out/run.log"
  die "step $name failed: $exits exited during the step, so its row in $out/TABLES.md does not measure the load"
fi
"$stack" loadgen tables "$out"
printf 'run-step.sh: step %s done in %d s: %s\n' "$name" "$((SECONDS - started))" "$out" >&2
