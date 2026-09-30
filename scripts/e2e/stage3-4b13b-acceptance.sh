#!/usr/bin/env bash
# Live acceptance for LEGION-208 task 4b.13b (PR sjawhar/legion#1333) and the owed live proof of
# #1313's pane rule. It stands up the Stage 3 rig (scratch Postgres, NATS, Envoy listener, native
# Dispatch, the subscribe-only GitHub bridge, the branch plugin in an isolated OMP profile, the Go
# daemon built from ACCEPT_ROOT) exactly as scripts/e2e/stage3-devbox-workflow.sh does, sourcing
# that checkout's lib/rig.sh and lib/workflow.sh, and drives real agents through:
#   - prompts.New rewriting a stale Go prompt part at boot and leaving identical ones untouched;
#   - the refused root stop naming `legion claims close` only for a tree no workflow issue backs;
#   - the pane rule (#1313) in a root-architect pane, a phase-worker pane (with the phase still open
#     after each refusal and the phase-stall follow-up clearing after the tool's handoff_complete),
#     a `task` subagent, and an operator-spawned sub-architect, each against `legion handoff
#     complete` plain, through `bash -lc`, and through eval, with `legion handoff read` from bash as
#     the narrowed-rule control;
#   - park_child / rerun_child with their refusals, and the Dispatch writes' actor;
#   - phase-finished carrying the worker's summary and verdict;
#   - the daemon posting the merger's READY packet (direct, and stored across a refused gate), and
#     publishing it to merge_queue_role (a live holder, and no holder);
#   - pr-merged / pr-closed-unmerged, and an issue whose PR merged early skipping READY.
# It never merges into sjawhar/legion-smoke's main: each proof PR is retargeted to a scratch base
# branch before any merge, and the scratch base is deleted at the end.
#
# Run it as `bash scripts/e2e/stage3-4b13b-acceptance.sh` from the operator's own Oh My Pi session,
# with the Stage 3 proof's two required inputs, LEGION_E2E_MODEL_GATEWAY_URL and SMOKE_UPSTREAM_NATS
# (scripts/e2e/README.md), and three of its own, since it creates no Docker container:
# ACCEPT_PG_CONTAINER and ACCEPT_PG_PORT name a running Postgres container (user postgres, password
# ci) in which the daemon and Dispatch take their own databases, and ACCEPT_NATS_BIN a nats-server
# binary it runs natively. Its GitHub writes are the Stage 3 proof human's, the devbox gh acting as
# the sjawhar-agent App, so the session is not a Legion pane and carries no personal GH_TOKEN:
# `prerequisites` refuses to start otherwise (require_proof_human, lib/workflow.sh). The binary
# under test is stamped (vcs.revision and main.revision) and the run's scratch workspace is always
# kept; phase workers, which act on the daemon's assignment alone when no instruction reaches them,
# are instructed by a background watcher the moment each assignment arrives, and the script asserts
# what the daemon did rather than the order it expected.
set -Eeuo pipefail

root=${ACCEPT_ROOT:-$(cd "$(dirname "$0")/../.." && pwd)}
base_rev=${ACCEPT_BASE_REV:-5ca2e53c}
stamp=$(date +%s)
work=$(mktemp -d /tmp/legion-accept4b13b.XXXXXXXX)
evidence=${ACCEPT_EVIDENCE_DIR:-$work/evidence}
mkdir -p "$evidence/logs" "$evidence/transcripts"
# Every line of the run also goes to $evidence/transcript.log (lib/transcript.sh), so the driver and
# its cleanup, which closes the proof's pull requests, never fail on a write whoever is reading. The
# evidence is under $work by default, and the sweeps in services-stopped and cleanup SIGKILL every
# process whose command line names $work (run_processes, lib/rig.sh), so the tee gets the transcript
# as fd 8, never by path. The tee keeps the copy it forked with, and the driver closes its own so
# nothing it starts inherits it. The lib is this script's own, not ACCEPT_ROOT's.
exec 8>>"$evidence/transcript.log"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$(dirname "$0")/lib/transcript.sh"
transcript_to /dev/fd/8
exec 8>&-
ok=
check=setup
project="AC$(( ($$ + stamp) % 100000000 ))"
project=${project:0:10}
ptoken=${project,,}
profile="legion-accept4b13b-$$-$stamp"
state="$work/state"
# The HOME the run's Oh My Pi processes run under, so its profile lives in the work directory
# (make_omp_home, lib/omp-home.sh).
omp_home="$work/omp-home"
profile_agent="$omp_home/.omp/profiles/$profile/agent"
repo="sjawhar/legion-smoke"
scratch_base="l208-4b13b-accept-$stamp"
merge_base="$scratch_base-merge"
# The shared Postgres container: this run only creates and reads its own two databases in it and
# never removes it.
pg_container=${ACCEPT_PG_CONTAINER:?ACCEPT_PG_CONTAINER names the running Postgres container the run takes its databases in}
port_pg=${ACCEPT_PG_PORT:?ACCEPT_PG_PORT is the host port of that container}
pg_user=postgres
legion_db="legion_$ptoken" dispatch_db="dispatch_$ptoken"
nats_bin=${ACCEPT_NATS_BIN:?ACCEPT_NATS_BIN is the nats-server binary the run starts}
daemon_pid='' dispatch_pid='' listener_pid='' bridge_pid='' watcher_pid='' nats_pid=''
port_daemon='' port_listener='' port_dispatch='' port_worker_stream='' port_nats=''
# timeout_hook names a function lib/rig.sh's until_true runs when a wait times out.
# shellcheck disable=SC2034
timeout_hook=''
prod_baseline='' prod_dispatch_url=''
prod_envoy_url=${STAGE3_PRODUCTION_ENVOY_URL:-http://127.0.0.1:9020}
audited=
soft_failures="$evidence/soft-failures.txt"
: >"$soft_failures"

begin() { check=$1; printf '== %s  (%s)\n' "$check" "$(date -u +%T)"; }
note() { printf '   %s\n' "$*"; }
pass() { printf 'ok %s\n' "$check"; }
fail() { printf 'FAIL %s: %s\n' "$check" "$*" >&2; exit 1; }
# soft records a failed assertion and lets the run go on, so one run yields every observation; the
# run ends non-zero naming each one.
soft() { printf 'SOFT-FAIL %s: %s\n' "$check" "$*" | tee -a "$soft_failures" >&2; }
# shellcheck source=/dev/null
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source=/dev/null
. "$root/scripts/e2e/lib/omp-home.sh"
# shellcheck source=/dev/null
. "$root/scripts/e2e/lib/workflow.sh"
# shellcheck source=/dev/null
. "$root/scripts/e2e/lib/stage-role-prompts.sh"
# The daemon's database is this run's own in the shared Postgres container, read with the host psql.
db_value() { PGPASSWORD=$(cat "$work/postgres-password") psql -h 127.0.0.1 -p "$port_pg" -U "$pg_user" -d "$legion_db" -tAc "$1"; }

collect_transcripts() {
  local sessions="$profile_agent/sessions"
  [ -d "$sessions" ] || return 0
  cp -a "$sessions/." "$evidence/transcripts/" 2>/dev/null || true
}

cleanup() {
  local p
  set +e
  # Teardown is best effort, and errexit off does not turn the ERR trap off: a command that fails
  # here is a warning about the teardown, never a check's FAIL line, and the run's exit status is
  # left as the checks set it. The warning names the line alone: inside a trap BASH_COMMAND is the
  # command the trap interrupted, not the cleanup command that failed.
  trap 'printf "cleanup warning: line %s exited %s\n" "$LINENO" "$?" >&2' ERR
  if [ -z "${ok:-}" ] && [ -z "$audited" ] && [ -n "$prod_baseline" ]; then
    printf 'production audit after failure:\n' >&2
    production_audit >&2
  fi
  # A pass has already captured both in services-stopped and stopped the daemon; a redirection to
  # the stopped daemon would truncate them.
  if [ -n "${daemon_pid:-}" ]; then
    daemon_state >"$evidence/final-state.json" 2>/dev/null
    "$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" >"$evidence/final-claims.json" 2>/dev/null
  fi
  stop_pid "$watcher_pid"
  stop_pid "$daemon_pid"
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  stop_pid "$bridge_pid"
  stop_pid "$nats_pid"
  stop_pid "${subagent_watch_pid:-}"
  stop_pid "${instructor_pid:-}"
  TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
  collect_transcripts
  rm -rf "$work/model-gateway-cache" || true
  github_cleanup
  printf "the run's scratch workspace, kept for review, is %s\n" "$work" >&2
  printf "the run's evidence is %s\n" "$evidence" >&2
  return 0
}
# github_cleanup closes every proof PR still open, deletes every proof head branch, and deletes the
# scratch base. Nothing here touches the smoke main, and nothing runs unless require_proof_human
# passed: a refused run's gh acts as someone else.
github_cleanup() {
  local n b
  [ -n "$proof_human" ] || return 0
  [ -n "${main_sha:-}" ] || return 0
  for n in $(gh -R "$repo" pr list --state open --limit 100 --json number,headRefName \
    --jq ".[] | select(.headRefName | startswith(\"legion/$project-\")) | .number" 2>/dev/null); do
    gh -R "$repo" pr close "$n" >/dev/null 2>&1 || true
  done
  for b in $(gh api "repos/$repo/git/matching-refs/heads/legion/$project-" --jq '.[].ref' 2>/dev/null); do
    gh api -X DELETE "repos/$repo/git/$b" >/dev/null 2>&1 || true
  done
  gh api -X DELETE "repos/$repo/git/refs/heads/$scratch_base" >/dev/null 2>&1 || true
  gh api -X DELETE "repos/$repo/git/refs/heads/$merge_base" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'printf "FAIL %s: line %s exited %s: %s\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- the rig: copied from stage3-devbox-workflow.sh ------------------------------------------------
start_listener() {
  local token attempt offset result
  token=$(cat "$work/envoy-token")
  for attempt in 1 2 3 4 5; do
    pick_port port_listener
    offset=$(log_size listener)
    ENVOY_API_TOKEN="$token" PORT="$port_listener" ENVOY_LISTEN_HOST=127.0.0.1 \
      ENVOY_MACHINE_ID="legion-accept4b13b-$$" NATS_URLS="nats://127.0.0.1:$port_nats" \
      start_process listener env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
        -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/envoy-listener"
    result=0
    await_start listener "$listener_pid" "$offset" 60 "the Envoy listener to answer /v1/sessions" \
      curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$port_listener/v1/sessions" || result=$?
    [ "$result" != 0 ] || return 0
    note "the Envoy listener lost port $port_listener (attempt $attempt); picking another"
  done
  fail "the Envoy listener lost its picked port five times"
}

start_dispatch() {
  local keep=${1:-} dispatch_token envoy_token pg_password attempt offset result
  dispatch_token=$(cat "$work/dispatch-token")
  envoy_token=$(cat "$work/envoy-token")
  pg_password=$(cat "$work/postgres-password")
  mkdir -p "$work/dispatch-home"
  chmod 0700 "$work/dispatch-home"
  for attempt in 1 2 3 4 5; do
    [ -n "$keep" ] || pick_port port_dispatch
    offset=$(log_size dispatch)
    DATABASE_URL="postgres://$pg_user:$pg_password@127.0.0.1:$port_pg/$dispatch_db?sslmode=disable" \
      DISPATCH_AGENT_TOKEN="$dispatch_token" ENVOY_TOKEN="$envoy_token" HOME="$work/dispatch-home" \
      DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_ALLOWED_LOGINS=smoke \
      DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$port_dispatch" \
      DISPATCH_SERVER_URL="http://127.0.0.1:$port_dispatch" NATS_URLS="nats://127.0.0.1:$port_nats" \
      ENVOY_URL="http://127.0.0.1:$port_listener" \
      start_process dispatch env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
        -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/envoy-dispatch"
    result=0
    await_start dispatch "$dispatch_pid" "$offset" 60 "the scratch Dispatch server" \
      curl -fsS "http://127.0.0.1:$port_dispatch/api/v1" || result=$?
    [ "$result" != 0 ] || return 0
    [ -z "$keep" ] || fail "the restarted scratch Dispatch lost port $port_dispatch"
    note "the scratch Dispatch lost port $port_dispatch (attempt $attempt); picking another"
  done
  fail "the scratch Dispatch lost its picked port five times"
}

# The one difference from Stage 3's configuration: admission_cap 3 (three roots) and a merge queue
# role on the project, so the daemon's READY publish is exercised.
write_legion_config() {
  cat >"$work/legion.yaml" <<EOF
project: $project
port: $port_daemon
worker_stream_port: $port_worker_stream
postgres_dsn: postgres://$pg_user:$(cat "$work/postgres-password")@127.0.0.1:$port_pg/$legion_db?sslmode=disable
state_dir: $state
operator_token_file: $work/operator-token
omp_invocation: mise x $pin -- omp
envoy_url: http://127.0.0.1:$port_listener
envoy_token_file: $work/envoy-token
nats_urls:
  - nats://127.0.0.1:$port_nats
dispatch_url: http://127.0.0.1:$port_dispatch
dispatch_token_file: $work/dispatch-token
projects:
  $project: { repo: $repo, merge_queue_role: merge-queue }
gates:
  design: root-issues
admission_cap: 3
review_round_cap: 3
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
  local keep=${1:-} attempt offset result
  for attempt in 1 2 3 4 5; do
    if [ -z "$keep" ]; then
      pick_port port_daemon
      pick_port port_worker_stream
      write_legion_config
    fi
    offset=$(log_size daemon)
    HOME="$omp_home" OMP_PROFILE="$profile" LEGION_GH_PATH="$real_gh" env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE \
      -u NATS_DAEMON_NKEY_SEED -u NATS_DAEMON_NKEY_SEED_FILE -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
      -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 \
      "$work/legion" start --config "$work/legion.yaml" >>"$evidence/logs/daemon.log" 2>&1 &
    daemon_pid=$!
    result=0
    await_start daemon "$daemon_pid" "$offset" 180 "the Go daemon to answer /healthz" \
      curl -fsS "http://127.0.0.1:$port_daemon/healthz" || result=$?
    [ "$result" != 0 ] || return 0
    [ -z "$keep" ] || fail "the restarted Go daemon lost port $port_daemon or $port_worker_stream"
    note "the Go daemon lost a port (attempt $attempt); picking again"
  done
  fail "the Go daemon lost a picked port five times"
}
stop_daemon() { kill -TERM "$daemon_pid" 2>/dev/null || true; stop_pid "$daemon_pid"; daemon_pid=; }

claim_session_file() {
  local issue=$1 role=$2
  "$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" |
    jq -er --arg issue "$issue" --arg role "$role" \
      '[.claims[] | select(.issue == $issue and .role == $role and .sessionFile != null and .sessionFile != "")] | last | .sessionFile'
}
claim_session_text() { local f; f=$(claim_session_file "$1" "$2") || return 1; cat -- "$f"; }
workspace_jj() { local issue=$1; shift; jj -R "$state/workspaces/$repo/${issue,,}" "$@"; }

omp_descendant() {
  local queue=("$1") pid children
  while [ "${#queue[@]}" -gt 0 ]; do
    pid=${queue[0]}
    queue=("${queue[@]:1}")
    if [ "$(basename "$(tr '\0' '\n' <"/proc/$pid/cmdline" 2>/dev/null | sed -n 1p)")" = omp ]; then
      printf '%s\n' "$pid"
      return 0
    fi
    children=$(cat /proc/"$pid"/task/*/children 2>/dev/null || true)
    for child in $children; do queue+=("$child"); done
  done
  return 1
}
claim_pane_pid() {
  daemon_state | jq -er --arg issue "$1" --arg role "$2" \
    '(if $role == "architect" then .issues[$issue].architect else .issues[$issue].workers[$role].claim end).locator.incarnation | split(":")[0]'
}
pane_value() { tr '\0' '\n' <"/proc/$1/environ" | sed -n "s/^$2=//p"; }
endpoint_mismatch() {
  local omp=$1 name want got
  for name in DISPATCH_URL DISPATCH_TOKEN_FILE ENVOY_URL ENVOY_NATS_URL; do
    case "$name" in
      DISPATCH_URL) want="http://127.0.0.1:$port_dispatch" ;;
      DISPATCH_TOKEN_FILE) want="$state/secrets/dispatch-token" ;;
      ENVOY_URL) want="http://127.0.0.1:$port_listener" ;;
      ENVOY_NATS_URL) want="nats://127.0.0.1:$port_nats" ;;
    esac
    got=$(pane_value "$omp" "$name")
    if [ "$got" != "$want" ]; then printf '%s=%s, want %s\n' "$name" "${got:-<unset>}" "$want"; return 0; fi
  done
  if grep -qz '^ANTHROPIC_API_KEY=' "/proc/$omp/environ" 2>/dev/null; then printf 'ANTHROPIC_API_KEY set, want unset\n'; return 0; fi
  return 1
}
assert_claim_endpoints() {
  local issue=$1 role=$2 pane_pid omp mismatch
  pane_pid=$(claim_pane_pid "$issue" "$role") || fail "$role pane on $issue has no process locator"
  omp=$(omp_descendant "$pane_pid") || fail "$role pane on $issue (pid $pane_pid) has no OMP process"
  if mismatch=$(endpoint_mismatch "$omp"); then fail "ABORT: $role pane on $issue (OMP pid $omp) has $mismatch"; fi
}
pane_watcher() {
  local claims inc issue role omp mismatch
  trap - EXIT ERR
  set +e
  while :; do
    if claims=$("$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" 2>/dev/null); then
      while IFS=$'\t' read -r inc issue role; do
        grep -qF "$inc " "$evidence/pane-endpoints-checked.txt" 2>/dev/null && continue
        omp=$(omp_descendant "${inc%%:*}") || continue
        if mismatch=$(endpoint_mismatch "$omp"); then
          printf '%s pane on %s (incarnation %s, OMP pid %s) has %s\n' "$role" "$issue" "$inc" "$omp" "$mismatch" >"$evidence/pane-endpoint-violation.txt"
          TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
          return 0
        fi
        printf '%s %s %s %s %s\n' "$inc" "$issue" "$role" "$omp" "$(date -u +%FT%T.%3NZ)" >>"$evidence/pane-endpoints-checked.txt"
      done < <(jq -r '.claims[] | select(.locator != null) | [.locator.incarnation, .issue, .role] | @tsv' <<<"$claims")
    fi
    sleep 0.2
  done
}
unchecked_launches() {
  local inc
  for inc in $(jq -R -r 'fromjson? | select(.msg == "supervise: launched") | .incarnation' "$evidence/logs/daemon.log"); do
    grep -qF "$inc " "$evidence/pane-endpoints-checked.txt" || printf '%s\n' "$inc"
  done
}
no_unchecked_launches() { [ -z "$(unchecked_launches)" ]; }
report_unchecked_launches() { note "launched panes never endpoint-checked: $(unchecked_launches | tr '\n' ' ')"; }

prod_header_file() {
  local header=$work/production-dispatch-auth
  (umask 077 && jq -r '"Authorization: Bearer " + .dispatch.token' "$HOME/.config/opencode/envoy.json" >"$header")
  printf '%s\n' "$header"
}
production_baseline() {
  local header since keys key id max=0
  prod_dispatch_url=$(jq -r '.dispatch.serverUrl // empty' "$HOME/.config/opencode/envoy.json" 2>/dev/null || true)
  [ -n "$prod_dispatch_url" ] || { prod_baseline=none; return 0; }
  header=$(prod_header_file)
  since=$(date -u -d '-24 hours' +%Y-%m-%dT%H:%M:%SZ)
  keys=$(curl -fsS --max-time 20 -H "@$header" "$prod_dispatch_url/api/v1/issues?updated_since=$since" | jq -r '.[:50][].key')
  for key in $keys; do
    id=$(curl -fsS --max-time 20 -H "@$header" "$prod_dispatch_url/api/v1/issues/$key/events?order=desc&limit=1" | jq -r '.[0].id // 0')
    if [ "$id" -gt "$max" ]; then max=$id; fi
  done
  rm -f "$header"
  [ "$max" -gt 0 ] || fail "could not read a production Dispatch baseline event id"
  prod_baseline=$max
}
rig_sessions_json() {
  jq -R -s -c 'split("\n") | map(fromjson? | select(.msg == "api: claim registered") | .session) | unique' "$evidence/logs/daemon.log"
}
production_audit() {
  local sessions header events envoy found=0
  audited=1
  sessions=$(rig_sessions_json)
  printf '%s\n' "$sessions" >"$evidence/rig-sessions.json"
  envoy=$(curl -fsS --max-time 10 "$prod_envoy_url/v1/sessions" |
    jq -c --argjson ids "$sessions" --arg work "$work" '[.[] | select((.session_id as $s | $ids | index($s)) or ((.dir // "") | startswith($work)))]')
  printf '%s\n' "$envoy" >"$evidence/production-envoy-rig-sessions.json"
  if [ "$envoy" != "[]" ]; then printf 'production Envoy listener holds rig sessions: %s\n' "$envoy"; found=1; fi
  if [ "$prod_baseline" = none ]; then printf 'no user-level Dispatch configuration\n'; return "$found"; fi
  header=$(prod_header_file)
  curl -sS -N --max-time 25 -H "@$header" "$prod_dispatch_url/api/v1/events?since=$((prod_baseline - 1))" >"$work/production-events.sse" 2>/dev/null || true
  rm -f "$header"
  events=$(sed -n 's/^data: //p' "$work/production-events.sse" | jq -s -c --argjson ids "$sessions" --arg project "$project" --argjson baseline "$prod_baseline" '
    {control: any(.[]; .id == $baseline), count: length,
     rig: [.[] | select((.actor.id as $a | $ids | index($a)) or (tostring | contains($project))) | {id, issue_key, type, actor: .actor.id, created_at}]}')
  printf '%s\n' "$events" >"$evidence/production-dispatch-audit.json"
  if ! jq -e '.control' >/dev/null <<<"$events"; then printf 'production Dispatch replay did not return its baseline event %s\n' "$prod_baseline"; return 1; fi
  if ! jq -e '.rig == []' >/dev/null <<<"$events"; then printf 'production Dispatch has rig-authored events: %s\n' "$(jq -c .rig <<<"$events")"; found=1; fi
  printf 'production audit: %s events replayed since %s, rig events %s, rig Envoy sessions %s\n' \
    "$(jq -r .count <<<"$events")" "$prod_baseline" "$(jq -c .rig <<<"$events")" "$envoy"
  return "$found"
}

# ---- this acceptance's own vocabulary ---------------------------------------------------------------
claims() { local sub=$1; shift; "$work/legion" claims "$sub" --config "$work/legion.yaml" --operator-token-file "$work/operator-token" "$@"; }
claim_json() { claims list --json | jq -ce --arg t "$1" '.claims[] | select(.token == $t)'; }
claim_is() { claim_json "$1" | jq -e "$2" >/dev/null; }
token_session_file() { claim_json "$1" | jq -er '.sessionFile // empty | select(. != "")'; }
envoy_role_holder() { curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$port_listener/v1/roles/$1" | jq -er .holder; }
role_unheld() { ! envoy_role_holder "$1" >/dev/null 2>&1; }
role_held_by() { [ "$(envoy_role_holder "$1" 2>/dev/null)" = "$2" ]; }
token_session() { claim_json "$1" | jq -er '.session // empty | select(. != "")'; }

# The refusal every rule-bound pane gives `legion handoff complete` (packages/pi-envoy/extensions/
# legion.ts LEGION_HANDOFF_COMPLETE.refusal), and the follow-up the phase stall sends
# (src/legion/phase-stall.ts FOLLOW_UP).
refusal_needle="a phase is completed with the \`legion\` tool's \`handoff_complete\`, never a shell command"
followup_needle="Your turn ended with your Legion phase still open"

# tool_outcomes FILE: every tool result in an OMP session file, joined to the call that produced it.
tool_outcomes() {
  jq -s -c '
    ([.[] | select(.type == "message" and .message.role == "assistant") | .message.content[]?
      | select(.type == "toolCall") | {key: .id, value: {name, arguments}}] | from_entries) as $calls
    | [.[] | select(.type == "message" and .message.role == "toolResult")
        | {id: .message.toolCallId, tool: .message.toolName, isError: .message.isError,
           text: ([.message.content[]? | select(.type == "text") | .text] | join("\n")),
           call: $calls[.message.toolCallId]}]' "$1"
}
refusal_count() { tool_outcomes "$1" | jq --arg n "$refusal_needle" '[.[] | select(.text | contains($n))] | length'; }
refusals_at_least() { local f; f=$($2) || return 1; [ "$(refusal_count "$f")" -ge "$3" ]; }
# read_result FILE: the tool result of the bash call that ran `legion handoff read`.
read_results() {
  tool_outcomes "$1" | jq -c '[.[] | select(.tool == "bash" and ((.call.arguments.command // "") | test("legion handoff read")))]'
}
read_ran() { local f; f=$($1) || return 1; [ "$(read_results "$f" | jq length)" -ge 1 ]; }

# probe_refusals LABEL FILEFN ISSUE PHASE: waits for each of the three refusals in the session FILEFN
# prints, snapshotting the daemon's state after each, and requires ISSUE still in PHASE every time;
# then requires the `legion handoff read` control to have run unrefused.
probe_refusals() {
  local label=$1 filefn=$2 issue=$3 want=$4 n f
  for n in 1 2 3; do
    until_true 900 "$label pane refusal $n" refusals_at_least "$label" "$filefn" "$n"
    daemon_state >"$evidence/$label-after-refusal-$n.json"
    if ! jq -e --arg i "$issue" --arg p "$want" '.issues[$i].phase == $p' "$evidence/$label-after-refusal-$n.json" >/dev/null; then
      soft "$label: after refusal $n, $issue is in $(jq -r --arg i "$issue" '.issues[$i].phase' "$evidence/$label-after-refusal-$n.json"), want $want"
    fi
  done
  f=$($filefn)
  tool_outcomes "$f" | jq --arg n "$refusal_needle" '[.[] | select(.text | contains($n)) | {tool, call: .call.arguments, refusal: .text}]' >"$evidence/$label-refusals.json"
  note "$label refusals ($(jq length "$evidence/$label-refusals.json")), each with $issue still $want:"
  jq -r '.[] | "     [\(.tool)] \((.call.command // .call.code // .call | tostring) | .[0:140]) → \(.refusal | .[0:160])"' "$evidence/$label-refusals.json"
  local shapes
  shapes=$(jq -r '[.[] | if .tool == "eval" then "eval" elif ((.call.command // "") | test("bash -l?c|sh -c")) then "bash-c" else "plain" end] | unique | join(",")' "$evidence/$label-refusals.json")
  [ "$shapes" = "bash-c,eval,plain" ] || soft "$label: refused shapes are '$shapes', want plain, bash -c, and eval"
  until_true 600 "$label: the legion handoff read control to run" read_ran "$filefn"
  read_results "$f" >"$evidence/$label-handoff-read.json"
  if jq -e --arg n "$refusal_needle" 'all(.[]; (.text | contains($n)) | not)' "$evidence/$label-handoff-read.json" >/dev/null; then
    note "$label control: \`legion handoff read\` from bash ran unrefused: $(jq -r '.[0].text | .[0:160] | gsub("\n"; " ")' "$evidence/$label-handoff-read.json")"
  else
    soft "$label: \`legion handoff read\` was refused by the pane rule"
  fi
}

# ---- driving several issues at once ------------------------------------------------------------------
declare -A gate_artifacts pr_of
# gate_send ISSUE records the root's primary artifact and waits for its architect to register the
# gate on its own, from the daemon's catch-up notice (architect_registers_gate); nobody prompts it.
gate_send() {
  local issue=$1
  gate_artifacts[$issue]=$(dispatch_get "issues/$issue" | jq -er .primary_artifact_id)
  architect_registers_gate "$issue" "$issue"
}
gate_finish() {
  local issue=$1 artifact=${gate_artifacts[$1]}
  until_true 600 "$issue architect to register primary artifact $artifact" sh -c \
    "'$work/legion' state --json --port '$port_daemon' | jq -e --arg issue '$issue' --arg artifact '$artifact' '.issues[\$issue].designGate.artifactId == \$artifact and .issues[\$issue].designGate.currentVersion > 0'"
  dispatch_human POST "artifacts/$artifact/reviews" '{"state":"approved"}' >/dev/null
  wait_for_phase "$issue" planning
  wait_for_worker "$issue" planner
}
pr_open_for() { gh -R "$repo" pr list --head "legion/$1" --state open --json number | jq -e 'length == 1' >/dev/null; }
# retarget_pr ISSUE: the proof PR moves to the scratch base, so no merge ever reaches smoke main.
retarget_pr() {
  local issue=$1 n base
  until_true 1200 "$issue implementer pull request" pr_open_for "$issue"
  n=$(gh -R "$repo" pr list --head "legion/$issue" --state open --json number --jq '.[0].number')
  pr_of[$issue]=$n
  base=$(gh -R "$repo" pr view "$n" --json baseRefName --jq .baseRefName)
  [ "$base" = "$scratch_base" ] || gh -R "$repo" pr edit "$n" --base "$scratch_base" >/dev/null
  [ "$(gh -R "$repo" pr view "$n" --json baseRefName --jq .baseRefName)" = "$scratch_base" ] || fail "$repo#$n did not retarget to $scratch_base"
  note "$issue opened $repo#$n; its base read $base here, and is $scratch_base"
}
# reviewer_approved N prints each commit the review App approved on pull request N, and fails when
# it approved none.
reviewer_approved() {
  local approved
  approved=$(gh api --paginate "repos/$repo/pulls/$1/reviews" --jq '.[] | select(.user.login == "legion-reviewer[bot]" and .state == "APPROVED") | .commit_id') || return 1
  [ -n "$approved" ] && printf '%s\n' "$approved"
}

# ---- the instructor: every routine phase worker is instructed the moment its assignment arrives -----
# Phase workers act on the daemon's assignment alone when no instruction reaches them promptly (a
# later root can reach testing while the script still holds an earlier root's planner), so the
# routine roles are not instructed in script order but by this watcher, as soon as each one's
# assignment is in its session: implementers (their pull request opened against the scratch base,
# never main), testers, reviewers, retro, and a hold for each merger until its READY scenario is set
# up. It also retargets any proof pull request whose base is not the scratch base, and logs it.
# Planners and architects are instructed by the script, where each scenario needs them.
assigned_in() { jq -e -s --arg a "Issue: $2. Phase: $3." 'any(.[]; .type == "message" and .message.role == "user" and ([.message.content[]? | .text? // empty] | join(" ") | contains($a)))' "$1" >/dev/null; }
instruction_for() {
  local issue=$1 role=$2 phase=$3
  case "$role:$phase" in
    implementer:implementing) printf '%s' "Acceptance implementation operation: make the smallest one-file change described by this issue in your $repo workspace, commit it on legion/$issue, and open its pull request with base branch $scratch_base (pass --base $scratch_base to legion gh -- pr create; this proof's pull requests never target main). Record the required implementation proof and handoff, then call the legion tool's handoff_complete. Do not merge." ;;
    tester:testing) printf '%s' "Acceptance test operation: inspect the implementer's actual one-file change and this issue's pull request, run a focused observable check, record the required test handoff with verdict pass, then call the legion tool's handoff_complete with verdict pass and a summary that begins 'TESTER-SUMMARY $issue:'." ;;
    reviewer:reviewing) printf '%s' "Acceptance final review: use the bash tool to submit APPROVE on this issue's pull request (head legion/$issue) in $repo at its current head as legion-reviewer[bot], then complete the reviewer handoff. This exact smoke instruction takes precedence over waiting for another review round." ;;
    implementer:retro) printf '%s' "Acceptance retro: write the required retro handoff for this issue's pull request and complete the phase. Do not change the approved implementation." ;;
    merger:merging) printf '%s' "Acceptance hold: this is a proof run. Do not verify, build or send READY yet, and do not call handoff_complete. Reply with the single line 'holding' and wait for the next targeted instruction, which says exactly what to do." ;;
  esac
}
instructor() {
  local issue spec role phase f key tick=0 n st cl cur live
  trap - EXIT ERR
  set +e
  mkdir -p "$evidence/instructed"
  while :; do
    # One state read and one claims read per pass, so the watcher adds two daemon calls every two
    # seconds, not one per issue and role.
    if ! st=$(daemon_state 2>/dev/null) || ! cl=$(claims list --json 2>/dev/null); then sleep 2; continue; fi
    for issue in $(cat "$work/instructor-issues"); do
      cur=$(jq -r --arg i "$issue" '.issues[$i].phase // ""' <<<"$st")
      for spec in implementer:implementing tester:testing reviewer:reviewing implementer:retro merger:merging; do
        role=${spec%%:*} phase=${spec#*:}
        [ "$cur" = "$phase" ] || continue
        live=$(jq -r --arg i "$issue" --arg r "$role" '.issues[$i].workers[$r].claim as $c
          | (($c.session // "") != "" and (($c.state // "") | IN("ready", "working", "idle")))' <<<"$st")
        [ "$live" = true ] || continue
        f=$(jq -r --arg i "$issue" --arg r "$role" '[.claims[] | select(.issue == $i and .role == $r and (.sessionFile // "") != "")] | last | .sessionFile // ""' <<<"$cl")
        [ -n "$f" ] || continue
        key="$evidence/instructed/$issue.$role.$phase.$(basename "$f" .jsonl)"
        [ ! -e "$key" ] || continue
        assigned_in "$f" "$issue" "$phase" || continue
        if send_agent "$issue" "$role" "$(instruction_for "$issue" "$role" "$phase")" >>"$evidence/instructor.log" 2>&1 </dev/null; then
          printf '%s %s %s %s\n' "$(date -u +%FT%T.%3NZ)" "$issue" "$role" "$phase" | tee "$key" >>"$evidence/instructor.log"
        fi
      done
    done
    if [ $((tick++ % 4)) = 0 ]; then
      for n in $(gh -R "$repo" pr list --state open --limit 100 --json number,headRefName,baseRefName \
        --jq ".[] | select((.headRefName | startswith(\"legion/$project-\")) and .baseRefName != \"$scratch_base\" and .baseRefName != \"$merge_base\") | .number" 2>/dev/null); do
        gh -R "$repo" pr edit "$n" --base "$scratch_base" >/dev/null 2>&1 &&
          printf '%s retargeted %s#%s to %s\n' "$(date -u +%FT%T.%3NZ)" "$repo" "$n" "$scratch_base" >>"$evidence/retargets.log"
      done
    fi
    sleep 2
  done
}
phase_rank() {
  case "$1" in
    admitted) echo 0 ;; planning) echo 1 ;; implementing) echo 2 ;; testing) echo 3 ;; reviewing) echo 4 ;;
    retro) echo 5 ;; merging) echo 6 ;; awaiting_merge) echo 7 ;; production_check) echo 8 ;; done) echo 9 ;; *) echo -1 ;;
  esac
}
phase_at_least() { local p; p=$(daemon_state | jq -r --arg i "$1" '.issues[$i].phase') || return 1; [ "$(phase_rank "$p")" -ge "$(phase_rank "$2")" ]; }
wait_for_phase_at_least() { until_true "${3:-600}" "$1 to reach $2 or later" phase_at_least "$1" "$2"; }
instructed() { compgen -G "$evidence/instructed/$1.$2.$3.*" >/dev/null; }
dispatch_messages() { dispatch_events "$1" | jq -c '[.[] | select(.type == "message.created") | {seq, actor: .actor.id, body: .payload.body}]'; }
ready_messages() { dispatch_messages "$1" | jq -c '[.[] | select(.body | startswith("READY"))]'; }
no_holder_messages() { dispatch_messages "$1" | jq -c '[.[] | select(.body | startswith("merge queue role merge-queue had no live holder"))]'; }
merger_summary() {
  local f
  f=$(claim_session_file "$1" merger) || return 1
  jq -s -r '[.[] | select(.type == "message" and .message.role == "assistant") | .message.content[]?
    | select(.type == "toolCall" and .name == "legion" and .arguments.op == "handoff_complete") | .arguments.summary] | last // empty' "$f"
}
# merger_self_posted ISSUE: the merger's own tool calls that would post or publish READY itself.
merger_self_posted() {
  local f
  f=$(claim_session_file "$1" merger) || return 1
  jq -s -c '[.[] | select(.type == "message" and .message.role == "assistant") | .message.content[]?
    | select(.type == "toolCall" and (.name == "dispatch_message" or .name == "envoy_publish" or
        (.name == "write" and ((.arguments.path // "") | test("xd://(dispatch_message|envoy_publish)")))))
    | select(.name == "envoy_publish" or ((.arguments.path // "") | test("envoy_publish")) or
        ((.arguments.body // ((.arguments.content // "{}") | fromjson? // {} | .body) // "") | ltrimstr(" ") | startswith("READY")))
    | {name, arguments}]' "$f"
}
notice_line() { { claim_session_text "$1" "$2" || true; } | grep -F '"customType":"envoy-message"' | grep -F -- "$3" || true; }
notices_at_least() { [ "$(notice_deliveries "$1" "$2" "$3")" -ge "$4" ]; }
# phase_finished_line ISSUE PHASE: the architect's delivered phase-finished notice for PHASE.
phase_finished_line() { notice_line "$1" architect "$(notice_needle phase-finished "$1")" | grep -F -- "phase: $2" | head -1 || true; }
phase_finished_seen() { [ -n "$(phase_finished_line "$1" "$2")" ]; }
ready_count_at_least() { [ "$(ready_messages "$1" | jq length)" -ge "$2" ]; }
no_holder_count_at_least() { [ "$(no_holder_messages "$1" | jq length)" -ge "$2" ]; }
# session_holds FILEFN TEXT: the session FILEFN prints holds TEXT.
session_file_holds() { local f; f=$($1) || return 1; grep -qF -- "$2" "$f"; }
# until_true_quiet SECONDS COMMAND...: until_true that returns 1 on a timeout instead of failing.
until_true_quiet() {
  local limit=$1 i
  shift
  for ((i = 0; i < limit * 2; i++)); do
    [ ! -s "$evidence/pane-endpoint-violation.txt" ] || fail "ABORT: $(cat "$evidence/pane-endpoint-violation.txt")"
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  return 1
}

begin prerequisites
for tool in go psql jq curl ss tmux bun mise secrets gh jj hawk-token; do command -v "$tool" >/dev/null || fail "$tool is required"; done
[ -x "$nats_bin" ] || fail "no native nats-server at $nats_bin"
real_gh=$(mise which gh) || fail "mise has no gh"
require_proof_human
gh api "repos/$repo" --jq .name >/dev/null || fail "the devbox's ordinary gh cannot read $repo"
head_commit=$(jj -R "$root" log -r @- --no-graph -T commit_id)
note "head under test: $head_commit ($(jj -R "$root" log -r @- --no-graph -T 'description.first_line()'))"
[ -z "$(jj -R "$root" diff -r @ --summary)" ] || fail "the working copy at $root carries uncommitted changes"
printf '%s\n' "$head_commit" >"$evidence/head.txt"
mkdir -p "$state" "$work/xdg" "$work/tmux"
chmod 0700 "$state" "$work/xdg" "$work/tmux"
make_omp_home "$omp_home"
key_command=$(bash "$root/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --home "$omp_home" --dest "$evidence/model-gateway" --cache-dir "$work/model-gateway-cache") ||
  fail "the agents' model route through the Hawk model gateway could not be installed"
note "the agents' model route keyed by $key_command"
export XDG_STATE_HOME="$work/xdg"
export TMUX_TMPDIR="$work/tmux"
pass

begin rig
# The bridge dials the production Envoy NATS by the operator's fully-qualified name for it, never a
# bare alias a resolver's search domain would complete (stage3-devbox-workflow.sh's rule and
# pattern). The value is never printed.
upstream_nats=${SMOKE_UPSTREAM_NATS:-}
[ -n "$upstream_nats" ] || fail "SMOKE_UPSTREAM_NATS is unset: the production Envoy NATS the GitHub bridge subscribes on, by its fully-qualified name"
nats_url='^([A-Za-z][A-Za-z0-9+.-]*://)?([^@/?#,[:space:]]+@)?[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)+(:[0-9]+)?/?$'
[[ "$upstream_nats" =~ $nats_url ]] || fail "SMOKE_UPSTREAM_NATS is not one NATS URL naming a fully-qualified host"
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/dispatch-token" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token" &&
  printf 'ci' >"$work/postgres-password")
chmod 0600 "$work"/*token "$work/envoy-auth-header" "$work/postgres-password"
(cd "$root/packages/daemon-go" && go build -ldflags "-X main.revision=$head_commit" -o "$work/legion" ./cmd/legion)
stage_role_prompts "$root" "$work"
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener && go build -o "$work/envoy-dispatch" ./cmd/dispatch)
{
  printf 'head under test %s\n' "$head_commit"
  printf 'legion version: %s\n' "$("$work/legion" version)"
  go version -m "$work/legion" | grep -E 'vcs\.|^\s+mod\s'
  sha256sum "$work/legion" "$work/envoy-listener" "$work/envoy-dispatch" "$nats_bin"
  "$nats_bin" --version
} >"$evidence/binary-stamp.txt"
vcs_rev=$(go version -m "$work/legion" | sed -n 's/^[[:space:]]*build[[:space:]]*vcs\.revision=//p')
[ "$vcs_rev" = "$head_commit" ] || fail "the daemon binary's vcs.revision is '$vcs_rev', want $head_commit"
"$work/legion" version | grep -qF "commit $head_commit" || fail "the daemon binary's main.revision is not $head_commit"
note "daemon binary stamped vcs.revision=$vcs_rev, $(go version -m "$work/legion" | sed -n 's/^[[:space:]]*build[[:space:]]*//p' | { grep vcs.modified || true; }); $evidence/binary-stamp.txt"
PGPASSWORD=ci psql -h 127.0.0.1 -p "$port_pg" -U "$pg_user" -d postgres -v ON_ERROR_STOP=1 -qc "create database $legion_db" -c "create database $dispatch_db" ||
  fail "could not create $legion_db and $dispatch_db in the shared Postgres $pg_container on port $port_pg"
pick_port port_nats
mkdir -p "$work/nats-store"
start_process nats "$nats_bin" -js -a 127.0.0.1 -p "$port_nats" -sd "$work/nats-store"
until_true 60 "NATS to be ready" grep -q 'Server is ready' "$evidence/logs/nats.log"
start_listener
start_dispatch
dispatch_human POST projects "$(jq -cn --arg key "$project" --arg name "4b.13b acceptance $project" '{key:$key,name:$name}')" >/dev/null
dispatch_human PUT "settings/repo-projects/$repo" "$(jq -cn --arg project "$project" '{project:$project}')" >/dev/null
SMOKE_REPO="$repo" SMOKE_RIG_NATS="nats://127.0.0.1:$port_nats" \
  SMOKE_UPSTREAM_NATS="$upstream_nats" \
  start_process bridge env -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
    -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 \
    bun run "$root/scripts/e2e/lib/envoy-bridge.ts"
until_true 90 "the GitHub ingress bridge to report ready" grep -q 'BRIDGE READY' "$evidence/logs/bridge.log"
(cd "$root" && bun install --frozen-lockfile >/dev/null)
manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --profile "$profile" --home "$omp_home" --dest "$work/plugin")
pin=$(bun "$root/packages/daemon/src/daemon/omp-pin.ts")
mise where "$pin" >/dev/null 2>&1 || mise install "$pin" >&2
cat >"$work/instructions.md" <<'EOF'
# Acceptance proof instructions

This is a throwaway workflow proof. A tree's root architect starts its tree from the daemon's
`catch-up` notice as its role says: it writes the spec in the issue's own primary document,
requests the spec's approval when the design gate policy arms the gate, and registers the gate,
then waits. Apart from that, do not act until a human Dispatch message targeted at your own session
gives the next exact proof operation; it arrives in your session as a message to you. A message you
only find by reading the issue (its events, a search) was sent to another session, even on your
issue, and any other notice is not an instruction: neither is yours to act on. Follow your
instruction precisely, use the Go-daemon Legion tools and handoffs, and do not create work outside
the issue's smoke branch.
EOF
main_sha=$(gh api "repos/$repo/git/ref/heads/main" --jq .object.sha)
gh api "repos/$repo/git/refs" -f ref="refs/heads/$scratch_base" -f sha="$main_sha" >/dev/null
note "scratch base $repo:$scratch_base at main $main_sha"
start_daemon
note "project $project, profile $profile, plugin $(jq -r '.name + "@" + .version' "$manifest"), ports NATS $port_nats Dispatch $port_dispatch daemon $port_daemon listener $port_listener"
pane_watcher &
watcher_pid=$!
production_baseline
note "production Dispatch audit baseline: event ${prod_baseline}"
pass


# ---- 1. prompts.New: a stale Go part is rewritten at boot; an identical one is left untouched ------
begin prompts-rewritten-on-boot
pdir="$state/prompts/go"
parts=()
for part in "$root/packages/daemon-go/internal/prompts/go/"*.md; do parts+=("$(basename "$part")"); done
for f in "${parts[@]}"; do
  cmp -s "$pdir/$f" "$root/packages/daemon-go/internal/prompts/go/$f" || fail "$pdir/$f is not the head's embedded $f"
done
note "all ${#parts[@]} Go prompt parts in $pdir equal the head's packages/daemon-go/internal/prompts/go/* (${parts[*]})"
jj -R "$root" file show -r "$base_rev" root:packages/daemon-go/internal/prompts/go/merger.md >"$evidence/merger.base.md"
[ -s "$evidence/merger.base.md" ] || fail "the base revision's merger.md read back empty"
cmp -s "$evidence/merger.base.md" "$pdir/merger.md" && fail "the base merger.md equals the head's; nothing to prove"
stat -c '%n %.9Y' "$pdir"/*.md >"$evidence/prompts-mtime-before.txt"
cp "$evidence/merger.base.md" "$pdir/merger.md"
# negative control: the stale part is observably different before the restart
cmp -s "$pdir/merger.md" "$root/packages/daemon-go/internal/prompts/go/merger.md" && fail "the planted stale merger.md equals the embedded one"
note "planted the base ($base_rev) merger.md: $(grep -c 'publish READY yourself' "$pdir/merger.md" || true) line(s) telling the merger it publishes nothing and to send ready:true, $(wc -c <"$pdir/merger.md") bytes"
sleep 1.1
stop_daemon
start_daemon keep
cmp -s "$pdir/merger.md" "$root/packages/daemon-go/internal/prompts/go/merger.md" || fail "the restarted daemon left the stale merger.md in place"
stat -c '%n %.9Y' "$pdir"/*.md >"$evidence/prompts-mtime-after.txt"
unchanged=$(join <(sort "$evidence/prompts-mtime-before.txt") <(sort "$evidence/prompts-mtime-after.txt") | awk '$2 == $3' | wc -l)
changed=$(join <(sort "$evidence/prompts-mtime-before.txt") <(sort "$evidence/prompts-mtime-after.txt") | awk '$2 != $3 {print $1}')
[ "$unchanged" = "$(( ${#parts[@]} - 1 ))" ] && [ "$(basename "$changed")" = merger.md ] || fail "rewritten parts: '$changed' ($unchanged untouched), want merger.md alone"
note "the restart rewrote merger.md alone back to the embedded text; the $unchanged identical parts kept their mtimes"
pass

# ---- 2. the merge queue holder and the refused root stops -------------------------------------------
begin merge-queue-holder
holder_tree="${project}-900"
# The holder is no workflow issue, so no outbox row provisions its workspace. The tmux runtime needs
# the directory, and a delivery first adopts the working copy for the role's App (jj metaedit), so it
# is a jj repository of its own; the holder never touches the smoke repository.
holder_dir="$state/workspaces/$repo/${holder_tree,,}"
mkdir -p "$holder_dir" && jj git init "$holder_dir" >/dev/null 2>&1 || fail "could not create the holder's jj workspace"
holder=$(claims spawn --json --tree "$holder_tree" --issue "$holder_tree" --role architect \
  --task "Acceptance operation: call your envoy_role_set tool with role merge-queue now, so this session holds the merge-queue role. Then reply with the single line 'holding merge-queue' and wait. Do nothing else." | jq -er .token)
claim_registered() { claim_is "$1" '(.session // "") != ""'; }
until_true 600 "holder claim $holder to register" claim_registered "$holder"
holder_session=$(token_session "$holder")
until_true 600 "the holder session to claim merge-queue" role_held_by merge-queue "$holder_session"
note "holder claim $holder (session $holder_session, a tree no workflow issue backs) holds merge-queue"
pass

begin root-stop-refusal-names-close-only-without-a-workflow-issue
if refusal=$(claims stop --claim "$holder" 2>&1 >/dev/null); then fail "the stop of root claim $holder was accepted"; fi
printf '%s\n' "$refusal" >"$evidence/stop-refusal-holder.txt"
case "$refusal" in
  *"stop refused: the tree's root claim ends only when its tree closes; suspend it to stop its process; no workflow issue backs its tree, so legion claims close ends it"*) note "no-workflow tree: $refusal" ;;
  *) soft "the no-workflow root's stop refusal lacks the close clause: $refusal" ;;
esac
claim_is "$holder" '.state == "ready" or .state == "idle" or .state == "working"' || soft "the refused stop moved $holder"
pass

begin admission
root1=$(new_issue "Primary READY and pane-rule profile")
root2=$(new_issue "Early merge sentinel")
root3=$(new_issue "Gated READY compass")
set_status "$root1" todo
set_status "$root2" todo
set_status "$root3" todo
until_true 240 "three roots admitted" sh -c \
  "'$work/legion' state --json --port '$port_daemon' | jq -e --arg a '$root1' --arg b '$root2' --arg c '$root3' '(.admission.active | sort) == ([\$a,\$b,\$c] | sort)'"
child=$(new_issue "Parked child leaf" "$root1")
set_status "$child" todo
until_true 180 "$child recorded under $root1" sh -c \
  "'$work/legion' state --json --port '$port_daemon' | jq -e --arg c '$child' '.issues[\$c].phase == \"admitted\"'"
note "roots $root1 $root2 $root3; child $child admitted under $root1"
for issue in "$root1" "$root2" "$root3"; do wait_for_worker "$issue" architect; done
printf '%s\n' "$root1" "$root2" "$root3" "$child" >"$work/instructor-issues"
instructor &
instructor_pid=$!
note "the instructor watches $root1 $root2 $root3 $child"
pass

begin root-stop-refusal-with-a-workflow-issue
root1_arch=$(claim_token "$root1" architect)
if refusal=$(claims stop --claim "$root1_arch" 2>&1 >/dev/null); then fail "the stop of root claim $root1_arch was accepted"; fi
printf '%s\n' "$refusal" >"$evidence/stop-refusal-root1.txt"
case "$refusal" in
  *"legion claims close"*) soft "a workflow issue's root stop refusal names legion claims close: $refusal" ;;
  *"stop refused: the tree's root claim ends only when its tree closes; suspend it to stop its process"*) note "workflow tree (negative control): $refusal" ;;
  *) soft "unexpected root stop refusal for $root1: $refusal" ;;
esac
pass

begin gates
for issue in "$root1" "$root2" "$root3"; do gate_send "$issue"; done
root1_architect_file() { claim_session_file "$root1" architect; }
root1_planner_file() { claim_session_file "$root1" planner; }
# assignment_delivered ISSUE ROLE: the daemon's assignment (a user message) reached the worker and
# armed its phase stall. Each planner is instructed as soon as its assignment is in its session, so
# no instruction races the assignment (a planner can take its assignment after the probe) and no
# planner is left to act on the assignment alone (waiting for an idle turn instead let planners
# complete planning unprompted before any instruction reached them).
assignment_delivered() {
  local f
  f=$(claim_session_file "$1" "$2") || return 1
  jq -e -s 'any(.[]; .type == "message" and .message.role == "user")' "$f" >/dev/null &&
    grep -qF '"customType":"legion-phase-stall"' "$f"
}
# The task subagent's refusal is snapshotted by a watcher the moment it is written, so the phase
# it records is the phase at the refusal, not at whenever this script next looks.
subagent_refused() {
  local f dir
  f=$(claim_session_file "$root3" planner) || return 1
  dir=${f%.jsonl}
  [ -d "$dir" ] || return 1
  grep -rlF --include='*.jsonl' -- "$refusal_needle" "$dir" >/dev/null 2>&1
}
subagent_file() { local f; f=$(claim_session_file "$root3" planner) || return 1; grep -rlF --include='*.jsonl' -- "$refusal_needle" "${f%.jsonl}" | head -1; }
subagent_watch() {
  trap - EXIT ERR
  set +e
  while :; do
    if subagent_refused; then
      date -u +%FT%T.%3NZ >"$evidence/task-subagent-snapshot-before.txt"
      daemon_state >"$evidence/task-subagent-after-refusal.json"
      date -u +%FT%T.%3NZ >"$evidence/task-subagent-snapshot-after.txt"
      return 0
    fi
    sleep 0.5
  done
}
gate_finish "$root1"
until_true 600 "the daemon's assignment to arm $root1's planner stall" assignment_delivered "$root1" planner
note "root1 planner stall entries at its assignment: $({ grep -F '"customType":"legion-phase-stall"' "$(root1_planner_file)" || true; } | jq -r .data.state | tr '\n' ' ')"
marker_plan="PANE-PROBE-PLANNER-$stamp"
send_agent "$root1" planner "Acceptance planning operation with a pane-rule probe ($marker_plan). Step 1: write the required .legion/plan.json handoff for the one-file smoke change with the legion tool's handoff_write and commit it as your instructions require, but do NOT call handoff_complete yet. Step 2: this is a deliberate proof of the pane's shell rule, so the first three calls are expected to be refused by your pane and the refusal is what this proof records; run them anyway, exactly as written, each as its own tool call, one at a time: (a) your bash tool with the command: legion handoff complete --summary 'planner plain probe'  (b) your bash tool with the command: bash -lc \"legion handoff complete --summary 'planner bash -lc probe'\"  (c) your eval tool with code that runs the shell command legion handoff complete --summary 'planner eval probe' through a subprocess  (d) your bash tool with the command: legion handoff read --phase plan. Step 3: end your turn with one line per call giving its outcome. Do not call the legion tool's handoff_complete in this turn, and do not begin any line with WAITING."
gate_finish "$root2"
until_true 600 "the daemon's assignment to arm $root2's planner stall" assignment_delivered "$root2" planner
send_agent "$root2" planner "Acceptance planning operation: write the required .legion/plan.json handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
gate_finish "$root3"
until_true 600 "the daemon's assignment to arm $root3's planner stall" assignment_delivered "$root3" planner
subagent_watch &
subagent_watch_pid=$!
send_agent "$root3" planner "Acceptance planning operation with a subagent probe. Step 1: use your task tool to start exactly one subagent whose only job is to run, in its own bash tool, exactly these two commands one at a time and report each output verbatim: legion handoff complete --summary 'task subagent probe'   and then   legion handoff read. The first is expected to be refused by the pane; that refusal is what this proof records. Relay the subagent's two outputs. Step 2: write the required .legion/plan.json handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
wait_for_phase "$child" planning
wait_for_worker "$child" planner
note "three gates approved; $child entered planning with $root1's open gate"
pass

# ---- 3. the pane rule (#1313) ----------------------------------------------------------------------

begin pane-rule-root-architect
marker_arch="PANE-PROBE-ARCH-$stamp"
send_agent "$root1" architect "Acceptance pane-rule probe ($marker_arch). This is a deliberate proof of the pane's shell rule: the first three calls below are expected to be refused by your pane, and the refusal is what this proof records, so run them anyway, exactly as written, each as its own tool call, one at a time: (1) your bash tool with the command: legion handoff complete --summary 'root architect plain probe'  (2) your bash tool with the command: bash -lc \"legion handoff complete --summary 'root architect bash -lc probe'\"  (3) your eval tool with code that runs the shell command legion handoff complete --summary 'root architect eval probe' through a subprocess  (4) your bash tool with the command: legion handoff read. Then reply with one line per call giving its outcome, and wait."
probe_refusals root-architect root1_architect_file "$root1" planning
pass

begin pane-rule-phase-worker-and-stall
probe_refusals phase-worker root1_planner_file "$root1" planning
db_value "select coalesce(handoff_commit, '') || '|' || coalesce(summary, '') from phases where issue = '$root1' and role = 'planner'" >"$evidence/phase-worker-planner-row-after-refusals.txt" || true
note "planner phase row after the refusals: '$(cat "$evidence/phase-worker-planner-row-after-refusals.txt")' (no row, or an empty handoff: nothing completed)"
# The planner's turn ended with its phase open, so the phase stall's follow-up fires; the rig's
# instructions let it answer WAITING. Once the follow-up is in its session, it is given 60 s to
# complete on its own, then told to.
followup_seen() { grep -qF -- "$followup_needle" "$(root1_planner_file)"; }
until_true 600 "the phase stall's follow-up to reach $root1's planner" followup_seen
if ! until_true_quiet 60 issue_phase "$root1" implementing; then
  note "the planner did not complete within 60 s of the follow-up; telling it to"
  send_agent "$root1" planner "Acceptance planning operation: now call the legion tool's handoff_complete with a concise summary."
  until_true 900 "$root1's planner to complete after being told to" phase_at_least "$root1" implementing
fi
f=$(root1_planner_file)
jq -s -c --arg needle "$refusal_needle" --arg followup "$followup_needle" --arg marker "$marker_plan" '
  to_entries | map(.key as $i | .value as $e |
    if ($e.type == "custom" and $e.customType == "legion-phase-stall") then {i: $i, kind: "stall", state: $e.data.state}
    elif ($e.type == "message" and $e.message.role == "toolResult") then
      ([$e.message.content[]? | select(.type == "text") | .text] | join("\n")) as $t
      | if ($t | contains($needle)) then {i: $i, kind: "refusal", tool: $e.message.toolName}
        elif $e.message.toolName == "legion" then {i: $i, kind: "legion-result", isError: $e.message.isError, text: ($t | .[0:240])}
        else empty end
    elif ($e.type == "message" and $e.message.role == "assistant") then
      [$e.message.content[]? | select(.type == "toolCall" and .name == "legion") | .arguments.op] as $ops
      | if ($ops | length) > 0 then {i: $i, kind: "legion-call", ops: $ops} else empty end
    elif (($e | tostring) | contains($followup)) then {i: $i, kind: "followup", type: $e.type, customType: ($e.customType // $e.message.role // null)}
    elif (($e | tostring) | contains($marker)) then {i: $i, kind: "instruction", type: $e.type}
    else empty end)' "$f" >"$evidence/phase-worker-stall-timeline.json"
# The predicate: Oh My Pi appends the re-arming
# `open` for an arriving Envoy message before that message's own entry, so the check is on the last
# stall state before the first refusal, not on an `open` after the instruction.
jq '
  (map(select(.kind == "refusal"))) as $refs
  | ($refs | first | .i) as $firstref | ($refs | last | .i) as $lastref
  | ([.[] | select(.kind == "stall" and .i < $firstref)] | last | .state) as $stall_at
  | ([.[] | select(.kind == "followup" and .i > $lastref)] | first | .i) as $follow
  | ([.[] | select(.kind == "legion-call" and (.ops | index("handoff_complete")) and .i > $follow)] | first | .i) as $call
  | ([.[] | select(.kind == "legion-result" and .isError == false and .i > $call)] | first | .i) as $done
  | {stall_at_refusals: $stall_at, refusals: [$refs[].i], follow: $follow, call: $call, done: $done,
     closed_after_call: any(.[]; .kind == "stall" and .state == "closed" and .i > $call),
     followups_after_done: (if $done == null then null else [.[] | select(.kind == "followup" and .i > $done)] | length end)}' \
  "$evidence/phase-worker-stall-timeline.json" >"$evidence/phase-worker-stall-verdict.json"
if jq -e '.stall_at_refusals == "open" and (.refusals | length) >= 3 and .follow != null and .call != null and .done != null
    and .closed_after_call and .followups_after_done == 0' "$evidence/phase-worker-stall-verdict.json" >/dev/null; then
  note "stall: open at the refusals → 3 refusals → the follow-up → the tool's handoff_complete → closed; no follow-up after it: $(jq -c . "$evidence/phase-worker-stall-verdict.json")"
else
  soft "the planner's stall timeline fails the corrected predicate: $(jq -c . "$evidence/phase-worker-stall-verdict.json")"
fi
state_file phase-worker-after-tool-complete
pass

begin pane-rule-task-subagent
until_true 900 "the snapshot at $root3's task subagent refusal" test -s "$evidence/task-subagent-snapshot-after.txt"
subagent_phase=$(jq -r --arg i "$root3" '.issues[$i].phase' "$evidence/task-subagent-after-refusal.json")
[ "$subagent_phase" = planning ] || soft "$root3 was $subagent_phase in the snapshot at its subagent's refusal, want planning"
until_true 300 "$root3's task subagent to run legion handoff read" read_ran subagent_file
subfile=$(subagent_file)
tool_outcomes "$subfile" >"$evidence/task-subagent-outcomes.json"
cp "$subfile" "$evidence/task-subagent-session.jsonl"
note "subagent session $subfile: $(jq -r --arg n "$refusal_needle" '[.[] | select(.text | contains($n))][0] | "[\(.tool)] \(.call.arguments.command // "") → \(.text | .[0:140])"' "$evidence/task-subagent-outcomes.json")"
if jq -e --arg n "$refusal_needle" 'any(.[]; .tool == "bash" and ((.call.arguments.command // "") | test("legion handoff read")) and ((.text | contains($n)) | not))' "$evidence/task-subagent-outcomes.json" >/dev/null; then
  note "subagent control: legion handoff read ran unrefused in the same subagent"
else
  soft "the task subagent's legion handoff read did not run unrefused"
fi
wait_for_phase_at_least "$root2" implementing 900
wait_for_phase_at_least "$root3" implementing 900
# The refusal's own session timestamp, the snapshot window, and the planner's later completion.
jq -n --arg refusal_at "$(jq -r -s --arg n "$refusal_needle" '[.[] | select(.type == "message" and .message.role == "toolResult"
      and (([.message.content[]? | select(.type == "text") | .text] | join("\n")) | contains($n))) | .timestamp] | first' "$subfile")" \
  --arg before "$(cat "$evidence/task-subagent-snapshot-before.txt")" --arg after "$(cat "$evidence/task-subagent-snapshot-after.txt")" \
  --arg phase "$subagent_phase" \
  --arg complete_at "$(jq -r -s '[.[] | select(.type == "message" and .message.role == "assistant" and any(.message.content[]?; .type == "toolCall"
      and .name == "legion" and .arguments.op == "handoff_complete")) | .timestamp] | first // "none"' "$(claim_session_file "$root3" planner)")" \
  '{refusal_written_at: $refusal_at, snapshot_started_at: $before, snapshot_finished_at: $after, phase_in_snapshot: $phase,
    planner_handoff_complete_called_at: $complete_at}' >"$evidence/task-subagent-timing.json"
note "task subagent timing: $(jq -c . "$evidence/task-subagent-timing.json")"
pass

# ---- 4. park_child / rerun_child ----------------------------------------------------------------------
begin park-child
child_planner_before=$(daemon_state | jq -c --arg c "$child" '.issues[$c] | {generation, phase, planner: .workers.planner.claim.state}')
send_agent "$root1" architect "Acceptance child operations, one legion tool call at a time, reporting each result verbatim before the next. (1) Call park_child with issue $root1. (2) Call rerun_child with issue $child. (3) Now take child $child out of the workflow, with the legion operation your instructions name for that. Then wait."
until_true 600 "$child to be backlog in Dispatch" dispatch_status_is "$child" backlog
until_true 300 "$child to leave the workflow" issue_phase "$child" "done"
until_true 300 "$child's planner to be suspended" issue_worker_state "$child" planner suspended
until_true 300 "the child-status notice for $child" notice_delivered "$root1" architect "$(notice_needle child-status "$child")"
state_file park-child
dispatch_events "$child" >"$evidence/child-events-after-park.json"
f=$(root1_architect_file)
tool_outcomes "$f" | jq -c '[.[] | select(.tool == "legion" and (.call.arguments.op | IN("park_child", "rerun_child")))]' >"$evidence/architect-child-ops.json"
jq -e --arg r "$root1" 'any(.[]; .call.arguments.op == "park_child" and .call.arguments.issue == $r and .isError and (.text | contains("CHILD_REQUIRED") or contains("tree root")))' "$evidence/architect-child-ops.json" >/dev/null ||
  soft "park_child on the root was not refused as CHILD_REQUIRED: $(cat "$evidence/architect-child-ops.json")"
jq -e --arg c "$child" 'any(.[]; .call.arguments.op == "rerun_child" and .call.arguments.issue == $c and .isError and (.text | contains("CHILD_RUNNING") or contains("park_child takes it out")))' "$evidence/architect-child-ops.json" >/dev/null ||
  soft "rerun_child on the running child was not refused as CHILD_RUNNING: $(cat "$evidence/architect-child-ops.json")"
jq -e --arg c "$child" 'any(.[]; .call.arguments.op == "park_child" and .call.arguments.issue == $c and (.isError | not))' "$evidence/architect-child-ops.json" >/dev/null ||
  soft "the architect did not choose park_child for 'take the child out of the workflow'"
park_actor=$(jq -r '[.[] | select(.type == "issue.updated" and .payload.status == "backlog")] | last | .actor.id' "$evidence/child-events-after-park.json")
[ "$park_actor" = "legion-daemon:$project" ] || soft "the backlog write's actor is $park_actor, want legion-daemon:$project"
note "architect refusals: $(jq -r '[.[] | select(.isError) | "\(.call.arguments.op) \(.call.arguments.issue): \(.text | .[0:110])"] | join(" | ")' "$evidence/architect-child-ops.json")"
note "park: $child backlog in Dispatch by $park_actor, phase done, planner suspended (before: $child_planner_before); child-status reached $root1's architect"
sleep 20
dispatch_status_is "$child" backlog >/dev/null || soft "$child's backlog was undone"
pass

begin rerun-child
gen_before=$(daemon_state | jq -r --arg c "$child" '.issues[$c].generation')
send_agent "$root1" architect "Acceptance child operation: now run child $child again from planning, with the legion operation your instructions name for that. Report the result verbatim, then wait."
until_true 600 "$child to be todo in Dispatch" dispatch_status_is "$child" todo
until_true 300 "$child to run again from planning" issue_phase "$child" planning
until_true 300 "$child's planner live again" issue_worker_live "$child" planner
gen_after=$(daemon_state | jq -r --arg c "$child" '.issues[$c].generation')
[ "$gen_after" -gt "$gen_before" ] || soft "$child's generation did not advance on rerun ($gen_before → $gen_after)"
until_true 300 "the second child-status notice for $child" notices_at_least "$root1" architect "$(notice_needle child-status "$child")" 2
dispatch_events "$child" >"$evidence/child-events-after-rerun.json"
rerun_actor=$(jq -r '[.[] | select(.type == "issue.updated" and .payload.status == "todo")] | last | .actor.id' "$evidence/child-events-after-rerun.json")
[ "$rerun_actor" = "legion-daemon:$project" ] || soft "the todo write's actor is $rerun_actor, want legion-daemon:$project"
f=$(root1_architect_file)
tool_outcomes "$f" | jq -c '[.[] | select(.tool == "legion" and (.call.arguments.op | IN("park_child", "rerun_child")))]' >"$evidence/architect-child-ops.json"
jq -e --arg c "$child" '[.[] | select(.call.arguments.op == "rerun_child" and .call.arguments.issue == $c and (.isError | not))] | length >= 1' "$evidence/architect-child-ops.json" >/dev/null ||
  soft "the architect did not choose rerun_child for 'run the child again'"
state_file rerun-child
note "rerun: $child todo by $rerun_actor, generation $gen_before → $gen_after, planning with its planner live; child-status notices on $root1's architect: $(notice_deliveries "$root1" architect "$(notice_needle child-status "$child")")"
pass

begin rerun-a-closed-child
child2=$(new_issue "Closed child leaf" "$root1")
printf '%s\n' "$child2" >>"$work/instructor-issues"
set_status "$child2" todo
wait_for_phase "$child2" planning
wait_for_worker "$child2" planner
# The proof human closes the child: the workflow's leave parks it as a sign-off does (phase done,
# Dispatch status done), and rerun_child must reopen a closed Dispatch issue.
set_status "$child2" "done"
until_true 300 "$child2 to leave the workflow" issue_phase "$child2" "done"
until_true 300 "the child-closed notice for $child2" notice_delivered "$root1" architect "$(notice_needle child-closed "$child2")"
gen_before=$(daemon_state | jq -r --arg c "$child2" '.issues[$c].generation')
send_agent "$root1" architect "Acceptance child operation: child $child2 is closed (done). Run it again from planning, with the legion operation your instructions name for that. Report the result verbatim, then wait."
until_true 600 "$child2 to be todo in Dispatch" dispatch_status_is "$child2" todo
until_true 300 "$child2 to run again from planning" issue_phase "$child2" planning
until_true 300 "$child2's planner live again" issue_worker_live "$child2" planner
gen_after=$(daemon_state | jq -r --arg c "$child2" '.issues[$c].generation')
[ "$gen_after" -gt "$gen_before" ] || soft "$child2's generation did not advance on rerun ($gen_before → $gen_after)"
dispatch_events "$child2" >"$evidence/closed-child-events.json"
rerun_actor=$(jq -r '[.[] | select(.type == "issue.updated" and .payload.status == "todo")] | last | .actor.id' "$evidence/closed-child-events.json")
[ "$rerun_actor" = "legion-daemon:$project" ] || soft "the reopening todo write's actor is $rerun_actor, want legion-daemon:$project"
f=$(root1_architect_file)
tool_outcomes "$f" | jq -c '[.[] | select(.tool == "legion" and (.call.arguments.op | IN("park_child", "rerun_child")))]' >"$evidence/architect-child-ops-closed.json"
jq -e --arg c "$child2" 'any(.[]; .call.arguments.op == "rerun_child" and .call.arguments.issue == $c and (.isError | not))' "$evidence/architect-child-ops-closed.json" >/dev/null ||
  soft "the architect did not rerun the closed child with rerun_child"
state_file closed-child-rerun
note "closed $child2 (done) → child-closed → the architect's rerun_child reopened it: todo by $rerun_actor, generation $gen_before → $gen_after, planning with its planner live"
set_status "$child2" backlog
note "the proof human parked $child2 (backlog) so it takes no further turns"
pass

# ---- 5. implementation, the closed-unmerged PR, testing ------------------------------------------------
begin implementers
# The instructor tells each implementer, on its assignment, to open its pull request against the
# scratch base; this section asserts what happened, in whatever order the agents did it.
if issue_phase "$child" planning >/dev/null 2>&1; then
  send_agent "$child" planner "Acceptance planning operation: write the required .legion/plan.json handoff for a one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
fi
for issue in "$root1" "$root2" "$root3"; do retarget_pr "$issue"; done
wait_for_phase_at_least "$child" implementing 900
for issue in "$root1" "$root2" "$root3"; do wait_for_phase_at_least "$issue" testing 1500; done
note "instructed so far: $(cut -d' ' -f2- "$evidence"/instructed/* 2>/dev/null | tr '\n' ';')"
note "proof pull requests the watcher found on a base other than $scratch_base: $(cat "$evidence/retargets.log" 2>/dev/null | grep -c retargeted || true)"
pass

begin pr-closed-unmerged
retarget_pr "$child"
gh -R "$repo" pr close "${pr_of[$child]}" >/dev/null
until_true 300 "the pr-closed-unmerged notice for $child on $root1's architect" notice_delivered "$root1" architect "$(notice_needle pr-closed-unmerged "$child")"
sleep 30
n=$(notice_deliveries "$root1" architect "$(notice_needle pr-closed-unmerged "$child")")
[ "$n" = 1 ] || soft "$root1's architect received $n pr-closed-unmerged notices for $child, want 1"
notice_line "$root1" architect "$(notice_needle pr-closed-unmerged "$child")" | head -1 >"$evidence/notice-pr-closed-unmerged.jsonl"
note "closed $repo#${pr_of[$child]} unmerged; $root1's architect got pr-closed-unmerged for $child $n time(s)"
set_status "$child" backlog
note "the proof human parked $child (backlog) so it takes no further turns"
pass

begin testers
for issue in "$root1" "$root2" "$root3"; do wait_for_phase_at_least "$issue" reviewing 1200; done
pass

begin phase-finished-carries-summary-and-verdict
until_true 300 "the tester's phase-finished notice on $root1's architect" phase_finished_seen "$root1" testing
tester_line=$(phase_finished_line "$root1" testing)
planner_line=$(phase_finished_line "$root1" planning)
printf '%s\n' "$tester_line" >"$evidence/notice-phase-finished-tester.jsonl"
printf '%s\n' "$planner_line" >"$evidence/notice-phase-finished-planner.jsonl"
grep -qF 'verdict: pass' <<<"$tester_line" || soft "the tester's phase-finished notice carries no 'verdict: pass'"
tester_own=$(jq -s -r '[.[] | select(.type == "message" and .message.role == "assistant") | .message.content[]? | select(.type == "toolCall" and .name == "legion" and .arguments.op == "handoff_complete") | .arguments.summary] | last // ""' "$(claim_session_file "$root1" tester)" | head -1 | cut -c1-60)
printf '%s\n' "$tester_own" >"$evidence/tester-own-summary-head-$root1.txt"
{ [ -n "$tester_own" ] && grep -qF -- "$tester_own" <<<"$tester_line"; } || soft "the tester's phase-finished notice does not carry the summary the tester sent ('$tester_own')"
instructed "$root1" tester testing || note "$root1's tester was not instructed before it completed; its summary is its own"
[ -n "$planner_line" ] || soft "no planning phase-finished notice reached $root1's architect"
grep -qF 'verdict:' <<<"$planner_line" && soft "the planner's phase-finished notice carries a verdict"
note "tester notice: $({ grep -o 'summary: \\"TESTER-SUMMARY[^\\]*' <<<"$tester_line" || true; } | head -1) / $(grep -o 'verdict: [a-z]*' <<<"$tester_line" || true)"
note "planner notice (control): verdict line absent: $(grep -c 'verdict:' <<<"$planner_line" || true)"
pass

begin reviewers
# A review ends when its reviewer completes it: the reviewer approves the head it has, then commits
# and pushes its handoff, so the pull request's head moves past the approved commit before the issue
# leaves reviewing, and the retro's handoff moves it again. Whether that approval still stands for
# the head the round ends on is the daemon's judgement (classify.ApprovalStands), recorded on the
# round as its decision; the proof checks the review App approved and the daemon ended the round on
# that approval.
reviewed_through() {
  reviewer_approved "${pr_of[$1]}" >/dev/null && phase_at_least "$1" retro &&
    daemon_state | jq -e --arg i "$1" '.issues[$i].pullRequest.reviewDecision == "approved"' >/dev/null
}
for issue in "$root1" "$root2" "$root3"; do
  until_true 900 "$repo#${pr_of[$issue]} approved by legion-reviewer[bot], and $issue past reviewing on that approval" reviewed_through "$issue"
  note "$issue: legion-reviewer[bot] approved $({ reviewer_approved "${pr_of[$issue]}" || true; } | cut -c1-8 | tr '\n' ' '); the head is now $(gh api "repos/$repo/pulls/${pr_of[$issue]}" --jq .head.sha | cut -c1-8)"
done
pass

begin retros
# The instructor holds each merger at its assignment; the READY scenarios below need all three in
# merging, held.
merging_and_held() { issue_phase "$1" merging && instructed "$1" merger merging; }
for issue in "$root1" "$root2" "$root3"; do until_true 1800 "$issue in merging with its merger held" merging_and_held "$issue"; done
note "each merger held at its assignment: $({ grep ' merger merging' "$evidence/instructor.log" || true; } | tr '\n' ';')"
pass

# ---- 6. an early merge: pr-merged, and no READY for a merged PR ----------------------------------------
begin early-merge-skips-ready
gh -R "$repo" pr merge "${pr_of[$root2]}" --squash --delete-branch
until_true 300 "the pr-merged notice on $root2's architect" notice_delivered "$root2" architect "$(notice_needle pr-merged "$root2")"
issue_phase "$root2" merging >/dev/null || soft "$root2 left merging on the early merge"
notice_line "$root2" architect "$(notice_needle pr-merged "$root2")" | head -1 >"$evidence/notice-pr-merged.jsonl"
send_agent "$root2" merger "Acceptance READY operation: the proof human has already merged pull request #${pr_of[$root2]}. Build your READY packet exactly as your instructions describe anyway and end this phase with the legion tool: op handoff_complete, ready true, and the packet as the summary. Then wait."
wait_for_phase "$root2" production_check 600
sleep 30
ready_messages "$root2" >"$evidence/ready-messages-$root2.json"
[ "$(jq length "$evidence/ready-messages-$root2.json")" = 0 ] || soft "a READY was posted for $root2 whose PR had merged: $(cat "$evidence/ready-messages-$root2.json")"
n=$(notice_deliveries "$root2" architect "$(notice_needle pr-merged "$root2")")
c=$(notice_deliveries "$root2" architect "$(notice_needle pr-closed-unmerged "$root2")")
[ "$n" = 1 ] || soft "$root2's architect got $n pr-merged notices, want 1"
[ "$c" = 0 ] || soft "$root2's merged PR also produced $c pr-closed-unmerged notices"
note "merged $repo#${pr_of[$root2]} in merging: pr-merged $n, pr-closed-unmerged $c (control); the merger's READY moved $root2 straight to production_check with $(jq length "$evidence/ready-messages-$root2.json") READY messages posted"
pass

# ---- 7a. the READY cap at its exact boundary, through the daemon's own completion route ------------
# utf16_len: stdin's length in UTF-16 code units, as Dispatch's len16 and MessageBodyLength count it.
utf16_len() { python3 -c 'import sys; print(len(sys.stdin.read().encode("utf-16-le")) // 2)'; }
cap_refusals() { tool_outcomes "$1" | jq -c '[.[] | select(.tool == "legion" and (.text | contains("READY_PACKET_TOO_LONG")))]'; }
grant_mtime() { stat -c %.9Y "$1" 2>/dev/null || printf 'none\n'; }
grant_changed() { [ "$(grant_mtime "$1")" != "$2" ]; }
begin ready-cap-refused-at-the-boundary
merger_omp=$(omp_descendant "$(claim_pane_pid "$root1" merger)") || fail "$root1's merger pane has no OMP process"
# Under the Go daemon the pane's exec environment names no LEGION_GRANT_FILE: the plugin sets it in
# its own process.env after registration (pi-envoy src/legion/go-bootstrap.ts), as
# <LEGION_STATE_DIR>/secrets/<claim token>-grant, which /proc/<pid>/environ never shows.
merger_state=$(pane_value "$merger_omp" LEGION_STATE_DIR)
merger_ws=$(readlink "/proc/$merger_omp/cwd")
[ -n "$merger_state" ] || fail "$root1's merger pane names no LEGION_STATE_DIR"
grant_file="$merger_state/secrets/$(claim_token "$root1" merger)-grant"
note "the merger's grant file, as the plugin derives it: $grant_file"
before=$(grant_mtime "$grant_file")
send_agent "$root1" merger "Acceptance proof step: run exactly one bash command, legion state --json > /dev/null && echo state-read , then reply with the single line 'state read' and wait for the next instruction. Do not call handoff_complete yet."
# The pane's bash hook mints a sixty-second grant into LEGION_GRANT_FILE before the command runs;
# the completion below redeems it as the pane's own `legion` tool does, with the pane's environment.
until_true 600 "$root1's merger bash hook to write a fresh grant" grant_changed "$grant_file" "$before"
# record.MessagePostLimit: Dispatch's 2,000 units less the separator and the outbox marker at its longest.
over=$(printf '\U0001F600'; printf 'x%.0s' $(seq 1 1955))
[ "$(printf '%s' "$over" | utf16_len)" = 1957 ] || fail "the boundary packet is not 1957 UTF-16 units"
mapfile -d '' -t pane_env <"/proc/$merger_omp/environ"
cap_exit=0
env -i "${pane_env[@]}" LEGION_GRANT_FILE="$grant_file" "$work/legion" handoff complete --ready --summary "$over" --workspace "$merger_ws" \
  >"$evidence/ready-cap-boundary.stdout" 2>"$evidence/ready-cap-boundary.stderr" || cap_exit=$?
printf 'exit %s\n' "$cap_exit" >"$evidence/ready-cap-boundary.exit"
note "boundary packet (1 emoji + 1955 x = 1957 UTF-16 units, $(printf '%s' "$over" | wc -m) code points, $(printf '%s' "$over" | wc -c) bytes) → exit $cap_exit: $(cat "$evidence/ready-cap-boundary.stderr")"
[ "$cap_exit" != 0 ] || soft "the 1957-unit READY packet was accepted"
grep -qF 'READY_PACKET_TOO_LONG' "$evidence/ready-cap-boundary.stderr" && grep -qF '(1957/1956)' "$evidence/ready-cap-boundary.stderr" ||
  soft "the 1957-unit READY packet was not refused as READY_PACKET_TOO_LONG naming 1957/1956"
state_file ready-cap-after-boundary-refusal
issue_phase "$root1" merging >/dev/null || soft "$root1 left merging on the refused boundary packet"
[ "$(ready_messages "$root1" | jq length)" = 0 ] || soft "a READY message was posted for the refused boundary packet"
db_value "select coalesce(summary, '') from phases where issue = '$root1' and role = 'merger'" >"$evidence/ready-cap-merger-row-after-refusal.txt"
[ ! -s "$evidence/ready-cap-merger-row-after-refusal.txt" ] || [ "$(tr -d '\n' <"$evidence/ready-cap-merger-row-after-refusal.txt")" = "" ] ||
  soft "the refused boundary packet was stored on $root1's merger row"
note "after the refusal: $root1 $(jq -r --arg i "$root1" '.issues[$i].phase' "$evidence/ready-cap-after-boundary-refusal.json"), READY messages $(ready_messages "$root1" | jq length), merger row summary $(wc -c <"$evidence/ready-cap-merger-row-after-refusal.txt") bytes"
pass

# ---- 7. the daemon posts READY (direct) and publishes it to a live holder -----------------------------
begin ready-posted-and-published-to-holder
# The holder claimed merge-queue an hour of agent turns ago. Losing it is recorded and repaired
# rather than ending the run, so the sections after this one are still measured.
if ! role_held_by merge-queue "$holder_session"; then
  soft "the holder no longer held merge-queue at the READY section (holder: $(envoy_role_holder merge-queue 2>/dev/null || echo none))"
  claims deliver --claim "$holder" --task "Acceptance operation: call your envoy_role_set tool with role merge-queue now, then reply 'holding merge-queue' and wait." >/dev/null
  until_true 300 "the holder session to claim merge-queue again" role_held_by merge-queue "$holder_session"
fi
[ "$(ready_messages "$root1" | jq length)" = 0 ] || soft "$root1 already had a READY message before its merger's completion"
send_agent "$root1" merger "Acceptance READY operation: verify the merge gate as your instructions describe. Then, as a deliberate proof of the READY packet's length limit, first end the phase with the legion tool (op handoff_complete, ready true) using as the summary your READY packet followed by the pull request body quoted in full and then a padding paragraph of at least 2,500 characters; that first call is expected to be refused. Then do exactly what the refusal tells you, and end this phase with the legion tool: op handoff_complete, ready true, and a packet built exactly as your instructions describe as the summary. Then wait."
wait_for_phase "$root1" awaiting_merge 900
until_true 180 "the daemon's READY message on $root1" ready_count_at_least "$root1" 1
sleep 20
ready_messages "$root1" >"$evidence/ready-messages-$root1.json"
packet=$(merger_summary "$root1")
printf '%s\n' "$packet" >"$evidence/merger-summary-$root1.txt"
[ "$(jq length "$evidence/ready-messages-$root1.json")" = 1 ] || soft "$root1 has $(jq length "$evidence/ready-messages-$root1.json") READY messages, want 1"
jq -e --arg p "$packet" --arg d "legion-daemon:$project" '(.[0].body | sub("\n\n<!-- legion-outbox:[0-9]+ -->$"; "")) == $p and .[0].actor == $d' "$evidence/ready-messages-$root1.json" >/dev/null ||
  soft "$root1's READY message is not the merger's packet verbatim by $project's daemon: $(jq -c '.[0] | {actor, body: (.body | .[0:120])}' "$evidence/ready-messages-$root1.json") vs summary '${packet:0:120}'"
merger_self_posted "$root1" >"$evidence/merger-self-posts-$root1.json"
[ "$(jq length "$evidence/merger-self-posts-$root1.json")" = 0 ] || soft "$root1's merger posted or published READY itself: $(cat "$evidence/merger-self-posts-$root1.json")"
holder_file() { token_session_file "$holder"; }
first_line=$(head -1 <<<"$packet")
until_true 180 "the merge-queue holder to receive $root1's READY" session_file_holds holder_file "$first_line"
grep -F '"customType":"envoy-message"' "$(holder_file)" | grep -F -- "$first_line" | head -1 >"$evidence/holder-ready-delivery.jsonl"
[ -s "$evidence/holder-ready-delivery.jsonl" ] || soft "the holder saw the READY line, but not as an Envoy delivery"
[ "$(no_holder_messages "$root1" | jq length)" = 0 ] || soft "$root1 was told merge-queue had no holder while one held it"
note "READY on $root1 by $(jq -r '.[0].actor' "$evidence/ready-messages-$root1.json"): '$first_line' — the merger's summary verbatim; the merger itself posted/published nothing; the holder session received it as an Envoy delivery"
f=$(claim_session_file "$root1" merger)
cap_refusals "$f" >"$evidence/ready-cap-merger-refusals-$root1.json"
posted_len=$(jq -j '.[0].body' "$evidence/ready-messages-$root1.json" | utf16_len)
note "the merger's own over-cap attempt: $(jq length "$evidence/ready-cap-merger-refusals-$root1.json") READY_PACKET_TOO_LONG refusal(s) ($(jq -r '[.[].text | .[0:150]] | join(" | ")' "$evidence/ready-cap-merger-refusals-$root1.json")); the posted packet is $posted_len UTF-16 units"
[ "$posted_len" -le 1956 ] || soft "$root1's posted READY is $posted_len units, over record.MessagePostLimit (1956)"
# The tool's path through the READY cap, as measured: the census's claim that both completion
# callers met the refusal live rests on this count, so it is a file, not only a log line.
jq -n --arg issue "$root1" --argjson refusals "$(jq length "$evidence/ready-cap-merger-refusals-$root1.json")" --argjson posted "$posted_len" \
  '{issue: $issue, tool_refusals: $refusals, posted_utf16: $posted}' >"$evidence/ready-cap-tool-path-$root1.json"
pass

begin merge-at-awaiting-merge-gives-no-pr-merged
# The early-merge section merged $root2's pull request into the scratch base, and every proof pull
# request carries its phase handoffs under .legion/, so $root1's no longer merges there cleanly. It
# merges into a base of its own, cut from the same main commit.
gh api "repos/$repo/git/refs" -f ref="refs/heads/$merge_base" -f sha="$main_sha" >/dev/null
gh -R "$repo" pr edit "${pr_of[$root1]}" --base "$merge_base" >/dev/null
note "$repo#${pr_of[$root1]} retargeted to its own base $merge_base at main $main_sha"
gh -R "$repo" pr merge "${pr_of[$root1]}" --squash --delete-branch
wait_for_phase "$root1" production_check 600
sleep 60
n=$(notice_deliveries "$root1" architect "$(notice_needle pr-merged "$root1")")
[ "$n" = 0 ] || soft "a merge at awaiting_merge still told $root1's architect pr-merged ($n)"
note "merged $repo#${pr_of[$root1]} at awaiting_merge: $root1 → production_check, pr-merged notices $n (negative control)"
pass

# ---- 8. READY stored across a refused gate, posted at the approval, with no merge queue holder ------
begin gated-ready-posted-with-no-holder
claims deliver --claim "$holder" --task "Acceptance operation: call your envoy_role_set tool with role mq-released-$ptoken now, then reply 'released' and wait." >/dev/null
until_true 300 "merge-queue to have no holder" role_unheld merge-queue
note "the holder released merge-queue (GET /v1/roles/merge-queue: no holder)"
dispatch_human POST "issues/$root3/artifacts" \
  "$(jq -cn --arg name spec.md --arg content "# Acceptance smoke\n\nThe human revised this proof specification before READY.\n" '{name:$name,content:$content,summary:"4b.13b gate re-close"}')" >/dev/null
until_true 180 "the new primary spec version to close $root3's gate" sh -c \
  "'$work/legion' state --json --port '$port_daemon' | jq -e --arg issue '$root3' '.issues[\$issue].designGate.currentVersion > .issues[\$issue].designGate.approvedVersion'"
send_agent "$root3" merger "Acceptance READY operation: verify the merge gate as your instructions describe, build your READY packet exactly as they describe, and end this phase with the legion tool: op handoff_complete, ready true, and the packet as the summary. If it is refused, record its exact refusal and wait; do not retry it."
until_true 600 "$root3's merger to observe the READY refusal" session_contains "$root3" merger "READY refused: approve design version"
sleep 20
ready_messages "$root3" >"$evidence/ready-messages-$root3-while-refused.json"
[ "$(jq length "$evidence/ready-messages-$root3-while-refused.json")" = 0 ] || soft "a READY was posted for $root3 while its gate refused it"
db_value "select summary from phases where issue = '$root3' and role = 'merger'" >"$evidence/merger-stored-summary-$root3.txt"
[ -s "$evidence/merger-stored-summary-$root3.txt" ] || soft "$root3's refused READY packet is not stored on the merger's phase row"
issue_phase "$root3" merging >/dev/null || soft "$root3 left merging on a refused READY"
note "READY refused at the new spec version; no READY message posted (negative control); packet stored on the merger's row ($(wc -c <"$evidence/merger-stored-summary-$root3.txt") bytes)"
artifact3=${gate_artifacts[$root3]}
dispatch_human POST "artifacts/$artifact3/reviews" '{"state":"approved"}' >/dev/null
wait_for_phase "$root3" awaiting_merge 600
until_true 180 "the daemon's READY message on $root3" ready_count_at_least "$root3" 1
until_true 180 "the no-holder message on $root3" no_holder_count_at_least "$root3" 1
sleep 20
ready_messages "$root3" >"$evidence/ready-messages-$root3.json"
no_holder_messages "$root3" >"$evidence/no-holder-messages-$root3.json"
packet3=$(merger_summary "$root3")
printf '%s\n' "$packet3" >"$evidence/merger-summary-$root3.txt"
jq -e --arg p "$packet3" --arg d "legion-daemon:$project" 'length == 1 and (.[0].body | sub("\n\n<!-- legion-outbox:[0-9]+ -->$"; "")) == $p and .[0].actor == $d' "$evidence/ready-messages-$root3.json" >/dev/null ||
  soft "$root3's READY after approval is not exactly one daemon message holding the stored packet: $(jq -c '[.[] | {actor, body: (.body | .[0:100])}]' "$evidence/ready-messages-$root3.json")"
jq -e --arg d "legion-daemon:$project" 'length == 1 and .[0].actor == $d' "$evidence/no-holder-messages-$root3.json" >/dev/null ||
  soft "$root3 did not get exactly one daemon no-holder message: $(cat "$evidence/no-holder-messages-$root3.json")"
merger_self_posted "$root3" >"$evidence/merger-self-posts-$root3.json"
[ "$(jq length "$evidence/merger-self-posts-$root3.json")" = 0 ] || soft "$root3's merger posted or published READY itself"
note "approval advanced $root3 to awaiting_merge and the daemon posted the stored packet: '$(head -1 <<<"$packet3")'; then: '$(jq -r '.[0].body' "$evidence/no-holder-messages-$root3.json")'"
posted_len3=$(jq -j '.[0].body' "$evidence/ready-messages-$root3.json" | utf16_len)
[ "$posted_len3" -le 1956 ] || soft "$root3's posted READY is $posted_len3 units, over record.MessagePostLimit (1956)"
cap_refusals "$(claim_session_file "$root3" merger)" >"$evidence/ready-cap-merger-refusals-$root3.json"
jq -n --arg issue "$root3" --argjson refusals "$(jq length "$evidence/ready-cap-merger-refusals-$root3.json")" --argjson posted "$posted_len3" \
  '{issue: $issue, tool_refusals: $refusals, posted_utf16: $posted}' >"$evidence/ready-cap-tool-path-$root3.json"
note "$root3's posted READY is $posted_len3 UTF-16 units; its merger met $(jq length "$evidence/ready-cap-merger-refusals-$root3.json") READY_PACKET_TOO_LONG refusal(s)"
pass

# ---- 9. the pane rule in an operator-spawned sub-architect ---------------------------------------------
begin pane-rule-sub-architect
child_phase=$(daemon_state | jq -r --arg c "$child" '.issues[$c].phase')
sub=$(claims spawn --json --tree "$root1" --issue "$child" --role architect \
  --task "Acceptance pane-rule probe. This is a deliberate proof of the pane's shell rule: the first three calls below are expected to be refused by your pane, and the refusal is what this proof records, so run them anyway, exactly as written, each as its own tool call, one at a time: (1) your bash tool with the command: legion handoff complete --summary 'sub-architect plain probe'  (2) your bash tool with the command: bash -lc \"legion handoff complete --summary 'sub-architect bash -lc probe'\"  (3) your eval tool with code that runs the shell command legion handoff complete --summary 'sub-architect eval probe' through a subprocess  (4) your bash tool with the command: legion handoff read. Then reply with one line per call giving its outcome, and wait. Do nothing else." | jq -er .token)
sub_file() { token_session_file "$sub"; }
probe_refusals sub-architect sub_file "$child" "$child_phase"
claims stop --claim "$sub" >/dev/null || soft "the sub-architect probe claim $sub did not stop"
note "sub-architect claim $sub (issue $child in tree $root1) probed and stopped; $child stayed $child_phase"
pass

begin production-untouched
claims list --json >"$evidence/claims.json"
timeout_hook=report_unchecked_launches
until_true 60 "every launched pane to be endpoint-checked" no_unchecked_launches
# shellcheck disable=SC2034
timeout_hook=''
note "$(wc -l <"$evidence/pane-endpoints-checked.txt") launched panes endpoint-checked when their OMP started"
cap_exceeded=$(grep -c 'CAP_EXCEEDED' "$evidence/logs/daemon.log" || true)
printf '%s\n' "$cap_exceeded" >"$evidence/daemon-log-cap-exceeded-count.txt"
[ "$cap_exceeded" = 0 ] || soft "the daemon log carries $cap_exceeded CAP_EXCEEDED line(s): Dispatch refused a message the daemon sent"
note "daemon log CAP_EXCEEDED lines: $cap_exceeded (before the READY cap, the daemon logged the READY packet's refusal on every outbox attempt)"
production_audit || fail "the rig touched production; see $evidence/production-dispatch-audit.json"
pass

begin services-stopped
daemon_state >"$evidence/final-state.json"
"$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" >"$evidence/final-claims.json"
stop_pid "$watcher_pid"; watcher_pid=
stop_pid "$daemon_pid"; daemon_pid=
stop_dispatch() { stop_pid "$dispatch_pid"; dispatch_pid=; }
stop_dispatch
stop_pid "$listener_pid"; listener_pid=
stop_pid "$bridge_pid"; bridge_pid=
stop_pid "${subagent_watch_pid:-}"
stop_pid "${instructor_pid:-}"; instructor_pid=
TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
stop_pid "$nats_pid"; nats_pid=
for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
[ -z "$(run_processes)" ] || fail "a proof process remains"
pass

begin model-turns-through-the-gateway
route=$(bash "$root/scripts/e2e/lib/check-model-route.sh" --sessions "$profile_agent/sessions" \
  --control "$evidence/model-route-control") || fail "an agent turn left the gateway route"
note "$route"
collect_transcripts
pass

begin profile-stays-in-the-run
# Every session the run's agents wrote is under the run's own home, inside its work directory, and
# the operator's profile root holds none of the run's profile (make_omp_home, lib/omp-home.sh).
[ -d "$profile_agent/sessions" ] || fail "no agent session directory at $profile_agent/sessions"
sessions=$(find "$profile_agent/sessions" -name '*.jsonl' -type f | wc -l)
[ "$sessions" -gt 0 ] || fail "no agent session under $profile_agent/sessions"
[ ! -e "$HOME/.omp/profiles/$profile" ] || fail "the run wrote the operator's profile root: $HOME/.omp/profiles/$profile exists"
note "$sessions agent session files under $profile_agent/sessions; $HOME/.omp/profiles/$profile does not exist"
pass

ok=1
if [ -s "$soft_failures" ]; then
  printf 'acceptance 4b.13b: FAIL — %s soft failure(s):\n' "$(wc -l <"$soft_failures")"
  cat "$soft_failures"
  exit 1
fi
echo "acceptance 4b.13b: PASS at $head_commit"
