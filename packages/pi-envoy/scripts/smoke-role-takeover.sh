#!/usr/bin/env bash
# smoke-role-takeover.sh — live proof that a soft role claim naming a predecessor takes the role
# only from a predecessor that is gone or is the claimant's own client instance (INFRA-404, R8 and
# A5 of dispatch://INFRA-404/spec).
#
# It builds natsd (packages/envoy/internal/testnats/cmd/natsd) and the listener
# (packages/envoy/cmd/listener) from this checkout with `go build` and runs both on loopback ports
# of its own, exactly as packages/envoy-client/test/envoy-stack.ts does, and then drives each row
# of R8's table with real client processes:
#
#   - one Bun process per Oh My Pi Envoy extension instance (scripts/role-takeover/omp-client.ts),
#     each loading this checkout's extensions/envoy.ts against the stand-in host the package's own
#     suite uses, so each process is one client instance with one client instance id;
#   - the real Claude Code channel server (packages/claude-envoy/bin/envoy-channel.ts), one per
#     `claude` process, spawned and driven over MCP by scripts/role-takeover/claude-client.ts,
#     which is also the parent whose handoff file a `/clear` writes.
#
# No model provider, no credential and no container: `go` and `bun`. Every client reaches the
# listener through a recording proxy of its own (scripts/role-takeover/listener-proxy.ts), so what
# each client sent and what the listener answered it is read from a log rather than inferred, and
# a request can be held: A5's first scenario holds a fork's role claim back until another process
# has resumed and registered its predecessor.
#
# The rows, each printed with its outcome:
#
#   R8 row 1  a claim under the id that holds the role keeps it — an Oh My Pi plugin reload, and a
#             Claude Code channel server restart, each under the session id that holds the role.
#   R8 row 2  an Oh My Pi fork gets its parent's role: the predecessor's live entry carries the
#             claimant's own client instance id (R6 case 2).
#   R8 row 3  an Oh My Pi move, in one process, to a session whose records name the held role gets
#             it, by the same case.
#   R8 row 4  a Claude Code `/clear` handoff gets the role: the deregistration removes the
#             process's own entry (R7), and the claim then finds no live predecessor (R6 case 1).
#   R8 row 5  each of those three after another process resumed the predecessor id and registered
#             it last is refused 409 naming the predecessor, and that process keeps the role. Its
#             Oh My Pi fork case is A5's first scenario and its Claude Code case A5's second.
#   A5        a refused process stays registered, and after the holder exits the refused Oh My Pi
#             process takes the role on its next session event.
#
# `--base-rev <rev>` builds the listener from that revision instead of the working copy, leaving
# every client as it is here. Against `main@origin` the two A5 scenarios (and row 5's third case,
# which is the same rule) must fail, because that listener takes a role from any predecessor a
# claim names, live or not. It reads the revision without touching this working copy: `jj
# --ignore-working-copy log` resolves it and `git archive` extracts packages/envoy.
#
# Exit status: 0 PASS (every row held); 1 FAIL (the failing rows are listed); 2 refused (a
# prerequisite is missing or the configuration is invalid); 129, 130, 143 on SIGHUP, SIGINT,
# SIGTERM. See scripts/README.md.
set -euo pipefail

# The Claude Code channel server heartbeats on a timer of its own; the Oh My Pi extension's
# heartbeat is this script's to tick, so its interval is set past the run. A holder that must not
# re-register after another process took over its entry gets the long one.
readonly claude_heartbeat_ms=1000
readonly claude_quiet_heartbeat_ms=3600000
readonly omp_heartbeat_ms=600000
readonly curl_max_time=20
readonly client_reply_timeout=180
readonly listener_wait_timeout=60

refuse() {
  printf 'smoke-role-takeover: %s\n' "$1" >&2
  exit 2
}

note() {
  printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*"
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "${script_dir}/../../.." && pwd)"
readonly repo_root
readonly drivers="${script_dir}/role-takeover"
readonly channel_server="${repo_root}/packages/claude-envoy/bin/envoy-channel.ts"

# The loopback stack this smoke shares with smoke-registration-stall.sh and smoke-replay.sh: the
# ports, the two builds, what each service is run as and when it is up. `pick_port` reports an
# exhausted range the way everything else here refuses.
local_stack_fail=refuse
# shellcheck source=packages/pi-envoy/scripts/local-stack.sh
source "${script_dir}/local-stack.sh"

base_rev=""
while (($# > 0)); do
  case "$1" in
    --base-rev)
      base_rev="${2:-}"
      shift 2 || refuse "--base-rev needs a revision"
      [[ -n "$base_rev" ]] || refuse "--base-rev needs a revision"
      ;;
    *)
      refuse "unknown argument: $1"
      ;;
  esac
done
readonly base_rev

required=(go bun curl jq)
[[ -z "$base_rev" ]] || required+=(jj git tar)
for tool in "${required[@]}"; do
  command -v "$tool" >/dev/null 2>&1 || refuse "${tool} is required"
done
for driver in listener-proxy.ts omp-client.ts host-stub.ts claude-client.ts; do
  [[ -r "${drivers}/${driver}" ]] || refuse "${drivers}/${driver} is not readable"
done
[[ -r "${repo_root}/packages/pi-envoy/extensions/envoy.ts" ]] ||
  refuse "the Oh My Pi extension under test is not readable"
[[ -r "$channel_server" ]] ||
  refuse "the Claude Code channel server (${channel_server}) is not readable"
[[ -f "${repo_root}/node_modules/nats/package.json" ]] ||
  refuse "${repo_root}/node_modules has no nats package: run bun install in the checkout"

port_range

# ---- scratch, processes and teardown ----------------------------------------------------------
work="$(mktemp -d "${TMPDIR:-/tmp}/smoke-role-takeover.XXXXXX")"
readonly work
mkdir -p "${work}/logs" "${work}/bin" "${work}/proxies"
readonly run_id="${RANDOM}${RANDOM}"

# Every process this run started, and every stdin this shell holds open for a client.
service_pids=()
scenario_pids=()
scenario_fds=()
passed=0

stop_pid() {
  local pid=${1:-} i
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 0
  kill -TERM "$pid" 2>/dev/null || return 0
  for ((i = 0; i < 25; i++)); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
  done
  kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# end_scenario: stops every client and proxy the scenario started, so at most one scenario's
# processes are alive at a time.
end_scenario() {
  local fd pid
  for fd in "${scenario_fds[@]:-}"; do
    [[ -z "$fd" ]] || eval "exec ${fd}>&-" 2>/dev/null || true
  done
  scenario_fds=()
  for pid in "${scenario_pids[@]:-}"; do stop_pid "$pid"; done
  scenario_pids=()
}

cleanup() {
  local status=$?
  trap - EXIT
  set +e
  end_scenario
  local pid
  for pid in "${service_pids[@]:-}"; do stop_pid "$pid"; done
  if ((passed == 1)); then
    rm -rf "$work"
  else
    printf 'smoke-role-takeover: kept %s (each client'"'"'s stdout, the listener and natsd logs, and every proxy'"'"'s request log)\n' "$work"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# wait_for SECONDS COMMAND...: polls COMMAND every 0.2 s; 1 once SECONDS have passed.
wait_for() {
  local seconds=$1
  shift
  poll_until "$seconds" 0.2 "$@"
}

# ---- the results -------------------------------------------------------------------------------
# One row of R8's table (or one of A5's own claims), its outcome and the evidence for it. A failed
# check never stops the run: every row is driven, so one run says which rows hold and which do not.
row_names=()
row_outcomes=()
row_details=()
current_row=""
current_failure=""
current_evidence=""

begin_row() {
  current_row="$1"
  current_failure=""
  current_evidence=""
  note "== ${current_row}"
}

# expect WHAT ACTUAL EXPECTED: records the first failure of the row and goes on.
expect() {
  if [[ "$2" == "$3" ]]; then
    note "   ok: $1 — ${2}"
    return 0
  fi
  note "   FAILED: $1 — got ${2:-<empty>}, want ${3}"
  [[ -n "$current_failure" ]] || current_failure="$1 — got ${2:-<empty>}, want ${3}"
}

# expect_true WHAT COMMAND...: the same for a condition with no value to print.
expect_true() {
  local what="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    note "   ok: ${what}"
    return 0
  fi
  note "   FAILED: ${what}"
  [[ -n "$current_failure" ]] || current_failure="$what"
}

evidence() {
  current_evidence="$1"
}

end_row() {
  row_names+=("$current_row")
  if [[ -n "$current_failure" ]]; then
    row_outcomes+=("FAIL")
    row_details+=("$current_failure")
  else
    row_outcomes+=("PASS")
    row_details+=("$current_evidence")
  fi
  current_row=""
}

# ---- the services ------------------------------------------------------------------------------
note "scratch ${work}"
note "bun $(bun --version), $(go version)"

listener_source="${repo_root}/packages/envoy"
if [[ -n "$base_rev" ]]; then
  base_commit="$(jj -R "$repo_root" --ignore-working-copy log -r "$base_rev" --no-graph -T commit_id 2>/dev/null)" ||
    refuse "jj could not resolve --base-rev ${base_rev} in ${repo_root}"
  [[ "$base_commit" =~ ^[0-9a-f]{40}$ ]] ||
    refuse "--base-rev ${base_rev} resolved to '${base_commit}', which is not one commit id"
  mkdir -p "${work}/base"
  # A read of the object store alone: it moves no bookmark and never touches this working copy.
  git -C "$repo_root" archive "$base_commit" packages/envoy | tar -x -C "${work}/base" ||
    refuse "git archive ${base_commit} packages/envoy failed"
  listener_source="${work}/base/packages/envoy"
  note "the listener is built from ${base_rev} (${base_commit}); every client is this working copy's"
fi
readonly listener_source

note "building natsd from ${repo_root}/packages/envoy and the listener from ${listener_source}"
build_natsd "${repo_root}/packages/envoy" "${work}/bin/natsd" ||
  refuse "go build of natsd failed (the reason is above)"
build_listener "$listener_source" "${work}/bin/listener" ||
  refuse "go build of the listener failed (the reason is above)"

pick_port nats_port
pick_port monitor_port
pick_port listener_port
readonly nats_port monitor_port listener_port
readonly envoy_url="http://127.0.0.1:${listener_port}"
readonly nats_url="nats://127.0.0.1:${nats_port}"

set_natsd_argv "${work}/bin/natsd" "$nats_port" "$monitor_port" "${work}/nats-store"
(exec env -i PATH="$PATH" HOME="${work}/nats-home" "${natsd_argv[@]}") \
  >"${work}/logs/natsd.log" 2>&1 &
service_pids+=($!)
wait_for 60 natsd_serving "${work}/logs/natsd.log" ||
  refuse "natsd did not start: $(tail -n 5 "${work}/logs/natsd.log")"
note "natsd serves ${nats_url} (monitoring 127.0.0.1:${monitor_port})"

set_listener_env "$listener_port" "$nats_url" "smoke-role-takeover-$$"
(exec env -i PATH="$PATH" HOME="${work}/listener-home" "${listener_env[@]}" \
  "${work}/bin/listener") >"${work}/logs/listener.log" 2>&1 &
service_pids+=($!)
wait_for 90 listener_healthy "$envoy_url" ||
  refuse "the listener did not become healthy: $(tail -n 5 "${work}/logs/listener.log")"
note "the listener is healthy at ${envoy_url}"

listener_get() {
  curl -sS -o "${work}/answer.json" -w '%{http_code}' --max-time "$curl_max_time" "${envoy_url}$1" 2>/dev/null ||
    printf '000'
}

# role_holder ROLE: the session id the listener names as the role's holder, empty for none.
role_holder() {
  local code
  code="$(listener_get "/v1/roles/$1")"
  [[ "$code" == 200 ]] || return 0
  jq -r '.holder // ""' "${work}/answer.json"
}

registered() {
  [[ "$(listener_get "/v1/registry/$1")" == 200 ]]
}

unregistered() {
  [[ "$(listener_get "/v1/registry/$1")" == 404 ]]
}

# ---- the clients ---------------------------------------------------------------------------------
declare -A proxy_url proxy_log client_fd client_out client_pid
next_command_id=0

# start_proxy LABEL: this client's own HTTP path to the listener, recording every request.
start_proxy() {
  local label="$1" port
  pick_port port
  proxy_log["$label"]="${work}/proxies/${label}.jsonl"
  : >"${proxy_log[$label]}"
  (exec env -i PATH="$PATH" HOME="${work}/bun-home" TMPDIR="${TMPDIR:-/tmp}" \
    bun "${drivers}/listener-proxy.ts" --port "$port" --upstream "$envoy_url" \
    --log "${proxy_log[$label]}" --label "$label") >>"${work}/logs/proxy-${label}.log" 2>&1 &
  scenario_pids+=($!)
  proxy_url["$label"]="http://127.0.0.1:${port}"
  wait_for 30 port_open "$port" ||
    refuse "the ${label} proxy did not listen on 127.0.0.1:${port}: $(tail -n 5 "${work}/logs/proxy-${label}.log")"
}

# client_fifo LABEL: the stdin this shell holds open for the client, so it never sees end of input.
client_fifo() {
  local label="$1" fifo="${work}/${1}.stdin" fd
  mkfifo "$fifo"
  exec {fd}<>"$fifo"
  client_fd["$label"]="$fd"
  scenario_fds+=("$fd")
  client_out["$label"]="${work}/${label}.out"
  : >"${client_out[$label]}"
}

# client_ready LABEL: the client's first line, which it writes once it is up.
client_ready() {
  grep -q '"ready":true' "${client_out[$1]}" 2>/dev/null
}

# start_omp LABEL: one Oh My Pi Envoy extension instance in a process of its own, pointed at this
# label's proxy and at natsd. `env -i` keeps the operator's own ENVOY_*, NATS_* and DISPATCH_*
# variables out of it, so the configuration is this script's alone.
start_omp() {
  local label="$1" home="${work}/${1}-home"
  start_proxy "$label"
  mkdir -p "${home}/cwd"
  jq -n --arg url "${proxy_url[$label]}" --arg nats "$nats_url" \
    '{envoy: {url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}, askgate: {enabled: false}}' \
    >"${home}/envoy.json"
  client_fifo "$label"
  (cd -- "${repo_root}/packages/pi-envoy" &&
    exec env -i PATH="$PATH" HOME="$home" TMPDIR="${TMPDIR:-/tmp}" \
      ENVOY_CONFIG_FILE="${home}/envoy.json" ENVOY_HEARTBEAT_MS="$omp_heartbeat_ms" \
      ENVOY_PASS_DEADLINE_MS="$omp_heartbeat_ms" ROLE_TAKEOVER_CWD="${home}/cwd" \
      ROLE_TAKEOVER_CLIENT_LOG="${work}/logs/${label}-notices.log" \
      bun --preload "${drivers}/host-stub.ts" "${drivers}/omp-client.ts") \
    <"${work}/${label}.stdin" >"${client_out[$label]}" 2>"${work}/logs/${label}.err" &
  client_pid["$label"]=$!
  scenario_pids+=($!)
  wait_for 90 client_ready "$label" ||
    refuse "the ${label} Oh My Pi client did not start: $(tail -n 10 "${work}/logs/${label}.err")"
}

# start_claude LABEL SESSION STATE_DIR HEARTBEAT_MS: one `claude` process and the real channel
# server under it. Two labels given one STATE_DIR are two `claude` processes of one user, which
# share one plugin data directory, as they do on a real machine.
start_claude() {
  local label="$1" session="$2" state="$3" heartbeat="$4" config="${work}/${1}-claude-config"
  start_proxy "$label"
  mkdir -p "$config" "$state"
  jq -n --arg url "${proxy_url[$label]}" --arg nats "$nats_url" \
    '{envoy: {enabled: true, url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}}' \
    >"${config}/envoy.json"
  client_fifo "$label"
  (cd -- "${repo_root}/packages/claude-envoy" &&
    exec env -i PATH="$PATH" HOME="${work}/bun-home" TMPDIR="${TMPDIR:-/tmp}" \
      bun "${drivers}/claude-client.ts" --server "$channel_server" --session "$session" \
      --state-dir "$state" --config-dir "$config" --heartbeat-ms "$heartbeat" \
      --stderr "${work}/logs/${label}-channel.err") \
    <"${work}/${label}.stdin" >"${client_out[$label]}" 2>"${work}/logs/${label}.err" &
  client_pid["$label"]=$!
  scenario_pids+=($!)
  wait_for 120 client_ready "$label" ||
    refuse "the ${label} Claude Code channel server did not start: $(tail -n 10 "${work}/logs/${label}.err") $(tail -n 10 "${work}/logs/${label}-channel.err" 2>/dev/null)"
}

reply=""
# send LABEL JSON: one command, answered on the client's stdout. The answer is left in `reply`.
send() {
  local label="$1" body="$2" id
  next_command_id=$((next_command_id + 1))
  id="c${next_command_id}"
  printf '%s\n' "$(jq -c --arg id "$id" '. + {id: $id}' <<<"$body")" >&"${client_fd[$label]}"
  local answered=()
  answered=(grep -q "\"id\":\"${id}\"" "${client_out[$label]}")
  wait_for "$client_reply_timeout" "${answered[@]}" ||
    refuse "the ${label} client did not answer ${body} within ${client_reply_timeout}s: $(tail -n 10 "${work}/logs/${label}.err")"
  reply="$(grep -F "\"id\":\"${id}\"" "${client_out[$label]}" | tail -n 1)"
  jq -e '.ok == true' <<<"$reply" >/dev/null ||
    refuse "the ${label} client refused ${body}: ${reply}"
}

# ---- reading what a client sent ------------------------------------------------------------------
# claim_status LABEL SESSION PREVIOUS: the status the listener answered that client's last soft
# claim for SESSION naming PREVIOUS (empty PREVIOUS: a claim naming none), empty when it made none.
claim_status() {
  jq -r --arg sid "$2" --arg previous "$3" \
    'select(.path == "/v1/roles/set" and .request.session_id == $sid
            and (.request.previous_session_id // "") == $previous) | .status' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

# claim_holder LABEL SESSION PREVIOUS: the holder the listener named in refusing that claim.
claim_holder() {
  jq -r --arg sid "$2" --arg previous "$3" \
    'select(.path == "/v1/roles/set" and .request.session_id == $sid
            and (.request.previous_session_id // "") == $previous) | .response.holder // ""' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

# instance_id LABEL SESSION: the client instance id that client last registered SESSION with.
instance_id() {
  jq -r --arg sid "$2" \
    'select(.path == "/v1/interests/subscribe" and .request.session_id == $sid)
     | .request.client_instance_id // ""' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

# registered_at LABEL SESSION: when that client last registered SESSION, 0 for never.
registered_at() {
  local at
  at="$(jq -r --arg sid "$2" \
    'select(.path == "/v1/interests/subscribe" and .request.session_id == $sid) | .at' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1)"
  printf '%s' "${at:-0}"
}

# delete_answer LABEL SESSION FIELD: a field of the listener's answer to that client's
# deregistration of SESSION. `//` would read a false `deleted` as absent, so the field is taken
# by name and rendered whatever its type.
delete_answer() {
  jq -r --arg sid "/v1/sessions/$2" --arg field "$3" \
    'select(.method == "DELETE" and .path == $sid)
     | (.response // {})
     | if type == "object" and has($field) then (.[$field] | tostring) else "" end' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

made_claim() {
  [[ -n "$(claim_status "$1" "$2" "$3")" ]]
}

held_at_proxy() {
  [[ "$(curl -sS --max-time 5 "${proxy_url[$1]}/__smoke/held?path=/v1/roles/set" | jq -r '.held')" -ge 1 ]]
}

# ---- the rows -----------------------------------------------------------------------------------
role_for() {
  printf 'smoke-takeover-%s-%s' "$1" "$run_id"
}

session_for() {
  printf 'ses_smoke_%s_%s' "$1" "$run_id"
}

# R8 row 1, Oh My Pi: the extension instance goes away without deregistering (a plugin reload
# leaves the session's registration standing) and a new instance comes up on the same session id,
# whose transcript records the claim. Its claim is under the id that holds the role.
row1_omp() {
  local role session first second
  role="$(role_for r1omp)"
  session="$(session_for r1omp)"
  begin_row "R8 row 1 — an Oh My Pi plugin reload claims under the id that holds the role"
  start_omp omp-reload-1
  send omp-reload-1 "$(jq -nc --arg s "$session" '{op:"start",session:$s,roles:[]}')"
  send omp-reload-1 "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "the first instance holds the role" "$(role_holder "$role")" "$session"
  first="$(instance_id omp-reload-1 "$session")"
  # SIGKILL, so the registration outlives the instance, as it does across a reload.
  kill -KILL "${client_pid[omp-reload-1]}" 2>/dev/null || true
  wait "${client_pid[omp-reload-1]}" 2>/dev/null || true
  expect_true "the session is still registered after the instance is gone" registered "$session"

  start_omp omp-reload-2
  send omp-reload-2 "$(jq -nc --arg s "$session" --arg r "$role" '{op:"start",session:$s,roles:[$r]}')"
  second="$(instance_id omp-reload-2 "$session")"
  expect "the reloaded instance is another client instance" \
    "$([[ -n "$second" && "$second" != "$first" ]] && echo different || echo same)" "different"
  expect "its claim under the holder's own id is granted" "$(claim_status omp-reload-2 "$session" "")" "200"
  expect "the session keeps the role" "$(role_holder "$role")" "$session"
  evidence "${session} kept ${role} across a reload from client instance ${first} to ${second}"
  end_row
  end_scenario
}

# R8 row 1, Claude Code: the channel server is killed and another starts on the same session id,
# reading the role record this one wrote. Its claim names that same id as its predecessor, which
# is the id that holds the role.
row1_claude() {
  local role session state first second
  role="$(role_for r1cc)"
  session="$(session_for r1cc)"
  state="${work}/r1cc-plugin-data"
  begin_row "R8 row 1 — a Claude Code channel server restart claims under the id that holds the role"
  start_claude cc-restart-1 "$session" "$state" "$claude_quiet_heartbeat_ms"
  expect_true "the first channel server registered" wait_for "$listener_wait_timeout" registered "$session"
  send cc-restart-1 "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "the first channel server holds the role" "$(role_holder "$role")" "$session"
  first="$(instance_id cc-restart-1 "$session")"
  send cc-restart-1 '{"op":"kill"}'
  expect_true "the session is still registered after the server is gone" registered "$session"

  start_claude cc-restart-2 "$session" "$state" "$claude_quiet_heartbeat_ms"
  # A listener that grants the claim and one that refuses it both answer it; a listener that
  # never sees one leaves the status below empty, which is the row's failure to report.
  wait_for "$listener_wait_timeout" made_claim cc-restart-2 "$session" "$session" || true
  second="$(instance_id cc-restart-2 "$session")"
  expect "the restarted server is another client instance" \
    "$([[ -n "$second" && "$second" != "$first" ]] && echo different || echo same)" "different"
  expect "its claim under the holder's own id is granted" "$(claim_status cc-restart-2 "$session" "$session")" "200"
  expect "the session keeps the role" "$(role_holder "$role")" "$session"
  evidence "${session} kept ${role} across a channel server restart from client instance ${first} to ${second}"
  end_row
  end_scenario
}

# R8 row 2: a fork mints a new id in the same process, and its branch is a copy of its parent's,
# so it records the claim. The claim names the parent, whose live entry carries this same client
# instance id (R6 case 2).
row2_fork() {
  local role parent child
  role="$(role_for r2)"
  parent="$(session_for r2parent)"
  child="$(session_for r2child)"
  begin_row "R8 row 2 — an Oh My Pi fork gets its parent's role"
  start_omp omp-fork
  send omp-fork "$(jq -nc --arg s "$parent" '{op:"start",session:$s,roles:[]}')"
  send omp-fork "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "the parent holds the role" "$(role_holder "$role")" "$parent"
  send omp-fork "$(jq -nc --arg s "$child" '{op:"switch",session:$s,reason:"fork"}')"
  expect "the fork claims naming its parent" "$(claim_status omp-fork "$child" "$parent")" "200"
  expect "the fork holds the role" "$(role_holder "$role")" "$child"
  expect_true "the parent's entry carried this instance's id" \
    test "$(instance_id omp-fork "$parent")" = "$(instance_id omp-fork "$child")"
  evidence "${child} took ${role} from ${parent}, both registered by client instance $(instance_id omp-fork "$child")"
  end_row
  end_scenario
}

# R8 row 3: a move in the same process to a session whose records name the held role. The claim
# names the id the role was held under, whose live entry is this same client instance's.
row3_move() {
  local role held moved
  role="$(role_for r3)"
  held="$(session_for r3held)"
  moved="$(session_for r3moved)"
  begin_row "R8 row 3 — an Oh My Pi move to a session whose records name the held role gets it"
  start_omp omp-move
  send omp-move "$(jq -nc --arg s "$held" '{op:"start",session:$s,roles:[]}')"
  send omp-move "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "the first session holds the role" "$(role_holder "$role")" "$held"
  send omp-move "$(jq -nc --arg s "$moved" --arg r "$role" '{op:"switch",session:$s,reason:"resume",roles:[$r]}')"
  expect "the move claims naming the id the role was held under" "$(claim_status omp-move "$moved" "$held")" "200"
  expect "the session moved to holds the role" "$(role_holder "$role")" "$moved"
  evidence "${moved} took ${role} from ${held} inside one client instance $(instance_id omp-move "$moved")"
  end_row
  end_scenario
}

# R8 row 4: a `/clear` mints a new id in one channel server process. The handoff deregisters the
# old id carrying this process's client instance id, which removes its own entry (R7), and the
# claim then finds no live predecessor (R6 case 1).
row4_handoff() {
  local role before after state
  role="$(role_for r4)"
  before="$(session_for r4before)"
  after="$(session_for r4after)"
  state="${work}/r4-plugin-data"
  begin_row "R8 row 4 — a Claude Code /clear handoff gets the role"
  start_claude cc-handoff "$before" "$state" "$claude_quiet_heartbeat_ms"
  expect_true "the channel server registered" wait_for "$listener_wait_timeout" registered "$before"
  send cc-handoff "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "the session before the /clear holds the role" "$(role_holder "$role")" "$before"
  send cc-handoff "$(jq -nc --arg s "$after" '{op:"clear",session:$s}')"
  wait_for "$listener_wait_timeout" made_claim cc-handoff "$after" "$before" || true
  # What this row turns on is that the predecessor's entry is this process's own and goes, not
  # the body R7 added to say so; the body is in the evidence below.
  expect_true "the deregistration removed this process's own entry" unregistered "$before"
  expect "the claim naming the pre-/clear id is granted" "$(claim_status cc-handoff "$after" "$before")" "200"
  expect "the session after the /clear holds the role" "$(role_holder "$role")" "$after"
  evidence "${after} took ${role} from ${before} after deregistering it as client instance $(instance_id cc-handoff "$after") (the listener answered deleted=$(delete_answer cc-handoff "$before" deleted))"
  end_row
  end_scenario
}

# R8 row 5, Oh My Pi (A5 scenario 1), then row 5's move case, then A5's two remaining claims. One
# story of two processes: A holds the role under X and forks to Y with its claim held at its own
# proxy; B resumes X, registers it and holds the role under X; A's claim is then released.
row5_omp() {
  local role x y z instance_a instance_b a_at b_at
  role="$(role_for r5omp)"
  x="$(session_for r5x)"
  y="$(session_for r5y)"
  z="$(session_for r5z)"
  begin_row "R8 row 5 (A5 scenario 1) — an Oh My Pi fork after another process resumed and registered its predecessor is refused"
  start_omp omp-a
  send omp-a "$(jq -nc --arg s "$x" '{op:"start",session:$s,roles:[]}')"
  send omp-a "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "process A holds the role under X" "$(role_holder "$role")" "$x"
  instance_a="$(instance_id omp-a "$x")"
  a_at="$(registered_at omp-a "$x")"

  curl -fsS --max-time 5 -X POST "${proxy_url[omp-a]}/__smoke/hold?path=/v1/roles/set" >/dev/null ||
    refuse "could not arm the hold on process A's proxy"
  send omp-a "$(jq -nc --arg s "$y" '{op:"switch",session:$s,reason:"fork",detach:true}')"
  wait_for 60 held_at_proxy omp-a ||
    refuse "process A's claim for ${y} never reached its proxy to be held"
  note "   process A's claim for ${y} is held at its proxy"

  start_omp omp-b
  send omp-b "$(jq -nc --arg s "$x" --arg r "$role" '{op:"start",session:$s,roles:[$r]}')"
  instance_b="$(instance_id omp-b "$x")"
  b_at="$(registered_at omp-b "$x")"
  expect "process B is another client instance" \
    "$([[ -n "$instance_b" && "$instance_b" != "$instance_a" ]] && echo different || echo same)" "different"
  expect "process B registered X last" "$([[ "$b_at" -gt "$a_at" ]] && echo later || echo earlier)" "later"
  expect "process B holds the role under X" "$(role_holder "$role")" "$x"

  curl -fsS --max-time 5 -X POST "${proxy_url[omp-a]}/__smoke/release?path=/v1/roles/set" >/dev/null ||
    refuse "could not release the hold on process A's proxy"
  send omp-a '{"op":"join"}'
  expect "process A's fork claim naming X is refused" "$(claim_status omp-a "$y" "$x")" "409"
  expect "the refusal names X as the holder" "$(claim_holder omp-a "$y" "$x")" "$x"
  expect "process B keeps the role" "$(role_holder "$role")" "$x"
  evidence "${y} was refused ${role} (409 naming ${x}); client instance ${instance_b} kept it, ${instance_a} did not take it"
  end_row

  begin_row "R8 row 5 — an Oh My Pi move after another process resumed and registered its predecessor is refused"
  send omp-a "$(jq -nc --arg s "$z" --arg r "$role" '{op:"tree",session:$s,roles:[$r]}')"
  expect "process A's move claim naming X is refused" "$(claim_status omp-a "$z" "$x")" "409"
  expect "the refusal names X as the holder" "$(claim_holder omp-a "$z" "$x")" "$x"
  expect "process B keeps the role" "$(role_holder "$role")" "$x"
  evidence "${z} was refused ${role} (409 naming ${x}) in the same process that was refused for ${y}"
  end_row

  begin_row "A5 — a refused Oh My Pi process stays registered"
  expect_true "the refused session is registered" registered "$z"
  # The session this process moved away from is gone at once, not left behind: dispatch://INFRA-392
  # makes a session id a process leaves gone the moment it leaves it, so that no peer reads a dead
  # id as live for the live window and as unresponsive for the gone horizon after that. A5's claim
  # is that being refused a role does not take the process out of the registry, and the session it
  # now holds is where that is read.
  expect_true "the session it was refused for first is gone" unregistered "$y"
  evidence "${z} is in the listener's registry with no role, and ${y}, which this process left, is gone"
  end_row

  begin_row "A5 — after the holder exits, the refused Oh My Pi process takes the role on its next session event"
  send omp-b '{"op":"shutdown"}'
  expect_true "process B's session left the registry when it shut down" \
    wait_for "$listener_wait_timeout" unregistered "$x"
  send omp-a "$(jq -nc --arg s "$z" --arg r "$role" '{op:"tree",session:$s,roles:[$r]}')"
  expect "the refused process now holds the role" "$(role_holder "$role")" "$z"
  evidence "${z} took ${role} on its next session event once ${x} had left the registry"
  end_row
  end_scenario
}

# R8 row 5, Claude Code (A5 scenario 2). A holds the role under X and registers X for the last
# time; B resumes X, registers it and holds the role under X; A then runs a `/clear` handoff.
row5_claude() {
  local role x x2 state instance_a instance_b a_at b_at record before_mtime
  role="$(role_for r5cc)"
  x="$(session_for r5ccx)"
  x2="$(session_for r5ccx2)"
  state="${work}/r5cc-plugin-data"
  record="${state}/roles/${x}.json"
  begin_row "R8 row 5 (A5 scenario 2) — a Claude Code /clear handoff after another process resumed and registered its predecessor is refused"
  start_claude cc-a "$x" "$state" "$claude_quiet_heartbeat_ms"
  expect_true "process A's channel server registered" wait_for "$listener_wait_timeout" registered "$x"
  send cc-a "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "process A holds the role under X" "$(role_holder "$role")" "$x"
  instance_a="$(instance_id cc-a "$x")"
  a_at="$(registered_at cc-a "$x")"

  # B shares A's plugin data directory, as two `claude` processes of one user do, so it reads the
  # role record A wrote for X and claims it back under that same id.
  start_claude cc-b "$x" "$state" "$claude_heartbeat_ms"
  wait_for "$listener_wait_timeout" made_claim cc-b "$x" "$x" || true
  instance_b="$(instance_id cc-b "$x")"
  b_at="$(registered_at cc-b "$x")"
  expect "process B is another client instance" \
    "$([[ -n "$instance_b" && "$instance_b" != "$instance_a" ]] && echo different || echo same)" "different"
  expect "process B registered X last" "$([[ "$b_at" -gt "$a_at" ]] && echo later || echo earlier)" "later"
  expect "process B holds the role under X" "$(role_holder "$role")" "$x"

  send cc-a "$(jq -nc --arg s "$x2" '{op:"clear",session:$s}')"
  # A listener that refuses the handoff's claim leaves the process holding a role it no longer
  # has, which its re-assertion then asks about; one that grants it makes no re-assertion at all,
  # and the empty status below is this row's failure to report.
  wait_for "$listener_wait_timeout" made_claim cc-a "$x2" "$x" || true
  wait_for 15 made_claim cc-a "$x2" "" || true
  expect "process B's entry stayed: A's deregistration of X removed nothing" "$(delete_answer cc-a "$x" deleted)" "false"
  expect "the listener said why it stayed" "$(delete_answer cc-a "$x" reason)" "the registration belongs to another client instance"
  expect "process A registered its new id" \
    "$([[ "$(registered_at cc-a "$x2")" -gt 0 ]] && echo registered || echo no)" "registered"
  expect "A's handoff claim naming X is refused" "$(claim_status cc-a "$x2" "$x")" "409"
  expect "the refusal names X as the holder" "$(claim_holder cc-a "$x2" "$x")" "$x"
  expect "A's heartbeat re-assertion is refused too" "$(claim_status cc-a "$x2" "")" "409"
  expect "process B keeps the role" "$(role_holder "$role")" "$x"
  expect_true "process B's role record for X still exists" test -f "$record"
  before_mtime="$(stat -c %Y "$record" 2>/dev/null || echo 0)"
  record_refreshed() {
    [[ "$(stat -c %Y "$record" 2>/dev/null || echo 0)" -gt "$before_mtime" ]]
  }
  expect_true "process B's next heartbeat refreshes that record" wait_for 30 record_refreshed
  evidence "${x2} was refused ${role} twice (409 naming ${x}); client instance ${instance_b} kept it and kept its record of ${x}"
  end_row
  end_scenario
}

# ---- the run ---------------------------------------------------------------------------------
row1_omp
row1_claude
row2_fork
row3_move
row4_handoff
row5_omp
row5_claude

printf '\n'
failures=0
for index in "${!row_names[@]}"; do
  printf '%s: %s — %s\n' "${row_outcomes[$index]}" "${row_names[$index]}" "${row_details[$index]}"
  [[ "${row_outcomes[$index]}" == PASS ]] || failures=$((failures + 1))
done
printf '\n'
if ((failures > 0)); then
  printf 'SMOKE ROLE TAKEOVER FAIL: %d of %d rows did not hold against the listener built from %s\n' \
    "$failures" "${#row_names[@]}" "${base_rev:-this working copy}"
  exit 1
fi
passed=1
printf 'SMOKE ROLE TAKEOVER PASS: all %d rows held against the listener built from %s\n' \
  "${#row_names[@]}" "${base_rev:-this working copy}"
