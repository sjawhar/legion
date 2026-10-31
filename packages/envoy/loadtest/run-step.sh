#!/usr/bin/env bash
# run-step.sh: the one command for a named load step (README.md).
#
#   run-step.sh <name> --events <per s> --sessions <n> --requests <per s>
#     [--duration 180s] [--warmup 30s] [--grace 20s] [--event-mix <preset or kind=w,...>]
#     [--route-mix <file>] [--human 0.1] [--live <n>] [--rng-seed <n>] [--xact-interval 10ms] [--out <dir>]
#
#   run-step.sh <name> --replay [--rate F] [--reads RHO] [--read-mix M] [--uploads U]
#     [--rng-seed N] [--from D --to D] [--roster N] [--keys-file FILE] [--warmup D]
#     [--gomaxprocs N] [--dispatch-commit REV] [--manifest FILE] [--out <dir>]
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
# A --replay step replays the LINUX bundle (README.md, The replay) instead: stack.sh setup
# (incremental; with --dispatch-commit, envoy-dispatch from that commit, a rev that names no commit
# refused before anything starts), start, replay-get (the manifest's parts, default
# replay-2026-10-26.json; nothing to do once done; LOADTEST_REPLAY_ISSUE names the issue they are
# file artifacts of and is required, refused here before anything starts), replay-prepare (this
# build's template, once per build) and reset --replay, then `loadgen run -source replay` with the
# -replay-* flags: --rate, --reads, --read-mix, --uploads, --from, --to, --roster and --keys-file
# are -replay-rate, -replay-reads, -replay-read-mix, -replay-uploads, -replay-from, -replay-to,
# -replay-roster and -replay-keys-file. --events, --sessions, --requests and the other flags of
# the generated load are refused; the synthetic seed and its migration check are not used.
# Dispatch reaches Postgres directly (LOADTEST_PG_DIRECT=1) unless the environment sets
# LOADTEST_PG_DIRECT; --gomaxprocs sets LOADTEST_GOMAXPROCS. The reset's replay/last-reset.json is
# copied into the step directory.
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

replay=false
for flag in "$@"; do
  [ "$flag" = --replay ] && replay=true
done

out=$ROOT/runs/$name
args=()
have_events=false have_sessions=false have_requests=false
manifest="" gomaxprocs="" dispatch_rev=""
while [ $# -gt 0 ]; do
  flag=$1
  if [ "$flag" = --replay ]; then
    shift
    continue
  fi
  [ $# -ge 2 ] || die "$flag needs a value"
  value=$2
  shift 2
  if [ "$flag" = --out ]; then
    out=$value
    continue
  fi
  if $replay; then
    case $flag in
      --events | --sessions | --requests | --duration | --grace | --event-mix | --route-mix | --human | --live)
        die "$flag is not a replay flag: a --replay step sends the bundle's logged writes"
        ;;
      --rate | --reads | --read-mix | --uploads | --from | --to | --roster) flag=--replay-${flag#--} ;;
      --keys-file)
        flag=--replay-keys-file
        value=$(abs_file --keys-file "$value")
        ;;
      --warmup | --rng-seed) ;;
      --gomaxprocs)
        [[ $value =~ ^[1-9][0-9]*$ ]] || die "--gomaxprocs '$value' is not a positive integer"
        gomaxprocs=$value
        continue
        ;;
      --dispatch-commit)
        dispatch_rev=$value
        continue
        ;;
      --manifest)
        manifest=$(abs_file --manifest "$value")
        continue
        ;;
      *) die "unknown flag '$flag'" ;;
    esac
  else
    case $flag in
      --events) have_events=true ;;
      --sessions) have_sessions=true ;;
      --requests) have_requests=true ;;
      --duration | --warmup | --grace | --event-mix | --human | --live | --rng-seed | --xact-interval) ;;
      --route-mix) value=$(abs_file --route-mix "$value") ;;
      *) die "unknown flag '$flag'" ;;
    esac
  fi
  args+=("-${flag#--}" "$value")
done
if ! $replay; then
  $have_events || die "--events is required (0 sends no writes)"
  $have_sessions || die "--sessions is required"
  $have_requests || die "--requests is required (0 sends no requests)"
fi
if [ -n "$dispatch_rev" ]; then
  dispatch_commit=$(git -C "$SRC" rev-parse --verify --quiet "$dispatch_rev^{commit}" 2>/dev/null) ||
    die "--dispatch-commit '$dispatch_rev' names no commit in $SRC"
fi
if $replay; then
  # stack.sh replay-get takes the issue the replay's parts are file artifacts of from the
  # environment, as the manifest names none; refuse here too, before the step touches the stack.
  [ -n "${LOADTEST_REPLAY_ISSUE:-}" ] ||
    die "LOADTEST_REPLAY_ISSUE is required for a --replay step: set it to the issue the replay's parts are file artifacts of"
fi

lock_root "$ROOT"

out=$(realpath -m -- "$out")
if [ -e "$out" ] && [ -n "$(ls -A -- "$out")" ]; then
  die "$out already holds a step; remove it or name another step"
fi

started=$SECONDS
if $replay; then
  # The caller's LOADTEST_PG_DIRECT wins; meta.json's pg_direct records what Dispatch used.
  export LOADTEST_PG_DIRECT=${LOADTEST_PG_DIRECT-1}
  [ -z "$gomaxprocs" ] || export LOADTEST_GOMAXPROCS=$gomaxprocs
  [ -z "$dispatch_rev" ] || export LOADTEST_DISPATCH_COMMIT=$dispatch_commit
  get_args=()
  [ -z "$manifest" ] || get_args=(--manifest "$manifest")
  "$stack" setup
  "$stack" start
  "$stack" replay-get "${get_args[@]}"
  "$stack" replay-prepare
  BUILD=$("$stack" build-info)
elif $LOCK_INHERITED && [ -n "${LOADTEST_SERIES_BUILD:-}" ]; then
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
if $replay; then
  "$stack" reset --replay "$out/dispatch.log"
  cp -- "$ROOT/replay/last-reset.json" "$out/last-reset.json"
  args=(-source replay -events 0 -requests 0 -replay-inputs "$ROOT/replay/bundle" "${args[@]}")
else
  "$stack" reset "$out/dispatch.log"
fi
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
