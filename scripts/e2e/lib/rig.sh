# shellcheck shell=bash disable=SC2154 # the variables below are the caller's (see the header)
# The host-side rig helpers a stage proof shares: bounded waits, ports, the processes the run starts,
# and their teardown. Everything they touch is the run's own — its work directory, its processes,
# and the ports it picked.
#
# Sourced, never run. The caller sets
#   root           the checkout
#   work           the run's scratch directory; every process whose command line or working
#                  directory names it belongs to the run
#   evidence       the evidence directory; each service started by start_process logs to
#                  $evidence/logs/<name>.log
#   timeout_hook   empty, or a function a timed-out until_true runs before it fails
# and defines note and fail (which exits). A production guard that finds a violation writes it to
# $evidence/pane-endpoint-violation.txt, and the next bounded wait aborts naming it.

# until_true SECONDS DESCRIPTION COMMAND... — all synchronization has a bounded named wait.
until_true() {
  local limit=$1 what=$2 i
  shift 2
  for ((i = 0; i < limit * 2; i++)); do
    [ ! -s "$evidence/pane-endpoint-violation.txt" ] || fail "ABORT: $(cat "$evidence/pane-endpoint-violation.txt")"
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  if [ -n "$timeout_hook" ]; then "$timeout_hook" || true; fi
  fail "timed out after ${limit}s waiting for $what"
}

# pick_port VAR assigns VAR a port from lib/free-port.sh that is distinct from every earlier pick of
# this run. It assigns in place, never through a command substitution, so the run-wide set of picks
# survives.
picked_ports=
pick_port() {
  local port
  # The picks are separate arguments: word splitting is the point.
  # shellcheck disable=SC2086
  port=$(bash "$root/scripts/e2e/lib/free-port.sh" $picked_ports) || fail "no free port for $1 (the reason is above)"
  picked_ports="$picked_ports $port"
  printf -v "$1" '%s' "$port"
}

log_size() { stat -c %s "$evidence/logs/$1.log" 2>/dev/null || printf '0\n'; }

# await_start NAME PID OFFSET SECONDS DESCRIPTION COMMAND... waits, bounded, for COMMAND to succeed
# while the service NAME started as PID lives. It returns 2 when the service exited because another
# process bound its picked port first — the one race picking a port before the service binds it
# cannot close, so the caller picks again — and fails the check naming the log on any other exit.
# OFFSET is the size of the service's log before this start, so only this start's lines count.
await_start() {
  local name=$1 pid=$2 offset=$3 limit=$4 what=$5 i
  shift 5
  for ((i = 0; i < limit * 2; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    if ! kill -0 "$pid" 2>/dev/null; then
      if tail -c "+$((offset + 1))" "$evidence/logs/$name.log" | grep -qi 'address already in use'; then return 2; fi
      fail "$what: the process exited; see $evidence/logs/$name.log"
    fi
    sleep 0.5
  done
  fail "timed out after ${limit}s waiting for $what"
}

# start_process NAME COMMAND... starts COMMAND in the background, logging to the service's log, and
# sets <NAME>_pid to its pid.
start_process() {
  local name=$1
  shift
  "$@" >>"$evidence/logs/$name.log" 2>&1 &
  printf -v "${name}_pid" '%s' "$!"
}

# stop_pid is intentionally best effort: a failed cleanup must never obscure the check that failed.
stop_pid() {
  local pid=${1:-} i
  [ -n "$pid" ] || return 0
  kill -TERM "$pid" 2>/dev/null || return 0
  for i in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.2
  done
  kill -KILL "$pid" 2>/dev/null || true
  return 0
}

run_processes() {
  local p cwd
  for p in /proc/[0-9]*; do
    cwd=$(readlink "$p/cwd" 2>/dev/null) || continue
    case "$cwd/" in
      "$work"/*) printf '%s\n' "${p#/proc/}"; continue ;;
    esac
    grep -qsF "$work" "$p/cmdline" 2>/dev/null && printf '%s\n' "${p#/proc/}"
  done
}
