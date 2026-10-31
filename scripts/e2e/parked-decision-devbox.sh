#!/usr/bin/env bash
# The devbox proof that a person's decision on a parked Legion tree reaches the controller
# (INFRA-214, A3 and A4). A scratch Dispatch, NATS, a real Envoy listener, the Go daemon and the
# operator-launched controller, all built from this checkout, run on this box with real agents.
# Trees 1, 2, 4 and 5 wait on their design gate and are parked: the operator's `legion status` parks
# trees 1 and 2 (the daemon's session), with the controller's hold mark beside tree 1's park only,
# the person parks tree 4, and another session parks tree 5. The person approves tree 1, and the
# controller re-admits it from the `parked-decision` notice, since it is Legion's own hold; the
# person questions tree 2, and it stays parked; the person approves trees 2, 4 and 5, and the
# controller comments but leaves them parked, since none is Legion's own hold. Tree 3 is parked with
# the hold mark and approved while no controller runs, and the next controller's start walk
# re-admits it. The rig is Stage 3's (stage3-devbox-workflow.sh), copied: nothing here reads
# production, and no tree reaches a pull request.
#
# Run it as `bash scripts/e2e/parked-decision-devbox.sh` from the operator's own Oh My Pi session;
# `--help` prints the inputs and the checkpoints. scripts/e2e/README.md, "parked-decision-devbox.sh",
# describes the run.
set -Eeuo pipefail

checkpoints="prerequisites rig boot controller-up trees-wait-on-a-person trees-parked approval-readmits question-stays-parked foreign-park-stays controller-down walk-on-start verdict cleanup-is-complete"

usage() {
  cat <<EOF
usage: bash scripts/e2e/parked-decision-devbox.sh [--help]

The devbox proof that a person's decision on a parked Legion tree reaches the controller, and
that the controller's next start walks the parked roots (INFRA-214, A3 and A4). Devbox only.

Inputs (environment):
  LEGION_E2E_MODEL_ROUTE         gateway or key-file (required)
  LEGION_E2E_MODEL_GATEWAY_URL   the model gateway's Anthropic endpoint (gateway; needs hawk-token)
  LEGION_E2E_ANTHROPIC_KEY_FILE  a 0600 file that holds an Anthropic key (key-file)
  PARKED_DECISION_EVIDENCE_DIR   the evidence directory (default: a fresh /tmp directory, kept)
  PARKED_DECISION_UNTIL          stop after this checkpoint passes (a development run, not the proof)
The two Legion GitHub App keys come through the daemon's private_key_command on agent-tier
secrets. The devbox gh must act as the proof human.

Checkpoints, in order:
EOF
  # The names are separate arguments: word splitting is the point.
  # shellcheck disable=SC2086
  printf '  %s\n' $checkpoints
  printf '\nThe last line is "parked decision devbox e2e: PASS" or "parked decision devbox e2e: FAIL (check <name>)".\n'
}
case "${1:-}" in
  -h | --help) usage; exit 0 ;;
  "") ;;
  *) usage >&2; exit 2 ;;
esac

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "/tmp/legion-e2e-pd.$$.XXXXXXXX")
evidence=${PARKED_DECISION_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e-pd-evidence.XXXXXXXX)}
# Refused before anything is written into the evidence directory or any trap is set
# (lib/model-gateway-unserved.sh --fresh).
if ! reason=$(bash "$root/scripts/e2e/lib/model-gateway-unserved.sh" --fresh "$evidence"); then
  echo "FAIL setup: $reason" >&2
  rmdir "$work"
  exit 1
fi
mkdir -p "$evidence/logs" "$evidence/transcripts"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
ok=
check=setup
TZ=UTC printf -v check_started '%(%FT%TZ)T' -1 # when the current check began (lib/model-gateway-unserved.sh)
project="PD$(( ($$ + $(date +%s)) % 100000000 ))"
project=${project:0:10}
ptoken=${project,,}
profile="legion-e2e-pd-$$-$(date +%s)"
state="$work/state"
omp_home="$work/omp-home"
profile_agent="$omp_home/.omp/profiles/$profile/agent"
repo="sjawhar/legion-smoke"
pg_container="legion-e2e-pd-pg-$$"
nats_container="legion-e2e-pd-nats-$$"
tmux_server="legion-e2e-pd-$$"
daemon_pid=
dispatch_pid=
listener_pid=
port_daemon=
port_listener=
port_dispatch=
port_worker_stream=
port_pg=
port_nats=
dispatch_base=
# Every proof-human write carries no actor: the X-Dispatch-User header alone names the person
# (lib/workflow.sh, dispatch_actor).
dispatch_actor=
proof_human_user=smoke
pr_number=
smoke_file=
timeout_hook=
tree1=
tree2=
tree3=
tree4=
tree5=
# The field of an Oh My Pi session header that holds the session id (controller-up finds it).
session_field=
until_checkpoint=

begin() { check=$1; TZ=UTC printf -v check_started '%(%FT%TZ)T' -1; printf '== %s\n' "$check"; }
note() { printf '   %s\n' "$*"; }
pass() { printf 'ok %s\n' "$check"; }
fail() { printf 'FAIL %s: %s\n' "$check" "$*" >&2; exit 1; }
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/omp-home.sh
. "$root/scripts/e2e/lib/omp-home.sh"
# shellcheck source-path=SCRIPTDIR source=lib/workflow.sh
. "$root/scripts/e2e/lib/workflow.sh"
# shellcheck source-path=SCRIPTDIR source=lib/leftovers.sh
. "$root/scripts/e2e/lib/leftovers.sh"

tm() { tmux -L "$tmux_server" "$@"; }

# collect_transcripts copies every Oh My Pi session of the run's profile into the evidence
# directory before the profile is removed.
collect_transcripts() {
  local sessions="$profile_agent/sessions"
  [ -d "$sessions" ] || return 0
  cp -a "$sessions/." "$evidence/transcripts/" 2>/dev/null || true
}

# remove_run_branches closes the open pull request on legion/<tree> and deletes that branch on the
# smoke repository, for each tree of the run (the daemon makes the branch at admission,
# daemon/outbox.go). It makes no gh call unless require_proof_human passed: a refused run's gh acts
# as someone else.
remove_run_branches() {
  local issue number
  [ -n "$proof_human" ] || return 0
  for issue in $tree1 $tree2 $tree3 $tree4 $tree5; do
    number=$(timeout 60 gh -R "$repo" pr list --head "legion/$issue" --state open --json number --jq '.[0].number // empty' 2>/dev/null) || number=
    if [ -n "$number" ]; then
      timeout 60 gh -R "$repo" pr close "$number" --comment "Closed by the parked-decision run that opened it, at its teardown." >/dev/null 2>&1 &&
        note "closed the run's open pull request $repo#$number (legion/$issue)"
    fi
    timeout 60 gh api -X DELETE "repos/$repo/git/refs/heads/legion/$issue" >/dev/null 2>&1 && note "deleted the run's branch legion/$issue from $repo"
  done
  return 0
}

cleanup() {
  local status=$? p
  set +e
  # Teardown is best effort: a command that fails here is a warning, never a check's FAIL line.
  trap 'printf "cleanup warning: line %s exited %s\n" "$LINENO" "$?" >&2' ERR
  # The key-file route's key command bakes in the key file's path: it never survives the run.
  rm -rf "$work/model-gateway-key" || true
  tm kill-server >/dev/null 2>&1 || true
  stop_pid "$daemon_pid"
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
  docker rm -f "$pg_container" "$nats_container" >/dev/null 2>&1 || true
  close_unpassed_run_pull_requests
  remove_run_branches >&2
  collect_transcripts
  rm -rf "$work/model-gateway-cache" || true
  if [ -n "${ok:-}" ]; then
    rm -rf "$work" || true
  else
    printf "the run's scratch workspace is %s\n" "$work" >&2
  fi
  printf "the run's evidence is %s\n" "$evidence" >&2
  [ -n "${ok:-}" ] || [[ $status =~ ^(129|130|143)$ ]] ||
    bash "$root/scripts/e2e/lib/model-gateway-unserved.sh" --notes "$evidence/model-gateway" "$check_started" "$check" >&2 || true
  [ -n "${ok:-}" ] || printf 'parked decision devbox e2e: FAIL (check %s)\n' "$check" >&2
  return 0
}
trap cleanup EXIT
trap 'printf "FAIL %s: line %s exited %s: %s\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# done_with CHECK ends a development run once CHECK has passed (PARKED_DECISION_UNTIL).
done_with() {
  [ "$until_checkpoint" = "$1" ] || return 0
  ok=1
  echo "parked decision devbox e2e: development run until $1 finished (not the proof)"
  exit 0
}

# ---- the rig, copied from stage3-devbox-workflow.sh ----------------------------------------------
start_listener() {
  local token attempt offset result
  token=$(cat "$work/envoy-token")
  for attempt in 1 2 3 4 5; do
    pick_port port_listener
    offset=$(log_size listener)
    ENVOY_API_TOKEN="$token" PORT="$port_listener" ENVOY_LISTEN_HOST=127.0.0.1 \
      ENVOY_MACHINE_ID="legion-e2e-pd-$$" NATS_URLS="nats://127.0.0.1:$port_nats" \
      start_process listener env -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
        -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/envoy-listener"
    result=0
    await_start listener "$listener_pid" "$offset" 60 "the Envoy listener to answer /v1/sessions" \
      curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$port_listener/v1/sessions" || result=$?
    [ "$result" != 0 ] || return 0
    note "the Envoy listener lost port $port_listener to another process (attempt $attempt); picking another"
  done
  fail "the Envoy listener lost its picked port five times"
}

start_dispatch() {
  local dispatch_token envoy_token pg_password attempt offset result
  dispatch_token=$(cat "$work/dispatch-token")
  envoy_token=$(cat "$work/envoy-token")
  pg_password=$(cat "$work/postgres-password")
  mkdir -p "$work/dispatch-home"
  chmod 0700 "$work/dispatch-home"
  for attempt in 1 2 3 4 5; do
    pick_port port_dispatch
    offset=$(log_size dispatch)
    # HOME and XDG_CONFIG_HOME are the scratch home, so the server reads no envoy.json of the
    # operator's: make_omp_home exports the operator's XDG_CONFIG_HOME.
    DATABASE_URL="postgres://legion:$pg_password@127.0.0.1:$port_pg/dispatch?sslmode=disable" \
      DISPATCH_AGENT_TOKEN="$dispatch_token" ENVOY_TOKEN="$envoy_token" HOME="$work/dispatch-home" \
      XDG_CONFIG_HOME="$work/dispatch-home/.config" \
      DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1 \
      DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$port_dispatch" \
      DISPATCH_SERVER_URL="http://127.0.0.1:$port_dispatch" NATS_URLS="nats://127.0.0.1:$port_nats" \
      ENVOY_URL="http://127.0.0.1:$port_listener" \
      start_process dispatch env -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
        -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/envoy-dispatch"
    result=0
    await_start dispatch "$dispatch_pid" "$offset" 60 "the scratch Dispatch server" \
      curl -fsS "http://127.0.0.1:$port_dispatch/api/v1" || result=$?
    dispatch_base="http://127.0.0.1:$port_dispatch"
    [ "$result" != 0 ] || return 0
    note "the scratch Dispatch lost port $port_dispatch to another process (attempt $attempt); picking another"
  done
  fail "the scratch Dispatch lost its picked port five times"
}

write_legion_config() {
  cat >"$work/legion.yaml" <<EOF
project: $project
port: $port_daemon
worker_stream_port: $port_worker_stream
postgres_dsn: postgres://legion:$(cat "$work/postgres-password")@127.0.0.1:$port_pg/legion?sslmode=disable
state_dir: $state
operator_token_file: $work/operator-token
omp_invocation: mise x $pin -- omp
envoy_url: http://127.0.0.1:$port_listener
envoy_token_file: $work/envoy-token
nats_urls:
  - nats://127.0.0.1:$port_nats
dispatch_url: http://127.0.0.1:$port_dispatch
dispatch_token_file: $work/dispatch-token
# The project's repository is Dispatch's mapping (the PUT in rig makes $repo its default).
projects:
  $project: {}
gates:
  design: root-issues
admission_cap: 2
review_round_cap: 3
# A day: the one tick is the boot's, before any tree exists, so no tick walk races the notice of
# approval-readmits, and the walk runs only at a controller's start (walk-on-start).
controller_wake_interval_seconds: 86400
instructions: $work/instructions.md
github_apps:
  implement:
    app_id: "3202636"
    private_key_command: "secrets LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 -- sh -c 'value=\$(printf %s \"\${LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64}\" | tr \"_-\" \"/+\"); case \$((\${#value} % 4)) in 1) value=\${value%?};; 2) value=\"\${value}==\";; 3) value=\"\${value}=\";; esac; printf %s \"\$value\" | base64 -d'"
  review:
    app_id: "3202653"
    private_key_command: "secrets GH_REVIEW_APP_PRIVATE_KEY_B64 -- sh -c 'value=\$(printf %s \"\${GH_REVIEW_APP_PRIVATE_KEY_B64}\" | tr \"_-\" \"/+\"); case \$((\${#value} % 4)) in 1) value=\${value%?};; 2) value=\"\${value}==\";; 3) value=\"\${value}=\";; esac; printf %s \"\$value\" | base64 -d'"
EOF
}

start_daemon() {
  local attempt offset result
  for attempt in 1 2 3 4 5; do
    pick_port port_daemon
    pick_port port_worker_stream
    write_legion_config
    offset=$(log_size daemon)
    HOME="$omp_home" OMP_PROFILE="$profile" PATH="$realbin:$PATH" env \
      -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
      -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 \
      "$work/legion" start --config "$work/legion.yaml" >>"$evidence/logs/daemon.log" 2>&1 &
    daemon_pid=$!
    result=0
    await_start daemon "$daemon_pid" "$offset" 180 "the Go daemon to answer /healthz" \
      curl -fsS "http://127.0.0.1:$port_daemon/healthz" || result=$?
    [ "$result" != 0 ] || return 0
    note "the Go daemon lost port $port_daemon or $port_worker_stream to another process (attempt $attempt); picking again"
  done
  fail "the Go daemon lost a picked port five times"
}

claim_session_file() {
  local issue=$1 role=$2
  "$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" |
    jq -er --arg issue "$issue" --arg role "$role" \
      '[.claims[] | select(.issue == $issue and .role == $role and .sessionFile != null and .sessionFile != "")] | last | .sessionFile'
}
claim_session_text() {
  local f
  f=$(claim_session_file "$1" "$2") || return 1
  cat -- "$f"
}
# ---- the controller ---------------------------------------------------------------------------------
# start_controller WINDOW runs `legion controller start` in WINDOW of the run's own tmux server, with
# the variables controller-start-tmux.sh unsets (the operator's Oh My Pi session, which runs this
# script, sets OMP_SESSION_ID) and the run's isolated profile, which holds this checkout's pi-envoy
# and pi-legion and the model route.
start_controller() {
  local window=$1 command
  command="cd '$work' && env -u OMP_SESSION_ID -u OMPCODE -u PI_CONFIG_FILES -u ENVOY_NATS_URL -u LEGION_OMP_PATH HOME='$omp_home' OMP_PROFILE='$profile' LEGION_GH_PATH='$real_gh' '$work/legion' controller start --config '$work/controller.yaml' 2>'$evidence/logs/$window.stderr'; echo \$? >'$evidence/logs/$window.exit'; sleep 3600"
  if tm has-session 2>/dev/null; then
    tm new-window -d -n "$window" "$command"
  else
    tm new-session -d -s pd -n "$window" -x 200 -y 50 "$command"
  fi
}
controller_token=legion-$ptoken-controller
# role_answer prints the listener's status for the lookup of the controller role, and leaves its body
# in $work/controller-role.json (stage4b-sandbox-tree.sh).
role_answer() { curl -sS --max-time 20 -o "$work/controller-role.json" -w '%{http_code}' -H "@$work/envoy-auth-header" "http://127.0.0.1:$port_listener/v1/roles/$controller_token" 2>/dev/null; }
role_unheld() { [ "$(role_answer)" = 404 ]; }
controller_runs() { pgrep -f -- "^$work/legion controller start --config $work/controller.yaml" >/dev/null; }
controller_gone() { ! controller_runs; }
# controller_quit WINDOW quits the controller as a person does (stage4b-sandbox-tree.sh): Ctrl-C ends
# a turn and asks again, and Ctrl-D on the empty editor quits.
controller_quit() {
  local window=$1
  if controller_runs; then
    for _ in 1 2 3 4 5; do
      controller_runs || break
      tm send-keys -t "pd:$window" C-c
      sleep 2
    done
    if controller_runs; then tm send-keys -t "pd:$window" C-d; fi
  fi
  until_true 60 "legion controller start in $window to exit" controller_gone
  until_true 300 "the listener to answer that the controller role $controller_token has no live holder" role_unheld
}
controller_session_id() { daemon_state | jq -er '.controllerLocator.sessionId // empty'; }
# session_header FILE prints FILE's Oh My Pi session header, its first line of type `session`, and
# fails while there is none: the pinned build writes a `title` line before the header.
session_header() { jq -cen 'first(inputs | select(type == "object" and .type == "session"))' "$1" 2>/dev/null; }
# controller_session_file SESSION prints the session file of the run's profile whose session header
# carries SESSION in the field controller-up found session 1's id in.
controller_session_file() {
  local file
  for file in "$profile_agent/sessions"/*/*.jsonl; do
    [ -f "$file" ] || continue
    if session_header "$file" | jq -e --arg field "$session_field" --arg s "$1" '.[$field] == $s' >/dev/null 2>&1; then
      printf '%s\n' "$file"
      return 0
    fi
  done
  return 1
}
start_message='Legion controller start: follow skill://legion-controller'
# started_itself FILE: the file's first user message is the controller's start message, and an
# assistant message follows (stage4b-sandbox-tree.sh's controller_started_itself, on one file).
started_itself() {
  grep -m1 '"role":"user"' "$1" | grep -qF "$start_message" && grep -q '"role":"assistant"' "$1"
}
# controller_delivered FILE NEEDLE: an Envoy delivery line of FILE holds NEEDLE.
controller_delivered() { [ "$(controller_deliveries "$1" "$2")" -ge 1 ]; }
controller_deliveries() { { grep -F '"customType":"envoy-message"' "$1" || true; } | grep -cF -- "$2" || true; }
# set_status_after FILE ISSUE NEEDLE: an assistant message of FILE calls the legion tool with
# {op: set_status, issue: ISSUE, status: todo}, after the first Envoy delivery holding NEEDLE when
# NEEDLE is set, and anywhere in the file when it is empty. Eval code that calls the tool hands it no
# arguments call_arguments can read (lib/omp-tool-calls.jq), so such a cell counts when its code
# names set_status, ISSUE and todo, each of the last two as a quoted string.
set_status_after() {
  jq -R -s -e -L "$root/scripts/e2e/lib" --arg issue "$2" --arg needle "$3" 'include "omp-tool-calls";
    [split("\n") | to_entries[] | {i: .key, raw: .value, m: (.value | fromjson? // null)}] as $lines
    | (if $needle == "" then -1 else
        ([$lines[] | select((.raw | contains("\"customType\":\"envoy-message\"")) and (.raw | contains($needle))) | .i] | first) end) as $after
    | $after != null and any($lines[]; .i > $after and .m.type? == "message" and .m.message.role? == "assistant"
        and any(.m.message.content[]?; calls("legion") and (
          (call_arguments | .op == "set_status" and .issue == $issue and .status == "todo")
          or (.name == "eval" and ((.arguments.code? // "") | tostring
            | contains("set_status") and test("[\"'"'"'`]" + $issue + "[\"'"'"'`]") and test("[\"'"'"'`]todo[\"'"'"'`]"))))))
  ' "$1" >/dev/null
}
# todo_by_daemon_after ISSUE SEQ: Dispatch shows ISSUE set `todo` by the daemon's session after the
# event SEQ.
todo_by_daemon_after() {
  dispatch_events "$1" | jq -e --argjson seq "$2" --arg daemon "legion-daemon:$project" '
    any(.[]; .seq > $seq and .type == "issue.updated" and .payload.status == "todo" and .actor.id == $daemon)' >/dev/null
}
last_seq() { dispatch_events "$1" | jq -er 'map(.seq) | max // 0'; }
# read_comment ISSUE SESSION prints the newest comment on ISSUE by SESSION that begins
# `Parked decision read:`, and fails while there is none; any_read_comment ISSUE prints the newest by
# any author.
read_comment() {
  dispatch_get "issues/$1/comments" | jq -ce --arg s "$2" \
    '[.[] | select(.author.id == $s and (.body | startswith("Parked decision read:")))] | last // empty'
}
any_read_comment() {
  dispatch_get "issues/$1/comments" | jq -c '[.[] | select(.body | startswith("Parked decision read:"))] | last // null'
}
# read_comment_after ISSUE SESSION OLD prints the newest such comment by SESSION while it is not OLD
# (the one read before), and fails while it is; delivered_more FILE NEEDLE N: FILE holds more than N
# deliveries of NEEDLE.
read_comment_after() {
  local got
  got=$(read_comment "$1" "$2") && [ "$got" != "$3" ] && printf '%s\n' "$got"
}
delivered_more() { [ "$(controller_deliveries "$1" "$2")" -gt "$3" ]; }
# log_has JQ: a line of the daemon log satisfies the jq condition JQ, each line parsed on its own.
log_has() {
  jq -R -s -e "any(split(\"\\n\")[] | fromjson? | select(type == \"object\"); $1)" "$evidence/logs/daemon.log" >/dev/null
}
undelivered_logged() {
  log_has ".issue == \"$1\" and (.msg | startswith(\"outbox notice finished undelivered: its architect\"))"
}
sent_logged() { log_has ".issue == \"$1\" and .msg == \"outbox parked decision sent to the controller\""; }
# architect_turn_holds ISSUE TEXT: a user turn of ISSUE's architect session holds TEXT.
architect_turn_holds() {
  { claim_session_text "$1" architect || true; } | jq -R -s -e --arg text "$2" \
    'any(split("\n")[] | fromjson? | select(.type? == "message" and .message.role? == "user"); .message.content | tostring | contains($text))' >/dev/null
}
waits_on_gate() {
  daemon_state | jq -e --arg k "$1" '.issues[$k].phase == "awaiting_person" and .issues[$k].waitingOn.kind == "gate"' >/dev/null
}
# parked ISSUE: the tree left the workflow (phase done, no slot) and its architect is suspended. A
# park lands while the architect's last turn may still run, its suspension held for that turn; a
# decision made then reaches that live session as its notice, which leaves the daemon nothing to
# relay, so a check approves a parked tree only once this holds.
parked() {
  daemon_state | jq -e --arg k "$1" '.issues[$k].phase == "done" and .issues[$k].slot == null and .issues[$k].architect.state == "suspended"' >/dev/null
}
active() { daemon_state | jq -e --arg k "$1" '.admission.active | index($k) != null' >/dev/null; }
park() { "$work/legion" status "$1" backlog --operator-token-file "$work/operator-token" --config "$work/legion.yaml" >/dev/null; }
# parked_by ISSUE prints `<kind>:<id>` of the newest status write to backlog on ISSUE.
parked_by() {
  dispatch_events "$1" | jq -er '[.[] | select(.type == "issue.updated" and .payload.status == "backlog")] | last | "\(.actor.kind):\(.actor.id)"'
}
# controller_mark ISSUE CAUSE posts the controller's hold mark on ISSUE as session 1, a controller
# session, over the agents' bearer: the run's stand-in for the comment the controller posts after
# its `set_status` when it parks a tree for a hold (the `held` row). has_mark ISSUE says whether
# ISSUE carries one.
controller_mark() {
  curl -sS --fail-with-body --max-time 20 -X POST -H "@$work/dispatch-auth-header" -H 'content-type: application/json' \
    --data "$(session_actor_body "$session1" "$(jq -cn --arg body "Parked by Legion while it waits on: $2." '{body: $body}')")" \
    "$(dispatch_url)/api/v1/issues/$1/comments" >/dev/null
}
has_mark() {
  dispatch_get "issues/$1/comments" | jq -e --arg s "$session1" 'any(.[]; .author.id == $s and (.body | startswith("Parked by Legion while it waits on:")))' >/dev/null
}
gate_artifact_of() { daemon_state | jq -er --arg k "$1" '.issues[$k].designGate.artifactId'; }
approve() { dispatch_human POST "artifacts/$1/reviews" '{"state":"approved"}' >/dev/null; }
# file_quiet FILE SECONDS: FILE has not grown for SECONDS.
file_quiet() { (($(date +%s) - $(stat -c %Y "$1") >= $2)); }
since() { printf '%ss' "$((SECONDS - $1))"; }
# wait_for_run_processes [DEADLINE_SECONDS] [MIN_POLLS] waits for every process of the run to end,
# polling run_processes every half second, and fails naming the pids that still run. It gives up
# only once both bounds are spent: DEADLINE_SECONDS of wall time (default 600) have passed and
# MIN_POLLS polls (default 10) have been made. The poll floor exists because one poll's cost belongs
# to the machine, not to the run: run_processes walks /proc with a readlink, and often a grep, per
# process. On a devbox at load ≈370 with ≈4,300 processes one walk took about 9 s, so the first walk
# outran the old 30 s budget, until_true made one attempt and failed the run, and the processes
# ended four minutes later. A wait that can make a single attempt is not a wait, and a longer
# deadline alone would only be a new wager on the machine.
wait_for_run_processes() {
  local limit=${1:-600} floor=${2:-10} polls=0 started=$SECONDS beat=$SECONDS left
  note "waiting up to ${limit}s and at least $floor polls for every process of the run to end"
  while :; do
    polls=$((polls + 1))
    # run_processes's status is that of the last thing its loop ran and means nothing.
    left=$(run_processes || true)
    if [ -z "$left" ]; then
      note "every process of the run ended: poll $polls, $(since "$started")"
      return 0
    fi
    if ((SECONDS - started >= limit && polls >= floor)); then
      fail "timed out after $(since "$started") ($polls polls) waiting for every process of the run to end; still running: ${left//$'\n'/ }"
    fi
    if ((SECONDS - beat >= 60)); then
      beat=$SECONDS
      note "still waiting for every process of the run to end: poll $polls, $((SECONDS - started))s of ${limit}s"
    fi
    sleep 0.5
  done
}

# ---- checkpoints --------------------------------------------------------------------------------------
begin prerequisites
for tool in go docker jq curl ss tmux bun mise secrets gh shellcheck jj pgrep; do command -v "$tool" >/dev/null || fail "$tool is required"; done
until_checkpoint=${PARKED_DECISION_UNTIL:-}
if [ -n "$until_checkpoint" ]; then
  case " $checkpoints " in
    *" $until_checkpoint "*) ;;
    *) fail "PARKED_DECISION_UNTIL must be one of: $checkpoints; not $until_checkpoint" ;;
  esac
fi
model_route=${LEGION_E2E_MODEL_ROUTE:-}
case "$model_route" in
  gateway) command -v hawk-token >/dev/null || fail "hawk-token is required under LEGION_E2E_MODEL_ROUTE=gateway" ;;
  key-file) key_file=${LEGION_E2E_ANTHROPIC_KEY_FILE:-}
    [ -n "$key_file" ] || fail "LEGION_E2E_ANTHROPIC_KEY_FILE is unset: name a 0600 file holding a plaintext Anthropic key" ;;
  *) fail "LEGION_E2E_MODEL_ROUTE must be gateway or key-file, not '$model_route'" ;;
esac
refuse_leftovers legion-e2e-pd
require_proof_human
gh repo view "$repo" --json name >/dev/null || fail "the devbox's ordinary gh cannot read $repo"
real_gh=$(mise which gh) || fail "mise has no gh"
# No process of the run inherits an operator's endpoint or bearer (as packages/dispatch/e2e/run-server.sh).
mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    DISPATCH_* | ENVOY_* | NATS_*) unset "$name" ;;
  esac
done
note "model route $model_route; the proof human is $proof_human; DISPATCH_*, ENVOY_* and NATS_* unset"
pass
done_with prerequisites

begin rig
realbin=$(real_bin_path "$work/realbin" gh jj tmux)
mkdir -p "$state" "$work/xdg" "$work/tmux"
chmod 0700 "$state" "$work/xdg" "$work/tmux"
make_omp_home "$omp_home"
case "$model_route" in
  gateway)
    key_command=$(bash "$root/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --home "$omp_home" --dest "$evidence/model-gateway" --cache-dir "$work/model-gateway-cache") ||
      fail "the agents' model route through the Hawk model gateway could not be installed (the reason is above)"
    ;;
  key-file)
    key_command=$(bash "$root/scripts/e2e/lib/install-key-file-model-route.sh" --profile "$profile" --home "$omp_home" --dest "$evidence/model-gateway" --key-dir "$work/model-gateway-key" --key-file "$key_file") ||
      fail "the agents' model route through the key file could not be installed (the reason is above)"
    ;;
esac
note "the agents' model route: $model_route, keyed by $key_command"
export XDG_STATE_HOME="$work/xdg"
export TMUX_TMPDIR="$work/tmux"
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/dispatch-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/dispatch-token")" >"$work/dispatch-auth-header" &&
  printf 'X-Dispatch-User: %s\n' "$proof_human_user" >"$work/dispatch-human-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/postgres-password")
chmod 0600 "$work"/*token "$work"/*-header "$work/postgres-password"
(cd "$root/packages/daemon" && go build -o "$work/legion" ./cmd/legion)
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener && go build -o "$work/envoy-dispatch" ./cmd/dispatch)
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/legion" "$work/envoy-listener" "$work/envoy-dispatch") || fail "lib/built-from.sh could not say what the run built"
while IFS= read -r line; do note "$line"; done <<<"$built"
docker ps >/dev/null
docker run -d --name "$pg_container" --mount type=tmpfs,destination=/var/lib/postgresql/data \
  -e POSTGRES_USER=legion -e POSTGRES_PASSWORD="$(cat "$work/postgres-password")" -e POSTGRES_DB=dispatch \
  -p "127.0.0.1::5432" mirror.gcr.io/pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a >/dev/null
port_pg=$(docker port "$pg_container" 5432/tcp | sed -n '1s/.*://p')
[ -n "$port_pg" ] || fail "docker assigned Postgres no host port"
until_true 60 "Postgres to accept TCP connections" docker exec "$pg_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d dispatch
docker exec "$pg_container" createdb -U legion legion
docker run -d --name "$nats_container" -p "127.0.0.1::4222" mirror.gcr.io/library/nats:2.10 -js >/dev/null
port_nats=$(docker port "$nats_container" 4222/tcp | sed -n '1s/.*://p')
[ -n "$port_nats" ] || fail "docker assigned NATS no host port"
until_true 60 "NATS to be ready" sh -c "docker logs '$nats_container' 2>&1 | grep -q 'Server is ready'"
start_listener
start_dispatch
dispatch_human POST projects "$(jq -cn --arg key "$project" --arg name "Parked decision proof $project" '{key:$key,name:$name}')" >/dev/null
dispatch_human PUT "settings/repo-projects/$repo" "$(jq -cn --arg project "$project" '{project:$project,default:true}')" >/dev/null
(cd "$root" && bun install --frozen-lockfile >/dev/null)
envoy_manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-envoy --profile "$profile" --home "$omp_home" --dest "$work/pi-envoy")
manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-legion --profile "$profile" --home "$omp_home" --dest "$work/pi-legion")
pin=$(<"$root/.omp-pin")
mise where "$pin" >/dev/null 2>&1 || mise install "$pin" >&2
cat >"$work/instructions.md" <<'EOF'
# Parked decision proof instructions

This is a throwaway workflow proof. A tree's root architect starts its tree from the daemon's
`catch-up` notice as its role says: it writes the spec in the issue's own primary document,
requests the spec's approval when the design gate policy arms the gate, and registers the gate,
then waits. Apart from that, do not act until a human Dispatch message targeted at your own session
gives the next exact proof operation; it arrives in your session as a message to you. A message you
only find by reading the issue (its events, a search) was sent to another session, even on your
issue, and any other notice is not an instruction: neither is yours to act on. This proof sends no
targeted message, so every phase worker reads what its role says to read and waits, and no tree
reaches a pull request. A re-admitted architect reads the task it is started with and its catch-up,
works the catch-up as above, and waits.
EOF
cat >"$work/controller-instructions.md" <<EOF
This is a throwaway proof on the disposable $project project. Hand Legion no issue from the \`todo\`
walk, file no issue, and post no daily report. Handle each \`parked-decision\` notice and run the
parked-root walk as \`skill://legion-controller\` says, without waiting for any message, for this
run's roots alone, whose titles end \`($work)\`; leave every other root as it is.
EOF
note "project $project, profile $profile, plugins $(jq -r '.name + "@" + .version' "$envoy_manifest") and $(jq -r '.name + "@" + .version' "$manifest"), NATS/Postgres ports $port_nats/$port_pg, listener $port_listener, Dispatch $port_dispatch"
pass
done_with rig

begin boot
pick_port port_daemon
pick_port port_worker_stream
write_legion_config
# In the environment start_daemon gives the daemon: boot refuses a first gh on PATH that is a script,
# such as the devbox's dotfiles shim, and realbin puts the real gh first (lib/rig.sh, real_bin_path).
out=$(HOME="$omp_home" OMP_PROFILE="$profile" PATH="$realbin:$PATH" "$work/legion" start --check-config --config "$work/legion.yaml") ||
  fail "legion start --check-config refused the run's legion.yaml: $out"
[ "$(head -n 1 <<<"$out")" = "Config OK: project=$project" ] || fail "legion start --check-config said: $out"
note "$(head -n 1 <<<"$out")"
start_daemon
daemon_state >"$evidence/boot-state.json" || fail "legion state does not answer"
note "the daemon answers on $port_daemon; legion state reads project $(jq -r .daemon.project "$evidence/boot-state.json")"
cat >"$work/controller.yaml" <<EOF
project: $project
daemon_url: http://127.0.0.1:$port_daemon
operator_token_file: $work/operator-token
envoy_url: http://127.0.0.1:$port_listener
envoy_token_file: $work/envoy-token
nats_urls: [nats://127.0.0.1:$port_nats]
dispatch_url: $dispatch_base
dispatch_token_file: $work/dispatch-token
instructions: $work/controller-instructions.md
omp_invocation: mise x $pin -- omp
state_dir: $work/controller-state
EOF
pass
done_with boot

begin controller-up
waited=$SECONDS
start_controller ctl1
until_true 300 "controllerLocator to name session 1" controller_session_id
session1=$(controller_session_id)
note "controllerLocator.sessionId $session1 ($(since "$waited"))"
until_true 120 "the profile to hold the controller's session file" compgen -G "$profile_agent/sessions/*/*.jsonl"
mapfile -t files < <(compgen -G "$profile_agent/sessions/*/*.jsonl")
[ "${#files[@]}" = 1 ] || fail "the profile holds ${#files[@]} session files, want 1: ${files[*]}"
session1_file=${files[0]}
until_true 60 "$session1_file to hold its session header" session_header "$session1_file"
session_field=$(session_header "$session1_file" | jq -er --arg s "$session1" 'first(to_entries[] | select(.value == $s) | .key)') ||
  fail "the session header of $session1_file names no field holding $session1: $(session_header "$session1_file")"
note "session 1's file $session1_file; its header names the session in .$session_field"
until_true 300 "session 1's first turn to start itself, with nothing typed" started_itself "$session1_file"
note "the first user message is the start message, and an assistant message follows ($(since "$waited"))"
pass
done_with controller-up

begin trees-wait-on-a-person
waited=$SECONDS
tree1=$(new_issue "Parked decision proof tree 1: approved after its park ($work)")
tree2=$(new_issue "Parked decision proof tree 2: questioned after its park ($work)")
tree4=$(new_issue "Parked decision proof tree 4: parked by the person, then approved ($work)")
tree5=$(new_issue "Parked decision proof tree 5: parked by another session, then approved ($work)")
for tree in $tree1 $tree2 $tree4 $tree5; do
  set_status "$tree" todo
done
for tree in $tree1 $tree2 $tree4 $tree5; do
  until_true 1800 "$tree to wait on its design gate" waits_on_gate "$tree"
done
artifact1=$(gate_artifact_of "$tree1")
artifact2=$(gate_artifact_of "$tree2")
artifact4=$(gate_artifact_of "$tree4")
artifact5=$(gate_artifact_of "$tree5")
note "$tree1 (spec $artifact1), $tree2 (spec $artifact2), $tree4 (spec $artifact4) and $tree5 (spec $artifact5) are awaiting_person on their gate ($(since "$waited"))"
pass
done_with trees-wait-on-a-person

begin trees-parked
waited=$SECONDS
park "$tree1"
controller_mark "$tree1" "the proof's hold of tree 1"
park "$tree2"
set_status "$tree4" backlog
dispatch_agent PATCH "issues/$tree5" '{"status":"backlog"}' >/dev/null
for tree in $tree1 $tree2 $tree4 $tree5; do
  until_true 300 "$tree to be parked" parked "$tree"
done
# Who parked a tree is the author of its park: the operator's `legion status` writes as the daemon's
# session, the person's own write as the person, and the stand-in's as its session. Tree 1 also
# carries the controller's hold mark, as session 1 posts it after a park for a hold, so its park is
# Legion's own hold; tree 2's operator park carries none.
[ "$(parked_by "$tree1")" = "session:legion-daemon:$project" ] || fail "$tree1's park is by $(parked_by "$tree1"), want the daemon's session"
[ "$(parked_by "$tree2")" = "session:legion-daemon:$project" ] || fail "$tree2's park is by $(parked_by "$tree2"), want the daemon's session"
[ "$(parked_by "$tree4")" = "user:$proof_human_user" ] || fail "$tree4's park is by $(parked_by "$tree4"), want user:$proof_human_user"
[ "$(parked_by "$tree5")" = "session:$dispatch_agent_session" ] || fail "$tree5's park is by $(parked_by "$tree5"), want session:$dispatch_agent_session"
has_mark "$tree1" || fail "$tree1 carries no hold mark from $session1"
! has_mark "$tree2" || fail "$tree2, the operator's park, carries a hold mark"
note "$tree1 (the daemon's session, with the hold mark) and $tree2 (the daemon's session, no mark), $tree4 (by $proof_human_user) and $tree5 (by session $dispatch_agent_session) are phase done with no slot, each architect suspended ($(since "$waited"))"
pass
done_with trees-parked

begin approval-readmits
waited=$SECONDS
approval_seq=$(last_seq "$tree1")
approve "$artifact1"
until_true 300 "the outbox to finish $tree1's approval undelivered" undelivered_logged "$tree1"
until_true 300 "the outbox to relay $tree1's approval to the controller" sent_logged "$tree1"
note "the daemon log holds the undelivered line and the relay for $tree1 ($(since "$waited"))"
needle1=$(notice_needle parked-decision "$tree1")
until_true 600 "session 1 to receive '$needle1'" controller_delivered "$session1_file" "$needle1"
until_true 900 "session 1 to set $tree1 todo after the notice" set_status_after "$session1_file" "$tree1" "$needle1"
until_true 300 "Dispatch to show $tree1 todo by legion-daemon:$project" todo_by_daemon_after "$tree1" "$approval_seq"
note "session 1 got the notice and set $tree1 todo; Dispatch records the write as the daemon's ($(since "$waited"))"
until_true 600 "$tree1 to be admitted" active "$tree1"
until_true 1800 "$tree1 to reach planning" issue_phase "$tree1" planning
spec1="dispatch://$tree1/spec (design-approved by user:$proof_human_user)"
until_true 300 "$tree1's architect to be told '$spec1'" architect_turn_holds "$tree1" "$spec1"
until_true 600 "session 1 to post its Parked decision read: comment on $tree1" read_comment "$tree1" "$session1"
note "$tree1 is admitted and planning; its architect's task names $spec1; session 1 commented on it ($(since "$waited"))"
pass
done_with approval-readmits

begin question-stays-parked
waited=$SECONDS
ask2=$(dispatch_get "issues/$tree2/asks" | jq -er 'first(.[] | select(.kind == "approval" and .state == "open")) | .id') ||
  fail "$tree2 has no open approval ask"
reply_seq=$(last_seq "$tree2")
dispatch_human POST "issues/$tree2/comments" "$(jq -cn --arg ask "$ask2" '{body: "Why does this smoke proof need a file of its own?", ask_id: $ask}')" >/dev/null
until_true 300 "the workflow to relay the person's reply on $tree2" log_has \
  ".msg == \"workflow: a person's decision on a parked tree goes to the controller\" and .issue == \"$tree2\" and .decision == \"ask-reply\" and .actor == \"user:$proof_human_user\""
needle2=$(notice_needle parked-decision "$tree2")
until_true 600 "session 1 to receive '$needle2'" controller_delivered "$session1_file" "$needle2"
until_true 900 "session 1 to post its Parked decision read: reply on $tree2" read_comment "$tree2" "$session1"
comment2=$(read_comment "$tree2" "$session1")
# A question is answered on its own ask, and the reply hands the turn to the person.
jq -e --arg ask "$ask2" '.ask_id == $ask' <<<"$comment2" >/dev/null ||
  fail "session 1's Parked decision read: comment on $tree2 is not a reply on the ask $ask2: $(jq -c '{id, ask_id}' <<<"$comment2")"
dispatch_get "issues/$tree2/asks" | jq -e --arg ask "$ask2" 'any(.[]; .id == $ask and .waiting_on == "human")' >/dev/null ||
  fail "the ask $ask2 does not wait on the person after session 1's reply: $(dispatch_get "issues/$tree2/asks" | jq -c --arg ask "$ask2" '[.[] | select(.id == $ask) | {state, waiting_on}]')"
note "session 1 got the reply's notice and answered on the ask, which now waits on the person: $(jq -r '.body | split("\n")[0]' <<<"$comment2") ($(since "$waited"))"
dispatch_agent POST "issues/$tree2/comments" "$(jq -cn --arg ask "$ask2" '{body: "A stand-in session reply for the parked decision proof.", ask_id: $ask, turn: "agent"}')" >/dev/null
until_true 300 "the workflow to log that the session's reply is not relayed" log_has \
  ".msg == \"workflow: a session's reply on a parked tree is not relayed\" and .issue == \"$tree2\" and .actor == \"session:$dispatch_agent_session\""
commented_at=$(date -d "$(jq -r .created_at <<<"$comment2")" +%s)
rest=$((commented_at + 600 - $(date +%s)))
if ((rest > 0)); then
  note "reading $tree2 again ${rest}s from now, 600 s after the controller's comment"
  sleep "$rest"
fi
[ "$(controller_deliveries "$session1_file" "$needle2")" = 1 ] ||
  fail "session 1 holds $(controller_deliveries "$session1_file" "$needle2") deliveries of '$needle2', want 1"
dispatch_status_is "$tree2" backlog >/dev/null || fail "$tree2 is $(dispatch_get "issues/$tree2" | jq -r .status), want backlog"
parked "$tree2" || fail "$tree2 is no longer phase done with no slot and its architect suspended: $(daemon_state | jq -c --arg k "$tree2" '.issues[$k] | {phase, slot, architect: .architect.state}')"
! todo_by_daemon_after "$tree2" "$reply_seq" || fail "Dispatch shows $tree2 set todo after the reply"
dispatch_events "$tree2" | jq -e --argjson seq "$reply_seq" '[.[] | select(.seq > $seq and .type == "issue.updated" and .payload.status == "todo")] == []' >/dev/null ||
  fail "Dispatch shows a status todo event on $tree2 after the reply"
note "600 s on, $tree2 is backlog, phase done, with no todo event after the reply; the session's reply woke no second notice ($(since "$waited"))"
pass
done_with question-stays-parked

begin foreign-park-stays
waited=$SECONDS
# A park that is not Legion's own hold moves only when whoever made it asks: the controller moves
# none of these trees and names who parked each, the daemon's session with no hold mark for tree 2
# (the operator's `legion status`), the person for tree 4 and the stand-in session for tree 5.
declare -A seq_of before_of told_of parker_of read_of
parker_of[$tree2]="legion-daemon"
parker_of[$tree4]="$proof_human_user parked the tree"
parker_of[$tree5]="$dispatch_agent_session parked the tree"
for tree in $tree2 $tree4 $tree5; do
  seq_of[$tree]=$(last_seq "$tree")
  before_of[$tree]=$(read_comment "$tree" "$session1" || true)
  told_of[$tree]=$(controller_deliveries "$session1_file" "$(notice_needle parked-decision "$tree")")
done
approve "$artifact2"
approve "$artifact4"
approve "$artifact5"
later=0
for tree in $tree2 $tree4 $tree5; do
  until_true 300 "the outbox to relay $tree's approval to the controller" sent_logged "$tree"
  needle=$(notice_needle parked-decision "$tree")
  until_true 600 "session 1 to receive another '$needle'" delivered_more "$session1_file" "$needle" "${told_of[$tree]}"
  until_true 900 "session 1 to post a new Parked decision read: comment on $tree" read_comment_after "$tree" "$session1" "${before_of[$tree]}"
  comment=$(read_comment "$tree" "$session1")
  jq -e --arg p "${parker_of[$tree]}" '.body | contains($p) and contains("stays parked")' <<<"$comment" >/dev/null ||
    fail "session 1's new Parked decision read: comment on $tree does not name '${parker_of[$tree]}' and say it stays parked: $(jq -r .body <<<"$comment")"
  at=$(date -d "$(jq -r .created_at <<<"$comment")" +%s)
  if ((at > later)); then
    later=$at
  fi
  note "session 1 commented on $tree without moving it: $(jq -r '.body | split("\n")[0]' <<<"$comment")"
done
rest=$((later + 600 - $(date +%s)))
if ((rest > 0)); then
  note "reading $tree2, $tree4 and $tree5 again ${rest}s from now, 600 s after the latest comment"
  sleep "$rest"
fi
for tree in $tree2 $tree4 $tree5; do
  ! set_status_after "$session1_file" "$tree" "" || fail "session 1 set $tree, which is no hold of Legion's own, to todo"
  dispatch_status_is "$tree" backlog >/dev/null || fail "$tree is $(dispatch_get "issues/$tree" | jq -r .status), want backlog"
  parked "$tree" || fail "$tree is no longer phase done with no slot and its architect suspended: $(daemon_state | jq -c --arg k "$tree" '.issues[$k] | {phase, slot, architect: .architect.state}')"
  dispatch_events "$tree" | jq -e --argjson seq "${seq_of[$tree]}" '[.[] | select(.seq > $seq and .type == "issue.updated" and .payload.status == "todo")] == []' >/dev/null ||
    fail "Dispatch shows a status todo event on $tree after the approval"
  read_of[$tree]=$(any_read_comment "$tree")
done
note "600 s on, $tree2, $tree4 and $tree5 are backlog, phase done, with no todo event after their approvals ($(since "$waited"))"
pass
done_with foreign-park-stays

begin controller-down
waited=$SECONDS
controller_quit ctl1
note "session 1 quit, and the controller role has no holder ($(since "$waited"))"
pass
done_with controller-down

begin walk-on-start
waited=$SECONDS
tree3=$(new_issue "Parked decision proof tree 3: approved while no controller runs ($work)")
set_status "$tree3" todo
until_true 1800 "$tree3 to wait on its design gate" waits_on_gate "$tree3"
artifact3=$(gate_artifact_of "$tree3")
park "$tree3"
controller_mark "$tree3" "the proof's hold of tree 3"
until_true 300 "$tree3 to be parked" parked "$tree3"
approve "$artifact3"
until_true 300 "the outbox to relay $tree3's approval while no controller runs" sent_logged "$tree3"
controller_runs && fail "a controller runs before ctl2 starts"
start_seq=$(last_seq "$tree3")
note "$tree3 was approved after its park; the daemon relayed it while no controller ran ($(since "$waited"))"
start_controller ctl2
until_true 300 "controllerLocator to name a session other than $session1" sh -c \
  "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg s '$session1' '.controllerLocator.sessionId != null and .controllerLocator.sessionId != \$s' >/dev/null"
session2=$(controller_session_id)
until_true 120 "session 2's file to exist" controller_session_file "$session2"
session2_file=$(controller_session_file "$session2")
note "session 2 is $session2, its file $session2_file"
until_true 300 "session 2's first turn to start itself" started_itself "$session2_file"
needle3=$(notice_needle parked-decision "$tree3")
until_true 900 "session 2 to set $tree3 todo" set_status_after "$session2_file" "$tree3" ""
! controller_delivered "$session2_file" "$needle3" || fail "session 2 holds a '$needle3' delivery: the walk, not a notice, must re-admit $tree3"
until_true 300 "Dispatch to show $tree3 todo by legion-daemon:$project" todo_by_daemon_after "$tree3" "$start_seq"
until_true 1800 "$tree3 to reach planning" issue_phase "$tree3" planning
spec3="dispatch://$tree3/spec (design-approved by user:$proof_human_user)"
until_true 300 "$tree3's architect to be told '$spec3'" architect_turn_holds "$tree3" "$spec3"
until_true 600 "session 2 to post its Parked decision read: comment on $tree3" read_comment "$tree3" "$session2"
note "the walk re-admitted $tree3 with no notice; its architect's task names $spec3; session 2 commented on it ($(since "$waited"))"
until_true 1800 "session 2's file to stay unchanged for 120 s" file_quiet "$session2_file" 120
for tree in $tree2 $tree4 $tree5; do
  dispatch_status_is "$tree" backlog >/dev/null || fail "$tree is $(dispatch_get "issues/$tree" | jq -r .status), want backlog"
  ! set_status_after "$session2_file" "$tree" "" || fail "session 2 set $tree, which is no hold of Legion's own, to todo"
  [ "$(any_read_comment "$tree")" = "${read_of[$tree]}" ] || fail "$tree has a Parked decision read: comment newer than foreign-park-stays's: the walk judged its approval again"
done
! controller_delivered "$session2_file" "$needle3" || fail "session 2 holds a '$needle3' delivery"
note "$tree2, $tree4 and $tree5 are still backlog with no newer Parked decision read: comment ($(since "$waited"))"
pass
done_with walk-on-start

begin verdict
verdict=$(jq -R -s -c -f "$root/scripts/e2e/lib/parked-decision-verdict.jq" "$evidence/logs/daemon.log")
printf '%s\n' "$verdict" >"$evidence/parked-decision-verdict.json"
jq -e '.unmatched == [] and .unrelayed == []' <<<"$verdict" >/dev/null ||
  fail "the verdict has unmatched or unrelayed rows: $(jq -c '{unmatched, unrelayed}' <<<"$verdict")"
jq -e --arg a "$tree1" --arg b "$tree2" --arg c "$tree3" --arg d "$tree4" --arg e "$tree5" '[.sent[].issue] as $s | all($a, $b, $c, $d, $e; . as $k | $s | index($k) != null)' <<<"$verdict" >/dev/null ||
  fail "the verdict's sent rows do not name $tree1, $tree2, $tree3, $tree4 and $tree5: $(jq -c .sent <<<"$verdict")"
note "undelivered $(jq '.undelivered | length' <<<"$verdict"), sent $(jq -c '[.sent[].issue]' <<<"$verdict"), unrelayed 0, unmatched 0"
pass
done_with verdict

begin cleanup-is-complete
controller_quit ctl2
tm kill-server >/dev/null 2>&1 || true
stop_pid "$daemon_pid"
stop_pid "$dispatch_pid"
stop_pid "$listener_pid"
TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
for pid in $daemon_pid $dispatch_pid $listener_pid; do
  ! kill -0 "$pid" 2>/dev/null || fail "process $pid still runs"
done
docker rm -f "$pg_container" "$nats_container" >/dev/null
for container in "$pg_container" "$nats_container"; do
  ! docker inspect "$container" >/dev/null 2>&1 || fail "the container $container remains"
done
wait_for_run_processes
remove_run_branches
for issue in $tree1 $tree2 $tree3 $tree4 $tree5; do
  if out=$(timeout 60 gh api "repos/$repo/branches/legion/$issue" 2>&1); then
    fail "$repo still has the branch legion/$issue"
  fi
  grep -qF 'HTTP 404' <<<"$out" || fail "gh could not tell whether $repo has legion/$issue: $out"
done
collect_transcripts
[ ! -e "$HOME/.omp/profiles/$profile" ] || fail "the run wrote the operator's profile root: $HOME/.omp/profiles/$profile exists"
rm -rf "$work"
[ ! -e "$work" ] || fail "the scratch work directory $work remains"
note "no service, container, tmux server or branch of the run remains; $work is gone; the evidence is at $evidence"
pass

ok=1
if [ -n "$until_checkpoint" ]; then
  echo "parked decision devbox e2e: development run until $until_checkpoint finished (not the proof)"
else
  echo "parked decision devbox e2e: PASS"
fi
