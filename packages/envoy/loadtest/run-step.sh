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
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
stack=$here/stack.sh
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

build_info=$("$stack" build-info)
read -r commit dirty <<<"$build_info"

mkdir -p -- "$out"
trap '"$stack" stop dispatch' EXIT
"$stack" reset "$out/dispatch.log"
"$stack" loadgen run -name "$name" -out "$out" -build-commit "$commit" -build-dirty="$dirty" "${args[@]}"
"$stack" stop dispatch
trap - EXIT
cp -- "$ROOT/logs/listener.out" "$out/listener.log"
cp -- "$ROOT/logs/nats.out" "$out/nats.log"
"$stack" loadgen tables "$out"
printf 'run-step.sh: step %s done in %d s: %s\n' "$name" "$((SECONDS - started))" "$out" >&2
