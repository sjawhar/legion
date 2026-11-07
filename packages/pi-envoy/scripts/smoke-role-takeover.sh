#!/usr/bin/env bash
# smoke-role-takeover.sh — live proof that a role claim is weighed on the process that holds it:
# a second process that resumed a live session's id can neither take, release nor outlast that
# session's role (INFRA-783), and a soft claim naming a predecessor takes the role only from a
# predecessor that is gone or is the claimant's own client instance (INFRA-404, R8 and A5 of
# dispatch://INFRA-404/spec).
#
# It builds natsd (packages/envoy/internal/testnats/cmd/natsd) and the listener
# (packages/envoy/cmd/listener) from this checkout with `go build` and runs both on loopback ports
# of its own, exactly as packages/envoy-client/test/envoy-stack.ts does, and then drives each row
# with real client processes:
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
# each client sent and what the listener answered it is read from a log rather than inferred.
#
# The listener runs with a live window of seconds (`session_live_window`), because every row now
# turns on whether a claim's holder is still answering for it: a holder renews its claim by
# registering, and a claim nobody renewed inside that window is the next claimant's to take. A
# row that waits that window out waits for the listener to say so — `role_unrenewed`, or the
# claim a refused process keeps asking for being granted — and never for a sleep.
#
# The rows, each printed with its outcome:
#
#   R8 row 1  a claim under the id that holds the role takes it once nobody has answered for
#             that claim inside the live window — an Oh My Pi plugin reload, which claims after
#             the window has passed, and a Claude Code channel server restart inside it, which
#             is refused 409 naming its own session and keeps asking until the window passes.
#   R8 row 2  an Oh My Pi fork gets its parent's role: the predecessor's live entry carries the
#             claimant's own client instance id (R6 case 2).
#   R8 row 3  an Oh My Pi move, in one process, to a session whose records name the held role gets
#             it, by the same case.
#   R8 row 4  a Claude Code `/clear` handoff gets the role: the deregistration removes the
#             process's own entry (R7), and the claim then finds no live predecessor (R6 case 1).
#   INFRA-783 a second process that resumed a live session's id is refused the role that session
#             holds (409 naming the holder), is granted it once the holder stops renewing the
#             claim and the window passes, and a release from the process that no longer holds
#             the claim frees nothing. The Oh My Pi story drives all three; the Claude Code
#             story drives the refusal, with the holder answering for its claim throughout, so
#             the second server's asking again leaves the role where it is.
#   R8 row 5  a claim naming a predecessor that is live inside another client instance is refused
#             409 naming it, in Oh My Pi (A5 scenario 1) and in the Claude Code `/clear` handoff
#             (A5 scenario 2).
#   A5        a refused process stays registered, and takes the role on its next session event
#             once the predecessor that held it is gone.
#   INFRA-783 an older client's hand-back marks a live holder's claim: a release of that role's
#             own lane topic, under the holder's own session id, stating no `client_instance_id`
#             — the shape a client built before this change sends — frees nothing, marks the
#             claim, and the lookup answers `handed_back`. The holder is a current client, which
#             reads a marked claim as a role it does not hold, so it claims the role back at its
#             next heartbeat and keeps taking the lane's messages.
#   INFRA-783 the same hand-back from an old-build client, which does not read the mark and so
#             never claims back, frees the role one live window plus up to one reap interval
#             later while that client is still registering: what the reaper weighs is the
#             claim's own renewal, which the mark stops, and not the liveness of the session id
#             it was claimed under.
#   INFRA-783 a client that states no process at all keeps its role across several live windows:
#             a registration carrying no `client_instance_id` renews the claims of its own
#             session that name none, which is the shape every claim standing at a deploy is in.
#
# `--base-rev <rev>` builds the listener from that revision instead of the working copy, leaving
# every client as it is here. Against `main@origin` eleven of the seventeen rows fail, which is
# this smoke's measure of what the change is for: the four original INFRA-783 rows, because that
# listener hands a role held under a session id to any process that claims under that id; A5's
# two scenarios and the row where the refused process takes the role once the holder exits,
# because it takes a role from any predecessor a claim names, live or not and whichever process
# holds the claim; the two row 1 rows, whose claim it grants but whose holder it names no
# process for, the Claude Code one also because it grants inside the window the claim this row
# wants refused there; and the two hand-back rows, because it has no mark to leave — it takes
# the live holder's lane topic, the interest half of a claim that still stands, out of the
# record and goes on renewing the claim for ever.
# The third new row passes there: a claim that names no process was never aged out by that
# listener either. It reads the
# revision without touching this working copy: `jj --ignore-working-copy log` resolves it and
# `git archive` extracts packages/envoy.
#
# Exit status: 0 PASS (every row held); 1 FAIL (the failing rows are listed); 2 refused (a
# prerequisite is missing or the configuration is invalid); 129, 130, 143 on SIGHUP, SIGINT,
# SIGTERM. See scripts/README.md.
set -euo pipefail

# The Claude Code channel server heartbeats on a timer of its own; the Oh My Pi extension's
# heartbeat is this script's to tick (`{"op":"heartbeat"}` in omp-client.ts), so its interval is
# set past the run and every Oh My Pi pass is driven. A Claude Code process that must keep
# answering for its claim gets the short interval, or is told to register through a tool
# (`renew_claude`); one that must stop answering gets the long one.
readonly claude_heartbeat_ms=1000
readonly claude_quiet_heartbeat_ms=3600000
readonly omp_heartbeat_ms=600000
# The deadline one Oh My Pi pass gets, set on its own rather than from the heartbeat above:
# ENVOY_PASS_DEADLINE_MS has a floor of 30 s (MIN_PASS_DEADLINE_MS in
# packages/envoy-client/src/envoy-settings.ts), and a pass that takes the heartbeat's ten minutes
# to be abandoned would hang a row instead of failing it.
readonly omp_pass_deadline_ms=30000
# The listener's own windows for this run.
#
# ENVOY_SESSION_LIVE_WINDOW is how long a registration keeps a session live and, with it, how long
# a claim stands renewed: the rows where a second process takes over wait it out, so it is seconds
# rather than the five minutes a deployment runs. It is the whole budget between a holder's last
# registration and a claimant's claim in the rows where the holder must still be answering, so it
# is set well past what a Claude Code channel server costs to start.
#
# ENVOY_ROLE_CLAIM_REAP_INTERVAL is the role claim reaper's period, short so that every row is
# swept several times. What a pass weighs is the claim's own renewal and not the liveness of the
# session id its holder registered under, so this reaper does free claims in this run: a claim
# nobody has answered for inside the live window is gone within one more reap interval, whoever
# goes on registering that session id. ENVOY_SESSION_GONE_HORIZON is left at its hour-long
# default all the same, because a claim that names no process is the one kind a pass still reads
# its holder's liveness for, and the row that holds such a claim needs that holder to read
# unresponsive rather than gone.
#
# The rows that wait a window out still prove what they claim, and the reaper is why they are
# worth driving at all. A row where a claimant must be refused is decided inside the window: the
# holder answers for its claim immediately before the claim is made, so no pass can have touched
# it. A row where the role must move reads the listener's own word for the move — the lookup's
# 404 (`role_unrenewed`), or a claim granted — and that word reads the same whether the standing
# claim lapsed where it stood or a pass had already removed it. In both the role went to the
# process that kept asking for it, and in neither did it go to a process while another was still
# answering for the claim.
readonly session_live_window=10s
readonly live_window_seconds="${session_live_window%s}"
readonly role_claim_reap_interval=2s
readonly curl_max_time=20
readonly client_reply_timeout=180
readonly listener_wait_timeout=60
# How long a refused Oh My Pi process is given to be granted the role once its holder stops
# renewing the claim: the live window above, the heartbeat passes that ask again inside it, and
# room for a loaded machine.
readonly takeover_timeout=90

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

# The listener's own HTTP surface: the raw calls every row drives a pre-INFRA-783 client with,
# and the readers of the role lookup and the registry each row states its outcome in. It reads
# `envoy_url`, `work` and `curl_max_time`, all set below.
# shellcheck source=packages/pi-envoy/scripts/listener-api.sh
source "${script_dir}/listener-api.sh"

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

# The two windows this run's listener carries are appended to the assignments local-stack.sh
# hands out, so the two smokes that share that file are run exactly as they were.
set_listener_env "$listener_port" "$nats_url" "smoke-role-takeover-$$"
listener_env+=(
  ENVOY_SESSION_LIVE_WINDOW="$session_live_window"
  ENVOY_ROLE_CLAIM_REAP_INTERVAL="$role_claim_reap_interval"
)
(exec env -i PATH="$PATH" HOME="${work}/listener-home" "${listener_env[@]}" \
  "${work}/bin/listener") >"${work}/logs/listener.log" 2>&1 &
service_pids+=($!)
wait_for 90 listener_healthy "$envoy_url" ||
  refuse "the listener did not become healthy: $(tail -n 5 "${work}/logs/listener.log")"
note "the listener is healthy at ${envoy_url}"

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
      ENVOY_PASS_DEADLINE_MS="$omp_pass_deadline_ms" ROLE_TAKEOVER_CWD="${home}/cwd" \
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

# first_claim_status LABEL SESSION PREVIOUS: the status the listener answered that client's
# *first* such claim, empty when it made none. A row where one claim is refused and a later one
# granted reads the refusal here and the grant in `claim_status`. Slurped rather than piped
# through `head`, which would leave jq writing into a closed pipe.
first_claim_status() {
  jq -rs --arg sid "$2" --arg previous "$3" \
    'map(select(.path == "/v1/roles/set" and .request.session_id == $sid
                and (.request.previous_session_id // "") == $previous))
     | if length == 0 then "" else (.[0].status | tostring) end' \
    "${proxy_log[$1]}" 2>/dev/null
}

# first_claim_holder LABEL SESSION PREVIOUS: the holder the listener named in refusing that
# first claim.
first_claim_holder() {
  jq -rs --arg sid "$2" --arg previous "$3" \
    'map(select(.path == "/v1/roles/set" and .request.session_id == $sid
                and (.request.previous_session_id // "") == $previous))
     | .[0].response.holder // ""' \
    "${proxy_log[$1]}" 2>/dev/null
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

# claim_count LABEL SESSION PREVIOUS: how many such claims that client has made, so a row that
# must see one more claim reads this before the pass it drives and waits for it to rise.
claim_count() {
  jq -rs --arg sid "$2" --arg previous "$3" \
    'map(select(.path == "/v1/roles/set" and .request.session_id == $sid
                and (.request.previous_session_id // "") == $previous)) | length' \
    "${proxy_log[$1]}" 2>/dev/null
}

# release_status LABEL SESSION TOPIC: the status the listener answered that client's last
# hand-back of TOPIC for SESSION (POST /v1/interests/unsubscribe), empty when it made none.
release_status() {
  jq -r --arg sid "$2" --arg topic "$3" \
    'select(.path == "/v1/interests/unsubscribe" and .request.session_id == $sid
            and ((.request.topics // []) | index($topic)) != null) | .status' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

# release_instance LABEL SESSION TOPIC: the process that hand-back named as the one asking.
release_instance() {
  jq -r --arg sid "$2" --arg topic "$3" \
    'select(.path == "/v1/interests/unsubscribe" and .request.session_id == $sid
            and ((.request.topics // []) | index($topic)) != null)
     | .request.client_instance_id // ""' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

released() {
  [[ -n "$(release_status "$1" "$2" "$3")" ]]
}

# ---- making a holder answer for its claim --------------------------------------------------------
# A claim is renewed by its holder's registrations, so a row where the holder must still hold the
# role makes it register, and one where it must lose the role simply stops.

# renew_omp LABEL SESSION: one heartbeat pass in that Oh My Pi process, waited out at its proxy.
# The pass re-registers SESSION, which renews every claim naming that session and that process.
renew_omp() {
  local label="$1" session="$2" before
  before="$(registered_at "$label" "$session")"
  registered_again() {
    [[ "$(registered_at "$label" "$session")" -gt "$before" ]]
  }
  send "$label" '{"op":"heartbeat"}'
  wait_for "$listener_wait_timeout" registered_again ||
    refuse "the ${label} client did not re-register ${session} within ${listener_wait_timeout}s"
}

# renew_claude LABEL: one registration from that channel server, answered once it has landed. The
# subscribe tool follows a topic and registers the session on the way (`follow` in
# src/envoy-channel-server.ts), which is this script's way of making a server on the long
# heartbeat answer for its claim at a moment of the row's choosing.
renew_claude() {
  send "$1" "$(jq -nc --arg t "notifications.team.smoke-takeover-${run_id}" \
    '{op:"tool",name:"envoy_subscribe",args:{topics:[$t]}}')"
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
# whose transcript records the claim. Its claim is under the id that holds the role, and the
# process that made the standing claim is gone: what grants it is that nobody renewed that claim
# inside the live window, which is the only thing that now hands a role to a second process
# claiming under the holder's own session id.
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

  # The reloaded instance starts while the window runs out: nothing renews the gone instance's
  # claim, and only a claim the listener reads as unrenewed is the next instance's to take.
  start_omp omp-reload-2
  expect_true "nobody renewed the gone instance's claim inside the live window" \
    wait_for "$listener_wait_timeout" role_unrenewed "$role"
  send omp-reload-2 "$(jq -nc --arg s "$session" --arg r "$role" '{op:"start",session:$s,roles:[$r]}')"
  second="$(instance_id omp-reload-2 "$session")"
  expect "the reloaded instance is another client instance" \
    "$([[ -n "$second" && "$second" != "$first" ]] && echo different || echo same)" "different"
  expect "its claim under the holder's own id is granted" "$(claim_status omp-reload-2 "$session" "")" "200"
  expect "the session keeps the role" "$(role_holder "$role")" "$session"
  expect "the claim names the reloaded instance now" "$(role_instance "$role")" "$second"
  evidence "${session} kept ${role} across a reload from client instance ${first} to ${second}, granted because nobody renewed ${first}'s claim inside the ${session_live_window} live window"
  end_row
  end_scenario
}

# R8 row 1, Claude Code: the channel server is killed and another starts on the same session id,
# reading the role record this one wrote. Its claim names that same id as its predecessor, which
# is the id that holds the role.
#
# The restart happens inside the live window, where a `claude --resume` and a channel-server
# restart land: the gone server renewed its claim at its last registration, so that claim still
# stands and the first thing the restarted server hears is a 409 naming this very session. What
# the row proves is that it keeps asking — each heartbeat presses the record again — and is
# granted the role one window later, rather than being left roleless for the life of the
# process (dispatch://INFRA-783).
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
  # The claim is renewed at the last moment this server has: the window the restarted server
  # must make its first claim inside starts here, and this row is about a claim made inside it.
  renew_claude cc-restart-1
  send cc-restart-1 '{"op":"kill"}'
  expect_true "the session is still registered after the server is gone" registered "$session"

  # On the short heartbeat: the row turns on the claim being pressed again, which is what each
  # heartbeat does. Nothing renews the gone server's claim meanwhile — a registration renews
  # only a claim its own process holds — so the window runs out under the restarted server's own
  # registrations.
  start_claude cc-restart-2 "$session" "$state" "$claude_heartbeat_ms"
  # A listener that grants the claim and one that refuses it both answer it; a listener that
  # never sees one leaves the status below empty, which is the row's failure to report.
  wait_for "$listener_wait_timeout" made_claim cc-restart-2 "$session" "$session" || true
  second="$(instance_id cc-restart-2 "$session")"
  expect "the restarted server is another client instance" \
    "$([[ -n "$second" && "$second" != "$first" ]] && echo different || echo same)" "different"
  expect "its first claim, made inside the window, is refused" \
    "$(first_claim_status cc-restart-2 "$session" "$session")" "409"
  expect "that refusal names this very session" \
    "$(first_claim_holder cc-restart-2 "$session" "$session")" "$session"
  claim_granted() {
    [[ "$(claim_status cc-restart-2 "$session" "$session")" == "200" ]]
  }
  expect_true "a later heartbeat's claim is granted once the window has passed with nobody answering for the standing claim" \
    wait_for "$takeover_timeout" claim_granted
  expect "the session keeps the role" "$(role_holder "$role")" "$session"
  expect "the claim names the restarted server now" "$(role_instance "$role")" "$second"
  evidence "${session} kept ${role} across a channel server restart from client instance ${first} to ${second}: refused 409 naming ${session} inside the ${session_live_window} live window, asked again on each heartbeat, and granted once nobody had renewed ${first}'s claim"
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
  # The parent answers for its claim immediately before the fork claims, so the row turns on the
  # client instance the parent's entry carries and never on a window that ran out beneath it.
  renew_omp omp-fork "$parent"
  send omp-fork "$(jq -nc --arg s "$child" '{op:"switch",session:$s,reason:"fork"}')"
  expect "the fork claims naming its parent" "$(claim_status omp-fork "$child" "$parent")" "200"
  expect "the fork holds the role" "$(role_holder "$role")" "$child"
  expect_true "the parent's entry carried this instance's id" \
    test "$(instance_id omp-fork "$parent")" = "$(instance_id omp-fork "$child")"
  evidence "${child} took ${role} from ${parent}, live inside the window and registered by this same client instance $(instance_id omp-fork "$child")"
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
  # As in row 2: the id the role is held under answers for its claim right before the move, so
  # the predecessor that claim names is live and is this instance's.
  renew_omp omp-move "$held"
  send omp-move "$(jq -nc --arg s "$moved" --arg r "$role" '{op:"switch",session:$s,reason:"resume",roles:[$r]}')"
  expect "the move claims naming the id the role was held under" "$(claim_status omp-move "$moved" "$held")" "200"
  expect "the session moved to holds the role" "$(role_holder "$role")" "$moved"
  evidence "${moved} took ${role} from the live ${held} inside one client instance $(instance_id omp-move "$moved")"
  end_row
  end_scenario
}

# R8 row 4: a `/clear` mints a new id in one channel server process. The handoff deregisters the
# old id carrying this process's client instance id, which removes its own entry (R7), and the
# claim then finds no live predecessor (R6 case 1). The claim it is weighed against is still
# standing: this server claimed the role moments earlier and the handoff follows at once, so
# nothing has had a live window to lapse in.
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
  evidence "${after} took ${role} from ${before}, whose entry this process deregistered as client instance $(instance_id cc-handoff "$after") (the listener answered deleted=$(delete_answer cc-handoff "$before" deleted))"
  end_row
  end_scenario
}

# INFRA-783, Oh My Pi. One story of two processes on one session id, which is what a continuation
# resuming a live session is: A holds the role under X and keeps answering for its claim, B
# resumes X and is held off, A then stops answering and B takes the role, and A's release on its
# way out frees nothing of B's.
row5_omp() {
  local role x left topic instance_a instance_b
  role="$(role_for r5omp)"
  x="$(session_for r5x)"
  left="$(session_for r5left)"
  topic="notifications.role.${role}"
  begin_row "INFRA-783 — a second Oh My Pi process in the live holder's session is refused the role"
  start_omp omp-a
  send omp-a "$(jq -nc --arg s "$x" '{op:"start",session:$s,roles:[]}')"
  send omp-a "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "process A holds the role under X" "$(role_holder "$role")" "$x"
  instance_a="$(instance_id omp-a "$x")"
  start_omp omp-b
  # A answers for its claim immediately before B claims, so B is weighed against a claim inside
  # the live window however long B's process took to start.
  renew_omp omp-a "$x"
  send omp-b "$(jq -nc --arg s "$x" --arg r "$role" '{op:"start",session:$s,roles:[$r]}')"
  instance_b="$(instance_id omp-b "$x")"
  expect "process B is another client instance" \
    "$([[ -n "$instance_b" && "$instance_b" != "$instance_a" ]] && echo different || echo same)" "different"
  expect "process B registered X last" \
    "$([[ "$(registered_at omp-b "$x")" -gt "$(registered_at omp-a "$x")" ]] && echo later || echo earlier)" "later"
  expect "process B's claim under X is refused" "$(claim_status omp-b "$x" "")" "409"
  expect "the refusal names X as the holder" "$(claim_holder omp-b "$x" "")" "$x"
  expect "process A keeps the role" "$(role_holder "$role")" "$x"
  expect "the listener names A's process as the one holding it" "$(role_instance "$role")" "$instance_a"
  evidence "${x} kept ${role} with client instance ${instance_a}, which answers for the claim; ${instance_b}, which resumed ${x} and registered it last, was refused 409 naming ${x}"
  end_row

  begin_row "INFRA-783 — the second process is granted the role once the holder stops renewing its claim"
  # Nothing renews A's claim from here: the Oh My Pi heartbeat is this script's to tick, and it
  # ticks no more for A. B's own registrations keep X live, so the lookup goes on naming X under
  # another process, which is the one refusal a heartbeat pass asks about again.
  # granted_after_asking: B's claim where it has been granted, and one more heartbeat pass where
  # it has not. It is a predicate `poll_until` drives, so B asks again on every pass of the
  # takeover budget until the claim lands.
  granted_after_asking() {
    [[ "$(claim_status omp-b "$x" "")" == 200 ]] && return 0
    send omp-b '{"op":"heartbeat"}'
    return 1
  }
  poll_until "$takeover_timeout" 1 granted_after_asking || true
  expect "process B's claim under X is granted" "$(claim_status omp-b "$x" "")" "200"
  expect "the role is still held under X" "$(role_holder "$role")" "$x"
  expect "the listener names B's process as the one holding it now" "$(role_instance "$role")" "$instance_b"
  evidence "${instance_b} took ${role} under ${x} once ${instance_a} had left its claim unrenewed past the ${session_live_window} live window, asking again on each heartbeat pass while ${x} stayed live"
  end_row

  begin_row "INFRA-783 — a release from the process that no longer holds the claim frees nothing"
  # A's branch records a release, so A ends its hold and hands the role topic back naming its own
  # process. A is the second process in X now — the standing claim is B's — and a release frees
  # the claims of the process that asks and no others, so B keeps the role. This is the incident
  # itself: the process whose claim is gone leaves, and the live holder is untouched by it.
  send omp-a "$(jq -nc --arg s "$left" '{op:"switch",session:$s,reason:"new",roles:[null]}')"
  expect_true "process A handed the role topic back" \
    wait_for "$listener_wait_timeout" released omp-a "$x" "$topic"
  expect "the listener took the release" "$(release_status omp-a "$x" "$topic")" "200"
  expect "the release named A's process" "$(release_instance omp-a "$x" "$topic")" "$instance_a"
  expect "A's deregistration of X removed nothing either" "$(delete_answer omp-a "$x" deleted)" "false"
  expect "the role is still held under X" "$(role_holder "$role")" "$x"
  expect "process B still holds it" "$(role_instance "$role")" "$instance_b"
  evidence "${instance_a} handed ${topic} back for ${x} and left the id; ${instance_b}'s claim of ${role} stood through it"
  end_row
  end_scenario
}

# R8 row 5 (A5 scenario 1), Oh My Pi: a soft claim naming a predecessor that is live inside
# another client instance is refused.
#
# The live predecessor is built from two session ids rather than from one. Process D claims the
# role under Q and moves it to P, so the role is held under P by D's own process; process C then
# resumes P and forks away from it, and its fork's claim names P. Built the other way — one id,
# resumed by a second process that then takes the role under it — it proves this rule no longer:
# that second process is refused under that id now, and the claim it makes is decided on the
# standing claim rather than on the predecessor it names.
row6_predecessor() {
  local role q p z z2 instance_c instance_d
  role="$(role_for r6)"
  q="$(session_for r6q)"
  p="$(session_for r6p)"
  z="$(session_for r6z)"
  z2="$(session_for r6z2)"
  begin_row "R8 row 5 (A5 scenario 1) — an Oh My Pi claim naming a predecessor live inside another client instance is refused"
  start_omp omp-d
  send omp-d "$(jq -nc --arg s "$q" '{op:"start",session:$s,roles:[]}')"
  send omp-d "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  send omp-d "$(jq -nc --arg s "$p" --arg r "$role" '{op:"switch",session:$s,reason:"resume",roles:[$r]}')"
  expect "process D moved the role from Q to P" "$(role_holder "$role")" "$p"
  instance_d="$(instance_id omp-d "$p")"

  start_omp omp-c
  send omp-c "$(jq -nc --arg s "$p" '{op:"start",session:$s,roles:[]}')"
  instance_c="$(instance_id omp-c "$p")"
  expect "process C is another client instance" \
    "$([[ -n "$instance_c" && "$instance_c" != "$instance_d" ]] && echo different || echo same)" "different"
  # C registered P last, which is what a predecessor read would find; D answers again here, so
  # the predecessor C is about to name is live and is D's.
  renew_omp omp-d "$p"
  send omp-c "$(jq -nc --arg s "$z" --arg r "$role" '{op:"switch",session:$s,reason:"fork",roles:[$r]}')"
  expect "process C's fork claim naming P is refused" "$(claim_status omp-c "$z" "$p")" "409"
  expect "the refusal names P as the holder" "$(claim_holder omp-c "$z" "$p")" "$p"
  expect "process D keeps the role" "$(role_holder "$role")" "$p"
  expect "the listener names D's process as the one holding it" "$(role_instance "$role")" "$instance_d"
  evidence "${z} was refused ${role} (409 naming ${p}), whose entry is live inside client instance ${instance_d}; ${instance_c}, which had resumed ${p}, did not take it"
  end_row

  begin_row "A5 — a refused Oh My Pi process stays registered"
  expect_true "the refused session is registered" registered "$z"
  # A session a process leaves is gone at once where that entry was its own
  # (dispatch://INFRA-392), and stays where it was not: P belongs to D, so C's deregistration of
  # it removed nothing, which is what left a live predecessor for the claim above to be weighed
  # against.
  expect "P's registration stayed with D when C left it" "$(delete_answer omp-c "$p" deleted)" "false"
  evidence "${z} is in the listener's registry with no role, and ${p}, which this process left, stayed registered to ${instance_d}"
  end_row

  begin_row "R8 row 5 — an Oh My Pi move in the refused process is refused too"
  send omp-c "$(jq -nc --arg s "$z2" --arg r "$role" '{op:"tree",session:$s,roles:[$r]}')"
  expect "process C's move claim naming P is refused" "$(claim_status omp-c "$z2" "$p")" "409"
  expect "the refusal names P as the holder" "$(claim_holder omp-c "$z2" "$p")" "$p"
  expect "process D keeps the role" "$(role_holder "$role")" "$p"
  evidence "${z2} was refused ${role} (409 naming ${p}) in the same process that was refused for ${z}"
  end_row

  begin_row "A5 — after the holder exits, the refused Oh My Pi process takes the role on its next session event"
  send omp-d '{"op":"shutdown"}'
  expect_true "process D's session left the registry when it shut down" \
    wait_for "$listener_wait_timeout" unregistered "$p"
  send omp-c "$(jq -nc --arg s "$z2" --arg r "$role" '{op:"tree",session:$s,roles:[$r]}')"
  expect "the claim the refused process makes now is granted" "$(claim_status omp-c "$z2" "")" "200"
  expect "the refused process now holds the role" "$(role_holder "$role")" "$z2"
  expect "the listener names C's process as the one holding it" "$(role_instance "$role")" "$instance_c"
  evidence "${z2} took ${role} on its next session event once ${p} had left the registry"
  end_row
  end_scenario
}

# INFRA-783 and R8 row 5 (A5 scenario 2), Claude Code. A holds the role under X and answers for
# its claim; B, a second `claude` of the same user, resumes X, reads the role record A wrote and
# is refused the role under that id; A then runs a `/clear` handoff whose claim names X, which B
# registers now, and that claim is refused by the predecessor rule.
row5_claude() {
  local role x x2 state record instance_a instance_b
  role="$(role_for r5cc)"
  x="$(session_for r5ccx)"
  x2="$(session_for r5ccx2)"
  state="${work}/r5cc-plugin-data"
  record="${state}/roles/${x}.json"
  begin_row "INFRA-783 — a second Claude Code channel server in the live holder's session is refused the role"
  start_claude cc-a "$x" "$state" "$claude_quiet_heartbeat_ms"
  expect_true "process A's channel server registered" wait_for "$listener_wait_timeout" registered "$x"
  send cc-a "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "process A holds the role under X" "$(role_holder "$role")" "$x"
  instance_a="$(instance_id cc-a "$x")"
  # A answers for its claim here rather than on a timer of its own: this row needs A's claim to
  # be inside the live window when B claims, and the row below needs B to have registered X
  # last, which two servers heartbeating against each other would settle by coin toss.
  renew_claude cc-a
  # B shares A's plugin data directory, as two `claude` processes of one user do, so it reads the
  # role record A wrote for X and claims it back under that same id as it boots.
  start_claude cc-b "$x" "$state" "$claude_heartbeat_ms"
  wait_for "$listener_wait_timeout" made_claim cc-b "$x" "$x" || true
  # A answers for its claim again, now that B has made its first: B asks again on each heartbeat
  # from here, and this row is what happens while the holder is still answering — the role stays
  # with A's process, and B's asking changes nothing.
  renew_claude cc-a
  instance_b="$(instance_id cc-b "$x")"
  expect "process B is another client instance" \
    "$([[ -n "$instance_b" && "$instance_b" != "$instance_a" ]] && echo different || echo same)" "different"
  # B's first claim, read on its own: every later one it makes while A answers is refused the
  # same way, and the row is about the first.
  expect "process B's claim under X is refused" "$(first_claim_status cc-b "$x" "$x")" "409"
  expect "the refusal names X as the holder" "$(first_claim_holder cc-b "$x" "$x")" "$x"
  expect "process A keeps the role" "$(role_holder "$role")" "$x"
  expect "the listener names A's process as the one holding it" "$(role_instance "$role")" "$instance_a"
  expect_true "the refused server kept the role record of X, which is the live holder's" test -f "$record"
  evidence "${x} kept ${role} with client instance ${instance_a}; ${instance_b}, which resumed ${x}, was refused 409 naming ${x} and left the record of ${x} where its writer can still use it"
  end_row

  begin_row "R8 row 5 (A5 scenario 2) — a Claude Code /clear handoff naming a predecessor live inside another client instance is refused"
  registered_last() {
    [[ "$(registered_at cc-b "$x")" -gt "$(registered_at cc-a "$x")" ]]
  }
  expect_true "process B registered X last" wait_for "$listener_wait_timeout" registered_last
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
  expect "A's re-assertion under its new id is refused too" "$(claim_status cc-a "$x2" "")" "409"
  expect "the role stays under X" "$(role_holder "$role")" "$x"
  expect_true "the role record of X survived both refusals" test -f "$record"
  evidence "${x2} was refused ${role} twice (409 naming ${x}), whose entry is live inside client instance ${instance_b}; the claim of ${role} stayed under ${x}"
  end_row
  end_scenario
}

# INFRA-783, Oh My Pi: an older client's hand-back of a role a live process holds. A release
# stating no `client_instance_id` is the shape a client built before this change sends, and it
# can free nothing here: the claim names a process, and a release that cannot say which process
# it is must not take the live holder's role. What it leaves instead is the mark — the lookup
# answers `handed_back`, no registration renews a marked claim, and the role claim reaper would
# free the role at its first pass past the live window, a live window and up to one reap
# interval later.
#
# This is where the design is weakest, so the row drives it on the live surface: the holder is a
# real Oh My Pi process, and what saves it is that a current client reads a marked claim as a
# role it does not hold (`roleHeldBy`) and claims again at its next heartbeat. The claim grant
# writes a fresh record, so the mark goes with the claim it was written on, and the holder has
# its role and its lane back.
row7_handback_mark() {
  local role session topic instance claims
  role="$(role_for r7)"
  session="$(session_for r7)"
  topic="notifications.role.${role}"
  begin_row "INFRA-783 — an older client's hand-back marks a live holder's claim, which the holder claims back at its next heartbeat"
  start_omp omp-handback
  send omp-handback "$(jq -nc --arg s "$session" '{op:"start",session:$s,roles:[]}')"
  send omp-handback "$(jq -nc --arg r "$role" '{op:"tool",name:"envoy_role_set",args:{role:$r}}')"
  expect "the holder holds the role" "$(role_holder "$role")" "$session"
  instance="$(instance_id omp-handback "$session")"
  # The holder answers for its claim immediately before the hand-back arrives, so what the row
  # reads next is the mark and never a window that ran out beneath it.
  renew_omp omp-handback "$session"
  claims="$(claim_count omp-handback "$session" "")"
  expect "the old client's hand-back of the role's lane topic is taken" \
    "$(old_handback "$session" "$topic")" "200"
  expect "it removed no topic: the lane topic is the live holder's" \
    "$(jq -c '.removed' "${work}/answer.json")" "[]"
  expect "the claim stands" "$(role_holder "$role")" "$session"
  expect "it is still the holder's own process's" "$(role_instance "$role")" "$instance"
  expect "the lookup reports the hand-back" "$(role_handed_back "$role")" "true"

  claimed_again() {
    [[ "$(claim_count omp-handback "$session" "")" -gt "$claims" ]]
  }
  unmarked() {
    [[ -z "$(role_handed_back "$role")" ]]
  }
  renew_omp omp-handback "$session"
  expect_true "the holder claimed the role again on that heartbeat" \
    wait_for "$listener_wait_timeout" claimed_again
  expect "that claim is granted" "$(claim_status omp-handback "$session" "")" "200"
  expect_true "the lookup no longer reports the mark" wait_for "$listener_wait_timeout" unmarked
  expect "the holder's own process holds the role" "$(role_instance "$role")" "$instance"
  expect "a message on the role lane reaches that holder" \
    "$(listener_post /v1/messages/publish "$(jq -nc --arg t "$topic" \
      '{topic:$t,message:"a message for the role'"'"'s holder"}')")" "200"
  expect "the listener forwarded it to this holder" "$(jq -r '.holder // ""' "${work}/answer.json")" "$session"
  evidence "an old client's hand-back of ${topic} marked ${session}'s claim of ${role} handed back without freeing it, and client instance ${instance} claimed the role again on its next heartbeat, cleared the mark and took the lane's next message"
  end_row
  end_scenario
}

# INFRA-783, an old-build client throughout: the same hand-back, from a client that does not
# read the mark and so never claims back. It is driven by curl alone, because every client in
# this checkout would take the role straight back, which is the row above.
#
# The claim is made stating no process, as a client built before this change makes it, and the
# first registration that states one adopts it — the deploy's own sequence. From there the claim
# names a process, so the hand-back can free nothing and marks it, and the role comes free a
# live window and up to one reap interval later though this client goes on registering the
# whole time: what the reaper weighs is the claim's own renewal, which the mark stops, and not
# the session id's liveness.
row8_handback_frees() {
  local role session instance topic
  role="$(role_for r8)"
  session="$(session_for r8)"
  instance="oldclient${run_id}"
  topic="notifications.role.${role}"
  begin_row "INFRA-783 — an older client's hand-back frees the role a live window and up to one reap interval later while that client still runs"
  expect "the old client registers" "$(old_register "$session" "$instance")" "200"
  expect "its claim, which states no process, is granted" "$(old_claim "$session" "$role" "")" "200"
  expect "the claim names no process yet" "$(role_instance "$role")" ""
  expect "its next registration is taken" "$(old_register "$session" "$instance")" "200"
  adopted() {
    [[ "$(role_instance "$role")" == "$instance" ]]
  }
  expect_true "that registration adopted the claim for its own process" \
    wait_for "$listener_wait_timeout" adopted
  expect "the hand-back of the role's lane topic is taken" "$(old_handback "$session" "$topic")" "200"
  expect "it removed no topic: the claim names a process this release cannot be" \
    "$(jq -c '.removed' "${work}/answer.json")" "[]"
  expect "the claim stands, marked handed back" "$(role_handed_back "$role")" "true"
  expect "the role is still held meanwhile" "$(role_holder "$role")" "$session"

  # free_after_registering: the role where the lookup already reports no claim on it, and one
  # more registration from this client where it does not. It is a predicate `poll_until` drives,
  # so the client keeps registering for as long as the wait runs: its session is live throughout
  # and only the claim's own renewal — which no registration moves now — decides.
  free_after_registering() {
    [[ "$(role_reason "$role")" == "unclaimed" ]] && return 0
    old_register "$session" "$instance" >/dev/null
    return 1
  }
  expect_true "the role came free a live window and up to one reap interval after the hand-back" \
    poll_until "$takeover_timeout" 1 free_after_registering
  expect "the lookup answers a role no record claims" "$(role_reason "$role")" "unclaimed"
  expect "it names no holder" "$(role_holder "$role")" ""
  expect_true "the old client is registering still" registered "$session"
  evidence "${session}, registering as client instance ${instance} throughout, lost ${role} within the ${session_live_window} live window of the hand-back of ${topic} that marked its claim and the ${role_claim_reap_interval} reap interval after it; the lookup answers unclaimed with that session still in the registry"
  end_row
}

# INFRA-783, an old-build client throughout: a client that states no process at all, which is
# the shape every claim standing at a deploy is in. Its claim names no process, and a
# registration that states none renews exactly the claims of its own session that name none, so
# the role stands through live window after live window of such registrations. Nothing adopts
# the claim meanwhile, because an absent instance names no process to adopt it with.
row9_no_instance() {
  local role session held index
  role="$(role_for r9)"
  session="$(session_for r9)"
  begin_row "INFRA-783 — a client that states no process keeps its role across several live windows"
  expect "the old client registers with no client_instance_id" "$(old_register "$session" "")" "200"
  expect "its claim, which states no process either, is granted" "$(old_claim "$session" "$role" "")" "200"
  expect "the claim names no process" "$(role_instance "$role")" ""
  held=held
  for ((index = 0; index < 3 * live_window_seconds; index++)); do
    old_register "$session" "" >/dev/null
    sleep 1
    [[ "$(role_holder "$role")" == "$session" ]] || held=lost
  done
  expect "the role was held at every second of three live windows" "$held" "held"
  expect "the role is still this session's" "$(role_holder "$role")" "$session"
  expect "its claim still names no process" "$(role_instance "$role")" ""
  expect "no hand-back marked it" "$(role_handed_back "$role")" ""
  evidence "${session} held ${role} through $((3 * live_window_seconds))s, three ${session_live_window} live windows, of registrations that state no client_instance_id at all, its claim naming no process throughout"
  end_row
}

# ---- the run ---------------------------------------------------------------------------------
started_at=$SECONDS
row1_omp
row1_claude
row2_fork
row3_move
row4_handoff
row5_omp
row6_predecessor
row5_claude
row7_handback_mark
row8_handback_frees
row9_no_instance
note "every row is driven; the rows took $((SECONDS - started_at))s"

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
