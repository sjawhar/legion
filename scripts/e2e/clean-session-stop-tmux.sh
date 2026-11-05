#!/usr/bin/env bash
# The daemon's clean stop, measured end to end (dispatch://INFRA-392): a Legion role whose agent
# process is SIGKILLed under its worker shim is `gone` to the Envoy listener within ten seconds,
# because the daemon deletes that session's registration the moment its probe finds the process
# gone (supervise.Deps.SessionGone -> cleanSessionStop's DELETE /v1/sessions/<id>), rather than the
# record waiting out the gone horizon.
#
# The clause holds for both harnesses a role runs on, so the proof kills both: Oh My Pi under
# `legion worker-shim`, and Claude Code under `legion claude-bridge`, which the shim runs in Oh My
# Pi's place for a role `worker_backends` puts on claude.
#
# The run carries its own negative control. A session the daemon never launched — one this script
# starts beside the daemon, outside any shim — is SIGKILLed too, and must still be waiting out the
# gone horizon ten seconds later, reaching `gone` only past it. Without that control the ten
# seconds would measure nothing: a horizon short enough to run in a proof would pass on its own.
#
# It is not a CI job. It runs a real `legion start` daemon under `runtime: tmux`, real agent
# processes of both harnesses, and a model route, which a Legion pod has and a hosted runner does
# not. Its row in .github/e2e-scripts.json is therefore under `elsewhere`.
#
#   LEGION_E2E_MODEL_BASE_URL=… LEGION_E2E_MODEL_KEY_FILE=… LEGION_E2E_MODEL_ID=anthropic/… \
#     LEGION_E2E_CLAUDE_MODEL=… CLEAN_SESSION_STOP_PG_DSN=… \
#     bash scripts/e2e/clean-session-stop-tmux.sh
#
# Nothing that names a deployment is defaulted: the model endpoint, its key file, the model ids and
# the PostgreSQL are the operator's to name, and an unset one is refused by name before the run
# starts anything. This repository is public, so no endpoint, host, tailnet, account or private
# repository name is written here.
#
# ---- inputs ---------------------------------------------------------------------------------------
# - LEGION_E2E_MODEL_BASE_URL (required; an http:// or https:// URL with no credentials in it) is
#   the Anthropic-compatible endpoint this pod's own model route answers on. Both harnesses call
#   it, so no Anthropic key is needed.
# - LEGION_E2E_MODEL_KEY_FILE (required) names a file holding that endpoint's key. It is copied
#   once (0600) into the run's work directory and preflighted (lib/model-endpoint.sh) before
#   anything starts: the endpoint must accept it, and must refuse a call with no key. No pane
#   carries the key in its environment: the Oh My Pi profile and the Claude Code claim directory
#   each name a key command that prints the copy.
# - LEGION_E2E_MODEL_ID (required) is the `anthropic/<id>` the Oh My Pi panes run; it reaches the
#   endpoint through their Anthropic provider.
# - LEGION_E2E_CLAUDE_MODEL (required) is the model id the Claude Code pane runs.
# - CLEAN_SESSION_STOP_PG_DSN (required) is a loopback PostgreSQL the run creates the daemon's own
#   database in and drops on exit. This script starts no database.
# - LEGION_E2E_OMP_PATH (default the `omp` on PATH, else the worker image's /opt/omp/bin/omp) is
#   the Oh My Pi the daemon runs in every Oh My Pi pane and in its own boot gate.
# - LEGION_E2E_CLAUDE_CODE_PIN (default the worker image's /opt/legion/claude-code) is the
#   directory holding the package.json and package-lock.json that pin @anthropic-ai/claude-code.
#   The image carries the pin and no Claude Code, so the run installs that exact release under
#   $TMPDIR with `npm ci` and holds its --version to the pin.
# - CLEAN_SESSION_STOP_CHECKS, or --checks <names>, is the comma-separated list of named checks to
#   run; unset, every check runs. --list prints every name with what it needs. A selection is an
#   operator's explicit choice and the summary line says which selection ran.
# - CLEAN_SESSION_STOP_EVIDENCE_DIR (default a fresh directory under $TMPDIR, kept and printed)
#   holds the run's transcript, every service's log, the daemon's log and each check's answers.
#
# Everything the run installs — the Go binaries, Claude Code and the two plugin builds — goes under
# $TMPDIR, never /tmp and never the home directory: a pod's own disk is small, and filling it
# evicts the pod with every role in it.
#
# ---- the tmux this needs, and where a pod gets one ----------------------------------------------
# `legion start` refuses to boot without tmux on PATH, and a Legion pod's worker image carries no
# tmux, no root and no package manager, so the run builds one from source:
#   PATH="$(bash scripts/e2e/lib/build-tmux.sh):$PATH"
# That script is idempotent and pins every tarball by version and sha256. It builds, under
# ${TMUX_BUILD_PREFIX:-$TMPDIR/tmux-build}, GNU make 4.4.1 first through its own bootstrap, since
# the pod has gcc and no make, then ncurses 6.5, byacc 20240109 (tmux's configure requires a yacc
# and the pod has none), libevent 2.1.12-stable and tmux 3.5a; a cold build takes about nine
# minutes and a built one prints its bin directory at once. This run calls it itself, so a caller
# that has not built tmux needs no step of its own; a tmux already first on PATH is used as it
# stands. The panes read terminfo from that prefix's own share/terminfo, which ncurses installs
# in full, rather than from the image's /usr/share/terminfo.
#
# ---- the windows, and why they are these numbers ------------------------------------------------
# The listener judges every record with
#   ENVOY_SESSION_LIVE_WINDOW=30s     a pane that stops heartbeating reads unresponsive six
#                                     heartbeats after its last one
#   ENVOY_SESSION_GONE_HORIZON=90s    the control's wait, and the boundary the clean stop must beat
#                                     by an order of magnitude; ten minutes would measure it no
#                                     better and would hold the run for half an hour
#   ENVOY_SESSION_REAP_INTERVAL=10s   the reaper acts promptly once a record passes the horizon
# and every pane heartbeats at ENVOY_HEARTBEAT_MS=5000 (runtime.tmux.pane_env), so a live pane
# refreshes six times inside the live window; the plugin's own default is 90 s, which no 30 s
# window could hold. The ten seconds the clean stop is held to are this issue's acceptance, not a
# number this rig chose: the daemon's probe runs the moment the shim's stream closes, and the
# delete is one HTTP call after it.
#
# ---- the named checks ---------------------------------------------------------------------------
# Each prints `PASS <name>` or `FAIL <name>: <reason>`, every selected check runs whatever the one
# before it did, and the last line says how many ran and how many passed.
#   clean-stop-of-an-omp-role        an architect on Oh My Pi, killed under `legion worker-shim`
#   clean-stop-of-a-claude-role      an implementer `worker_backends` puts on claude, killed under
#                                    `legion claude-bridge`
#   unseen-kill-waits-out-the-horizon   the control: a session of this script's own, outside any
#                                    shim, killed the same way
#
# ---- what the run starts, and what stops it ------------------------------------------------------
# A NATS server (internal/testnats/cmd/natsd), one Envoy listener, one `legion start` daemon with a
# private tmux server of its own, the panes that daemon launches, and the control session. Every
# one of them is stopped on any exit, by the pid it was started as; the daemon's tmux server is
# killed, the run's database is dropped and its work directory removed. The run is re-runnable: it
# takes a work directory of its own each time and inherits no DISPATCH_*, ENVOY_*, LEGION_*,
# NATS_*, OMP*, PI_*, TMUX*, ANTHROPIC_* or CLAUDE_* variable of the caller's.
set -Eeuo pipefail

# This script's own inputs are read first: the sweep below drops every variable of those families
# from the caller's environment, so that nothing the run starts inherits a Legion pane's real
# daemon, listener, bus or Oh My Pi profile, and it would otherwise drop these with them.
pg_dsn=${CLEAN_SESSION_STOP_PG_DSN:-}
wanted=${CLEAN_SESSION_STOP_CHECKS:-}
evidence=${CLEAN_SESSION_STOP_EVIDENCE_DIR:-}
model_base_url=${LEGION_E2E_MODEL_BASE_URL:-}
model_key_file=${LEGION_E2E_MODEL_KEY_FILE:-}
model_id=${LEGION_E2E_MODEL_ID:-}
claude_model=${LEGION_E2E_CLAUDE_MODEL:-}
omp_bin=${LEGION_E2E_OMP_PATH:-}
claude_pin=${LEGION_E2E_CLAUDE_CODE_PIN:-/opt/legion/claude-code}
for variable in $(compgen -e); do
  case $variable in
    DISPATCH_* | ENVOY_* | LEGION_* | NATS_* | OMP* | PI_* | TMUX* | ANTHROPIC_* | CLAUDE_*) unset "$variable" ;;
  esac
done

root=$(cd "$(dirname "$0")/../.." && pwd)
check=setup
timeout_hook=
ok=
ran=0
passed=0
selection=all
reason=
answer=/dev/null
api_status=
state=
# measured is the latency the running check measured, in milliseconds: the time from the SIGKILL to
# the first read of `gone`. Each check prints its own, and the summary prints them all, because a
# latency is the kind of fact a reader quotes.
measured=
declare -A latencies=()

# ---- the check table ----------------------------------------------------------------------------
# Each row is `<name> <what it kills>`.
checks=(
  "clean-stop-of-an-omp-role an Oh My Pi pane the daemon launched"
  "clean-stop-of-a-claude-role a Claude Code pane the daemon launched"
  "unseen-kill-waits-out-the-horizon a session of this script's own, outside any shim"
)

usage() {
  cat <<'EOF'
clean-session-stop-tmux.sh — the daemon's clean stop, measured (dispatch://INFRA-392)

  bash scripts/e2e/clean-session-stop-tmux.sh [--checks <name,name,…>] [--list]

--checks, or CLEAN_SESSION_STOP_CHECKS, runs only the named checks; unset, every check runs.
--list prints each check's name and the process it kills.
EOF
}

list_checks() {
  local row
  for row in "${checks[@]}"; do printf '%-36s %s\n' "${row%% *}" "${row#* }"; done
}

while [ $# -gt 0 ]; do
  case $1 in
    --checks)
      [ $# -ge 2 ] || {
        echo "--checks needs a comma-separated list of check names" >&2
        exit 2
      }
      wanted=$2
      shift 2
      ;;
    --checks=*)
      wanted=${1#--checks=}
      shift
      ;;
    --list)
      list_checks
      exit 0
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

[ -n "$evidence" ] || evidence=$(mktemp -d "${TMPDIR:-/tmp}/clean-session-stop-evidence.XXXXXXXX")
mkdir -p "$evidence/logs" "$evidence/answers"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

work=$(mktemp -d "${TMPDIR:-/tmp}/clean-session-stop.$$.XXXXXXXX")

note() { echo "   $*"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
begin() { echo "== $1"; }
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/model-endpoint.sh
. "$root/scripts/e2e/lib/model-endpoint.sh"

# ---- the selection ------------------------------------------------------------------------------
selected=()
if [ -z "$wanted" ]; then
  for row in "${checks[@]}"; do selected+=("${row%% *}"); done
else
  selection=$wanted
  IFS=, read -r -a asked <<<"$wanted"
  for name in "${asked[@]}"; do
    [ -n "$name" ] || continue
    found=
    for row in "${checks[@]}"; do [ "${row%% *}" != "$name" ] || found=1; done
    [ -n "$found" ] || fail "'$name' is no check of this proof; --list prints every name"
    selected+=("$name")
  done
  [ ${#selected[@]} -gt 0 ] || fail "--checks named no check"
fi
# Every check needs the daemon, its listener and both harnesses' builds: the daemon resolves Claude
# Code at boot for a configuration whose worker_backends names it, so a selection that kills only
# an Oh My Pi pane still boots that daemon.

# ---- the run's state ----------------------------------------------------------------------------
natsd_pid=
listener_pid=
listener_keeper_pid=
daemon_pid=
daemon_keeper_pid=
control_pid=
control_fd=
database=
nats_port=
nats_url=
listener_port=
daemon_port=
claude_bin=
claude_version=
project=
ptoken=
state_dir=$work/state
omp_home=$work/omp-home
profile=clean-session-stop-$$
# The windows the listener judges every record against; the header says why each is this number.
live_window=30s
gone_horizon=90s
gone_horizon_seconds=90
reap_interval=10s
heartbeat_ms=5000
# The acceptance this proof measures: a role's record is gone this many seconds after the kill.
clean_stop_limit_seconds=10

# own_session PIDFILE COMMAND… runs COMMAND in a session of its own and writes its pid to PIDFILE.
# The control session needs one: it is killed by pid, and a process in the run's own process group
# would take this script's group signals with it. The pid comes back through a file because `setsid
# --fork` returns at once and `$!` is the pid of a process that has already gone.
own_session() {
  local pidfile=$1
  shift
  # shellcheck disable=SC2016 # $$ and $@ are the new session's own, expanded by the shell it runs
  setsid --fork bash -c 'printf "%s\n" "$$" >"$1"; shift; exec "$@"' own-session "$pidfile" "$@"
}

# agent_pid PIDFILE VAR waits for the pid the new session wrote and assigns it to VAR.
agent_pid() {
  local waited=0
  while [ ! -s "$1" ]; do
    waited=$((waited + 1))
    [ "$waited" -lt 60 ] || return 1
    sleep 0.5
  done
  printf -v "$2" '%s' "$(cat "$1")"
}

# stop_agent PID ends the control session, and only while its command line or its environment still
# names this run's work directory, so a pid the kernel gave to another process since is never
# signalled. It leads a session of its own (own_session), so what it started goes with it.
stop_agent() {
  [ -n "${1:-}" ] || return 0
  grep -qsF "$work" "/proc/$1/cmdline" || grep -qsF "$work" "/proc/$1/environ" || return 0
  signalable "$1" || return 0
  kill -TERM -- -"$1" 2>/dev/null || true
  stop_pid "$1"
  kill -KILL -- -"$1" 2>/dev/null || true
}

cleanup() {
  local status=$1 p name
  trap - EXIT INT TERM
  set +e
  stop_agent "$control_pid"
  stop_pid "$daemon_pid"
  [ -z "$ptoken" ] || TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1
  stop_pid "$listener_pid"
  stop_pid "$natsd_pid"
  stop_pid "$daemon_keeper_pid"
  stop_pid "$listener_keeper_pid"
  # Any pane the daemon's tmux server left: every one of them runs out of this run's work
  # directory, and nothing else of this user's does.
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null; done
  if [ -n "$database" ]; then
    psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "drop database if exists \"$database\"" >/dev/null 2>&1 ||
      echo "could not drop database $database; drop it by hand" >&2
  fi
  rm -rf "$work"
  for name in "${!latencies[@]}"; do
    echo "   measured SIGKILL-to-gone latency, $name: ${latencies[$name]}"
  done
  if [ -n "$ok" ] && [ "$ran" -eq "$passed" ] && [ "$status" -eq 0 ]; then
    echo "clean session stop e2e: $ran checks ran, $passed passed — PASS ($selection, ${SECONDS}s, evidence: $evidence)"
  else
    [ "$status" -ne 0 ] || status=1
    echo "clean session stop e2e: $ran checks ran, $passed passed — FAIL at $check ($selection, ${SECONDS}s, evidence: $evidence)" >&2
  fi
  exit "$status"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

# ---- running a check ----------------------------------------------------------------------------
# A check is a function that returns 0, or sets `reason` and returns 1. run_check reports it and
# goes on to the next, so one failure never hides the checks after it, and the summary counts both.
run_check() {
  local name=$1
  check=$name
  begin "$name"
  reason=
  measured=
  ran=$((ran + 1))
  if "check_${name//-/_}"; then
    passed=$((passed + 1))
    echo "PASS $name"
  else
    echo "FAIL $name: ${reason:-no reason recorded}" >&2
  fi
}

# refuse MESSAGE records why the running check failed and fails it, so every failure leaves one
# sentence a reader can act on rather than a bare non-zero.
refuse() {
  reason=$1
  return 1
}

# wait_until SECONDS DESCRIPTION COMMAND… polls COMMAND twice a second until it succeeds, and fails
# the check when it has not by SECONDS: lib/rig.sh's until_true for a check, which fails its own
# check instead of ending the run, so the checks after it still run.
wait_until() {
  local limit=$1 what=$2 waited=0
  shift 2
  while [ "$waited" -lt "$((limit * 2))" ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    waited=$((waited + 1))
    sleep 0.5
  done
  refuse "timed out after ${limit}s waiting for $what"
}

# ---- the listener's HTTP surface ------------------------------------------------------------------
# api sets api_status and writes the body to $answer rather than printing either: a caller that read
# the status out of a command substitution would lose the body with the subshell.
api() { # api METHOD PATH
  local method=$1 path=$2 slug
  slug=$(printf '%s' "$path" | tr -c '[:alnum:]' '_' | cut -c1-48)
  answer=$evidence/answers/$check$slug.$RANDOM.json
  api_status=$(curl -sS --max-time 20 -o "$answer" -w '%{http_code}' -X "$method" \
    -H "@$work/envoy-auth-header" "http://127.0.0.1:$listener_port$path" 2>>"$evidence/logs/curl.log") || api_status=000
  [ "$api_status" = 200 ]
}

# session_state SESSION sets `state` to what the listener says of a session: the row's own `state`
# for an answered read, `gone` for the 404 that names no live session. Any other answer leaves
# `state` empty and fails, so a caller never reads a 503 as a verdict.
session_state() {
  state=
  api GET "/v1/registry/$1?include=unresponsive" || true
  case $api_status in
    200) state=$(jq -r '.state' "$answer") ;;
    404) state=$(jq -r '.state // "gone"' "$answer") ;;
    *) return 1 ;;
  esac
}

# state_is SESSION WANT is session_state as a predicate, for wait_until.
state_is() { session_state "$1" && [ "$state" = "$2" ]; }

# last_seen SESSION prints the session's last registration in epoch milliseconds.
last_seen() {
  api GET "/v1/registry/$1?include=unresponsive" || return 1
  jq -er '.last_seen' "$answer"
}

serving() { curl -fsS --max-time 3 -H "@$work/envoy-auth-header" "http://127.0.0.1:$1/v1/sessions"; }

# ---- the kill, and the measurement ----------------------------------------------------------------
# process_args PID NAME reads PID's arguments into the array NAME from the NUL-delimited
# /proc/PID/cmdline, in this shell: no pipe stands between the read and the check, so nothing ends
# early on a pane's command line, which holds its whole system prompt.
process_args() {
  local -n _args=$2
  { mapfile -d '' -t _args <"/proc/$1/cmdline"; } 2>/dev/null && [ "${#_args[@]}" -gt 0 ]
}

# agent_descendant PID NAME prints the first process under PID (PID included) whose argv[0] is
# called NAME, breadth first.
agent_descendant() {
  local queue=("$1") pid child children
  local -a args
  while [ "${#queue[@]}" -gt 0 ]; do
    pid=${queue[0]}
    queue=("${queue[@]:1}")
    if process_args "$pid" args && [ "$(basename -- "${args[0]}")" = "$2" ]; then
      printf '%s\n' "$pid"
      return 0
    fi
    children=$(cat /proc/"$pid"/task/*/children 2>/dev/null) || children=
    for child in $children; do queue+=("$child"); done
  done
  return 1
}

# parent_pid PID prints its parent's, from the stat fields after the comm field's own `) `.
parent_pid() {
  local stat
  stat=$(cat "/proc/$1/stat") || return 1
  read -r -a stat <<<"${stat##*) }"
  printf '%s\n' "${stat[1]}"
}

# shimmed PID succeeds when `legion worker-shim` is an ancestor of PID: the agent this proof kills
# is the one the daemon supervises, not a stray process of the same name. It prints the ancestry it
# walked, so a failure says what the pane's processes were.
shimmed() {
  local pid=$1 depth=0
  local -a args
  while [ -n "$pid" ] && [ "$pid" != 0 ] && [ "$pid" != 1 ] && [ "$depth" -lt 12 ]; do
    if process_args "$pid" args; then
      printf '     %s %s\n' "$pid" "${args[0]} ${args[1]-}" >&2
      [ "${args[1]-}" != worker-shim ] || return 0
    fi
    pid=$(parent_pid "$pid") || return 1
    depth=$((depth + 1))
  done
  return 1
}

# gone_within SESSION KILLED_MS LIMIT polls the listener ten times a second until it reads the
# session gone, and holds the latency it measures to LIMIT seconds. It reports the number it
# measured either way: a clean stop that took too long is a different fact from one that never
# happened, and a reader needs to be told which.
gone_within() {
  local session=$1 killed=$2 limit=$3 now elapsed
  while :; do
    session_state "$session" || true
    now=$(date +%s%3N)
    elapsed=$((now - killed))
    if [ "$state" = gone ]; then
      measured="${elapsed}ms"
      latencies[$check]=$measured
      note "GET /v1/registry/$session?include=unresponsive answered $api_status, $(jq -c '{state, error} | with_entries(select(.value != null))' "$answer"), ${elapsed}ms after the SIGKILL (the limit is ${limit}s)"
      [ "$elapsed" -le "$((limit * 1000))" ] ||
        refuse "the session reached gone ${elapsed}ms after the SIGKILL, past the ${limit}s this issue's acceptance allows" || return 1
      return 0
    fi
    if [ "$elapsed" -gt 60000 ]; then
      refuse "the session read '${state:-no answer}' 60s after the SIGKILL and never reached gone; the daemon's clean stop did not land" || return 1
    fi
    sleep 0.1
  done
}

# ---- the daemon's own surface -----------------------------------------------------------------------
legion() {
  env -i PATH="$PATH" HOME="$omp_home" TMPDIR="$work/tmp" XDG_STATE_HOME="$work/xdg" \
    "$work/bin/legion" "$@" --config "$work/legion.yaml" --operator-token-file "$work/operator-token"
}

# claim_json TOKEN prints the daemon's record of one claim.
claim_json() {
  legion claims list --json 2>>"$evidence/logs/legion-cli.log" |
    jq -ce --arg t "$1" '.claims[] | select(.token == $t)'
}
claim_is() { claim_json "$1" | jq -e "$2" >/dev/null; }
claim_field() { claim_json "$1" | jq -r "$2"; }
# claim_pane_pid TOKEN prints the process its pane was launched as, out of the claim's locator.
claim_pane_pid() { claim_field "$1" '.locator.incarnation | split(":")[0]'; }

# daemon_said PATTERN counts the daemon's log lines matching PATTERN.
daemon_said() {
  local count
  count=$(grep -c -- "$1" "$evidence/logs/daemon.log" 2>/dev/null) || count=0
  printf '%s\n' "$count"
}
daemon_said_at_least() { [ "$(daemon_said "$1")" -ge "$2" ]; }

# clean_stop_recorded SESSION reports whether the daemon acted on SESSION's death, by either of the
# two lines its clean stop can end in, and leaves the one it found in stop_record.
#
# The first is the stop it made. The second is the stop the listener refused because the
# registration was written after it: a heartbeat that landed under a second before the probe judged
# the process gone, inside cleanSessionStopSkewAllowance, after which the session's own plugin
# deregisters it instead. Both are triggered by the one kill, so which of them wins is a race this
# change admits rather than a fact it promises, and a check that pins either is a coin flip. What
# the clause claims is that the session is gone well inside the horizon, and gone_within above is
# what measures that; this asks only that the daemon saw the death and acted on it, which is what
# keeps the measurement from passing on the reaper (dispatch://INFRA-392).
stop_record=
clean_stop_recorded() {
  local session=$1 line
  for line in "the Envoy session of a role whose process is gone was stopped" \
    "it was written after the stop"; do
    if grep -F -- "$line" "$evidence/logs/daemon.log" 2>/dev/null | grep -qF -- "$session"; then
      stop_record=$line
      return 0
    fi
  done
  return 1
}

# spawn_claim ISSUE ROLE launches one claim of the run's tree and prints its token.
spawn_claim() {
  local spawned
  spawned=$(legion claims spawn --json --tree "$tree" --issue "$1" --role "$2" \
    --prompt-file "$work/role-prompt.md" 2>>"$evidence/logs/legion-cli.log") ||
    return 1
  printf '%s\n' "$spawned" >"$evidence/answers/spawn-$1-$2.json"
  jq -er .token <<<"$spawned"
}

# ensure_tree_root spawns the tree's root architect once and prints its token. A worker claim is
# bound to its tree, and a daemon with no workflow holds a tree's lifecycle only from the moment
# its root claim is admitted ("bind work of tree …: tree lifecycle is absent"), so the claude check
# run on its own starts the root too; with the Oh My Pi check before it, the claim is already
# there and this adds nothing.
ensure_tree_root() {
  local token=legion-$ptoken-${tree,,}-architect
  if claim_json "$token" >/dev/null 2>&1; then
    printf '%s\n' "$token"
    return 0
  fi
  spawn_claim "$tree" architect
}

# kill_the_agent TOKEN NAME SIGKILLs the agent process of that claim's pane and prints the
# millisecond it was killed at. The pane's own process is left alone: the clause is about the agent
# dying under its shim, which is what a crashed Oh My Pi or Claude Code does.
kill_the_agent() {
  local token=$1 name=$2 pane agent
  pane=$(claim_pane_pid "$token") || return 1
  [ -n "$pane" ] && [ "$pane" != null ] || return 1
  agent=$(agent_descendant "$pane" "$name") || return 1
  shimmed "$agent" || return 1
  kill -KILL "$agent" || return 1
  date +%s%3N
}

# ---- prerequisites --------------------------------------------------------------------------------
begin prerequisites
note "selection: $selection (${#selected[@]} checks)"
# A pod carries no tmux, so the run builds one rather than refusing for it; a tmux already on
# PATH is used as it stands, and the build prints its bin directory at once once it is built.
if ! command -v tmux >/dev/null; then
  note "no tmux on PATH; building one (scripts/e2e/lib/build-tmux.sh), which takes about nine minutes the first time"
  tmux_bin=$(bash "$root/scripts/e2e/lib/build-tmux.sh") ||
    fail "tmux could not be built: scripts/e2e/lib/build-tmux.sh named its own failure above"
  PATH="$tmux_bin:$PATH"
  export PATH
  note "tmux $(tmux -V | awk '{print $2}') from $tmux_bin"
fi
for tool in bun curl go jq npm od psql python3 setsid tar tmux; do
  command -v "$tool" >/dev/null || fail "$tool is required and is not on PATH"
done
[ -n "$model_base_url" ] ||
  fail "LEGION_E2E_MODEL_BASE_URL is required: the Anthropic-compatible endpoint this pod's model route answers on, which both harnesses call"
if why=$(model_base_url_refusal "$model_base_url") && [ -n "$why" ]; then
  fail "LEGION_E2E_MODEL_BASE_URL $why: name the model endpoint's base URL"
fi
[ -n "$model_key_file" ] ||
  fail "LEGION_E2E_MODEL_KEY_FILE is required: a file holding the key the model endpoint accepts"
[ -r "$model_key_file" ] ||
  fail "LEGION_E2E_MODEL_KEY_FILE names $model_key_file, which cannot be read"
[ -n "$model_id" ] || fail "LEGION_E2E_MODEL_ID is required: the anthropic/<id> the Oh My Pi panes run"
[[ $model_id =~ ^anthropic/[^[:space:]]+$ ]] ||
  fail "LEGION_E2E_MODEL_ID is '$model_id'; it must be anthropic/<id>, the provider Oh My Pi reaches the endpoint through"
[ -n "$claude_model" ] ||
  fail "LEGION_E2E_CLAUDE_MODEL is required: the model id Claude Code runs against that endpoint"
[ -n "$pg_dsn" ] ||
  fail "CLEAN_SESSION_STOP_PG_DSN is required: a loopback PostgreSQL the run creates the daemon's own database in"
case $pg_dsn in
  postgres://* | postgresql://*) ;;
  *) fail "CLEAN_SESSION_STOP_PG_DSN must be a postgres:// or postgresql:// URL" ;;
esac
authority=${pg_dsn#*://}
authority=${authority%%[/?]*}
hostport=${authority##*@}
case $hostport in
  \[*)
    db_host=${hostport#\[}
    db_host=${db_host%%\]*}
    ;;
  *) db_host=${hostport%%:*} ;;
esac
case $db_host in
  127.0.0.1 | localhost | ::1) ;;
  *) fail "CLEAN_SESSION_STOP_PG_DSN must name 127.0.0.1, localhost or ::1, not '$db_host': the run creates and drops a database in it" ;;
esac
[ -n "$omp_bin" ] || omp_bin=$(command -v omp || true)
[ -n "$omp_bin" ] || { [ ! -x /opt/omp/bin/omp ] || omp_bin=/opt/omp/bin/omp; }
[ -n "$omp_bin" ] ||
  fail "LEGION_E2E_OMP_PATH is required: no omp is on PATH and the worker image's /opt/omp/bin/omp is absent"
[ -x "$omp_bin" ] || fail "LEGION_E2E_OMP_PATH names $omp_bin, which is not an executable file"
omp_bin=$(realpath -e -- "$omp_bin")
{ [ -r "$claude_pin/package.json" ] && [ -r "$claude_pin/package-lock.json" ]; } ||
  fail "LEGION_E2E_CLAUDE_CODE_PIN names $claude_pin, which holds no package.json and package-lock.json pinning @anthropic-ai/claude-code; the worker image carries that pin and no Claude Code"
claude_version=$(jq -er '.dependencies["@anthropic-ai/claude-code"]' "$claude_pin/package.json") ||
  fail "$claude_pin/package.json pins no @anthropic-ai/claude-code"

mkdir -p "$work/bin" "$work/tmp" "$work/tmux" "$work/xdg" "$omp_home/.omp" "$state_dir"
chmod 0700 "$work/tmux" "$state_dir"
(
  umask 077
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/envoy-token"
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/operator-token"
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header"
  tr -d '[:space:]' <"$model_key_file" >"$work/model-key"
)
[ -s "$work/model-key" ] || fail "LEGION_E2E_MODEL_KEY_FILE ($model_key_file) holds no key"
model_key_preflight "$model_base_url" "$work/model-key" "$work/model-header" \
  "LEGION_E2E_MODEL_KEY_FILE ($model_key_file)"
note "the model endpoint accepts the key; the Oh My Pi panes run $model_id, the Claude Code pane $claude_model"
project=CSS$(od -An -N4 -tu4 /dev/urandom | tr -d ' \n')
project=${project:0:10}
ptoken=${project,,}
tree=CSS-1
note "work $work; evidence $evidence; project $project"
echo "ok prerequisites"

# ---- builds ----------------------------------------------------------------------------------------
begin build
go -C "$root/packages/daemon" build -o "$work/bin/legion" ./cmd/legion || fail "./cmd/legion did not build"
go -C "$root/packages/envoy" build -o "$work/bin/envoy-listener" ./cmd/listener ||
  fail "./cmd/listener did not build"
go -C "$root/packages/envoy" build -o "$work/bin/natsd" ./internal/testnats/cmd/natsd ||
  fail "./internal/testnats/cmd/natsd did not build"
while IFS= read -r line; do note "$line"; done < <(
  bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/bin/legion" "$work/bin/envoy-listener"
) || fail "lib/built-from.sh could not say what the run built"
(cd "$root" && bun install --frozen-lockfile) >"$evidence/logs/bun-install.log" 2>&1 ||
  fail "bun install failed; see $evidence/logs/bun-install.log"

# The Oh My Pi profile every pane and the daemon's own boot gate run under: this checkout's two
# plugins, packed as the release packs them and installed as the worker image installs them, under
# a HOME of the run's own. The caller's native-module cache is linked in, so a fresh home downloads
# nothing and the caller's own ~/.omp is neither written nor removed.
natives=$HOME/.omp/natives
[ -z "${XDG_DATA_HOME:-}" ] || [ ! -d "$XDG_DATA_HOME/omp" ] || natives=$XDG_DATA_HOME/omp/natives
mkdir -p "$natives"
ln -sfT "$natives" "$omp_home/.omp/natives"

# profile_omp runs that Oh My Pi as the daemon runs it: the binary by its absolute path, under the
# run's home and profile, from a directory holding no configuration of anyone's.
profile_omp() {
  (cd "$work" && env -i PATH="$PATH" HOME="$omp_home" TMPDIR="$work/tmp" OMP_PROFILE="$profile" \
    "$omp_bin" "$@")
}

install_plugin() { # install_plugin <pi-envoy|pi-legion>
  local package=$1 tarball dest=$work/$1 name version
  tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" "$package" "$work/pack-$package" \
    2>"$evidence/logs/pack-$package.log") ||
    fail "$package did not pack; see $evidence/logs/pack-$package.log"
  mkdir -p "$dest"
  tar xzf "$tarball" -C "$dest" --strip-components=1
  name=$(jq -r .name "$dest/package.json")
  version=$(jq -r .version "$dest/package.json")
  profile_omp plugin install "$dest" >>"$evidence/logs/omp-plugin-install.log" 2>&1 ||
    fail "omp plugin install of $package failed; see $evidence/logs/omp-plugin-install.log"
  profile_omp plugin list --json |
    jq -e --arg n "$name" --arg v "$version" \
      '[.npm[]? | select(.name == $n and .version == $v and .enabled == true)] | length == 1' >/dev/null ||
    fail "$name@$version is not installed and enabled in the run's OMP profile $profile"
  note "$name@$version from this checkout, in OMP profile $profile"
}
install_plugin pi-envoy
install_plugin pi-legion

# The panes' model route: the endpoint the key file's key is for, keyed by a command that reads the
# run's 0600 copy, so no key is in a pane's environment, its argv or its transcript.
mkdir -p "$omp_home/.omp/profiles/$profile/agent"
{
  printf 'providers:\n  anthropic:\n'
  printf '    baseUrl: %s\n' "$model_base_url"
  printf '    auth: apiKey\n    api: anthropic-messages\n'
  printf "    apiKey: '!cat %s'\n" "$work/model-key"
  printf '    models:\n'
  printf '      - {id: %s, name: %s, reasoning: true, input: [text], contextWindow: 200000, maxTokens: 8192}\n' \
    "${model_id#*/}" "${model_id#*/}"
} >"$omp_home/.omp/profiles/$profile/agent/models.yml"
{
  echo 'defaultThinkingLevel: low'
  echo 'modelRoles:'
  for role in default smol slow vision plan commit tiny task advisor review oracle deep; do
    echo "  $role: anthropic/${model_id#*/}"
  done
} >"$omp_home/.omp/profiles/$profile/agent/config.yml"

# Claude Code itself: the pinned release, installed from the image's own pin with `npm ci` into a
# directory of the run's own under $TMPDIR, its cache under $TMPDIR too — the install a pod's
# launch makes, held to the same version check.
mkdir -p "$work/claude-code" "$work/npm-cache"
cp "$claude_pin/package.json" "$claude_pin/package-lock.json" "$work/claude-code/" ||
  fail "could not copy the Claude Code pin out of $claude_pin"
(cd "$work/claude-code" && npm ci --ignore-scripts --omit=dev --no-audit --no-fund --cache "$work/npm-cache") \
  >"$evidence/logs/npm-claude-code.log" 2>&1 ||
  fail "installing the Claude Code $claude_version that $claude_pin pins failed; see $evidence/logs/npm-claude-code.log"
claude_bin=$work/claude-code/node_modules/@anthropic-ai/claude-code-linux-x64/claude
[ -x "$claude_bin" ] || fail "the install left no executable at $claude_bin"
installed=$("$claude_bin" --version) || fail "the installed Claude Code does not run"
[ "${installed%% *}" = "$claude_version" ] ||
  fail "the installed Claude Code prints --version '$installed'; $claude_pin pins $claude_version"
# The claude-envoy plugin a Claude Code pane loads, built from this checkout outside it: a pull
# request's tree carries no fresh dist/ or legion/.
bun "$root/packages/claude-envoy/scripts/build.ts" --out "$work/claude-envoy" \
  >"$evidence/logs/build-claude-envoy.log" 2>&1 ||
  fail "packages/claude-envoy did not build; see $evidence/logs/build-claude-envoy.log"
note "Claude Code $installed from $claude_pin, with this checkout's claude-envoy as its pane plugin"
echo "ok build"

# ---- services ---------------------------------------------------------------------------------------
begin services
start_process natsd "$work/bin/natsd" -p -1 -store_dir "$work/nats-store"
await_start natsd "$natsd_pid" 60 "natsd to be ready" grep -q 'Server is ready' "$evidence/logs/natsd.log"
nats_port=$(report_port "$evidence/logs/natsd.log" 'Listening for client connections on [^ ]*:') ||
  fail "natsd logged no client port; see $evidence/logs/natsd.log"
nats_url=nats://127.0.0.1:$nats_port
note "NATS on $nats_url, its JetStream store in $work/nats-store"

keep_port listener_keeper listener_port
start_kept listener listener_keeper ENVOY_LISTEN_FD -- env -i PATH="$PATH" HOME=/nonexistent \
  ENVOY_LISTEN_FD=3 ENVOY_API_TOKEN="$(cat "$work/envoy-token")" \
  ENVOY_MACHINE_ID="clean-session-stop-$$" ENVOY_LISTEN_HOST=127.0.0.1 NATS_URLS="$nats_url" \
  ENVOY_SESSION_LIVE_WINDOW="$live_window" ENVOY_SESSION_GONE_HORIZON="$gone_horizon" \
  ENVOY_SESSION_REAP_INTERVAL="$reap_interval" "$work/bin/envoy-listener"
await_start listener "$listener_pid" 60 "the Envoy listener to serve" serving "$listener_port"
note "the listener on 127.0.0.1:$listener_port judges records with a ${live_window} live window and a ${gone_horizon} gone horizon"

database=clean_session_stop_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "create database \"$database\"" >/dev/null ||
  fail "could not create database $database in CLEAN_SESSION_STOP_PG_DSN"
database_url=${pg_dsn%%\?*}
database_url=${database_url%/*}/$database
case $pg_dsn in *\?*) database_url="$database_url?${pg_dsn#*\?}" ;; esac
note "the daemon's store is the run's own database $database"
echo "ok services"

# ---- the daemon ---------------------------------------------------------------------------------------
begin daemon
keep_port daemon_keeper daemon_port
# omp_invocation is left out of the file: it takes the `mise x <tool> -- omp` form alone, and a pod
# has no mise, so the Oh My Pi under proof is named by LEGION_OMP_PATH in the daemon's own
# environment (omplaunch.ResolveInvocation), which the daemon resolves to its real path for every
# pane and for its own boot gate.
cat >"$work/legion.yaml" <<EOF
project: $project
port: $daemon_port
postgres_dsn: $database_url
state_dir: $state_dir
operator_token_file: $work/operator-token
envoy_url: http://127.0.0.1:$listener_port
envoy_token_file: $work/envoy-token
nats_urls:
  - $nats_url
probe_interval_seconds: 5
worker_backends:
  implementer: claude
runtime:
  tmux:
    pane_env:
      ANTHROPIC_BASE_URL: "$model_base_url"
      ENVOY_HEARTBEAT_MS: "$heartbeat_ms"
EOF
cat >"$work/role-prompt.md" <<'EOF'
You are a role in a Legion proof of the daemon's clean session stop. You have no work to do. Reply
to every message with the single word `ready`, run no tools, and wait.
EOF
# The Claude Code pane's own model route, written before the claim exists: the daemon makes the
# claim's Claude Code directory only if it is missing, and Claude Code reads settings.json there.
# Its apiKeyHelper prints the run's copy of the key, so the key reaches no argv and no environment.
claude_claim_dir=$state_dir/claude/legion-$ptoken-css-2-implementer
(
  umask 077
  mkdir -p "$claude_claim_dir"
  printf '#!/bin/sh\nexec cat %s\n' "$work/model-key" >"$work/claude-key-command"
  jq -n --arg helper "$work/claude-key-command" --arg model "$claude_model" \
    '{apiKeyHelper: $helper, model: $model}' >"$claude_claim_dir/settings.json"
)
chmod 0700 "$work/claude-key-command"

start_kept daemon daemon_keeper LEGION_LISTEN_FD -- env -i PATH="$PATH" \
  LEGION_LISTEN_FD=3 HOME="$omp_home" TMPDIR="$work/tmp" OMP_PROFILE="$profile" \
  TMUX_TMPDIR="$work/tmux" XDG_STATE_HOME="$work/xdg" LEGION_OMP_PATH="$omp_bin" \
  LEGION_CLAUDE_PATH="$claude_bin" LEGION_BUN_PATH="$(command -v bun)" \
  LEGION_CLAUDE_PLUGIN_DIR="$work/claude-envoy" \
  "$work/bin/legion" start --config "$work/legion.yaml"
await_start daemon "$daemon_pid" 300 "the daemon to answer /healthz" \
  curl -fsS --max-time 5 "http://127.0.0.1:$daemon_port/healthz"
note "the daemon serves on 127.0.0.1:$daemon_port, its private tmux server is legion-$ptoken under $work/tmux"
note "$(jq -R -c 'fromjson? | select(.msg == "legion daemon resolved Claude Code for the roles worker_backends puts on claude") | {claude, plugin}' \
  "$evidence/logs/daemon.log" | sed -n 1p)"
echo "ok daemon"

# ---- 1. an Oh My Pi role, killed under its worker shim ---------------------------------------------------
# The clause itself, on the harness every role runs by default. The architect's pane is `legion
# worker-shim … -- omp --mode rpc …`; the kill takes the omp process alone, which is what a crashed
# Oh My Pi leaves the shim with.
check_clean_stop_of_an_omp_role() {
  local token session killed
  token=$(ensure_tree_root) ||
    refuse "the architect claim could not be spawned: $(tail -c 300 "$evidence/logs/legion-cli.log")" || return 1
  wait_until 420 "the architect claim $token to be ready" claim_is "$token" '.state == "ready"' || return 1
  session=$(claim_field "$token" .session)
  [ -n "$session" ] && [ "$session" != null ] || refuse "the ready claim $token records no session" || return 1
  wait_until 120 "the architect's session to read live" state_is "$session" live || return 1
  note "claim $token is ready on session $session, pane $(claim_field "$token" .locator.tmux.pane)"
  killed=$(kill_the_agent "$token" omp) ||
    refuse "the architect pane runs no omp under a worker shim (its ancestry is above)" || return 1
  note "SIGKILLed the pane's omp process at $killed"
  gone_within "$session" "$killed" "$clean_stop_limit_seconds" || return 1
  # The daemon acted on this death, so the ten seconds above are its stop and not the horizon: the
  # listener's reaper could not have run inside ten seconds of a record whose horizon is ninety.
  wait_until 30 "the daemon to record its clean stop of $session" \
    clean_stop_recorded "$session" || return 1
  note "the daemon recorded the clean stop of $session: $stop_record"
}

# ---- 2. a Claude Code role, killed under its bridge ------------------------------------------------------
# The same clause for a role `worker_backends` puts on claude: the pane is `legion worker-shim …
# -- legion claude-bridge -- claude -p …`, and the kill takes the claude process. The daemon's
# supervision is the same for either harness, and the point of killing both is that the session a
# clean stop names is the one that harness registered.
check_clean_stop_of_a_claude_role() {
  local token session killed root
  root=$(ensure_tree_root) ||
    refuse "the tree's root architect could not be spawned: $(tail -c 300 "$evidence/logs/legion-cli.log")" || return 1
  note "the tree's root claim is $root"
  token=$(spawn_claim CSS-2 implementer) ||
    refuse "the implementer claim could not be spawned: $(tail -c 300 "$evidence/logs/legion-cli.log")" || return 1
  wait_until 600 "the implementer claim $token to be ready" claim_is "$token" '.state == "ready"' || return 1
  session=$(claim_field "$token" .session)
  [ -n "$session" ] && [ "$session" != null ] || refuse "the ready claim $token records no session" || return 1
  [ "$(claim_field "$token" .sessionFile)" != null ] ||
    refuse "the Claude Code claim $token registered no transcript" || return 1
  wait_until 120 "the implementer's session to read live" state_is "$session" live || return 1
  note "claim $token is ready on Claude Code session $session, transcript $(claim_field "$token" .sessionFile)"
  killed=$(kill_the_agent "$token" claude) ||
    refuse "the implementer pane runs no claude under a worker shim (its ancestry is above)" || return 1
  note "SIGKILLed the pane's claude process at $killed"
  gone_within "$session" "$killed" "$clean_stop_limit_seconds" || return 1
  wait_until 30 "the daemon to record its clean stop of $session" \
    clean_stop_recorded "$session" || return 1
  note "the daemon recorded the clean stop of $session: $stop_record"
}

# ---- 3. the control: a kill the daemon never sees --------------------------------------------------------
# One Oh My Pi of this script's own, registered against the same listener and supervised by nobody.
# Killed the same way, it must still be waiting ten seconds later, and reach gone only past the
# horizon. That is what makes the two checks above a measurement of the daemon's clean stop rather
# than of a horizon short enough to fit in a proof.
check_unseen_kill_waits_out_the_horizon() {
  local session killed_ms killed_at reading registered horizon_elapsed gone_at
  mkdir -p "$work/control-home/.omp" "$work/control-agent"
  ln -sfT "$natives" "$work/control-home/.omp/natives"
  cp "$omp_home/.omp/profiles/$profile/agent/models.yml" "$omp_home/.omp/profiles/$profile/agent/config.yml" \
    "$work/control-agent/"
  rm -f "$work/control.in" "$work/control.pid"
  mkfifo "$work/control.in"
  : >"$work/control.out"
  own_session "$work/control.pid" \
    env -i PATH="$PATH" HOME="$work/control-home" TMPDIR="$work/tmp" \
    PI_CODING_AGENT_DIR="$work/control-agent" \
    ENVOY_URL="http://127.0.0.1:$listener_port" ENVOY_TOKEN_FILE="$work/envoy-token" \
    ENVOY_NATS_URL="$nats_url" ENVOY_HEARTBEAT_MS="$heartbeat_ms" \
    ENVOY_MACHINE_ID="clean-session-stop-control-$$" \
    "$omp_bin" --extension "$work/pi-envoy" --mode rpc \
    <"$work/control.in" >"$work/control.out" 2>"$evidence/logs/control.stderr.log" &
  exec {control_fd}<>"$work/control.in"
  agent_pid "$work/control.pid" control_pid ||
    refuse "the control session wrote no pid" || return 1
  wait_until 300 "the control session to print ready" grep -q '"type":"ready"' "$work/control.out" || return 1
  printf '%s\n' '{"type":"get_state","id":"control"}' >&"$control_fd"
  wait_until 60 "the control session to answer get_state" \
    grep -q '"id":"control"' "$work/control.out" || return 1
  session=$(jq -r -R 'fromjson? | select(.id == "control") | .data.sessionId' "$work/control.out" | sed -n 1p)
  [ -n "$session" ] || refuse "the control session named no id" || return 1
  wait_until 180 "the control session to register live" state_is "$session" live || return 1
  registered=$(last_seen "$session") ||
    refuse "no last_seen for the control session while it is live" || return 1
  exec {control_fd}>&-
  control_fd=
  kill -KILL "$control_pid" || refuse "could not kill the control session" || return 1
  killed_ms=$(date +%s%3N)
  killed_at=$((killed_ms / 1000))
  note "SIGKILLed the control session (pid $control_pid, session $session), whose last registration was $registered"
  sleep "$clean_stop_limit_seconds"
  session_state "$session" || refuse "the listener would not answer for the control session" || return 1
  reading=$state
  [ "$reading" != gone ] ||
    refuse "the control session was gone ${clean_stop_limit_seconds}s after a kill nothing reported, so the ten seconds above measure the horizon rather than the daemon's clean stop" || return 1
  note "${clean_stop_limit_seconds}s after the kill the control session reads '$reading', not gone: nothing told the listener"
  wait_until 300 "the control session to reach gone past its ${gone_horizon} horizon" \
    state_is "$session" gone || return 1
  gone_at=$(date +%s)
  horizon_elapsed=$((gone_at - registered / 1000))
  measured="$((gone_at - killed_at))s"
  latencies[$check]="$measured (the horizon, not a clean stop)"
  note "the control session reached gone ${measured} after the kill, and ${horizon_elapsed}s after its last registration (gone horizon $gone_horizon, a reaper pass every $reap_interval)"
  [ "$horizon_elapsed" -ge "$gone_horizon_seconds" ] ||
    refuse "the control session read gone ${horizon_elapsed}s after its last registration, inside its own $gone_horizon horizon" || return 1
  control_pid=
}

# ---- the run -----------------------------------------------------------------------------------------
for name in "${selected[@]}"; do
  run_check "$name"
done
check=summary
ok=1
