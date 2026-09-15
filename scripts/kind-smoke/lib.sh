#!/usr/bin/env bash
# Shared by up.sh, down.sh, and checkpoints.sh. Source it; then call smoke_init. Nothing runs at
# source time except function definitions, so `up.sh` can name the missing tools before anything
# else executes. Every resource the rig creates outside the cluster is named by the instance and
# recorded as one file under $state/records; teardown and the checkpoints read only those records.
# shellcheck disable=SC2034  # smoke_init's globals are read by the sourcing scripts
set -euo pipefail

fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
note() { printf '%s\n' "$*"; }

# The checkout root: two levels above this file. Pure bash, so it works before `require_tools`.
smoke_repo_root() {
  local here="${BASH_SOURCE[0]%/*}"
  [ "$here" = "${BASH_SOURCE[0]}" ] && here=.
  cd -- "$here/../.." && pwd
}

# SMOKE_INSTANCE, else LEGION_ISSUE, else USER; lowercased, letters and digits only, 1-9 characters
# so the Dispatch project key S<INSTANCE> fits Dispatch's ^[A-Z][A-Z0-9]{1,9}$.
smoke_instance() {
  local raw="${SMOKE_INSTANCE:-${LEGION_ISSUE:-${USER:-}}}"
  raw="$(printf '%s' "$raw" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9')"
  [[ "$raw" =~ ^[a-z0-9]{1,9}$ ]] || fail "SMOKE_INSTANCE must be 1-9 lowercase letters or digits (got '${raw:-<empty>}'): it names the kind cluster legion-smoke-<instance>, the containers, the tmux server, the state directory, and the Dispatch project S<INSTANCE> (at most 10 characters)"
  printf '%s' "$raw"
}

smoke_port() { # nats 0, listener 1, dispatch 2, postgres 3, daemon port-forward 4
  case "$1" in
    nats) echo $((port_base + 0)) ;;
    listener) echo $((port_base + 1)) ;;
    dispatch) echo $((port_base + 2)) ;;
    postgres) echo $((port_base + 3)) ;;
    daemon) echo $((port_base + 4)) ;;
    *) fail "unknown port name $1" ;;
  esac
}

smoke_init() {
  repo_root="$(smoke_repo_root)"
  instance="$(smoke_instance)"
  state="${SMOKE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/legion-smoke/$instance}"
  records="$state/records"
  port_base="${SMOKE_PORT_BASE:-31000}"
  if ! [[ "$port_base" =~ ^[0-9]+$ ]] || [ "$port_base" -lt 1024 ] || [ "$port_base" -gt 65530 ]; then
    fail "SMOKE_PORT_BASE must be an integer between 1024 and 65530 (got '$port_base'); the instance uses SMOKE_PORT_BASE+0 (NATS), +1 (Envoy listener), +2 (Dispatch), +3 (Postgres), +4 (daemon port-forward)"
  fi
  port_nats="$(smoke_port nats)"
  port_listener="$(smoke_port listener)"
  port_dispatch="$(smoke_port dispatch)"
  port_postgres="$(smoke_port postgres)"
  port_daemon="$(smoke_port daemon)"
  cluster="legion-smoke-$instance"
  tmux_server="legion-smoke-$instance"
  nats_container="legion-smoke-$instance-nats"
  postgres_container="legion-smoke-$instance-postgres"
  project_key="S$(printf '%s' "$instance" | tr '[:lower:]' '[:upper:]')"
  mkdir -p "$state" "$records" "$state/logs" "$state/bin" "$state/pids" "$state/secrets"
  chmod 0700 "$state/secrets"
  gateway="$(record_read gateway)"
}

# ---- records: one file per resource or mode under $state/records -------------------------------

record_write() { printf '%s\n' "$2" >"$records/$1"; }
record_append() { printf '%s\n' "$2" >>"$records/$1"; }
record_read() { if [ -f "$records/$1" ]; then cat -- "$records/$1"; fi; }
record_require() {
  [ -s "$records/$1" ] || fail "record $1 missing under $state: run scripts/kind-smoke/up.sh first"
  cat -- "$records/$1"
}

# ---- secrets: 0600 files under the 0700 $state/secrets, never printed, never on an argv ---------

generate_secret() { # generate_secret NAME — written once, kept across reruns
  local f="$state/secrets/$1"
  if [ ! -s "$f" ]; then (umask 077; openssl rand -hex 24 >"$f"); fi
  chmod 0600 "$f"
}
# curl reads a header from a file with -H @file, so a bearer never appears in curl's argv either.
auth_header_file() { # auth_header_file SECRET_NAME → path of a 0600 file holding "Authorization: Bearer <value>"
  local f="$state/secrets/$1-auth-header"
  if [ ! -s "$f" ] || [ "$state/secrets/$1" -nt "$f" ]; then
    (umask 077; printf 'Authorization: Bearer %s\n' "$(<"$state/secrets/$1")" >"$f")
  fi
  chmod 0600 "$f"
  printf '%s' "$f"
}

# ---- process ownership: pid + /proc start ticks, so a reused pid is never mistaken for ours ----

process_start_time() {
  local pid="$1"
  [[ "$pid" =~ ^[0-9]+$ && -r "/proc/${pid}/stat" ]] || return 1
  awk '{print $22}' "/proc/${pid}/stat"
}
process_group_id() {
  local pid="$1"
  [[ "$pid" =~ ^[0-9]+$ && -r "/proc/${pid}/stat" ]] || return 1
  awk '{print $5}' "/proc/${pid}/stat"
}
pid_is_live() { # pid_is_live NAME — $state/pids/NAME.pid + NAME.start; the recorded process, not a reused pid
  local pid_file="$state/pids/$1.pid" start_file="$state/pids/$1.start" pid expected actual
  [[ -r "$pid_file" && -r "$start_file" ]] || return 1
  pid="$(<"$pid_file")"
  expected="$(<"$start_file")"
  kill -0 "$pid" 2>/dev/null || return 1
  actual="$(process_start_time "$pid")" || return 1
  [[ "$actual" == "$expected" ]]
}
start_process() { # start_process NAME CMD… — reuse when live; else launch, record pid + start ticks
  local name="$1" pid start_time
  shift
  if pid_is_live "$name"; then
    printf 'REUSED %s (pid %s)\n' "$name" "$(<"$state/pids/$name.pid")"
    return
  fi
  rm -f "$state/pids/$name.pid" "$state/pids/$name.start"
  "$@" >>"$state/logs/$name.log" 2>&1 &
  pid="$!"
  start_time="$(process_start_time "$pid")" || fail "$name exited before its ownership record was written; see $state/logs/$name.log"
  printf '%s\n' "$pid" >"$state/pids/$name.pid"
  printf '%s\n' "$start_time" >"$state/pids/$name.start"
  printf 'STARTED %s (pid %s)\n' "$name" "$pid"
}
start_process_group() { # setsid variant: the record is the process group id (a supervising loop and its children)
  local name="$1" pid pgid start_time
  shift
  if pid_is_live "$name"; then
    printf 'REUSED %s (pgid %s)\n' "$name" "$(<"$state/pids/$name.pid")"
    return
  fi
  rm -f "$state/pids/$name.pid" "$state/pids/$name.start"
  setsid "$@" >>"$state/logs/$name.log" 2>&1 &
  pid="$!"
  start_time="$(process_start_time "$pid")" || fail "$name exited before its ownership record was written; see $state/logs/$name.log"
  pgid="$(process_group_id "$pid")" || fail "$name exited before its process group was recorded"
  [[ "$pgid" == "$pid" ]] || fail "$name did not start in its own process group"
  printf '%s\n' "$pgid" >"$state/pids/$name.pid"
  printf '%s\n' "$start_time" >"$state/pids/$name.start"
  printf 'STARTED %s (pgid %s)\n' "$name" "$pgid"
}
terminate_pid_file() { # signal only the recorded process (start ticks verified); always drop the record
  local name="$1" pid_file="$state/pids/$1.pid" start_file="$state/pids/$1.start" pid expected actual attempt
  [[ -r "$pid_file" && -r "$start_file" ]] || { rm -f "$pid_file" "$start_file"; return 0; }
  pid="$(<"$pid_file")"
  expected="$(<"$start_file")"
  actual="$(process_start_time "$pid" 2>/dev/null || true)"
  rm -f "$pid_file" "$start_file"
  if [[ -z "$actual" || "$actual" != "$expected" ]]; then
    printf 'GONE %s (pid %s is not the recorded process; record removed)\n' "$name" "$pid"
    return 0
  fi
  kill "$pid" 2>/dev/null || true
  for ((attempt = 1; attempt <= 10; attempt += 1)); do
    kill -0 "$pid" 2>/dev/null || { printf 'STOPPED %s (pid %s)\n' "$name" "$pid"; return 0; }
    sleep 1
  done
  kill -KILL "$pid" 2>/dev/null || true
  printf 'STOPPED %s (pid %s, killed)\n' "$name" "$pid"
}
terminate_process_group_file() {
  local name="$1" pid_file="$state/pids/$1.pid" start_file="$state/pids/$1.start" pgid expected actual attempt
  [[ -r "$pid_file" && -r "$start_file" ]] || { rm -f "$pid_file" "$start_file"; return 0; }
  pgid="$(<"$pid_file")"
  expected="$(<"$start_file")"
  actual="$(process_start_time "$pgid" 2>/dev/null || true)"
  rm -f "$pid_file" "$start_file"
  if [[ -z "$actual" || "$actual" != "$expected" ]]; then
    printf 'GONE %s (pgid %s is not the recorded process group; record removed)\n' "$name" "$pgid"
    return 0
  fi
  kill -- "-$pgid" 2>/dev/null || true
  for ((attempt = 1; attempt <= 10; attempt += 1)); do
    kill -0 -- "-$pgid" 2>/dev/null || { printf 'STOPPED %s (pgid %s)\n' "$name" "$pgid"; return 0; }
    sleep 1
  done
  kill -KILL -- "-$pgid" 2>/dev/null || true
  printf 'STOPPED %s (pgid %s, killed)\n' "$name" "$pgid"
}

# assert_port_free NAME PORT [OWNER_FN] — OWNER_FN returning 0 means the listener is this instance's
# own recorded process or container, which is reused, never killed.
assert_port_free() {
  local name="$1" port="$2" owner="${3:-}"
  if ! [[ "$port" =~ ^[0-9]+$ ]] || ((port < 1 || port > 65535)); then fail "$name port must be between 1 and 65535 (got '$port')"; fi
  if [ -n "$owner" ] && "$owner"; then return 0; fi
  [[ -z "$(ss -H -ltn "sport = :$port")" ]] ||
    fail "port $port ($name) is already in use and is not this instance's; choose another SMOKE_PORT_BASE (current $port_base)"
}

# poll BUDGET_S DESCRIPTION CMD… — CMD every ${SMOKE_POLL_INTERVAL:-5}s until it returns 0 or the budget
# is spent (1). The harness sets SMOKE_POLL_INTERVAL=0; the budget still counts one second per try.
# A caller that reports the timeout itself (checkpoints.sh, one line per verdict) sets
# poll_timeout_line=0.
poll_timeout_line=1
poll() {
  local budget="$1" what="$2"
  shift 2
  local interval="${SMOKE_POLL_INTERVAL:-5}" waited=0 step
  step=$((interval > 0 ? interval : 1))
  until "$@"; do
    waited=$((waited + step))
    if [ "$waited" -gt "$budget" ]; then
      [ "$poll_timeout_line" = 0 ] || printf 'timed out after %ss waiting for %s\n' "$budget" "$what" >&2
      return 1
    fi
    sleep "$interval"
  done
}

# ---- readers: kubectl with the instance kubeconfig, daemon state through the port-forward, Dispatch --

kc() { kubectl --kubeconfig "$state/kubeconfig" -n legion "$@"; }
daemon_state() {
  local _
  for _ in 1 2 3; do
    if curl -fsS --max-time 10 "http://127.0.0.1:${port_daemon}/legion/v1/state"; then return 0; fi
    sleep 2
  done
  return 1
}
dispatch_url() { printf 'http://%s:%s' "$gateway" "$port_dispatch"; }
dispatch_get() { curl -fsS --max-time 20 -H "@$(auth_header_file dispatch-token)" "$(dispatch_url)/api/v1/$1"; }
dispatch_human() { # dispatch_human METHOD PATH [JSON] — the one trusted human login, by header
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -fsS --max-time 20 -X "$method" -H "X-Dispatch-User: $(record_require dispatch-login)" -H 'content-type: application/json' --data "$body" "$(dispatch_url)/api/v1/$path"
  else
    curl -fsS --max-time 20 -X "$method" -H "X-Dispatch-User: $(record_require dispatch-login)" "$(dispatch_url)/api/v1/$path"
  fi
}
# role_token PROJECT KEY ROLE — legion-<project>-<prefix lowercased>-<number>-<role>, exactly as the
# daemon encodes it (packages/contracts/src/legion-roles.ts).
role_token() {
  local prefix="${2%%-*}" number="${2##*-}"
  printf 'legion-%s-%s-%s-%s' "$1" "$(printf '%s' "$prefix" | tr '[:upper:]' '[:lower:]')" "$number" "$3"
}
