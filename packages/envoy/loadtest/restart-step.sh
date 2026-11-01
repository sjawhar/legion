#!/usr/bin/env bash
# restart-step.sh: one load step, on the stack's own processes, whose listener is killed in the
# middle of the window and started again, and the report of what the sessions then got (README.md,
# "The listener restart").
#
#   restart-step.sh <name> [--events 40] [--sessions 1000] [--requests 600] [--duration 300s]
#     [--out <dir>] [--warm-at 60] [--kill-at 120] [--down 30] [--backlog-per 3]
#     [--backlog-over 25s] [--warm-over 10s] [other run-step.sh flags]
#
# --warm-at, --kill-at and --down are whole seconds; --duration, --backlog-over and --warm-over are
# durations as Go reads them ("25s", "1m30s").
#
# The step is a run-step.sh step under consumer delivery (LOADTEST_SESSION_DELIVERY=consumer, which
# this script sets). Counted from the window start:
#
#   --warm-at   one envelope per session (`loadgen backlog -per 1`), so every session has answered
#               a frame done and holds a resume point, as a session running for a while does. With
#               LOADTEST_NO_RESUME=1 this step is left out: no session holds a resume point, and
#               its registrations carry none.
#   --kill-at   the listener is SIGKILLed.
#   +2 s        the backlog: --backlog-per envelopes to each session over --backlog-over, so every
#               session has a backlog while the listener is down.
#   --down      after the kill, the listener is started again with the same binary, arguments and
#               environment (read from /proc/<pid>), its output appended to the stack's listener
#               log and its pid in the stack's pidfile, so `stack.sh stop` stops the new one. A
#               start that exits at once (on gVisor the killed listener's port can still be held:
#               "bind: address already in use") is started again after 1 s; the restart time is the
#               start that stayed up, and restart.json counts the attempts.
#
# The step itself fails by design: its sampler sees the listener's pid exit (summary.json
# `stack_exits`), and run-step.sh exits non-zero. It still writes summary.json, relay-frames.csv
# and TABLES.md, and this script then runs `loadgen restart-report`, which writes
# restart-report.json in the step directory and fails when a relay frame would reach a session's
# transcript twice. A step that failed for any other reason fails this script.
#
# The step directory holds, beside what a step writes: restart.json (the kill, the start that
# stayed up and its first answer, in Unix ns, and the start attempts), backlog.json and warm.json
# (the backlog runs' summaries) and restart-report.json.
#
# One run at a time per LOADTEST_ROOT: this script takes the root's lock (common.sh) and the
# run-step.sh it starts inherits it. A failure of this script ends that step, so the root is free
# for the next run instead of being held by a step that measures on alone.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
stack=$here/stack.sh
# shellcheck source=packages/envoy/loadtest/common.sh
source "$here/common.sh"
resolve_paths

[ $# -ge 1 ] || usage
name=$1
shift
[[ $name =~ ^[A-Za-z0-9._-]+$ ]] || die "step name '$name' may hold only letters, digits, '.', '_' and '-'"

events=40 sessions=1000 requests=600 duration=300s
warm_at=60 kill_at=120 down=30 backlog_per=3 backlog_over=25s warm_over=10s
out=$ROOT/runs/$name
step_args=()
while [ $# -gt 0 ]; do
  flag=$1
  [ $# -ge 2 ] || die "$flag needs a value"
  value=$2
  shift 2
  case $flag in
    --events) events=$value ;;
    --sessions) sessions=$value ;;
    --requests) requests=$value ;;
    --duration) duration=$value ;;
    --out) out=$value ;;
    --warm-at) warm_at=$value ;;
    --kill-at) kill_at=$value ;;
    --down) down=$value ;;
    --backlog-per) backlog_per=$value ;;
    --backlog-over) backlog_over=$value ;;
    --warm-over) warm_over=$value ;;
    *) step_args+=("$flag" "$value") ;;
  esac
done
for seconds in warm_at kill_at down; do
  [[ ${!seconds} =~ ^[0-9]+$ ]] || die "--${seconds//_/-} is a whole number of seconds, not '${!seconds}'"
done
((warm_at < kill_at)) || die "--warm-at ($warm_at s) must come before --kill-at ($kill_at s)"

lock_root "$ROOT"
out=$(realpath -m -- "$out")
RUN=$ROOT/run
LOGS=$ROOT/logs
export LOADTEST_SESSION_DELIVERY=consumer
no_resume=false
if [ "${LOADTEST_NO_RESUME:-}" = 1 ]; then no_resume=true; fi

# backlog PER OVER OUT [ID-BASE]: publish PER envelopes to each session over OVER, as the stack's
# own loadgen, and keep the summary in OUT.
backlog() {
  local args=(-sessions "$sessions" -per "$1" -over "$2" -out "$3")
  if [ $# -ge 4 ]; then args+=(-id-base "$4"); fi
  "$stack" loadgen backlog "${args[@]}" >/dev/null
}

# sleep_until SECONDS: wait for that Unix time; a time already past waits not at all.
sleep_until() {
  local left=$(($1 - $(date +%s)))
  if ((left > 0)); then sleep "$left"; fi
}

# kill_and_restart: SIGKILL the stack's listener, keep it down, start it again with the same
# binary, arguments and environment, and write restart.json.
kill_and_restart() {
  local pidf=$RUN/listener.pid pid port token envs cmd
  pid=$(cat -- "$pidf")
  [ -r "/proc/$pid/environ" ] || die "the stack's listener (pid $pid) is not running; nothing to restart"
  mapfile -d '' envs <"/proc/$pid/environ"
  mapfile -d '' cmd <"/proc/$pid/cmdline"
  port=$(jq -er .ports.listener "$ROOT/stack.json")
  token=$(jq -er .listener_token "$ROOT/stack.json")
  local kill_ns first_ns restart_ns ready_ns attempts=0 new up
  kill_ns=$(date +%s%N)
  kill -KILL "$pid"
  while [ -e "/proc/$pid" ] && ! grep -q '^State:.*Z' "/proc/$pid/status" 2>/dev/null; do sleep 0.05; done
  note "killed the listener (pid $pid); it stays down $down s"
  sleep "$down"
  first_ns=$(date +%s%N)
  while :; do
    attempts=$((attempts + 1))
    restart_ns=$(date +%s%N)
    rm -f -- "$pidf"
    # stack.sh spawn's own start, so the pidfile names the listener and the lock (descriptor 9) is
    # not inherited.
    # shellcheck disable=SC2016
    setsid -f env -i "${envs[@]}" /bin/sh -c 'echo "$$" >"$0.tmp" && mv -f -- "$0.tmp" "$0" && exec "$@"' \
      "$pidf" "${cmd[@]}" >>"$LOGS/listener.out" 2>&1 </dev/null 9>&-
    until [ -s "$pidf" ]; do sleep 0.02; done
    new=$(cat -- "$pidf")
    up=false
    while [ -e "/proc/$new" ] && ! grep -q '^State:.*Z' "/proc/$new/status" 2>/dev/null; do
      if curl -fsS --max-time 1 -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v1/sessions" >/dev/null 2>&1; then
        up=true
        break
      fi
      sleep 0.05
    done
    $up && break
    ((attempts < 120)) || die "the listener did not stay up in $attempts starts; see $LOGS/listener.out"
    sleep 1
  done
  ready_ns=$(date +%s%N)
  note "the listener answered again as pid $new after $attempts start(s)"
  jq -n --argjson o "$pid" --argjson n "$new" --argjson k "$kill_ns" --argjson f "$first_ns" \
    --argjson r "$restart_ns" --argjson a "$ready_ns" --argjson s "$attempts" \
    '{old_pid: $o, new_pid: $n, kill_ns: $k, first_start_ns: $f, restart_ns: $r, answered_ns: $a, starts: $s}' \
    >"$out/restart.json"
}

# end_step: on a failure of this script, end the step that is still running, so that it frees the
# root's lock instead of measuring on alone. SIGTERM goes to `loadgen run`, not to run-step.sh, so
# the step renders its tables, stops Dispatch and releases the lock as it does for any failed run.
end_step() {
  local rc=$?
  if ((rc != 0)) && [ -n "${step:-}" ] && kill -0 "$step" 2>/dev/null; then
    note "ending the step: this script failed with $rc"
    pkill -TERM -f "loadgen run -name $name -out $out" || true
    wait "$step" || true
  fi
}

started=$SECONDS
rc=0
"$here/run-step.sh" "$name" --events "$events" --sessions "$sessions" --requests "$requests" \
  --duration "$duration" --out "$out" "${step_args[@]}" &
step=$!
trap end_step EXIT

# The window opens once the sessions have registered; meta.json carries its start from then on.
until jq -e '.window_start | type == "string" and (startswith("0001") | not)' "$out/meta.json" >/dev/null 2>&1; do
  kill -0 "$step" 2>/dev/null || die "the step ended before its window opened; see $out/run.log"
  sleep 1
done
window_start=$(date -d "$(jq -er .window_start "$out/meta.json")" +%s)
note "window starts $(date -u -d "@$window_start" +%FT%TZ); kill at $(date -u -d "@$((window_start + kill_at))" +%FT%TZ)"

if $no_resume; then
  note "LOADTEST_NO_RESUME=1: no warm envelope, so no session holds a resume point"
else
  sleep_until $((window_start + warm_at))
  backlog 1 "$warm_over" "$out/warm.json" 910000000
  note "every session has answered one envelope done"
fi

sleep_until $((window_start + kill_at))
kill_and_restart &
restart=$!
sleep 2
backlog "$backlog_per" "$backlog_over" "$out/backlog.json"
wait "$restart" || die "the listener was not restarted (above); the step is still running, see $out/run.log"
wait "$step" || rc=$?
if ((rc != 0)); then
  # The step fails by design: the sampler sees the listener exit. Any other failure is this run's.
  [ -f "$out/summary.json" ] || die "the step exited $rc before it wrote summary.json; see $out/run.log"
  jq -e '[(.stack_exits // [])[].name] | length > 0 and all(. == "listener")' "$out/summary.json" >/dev/null ||
    die "the step exited $rc for something other than the listener this script killed: stack_exits $(jq -c '.stack_exits // []' "$out/summary.json"); see $out/run.log"
fi
"$stack" loadgen restart-report -run "$out"
note "restart step $name done in $((SECONDS - started)) s: $out"
