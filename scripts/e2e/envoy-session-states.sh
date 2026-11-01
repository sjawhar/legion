#!/usr/bin/env bash
# The pod proof of the three session states (dispatch://INFRA-392): a session is live,
# unresponsive or gone by its registration record's age alone, and an unresponsive session is not
# dead — it keeps its role claim and its issue claim, messages are stored for it, no replacement is
# started for it, and nothing but the gone horizon takes any of that away.
#
# It is not a CI job. It drives real agent sessions of all three plugin hosts — Oh My Pi on this
# checkout's @sjawhar/pi-envoy, Claude Code on this checkout's packages/claude-envoy, and OpenCode
# on this checkout's @sjawhar/opencode-legion-envoy — against an Envoy listener and a NATS server
# built from this checkout, so it needs a model route, which a Legion pod has and a hosted runner
# does not. Its row in .github/e2e-scripts.json is therefore under `elsewhere`.
#
#   LEGION_E2E_MODEL_BASE_URL=… LEGION_E2E_MODEL_KEY_FILE=… LEGION_E2E_MODEL_ID=anthropic/… \
#     LEGION_E2E_CLAUDE_MODEL=… ENVOY_SESSION_STATES_PG_DSN=… \
#     bash scripts/e2e/envoy-session-states.sh
#
# Nothing that names a deployment is defaulted: the model endpoint, its key file, the model ids and
# the PostgreSQL are the operator's to name, and an unset one is refused by name before the run
# starts anything. This repository is public, so no endpoint, host, tailnet, account or private
# repository name is written here.
#
# ---- inputs ---------------------------------------------------------------------------------------
# - LEGION_E2E_MODEL_BASE_URL (required for every check that needs a model turn; an http:// or
#   https:// URL with no credentials in it) is the Anthropic-compatible endpoint this pod's own
#   model route answers on. The three runtimes call it, so no Anthropic key is needed.
# - LEGION_E2E_MODEL_KEY_FILE (same) names a file holding that endpoint's key. It is copied once
#   (0600) into the run's work directory and preflighted (lib/model-endpoint.sh) before anything
#   starts: the endpoint must accept it, and must refuse a call with no key.
# - LEGION_E2E_MODEL_ID (same) is the `anthropic/<id>` Oh My Pi and OpenCode run; both reach the
#   endpoint through their Anthropic provider.
# - LEGION_E2E_CLAUDE_MODEL (same) is the model id Claude Code runs (ANTHROPIC_MODEL).
# - ENVOY_SESSION_STATES_PG_DSN (required for every check that reads an issue claim) is a loopback
#   PostgreSQL with pgvector the run creates its own database in and drops on exit. This script
#   starts no database; docs/solutions/testing/ holds a recipe that starts one in a pod with no
#   root ("The real Dispatch stack runs inside a Legion pod with no Postgres, no docker and no
#   root").
# - LEGION_E2E_OMP_PATH (default the `omp` on PATH, else the worker image's /opt/omp/bin/omp) is
#   the Oh My Pi the proof's sessions run.
# - LEGION_E2E_CLAUDE_CODE_PIN (default the worker image's /opt/legion/claude-code) is the
#   directory holding the package.json and package-lock.json that pin @anthropic-ai/claude-code.
#   The image carries the pin and no Claude Code, so the run installs that exact release under
#   $TMPDIR with `npm ci` and holds its --version to the pin, as a pod's own launch does
#   (claudeCodeBlock, packages/daemon/internal/runtime/sandbox/plugins.go).
# - LEGION_E2E_OPENCODE_VERSION (default the version packages/envoy-plugin/package.json pins for
#   @opencode-ai/plugin) is the opencode-ai release the run installs under $TMPDIR.
# - ENVOY_SESSION_STATES_CHECKS, or --checks <names>, is the comma-separated list of named checks
#   to run; unset, every check runs. --list prints every name with what it needs. A selection is an
#   operator's explicit choice and the summary line says which selection ran: no check ever skips
#   itself, what a selected check needs is required of the inputs, and a model route that cannot
#   reach a runtime fails the run by name before any check starts.
# - ENVOY_SESSION_STATES_EVIDENCE_DIR (default a fresh directory under $TMPDIR, kept and printed)
#   holds the run's transcript, every service's log, every agent's log and each check's answers.
#
# Everything the run installs — the Go binaries, Claude Code, OpenCode and the three plugin builds
# — goes under $TMPDIR, never /tmp and never the home directory: a pod's own disk is small, and
# filling it evicts the pod with every role in it.
#
# ---- the windows, and why they are these numbers ------------------------------------------------
# The two running listeners judge every record with
#   ENVOY_SESSION_LIVE_WINDOW=30s     a stall reads unresponsive six heartbeats after the last one
#   ENVOY_SESSION_GONE_HORIZON=10m    longer than check 2's bus outage and check 8's 7-minute
#                                     stall, so neither reads gone while it is only stalled
#   ENVOY_SESSION_REAP_INTERVAL=20s   a change to the bucket's maximum age reaches both inside the
#                                     two minutes check 8 allows, and both reapers act promptly
# and every plugin heartbeats at ENVOY_HEARTBEAT_MS=5000, so a live session refreshes six times
# inside the live window: the shipping margin — three refreshes inside five minutes
# (.github/scripts/check-heartbeat-margin.sh) — kept at this scale. Check 6 runs a listener of its
# own at a 90s gone horizon, because the one thing that check measures is the latency from a
# SIGKILL to `gone`, and ten minutes of waiting would measure the same boundary no better.
#
# The session bucket's maximum age is 2h for the whole run but check 8. At the shipping five
# minutes JetStream removes a stalled session's key at five minutes — before the gone horizon can
# be reached — so no stalled session could ever read `unresponsive` and nothing above could be
# proved at all. Raising it is the rollout this issue's sibling carries (dispatch://INFRA-887), and
# check 8 is its proof: the two running listeners leave ENVOY_SESSION_MAX_AGE unset, so neither
# writes the bucket's age and both adopt what the bucket carries (applyMaxAge, on every reaper
# pass), and a listener of the run's own writes it — 2h at setup, then in check 8 the shipping 5m,
# the raise back to 2h, and the rollback to 5m.
#
# ---- the named checks ---------------------------------------------------------------------------
# Each prints `PASS <name>` or `FAIL <name>: <reason>`, every selected check runs whatever the one
# before it did, and the last line says how many ran and how many passed. The eight requirements of
# this issue's proof map onto ten names, because requirement 3 holds three separate things whose
# needs differ and a reader has to be told which of them held:
#   1  stalled-session-reads-unresponsive     (3) -> 3a send-to-stalled-session-is-stored
#   2  bus-outage-keeps-claims                       3b stored-deliveries-arrive-once
#   4  role-claim-survives-lookups                   3c send-to-serving-port-is-refused
#   5  issue-claim-refuses-the-stalled-holder  6  gone-only-past-the-horizon
#   8  max-age-change-reaches-both-listeners   7  every-plugin-shows-the-state
#
# ---- what the run starts, and what stops it ------------------------------------------------------
# A NATS server (internal/testnats/cmd/natsd), two Envoy listeners on ports held from the kernel's
# choice until each serves on it (lib/held-port.py through lib/rig.sh), a Dispatch server, up to
# three agent processes and two short-lived listeners of the run's own. Every one of them is
# stopped on any exit, by the pid it was started as; the run's database is dropped and its work
# directory removed. The run is re-runnable: it takes a work directory of its own each time and
# inherits no DISPATCH_*, ENVOY_*, NATS_*, OMP*, PI_*, ANTHROPIC_*, CLAUDE_* or OPENCODE_* variable
# of the caller's.
set -Eeuo pipefail

# This script's own three ENVOY_SESSION_STATES_* inputs are read first: the sweep below drops every
# ENVOY_* variable of the caller's, so that nothing the run starts inherits a Legion pane's real
# listener, bus or Dispatch, and it would otherwise drop these with them.
pg_dsn=${ENVOY_SESSION_STATES_PG_DSN:-}
wanted=${ENVOY_SESSION_STATES_CHECKS:-}
evidence=${ENVOY_SESSION_STATES_EVIDENCE_DIR:-}
for variable in $(compgen -e); do
  case $variable in
    DISPATCH_* | ENVOY_* | NATS_* | OMP* | PI_* | ANTHROPIC_* | OPENCODE_* | CLAUDE_*) unset "$variable" ;;
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
# measured_kill_latency is check 6's one number: the seconds from the SIGKILL to the first read of
# `gone`. It is printed where it is measured and again in the summary, because a latency is the
# kind of fact a reader quotes.
measured_kill_latency=

# ---- the check table ----------------------------------------------------------------------------
# Each row is `<name>[ <need>…]`: what the check needs beyond the listener and NATS the run always
# builds. `omp`, `claude` and `opencode` are agent runtimes it starts; `model` is a check that has
# an agent take a model turn, and is the one need that makes the model route required; `dispatch`
# is a check that reads or writes an issue claim.
checks=(
  "stalled-session-reads-unresponsive omp"
  "bus-outage-keeps-claims dispatch"
  "send-to-stalled-session-is-stored"
  "stored-deliveries-arrive-once omp dispatch model"
  "send-to-serving-port-is-refused opencode model"
  "role-claim-survives-lookups"
  "issue-claim-refuses-the-stalled-holder dispatch"
  "gone-only-past-the-horizon omp claude opencode model"
  "every-plugin-shows-the-state omp claude opencode model"
  "max-age-change-reaches-both-listeners"
)

usage() {
  cat <<'EOF'
envoy-session-states.sh — the pod proof of live, unresponsive and gone (dispatch://INFRA-392)

  bash scripts/e2e/envoy-session-states.sh [--checks <name,name,…>] [--list]

--checks, or ENVOY_SESSION_STATES_CHECKS, runs only the named checks; unset, every check runs.
--list prints each check's name and what it needs beyond the listener and NATS.
EOF
}

list_checks() {
  local row name rest
  for row in "${checks[@]}"; do
    name=${row%% *}
    rest=${row#* }
    [ "$rest" != "$row" ] || rest="nothing more"
    printf '%-42s %s\n' "$name" "$rest"
  done
}

# wanted, pg_dsn and evidence were read above, before the sweep.
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

[ -n "$evidence" ] || evidence=$(mktemp -d "${TMPDIR:-/tmp}/envoy-session-states-evidence.XXXXXXXX")
mkdir -p "$evidence/logs" "$evidence/answers"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

work=$(mktemp -d "${TMPDIR:-/tmp}/envoy-session-states.$$.XXXXXXXX")

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
needs=
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
for name in "${selected[@]}"; do
  for row in "${checks[@]}"; do
    [ "${row%% *}" = "$name" ] || continue
    [ "${row#* }" = "$row" ] || needs="$needs ${row#* }"
  done
done
needs_omp=
needs_claude=
needs_opencode=
needs_dispatch=
needs_model=
case " $needs " in *" omp "*) needs_omp=1 ;; esac
case " $needs " in *" claude "*) needs_claude=1 ;; esac
case " $needs " in *" opencode "*) needs_opencode=1 ;; esac
case " $needs " in *" dispatch "*) needs_dispatch=1 ;; esac
case " $needs " in *" model "*) needs_model=1 ;; esac
# Dispatch is reached by the `dispatch` command the pi-envoy pack ships, and every agent needs that
# pack too, so one need pulls the other's build.
[ -z "$needs_dispatch" ] || needs_omp=${needs_omp:-build-only}

# ---- the run's state ----------------------------------------------------------------------------
natsd_pid=
listener_a_pid=
listener_b_pid=
listener_a_keeper_pid=
listener_b_keeper_pid=
dispatch_keeper_pid=
maxage_pid=
shortlistener_pid=
dispatch_pid=
exceptions_pid=
omp_pid=
omp_fd=
claude_pid=
claude_fd=
opencode_pid=
database=
nats_port=
nats_url=
port_a=
port_b=
short_port=
maxage_port=
dispatch_port=
dispatch_url=
opencode_port=
omp_session=
claude_session=
opencode_session=
claude_bin=
opencode_bin=
opencode_plugin=
claude_version=
# pg_dsn is read at the top, before the sweep.
# The windows every reader of this run judges a record's age against; the header says why each is
# this number.
live_window=30s
gone_horizon=10m
reap_interval=20s
heartbeat_ms=5000
max_age=2h
max_age_shown=2h0m0s
shipping_max_age=5m
shipping_max_age_shown=5m0s

# own_session PIDFILE COMMAND… runs COMMAND in a session of its own and writes its pid to PIDFILE.
# Every agent this proof stalls needs one. A stopped process in the run's own process group makes
# the kernel send that whole group a SIGHUP and a SIGCONT the moment the group counts as orphaned
# — which under a supervisor it already does — so a plain `kill -STOP` would kill this script and
# un-stall the very session it meant to stall. The pid comes back through a file because `setsid
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

# stop_agent PID ends one agent process the run started, and only while its command line or its
# environment still names this run's work directory, so a pid the kernel gave to another process
# since is never signalled. It is continued first: a check stops an agent with SIGSTOP, and a
# stopped process keeps a TERM pending. The agent leads a session of its own (own_session), so
# what it started goes with it.
stop_agent() {
  [ -n "${1:-}" ] || return 0
  grep -qsF "$work" "/proc/$1/cmdline" || grep -qsF "$work" "/proc/$1/environ" || return 0
  signalable "$1" || return 0
  kill -CONT "$1" 2>/dev/null || true
  kill -TERM -- -"$1" 2>/dev/null || true
  stop_pid "$1"
  kill -KILL -- -"$1" 2>/dev/null || true
}

cleanup() {
  local status=$1
  trap - EXIT INT TERM
  set +e
  stop_agent "$omp_pid"
  stop_agent "$claude_pid"
  stop_agent "$opencode_pid"
  stop_pid "$exceptions_pid"
  stop_pid "$dispatch_pid"
  stop_pid "$shortlistener_pid"
  stop_pid "$maxage_pid"
  stop_pid "$listener_a_pid"
  stop_pid "$listener_b_pid"
  stop_pid "$listener_a_keeper_pid"
  stop_pid "$listener_b_keeper_pid"
  stop_pid "$dispatch_keeper_pid"
  stop_pid "$natsd_pid"
  if [ -n "$database" ]; then
    psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "drop database if exists \"$database\"" >/dev/null 2>&1 ||
      echo "could not drop database $database; drop it by hand" >&2
  fi
  rm -rf "$work"
  [ -z "$measured_kill_latency" ] ||
    echo "   measured SIGKILL-to-gone latency: ${measured_kill_latency}s"
  if [ -n "$ok" ] && [ "$ran" -eq "$passed" ] && [ "$status" -eq 0 ]; then
    echo "envoy session states e2e: $ran checks ran, $passed passed — PASS ($selection, ${SECONDS}s, evidence: $evidence)"
  else
    [ "$status" -ne 0 ] || status=1
    echo "envoy session states e2e: $ran checks ran, $passed passed — FAIL at $check ($selection, ${SECONDS}s, evidence: $evidence)" >&2
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
  ran=$((ran + 1))
  if "check_${name//-/_}"; then
    passed=$((passed + 1))
    echo "PASS $name"
  else
    echo "FAIL $name: ${reason:-no reason recorded}" >&2
  fi
}

# refuse MESSAGE records why the running check failed and fails it. A check reads
# `<assertion> || refuse "<what was wrong>"`, so every failure leaves one sentence a reader can act
# on rather than a bare non-zero.
refuse() {
  reason=$1
  return 1
}

# wait_until SECONDS DESCRIPTION COMMAND… polls COMMAND twice a second until it succeeds, and
# fails the check when it has not by SECONDS. It is lib/rig.sh's until_true for a check: a bounded
# named wait that fails its own check instead of ending the run, so the checks after it still run.
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

# log_count FILE PATTERN is how many lines of FILE match PATTERN: 0 for a file holding none, and 0
# for one that is not there yet. `grep -c` prints the count and still exits 1 when it is 0, so a
# `… || printf 0` fallback prints that 0 and another after it and leaves a number no arithmetic
# can read.
log_count() {
  local count
  count=$(grep -c -- "$2" "$1" 2>/dev/null) || count=0
  printf '%s\n' "$count"
}

# log_count_at_least FILE PATTERN N: FILE holds at least N lines matching PATTERN. The wait for a
# server that has been restarted, and the wait for one more settled agent turn, are the same
# question asked of a log.
log_count_at_least() { [ "$(log_count "$1" "$2")" -ge "$3" ]; }

# ---- the listener's HTTP surface ------------------------------------------------------------------
# Every call names the listener it asks, because half this proof is that two listeners answer the
# same question the same way. api sets api_status and writes the body to $answer rather than
# printing either: a caller that read the status out of a command substitution would lose the body
# with the subshell, and a refusal here has to be as readable as an answer.
api() { # api PORT METHOD PATH [BODY]
  local port=$1 method=$2 path=$3 body=${4:-} slug
  slug=$(printf '%s' "$path" | tr -c '[:alnum:]' '_' | cut -c1-48)
  answer=$evidence/answers/$check.$port$slug.$RANDOM.json
  local -a args=(-sS --max-time 20 -o "$answer" -w '%{http_code}' -X "$method" -H "@$work/envoy-auth-header")
  [ -z "$body" ] || args+=(-H 'content-type: application/json' --data "$body")
  api_status=$(curl "${args[@]}" "http://127.0.0.1:$port$path" 2>>"$evidence/logs/curl.log") || api_status=000
  [ "$api_status" = 200 ]
}

# registered SESSION PORT SELF TOPICS… writes one session's registration straight onto the wire:
# the same POST /v1/interests/subscribe a plugin's heartbeat makes, for the sessions a check needs
# to age by the second rather than by a process's own clock. PORT is the session's serving port, 0
# for one that receives on its own NATS connection instead.
registered() {
  local session=$1 port=$2 self=$3 body
  shift 3
  body=$(jq -cn --arg s "$session" --arg d "$work/registered" --arg t "rig $session" \
    --argjson p "$port" --argjson self "$self" --args \
    '{session_id:$s,dir:$d,title:$t,port:$p,self_subscribed:$self,driving:true,topics:$ARGS.positional}' "$@")
  api "$port_a" POST /v1/interests/subscribe "$body" ||
    refuse "registering $session answered $api_status: $(head -c 200 "$answer")"
}

# session_state PORT SESSION [INCLUDE] sets `state` to what a listener says of a session: the row's
# own `state` for an answered read, `gone` for the 404 that names no live session. Any other answer
# leaves `state` empty and fails, so a caller never reads a 503 as a verdict. It sets globals
# rather than printing them because the body it leaves in $answer is half the answer, and a command
# substitution would take that body down with its subshell.
session_state() {
  local path=/v1/registry/$2
  [ -z "${3:-}" ] || path="$path?include=$3"
  state=
  api "$1" GET "$path" || true
  case $api_status in
    200) state=$(jq -r '.state' "$answer") ;;
    404) state=$(jq -r '.state // "gone"' "$answer") ;;
    *) return 1 ;;
  esac
}

# state_is PORT SESSION INCLUDE WANT is session_state as a predicate, for wait_until.
state_is() { session_state "$1" "$2" "$3" && [ "$state" = "$4" ]; }

# listed PORT SESSION [INCLUDE] succeeds when GET /v1/sessions names the session exactly once.
listed() {
  local path=/v1/sessions
  [ -z "${3:-}" ] || path="$path?include=$3"
  api "$1" GET "$path" || return 1
  jq -e --arg s "$2" 'map(select(.session_id == $s)) | length == 1' "$answer" >/dev/null
}

# last_seen PORT SESSION prints the session's last registration in epoch milliseconds.
last_seen() {
  api "$1" GET "/v1/registry/$2?include=unresponsive" || return 1
  jq -er '.last_seen' "$answer"
}

# healthz_max_age_is PORT WANT: that listener's /healthz says the session bucket carries WANT.
healthz_max_age_is() {
  api "$1" GET /healthz || return 1
  [ "$(jq -r '.session_max_age // empty' "$answer")" = "$2" ]
}

# role_reason_is PORT ROLE WANT: the role lookup's `reason` is WANT (`lapsed` while its holder has
# stopped answering, `unclaimed` once nothing holds it).
role_reason_is() {
  api "$1" GET "/v1/roles/$2" || true
  [ "$(jq -r '.reason // "held"' "$answer")" = "$3" ]
}

# topics_hold PORT SESSION TOPIC: the session's registry row carries that topic.
topics_hold() {
  api "$1" GET "/v1/registry/$2" || return 1
  jq -e --arg t "$3" '.topics | index($t)' "$answer" >/dev/null
}

# ---- NATS, straight ------------------------------------------------------------------------------
# Two reads this proof cannot make over HTTP: a publish onto a role's topic that never asks the
# listener at all, and the role claim as the `envoy_roles` bucket really holds it. Both run the
# NATS client this repository already depends on, from the checkout, so neither needs a `nats` CLI.
nats_publish() {
  (cd "$root" && ESS_NATS_URL=$nats_url ESS_TOPIC=$1 ESS_BODY=$2 bun -e '
    import { connect } from "nats";
    const nc = await connect({ servers: process.env.ESS_NATS_URL });
    nc.publish(process.env.ESS_TOPIC, new TextEncoder().encode(process.env.ESS_BODY ?? ""));
    await nc.flush();
    await nc.close();
  ')
}

# role_record ROLE prints the role's claim record as envoy_roles holds it, and nothing when the
# bucket holds no key for it.
role_record() {
  (cd "$root" && ESS_NATS_URL=$nats_url ESS_ROLE=$1 bun -e '
    import { connect } from "nats";
    const nc = await connect({ servers: process.env.ESS_NATS_URL });
    const kv = await nc.jetstream().views.kv("envoy_roles", { bindOnly: true });
    const entry = await kv.get(process.env.ESS_ROLE);
    if (entry !== null) process.stdout.write(new TextDecoder().decode(entry.value));
    await nc.close();
  ')
}

# watch_exceptions starts the run's one subscriber of notifications.envoy.exceptions.>, whose file
# must stay empty: a message stored for a stalled session is not an exception, and the lane that
# used to report one is the lane this change quiets.
watch_exceptions() {
  : >"$work/exceptions.jsonl"
  # shellcheck disable=SC2016 # the bun program is passed verbatim; ${…} inside it is JavaScript's
  (cd "$root" && ESS_NATS_URL=$nats_url ESS_OUT=$work/exceptions.jsonl exec bun -e '
    import { appendFileSync } from "node:fs";
    import { connect } from "nats";
    const nc = await connect({ servers: process.env.ESS_NATS_URL });
    for await (const message of nc.subscribe("notifications.envoy.exceptions.>")) {
      appendFileSync(process.env.ESS_OUT, `${message.subject}\n`);
    }
  ') >>"$evidence/logs/exceptions.log" 2>&1 &
  exceptions_pid=$!
}

# ---- prerequisites --------------------------------------------------------------------------------
begin prerequisites
model_base_url=${LEGION_E2E_MODEL_BASE_URL:-}
model_key_file=${LEGION_E2E_MODEL_KEY_FILE:-}
model_id=${LEGION_E2E_MODEL_ID:-}
claude_model=${LEGION_E2E_CLAUDE_MODEL:-}
omp_bin=${LEGION_E2E_OMP_PATH:-}
claude_pin=${LEGION_E2E_CLAUDE_CODE_PIN:-/opt/legion/claude-code}
opencode_version=${LEGION_E2E_OPENCODE_VERSION:-}

note "selection: $selection (${#selected[@]} checks)"
for tool in bun curl go jq npm od openssl python3 tar; do
  command -v "$tool" >/dev/null || fail "$tool is required and is not on PATH"
done

if [ -n "$needs_model" ]; then
  [ -n "$model_base_url" ] ||
    fail "LEGION_E2E_MODEL_BASE_URL is required: the Anthropic-compatible endpoint this pod's model route answers on, which the three agent runtimes call. Without it no agent can take a turn, and the selected checks include one that needs one"
  if why=$(model_base_url_refusal "$model_base_url") && [ -n "$why" ]; then
    fail "LEGION_E2E_MODEL_BASE_URL $why: name the model endpoint's base URL"
  fi
  [ -n "$model_key_file" ] ||
    fail "LEGION_E2E_MODEL_KEY_FILE is required: a file holding the key the model endpoint accepts"
  [ -r "$model_key_file" ] ||
    fail "LEGION_E2E_MODEL_KEY_FILE names $model_key_file, which cannot be read"
  [ -n "$model_id" ] ||
    fail "LEGION_E2E_MODEL_ID is required: the anthropic/<id> Oh My Pi and OpenCode run"
  [[ $model_id =~ ^anthropic/[^[:space:]]+$ ]] ||
    fail "LEGION_E2E_MODEL_ID is '$model_id'; it must be anthropic/<id>, the provider both Oh My Pi and OpenCode reach the endpoint through"
  [ -n "$claude_model" ] ||
    fail "LEGION_E2E_CLAUDE_MODEL is required: the model id Claude Code runs against that endpoint"
fi
if [ -n "$needs_dispatch" ]; then
  [ -n "$pg_dsn" ] ||
    fail "ENVOY_SESSION_STATES_PG_DSN is required: a loopback PostgreSQL with pgvector the run creates its own database in. The selected checks include one that reads an issue claim"
  case $pg_dsn in
    postgres://* | postgresql://*) ;;
    *) fail "ENVOY_SESSION_STATES_PG_DSN must be a postgres:// or postgresql:// URL" ;;
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
    *) fail "ENVOY_SESSION_STATES_PG_DSN must name 127.0.0.1, localhost or ::1, not '$db_host': the run creates and drops a database in it" ;;
  esac
  command -v psql >/dev/null || fail "psql is required for the checks that read an issue claim"
fi
if [ -n "$needs_omp" ]; then
  [ -n "$omp_bin" ] || omp_bin=$(command -v omp || true)
  [ -n "$omp_bin" ] || { [ ! -x /opt/omp/bin/omp ] || omp_bin=/opt/omp/bin/omp; }
  [ -n "$omp_bin" ] ||
    fail "LEGION_E2E_OMP_PATH is required: no omp is on PATH and the worker image's /opt/omp/bin/omp is absent"
  [ -x "$omp_bin" ] || fail "LEGION_E2E_OMP_PATH names $omp_bin, which is not an executable file"
fi
if [ -n "$needs_claude" ]; then
  { [ -r "$claude_pin/package.json" ] && [ -r "$claude_pin/package-lock.json" ]; } ||
    fail "LEGION_E2E_CLAUDE_CODE_PIN names $claude_pin, which holds no package.json and package-lock.json pinning @anthropic-ai/claude-code; the worker image carries that pin and no Claude Code"
  claude_version=$(jq -er '.dependencies["@anthropic-ai/claude-code"]' "$claude_pin/package.json") ||
    fail "$claude_pin/package.json pins no @anthropic-ai/claude-code"
fi
if [ -n "$needs_opencode" ]; then
  if [ -z "$opencode_version" ]; then
    opencode_version=$(jq -er '.dependencies["@opencode-ai/plugin"]' "$root/packages/envoy-plugin/package.json" | tr -d '~^') ||
      fail "packages/envoy-plugin/package.json pins no @opencode-ai/plugin, so the opencode-ai release to install is unknown; name it in LEGION_E2E_OPENCODE_VERSION"
  fi
fi

mkdir -p "$work/bin" "$work/registered" "$work/home/.omp" "$work/agent" "$work/tmp" "$work/omp-cwd"
(
  umask 077
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/envoy-token"
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/dispatch-token"
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header"
)

if [ -n "$needs_model" ]; then
  (umask 077 && tr -d '[:space:]' <"$model_key_file" >"$work/model-key")
  [ -s "$work/model-key" ] || fail "LEGION_E2E_MODEL_KEY_FILE ($model_key_file) holds no key"
  model_key_preflight "$model_base_url" "$work/model-key" "$work/model-header" \
    "LEGION_E2E_MODEL_KEY_FILE ($model_key_file)"
  note "the model endpoint accepts the key; Oh My Pi and OpenCode run $model_id, Claude Code $claude_model"
fi
note "work $work; evidence $evidence"
echo "ok prerequisites"

# ---- builds ----------------------------------------------------------------------------------------
begin build
go -C "$root/packages/envoy" build -o "$work/bin/envoy-listener" ./cmd/listener ||
  fail "./cmd/listener did not build"
go -C "$root/packages/envoy" build -o "$work/bin/natsd" ./internal/testnats/cmd/natsd ||
  fail "./internal/testnats/cmd/natsd did not build"
note "built the listener and natsd from this checkout"
if [ -n "$needs_dispatch" ]; then
  go -C "$root/packages/envoy" build -o "$work/bin/envoy-dispatch" ./cmd/dispatch ||
    fail "./cmd/dispatch did not build"
  note "built envoy-dispatch from this checkout"
fi
if [ -n "$needs_omp" ] || [ -n "$needs_opencode" ] || [ -n "$needs_claude" ]; then
  (cd "$root" && bun install --frozen-lockfile) >"$evidence/logs/bun-install.log" 2>&1 ||
    fail "bun install failed; see $evidence/logs/bun-install.log"
fi
if [ -n "$needs_omp" ]; then
  # pi-envoy packed exactly as the release packs it and unpacked as the worker image unpacks it, so
  # `--extension` loads what a Legion pane loads; its bin/dispatch is the branch's own `dispatch`
  # command, which the issue-claim check runs as two different sessions.
  tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" pi-envoy "$work/pack" 2>"$evidence/logs/pack-pi-envoy.log") ||
    fail "pi-envoy did not pack; see $evidence/logs/pack-pi-envoy.log"
  mkdir -p "$work/pi-envoy"
  tar xzf "$tarball" -C "$work/pi-envoy" --strip-components=1
  note "pi-envoy $(jq -r .version "$work/pi-envoy/package.json") from this checkout"
fi
if [ -n "$needs_claude" ]; then
  # Claude Code itself: the pinned release, installed from the image's own pin with `npm ci` into a
  # directory of the run's own under $TMPDIR, its cache under $TMPDIR too — the install a pod's
  # launch makes, held to the same version check.
  mkdir -p "$work/claude-code" "$work/npm-cache" "$work/claude-config"
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
  # The claude-envoy plugin the session loads, built from this checkout outside it: a pull
  # request's tree carries no fresh dist/ or legion/.
  bun "$root/packages/claude-envoy/scripts/build.ts" --out "$work/claude-envoy" \
    >"$evidence/logs/build-claude-envoy.log" 2>&1 ||
    fail "packages/claude-envoy did not build; see $evidence/logs/build-claude-envoy.log"
  jq -n --arg bun "$(command -v bun)" --arg channel "$work/claude-envoy/dist/envoy-channel.js" \
    --arg data "$work/claude-config/envoy-plugin-data" \
    '{mcpServers:{legion:{command:$bun,args:[$channel],env:{CLAUDE_PLUGIN_DATA:$data}}}}' >"$work/claude-mcp.json"
  note "Claude Code $installed from $claude_pin, with this checkout's claude-envoy as its one MCP server"
fi
if [ -n "$needs_opencode" ]; then
  # OpenCode, the release this checkout's plugin pins its @opencode-ai/plugin to, installed under
  # $TMPDIR, with the plugin packed from this checkout and unpacked beside it, as pi-envoy is
  # (lib/pack-plugin.sh). The pack is what makes it load: the plugin reads the Legion skills from
  # beside its own bundle, its tarball stages them there at prepack
  # (`skillsDirectory`, packages/envoy-plugin/src/server.ts), and a plain `bun run build` leaves
  # dist/ in the checkout with no skills/ next to it — with Dispatch configured the plugin then
  # refuses to load at all, OpenCode serves on with no Envoy tools, and no session ever registers.
  # The unpacked tarball's one external import, @opencode-ai/plugin, is resolved through the
  # checkout's own node_modules, which the `bun install --frozen-lockfile` above holds the pinned
  # release in; the bundle inlines everything else.
  mkdir -p "$work/opencode" "$work/npm-cache" "$work/opencode-pack" "$work/opencode-plugin"
  printf '{"name":"envoy-session-states-opencode","private":true}\n' >"$work/opencode/package.json"
  (cd "$work/opencode" && npm install --no-audit --no-fund --cache "$work/npm-cache" "opencode-ai@$opencode_version") \
    >"$evidence/logs/npm-opencode.log" 2>&1 ||
    fail "installing opencode-ai@$opencode_version failed; see $evidence/logs/npm-opencode.log"
  opencode_bin=$work/opencode/node_modules/.bin/opencode
  [ -x "$opencode_bin" ] || fail "the install left no executable at $opencode_bin"
  (cd "$root/packages/envoy-plugin" && bun pm pack --destination "$work/opencode-pack") \
    >"$evidence/logs/build-opencode-plugin.log" 2>&1 ||
    fail "packages/envoy-plugin did not pack; see $evidence/logs/build-opencode-plugin.log"
  opencode_tarballs=("$work"/opencode-pack/*.tgz)
  { [ "${#opencode_tarballs[@]}" -eq 1 ] && [ -f "${opencode_tarballs[0]}" ]; } ||
    fail "packing packages/envoy-plugin left ${#opencode_tarballs[@]} entries matching $work/opencode-pack/*.tgz; expected one tarball"
  opencode_plugin=$work/opencode-plugin
  tar xzf "${opencode_tarballs[0]}" -C "$opencode_plugin" --strip-components=1
  ln -sfT "$root/packages/envoy-plugin/node_modules" "$opencode_plugin/node_modules"
  [ -d "$opencode_plugin/skills/dispatch-first" ] ||
    fail "the packed plugin in $opencode_plugin carries no skills/dispatch-first, and refuses to load without it"
  note "OpenCode $("$opencode_bin" --version) with this checkout's @sjawhar/opencode-legion-envoy, packed as the release packs it"
fi
echo "ok build"

# ---- services ---------------------------------------------------------------------------------------
begin services
start_process natsd "$work/bin/natsd" -p -1 -store_dir "$work/nats-store"
await_start natsd "$natsd_pid" 60 "natsd to be ready" grep -q 'Server is ready' "$evidence/logs/natsd.log"
nats_port=$(report_port "$evidence/logs/natsd.log" 'Listening for client connections on [^ ]*:') ||
  fail "natsd logged no client port; see $evidence/logs/natsd.log"
nats_url=nats://127.0.0.1:$nats_port
note "NATS on $nats_url, its JetStream store in $work/nats-store"

# start_listener NAME KEEPER starts one of the two running listeners on the port its keeper holds,
# so a listener stopped and started again comes back on the same port and nothing can take it in
# between. Neither is given ENVOY_SESSION_MAX_AGE: both adopt the age the bucket carries, which is
# what lets a listener of the run's own move it under them. Each listener of the run gets a machine
# id of its own, because the durable consumer a listener binds is named after it and a second
# listener of the same name is refused the bind and shuts itself down.
start_listener() {
  start_kept "$1" "$2" ENVOY_LISTEN_FD -- env -i PATH="$PATH" HOME=/nonexistent \
    ENVOY_LISTEN_FD=3 ENVOY_API_TOKEN="$(cat "$work/envoy-token")" \
    ENVOY_MACHINE_ID="envoy-session-states-$$-$1" ENVOY_LISTEN_HOST=127.0.0.1 NATS_URLS="$nats_url" \
    ENVOY_SESSION_LIVE_WINDOW="$live_window" ENVOY_SESSION_GONE_HORIZON="$gone_horizon" \
    ENVOY_SESSION_REAP_INTERVAL="$reap_interval" "$work/bin/envoy-listener"
}

serving() { curl -fsS --max-time 3 -H "@$work/envoy-auth-header" "http://127.0.0.1:$1/v1/sessions"; }

keep_port listener_a_keeper port_a
keep_port listener_b_keeper port_b
start_listener listener_a listener_a_keeper
await_start listener_a "$listener_a_pid" 60 "listener A to serve" serving "$port_a"
start_listener listener_b listener_b_keeper
await_start listener_b "$listener_b_pid" 60 "listener B to serve" serving "$port_b"
note "listener A on 127.0.0.1:$port_a, listener B on 127.0.0.1:$port_b"

# set_max_age AGE runs one listener of the run's own with ENVOY_SESSION_MAX_AGE=AGE and stops it:
# its registry writes that age on the session bucket as it opens, and the two running listeners
# read it back on their own reaper passes. It is how this proof moves the bucket's maximum age
# without restarting the listeners whose /healthz it then reads.
set_max_age() {
  rm -f "$work/maxage.port"
  start_held maxage maxage_port ENVOY_LISTEN_FD -- env -i PATH="$PATH" HOME=/nonexistent \
    ENVOY_LISTEN_FD=3 ENVOY_API_TOKEN="$(cat "$work/envoy-token")" \
    ENVOY_MACHINE_ID="envoy-session-states-maxage-$$" ENVOY_LISTEN_HOST=127.0.0.1 \
    NATS_URLS="$nats_url" ENVOY_SESSION_MAX_AGE="$1" "$work/bin/envoy-listener"
  await_start maxage "$maxage_pid" 60 "the maximum-age listener to serve" serving "$maxage_port"
  stop_pid "$maxage_pid"
  maxage_pid=
}

set_max_age "$max_age"
until_true 120 "both listeners to read the bucket's $max_age maximum age" \
  healthz_max_age_is "$port_a" "$max_age_shown"
until_true 120 "listener B to read the bucket's $max_age maximum age" \
  healthz_max_age_is "$port_b" "$max_age_shown"
note "the session bucket's maximum age is $max_age, written by a listener of the run's own"
watch_exceptions

if [ -n "$needs_dispatch" ]; then
  database=envoy_session_states_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
  psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "create database \"$database\"" >/dev/null ||
    fail "could not create database $database in ENVOY_SESSION_STATES_PG_DSN"
  database_url=${pg_dsn%%\?*}
  database_url=${database_url%/*}/$database
  case $pg_dsn in *\?*) database_url="$database_url?${pg_dsn#*\?}" ;; esac
  psql "$database_url" -X -q -v ON_ERROR_STOP=1 -c 'create extension if not exists vector' >/dev/null ||
    fail "the database $database has no pgvector; ENVOY_SESSION_STATES_PG_DSN must name a PostgreSQL carrying it"
  mkdir -p "$work/web-dist"
  keep_port dispatch_keeper dispatch_port
  start_kept dispatch dispatch_keeper DISPATCH_LISTEN_FD -- env -i PATH="$PATH" HOME=/nonexistent \
    XDG_CONFIG_HOME=/nonexistent XDG_DATA_HOME=/nonexistent DISPATCH_LISTEN_FD=3 \
    DATABASE_URL="$database_url" DISPATCH_AGENT_TOKEN="$(cat "$work/dispatch-token")" \
    DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1 \
    DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_SERVER_URL="http://127.0.0.1:$dispatch_port" \
    DISPATCH_SIGNING_KEY="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')" \
    DISPATCH_WEB_DIST="$work/web-dist" ENVOY_URL="http://127.0.0.1:$port_a" \
    ENVOY_TOKEN="$(cat "$work/envoy-token")" NATS_URLS="$nats_url" "$work/bin/envoy-dispatch"
  dispatch_url=http://127.0.0.1:$dispatch_port
  await_start dispatch "$dispatch_pid" 120 "Dispatch to serve" \
    curl -fsS --max-time 3 "$dispatch_url/healthz"
  note "Dispatch on $dispatch_url over the run's own database"
fi
echo "ok services"

# ---- Dispatch's own surface ---------------------------------------------------------------------------
# Every write names its own session as `actor`, the identity `dispatch claim` fills from the host's
# own runtime, and reaches Dispatch over the run's agent token.
dispatch_project=ESS$(od -An -N2 -tu2 /dev/urandom | tr -d ' \n')

dispatch_api() { # dispatch_api METHOD PATH [BODY]
  local method=$1 path=$2 body=${3:-}
  answer=$evidence/answers/$check.dispatch.$RANDOM.json
  local -a args=(-sS --max-time 20 -o "$answer" -w '%{http_code}' -X "$method"
    -H "Authorization: Bearer $(cat "$work/dispatch-token")")
  [ -z "$body" ] || args+=(-H 'content-type: application/json' --data "$body")
  api_status=$(curl "${args[@]}" "$dispatch_url$path" 2>>"$evidence/logs/curl.log") || api_status=000
  [ "$api_status" = 200 ] || [ "$api_status" = 201 ]
}

# dispatch_cli SESSION ARGS… runs the branch's own `dispatch` command as SESSION: the CLI the
# pi-envoy pack ships, against the run's Dispatch, with nothing of the caller's environment but a
# home and a state directory of the run's own, which the command makes its session state under.
dispatch_cli() {
  local session=$1
  shift
  mkdir -p "$work/cli-home" "$work/cli-state"
  env -i PATH="$PATH" HOME="$work/cli-home" XDG_CONFIG_HOME="$work/cli-home/config" \
    XDG_STATE_HOME="$work/cli-state" XDG_DATA_HOME="$work/cli-home/data" \
    XDG_CACHE_HOME="$work/cli-home/cache" DISPATCH_STATE_DIR="$work/cli-state/dispatch" \
    DISPATCH_URL="$dispatch_url" DISPATCH_TOKEN="$(cat "$work/dispatch-token")" \
    DISPATCH_SESSION_ID="$session" DISPATCH_HOST=omp "$work/pi-envoy/bin/dispatch" "$@"
}

# make_issue SESSION TITLE creates the run's project when Dispatch does not hold it yet and one
# issue in it, and prints the issue's key.
make_issue() {
  local session=$1 title=$2
  if ! dispatch_api POST /api/v1/projects \
    "$(jq -cn --arg k "$dispatch_project" --arg s "$session" '{key:$k,name:$k,actor:{id:$s,kind:"session"}}')"; then
    { [ "$api_status" = 409 ] && jq -e '.code == "PROJECT_EXISTS"' "$answer" >/dev/null; } || return 1
  fi
  dispatch_api POST /api/v1/issues \
    "$(jq -cn --arg p "$dispatch_project" --arg t "$title" --arg s "$session" \
      '{project:$p,title:$t,force:true,spec:"A session-state proof issue.",actor:{id:$s,kind:"session"}}')" || return 1
  jq -er .key "$answer"
}

# ---- Oh My Pi ------------------------------------------------------------------------------------------
# It runs in `--mode rpc`, as a Legion pane runs it, with this checkout's Envoy plugin, under a HOME
# and agent directory of the run's own. Its RPC is newline JSON both ways, so the rig writes
# requests on a FIFO it holds open and reads answers out of the process's own stdout.
write_omp_profile() {
  local natives=$HOME/.omp/natives role url id key
  # The caller's native-module cache, linked as lib/omp-home.sh links it, so a fresh home downloads
  # nothing and the caller's own ~/.omp is neither written nor removed.
  if [ -n "${XDG_DATA_HOME:-}" ] && [ -d "$XDG_DATA_HOME/omp" ]; then natives=$XDG_DATA_HOME/omp/natives; fi
  mkdir -p "$natives" "$work/home/.omp" "$work/agent"
  ln -sfT "$natives" "$work/home/.omp/natives"
  # A selection with no check that needs a model turn needs no model route, and still needs a
  # session: the profile then names a provider on a loopback port nothing listens on, so Oh My Pi
  # starts, registers and heartbeats as it always does, and the one thing it cannot do is take a
  # turn — which no check of that selection asks it to.
  url=$model_base_url
  id=${model_id#*/}
  key="'!cat $work/model-key'"
  if [ -z "$url" ]; then
    url=http://127.0.0.1:1
    id=rig-unreachable-model
    key=rig-unreachable-key
  fi
  {
    printf 'providers:\n  anthropic:\n'
    printf '    baseUrl: %s\n' "$url"
    printf '    auth: apiKey\n    api: anthropic-messages\n'
    printf '    apiKey: %s\n' "$key"
    printf '    models:\n'
    printf '      - {id: %s, name: %s, reasoning: true, input: [text], contextWindow: 200000, maxTokens: 8192}\n' \
      "$id" "$id"
  } >"$work/agent/models.yml"
  {
    echo 'defaultThinkingLevel: low'
    echo 'modelRoles:'
    for role in default smol slow vision plan commit tiny task advisor review oracle deep; do
      echo "  $role: anthropic/$id"
    done
  } >"$work/agent/config.yml"
}

# omp_launch LABEL PIDVAR FDVAR starts one Oh My Pi of the run on $work/<label>.in and
# $work/<label>.out, sets PIDVAR to its pid and FDVAR to a descriptor open on its stdin. Two checks
# each want a session of their own, so the launch is a function.
#
# It opens the FIFO read-write because a writer's own open waits for a reader, and an Oh My Pi that
# failed to start never is one; and it runs the session in a session of its own (own_session),
# because a check stalls it with SIGSTOP.
omp_launch() {
  local label=$1
  local in=$work/$label.in out=$work/$label.out pidfile=$work/$label.pid fd
  rm -f "$in" "$pidfile"
  mkfifo "$in"
  : >"$out"
  own_session "$pidfile" \
    env -i PATH="$PATH" HOME="$work/home" TMPDIR="$work/tmp" PI_CODING_AGENT_DIR="$work/agent" \
    DISPATCH_URL="$dispatch_url" DISPATCH_TOKEN_FILE="$work/dispatch-token" \
    ENVOY_URL="http://127.0.0.1:$port_a" ENVOY_TOKEN_FILE="$work/envoy-token" \
    ENVOY_NATS_URL="$nats_url" ENVOY_HEARTBEAT_MS="$heartbeat_ms" \
    ENVOY_MACHINE_ID="envoy-session-states-$$" \
    "$omp_bin" --extension "$work/pi-envoy" --mode rpc \
    <"$in" >"$out" 2>"$evidence/logs/omp-$label.stderr.log" &
  exec {fd}<>"$in"
  printf -v "$3" '%s' "$fd"
  agent_pid "$pidfile" "$2"
}

rpc_answered() { # rpc_answered OUT ID
  [ -n "$(jq -r -R --arg id "$2" 'fromjson? | select(.id == $id) | @json' "$1" | sed -n 1p)" ]
}

rpc_answer() { jq -r -R --arg id "$2" 'fromjson? | select(.id == $id) | @json' "$1" | sed -n 1p; }

# running PID is whether that process is still there, so a wait for an agent's first line ends the
# moment the agent has gone instead of running out its own clock.
running() { kill -0 "$1" 2>/dev/null; }

omp_start() {
  write_omp_profile
  omp_launch omp omp_pid omp_fd
  wait_until 240 "Oh My Pi to print ready" grep -q '"type":"ready"' "$work/omp.out" ||
    refuse "Oh My Pi did not print ready$(running "$omp_pid" || printf ' and has gone'): $(tail -c 400 "$evidence/logs/omp-omp.stderr.log" 2>/dev/null)"
}

# omp_ask TYPE EXTRA SECONDS sends one RPC request and prints its answer. EXTRA is a JSON object
# merged into the request, `{}` for a request that takes no field of its own.
omp_ask() {
  local id="rig-$RANDOM-$RANDOM" extra=${2:-}
  [ -n "$extra" ] || extra='{}'
  jq -cn --arg t "$1" --arg id "$id" --argjson extra "$extra" '$extra + {type:$t,id:$id}' >&"$omp_fd"
  wait_until "${3:-240}" "Oh My Pi to answer $1" rpc_answered "$work/omp.out" "$id" || return 1
  rpc_answer "$work/omp.out" "$id"
}

# omp_typed TEXT types TEXT at the session's terminal — a slash command, or a prompt the model
# answers — and answers when the host has taken it. The `prompt` request is answered the moment
# the text is taken, not when whatever it starts is over.
omp_typed() { omp_ask prompt "$(jq -cn --arg m "$1" '{message:$m}')" "${2:-600}"; }

# omp_turn TEXT types TEXT and waits for the model turn it starts to end, the way claude_turn
# waits for Claude Code's own `result`. A check that reads what the model did — its tool calls and
# their results, which the host writes to the session file — has to read it after the turn, and
# `prompt`'s own answer comes before the turn has even started.
omp_turn() {
  local after
  after=$(($(log_count "$work/omp.out" '"type":"agent_end"') + 1))
  omp_typed "$1" "${2:-600}" || return 1
  wait_until "${2:-600}" "Oh My Pi to finish its turn" \
    log_count_at_least "$work/omp.out" '"type":"agent_end"' "$after"
}

omp_session_id() { omp_ask get_state | jq -r '.data.sessionId'; }
omp_session_file() { omp_ask get_state | jq -r '.data.sessionFile'; }

# delivery_cards FILE TEXT counts the Envoy deliveries an Oh My Pi session file holds whose card
# names TEXT. A card the extension hands the host is written as one entry carrying both
# `"custom_message"` and the `"envoy-message"` type, which is how
# packages/pi-envoy/extensions/card-delivery-omp.test.ts counts the same entries. Counting every
# line that merely names a delivery's text would count the model's own reply quoting it back, a
# tool call it made with it and that call's result, and read one delivery as several.
delivery_cards() {
  local count
  count=$(grep -F -e "$2" -- "$1" | grep -F -e '"custom_message"' | grep -c -F -e '"envoy-message"') ||
    count=0
  printf '%s\n' "$count"
}

# stored_cards_landed FILE MARKER: the mention and the direct message naming MARKER have both
# reached that session file.
stored_cards_landed() {
  [ "$(delivery_cards "$1" "mention $2")" -ge 1 ] && [ "$(delivery_cards "$1" "direct $2")" -ge 1 ]
}

# shows_state FILE: FILE shows a session row carrying both `state` and `last_seen`. Each host
# hands its model the listener's rows as JSON, and each of the three files this is asked of — an
# Oh My Pi session file, Claude Code's stream-json and OpenCode's answer — carries that JSON
# inside a JSON string, where every quote is written `\"`. The backslashes are dropped before the
# keys are looked for, because `"state"` is not a substring of `\"state\"` and a check that
# looked for it would read every host as showing nothing.
# shellcheck disable=SC1003 # '\\' is tr's own spelling of one backslash, not an escaped quote
shows_state() {
  tr -d '\\' <"$1" | grep -q '"state"' || return 1
  tr -d '\\' <"$1" | grep -q '"last_seen"'
}

# ---- Claude Code ----------------------------------------------------------------------------------------
# Headless in stream-json both ways, the shape a Legion worker's bridge runs it in
# (claudelaunch.Argv), with this checkout's claude-envoy as its one MCP server. CLAUDE_PLUGIN_DATA
# is in the launch's own environment, not just the MCP server's: the SessionStart hook writes the
# id Claude Code mints on `/clear` to `$CLAUDE_PLUGIN_DATA/sessions/<pid>/session-id` and the
# channel server polls that file, so a hook left on Claude Code's own per-plugin directory and a
# server told another one never meet and the `/clear` is never adopted.
claude_start() { # claude_start SESSION [--resume]
  local session=$1 flag=--session-id
  [ "${2:-}" != --resume ] || flag=--resume
  rm -f "$work/claude.in" "$work/claude.pid"
  mkfifo "$work/claude.in"
  touch "$work/claude.out"
  own_session "$work/claude.pid" \
    env -i PATH="$PATH" HOME="$work/home" TMPDIR="$work/tmp" CLAUDE_CONFIG_DIR="$work/claude-config" \
    CLAUDE_PLUGIN_DATA="$work/claude-config/envoy-plugin-data" \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 ANTHROPIC_BASE_URL="$model_base_url" \
    ANTHROPIC_MODEL="$claude_model" ANTHROPIC_API_KEY="$(cat "$work/model-key")" \
    DISPATCH_URL="$dispatch_url" DISPATCH_TOKEN_FILE="$work/dispatch-token" \
    ENVOY_URL="http://127.0.0.1:$port_a" ENVOY_TOKEN_FILE="$work/envoy-token" \
    ENVOY_NATS_URL="$nats_url" ENVOY_HEARTBEAT_MS="$heartbeat_ms" \
    ENVOY_MACHINE_ID="envoy-session-states-$$" \
    "$claude_bin" -p --input-format stream-json --output-format stream-json --verbose \
    --permission-mode bypassPermissions --strict-mcp-config --mcp-config "$work/claude-mcp.json" \
    --plugin-dir "$work/claude-envoy" --plugin-dir "$work/claude-envoy/legion" "$flag" "$session" \
    <"$work/claude.in" >>"$work/claude.out" 2>>"$evidence/logs/claude.stderr.log" &
  exec {claude_fd}<>"$work/claude.in"
  agent_pid "$work/claude.pid" claude_pid
}

# claude_turn TEXT sends one user turn and waits for that turn's result line.
claude_turn() {
  local after
  after=$(($(log_count "$work/claude.out" '"type":"result"') + 1))
  jq -cn --arg t "$1" '{type:"user",message:{role:"user",content:[{type:"text",text:$t}]}}' >&"$claude_fd"
  wait_until "${2:-600}" "Claude Code to finish its turn" \
    log_count_at_least "$work/claude.out" '"type":"result"' "$after"
}

claude_stop() {
  [ -n "$claude_fd" ] || return 0
  exec {claude_fd}>&-
  claude_fd=
  stop_agent "$claude_pid"
  claude_pid=
}

# ---- OpenCode -------------------------------------------------------------------------------------------
# `opencode serve` is the process a session's plugin registers from, with this checkout's plugin
# loaded by path. OpenCode's Anthropic provider appends `/messages` to the base URL it is given, so
# the provider's baseURL is the endpoint's `/v1`; the serve binds a port the rig picks, since
# `--port 0` is its own default port and not the kernel's choice.
free_port() {
  python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}

opencode_start() {
  mkdir -p "$work/opencode-config/opencode" "$work/opencode-data" "$work/opencode-cwd"
  jq -n --arg url "$model_base_url/v1" --arg key "$(cat "$work/model-key")" \
    --arg id "${model_id#*/}" --arg plugin "file://$opencode_plugin" \
    '{"$schema":"https://opencode.ai/config.json",plugin:[$plugin],
      provider:{anthropic:{options:{baseURL:$url,apiKey:$key},models:{($id):{}}}},
      model:("anthropic/" + $id)}' >"$work/opencode-config/opencode/opencode.json"
  jq -n --arg url "http://127.0.0.1:$port_a" --arg token "$(cat "$work/envoy-token")" \
    --arg dispatch "$dispatch_url" --arg dtoken "$(cat "$work/dispatch-token")" \
    '{envoy:{enabled:true,url:$url,token:$token},
      dispatch:(if $dispatch == "" then {enabled:false} else {enabled:true,serverUrl:$dispatch,token:$dtoken} end)}' \
    >"$work/opencode-config/opencode/envoy.json"
  opencode_port=$(free_port)
  rm -f "$work/opencode.pid"
  own_session "$work/opencode.pid" \
    env -i PATH="$PATH" HOME="$work/home" TMPDIR="$work/tmp" \
    XDG_CONFIG_HOME="$work/opencode-config" XDG_DATA_HOME="$work/opencode-data" \
    OPENCODE_CONFIG_DIR="$work/opencode-config/opencode" ENVOY_HEARTBEAT_MS="$heartbeat_ms" \
    ENVOY_MACHINE_ID="envoy-session-states-$$" \
    "$opencode_bin" serve --hostname 127.0.0.1 --port "$opencode_port" \
    >>"$evidence/logs/opencode.log" 2>&1 &
  agent_pid "$work/opencode.pid" opencode_pid
  await_start opencode "$opencode_pid" 300 "the OpenCode serve to answer" \
    curl -fsS --max-time 3 "http://127.0.0.1:$opencode_port/global/health"
}

opencode_api() { # opencode_api METHOD PATH [BODY]
  local method=$1 path=$2 body=${3:-}
  answer=$evidence/answers/$check.opencode.$RANDOM.json
  local -a args=(-sS --max-time 600 -o "$answer" -w '%{http_code}' -X "$method")
  [ -z "$body" ] || args+=(-H 'content-type: application/json' --data "$body")
  api_status=$(curl "${args[@]}" "http://127.0.0.1:$opencode_port$path" 2>>"$evidence/logs/curl.log") || api_status=000
  [ "$api_status" = 200 ]
}

# opencode_turn SESSION TEXT has that session take one model turn, which is what makes its plugin
# register it: a serve registers only the sessions it has actually run.
opencode_turn() {
  opencode_api POST "/session/$1/message" \
    "$(jq -cn --arg id "${model_id#*/}" --arg t "$2" \
      '{model:{providerID:"anthropic",modelID:$id},parts:[{type:"text",text:$t}]}')" ||
    refuse "OpenCode answered $api_status to a turn of $1: $(head -c 300 "$answer")"
}

opencode_new_session() {
  opencode_api POST /session '{"title":"envoy session states"}' || return 1
  jq -er .id "$answer"
}

# ---- 1. a stalled session reads unresponsive, and comes back ----------------------------------------------
# The whole of the change in one check: a session nothing heartbeats is not gone. Its own record —
# which is all any reader judges — carries the last registration it made, and both listeners say
# so, before and after one of them is restarted, since a listener that has just read the bucket
# must not disagree with one that has watched it all along.
check_stalled_session_reads_unresponsive() {
  local before stalled
  omp_start || return 1
  omp_session=$(omp_session_id) || refuse "Oh My Pi named no session id" || return 1
  wait_until 180 "the Oh My Pi session to register live" state_is "$port_a" "$omp_session" '' live || return 1
  before=$(last_seen "$port_a" "$omp_session") ||
    refuse "no last_seen for $omp_session while it is live" || return 1
  kill -STOP "$omp_pid" || refuse "could not stop the Oh My Pi process" || return 1
  note "SIGSTOPped Oh My Pi (pid $omp_pid); its last registration was $before"
  wait_until 180 "the stalled session to read unresponsive" \
    state_is "$port_a" "$omp_session" unresponsive unresponsive || return 1
  stalled=$(last_seen "$port_a" "$omp_session")
  [ "$stalled" -ge "$before" ] ||
    refuse "the stalled session's last_seen moved backwards: $stalled, was $before" || return 1
  [ "$(date +%s%3N)" -gt "$((stalled + 30000))" ] ||
    refuse "it read unresponsive while its last registration $stalled was still inside the ${live_window} live window" || return 1
  # The default list and the default per-session read leave it out; include=unresponsive answers.
  ! listed "$port_a" "$omp_session" ||
    refuse "GET /v1/sessions lists the stalled session without include=unresponsive" || return 1
  ! api "$port_a" GET "/v1/registry/$omp_session" ||
    refuse "GET /v1/registry/<id> without include answered 200 for a stalled session" || return 1
  [ "$api_status" = 404 ] ||
    refuse "GET /v1/registry/<id> without include answered $api_status, not the 404 that names no live session" || return 1
  jq -e --arg s "$omp_session" '.error | startswith("no live session " + $s)' "$answer" >/dev/null ||
    refuse "the 404 body no longer begins 'no live session <id>': $(head -c 200 "$answer")" || return 1
  jq -e --arg l "$stalled" '.state == "unresponsive" and (.last_seen | tostring) == $l' "$answer" >/dev/null ||
    refuse "the 404 carries no unresponsive state beside last_seen $stalled: $(head -c 200 "$answer")" || return 1
  listed "$port_a" "$omp_session" unresponsive ||
    refuse "GET /v1/sessions?include=unresponsive does not list the stalled session" || return 1
  jq -e --arg s "$omp_session" --arg l "$stalled" \
    'map(select(.session_id == $s))[0] | .state == "unresponsive" and (.last_seen | tostring) == $l' "$answer" >/dev/null ||
    refuse "the listed row carries no unresponsive state beside last_seen $stalled" || return 1
  { session_state "$port_a" "$omp_session" unresponsive && [ "$state" = unresponsive ]; } ||
    refuse "GET /v1/registry/<id>?include=unresponsive did not answer state unresponsive" || return 1
  # Two listeners at once agree, and so does one that has only just read the bucket.
  state_is "$port_b" "$omp_session" unresponsive unresponsive ||
    refuse "listener B reads the stalled session '$state', not unresponsive" || return 1
  [ "$(last_seen "$port_b" "$omp_session")" = "$stalled" ] ||
    refuse "listener B names another last_seen than listener A for the stalled session" || return 1
  stop_pid "$listener_b_pid"
  start_listener listener_b listener_b_keeper
  wait_until 60 "listener B to serve again" serving "$port_b" || return 1
  wait_until 60 "the restarted listener to read the stalled session unresponsive" \
    state_is "$port_b" "$omp_session" unresponsive unresponsive || return 1
  [ "$(last_seen "$port_b" "$omp_session")" = "$stalled" ] ||
    refuse "the restarted listener names another last_seen for the stalled session" || return 1
  note "stalled since $stalled: unresponsive on both listeners, and across listener B's restart"
  kill -CONT "$omp_pid" || refuse "could not continue the Oh My Pi process" || return 1
  wait_until 180 "the continued session to read live again" state_is "$port_a" "$omp_session" '' live || return 1
  wait_until 60 "listener B to read it live again" state_is "$port_b" "$omp_session" '' live || return 1
  [ "$(last_seen "$port_a" "$omp_session")" -gt "$stalled" ] ||
    refuse "the continued session's last_seen did not move past $stalled" || return 1
  note "SIGCONT returned it to live on both listeners"
}

# ---- 2. a bus outage takes nothing from a stalled session -------------------------------------------------
# The failure this issue is about is a box under load, where the listener's copy of the registry and
# the bus itself both suffer. With the bus down for minutes around two stalled sessions, neither may
# lose its role claim or its issue claim: a claim is released only for a holder past the gone horizon.
check_bus_outage_keeps_claims() {
  local issue first second role=ess-outage-$$ outage=180 claimed_at record
  first=ses_ess_outage_first_$$
  second=ses_ess_outage_second_$$
  registered "$first" 0 true "notifications.agent.$first" || return 1
  registered "$second" 0 true "notifications.agent.$second" || return 1
  api "$port_a" POST /v1/roles/set "$(jq -cn --arg s "$first" --arg r "$role" '{session_id:$s,role:$r}')" ||
    refuse "the role claim was refused: $(head -c 200 "$answer")" || return 1
  issue=$(make_issue "$second" "Bus outage keeps the claims") ||
    refuse "could not create the proof's issue in Dispatch" || return 1
  dispatch_cli "$second" claim --issue "$issue" >"$evidence/answers/$check.claim.txt" 2>&1 ||
    refuse "the issue claim was refused: $(head -c 300 "$evidence/answers/$check.claim.txt")" || return 1
  claimed_at=$(role_record "$role" | jq -r '.claimed_at')
  wait_until 180 "both sessions to read unresponsive" state_is "$port_a" "$first" unresponsive unresponsive || return 1
  state_is "$port_a" "$second" unresponsive unresponsive ||
    refuse "the second session did not reach unresponsive before the outage" || return 1
  stop_pid "$natsd_pid"
  natsd_pid=
  note "stopped NATS for ${outage}s with both sessions stalled and both claims held"
  sleep "$outage"
  start_process natsd "$work/bin/natsd" -p "$nats_port" -store_dir "$work/nats-store"
  wait_until 120 "natsd to be ready again" \
    log_count_at_least "$evidence/logs/natsd.log" 'Server is ready' 2 || return 1
  wait_until 180 "listener A to serve again after the outage" serving "$port_a" || return 1
  # Still unresponsive, not gone: the records stood through the outage, and so did both claims.
  wait_until 180 "the first session to read unresponsive after the outage" \
    state_is "$port_a" "$first" unresponsive unresponsive || return 1
  state_is "$port_a" "$second" unresponsive unresponsive ||
    refuse "the second session reads '$state' after the outage, not unresponsive" || return 1
  ! api "$port_a" GET "/v1/roles/$role" ||
    refuse "the role lookup after the outage answered 200, not the lapsed-holder 404" || return 1
  [ "$api_status" = 404 ] ||
    refuse "the role lookup after the outage answered $api_status: $(head -c 200 "$answer")" || return 1
  jq -e --arg s "$first" '.holder == $s and .holder_state == "unresponsive" and .claim_released == false' "$answer" >/dev/null ||
    refuse "the role's claim did not survive the outage with its holder: $(head -c 300 "$answer")" || return 1
  record=$(role_record "$role")
  [ "$(jq -r '.holder_session_id' <<<"$record")" = "$first" ] ||
    refuse "envoy_roles names $(jq -r '.holder_session_id' <<<"$record") after the outage, not $first" || return 1
  [ "$(jq -r '.claimed_at' <<<"$record")" = "$claimed_at" ] ||
    refuse "the role's claimed_at moved across the outage: $(jq -r '.claimed_at' <<<"$record"), was $claimed_at" || return 1
  dispatch_api GET "/api/v1/issues/$issue" ||
    refuse "Dispatch would not read $issue after the outage: $api_status" || return 1
  jq -e --arg s "$second" '.claim.actor.id == $s' "$answer" >/dev/null ||
    refuse "the issue claim did not survive the outage with its holder: $(jq -c '.claim' "$answer")" || return 1
  jq -e '.holder_state == "unresponsive"' "$answer" >/dev/null ||
    refuse "the issue read names holder_state $(jq -r '.holder_state // "none"' "$answer"), not unresponsive" || return 1
  note "after a ${outage}s bus outage both sessions are unresponsive and keep the role claim and the issue claim"
}

# ---- 3a. a send to a stalled session is stored, not refused ------------------------------------------------
check_send_to_stalled_session_is_stored() {
  local target=ses_ess_send_target_$$
  registered "$target" 0 true "notifications.agent.$target" || return 1
  wait_until 180 "the target to read unresponsive" state_is "$port_a" "$target" unresponsive unresponsive || return 1
  api "$port_a" POST /v1/messages/send \
    "$(jq -cn --arg t "$target" '{source:"agent",source_session:"ess-sender",target_session:$t,message:"A stored message for a stalled session."}')" ||
    refuse "the send to an unresponsive self-subscribed session answered $api_status, not 200: $(head -c 300 "$answer")" || return 1
  jq -e --arg t "$target" '.recipient == $t and .recipient_state == "unresponsive" and (.last_seen | numbers)' "$answer" >/dev/null ||
    refuse "the accepted send does not name the recipient's unresponsive state and its last_seen: $(head -c 300 "$answer")" || return 1
  note "the send was accepted with recipient_state unresponsive and last_seen $(jq -r .last_seen "$answer")"
}

# ---- 3b. every stored delivery reaches the session once ------------------------------------------------------
# Writes on a stalled session's routes — a mention and a direct message on its own route, a publish
# on a role route it holds — are stored while it is stalled and arrive when it answers again. Each
# must show exactly once in the session's own transcript, no delivery may be reported failed,
# deferred or given up, and nothing may land on the exceptions topic: a stored delivery is not a
# failed one.
check_stored_deliveries_arrive_once() {
  local role=ess-stored-$$ issue marker transcript mention direct published
  [ -n "$omp_pid" ] || omp_start || return 1
  [ -n "$omp_session" ] || omp_session=$(omp_session_id)
  wait_until 180 "the Oh My Pi session to be live" state_is "$port_a" "$omp_session" '' live || return 1
  transcript=$(omp_session_file) || refuse "Oh My Pi named no session file" || return 1
  api "$port_a" POST /v1/roles/set "$(jq -cn --arg s "$omp_session" --arg r "$role" '{session_id:$s,role:$r}')" ||
    refuse "the session could not take the role $role: $(head -c 200 "$answer")" || return 1
  issue=$(make_issue "$omp_session" "Stored deliveries arrive once") ||
    refuse "could not create the proof's issue" || return 1
  dispatch_cli "$omp_session" claim --issue "$issue" >"$evidence/answers/$check.claim.txt" 2>&1 ||
    refuse "the session could not claim $issue" || return 1
  marker=ess-stored-$RANDOM$RANDOM
  kill -STOP "$omp_pid" || refuse "could not stop Oh My Pi" || return 1
  wait_until 180 "the session to read unresponsive" state_is "$port_a" "$omp_session" unresponsive unresponsive || return 1
  # Dispatch routes a mention from the comment's `mentions` field, each target a route
  # (`session:<id>` or `role:<name>`, model.ParseRoute), never from an `@…` written in the body:
  # a body naming the session and no mentions is a comment on the issue's topic alone, which this
  # session is not subscribed to, so nothing is sent to it and nothing is stored for it.
  dispatch_api POST "/api/v1/issues/$issue/comments" \
    "$(jq -cn --arg s "$omp_session" --arg m "$marker" \
      '{body:("mention " + $m),mentions:[{target:("session:" + $s)}],delivery:"steer",
        actor:{id:"ess-peer",kind:"session"}}')" ||
    refuse "the mention was refused $api_status: $(head -c 300 "$answer")" || return 1
  api "$port_a" POST /v1/messages/send \
    "$(jq -cn --arg t "$omp_session" --arg m "$marker" \
      '{source:"agent",source_session:"ess-peer",target_session:$t,message:("direct " + $m)}')" ||
    refuse "the direct message to the stalled session was refused $api_status: $(head -c 300 "$answer")" || return 1
  ! api "$port_a" POST /v1/messages/publish \
    "$(jq -cn --arg r "$role" --arg m "$marker" '{topic:("notifications.role." + $r),message:("role " + $m)}')" ||
    refuse "the publish onto the role route of an unresponsive holder was accepted" || return 1
  [ "$api_status" = 404 ] ||
    refuse "the role publish answered $api_status: $(head -c 300 "$answer")" || return 1
  jq -e '.holder_state == "unresponsive" and .claim_released == false' "$answer" >/dev/null ||
    refuse "the role publish's refusal does not name the holder's unresponsive state and its kept claim" || return 1
  kill -CONT "$omp_pid" || refuse "could not continue Oh My Pi" || return 1
  wait_until 180 "the session to be live again" state_is "$port_a" "$omp_session" '' live || return 1
  wait_until 600 "the stored deliveries to reach the session's transcript" \
    stored_cards_landed "$transcript" "$marker" || return 1
  # A second copy of a stored delivery would land behind the first, so the count is taken a
  # settling period after both arrived, and the transcript it was taken from is kept as evidence.
  sleep 30
  cp -f "$transcript" "$evidence/answers/$check.transcript.jsonl"
  mention=$(delivery_cards "$transcript" "mention $marker")
  direct=$(delivery_cards "$transcript" "direct $marker")
  published=$(delivery_cards "$transcript" "role $marker")
  { [ "$mention" -eq 1 ] && [ "$direct" -eq 1 ]; } ||
    refuse "the session's transcript holds $mention delivery cards for the mention naming $marker and $direct for the direct message; each stored delivery arrives exactly once" || return 1
  [ "$published" -eq 0 ] ||
    refuse "the transcript holds $published delivery cards for the role publish naming $marker, which the listener refused" || return 1
  ! grep -qE 'agent delivery failed|deferred|given up' "$evidence/logs/listener_a.log" ||
    refuse "the listener's log names a failed, deferred or given-up delivery for a stored message" || return 1
  [ ! -s "$work/exceptions.jsonl" ] ||
    refuse "an exception landed on notifications.envoy.exceptions.*: $(head -c 200 "$work/exceptions.jsonl")" || return 1
  note "the mention and the direct message each arrived once after SIGCONT; the role publish was refused with the claim kept"
}

# ---- 3c. a send to a stalled session that receives on a port is refused ---------------------------------------
# A session with a serving port is reached by a push to that port, and a stalled one answers no
# push. That send is refused by name, with the state it was refused on, so the sender routes around
# the pane instead of believing its message was taken.
check_send_to_serving_port_is_refused() {
  [ -n "$opencode_pid" ] || opencode_start
  [ -n "$opencode_session" ] || opencode_session=$(opencode_new_session) ||
    refuse "OpenCode would not open a session" || return 1
  opencode_turn "$opencode_session" "Reply with the single word READY and nothing else." || return 1
  wait_until 240 "the OpenCode session to register live" state_is "$port_a" "$opencode_session" '' live || return 1
  api "$port_a" GET "/v1/registry/$opencode_session" || true
  jq -e --argjson p "$opencode_port" '.port == $p' "$answer" >/dev/null ||
    refuse "the OpenCode session registered port $(jq -r .port "$answer"), not its serve's $opencode_port" || return 1
  # The serve is reached through opencode-ai's bin/opencode, a Node launcher that runs the
  # platform binary as a child of its own and forwards the signals it can catch; SIGSTOP is not
  # one, so stopping the launcher's pid alone leaves the serve heartbeating. The stall goes to
  # the whole process group, which own_session gave this launcher as its session leader.
  kill -STOP -- -"$opencode_pid" || refuse "could not stop the OpenCode serve" || return 1
  wait_until 180 "the OpenCode session to read unresponsive" \
    state_is "$port_a" "$opencode_session" unresponsive unresponsive || return 1
  ! api "$port_a" POST /v1/messages/send \
    "$(jq -cn --arg t "$opencode_session" '{source:"agent",source_session:"ess-sender",target_session:$t,message:"A push to a stalled serving port."}')" ||
    refuse "the send to the stalled OpenCode session was accepted; a push to a port nothing answers must be refused" || return 1
  [ "$api_status" = 404 ] ||
    refuse "the send to the stalled OpenCode session answered $api_status, not the 404 that names its state" || return 1
  jq -e --arg s "$opencode_session" \
    '(.error | startswith("no live session " + $s)) and .state == "unresponsive" and (.last_seen | numbers)' "$answer" >/dev/null ||
    refuse "the refusal does not name the session's unresponsive state and last_seen: $(head -c 300 "$answer")" || return 1
  kill -CONT -- -"$opencode_pid" || true
  note "a send to the stalled OpenCode session on port $opencode_port was refused 404 naming unresponsive"
}

# ---- 4. a role claim outlives every lookup --------------------------------------------------------------
# What used to release a claim: a lookup, a publish through the listener, a message straight onto
# the role's topic, and a reaper pass. None of them may now, while the holder is only unresponsive,
# and each answer must say so rather than reading as "the role is free".
check_role_claim_survives_lookups() {
  local role=ess-lookup-$$ holder=ses_ess_lookup_holder_$$ heir=ses_ess_lookup_heir_$$
  local second=ses_ess_lookup_second_$$ i before after
  registered "$holder" 0 true "notifications.agent.$holder" "notifications.role.$role" || return 1
  api "$port_a" POST /v1/roles/set "$(jq -cn --arg s "$holder" --arg r "$role" '{session_id:$s,role:$r}')" ||
    refuse "the role claim was refused: $(head -c 200 "$answer")" || return 1
  before=$(role_record "$role")
  [ -n "$before" ] || refuse "envoy_roles holds no claim for $role after it was set" || return 1
  wait_until 180 "the holder to read unresponsive" state_is "$port_a" "$holder" unresponsive unresponsive || return 1
  for i in 1 2 3 4 5; do
    ! api "$port_a" GET "/v1/roles/$role" ||
      refuse "lookup $i answered 200 for a holder that has stopped answering" || return 1
    [ "$api_status" = 404 ] ||
      refuse "lookup $i answered $api_status: $(head -c 200 "$answer")" || return 1
    jq -e --arg h "$holder" \
      '.reason == "holder_lapsed" and .holder == $h and .holder_state == "unresponsive" and (.last_seen | numbers) and .claim_released == false' \
      "$answer" >/dev/null ||
      refuse "lookup $i does not name the holder, its unresponsive state, its last_seen and claim_released false: $(head -c 300 "$answer")" || return 1
  done
  ! api "$port_a" POST /v1/messages/publish \
    "$(jq -cn --arg r "$role" '{topic:("notifications.role." + $r),message:"a publish onto a lapsed holder'\''s role"}')" ||
    refuse "the publish onto a lapsed holder's role was accepted" || return 1
  [ "$api_status" = 404 ] ||
    refuse "the publish answered $api_status: $(head -c 200 "$answer")" || return 1
  jq -e '.claim_released == false' "$answer" >/dev/null ||
    refuse "the publish's refusal does not say the claim was kept" || return 1
  # The same topic from a bare bus client, which never asks the listener at all.
  nats_publish "notifications.role.$role" "a direct publish onto a lapsed holder's role" \
    >"$evidence/logs/nats-publish.log" 2>&1 ||
    refuse "the direct nats publish on notifications.role.$role failed; see $evidence/logs/nats-publish.log" || return 1
  # A reaper pass, which is the one thing that may ever release a claim, and only for a gone holder.
  sleep "$((2 * ${reap_interval%s}))"
  after=$(role_record "$role")
  [ "$(jq -r '.holder_session_id' <<<"$after")" = "$holder" ] ||
    refuse "envoy_roles names $(jq -r '.holder_session_id' <<<"$after") after a reaper pass, not $holder" || return 1
  [ "$(jq -r '.claimed_at' <<<"$after")" = "$(jq -r '.claimed_at' <<<"$before")" ] ||
    refuse "the claim's claimed_at moved: $after, was $before" || return 1
  note "five lookups, a publish, a direct nats publish and a reaper pass left $role with $holder, claimed_at unchanged"
  # A soft claim is the one move that takes the role: a successor saying "this is me again", with
  # and without naming the predecessor it continues.
  registered "$heir" 0 true "notifications.agent.$heir" || return 1
  api "$port_a" POST /v1/roles/set \
    "$(jq -cn --arg s "$heir" --arg r "$role" --arg p "$holder" '{session_id:$s,role:$r,soft:true,previous_session_id:$p}')" ||
    refuse "a soft claim naming the stalled predecessor was refused $api_status: $(head -c 300 "$answer")" || return 1
  [ "$(jq -r '.holder_session_id' <<<"$(role_record "$role")")" = "$heir" ] ||
    refuse "the soft claim naming a predecessor did not take the role" || return 1
  wait_until 180 "the heir to read unresponsive" state_is "$port_a" "$heir" unresponsive unresponsive || return 1
  registered "$second" 0 true "notifications.agent.$second" || return 1
  api "$port_a" POST /v1/roles/set \
    "$(jq -cn --arg s "$second" --arg r "$role" '{session_id:$s,role:$r,soft:true}')" ||
    refuse "a soft claim with no named predecessor was refused for a holder past the live window: $(head -c 300 "$answer")" || return 1
  [ "$(jq -r '.holder_session_id' <<<"$(role_record "$role")")" = "$second" ] ||
    refuse "the soft claim with no named predecessor did not take the role" || return 1
  note "a soft claim took the role, with and without a named predecessor"
}

# ---- 5. an issue claim refuses the take while its holder is only unresponsive ------------------------------
check_issue_claim_refuses_the_stalled_holder() {
  local issue holder=ses_ess_issue_holder_$$ other=ses_ess_issue_other_$$ out=$evidence/answers/$check
  registered "$holder" 0 true "notifications.agent.$holder" || return 1
  issue=$(make_issue "$holder" "An issue claim a stalled holder keeps") ||
    refuse "could not create the proof's issue" || return 1
  dispatch_cli "$holder" claim --issue "$issue" >"$out.claim.txt" 2>&1 ||
    refuse "the holder could not claim $issue: $(head -c 300 "$out.claim.txt")" || return 1
  wait_until 180 "the holder to read unresponsive" state_is "$port_a" "$holder" unresponsive unresponsive || return 1
  # Another agent's take, and its release of someone else's claim, are both refused, naming the
  # state and the moment the holder was last seen.
  ! dispatch_cli "$other" claim --issue "$issue" >"$out.take.txt" 2>&1 ||
    refuse "another session took the claim of an unresponsive holder: $(head -c 300 "$out.take.txt")" || return 1
  grep -q 'unresponsive since' "$out.take.txt" ||
    refuse "the refused take does not say since when the holder is unresponsive: $(head -c 300 "$out.take.txt")" || return 1
  grep -q 'not gone' "$out.take.txt" ||
    refuse "the refused take does not say the holder is not gone: $(head -c 300 "$out.take.txt")" || return 1
  ! dispatch_cli "$other" claim --issue "$issue" --release >"$out.release.txt" 2>&1 ||
    refuse "another session released the claim of an unresponsive holder: $(head -c 300 "$out.release.txt")" || return 1
  grep -q 'unresponsive since' "$out.release.txt" ||
    refuse "the refused release does not name the holder's state and since when: $(head -c 300 "$out.release.txt")" || return 1
  dispatch_cli "$other" read --issue "$issue" >"$out.read.txt" 2>&1 ||
    refuse "dispatch read --issue failed: $(head -c 300 "$out.read.txt")" || return 1
  grep -q 'unresponsive since' "$out.read.txt" ||
    refuse "dispatch read --issue does not print 'unresponsive since <t>': $(grep -i claim "$out.read.txt" | head -c 300)" || return 1
  grep -q 'takeable at' "$out.read.txt" ||
    refuse "dispatch read --issue names no takeable time beside the claim: $(grep -i claim "$out.read.txt" | head -c 300)" || return 1
  note "$(grep -m1 'unresponsive since' "$out.read.txt")"
}

# ---- 6. gone only past the horizon, and at once for a clean stop ---------------------------------------------
# The boundary the whole change turns on, measured rather than asserted: a SIGKILLed session stays
# unresponsive until the gone horizon and is gone past it, and only then does the role reaper take
# its claim. The horizon here is a listener of the run's own at 90s — the same boundary, which ten
# minutes of waiting would measure no better.
check_gone_only_past_the_horizon() {
  local role=ess-kill-$$ victim victim_pid victim_fd killed_at gone_at flight before_new topic
  write_omp_profile
  omp_launch victim victim_pid victim_fd
  wait_until 240 "the victim session to print ready" grep -q '"type":"ready"' "$work/victim.out" || return 1
  printf '%s\n' '{"type":"get_state","id":"victim"}' >&"$victim_fd"
  wait_until 60 "the victim session to answer get_state" rpc_answered "$work/victim.out" victim || return 1
  victim=$(rpc_answer "$work/victim.out" victim | jq -r '.data.sessionId')
  [ -n "$victim" ] || refuse "the victim session named no id" || return 1
  wait_until 180 "the victim session to register live" state_is "$port_a" "$victim" '' live || return 1
  api "$port_a" POST /v1/roles/set "$(jq -cn --arg s "$victim" --arg r "$role" '{session_id:$s,role:$r}')" ||
    refuse "the victim could not take the role $role" || return 1
  # The listener that judges this boundary: a 90s gone horizon and a reaper pass every 10s.
  rm -f "$work/shortlistener.port"
  start_held shortlistener short_port ENVOY_LISTEN_FD -- env -i PATH="$PATH" HOME=/nonexistent \
    ENVOY_LISTEN_FD=3 ENVOY_API_TOKEN="$(cat "$work/envoy-token")" \
    ENVOY_MACHINE_ID="envoy-session-states-short-$$" ENVOY_LISTEN_HOST=127.0.0.1 \
    NATS_URLS="$nats_url" ENVOY_SESSION_LIVE_WINDOW=30s ENVOY_SESSION_GONE_HORIZON=90s \
    ENVOY_SESSION_REAP_INTERVAL=10s "$work/bin/envoy-listener"
  await_start shortlistener "$shortlistener_pid" 60 "the short-horizon listener to serve" serving "$short_port"
  exec {victim_fd}>&-
  kill -KILL "$victim_pid" || refuse "could not kill the victim session" || return 1
  killed_at=$(date +%s)
  wait_until 120 "the killed session to read unresponsive" \
    state_is "$short_port" "$victim" unresponsive unresponsive || return 1
  role_reason_is "$short_port" "$role" holder_lapsed ||
    refuse "the role lookup for the killed-but-unresponsive holder answered $(head -c 200 "$answer")" || return 1
  jq -e --arg v "$victim" '.holder == $v and .claim_released == false' "$answer" >/dev/null ||
    refuse "the killed session lost its role claim while it was only unresponsive" || return 1
  wait_until 300 "the killed session to reach gone past its 90s horizon" \
    state_is "$short_port" "$victim" unresponsive gone || return 1
  gone_at=$(date +%s)
  measured_kill_latency=$((gone_at - killed_at))
  note "SIGKILL-to-gone latency: ${measured_kill_latency}s (gone horizon 90s, a reaper pass every 10s)"
  [ "$measured_kill_latency" -ge 90 ] ||
    refuse "the killed session read gone ${measured_kill_latency}s after the kill, inside its own 90s horizon" || return 1
  # The role claim reaper has a period of its own, a fixed five minutes that
  # ENVOY_SESSION_REAP_INTERVAL does not move (StartRoleClaimReaper, cmd/listener/main.go), so the
  # release waits longer than one of its passes, not the ten seconds the session reaper runs at.
  wait_until 420 "the role reaper to release the gone holder's claim" \
    role_reason_is "$short_port" "$role" unclaimed || return 1
  note "the role claim was released only once its holder was gone"
  stop_pid "$shortlistener_pid"
  shortlistener_pid=
  # A clean stop is gone at once, registration still settling and all: the plugin waits for its own
  # registration to land before it deregisters, so no DELETE can be overtaken and undone.
  [ -n "$opencode_pid" ] || opencode_start
  flight=$(opencode_new_session) || refuse "OpenCode would not open a session" || return 1
  opencode_turn "$flight" "Reply with the single word READY and nothing else." || return 1
  wait_until 240 "the in-flight session to register" state_is "$port_a" "$flight" '' live || return 1
  kill -TERM "$opencode_pid" || refuse "could not stop the OpenCode serve" || return 1
  wait_until 60 "the cleanly stopped session to be gone at once" \
    state_is "$port_a" "$flight" unresponsive gone || return 1
  opencode_pid=
  note "a clean SIGTERM left the OpenCode session gone at once, its registration still settling"
  # /clear mints a new session id; the one it leaves must be gone at once, and a resume inside the
  # 15 minutes the registry remembers a last heartbeat for gets its topics back.
  topic=notifications.team.ess-$$.all
  claude_session=$(python3 -c 'import uuid; print(uuid.uuid4())')
  claude_start "$claude_session"
  claude_turn "Call the envoy_subscribe tool for the topic $topic, then reply with the word SUBSCRIBED." || return 1
  wait_until 240 "the Claude Code session to register" state_is "$port_a" "$claude_session" '' live || return 1
  topics_hold "$port_a" "$claude_session" "$topic" ||
    refuse "the Claude Code session holds no subscribed topic to lose and get back: $(jq -c '.topics' "$answer")" || return 1
  claude_turn "/clear" || return 1
  wait_until 120 "the cleared session id to be gone" state_is "$port_a" "$claude_session" unresponsive gone || return 1
  claude_stop
  claude_start "$claude_session" --resume
  claude_turn "Reply with the single word RESUMED." || return 1
  wait_until 240 "the resumed session to get its topics back" topics_hold "$port_a" "$claude_session" "$topic" || return 1
  note "/clear left the old id gone at once, and claude --resume brought its topics back"
  # Oh My Pi's own /new does the same to the id it leaves.
  [ -n "$omp_pid" ] || omp_start || return 1
  before_new=$(omp_session_id)
  omp_typed "/new" >/dev/null || refuse "/new was refused" || return 1
  wait_until 120 "the session /new left to be gone" state_is "$port_a" "$before_new" unresponsive gone || return 1
  omp_session=$(omp_session_id)
  note "/new left $before_new gone at once and registered $omp_session"
}

# ---- 7. every plugin shows the state ---------------------------------------------------------------------
# The three hosts each show their model the same two facts about a peer: which state it is in, and
# when it was last seen. A host that showed membership of a list instead would show nothing at all
# when the list dropped a live session, which is the failure this issue is about.
check_every_plugin_shows_the_state() {
  local peer=ses_ess_peer_$$ transcript deleted ask
  # The tool takes no `include`: it lists the unresponsive sessions by name on every call, which
  # is the behaviour this check reads. An ask that named one would have every host refuse the call
  # on its arguments (parseEnvoyToolArguments) before it reached the listener.
  ask="Call the envoy_sessions tool, then reply with the raw JSON row it gave for the session $peer and nothing else."
  registered "$peer" 0 true "notifications.agent.$peer" || return 1
  wait_until 180 "the peer to read unresponsive" state_is "$port_a" "$peer" unresponsive unresponsive || return 1
  [ -n "$omp_pid" ] || omp_start || return 1
  # omp_turn, not omp_typed: the transcript is read for what the tool answered, and the host takes
  # the text well before the turn that calls the tool has run.
  omp_turn "$ask" >"$evidence/answers/$check.omp.json" ||
    refuse "Oh My Pi would not answer the envoy_sessions turn" || return 1
  transcript=$(omp_session_file)
  cp -f "$transcript" "$evidence/answers/$check.transcript.jsonl"
  shows_state "$transcript" ||
    refuse "Oh My Pi's transcript shows no state and last_seen from envoy_sessions" || return 1
  grep -q "$peer" "$transcript" ||
    refuse "Oh My Pi's envoy_sessions answer does not name the unresponsive peer $peer" || return 1
  if [ -z "$claude_pid" ]; then
    claude_session=$(python3 -c 'import uuid; print(uuid.uuid4())')
    claude_start "$claude_session"
  fi
  claude_turn "$ask" || return 1
  shows_state "$work/claude.out" ||
    refuse "Claude Code's envoy_sessions answer shows no state and last_seen" || return 1
  grep -q "$peer" "$work/claude.out" ||
    refuse "Claude Code's envoy_sessions answer does not name the unresponsive peer $peer" || return 1
  [ -n "$opencode_pid" ] || opencode_start
  [ -n "$opencode_session" ] || opencode_session=$(opencode_new_session) ||
    refuse "OpenCode would not open a session" || return 1
  opencode_turn "$opencode_session" "$ask" || return 1
  shows_state "$answer" ||
    refuse "OpenCode's envoy_sessions answer shows no state and last_seen: $(head -c 300 "$answer")" || return 1
  grep -q "$peer" "$answer" ||
    refuse "OpenCode's envoy_sessions answer does not name the unresponsive peer $peer" || return 1
  note "all three hosts showed the peer's state and its last_seen"
  # Ending a session makes it gone at once, and for OpenCode on either way of ending one.
  deleted=$(opencode_new_session) || refuse "OpenCode would not open a second session" || return 1
  opencode_turn "$deleted" "Reply with the single word READY and nothing else." || return 1
  wait_until 240 "the second OpenCode session to register" state_is "$port_a" "$deleted" '' live || return 1
  opencode_api DELETE "/session/$deleted" ||
    refuse "OpenCode would not delete the session: $api_status $(head -c 200 "$answer")" || return 1
  wait_until 60 "the deleted session to be gone at once" state_is "$port_a" "$deleted" unresponsive gone || return 1
  note "session.deleted left $deleted gone at once"
  wait_until 240 "the first OpenCode session to be live" state_is "$port_a" "$opencode_session" '' live || return 1
  kill -TERM "$opencode_pid" || refuse "could not SIGTERM the OpenCode serve" || return 1
  wait_until 60 "the SIGTERMed serve's session to be gone at once" \
    state_is "$port_a" "$opencode_session" unresponsive gone || return 1
  opencode_pid=
  opencode_session=
  note "a SIGTERM to the serve left its session gone at once"
}

# ---- 8. a change to the bucket's maximum age reaches both listeners ---------------------------------------
# The rollout this issue's sibling carries (dispatch://INFRA-887). The raise is proved for a session
# stalled *after* it, never one stalled before: raising a bucket's maximum age restores no key
# JetStream has already removed. That restatement is this issue's architect's.
check_max_age_change_reaches_both_listeners() {
  local late=ses_ess_maxage_$$ stall=420
  set_max_age "$shipping_max_age"
  wait_until 120 "listener A to read the shipping $shipping_max_age maximum age" \
    healthz_max_age_is "$port_a" "$shipping_max_age_shown" || return 1
  wait_until 120 "listener B to read the shipping $shipping_max_age maximum age" \
    healthz_max_age_is "$port_b" "$shipping_max_age_shown" || return 1
  note "the bucket is at the shipping maximum age, which both listeners now report"
  set_max_age "$max_age"
  wait_until 120 "listener A to read the raised $max_age maximum age" \
    healthz_max_age_is "$port_a" "$max_age_shown" || return 1
  wait_until 120 "listener B to read the raised $max_age maximum age" \
    healthz_max_age_is "$port_b" "$max_age_shown" || return 1
  note "the raise to $max_age reached both running listeners' /healthz"
  registered "$late" 0 true "notifications.agent.$late" || return 1
  note "registered $late after the raise; stalling it ${stall}s, past the shipping maximum age"
  sleep "$stall"
  state_is "$port_a" "$late" unresponsive unresponsive ||
    refuse "a session stalled ${stall}s after the raise reads '$state', not unresponsive; at the shipping maximum age its key would be gone by now" || return 1
  state_is "$port_b" "$late" unresponsive unresponsive ||
    refuse "listener B reads the stalled session '$state', not unresponsive" || return 1
  set_max_age "$shipping_max_age"
  wait_until 120 "listener A to read the rolled-back $shipping_max_age maximum age" \
    healthz_max_age_is "$port_a" "$shipping_max_age_shown" || return 1
  wait_until 120 "listener B to read the rolled-back $shipping_max_age maximum age" \
    healthz_max_age_is "$port_b" "$shipping_max_age_shown" || return 1
  wait_until 300 "the stalled session to be gone under the rolled-back maximum age" \
    state_is "$port_a" "$late" unresponsive gone || return 1
  wait_until 120 "listener B to read it gone too" state_is "$port_b" "$late" unresponsive gone || return 1
  note "the rollback to $shipping_max_age reached both listeners, and the session stalled past it then reads gone"
}

# ---- the run -----------------------------------------------------------------------------------------
for name in "${selected[@]}"; do
  run_check "$name"
done
check=summary
ok=1
