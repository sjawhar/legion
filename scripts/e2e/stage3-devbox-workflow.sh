#!/usr/bin/env bash
# Stage 3's devbox gate for the Go coordinator. It drives the durable workflow through the real
# surfaces: a scratch Dispatch, real Envoy/NATS, the Go daemon, real OMP panes, and GitHub's
# sjawhar/legion-smoke sandbox. It deliberately does not source the kind smoke scripts: the
# small host-side rig below is copied and adapted so its lifecycle belongs to this run alone.
#
# Run it as `bash scripts/e2e/stage3-devbox-workflow.sh`. It needs agent-tier secrets and the
# operator's own hawk login, with the keyring holding it unlocked: the agents' model is Anthropic
# through the Hawk model gateway (lib/install-model-gateway.sh), the route every devbox agent
# session uses, and no Anthropic key reaches a pane. The proof human's reviews and merge are the
# devbox's ordinary gh (the dotfiles shim, acting as the sjawhar-agent App), never a Legion App.
# The App private keys are resolved by the daemon through private_key_command; they never enter
# this shell, a pane, an argv, or this transcript.
set -Eeuo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d /tmp/legion-e2e3.XXXXXXXX)
# Evidence survives every outcome: logs, captured state, negative controls, the production audit,
# and every agent transcript. Cleanup removes processes, containers, sockets, and profiles only.
evidence=${STAGE3_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e3-evidence.XXXXXXXX)}
mkdir -p "$evidence/logs" "$evidence/transcripts"
ok=
check=setup
project="S3$(( ($$ + $(date +%s)) % 100000000 ))"
project=${project:0:10}
ptoken=${project,,}
profile="legion-e2e3-$$-$(date +%s)"
state="$work/state"
repo="sjawhar/legion-smoke"
# Anthropic through the gateway: the Google provider answered long workflow turns with empty
# responses (finishReason STOP with no content), so no Gemini-backed implementer could finish.
pg_container="legion-e2e3-pg-$$"
nats_container="legion-e2e3-nats-$$"
daemon_pid=
dispatch_pid=
listener_pid=
bridge_pid=
watcher_pid=
port_daemon=
port_listener=
port_dispatch=
port_worker_stream=
port_pg=
port_nats=
pr_number=
timeout_hook=
gate_artifact=
gate_version=
prod_baseline=
prod_dispatch_url=
prod_envoy_url=${STAGE3_PRODUCTION_ENVOY_URL:-http://127.0.0.1:9020}
audited=

begin() { check=$1; printf '== %s\n' "$check"; }
note() { printf '   %s\n' "$*"; }
pass() { printf 'ok %s\n' "$check"; }
fail() { printf 'FAIL %s: %s\n' "$check" "$*" >&2; exit 1; }

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

# pick_port VAR assigns VAR a port no socket listens on, below the kernel's ephemeral range (so no
# outgoing connection holds it) and distinct from every earlier pick of this run. It assigns in
# place, never through a command substitution, so the run-wide set of picks survives.
picked_ports=" "
pick_port() {
  local low port i
  read -r low _ </proc/sys/net/ipv4/ip_local_port_range
  [ "$low" -gt 12000 ] || fail "the ephemeral port range starts at $low; the rig picks its ports below it"
  for ((i = 0; i < 200; i++)); do
    port=$((10000 + RANDOM % (low - 10000)))
    case "$picked_ports" in *" $port "*) continue ;; esac
    ss -ltn "sport = :$port" | grep -q LISTEN && continue
    picked_ports="$picked_ports$port "
    printf -v "$1" '%s' "$port"
    return 0
  done
  fail "no free port for $1"
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

# collect_transcripts copies every OMP session the rig's profile wrote into the evidence directory
# before the isolated profile is removed.
collect_transcripts() {
  local sessions="$HOME/.omp/profiles/$profile/agent/sessions"
  [ -d "$sessions" ] || return 0
  cp -a "$sessions/." "$evidence/transcripts/" 2>/dev/null || true
}

cleanup() {
  local p
  set +e
  if [ -z "${ok:-}" ] && [ -z "$audited" ] && [ -n "$prod_baseline" ]; then
    printf 'production audit after failure:\n' >&2
    production_audit >&2
  fi
  stop_pid "$watcher_pid"
  stop_pid "$daemon_pid"
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  stop_pid "$bridge_pid"
  TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
  docker rm -f "$pg_container" "$nats_container" >/dev/null 2>&1 || true
  collect_transcripts
  rm -rf "$HOME/.omp/profiles/$profile" || true
  if [ -n "${ok:-}" ]; then
    rm -rf "$work" || true
  else
    printf "the run's scratch workspace is %s\n" "$work" >&2
  fi
  printf "the run's evidence is %s\n" "$evidence" >&2
  return 0
}
trap cleanup EXIT
trap 'printf "FAIL %s: line %s exited %s: %s\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# The copies of kind-smoke's host helpers below own only this run's isolated resources.
start_process() {
  local name=$1
  shift
  "$@" >>"$evidence/logs/$name.log" 2>&1 &
  printf -v "${name}_pid" '%s' "$!"
}

# Each service below binds a port the rig picked. A first start that loses its port to another
# process picks again; a restart keeps its port, which the rest of the rig already names.
start_listener() {
  local token attempt offset result
  token=$(cat "$work/envoy-token")
  for attempt in 1 2 3 4 5; do
    pick_port port_listener
    offset=$(log_size listener)
    ENVOY_API_TOKEN="$token" PORT="$port_listener" ENVOY_LISTEN_HOST=127.0.0.1 \
      ENVOY_MACHINE_ID="legion-e2e3-$$" NATS_URLS="nats://127.0.0.1:$port_nats" \
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

# start_dispatch [keep] — keep restarts the scratch Dispatch on the port the daemon already names.
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
    DATABASE_URL="postgres://legion:$pg_password@127.0.0.1:$port_pg/dispatch?sslmode=disable" \
      DISPATCH_AGENT_TOKEN="$dispatch_token" ENVOY_TOKEN="$envoy_token" HOME="$work/dispatch-home" \
      DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_ALLOWED_LOGINS=smoke \
      DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$port_dispatch" \
      DISPATCH_SERVER_URL="http://127.0.0.1:$port_dispatch" NATS_URLS="nats://127.0.0.1:$port_nats" \
      ENVOY_URL="http://127.0.0.1:$port_listener" \
      start_process dispatch env -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
        -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/envoy-dispatch"
    result=0
    await_start dispatch "$dispatch_pid" "$offset" 60 "the scratch Dispatch server" \
      curl -fsS "http://127.0.0.1:$port_dispatch/api/v1" || result=$?
    [ "$result" != 0 ] || return 0
    [ -z "$keep" ] || fail "the restarted scratch Dispatch lost port $port_dispatch to another process"
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
projects:
  $project: { repo: $repo }
gates:
  design: root-issues
admission_cap: 2
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

# start_daemon [keep] — keep restarts the Go daemon on the ports every pane already names.
start_daemon() {
  local keep=${1:-} attempt offset result
  for attempt in 1 2 3 4 5; do
    if [ -z "$keep" ]; then
      pick_port port_daemon
      pick_port port_worker_stream
      write_legion_config
    fi
    offset=$(log_size daemon)
    OMP_PROFILE="$profile" LEGION_GH_PATH="$real_gh" env -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
      -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 \
      "$work/legion" start --config "$work/legion.yaml" >>"$evidence/logs/daemon.log" 2>&1 &
    daemon_pid=$!
    result=0
    await_start daemon "$daemon_pid" "$offset" 180 "the Go daemon to answer /healthz" \
      curl -fsS "http://127.0.0.1:$port_daemon/healthz" || result=$?
    [ "$result" != 0 ] || return 0
    [ -z "$keep" ] || fail "the restarted Go daemon lost port $port_daemon or $port_worker_stream to another process"
    note "the Go daemon lost port $port_daemon or $port_worker_stream to another process (attempt $attempt); picking again"
  done
  fail "the Go daemon lost a picked port five times"
}

stop_dispatch() {
  stop_pid "$dispatch_pid"
  dispatch_pid=
}

dispatch_url() { printf 'http://127.0.0.1:%s' "$port_dispatch"; }
dispatch_get() {
  curl -fsS --max-time 20 -H "Authorization: Bearer $(cat "$work/dispatch-token")" "$(dispatch_url)/api/v1/$1"
}
# dispatch_events ISSUE prints the issue's whole event log, paging past Dispatch's 200-event limit.
dispatch_events() {
  local issue=$1 after=0 page all='[]'
  while :; do
    page=$(dispatch_get "issues/$issue/events?after=$after&limit=200")
    all=$(jq -c --argjson page "$page" '. + $page' <<<"$all")
    [ "$(jq length <<<"$page")" -eq 200 ] || break
    after=$(jq '.[-1].seq' <<<"$page")
  done
  printf '%s\n' "$all"
}
dispatch_status_is() { dispatch_get "issues/$1" | jq -e --arg status "$2" '.status == $status'; }
review_cap_posted() {
  dispatch_events "$1" | jq -e 'any(.[]; .type == "message.created" and (.payload.body | contains("review_round_cap=3")))'
}
dispatch_human() {
  local method=$1 path=$2 body=${3:-}
  if [ -n "$body" ]; then
    curl -fsS --max-time 20 -X "$method" -H 'X-Dispatch-User: smoke' -H 'content-type: application/json' \
      --data "$body" "$(dispatch_url)/api/v1/$path"
  else
    curl -fsS --max-time 20 -X "$method" -H 'X-Dispatch-User: smoke' "$(dispatch_url)/api/v1/$path"
  fi
}

daemon_state() { "$work/legion" state --json --port "$port_daemon"; }
state_file() { daemon_state >"$evidence/$1.json"; }
issue_phase() { daemon_state | jq -e --arg issue "$1" --arg phase "$2" '.issues[$issue].phase == $phase'; }
issue_worker_state() {
  daemon_state | jq -e --arg issue "$1" --arg role "$2" --arg state "$3" \
    'if $role == "architect" then .issues[$issue].architect.state == $state else .issues[$issue].workers[$role].claim.state == $state end'
}
issue_worker_session() {
  daemon_state | jq -er --arg issue "$1" --arg role "$2" '.issues[$issue].workers[$role].claim.session'
}
architect_session() { daemon_state | jq -er --arg issue "$1" '.issues[$issue].architect.session'; }
issue_worker_pane() {
  daemon_state | jq -er --arg issue "$1" --arg role "$2" '.issues[$issue].workers[$role].claim.locator.tmux.pane'
}
# The state projection intentionally omits the internal tree key; the record itself is the
# observable source for this admission invariant.
child_is_slotless() {
  local child=$1 root_issue_key=$2 tree
  daemon_state | jq -e --arg child "$child" \
    '(.admission.active | length) == 2 and .issues[$child].key == $child and .issues[$child].phase == "admitted" and .issues[$child].slot == null' >/dev/null || return 1
  tree=$(docker exec "$pg_container" psql -U legion -d legion -tAc "select tree from issues where key = '$child'")
  [ "$tree" = "$root_issue_key" ]
}

# On an admission timeout, preserve the three views that disagree: daemon projection, durable
# record, and Dispatch's own issue events. The generic failure keeps the work directory.
admission_diagnostics() {
  daemon_state >"$evidence/admission-timeout-state.json" 2>&1
  docker exec "$pg_container" psql -U legion -d legion -tAc \
    "select key, tree, parent, phase, status, rank from issues order by key" >"$evidence/admission-timeout-records.txt" 2>&1
  dispatch_get "issues/$child_issue/events" >"$evidence/admission-timeout-child-events.json" 2>&1
  note "admission diagnostics: $evidence/admission-timeout-state.json, $evidence/admission-timeout-records.txt, $evidence/admission-timeout-child-events.json"
}
# A targeted human Dispatch message is the proof operator's only instruction surface for an OMP
# pane. Each message is delivered through the same listener and plugin the product uses.
send_agent() {
  local issue=$1 role=$2 message=$3 session body
  if [ "$role" = architect ]; then session=$(architect_session "$issue"); else session=$(issue_worker_session "$issue" "$role"); fi
  assert_claim_endpoints "$issue" "$role"
  body=$(jq -cn --arg body "$message" --arg target "session:$session" '{body:$body,target:$target,delivery:"steer"}')
  dispatch_human POST "issues/$issue/messages" "$body" >/dev/null
  note "sent $role instruction to session $session"
}

claim_session_file() {
  local issue=$1 role=$2
  "$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" |
    jq -er --arg issue "$issue" --arg role "$role" \
      '[.claims[] | select(.issue == $issue and .role == $role and .sessionFile != null and .sessionFile != "")] | last | .sessionFile'
}

# A pane's session file is the persisted evidence of what that one real agent saw and ran. The
# lookup is exact to its claim: another agent's transcript can never satisfy the check.
session_contains() {
  local issue=$1 role=$2 needle=$3 f
  f=$(claim_session_file "$issue" "$role") || return 1
  [ -r "$f" ] && grep -Fq -- "$needle" "$f"
}
db_value() { docker exec "$pg_container" psql -U legion -d legion -tAc "$1"; }

# ---- the production guards -------------------------------------------------------------------
# A pane that can reach the operator's Dispatch or Envoy is not a scratch pane. Every registered
# pane's OMP environment must name this rig's servers before the proof sends it an instruction.
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
# endpoint_mismatch OMP_PID prints the first rig endpoint the OMP process does not carry, or the
# ANTHROPIC_API_KEY it must not carry: the agents' model is the gateway's, and a key in OMP's
# environment would take the turn off that route.
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
    if [ "$got" != "$want" ]; then
      printf '%s=%s, want %s\n' "$name" "${got:-<unset>}" "$want"
      return 0
    fi
  done
  if grep -qz '^ANTHROPIC_API_KEY=' "/proc/$omp/environ" 2>/dev/null; then
    printf 'ANTHROPIC_API_KEY set, want unset\n'
    return 0
  fi
  return 1
}
assert_claim_endpoints() {
  local issue=$1 role=$2 pane_pid omp mismatch
  pane_pid=$(claim_pane_pid "$issue" "$role") || fail "$role pane on $issue has no process locator"
  omp=$(omp_descendant "$pane_pid") || fail "$role pane on $issue (pid $pane_pid) has no OMP process"
  if mismatch=$(endpoint_mismatch "$omp"); then
    fail "ABORT: $role pane on $issue (OMP pid $omp) has $mismatch"
  fi
}
# pane_watcher checks every pane the daemon launches — including those it starts on its own —
# as soon as its OMP process exists, which is before the plugin registers and before any turn.
# A mismatch is recorded, the private tmux server is killed so no agent can act, and the next
# bounded wait aborts naming the pane.
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
          printf '%s pane on %s (incarnation %s, OMP pid %s) has %s\n' "$role" "$issue" "$inc" "$omp" "$mismatch" \
            >"$evidence/pane-endpoint-violation.txt"
          TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
          return 0
        fi
        printf '%s %s %s %s %s\n' "$inc" "$issue" "$role" "$omp" "$(date -u +%FT%T.%3NZ)" >>"$evidence/pane-endpoints-checked.txt"
      done < <(jq -r '.claims[] | select(.locator != null) | [.locator.incarnation, .issue, .role] | @tsv' <<<"$claims")
    fi
    sleep 0.2
  done
}
# unchecked_launches prints every incarnation the daemon launched that the watcher has not checked.
unchecked_launches() {
  local inc
  for inc in $(jq -R -r 'fromjson? | select(.msg == "supervise: launched") | .incarnation' "$evidence/logs/daemon.log"); do
    grep -qF "$inc " "$evidence/pane-endpoints-checked.txt" || printf '%s\n' "$inc"
  done
}
# every_launch_checked fails naming any incarnation the daemon launched that the watcher never saw.
# The watcher checks a pane once its OMP process exists, so a launch made moments before this check
# is given a bounded wait to be seen.
every_launch_checked() {
  timeout_hook=report_unchecked_launches
  until_true 60 "every launched pane to be endpoint-checked" no_unchecked_launches
  timeout_hook=
}
no_unchecked_launches() { [ -z "$(unchecked_launches)" ]; }
report_unchecked_launches() { note "launched panes never endpoint-checked: $(unchecked_launches | tr '\n' ' ')"; }

prod_header_file() {
  local header=$work/production-dispatch-auth
  (umask 077 && jq -r '"Authorization: Bearer " + .dispatch.token' "$HOME/.config/opencode/envoy.json" >"$header")
  printf '%s\n' "$header"
}
# production_baseline reads a recent production Dispatch event id so the post-run audit replays
# only this run's window. It is a read-only query with the operator's existing Dispatch token.
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
  jq -R -s -c 'split("\n") | map(fromjson? | select(.msg == "api: claim registered") | .session) | unique' \
    "$evidence/logs/daemon.log"
}
# production_audit replays production Dispatch events since the baseline and fails on any event a
# rig session authored, any event naming this rig's project key, and any rig session on the
# production Envoy listener. The baseline event itself is the positive control that the replay ran.
production_audit() {
  local sessions header events envoy found=0
  audited=1
  sessions=$(rig_sessions_json)
  printf '%s\n' "$sessions" >"$evidence/rig-sessions.json"
  envoy=$(curl -fsS --max-time 10 "$prod_envoy_url/v1/sessions" |
    jq -c --argjson ids "$sessions" --arg work "$work" '[.[] | select((.session_id as $s | $ids | index($s)) or ((.dir // "") | startswith($work)))]')
  printf '%s\n' "$envoy" >"$evidence/production-envoy-rig-sessions.json"
  if [ "$envoy" != "[]" ]; then
    printf 'production Envoy listener holds rig sessions: %s\n' "$envoy"
    found=1
  fi
  if [ "$prod_baseline" = none ]; then
    printf 'no user-level Dispatch configuration exists, so no production Dispatch fallback was possible\n'
    return "$found"
  fi
  header=$(prod_header_file)
  curl -sS -N --max-time 25 -H "@$header" "$prod_dispatch_url/api/v1/events?since=$((prod_baseline - 1))" \
    >"$work/production-events.sse" 2>/dev/null || true
  rm -f "$header"
  events=$(sed -n 's/^data: //p' "$work/production-events.sse" | jq -s -c --argjson ids "$sessions" --arg project "$project" --argjson baseline "$prod_baseline" '
    {control: any(.[]; .id == $baseline), count: length,
     rig: [.[] | select((.actor.id as $a | $ids | index($a)) or (tostring | contains($project)))
       | {id, issue_key, type, actor: .actor.id, created_at}]}')
  printf '%s\n' "$events" >"$evidence/production-dispatch-audit.json"
  if ! jq -e '.control' >/dev/null <<<"$events"; then
    printf 'production Dispatch replay did not return its baseline event %s\n' "$prod_baseline"
    return 1
  fi
  if ! jq -e '.rig == []' >/dev/null <<<"$events"; then
    printf 'production Dispatch has rig-authored events: %s\n' "$(jq -c .rig <<<"$events")"
    found=1
  fi
  printf 'production audit: %s events replayed since %s, rig events %s, rig Envoy sessions %s\n' \
    "$(jq -r .count <<<"$events")" "$prod_baseline" "$(jq -c .rig <<<"$events")" "$envoy"
  return "$found"
}

new_issue() {
  local title=$1 parent=${2:-} payload
  payload=$(jq -cn --arg project "$project" --arg title "$title" --arg parent "$parent" \
    'if $parent == "" then {project:$project,title:$title} else {project:$project,title:$title,parent:$parent} end')
  dispatch_human POST issues "$payload" | jq -er .key
}
set_status() { dispatch_human PATCH "issues/$1" "$(jq -cn --arg status "$2" '{status:$status}')" >/dev/null; }

# A role's ordinary state is enough for the protocol; each real agent receives a deliberately
# narrow smoke instruction so the proof observes the workflow rather than an arbitrary feature.
wait_for_phase() { until_true 600 "$1 to reach $2" issue_phase "$1" "$2"; }
issue_worker_live() {
  local issue=$1 role=$2 not_pane=${3:-}
  daemon_state | jq -e --arg issue "$issue" --arg role "$role" --arg not_pane "$not_pane" '
    (if $role == "architect" then .issues[$issue].architect else .issues[$issue].workers[$role].claim end) as $claim
    | ($claim.session // "") != "" and ($claim.state | IN("ready", "working", "idle"))
      and ($not_pane == "" or ($claim.locator.tmux.pane // "") != $not_pane)'
}
wait_for_worker() {
  until_true 300 "$2 worker on $1 to register" issue_worker_live "$1" "$2"
  assert_claim_endpoints "$1" "$2"
}

# The architect owns spec editing and gate registration; the proof names the one primary artifact
# Dispatch created so a real agent cannot register an unrelated document.
drive_gate() {
  local issue=$1 label=$2 artifact
  artifact=$(dispatch_get "issues/$issue" | jq -er .primary_artifact_id)
  wait_for_worker "$issue" architect
  send_agent "$issue" architect "$label: update this issue's primary spec document with one tiny, concrete one-file smoke change for $repo, and say in it that a review of the pull request may ask for one more line appended to that same file, which is in scope. Request approval for primary artifact $artifact. Then use the Go-daemon Legion operation to register the gate for exactly artifact $artifact at the version returned by that approval request. Wait after registering."
  until_true 300 "$label architect to register primary artifact $artifact" sh -c \
    "'$work/legion' state --json --port '$port_daemon' | jq -e --arg issue '$issue' --arg artifact '$artifact' '.issues[\$issue].designGate.artifactId == \$artifact and .issues[\$issue].designGate.currentVersion > 0'"
  gate_artifact=$(daemon_state | jq -er --arg issue "$issue" '.issues[$issue].designGate.artifactId')
  gate_version=$(daemon_state | jq -er --arg issue "$issue" '.issues[$issue].designGate.currentVersion')
  dispatch_human POST "artifacts/$gate_artifact/reviews" '{"state":"approved"}' >/dev/null
  wait_for_phase "$issue" planning
  wait_for_worker "$issue" planner
}

# A direct review is deliberately the devbox's ordinary gh acting as the proof human. It is never
# `legion gh`, and the bridge is the only path that carries the event to the daemon.
request_changes() {
  local body=$1
  gh -R "$repo" pr review "$pr_number" --request-changes --body "$body"
}
# round_line ROUND is the line a scripted review round asks for: distinct per round and run, and
# within the spec, whose architect was told a review may ask for one more line in the smoke file.
round_line() { printf 'Stage 3 review round %s (%s)' "$1" "$project"; }
# round_correction_pushed ROUND: that round's line is added to a product file of the pull request
# (a .legion/ handoff that only quotes it is not the correction).
round_correction_pushed() {
  gh api --paginate "repos/$repo/pulls/$pr_number/files" --jq '.[] | select(.filename | startswith(".legion/") | not) | .patch // ""' |
    grep -qF -- "+$(round_line "$1")"
}

# REST names the review App's account legion-reviewer[bot]; GraphQL (`gh pr view --json reviews`)
# drops the suffix, and a user could hold the bare name. The approval must be of the current head.
reviewer_approved_head() {
  local head
  head=$(gh api "repos/$repo/pulls/$pr_number" --jq .head.sha) || return 1
  gh api --paginate "repos/$repo/pulls/$pr_number/reviews" \
    --jq '.[] | select(.user.login == "legion-reviewer[bot]" and .state == "APPROVED") | .commit_id' | grep -qx "$head"
}
# The Go daemon has no clean-head loop yet: skills/legion-worker/SKILL.md wants APPROVE only for a
# head that carries no .legion/, then the implementer's .legion/ deletion push, and the Go workflow
# neither asks for that round nor waits for it. Its destination is Stage 7's clean-head loop. Until
# then the proof's reviewer approves the head it has, the merge carries the run's .legion/ handoffs
# and retro learnings onto the smoke main, and clean_smoke_main removes them after the merge.
approve_as_reviewer() {
  local issue=$1
  send_agent "$issue" reviewer "Stage 3 proof final review: use the bash tool to submit APPROVE on pull request #$pr_number in $repo at its current head as legion-reviewer[bot], then complete the reviewer handoff. This exact smoke instruction takes precedence over waiting for another review round."
  until_true 300 "legion-reviewer[bot] approval of pull request #$pr_number at its head" reviewer_approved_head
}
# smoke_main_leftovers prints each path on the smoke repository's main under .legion/ or
# docs/solutions/: the handoffs and retro learnings a merged proof pull request carries there.
smoke_main_leftovers() {
  gh api "repos/$repo/git/trees/main?recursive=1" \
    --jq '.tree[] | select(.type == "blob") | .path | select(startswith(".legion/") or startswith("docs/solutions/"))'
}
# clean_smoke_main removes every leftover from the smoke main through the proof human's ordinary
# merge (the smoke main takes changes only through pull requests), so the next run starts from a
# fixture whose base carries no other issue's handoff. See approve_as_reviewer for why a merge
# leaves them.
clean_smoke_main() {
  local paths base branch path sha url
  paths=$(smoke_main_leftovers)
  [ -n "$paths" ] || return 0
  base=$(gh api "repos/$repo/git/ref/heads/main" --jq .object.sha)
  branch="proof/clean-main-$ptoken"
  gh api "repos/$repo/git/refs" -f ref="refs/heads/$branch" -f sha="$base" >/dev/null
  while IFS= read -r path; do
    sha=$(gh api "repos/$repo/contents/$path?ref=$branch" --jq .sha)
    gh api -X DELETE "repos/$repo/contents/$path" -f message="proof fixture: remove $path" -f sha="$sha" -f branch="$branch" >/dev/null
  done <<<"$paths"
  url=$(gh -R "$repo" pr create --base main --head "$branch" --title "proof fixture: remove the handoffs and learnings Stage 3 runs merged ($project)" \
    --body "The Stage 3 proof run $project removes what merged proof pull requests left on main: .legion/ handoffs and docs/solutions/ retro learnings. The Go daemon has no clean-head loop before Stage 7, so each proof merge carries them. This is a proof fixture change by the proof's human-merge identity; it changes no product.")
  gh -R "$repo" pr merge "${url##*/}" --squash --delete-branch
  note "the proof human removed $(wc -l <<<"$paths") leftover paths from $repo main through $url"
}
issue_phase_in() {
  local issue=$1
  shift
  daemon_state | jq -e --arg issue "$issue" '.issues[$issue].phase as $p | $ARGS.positional | index($p) != null' --args "$@"
}
# handoff_fact_commit ISSUE ROLE PHASE ROUND prints the commit carrying the handoff the daemon
# accepted for the role's completion of that phase round. A refused completion (a stale or
# not-new handoff) is a processed fact too, so the accepted one is the role's recorded last handoff,
# and it must be the commit of a processed fact for that phase round. The check runs right after the
# round's transition, before the role's next completion can move it.
handoff_fact_commit() {
  local accepted
  accepted=$(db_value "select last_handoff from phases where issue = '$1' and role = '$2'")
  [ -n "$accepted" ] || return 0
  [ "$(db_value "select count(*) from processed_events where source = 'api' and event_id like 'handoff:$1:%:$2:$3:$4:$accepted:%'")" -ge 1 ] || return 0
  printf '%s\n' "$accepted"
}
role_app() { case "$1" in implementer | merger) printf 'legion-implementer[bot]' ;; *) printf 'legion-reviewer[bot]' ;; esac; }
issue_workspace() { printf '%s/workspaces/%s/%s' "$state" "$repo" "${1,,}"; }
# assert_handoff_committer ISSUE ROLE PHASE ROUND: the commit carrying that completion's handoff is
# authored and committed by the role's own App, read from the issue's workspace (the commit need not
# be pushed), so no other pane sealed another role's handoff.
assert_handoff_committer() {
  local commit identity want
  commit=$(handoff_fact_commit "$1" "$2" "$3" "$4")
  [ -n "$commit" ] || fail "$1 has no $2 $3 round $4 handoff fact"
  identity=$(jj -R "$(issue_workspace "$1")" log -r "$commit" --no-graph -T 'author.name() ++ "|" ++ committer.name()' 2>&1) ||
    fail "read $1's $2 $3 round $4 handoff commit $commit: $identity"
  want="$(role_app "$2")|$(role_app "$2")"
  [ "$identity" = "$want" ] || fail "$1's $2 $3 round $4 handoff commit $commit is authored|committed by $identity, want $want"
  note "$2 $3 round $4 handoff $commit authored and committed by $identity"
}
# retro_reported ISSUE: the daemon applied the implementer's retro completion.
retro_reported() {
  [ "$(db_value "select count(*) from processed_events where source = 'api' and event_id like 'handoff:$1:%:implementer:retro:%'")" -ge 1 ]
}
# A delivered notice is rendered into the receiving pane's session as the listener's envelope,
# `summary: <kind> on <issue>`, which no role prompt or proof instruction contains: the bare kind
# does appear in the architect's prompt, so it can never be the needle.
notice_needle() { printf 'summary: %s on %s' "$1" "$2"; }
# notice_deliveries ISSUE ROLE NEEDLE counts the Envoy deliveries in the claim's session holding
# NEEDLE: one session line per delivered message, and never a line the agent wrote itself. The
# outbox cannot count them: the runner deletes each row it finishes.
notice_deliveries() {
  local f
  f=$(claim_session_file "$1" "$2") || { printf '0\n'; return 0; }
  grep -F '"customType":"envoy-message"' "$f" | grep -cF -- "$3" || true
}
# production_check_reported ISSUE: the daemon applied the implementer's production-check
# completion, whose fact id names the phase it completes (the completion moves no phase: the
# architect's sign-off does).
production_check_reported() {
  [ "$(db_value "select count(*) from processed_events where source = 'api' and event_id like 'handoff:$1:%:implementer:production_check:%'")" -ge 1 ]
}
# assert_round_handoff ISSUE ROUND: the issue reached testing on the implementer's own completion
# of that implementing round (the handoff fact id names the phase and the round), never on a push
# alone carrying an earlier round's handoff.
assert_round_handoff() {
  [ "$(db_value "select count(*) from processed_events where source = 'api' and event_id like 'handoff:$1:%:implementer:implementing:$2:%'")" -ge 1 ] ||
    fail "$1 reached testing without the implementer's implementing round $2 handoff"
}
# tree_suspended ISSUE: the lingering tree's root architect is suspended, its session kept.
tree_suspended() { issue_worker_state "$1" architect suspended; }
notice_delivered() { [ "$(notice_deliveries "$@")" -ge 1 ]; }
claim_token() { printf 'legion-%s-%s-%s' "$ptoken" "${1,,}" "$2"; }
claim_incarnation() {
  daemon_state | jq -er --arg issue "$1" --arg role "$2" '.issues[$issue].workers[$role].claim.locator.incarnation // empty'
}
# held_kill_ready ISSUE SEEN: the implementer's current incarnation is one the proof has not
# killed yet and the pane watcher has already endpoint-checked, so killing it loses no evidence.
held_kill_ready() {
  local inc
  inc=$(claim_incarnation "$1" implementer) || return 1
  case "$2" in *" $inc "*) return 1 ;; esac
  grep -qF "$inc " "$evidence/pane-endpoints-checked.txt"
}
held_or_relaunched() {
  issue_phase "$1" held && return 0
  local inc
  inc=$(claim_incarnation "$1" implementer) || return 1
  [ "$inc" != "$2" ]
}
# launches_after_failure CLAIM counts the daemon's launches of CLAIM logged after it failed it.
launches_after_failure() {
  jq -R -s --arg claim "$1" '
    [split("\n")[] | fromjson? | select(.claim == $claim)] as $lines
    | ([$lines | to_entries[] | select(.value.msg == "supervise: claim failed") | .key] | last) as $failed
    | if $failed == null then -1 else [$lines[($failed + 1):][] | select(.msg == "supervise: launched")] | length end
  ' "$evidence/logs/daemon.log"
}

# The negative controls mutate only captured evidence, never the production-like rig. Each
# checker is the exact assertion the positive check uses; a corrupt copy must be rejected before
# the original is accepted again.
expect_failure() {
  local name=$1
  shift
  if "$@" >"$evidence/negative-$name.out" 2>&1; then
    fail "negative control $name unexpectedly passed"
  fi
  note "negative control $name rejected the deliberately broken observation"
}
# An issue event's payload is the whole issue after the write, so a status write is an event whose
# status differs from the issue event before it (`issue.closed` for done). Every lifecycle
# transition must carry the daemon's actor, and all five lifecycle statuses must appear, so the
# check cannot pass on a history the workflow never wrote.
assert_status_actors() {
  local file=$1
  jq -e --arg daemon "legion-daemon:$project" '
    [ .[] | select(.type | IN("issue.created", "issue.updated", "issue.closed")) ] | sort_by(.seq)
    | [ range(1; length) as $i | select(.[$i].payload.status != .[$i - 1].payload.status) | .[$i] ]
    | map(select(.payload.status | IN("in_progress", "testing", "needs_review", "retro", "done")))
    | (map(.payload.status) | unique) == ["done", "in_progress", "needs_review", "retro", "testing"]
      and all(.actor.id == $daemon)
  ' "$file" >/dev/null
}
assert_held_snapshot() {
  local file=$1
  jq -e --arg issue "$held_issue" '
    .issues[$issue].phase == "held" and .issues[$issue].workers.implementer.claim.state == "failed"
  ' "$file" >/dev/null
}
assert_ready_gate_closed() {
  local file=$1
  jq -e --arg issue "$root_issue" '
    .issues[$issue].phase == "merging" and .issues[$issue].designGate.currentVersion > .issues[$issue].designGate.approvedVersion
  ' "$file" >/dev/null
}

begin prerequisites
for tool in go docker jq curl ss tmux bun mise secrets gh shellcheck jj hawk-token; do command -v "$tool" >/dev/null || fail "$tool is required"; done
# STAGE3_FROM is a development aid for iterating on the later scenarios against a fresh rig; a run
# with it set is never the proof and never prints PASS. `held` skips the first issue's workflow:
# the proof human closes that root, freeing its admission slot as its sign-off would, and the
# credential check reads STAGE3_PR (default: the newest smoke pull request) in place of its pull
# request. `restart` also skips the held-worker scenario. STAGE3_UNTIL=rework is the other
# development aid: it drives the first issue through its review rounds and the final review, then
# skips every later scenario.
from=${STAGE3_FROM:-}
case "$from" in
  "" | held | restart) ;;
  *) fail "STAGE3_FROM must be held or restart, not $from" ;;
esac
until=${STAGE3_UNTIL:-}
case "$until" in
  "" | rework) ;;
  *) fail "STAGE3_UNTIL must be rework, not $until" ;;
esac
[ -z "$from" ] || [ -z "$until" ] || fail "set STAGE3_FROM or STAGE3_UNTIL, not both"
development=${from:+from $from}${until:+until $until}
# The daemon runs gh by the path it resolves at boot. This box's PATH heads with a gh wrapper
# (the dotfiles shim, which hands an agent's explicit GH_TOKEN on to `knives gh`), so the proof
# names mise's gh as LEGION_GH_PATH, the override an operator uses for exactly this.
real_gh=$(mise which gh) || fail "mise has no gh"
gh repo view "$repo" --json name >/dev/null || fail "the devbox's ordinary gh cannot read $repo"
mkdir -p "$evidence/logs" "$state" "$work/xdg" "$work/tmux"
chmod 0700 "$state" "$work/xdg" "$work/tmux"
# The model route, installed while this shell still holds the operator's XDG directories, which
# the key command runs hawk-token under. Its first mint is the preflight: a locked keyring stops the
# run here, by name. The key command's log is evidence.
key_command=$(bash "$root/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --dest "$evidence/model-gateway") ||
  fail "the agents' model route through the Hawk model gateway could not be installed (the reason is above)"
note "the agents' model route: $(sed -n 's/^  default: //p' "$HOME/.omp/profiles/$profile/agent/config.yml") through the gateway, keyed by $key_command"
export XDG_STATE_HOME="$work/xdg"
export TMUX_TMPDIR="$work/tmux"
pass

begin rig
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/dispatch-token" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/postgres-password")
chmod 0600 "$work"/*token "$work/envoy-auth-header" "$work/postgres-password"
(cd "$root/packages/daemon-go" && go build -o "$work/legion" ./cmd/legion)
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener && go build -o "$work/envoy-dispatch" ./cmd/dispatch)
docker ps >/dev/null
# Docker assigns the containers' host ports when it binds them, so neither can lose a race.
docker run -d --name "$pg_container" --mount type=tmpfs,destination=/var/lib/postgresql/data \
  -e POSTGRES_USER=legion -e POSTGRES_PASSWORD="$(cat "$work/postgres-password")" -e POSTGRES_DB=dispatch \
  -p "127.0.0.1::5432" postgres:16 >/dev/null
port_pg=$(docker port "$pg_container" 5432/tcp | sed -n '1s/.*://p')
[ -n "$port_pg" ] || fail "docker assigned Postgres no host port"
until_true 60 "Postgres to accept TCP connections" docker exec "$pg_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d dispatch
docker exec "$pg_container" createdb -U legion legion
docker run -d --name "$nats_container" -p "127.0.0.1::4222" nats:2.10 -js >/dev/null
port_nats=$(docker port "$nats_container" 4222/tcp | sed -n '1s/.*://p')
[ -n "$port_nats" ] || fail "docker assigned NATS no host port"
until_true 60 "NATS to be ready" sh -c "docker logs '$nats_container' 2>&1 | grep -q 'Server is ready'"
start_listener
start_dispatch
dispatch_human POST projects "$(jq -cn --arg key "$project" --arg name "Stage 3 proof $project" '{key:$key,name:$name}')" >/dev/null
dispatch_human PUT "settings/repo-projects/$repo" "$(jq -cn --arg project "$project" '{project:$project}')" >/dev/null
# This is the production Envoy ingress bridge, subscribe-only from its perspective. GitHub events
# are observed, never manufactured, and only the smoke repository is forwarded to this run's NATS.
SMOKE_REPO="$repo" SMOKE_RIG_NATS="nats://127.0.0.1:$port_nats" \
  SMOKE_UPSTREAM_NATS="${SMOKE_UPSTREAM_NATS:-nats://envoy-nats.tailb86685.ts.net:4222}" \
  start_process bridge env -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
    -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 \
    bun run "$root/scripts/kind-smoke/envoy-bridge.ts"
until_true 90 "the GitHub ingress bridge to report ready" grep -q 'BRIDGE READY' "$evidence/logs/bridge.log"
(cd "$root" && bun install --frozen-lockfile >/dev/null)
manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --profile "$profile" --dest "$work/plugin")
pin=$(bun "$root/packages/daemon/src/daemon/omp-pin.ts")
mise where "$pin" >/dev/null 2>&1 || mise install "$pin" >&2
cat >"$work/instructions.md" <<'EOF'
# Stage 3 proof instructions

This is a throwaway workflow proof. Do not act until a targeted human Dispatch message gives the
next exact proof operation. Follow that instruction precisely, use the Go-daemon Legion tools and
handoffs, and do not create work outside the issue's smoke branch.
EOF
start_daemon
note "project $project, profile $profile, plugin $(jq -r '.name + "@" + .version' "$manifest"), NATS/Dispatch/daemon ports $port_nats/$port_dispatch/$port_daemon"
pane_watcher &
watcher_pid=$!
production_baseline
note "production Dispatch audit baseline: event ${prod_baseline} on ${prod_dispatch_url:-<no user-level Dispatch>}"
pass

begin admission
root_issue=$(new_issue "Primary durable workflow profile")
held_issue=$(new_issue "Budget exhaustion sentinel")
restart_issue=$(new_issue "Restart outbox compass")
set_status "$root_issue" todo
set_status "$held_issue" todo
set_status "$restart_issue" todo
until_true 180 "two roots admitted and one waiting in Dispatch rank order" sh -c \
  "'$work/legion' state --json --port '$port_daemon' | jq -e --arg a '$root_issue' --arg b '$held_issue' --arg c '$restart_issue' '.admission.cap == 2 and .admission.active == [\$a,\$b] and .admission.waiting == [\$c]'"
child_issue=$(new_issue "Dependent admission leaf" "$root_issue")
set_status "$child_issue" todo
timeout_hook=admission_diagnostics
until_true 120 "the child to join its first tree without taking a slot" child_is_slotless "$child_issue" "$root_issue"
timeout_hook=
state_file admission
note "active roots: $(jq -c .admission.active "$evidence/admission.json"); waiting: $(jq -c .admission.waiting "$evidence/admission.json"); child $child_issue remains slotless under $root_issue"
pass

begin panes-pinned-to-the-rig-before-any-agent-turn
# The two admitted architects are the only panes before the proof sends its first instruction.
# Every later pane is checked when it registers and again before each instruction.
wait_for_worker "$root_issue" architect
wait_for_worker "$held_issue" architect
note "$(wc -l <"$evidence/pane-endpoints-checked.txt") pane checks: DISPATCH_URL=http://127.0.0.1:$port_dispatch, DISPATCH_TOKEN_FILE=$state/secrets/dispatch-token, ENVOY_URL=http://127.0.0.1:$port_listener, ENVOY_NATS_URL=nats://127.0.0.1:$port_nats"
pass

# The first issue end to end: the gate, every phase, three review rounds, the READY gate, the human
# merge, the production check, and the sign-off.
primary_issue() {
  begin primary-architect-posts-and-registers-spec
  drive_gate "$root_issue" "Stage 3 proof gate operation"
  artifact_id=$gate_artifact
  note "architect registered $artifact_id version $gate_version; human approval advanced the daemon to planning"
  pass

  begin primary-planner-handoff
  send_agent "$root_issue" planner "Stage 3 proof planning operation: write the required .legion/plan.json handoff for the one-file smoke change, then run legion handoff complete with a concise summary. Do not start another role."
  wait_for_phase "$root_issue" implementing
  assert_handoff_committer "$root_issue" planner planning 0
  wait_for_worker "$root_issue" implementer
  pass

  begin primary-implementer-pull-request-and-handoff
  send_agent "$root_issue" implementer "Stage 3 proof implementation operation: make the smallest one-file change described by this issue in your $repo workspace, commit it on legion/$root_issue, open its pull request, record the required implementation proof and handoff, then run legion handoff complete. Do not merge."
  until_true 900 "implementer pull request on legion/$root_issue" sh -c \
    "gh -R '$repo' pr list --head 'legion/$root_issue' --state open --json number | jq -e 'length == 1' >/dev/null"
  pr_number=$(gh -R "$repo" pr list --head "legion/$root_issue" --state open --json number --jq '.[0].number')
  # Each pane commits as its role's App: the planner's handoff as legion-reviewer[bot], the
  # implementer's change as legion-implementer[bot], author and committer alike.
  authors=$(gh api "repos/$repo/pulls/$pr_number/commits" --jq '[.[] | .commit.author.name + "|" + .commit.committer.name] | unique | join(",")')
  [ "$authors" = "legion-implementer[bot]|legion-implementer[bot],legion-reviewer[bot]|legion-reviewer[bot]" ] ||
    fail "$repo#$pr_number commits are authored|committed by $authors, want the implementer's and planner's App bots for both"
  wait_for_phase "$root_issue" testing
  assert_round_handoff "$root_issue" 0
  assert_handoff_committer "$root_issue" implementer implementing 0
  wait_for_worker "$root_issue" tester
  note "implementer opened $repo#$pr_number on legion/$root_issue, its commits authored and committed by the implementer and planner App bots, and its handoff advanced the daemon to testing"
  pass

  begin primary-tester-pass
  send_agent "$root_issue" tester "Stage 3 proof test operation: inspect the implementer's actual one-file change and pull request #$pr_number, run a focused observable check, record the required test handoff with verdict pass, then run legion handoff complete --verdict pass."
  wait_for_phase "$root_issue" reviewing
  assert_handoff_committer "$root_issue" tester testing 0
  wait_for_worker "$root_issue" reviewer
  pass

  # Three real GitHub changes-requested reviews exercise the round counter, the return to
  # implementing, and the third `pr-blocked` publication. Every recovery repeats the real workers.
  for round in 1 2 3; do
    begin "review-round-$round-changes-requested"
    # A review that names no correction leaves the implementer nothing it may change under a spec
    # that pins the smoke line: it deliberated past the wait or escalated to a human. Each round
    # names one concrete correction the spec permits.
    request_changes "Stage 3 proof review, round $round: append the line \`$(round_line "$round")\` to the end of the same file this pull request changes, below the lines already there, and change nothing else. The spec permits one more line in that file for a review round, so this correction is in scope."
    wait_for_phase "$root_issue" implementing
    until_true 180 "round $round to write in_progress on the Dispatch board" dispatch_status_is "$root_issue" in_progress
    wait_for_worker "$root_issue" implementer
    if [ "$round" = 3 ]; then
      until_true 180 "the third review round to post pr-blocked" review_cap_posted "$root_issue"
      until_true 180 "the architect to receive the pr-blocked notice" notice_delivered "$root_issue" architect "$(notice_needle pr-blocked "$root_issue")"
      note "round three returned to in_progress and posted pr-blocked to Dispatch and the architect topic"
      pass
      break
    fi
    send_agent "$root_issue" implementer "Stage 3 proof correction round $round: make the correction the review names (append the line \`$(round_line "$round")\` to the file this pull request changes), push it to the existing pull request #$pr_number, write the implementation handoff, then run legion handoff complete: a push alone does not finish this round."
    # A correction round runs the implementer's whole loop (the edit, the push, the handoff commit,
    # the completion) as the retro does, and took past 600 s in acceptance runs: 1200 s.
    until_true 1200 "$root_issue to reach testing on round $round's correction" issue_phase "$root_issue" testing
    until_true 120 "round $round's correction on pull request #$pr_number" round_correction_pushed "$round"
    assert_round_handoff "$root_issue" "$round"
    assert_handoff_committer "$root_issue" implementer implementing "$round"
    wait_for_worker "$root_issue" tester
    send_agent "$root_issue" tester "Stage 3 proof retest round $round: verify the correction on pull request #$pr_number, write the tester handoff with verdict pass, and complete the phase."
    wait_for_phase "$root_issue" reviewing
    assert_handoff_committer "$root_issue" tester testing "$round"
    wait_for_worker "$root_issue" reviewer
    pass
  done

  begin final-review-cycle
  send_agent "$root_issue" implementer "Stage 3 proof final correction: make the correction the round 3 review names (append the line \`$(round_line 3)\` to the file this pull request changes), push it to pull request #$pr_number, write the implementation handoff, then run legion handoff complete: a push alone does not finish this round."
  # The same whole correction loop as each review round: 1200 s.
  until_true 1200 "$root_issue to reach testing on round 3's correction" issue_phase "$root_issue" testing
  until_true 120 "round 3's correction on pull request #$pr_number" round_correction_pushed 3
  assert_round_handoff "$root_issue" 3
  assert_handoff_committer "$root_issue" implementer implementing 3
  wait_for_worker "$root_issue" tester
  send_agent "$root_issue" tester "Stage 3 proof final test: verify pull request #$pr_number, record the tester pass handoff, then complete it."
  wait_for_phase "$root_issue" reviewing
  assert_handoff_committer "$root_issue" tester testing 3
  wait_for_worker "$root_issue" reviewer
  approve_as_reviewer "$root_issue"
  # The approval starts the implementer's retro, whose task names the phase; the resumed implementer
  # may finish it before the proof's instruction reaches it, so the wait is for the issue to leave
  # reviewing, not to sit in retro.
  until_true 600 "$root_issue to leave reviewing for retro" issue_phase_in "$root_issue" retro merging
  pass
  [ "$until" != rework ] || return 0

  begin retro-handoff
  if issue_phase "$root_issue" retro >/dev/null; then
    wait_for_worker "$root_issue" implementer
    send_agent "$root_issue" implementer "Stage 3 proof retro: write the required retro handoff for pull request #$pr_number and complete the phase. Do not change the approved implementation."
  fi
  # The retro skill's fresh-eyes review is a mandatory subagent (4 min 12 s in one acceptance run),
  # and the retro then commits, pushes, and edits the pull request body: a retro took 5 to 10
  # minutes, past wait_for_phase's 600 s bound, so this wait allows 1200 s.
  until_true 1200 "$root_issue to reach merging" issue_phase "$root_issue" merging
  retro_reported "$root_issue" || fail "$root_issue reached merging with no implementer retro completion recorded"
  until_true 60 "the daemon's retro status on the Dispatch board" dispatch_status_is "$root_issue" retro
  wait_for_worker "$root_issue" merger
  note "the implementer's retro completion advanced $root_issue to merging"
  pass

  begin ready-gated-on-new-spec-version
  # A named primary-artifact upload creates version two and emits artifact.version. The gate must
  # close before the merger sends READY; no second READY is allowed after the human approves it.
  dispatch_human POST "issues/$root_issue/artifacts" \
    "$(jq -cn --arg name spec.md --arg content "# Stage 3 smoke\n\nThe human revised this proof specification before READY.\n" '{name:$name,content:$content,summary:"Stage 3 gate re-close"}')" >/dev/null
  until_true 120 "the new primary spec version to close the gate" sh -c \
    "'$work/legion' state --json --port '$port_daemon' | jq -e --arg issue '$root_issue' '.issues[\$issue].designGate.currentVersion > .issues[\$issue].designGate.approvedVersion'"
  state_file ready-refused
  send_agent "$root_issue" merger "Stage 3 proof READY operation: run legion handoff complete --summary 'stage 3 ready gate proof' --ready now. Record its exact refusal and then wait; do not retry it."
  until_true 180 "merger to observe the READY 409 refusal naming the new version" session_contains "$root_issue" merger "READY refused: approve design version"
  assert_ready_gate_closed "$evidence/ready-refused.json" || fail "the versioned design gate did not close before READY"
  jq '.issues["'"$root_issue"'"].designGate.currentVersion = .issues["'"$root_issue"'"].designGate.approvedVersion' \
    "$evidence/ready-refused.json" >"$evidence/ready-open-negative.json"
  expect_failure ready-gate-not-reclosed assert_ready_gate_closed "$evidence/ready-open-negative.json"
  assert_ready_gate_closed "$evidence/ready-refused.json" || fail "the READY gate did not restore after its negative control"
  new_version=$(daemon_state | jq -er --arg issue "$root_issue" '.issues[$issue].designGate.currentVersion')
  dispatch_human POST "artifacts/$artifact_id/reviews" '{"state":"approved"}' >/dev/null
  wait_for_phase "$root_issue" awaiting_merge
  note "READY refused at unapproved spec version $new_version; approval advanced awaiting_merge without another READY"
  pass

  begin ordinary-human-squash-merge
  # This is intentionally the devbox's ordinary gh as the proof human (the dotfiles shim, acting as the
  # sjawhar-agent App). Legion's Apps are neither invoked nor able to merge.
  gh -R "$repo" pr merge "$pr_number" --squash --delete-branch
  until_true 300 "the daemon to observe the ordinary human merge" issue_phase "$root_issue" production_check
  # The resumed implementer's task names production_check, and it may record the check before the
  # proof's instruction reaches it; its completion is observed, not its instruction.
  if ! production_check_reported "$root_issue" >/dev/null 2>&1; then
    wait_for_worker "$root_issue" implementer
    send_agent "$root_issue" implementer "Stage 3 proof production check: verify the merged smoke change through its repository surface, record the production-check handoff and required PR/Dispatch record, then complete the phase."
  fi
  until_true 600 "the implementer's production-check completion to be recorded" production_check_reported "$root_issue"
  until_true 180 "the architect to receive the production-check phase-finished notice" notice_delivered "$root_issue" architect \
    "$(notice_needle phase-finished "$root_issue")"'\n  message:\n    kind: phase-finished\n    role: implementer\n    phase: production_check'
  if issue_phase "$root_issue" production_check >/dev/null 2>&1; then
    send_agent "$root_issue" architect "Stage 3 proof sign-off: the implementer's production check for $root_issue is recorded; use the Go-daemon sign-off operation for $root_issue now."
  fi
  wait_for_phase "$root_issue" "done"
  until_true 60 "the daemon's done status on the Dispatch board" dispatch_status_is "$root_issue" "done"
  until_true 120 "the lingering tree's architect to be suspended" tree_suspended "$root_issue"
  note "ordinary gh squash-merged $repo#$pr_number; the daemon resumed production_check, the implementer's completion reached the architect, and its sign-off closed the issue; the tree lingers with its architect suspended"
  pass

  begin smoke-main-clean
  clean_smoke_main
  leftovers=$(smoke_main_leftovers)
  [ -z "$leftovers" ] || fail "$repo main still carries proof leftovers: $(tr '\n' ' ' <<<"$leftovers")"
  note "$repo main carries no .legion/ handoff and no docs/solutions/ learning"
  pass
}

held_worker() {
  begin held-after-launch-budget
  # The second admitted root is driven just far enough to own a real implementer pane. A launch that
  # never becomes ready spends the claim's launch budget (three) and a ready agent resets it, so the
  # first kill takes the ready implementer and every later kill takes the relaunch as soon as the pane
  # watcher has checked it, until the claim fails and the issue is held.
  drive_gate "$held_issue" "Stage 3 held-worker proof"
  send_agent "$held_issue" planner "Stage 3 held-worker proof: write a minimal plan handoff and complete planning."
  wait_for_phase "$held_issue" implementing
  wait_for_worker "$held_issue" implementer
  killed=" "
  kills=0
  until issue_phase "$held_issue" held >/dev/null 2>&1; do
    [ "$kills" -lt 8 ] || fail "$held_issue was not held after $kills killed implementer launches"
    until_true 240 "a new implementer launch on $held_issue checked by the pane watcher" held_kill_ready "$held_issue" "$killed"
    inc=$(claim_incarnation "$held_issue" implementer)
    pane=$(issue_worker_pane "$held_issue" implementer)
    TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-pane -t "$pane"
    killed="$killed$inc "
    kills=$((kills + 1))
    note "killed implementer launch $kills (incarnation $inc, pane $pane)"
    until_true 240 "$held_issue to be held or its implementer relaunched after kill $kills" held_or_relaunched "$held_issue" "$inc"
  done
  until_true 180 "the held tree architect to receive worker-died" notice_delivered "$held_issue" architect "$(notice_needle worker-died "$held_issue")"
  state_file held
  worker_died=$(notice_deliveries "$held_issue" architect "$(notice_needle worker-died "$held_issue")")
  [ "$worker_died" = 1 ] || fail "$held_issue's architect received $worker_died worker-died notices, want exactly one"
  relaunched=$(launches_after_failure "$(claim_token "$held_issue" implementer)")
  [ "$relaunched" = 0 ] || fail "the daemon launched the held implementer $relaunched times after failing it (-1: no failure logged)"
  assert_held_snapshot "$evidence/held.json" || fail "worker stayed relaunched instead of held after its launch budget"
  jq --arg issue "$held_issue" '.issues[$issue].phase = "implementing" | .issues[$issue].workers.implementer.claim.state = "ready"' \
    "$evidence/held.json" >"$evidence/held-relaunch-negative.json"
  expect_failure held-relaunches-after-budget assert_held_snapshot "$evidence/held-relaunch-negative.json"
  assert_held_snapshot "$evidence/held.json" || fail "held assertion did not restore after its negative control"
  note "$kills killed implementer launches spent the launch budget; $held_issue is held with no relaunch, and exactly one worker-died reached its architect"
  pass

  begin held-phase-retry-relaunches
  # The architect's retry of the held phase is the decision a failed claim waits for: the phase
  # returns and the same session relaunches with fresh budgets.
  send_agent "$held_issue" architect "Stage 3 held-worker proof: the implementer of $held_issue is held after its launch budget. Use the Go-daemon retry_or_escalate operation for $held_issue with decision retry now, then wait."
  until_true 240 "the retried held phase to return to implementing" issue_phase "$held_issue" implementing
  until_true 300 "the retried implementer to relaunch and register" issue_worker_live "$held_issue" implementer
  inc=$(claim_incarnation "$held_issue" implementer)
  case "$killed" in *" $inc "*) fail "the retried implementer still reports killed incarnation $inc" ;; esac
  note "the architect's retry returned $held_issue to implementing and relaunched its implementer (incarnation $inc, session kept)"
  pass
}

if [ -z "$from" ]; then
  primary_issue
else
  begin "development-run-from-$from"
  # The proof human closes the first root, which frees its admission slot as its sign-off would.
  set_status "$root_issue" "done"
  until_true 120 "the closed tree's architect to be suspended" tree_suspended "$root_issue"
  pr_number=${STAGE3_PR:-$(gh -R "$repo" pr list --state all --limit 1 --json number --jq '.[0].number')}
  [ -n "$pr_number" ] || fail "no smoke pull request for the credential check; set STAGE3_PR"
  note "STAGE3_FROM=$from is a development run, never the proof: $root_issue closed by the proof human, credential check against $repo#$pr_number"
  pass
fi

# restart_scenarios: the restart mid-implementer, the in-agent credentials, and the pending status
# write, all on the third root.
restart_scenarios() {
  begin restart-mid-implementer
  # The third root was waiting. Completing the first root freed its slot, so it must now be admitted
  # in rank order; this validates promotion before exercising the mid-phase restart.
  until_true 240 "the waiting root to take the freed admission slot" sh -c \
    "'$work/legion' state --json --port '$port_daemon' | jq -e --arg issue '$restart_issue' '.issues[\$issue].slot != null'"
  drive_gate "$restart_issue" "Stage 3 restart proof"
  send_agent "$restart_issue" planner "Stage 3 restart proof: write the minimal planner handoff and complete planning."
  wait_for_phase "$restart_issue" implementing
  wait_for_worker "$restart_issue" implementer
  # The same session in the same process incarnation is what "no phase restarted" means; the claim's
  # turn state (working, idle) may change across the restart as the agent finishes a turn.
  before_restart=$(daemon_state | jq -c --arg issue "$restart_issue" '.issues[$issue].workers.implementer.claim | {session, incarnation: (.locator.incarnation // "")}')
  kill -TERM "$daemon_pid"
  stop_pid "$daemon_pid"
  daemon_pid=
  # While the daemon is down, a human board write becomes a durable Dispatch event that the restored
  # consumer must process. It changes only the board status; no phase is manually advanced. It is
  # needs_review, never testing: the daemon writes no status the record already shows, so a human
  # testing here would leave the pending-status-write check below nothing to write.
  set_status "$restart_issue" needs_review
  start_daemon keep
  until_true 180 "the Dispatch status written during downtime to be consumed" sh -c \
    "'$work/legion' state --json --port '$port_daemon' | jq -e --arg issue '$restart_issue' '.issues[\$issue].status == \"needs_review\"'"
  after_restart=$(daemon_state | jq -c --arg issue "$restart_issue" '.issues[$issue].workers.implementer.claim | {session, incarnation: (.locator.incarnation // "")}')
  [ "$before_restart" = "$after_restart" ] || fail "restart relaunched the active implementer: $before_restart → $after_restart"
  issue_worker_live "$restart_issue" implementer >/dev/null || fail "the implementer claim is not live after the restart"
  issue_phase "$restart_issue" implementing >/dev/null || fail "$restart_issue left implementing across the restart"
  note "daemon restart kept the implementer's session and incarnation $after_restart and consumed the human Dispatch status written while down"
  pass

  begin in-agent-credentials
  # One bash command on purpose: the plugin writes one grant per command, so the two chained
  # `legion gh` calls prove a grant serves every redemption its command makes. Each output carries a
  # marker the instruction itself cannot produce (the instruction holds the unexpanded command), so
  # only the pane's own bash tool output satisfies a check.
  send_agent "$restart_issue" implementer "Stage 3 credential proof: run exactly this in your bash tool and include its exact output: echo \"CRED-GH=\$(command -v gh)\"; echo \"CRED-VIEWER=\$(legion gh -- api graphql -f query='{viewer{login}}' --jq .data.viewer.login)\"; echo \"CRED-PR=\$(legion gh -- pr view $pr_number --repo $repo --json url --jq .url)\"; gh pr merge $pr_number --repo $repo 2>&1 | sed 's/^/CRED-MERGE=/'. Wait after reporting."
  until_true 240 "the real Go pane bash tool to resolve worker-bin gh" session_contains "$restart_issue" implementer "CRED-GH=$state/worker-bin/gh"
  until_true 240 "the implementer pane to authenticate as its App" session_contains "$restart_issue" implementer "CRED-VIEWER=legion-implementer[bot]"
  until_true 240 "the implementer pane to read the pull request" session_contains "$restart_issue" implementer "CRED-PR=https://github.com/$repo/pull/$pr_number"
  until_true 240 "the real Go pane bash tool to refuse gh pr merge" session_contains "$restart_issue" implementer "CRED-MERGE=Legion never merges a pull request"
  note "real implementer pane resolved gh from $state/worker-bin, viewed pull request #$pr_number as legion-implementer[bot], and its plain gh pr merge was refused"
  pass

  begin pending-dispatch-status-write
  # The active implementer opens a second sandbox PR and writes its handoff while Dispatch is up, and
  # completes the phase only once it sees Dispatch gone: every instruction reaches a pane through
  # Dispatch, so none can follow the stop. testing's status write is then durably pending rather than
  # silently lost, and the board (left at the human's needs_review) catches up once Dispatch returns.
  send_agent "$restart_issue" implementer "Stage 3 outbox proof: make the smallest one-file smoke change for this issue, commit it and open pull request legion/$restart_issue in $repo, and write the implementation handoff. Before you run legion handoff complete, wait for the proof to stop the Dispatch server: run curl -fsS \"\$DISPATCH_URL/api/v1\" every 5 seconds until it fails (for up to 15 minutes), and only once it has failed run legion handoff complete. Do not post anything to Dispatch."
  until_true 900 "restart-tree pull request to open" sh -c \
    "gh -R '$repo' pr list --head 'legion/$restart_issue' --state open --json number | jq -e 'length == 1' >/dev/null"
  stop_dispatch
  until_true 600 "the stopped Dispatch server to leave testing status pending" sh -c \
    "'$work/legion' state --json --port '$port_daemon' | jq -e --arg issue '$restart_issue' '.issues[\$issue].phase == \"testing\" and any(.pendingStatusWrites[]; .issue == \$issue)'"
  state_file pending-status-write
  start_dispatch keep
  until_true 180 "the restarted scratch Dispatch board to catch up" dispatch_status_is "$restart_issue" testing
  note "testing transition survived Dispatch downtime as a pending status write and the board caught up after Dispatch returned"
  pass
}

# The first issue's whole history is the durable proof of writers: a development run never drove
# it, so it has no history to check.
status_actors() {
  begin status-actors-are-daemon-only
  # The Go client attributes its status writes to the stable daemon session, while the proof
  # human's own writes are never lifecycle writes.
  dispatch_events "$root_issue" >"$evidence/root-events.json"
  assert_status_actors "$evidence/root-events.json" || fail "a lifecycle status was not written by legion-daemon:$project"
  # The control re-attributes exactly one write, the first move to testing, to an agent session.
  jq '(map(select(.type == "issue.updated" and .payload.status == "testing")) | first | .seq) as $seq
    | map(if .seq == $seq then .actor.id = "agent-wrote-status" else . end)' \
    "$evidence/root-events.json" >"$evidence/status-agent-negative.json"
  expect_failure status-actor-check assert_status_actors "$evidence/status-agent-negative.json"
  assert_status_actors "$evidence/root-events.json" || fail "status actor assertion did not restore after its negative control"
  note "every lifecycle transition on $root_issue ($(jq -r '[.[] | select(.type | IN("issue.updated", "issue.closed")) | .payload.status] | join(" ")' "$evidence/root-events.json")) records actor legion-daemon:$project; one agent-attributed write was rejected"
  pass
}
if [ -z "$until" ]; then
  [ "$from" = restart ] || held_worker
  restart_scenarios
  [ -n "$from" ] || status_actors
fi

begin model-turns-through-the-gateway
# Every agent turn of the run, each subagent's included, ran as the profile's pinned model on the
# anthropic provider: the one provider the profile routes (to the gateway) and leaves enabled. Live
# agents may be mid-write, so a line that does not parse yet is skipped.
pinned=$(sed -n 's/^  default: //p' "$HOME/.omp/profiles/$profile/agent/config.yml")
mapfile -t sessions < <(find "$HOME/.omp/profiles/$profile/agent/sessions" -name '*.jsonl' -type f)
[ "${#sessions[@]}" -gt 0 ] || fail "the isolated OMP profile holds no agent session"
off_route=$(jq -R -r --arg pinned "$pinned" '
  fromjson? | select((.type == "message" and .message.role == "assistant"
      and ((.message.provider // "") + "/" + (.message.model // "")) != $pinned)
    or (.type == "model_change" and .model != $pinned))
  | input_filename' "${sessions[@]}" | sort -u)
[ -z "$off_route" ] || fail "sessions with a turn or model off $pinned: $(tr '\n' ' ' <<<"$off_route")"
turns=$(jq -R -c 'fromjson? | select(.type == "message" and .message.role == "assistant")' "${sessions[@]}" | wc -l)
[ "$turns" -gt 0 ] || fail "no agent session holds an assistant turn"
note "$turns assistant turns in ${#sessions[@]} agent sessions, every one $pinned on the gateway route; the key command minted $(grep -c ' invoked by pid ' "$evidence/model-gateway/hawk-token.log") times ($evidence/model-gateway/hawk-token.log)"
pass

begin production-untouched
"$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" >"$evidence/claims.json"
every_launch_checked
note "$(wc -l <"$evidence/pane-endpoints-checked.txt") launched panes endpoint-checked when their OMP started, before registration"
if ! production_audit; then
  fail "the rig touched production; see $evidence/production-dispatch-audit.json and $evidence/production-envoy-rig-sessions.json"
fi
pass

begin cleanup-is-complete
# Stop the remaining services explicitly and prove every named resource is gone. Transcripts are
# copied into the evidence directory before the isolated OMP profile is removed.
stop_pid "$watcher_pid"; watcher_pid=
stop_pid "$daemon_pid"; daemon_pid=
stop_dispatch
stop_pid "$listener_pid"; listener_pid=
stop_pid "$bridge_pid"; bridge_pid=
TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
docker rm -f "$pg_container" "$nats_container" >/dev/null 2>&1 || true
for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
[ "$(docker ps -aq --filter "name=^/$pg_container$" --filter "name=^/$nats_container$")" = "" ] || fail "a proof container remains"
TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" has-session 2>/dev/null && fail "the proof tmux server remains"
[ -z "$(run_processes)" ] || fail "a proof process remains"
collect_transcripts
rm -rf "$HOME/.omp/profiles/$profile"
[ ! -e "$HOME/.omp/profiles/$profile" ] || fail "the isolated OMP profile remains"
transcripts=$(find "$evidence/transcripts" -name '*.jsonl' -type f | wc -l)
[ "$transcripts" -gt 0 ] || fail "no agent transcript reached $evidence/transcripts"
[ -s "$evidence/logs/daemon.log" ] || fail "the daemon log is missing from $evidence/logs"
rm -rf "$work"
[ ! -e "$work" ] || fail "the scratch work directory $work remains"
note "no proof container, process, tmux server, OMP profile, or work directory ($work, which held the agents' workspaces) remains; the scratch Dispatch database lived in the removed Postgres container"
note "evidence kept at $evidence: $transcripts agent transcripts, daemon, Dispatch, listener, and bridge logs, state captures, negative controls, and the production audit"
pass

ok=1
if [ -n "$development" ]; then
  echo "stage 3 e2e: development run $development finished (not the proof)"
else
  echo "stage 3 e2e: PASS"
fi
