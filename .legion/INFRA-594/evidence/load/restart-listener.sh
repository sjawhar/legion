#!/usr/bin/env bash
# restart-listener.sh ROOT DOWN_S OUT: kill the stack's listener with SIGKILL, keep it down DOWN_S
# seconds, then start it again with the same binary, arguments and environment (read from
# /proc/<pid>), its output appended to ROOT/logs/listener.out, and its pid in ROOT/run/listener.pid
# (so `stack.sh stop` stops the new one). Writes the kill and restart times (Unix ns) and the
# time the restarted listener answered /v1/sessions to OUT as JSON. The run lock is not taken:
# this runs beside a step that holds it, and touches only the listener.
set -euo pipefail
root=$1 down=$2 out=$3
pidf=$root/run/listener.pid
pid=$(cat "$pidf")
[ -r "/proc/$pid/environ" ] || { echo "no listener pid $pid" >&2; exit 1; }
mapfile -d '' envs <"/proc/$pid/environ"
mapfile -d '' cmd <"/proc/$pid/cmdline"
port=$(jq -r .ports.listener "$root/stack.json")
token=$(jq -r .listener_token "$root/stack.json")
kill_ns=$(date +%s%N)
kill -KILL "$pid"
while [ -e "/proc/$pid" ] && ! grep -q '^State:.*Z' "/proc/$pid/status" 2>/dev/null; do sleep 0.05; done
sleep "$down"
first_ns=$(date +%s%N)
# A start that exits (on gVisor the killed listener's port can still be held: "bind: address
# already in use") is started again after 1 s; restart_ns is the start that stayed up.
attempts=0
while :; do
  attempts=$((attempts + 1))
  restart_ns=$(date +%s%N)
  rm -f -- "$pidf"
  # shellcheck disable=SC2016
  setsid -f env -i "${envs[@]}" /bin/sh -c 'echo "$$" >"$0.tmp" && mv -f -- "$0.tmp" "$0" && exec "$@"' \
    "$pidf" "${cmd[@]}" >>"$root/logs/listener.out" 2>&1 </dev/null 9>&-
  until [ -s "$pidf" ]; do sleep 0.02; done
  new=$(cat "$pidf")
  up=false
  while [ -e "/proc/$new" ] && ! grep -q '^State:.*Z' "/proc/$new/status" 2>/dev/null; do
    if curl -fsS --max-time 1 -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v1/sessions" >/dev/null 2>&1; then
      up=true
      break
    fi
    sleep 0.05
  done
  $up && break
  ((attempts < 120)) || { echo "restart-listener.sh: the listener did not stay up in $attempts starts" >&2; exit 1; }
  sleep 1
done
ready_ns=$(date +%s%N)
jq -n --arg old "$pid" --arg new "$new" --argjson k "$kill_ns" --argjson f "$first_ns" --argjson r "$restart_ns" --argjson a "$ready_ns" --argjson n "$attempts" \
  '{old_pid: ($old|tonumber), new_pid: ($new|tonumber), kill_ns: $k, first_start_ns: $f, restart_ns: $r, answered_ns: $a, starts: $n}' >"$out"
cat "$out"
