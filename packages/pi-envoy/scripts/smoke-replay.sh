#!/usr/bin/env bash
# smoke-replay.sh — live proof that an Oh My Pi session shows once each Envoy message it missed
# while its NATS subscription was down (INFRA-439).
#
# One real `omp --mode rpc` session runs this checkout's pi-envoy (extensions/envoy.ts) against a
# real Envoy listener and a real JetStream NATS, and reaches NATS only through a `socat` proxy this
# script owns, so killing the proxy breaks exactly that session's connection.
#
#   leg 1    the session registers; the proxy is killed and natsd's monitoring endpoint
#            (/connz?subs=1) shows the session's connection and its notifications.agent.<id>
#            subscription gone; a Dispatch-shaped envelope is published through JetStream to that
#            subject with the outbox's Nats-Msg-Id, and the stream is read back to show it stored
#            it; the proxy comes back, and within two heartbeats the session file holds exactly one
#            `envoy-message` entry carrying the event id, still one after three more heartbeats.
#   leg 2    omp stops, another envelope is published, omp resumes the same session file: that
#            event shows once within two heartbeats of the session's return and is still once
#            three heartbeats later, and leg 1's is still once. (Remote mode SIGKILLs omp instead
#            of ending its input, since /v1/messages/send needs the registration a clean exit drops.)
#   control  leg 1 again with a session running the released plugin (SMOKE_REPLAY_CONTROL_PLUGIN):
#            it ends with no entry for its publish, while a frame published live afterwards shows
#            once, so the session was connected and writing cards all along.
#
# Local mode (the default) builds natsd (packages/envoy/internal/testnats/cmd/natsd) and the
# listener (packages/envoy/cmd/listener) from this checkout and starts them on loopback ports of its
# own. Remote mode (SMOKE_REPLAY_ENVOY_URL and SMOKE_REPLAY_NATS_URL, both) builds nothing and runs
# against an existing listener and NATS: it publishes through the listener's /v1/messages/send,
# confirms the stream stored each publish through the listener's /v1/replay, and breaks the
# session's connection by killing its local proxy, since it cannot stop a remote NATS or read its
# monitoring endpoint.
#
# Nothing it starts inherits this shell's environment: every child gets PATH, the provider
# variables SMOKE_REPLAY_PROVIDER_ENV names (omp only), and what the script sets — a scratch HOME,
# TMPDIR and envoy.json, OMP_SESSION_STORAGE=file, ENVOY_HEARTBEAT_MS — so an operator's ENVOY_*,
# NATS_*, DISPATCH_*, LEGION_*, OMP_* or PI_* variables never reach it. It stops only the processes
# it started: each by its pid, and the proxy by the process group it alone runs in. Manual only,
# like the other smokes here: it needs a model provider and is never wired into CI.
#
# Exit status: 0 PASS; 1 FAIL (the `SMOKE REPLAY FAIL (<step>)` line says which check); 2 refused (a
# prerequisite is missing, the configuration is invalid, or the model provider did not answer the
# first prompt); 129, 130, 143 on SIGHUP, SIGINT, SIGTERM. See scripts/README.md.
set -euo pipefail

readonly heartbeat_ms=5000
readonly heartbeat_s=$((heartbeat_ms / 1000))
readonly curl_connect_timeout=5
readonly curl_max_time=20

refuse() {
  printf 'smoke-replay: %s\n' "$1" >&2
  exit 2
}

note() {
  printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*"
}

step=setup
begin() {
  step=$1
  printf '== %s\n' "$step"
}

fail() {
  printf 'SMOKE REPLAY FAIL (%s): %s\n' "$step" "$1"
  exit 1
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "${script_dir}/../../.." && pwd)"
readonly repo_root
readonly plugin="${repo_root}/packages/pi-envoy/extensions/envoy.ts"

# ---- configuration and prerequisites ---------------------------------------------------------
remote_envoy_url="${SMOKE_REPLAY_ENVOY_URL:-}"
remote_nats_url="${SMOKE_REPLAY_NATS_URL:-}"
token_file="${SMOKE_REPLAY_ENVOY_TOKEN_FILE:-}"
control_plugin="${SMOKE_REPLAY_CONTROL_PLUGIN:-/opt/legion/pi-envoy/dist/envoy.js}"
model="${SMOKE_REPLAY_MODEL:-anthropic/claude-haiku-4-5}"
provider_env="${SMOKE_REPLAY_PROVIDER_ENV:-ANTHROPIC_API_KEY}"
omp_bin="${SMOKE_REPLAY_OMP:-omp}"
readonly remote_envoy_url remote_nats_url token_file control_plugin model provider_env

mode=local
if [[ -n "$remote_envoy_url" || -n "$remote_nats_url" ]]; then
  [[ -n "$remote_envoy_url" && -n "$remote_nats_url" ]] ||
    refuse "remote mode needs both SMOKE_REPLAY_ENVOY_URL and SMOKE_REPLAY_NATS_URL"
  mode=remote
fi
readonly mode

required=(bun omp socat setsid curl jq pgrep)
[[ "$mode" == remote ]] || required+=(go)
for tool in "${required[@]}"; do
  if [[ "$tool" == omp ]]; then
    command -v "$omp_bin" >/dev/null 2>&1 ||
      refuse "omp is required: SMOKE_REPLAY_OMP (${omp_bin}) is not an executable on PATH"
    omp_bin="$(command -v "$omp_bin")"
  else
    command -v "$tool" >/dev/null 2>&1 || refuse "${tool} is required"
  fi
done
readonly omp_bin

# The model provider: the variables the omp child gets, each of which must be set. The script
# passes nothing else of this shell to omp, so a provider omp would otherwise find in the
# operator's own configuration does not count.
[[ -n "${provider_env// /}" ]] ||
  refuse "SMOKE_REPLAY_PROVIDER_ENV is empty: name the variable(s) holding the model provider's credentials"
for name in $provider_env; do
  [[ "$name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] ||
    refuse "SMOKE_REPLAY_PROVIDER_ENV names ${name}, which is not a variable name"
  [[ -n "${!name:-}" ]] ||
    refuse "no model provider for omp: ${name} is not set (SMOKE_REPLAY_PROVIDER_ENV=${provider_env}, SMOKE_REPLAY_MODEL=${model})"
done

[[ -r "$plugin" ]] || refuse "the plugin under test (${plugin}) is not readable"
[[ -f "${repo_root}/node_modules/nats/package.json" ]] ||
  refuse "${repo_root}/node_modules has no nats package: run bun install in the checkout"
[[ -r "$control_plugin" ]] ||
  refuse "the control plugin (SMOKE_REPLAY_CONTROL_PLUGIN=${control_plugin}) is not readable"

upstream=""
if [[ "$mode" == remote ]]; then
  [[ "$remote_envoy_url" =~ ^https?://[^/]+/?$ ]] ||
    refuse "SMOKE_REPLAY_ENVOY_URL (${remote_envoy_url}) must be an http(s) base URL"
  [[ "$remote_nats_url" =~ ^nats://([^/:@]+):([0-9]+)/?$ ]] ||
    refuse "SMOKE_REPLAY_NATS_URL (${remote_nats_url}) must be nats://host:port, the address the proxy fronts"
  upstream="${BASH_REMATCH[1]}:${BASH_REMATCH[2]}"
  if [[ -n "$token_file" ]]; then
    [[ "$token_file" == /* && -r "$token_file" ]] ||
      refuse "SMOKE_REPLAY_ENVOY_TOKEN_FILE (${token_file}) is not a readable absolute path"
  fi
elif [[ -n "$token_file" ]]; then
  refuse "SMOKE_REPLAY_ENVOY_TOKEN_FILE applies to remote mode only; the local listener takes no bearer"
fi

read -r ephemeral_low _ </proc/sys/net/ipv4/ip_local_port_range
readonly port_floor=10000
((ephemeral_low > port_floor + 1000)) ||
  refuse "the kernel's ephemeral range starts at ${ephemeral_low}, leaving no ports below it above ${port_floor}"

# ---- scratch, processes and teardown ---------------------------------------------------------
work="$(mktemp -d "${TMPDIR:-/tmp}/smoke-replay.XXXXXX")"
readonly work
mkdir -p "${work}/logs" "${work}/bin"
auth_header=""

nats_pid=""
listener_pid=""
proxy_pid=""
omp_pid=""
omp_fd=""
passed=0

# stop_pid PID: TERM, then KILL after 10 s. Best effort, for the teardown and for services.
stop_pid() {
  local pid=${1:-} i
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 0
  kill -TERM "$pid" 2>/dev/null || return 0
  for ((i = 0; i < 50; i++)); do
    running "$pid" || break
    sleep 0.2
  done
  kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# kill_proxy: the proxy and the per-connection children it forked, which outlive a parent killed
# alone and would keep the session's connection open. The proxy runs in a process group of its own
# (start_proxy), which holds exactly it and its forks, so one SIGKILL to that group ends them all
# at once.
proxy_children=""
kill_proxy() {
  [[ -n "$proxy_pid" ]] || return 0
  proxy_children="$(pgrep -g "$proxy_pid" || true)"
  kill -KILL -- "-${proxy_pid}" 2>/dev/null || true
  wait "$proxy_pid" 2>/dev/null || true
  proxy_pid=""
}

close_omp_stdin() {
  [[ -z "$omp_fd" ]] || exec {omp_fd}>&-
  omp_fd=""
}

cleanup() {
  local status=$?
  trap - EXIT
  set +e
  close_omp_stdin
  stop_pid "$omp_pid"
  kill_proxy
  stop_pid "$listener_pid"
  stop_pid "$nats_pid"
  [[ -z "$auth_header" ]] || rm -f "$auth_header"
  if ((passed == 1)); then
    rm -rf "$work"
  else
    printf 'smoke-replay: kept %s (logs/, each session'"'"'s rpc and stderr logs and session file)\n' "$work"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# isolated_exec KEEP NAME=VALUE... -- COMMAND...: replaces this (sub)shell with COMMAND, run with no
# variable of this shell but PATH, the names KEEP lists (space-separated) and the assignments. The
# values are exported, never put on a command line, which /proc shows every local user. Run it in
# a subshell: `(isolated_exec ...) &`. The child never holds the write end of omp's input: a proxy
# or omp itself holding it would keep omp from ever seeing the end of its input.
isolated_exec() {
  local keep=" PATH $1 " name
  shift
  [[ -z "$omp_fd" ]] || exec {omp_fd}>&-
  for name in $(compgen -e); do
    [[ "$keep" == *" ${name} "* ]] || export -n "${name?}"
  done
  while [[ "$1" != "--" ]]; do
    export "${1?}"
    shift
  done
  shift
  exec "$@"
}

# wait_until SECONDS COMMAND...: polls COMMAND every half second; 1 once SECONDS have passed.
wait_until() {
  local deadline=$((SECONDS + $1))
  shift
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  "$@" >/dev/null 2>&1
}

# running PID: PID is a live process, not a zombie its parent has not reaped.
running() {
  local state
  [[ -r "/proc/$1/stat" ]] || return 1
  read -r _ _ state _ 2>/dev/null <"/proc/$1/stat" || return 1
  [[ "$state" != Z ]]
}

ended() {
  ! running "$1"
}

# await PID WHAT LOG SECONDS COMMAND...: wait_until, failing at once, with LOG's tail, when PID
# (WHAT) exits first, and naming WHAT when SECONDS pass.
await() {
  local pid=$1 what=$2 log=$3 limit=$4 deadline=$((SECONDS + $4))
  shift 4
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    running "$pid" || fail "${what} (pid ${pid}) exited: $(tail -n 5 "$log" | tr '\n' ' ')"
    sleep 0.5
  done
  "$@" >/dev/null 2>&1 || fail "timed out after ${limit}s waiting for ${what}: $(tail -n 5 "$log" | tr '\n' ' ')"
}

port_open() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# pick_port VAR: a loopback port nothing answers on, below the kernel's ephemeral range (which every
# outbound connection autobinds from) and distinct from this run's earlier picks.
picked_ports=" "
pick_port() {
  local candidate i
  for ((i = 0; i < 100; i++)); do
    candidate=$((port_floor + RANDOM % (ephemeral_low - port_floor)))
    [[ "$picked_ports" != *" ${candidate} "* ]] || continue
    port_open "$candidate" && continue
    picked_ports+="${candidate} "
    printf -v "$1" '%s' "$candidate"
    return 0
  done
  fail "no free loopback port below ${ephemeral_low}"
}

listener_curl() {
  local -a args=(-sS --connect-timeout "$curl_connect_timeout" --max-time "$curl_max_time")
  [[ -z "$auth_header" ]] || args+=(-H "@${auth_header}")
  curl "${args[@]}" "$@"
}

# ---- the services -----------------------------------------------------------------------------
begin services
note "mode ${mode}; omp $("$omp_bin" --version 2>/dev/null | head -n 1) (${omp_bin}); model ${model}; heartbeat ${heartbeat_ms} ms"
note "plugin under test: ${plugin} (@sjawhar/pi-envoy $(jq -r .version "${repo_root}/packages/pi-envoy/package.json") in this checkout)"
control_version="$(jq -r '.version // "unknown"' "$(dirname -- "$control_plugin")/../package.json" 2>/dev/null || printf 'unknown')"
note "control plugin: ${control_plugin} (version ${control_version})"
note "scratch: ${work}"

nats_monitor=""
nats_port=""
monitor_port=""
listener_port=""
proxy_port=""
if [[ "$mode" == local ]]; then
  note "building natsd and the listener from ${repo_root}/packages/envoy"
  (cd -- "${repo_root}/packages/envoy" &&
    go build -o "${work}/bin/natsd" ./internal/testnats/cmd/natsd &&
    go build -o "${work}/bin/listener" ./cmd/listener) ||
    fail "go build of natsd or the listener failed (the reason is above)"

  pick_port nats_port
  pick_port monitor_port
  pick_port listener_port
  (isolated_exec "" HOME="${work}/nats-home" -- "${work}/bin/natsd" \
    -p "$nats_port" -m "$monitor_port" -js -store_dir "${work}/nats-store") \
    >"${work}/logs/natsd.log" 2>&1 &
  nats_pid=$!
  nats_monitor="http://127.0.0.1:${monitor_port}"
  nats_ready() {
    grep -q 'Server is ready' "${work}/logs/natsd.log" &&
      curl -fsS --max-time 2 "${nats_monitor}/healthz" | jq -e '.status == "ok"'
  }
  await "$nats_pid" natsd "${work}/logs/natsd.log" 30 nats_ready
  note "natsd (pid ${nats_pid}) serves nats://127.0.0.1:${nats_port}, monitoring ${nats_monitor}, JetStream store ${work}/nats-store"

  (isolated_exec "" HOME="${work}/listener-home" PORT="$listener_port" ENVOY_LISTEN_HOST=127.0.0.1 \
    ENVOY_MACHINE_ID="smoke-replay-$$" NATS_URLS="nats://127.0.0.1:${nats_port}" -- "${work}/bin/listener") \
    >"${work}/logs/listener.log" 2>&1 &
  listener_pid=$!
  envoy_url="http://127.0.0.1:${listener_port}"
  listener_ready() {
    curl -fsS --max-time 2 "${envoy_url}/healthz" | jq -e '.status == "healthy"'
  }
  await "$listener_pid" "the listener" "${work}/logs/listener.log" 60 listener_ready
  stream_seen() {
    curl -fsS --max-time 2 "${nats_monitor}/jsz?streams=true" |
      jq -e '[.account_details[]?.stream_detail[]?.name] | index("ENVOY_NOTIFICATIONS")'
  }
  wait_until 30 stream_seen || fail "natsd holds no ENVOY_NOTIFICATIONS stream 30s after the listener started"
  note "listener (pid ${listener_pid}) healthy at ${envoy_url}; natsd holds stream $(curl -fsS --max-time 2 "${nats_monitor}/jsz?streams=true&config=true" |
    jq -c '[.account_details[]?.stream_detail[]? | select(.name == "ENVOY_NOTIFICATIONS") | {name, subjects: .config.subjects, max_age_s: (.config.max_age / 1e9)}] | first')"
  upstream="127.0.0.1:${nats_port}"
else
  envoy_url="${remote_envoy_url%/}"
  if [[ -n "$token_file" ]]; then
    auth_header="${work}/auth-header"
    (
      umask 077
      printf 'Authorization: Bearer %s\n' "$(tr -d '\r\n' <"$token_file")" >"$auth_header"
    )
  fi
  code="$(listener_curl -o /dev/null -w '%{http_code}' "${envoy_url}/v1/sessions" 2>/dev/null)" || code=000
  [[ "$code" == 200 ]] || fail "GET ${envoy_url}/v1/sessions answered ${code}; the listener is not usable from here"
  note "remote listener ${envoy_url} answers GET /v1/sessions; the proxy fronts ${upstream}"
fi
pick_port proxy_port
readonly envoy_url upstream proxy_port

# start_proxy: socat in a new session and process group of its own (setsid execs it in place, so
# the group's id is the pid this script holds), forking one child per connection.
start_proxy() {
  (isolated_exec "" -- setsid socat "TCP-LISTEN:${proxy_port},bind=127.0.0.1,reuseaddr,fork" "TCP:${upstream}") \
    >>"${work}/logs/proxy.log" 2>&1 &
  proxy_pid=$!
  wait_until 10 port_open "$proxy_port" || fail "the proxy did not listen on 127.0.0.1:${proxy_port} within 10s"
  local group
  read -r _ _ _ _ group _ <"/proc/${proxy_pid}/stat"
  if [[ "$group" != "$proxy_pid" ]]; then
    kill -KILL "$proxy_pid" 2>/dev/null || true
    fail "the proxy (pid ${proxy_pid}) is in process group ${group}, not one of its own"
  fi
}

# ---- one omp session --------------------------------------------------------------------------
readonly system_note="This session is an automated smoke test. Answer every message, Envoy cards included, with the single word ok, and call no tools."
session_name=""
session_home=""
rpc_log=""
launches=0
sid=""
session_file=""
subject=""

# prepare_session NAME: the session's scratch HOME, working directory, session directory and the
# envoy.json the plugin reads (${HOME}/.config/legion/envoy.json, the neutral file), naming the
# listener and the proxy as its only NATS server, with Dispatch off.
prepare_session() {
  session_name=$1
  session_home="${work}/${session_name}"
  mkdir -p "${session_home}/home/.config/legion" "${session_home}/cwd" "${session_home}/sessions" "${session_home}/tmp"
  jq -n --arg url "$envoy_url" --arg nats "nats://127.0.0.1:${proxy_port}" \
    '{envoy: {url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}}' \
    >"${session_home}/home/.config/legion/envoy.json"
}

rpc_send() {
  printf '%s\n' "$1" >&"$omp_fd"
}

rpc_frame() {
  jq -ecnR --arg type "$1" --arg id "$2" \
    'first(inputs | fromjson? | select(.type == $type and .id == $id))' "$rpc_log"
}

omp_alive() {
  [[ -n "$omp_pid" ]] && running "$omp_pid"
}

omp_tail() {
  tail -n 5 "${session_home}/stderr-${launches}.log" 2>/dev/null | tr '\n' ' '
}

# start_session PLUGIN [RESUME_FILE]: omp in rpc mode on the prepared session, with PLUGIN as its
# only extension; sets sid and session_file from its get_state answer. A fresh session is sent one
# prompt, so its session file exists on disk: omp writes the file at its first answer.
start_session() {
  local with_plugin=$1 resume=${2:-}
  launches=$((launches + 1))
  rpc_log="${session_home}/rpc-${launches}.log"
  rm -f "${session_home}/stdin"
  mkfifo "${session_home}/stdin"
  exec {omp_fd}<>"${session_home}/stdin"
  local -a args=(--mode rpc --no-ui --model "$model" --thinking off --cwd "${session_home}/cwd"
    --session-dir "${session_home}/sessions" --no-extensions --no-skills --no-rules --no-tools --no-lsp
    --no-title --append-system-prompt "$system_note" -e "$with_plugin")
  [[ -z "$resume" ]] || args+=(--resume "$resume")
  local -a assignments=(HOME="${session_home}/home" TMPDIR="${session_home}/tmp" OMP_SESSION_STORAGE=file
    ENVOY_HEARTBEAT_MS="$heartbeat_ms")
  [[ -z "$token_file" ]] || assignments+=(ENVOY_TOKEN_FILE="$token_file")
  (isolated_exec "$provider_env" "${assignments[@]}" -- "$omp_bin" "${args[@]}") \
    <"${session_home}/stdin" >"$rpc_log" 2>"${session_home}/stderr-${launches}.log" &
  omp_pid=$!
  note "omp started (pid ${omp_pid}) with -e ${with_plugin}${resume:+, resuming ${resume}}"
  rpc_send '{"type":"get_state","id":"state"}'
  await "$omp_pid" omp "${session_home}/stderr-${launches}.log" 60 rpc_frame response state
  local state
  state="$(rpc_frame response state)"
  jq -e '.success == true' <<<"$state" >/dev/null || fail "omp refused get_state: ${state}"
  sid="$(jq -r '.data.sessionId' <<<"$state")"
  session_file="$(jq -r '.data.sessionFile' <<<"$state")"
  subject="notifications.agent.${sid}"
  note "session ${sid}, session file ${session_file}"
  [[ -z "$resume" ]] || return 0
  rpc_send '{"type":"prompt","id":"first","message":"Reply with the single word ok."}'
  await "$omp_pid" "omp's first prompt" "${session_home}/stderr-${launches}.log" 120 rpc_frame prompt_result first
  local answer
  answer="$(jq -cnR '[inputs | fromjson? | select(.type == "message" and .message.role == "assistant") | .message]
    | last | {stopReason, errorMessage, model}' "$session_file" 2>/dev/null)" ||
    fail "omp finished its first prompt but wrote no session file at ${session_file}"
  if ! jq -e '.stopReason == "stop"' <<<"$answer" >/dev/null; then
    printf 'smoke-replay: the model provider did not answer the first prompt: %s\n' "$answer" >&2
    exit 2
  fi
  note "first prompt answered by $(jq -r .model <<<"$answer"); session file written"
}

# stop_session_gracefully: end of input, which omp answers by shutting down, so the plugin
# unregisters itself.
stop_session_gracefully() {
  close_omp_stdin
  local deadline=$((SECONDS + 30))
  while omp_alive && ((SECONDS < deadline)); do sleep 0.5; done
  omp_alive && fail "omp (pid ${omp_pid}) did not exit within 30s of the end of its input"
  wait "$omp_pid" 2>/dev/null || true
  omp_pid=""
}

kill_session() {
  close_omp_stdin
  kill -KILL "$omp_pid" 2>/dev/null || true
  wait "$omp_pid" 2>/dev/null || true
  omp_pid=""
}

registered() {
  listener_curl -f "${envoy_url}/v1/sessions" | jq -e --arg s "$sid" 'any(.[]; .session_id == $s)'
}

wait_registered() {
  await "$omp_pid" "the session's registration at ${envoy_url}" "${session_home}/stderr-${launches}.log" 90 registered
  note "registered at the listener: $(listener_curl -f "${envoy_url}/v1/sessions" |
    jq -c --arg s "$sid" 'first(.[] | select(.session_id == $s)) | {session_id, dir, last_seen}')"
}

# The session's cursor on its own subject: the replay state the plugin wrote once its first sweep
# read the stream's watermark. A publish before that is history the session never followed.
cursor() {
  jq -cnR --arg s "$subject" '[inputs | fromjson? | select(.type == "custom" and .customType == "envoy-replay-state")]
    | last | .data.subjects[$s] // empty' "$session_file" 2>/dev/null
}

cursor_started() {
  [[ -n "$(cursor)" ]]
}

# The session's envoy-message entries carrying an event id.
card_entries() {
  jq -cnR --arg id "$1" 'inputs | fromjson? | select(.type == "custom_message" and .customType == "envoy-message")
    | select(.content | if type == "string" then . else tostring end | contains($id))
    | {timestamp, customType, content}' "$session_file"
}

cards() {
  local n
  n="$(jq -nR --arg id "$1" '[inputs | fromjson? | select(.type == "custom_message" and .customType == "envoy-message")
    | select(.content | if type == "string" then . else tostring end | contains($id))] | length' "$session_file" 2>/dev/null)" ||
    n=0
  printf '%s\n' "${n:-0}"
}

has_card() {
  (($(cards "$1") >= 1))
}

show_cards() {
  local line
  while IFS= read -r line; do printf '   transcript: %s\n' "$line"; done < <(card_entries "$1")
}

# ---- NATS's view of the session (local mode) --------------------------------------------------
# The connections natsd holds for the session: named omp-<id>, or subscribed to its agent subject.
session_connections() {
  curl -fsS --max-time 5 "${nats_monitor}/connz?subs=1&limit=1024" |
    jq -c --arg n "omp-${sid}" --arg s "$subject" '[.connections[]
      | select(.name == $n or ((.subscriptions_list // []) | index($s)))
      | {cid, name, client: "\(.ip):\(.port)", subscribed: ((.subscriptions_list // []) | index($s) != null)}]'
}

subscribed() {
  session_connections | jq -e 'any(.[]; .subscribed)'
}

disconnected() {
  session_connections | jq -e 'length == 0'
}

# ---- publishing --------------------------------------------------------------------------------
id_base="$(date +%s%3N)"
readonly id_base
event_id=""

# The JetStream publish and the stored-message read, as the outbox publishes (publisher.go:
# js.Publish with MsgId = dedupe_key + ":" + topic) and as STREAM.MSG.GET reads it back.
readonly publish_js='
import { connect, StringCodec } from "nats";
const { SMOKE_NATS_URL, SMOKE_SUBJECT, SMOKE_ENVELOPE, SMOKE_MSG_ID } = process.env;
const nc = await connect({ servers: SMOKE_NATS_URL, name: "smoke-replay-publisher", timeout: 5000 });
try {
  const ack = await nc.jetstream().publish(SMOKE_SUBJECT, StringCodec().encode(SMOKE_ENVELOPE), {
    msgID: SMOKE_MSG_ID,
    timeout: 5000,
  });
  const stored = await (await nc.jetstreamManager()).streams.getMessage(ack.stream, { seq: ack.seq });
  console.log(JSON.stringify({
    ack: { stream: ack.stream, seq: ack.seq, duplicate: ack.duplicate },
    stored: {
      seq: stored.seq,
      subject: stored.subject,
      msg_id: stored.header?.get("Nats-Msg-Id") ?? null,
      event_id: JSON.parse(StringCodec().decode(stored.data)).event_id,
    },
  }));
} finally {
  await nc.close();
}
'

# dispatch_event N LABEL: a Dispatch comment event as the outbox serializes model.Event.
dispatch_event() {
  jq -nc --argjson id "$1" --arg label "$2" --arg created "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" \
    '{id: $id, issue_key: "SMOKE-1", artifact_id: null, project: "SMOKE", seq: 1, type: "comment.created",
      actor: {kind: "human", id: "smoke-replay"}, notify: true, created_at: $created,
      payload: {id: "smoke-replay-comment-\($id)", body: "smoke-replay \($label)"}}'
}

# replay_watermark: the stream's last sequence as the listener's /v1/replay reports it for this
# session's subject (remote mode's way to find the publish in the stream afterwards).
replay_read() {
  local body
  if [[ -n "${2:-}" ]]; then
    body="$(jq -nc --arg s "$sid" --arg subj "$subject" --argjson after "$2" '{session_id: $s, subjects: [{subject: $subj, after: $after}]}')"
  else
    body="$(jq -nc --arg s "$sid" --arg subj "$subject" '{session_id: $s, subjects: [{subject: $subj}]}')"
  fi
  listener_curl -X POST -H 'Content-Type: application/json' -d "$body" -w '\n%{http_code}' "${envoy_url}/v1/replay"
}

watermark=""
take_watermark() {
  [[ "$mode" == remote ]] || return 0
  local answer code
  answer="$(replay_read)" || fail "POST ${envoy_url}/v1/replay failed"
  code="${answer##*$'\n'}"
  [[ "$code" == 200 ]] || fail "POST ${envoy_url}/v1/replay answered ${code}: ${answer%$'\n'*}"
  watermark="$(jq -r '.subjects[0].next_after' <<<"${answer%$'\n'*}")"
  note "/v1/replay starts a cursor for ${subject} at ${watermark}"
}

# publish N LABEL: publishes one Dispatch-shaped envelope to the session's agent subject and shows
# the stream stored it; sets event_id.
publish() {
  local n=$((id_base + $1)) event summary
  event="$(dispatch_event "$n" "$2")"
  summary="SMOKE-1: comment by smoke-replay (${2})"
  if [[ "$mode" == local ]]; then
    event_id="dispatch-${n}"
    local envelope result msg_id
    envelope="$(jq -nc --arg id "$event_id" --arg src "$n" --arg topic "$subject" --argjson at "$(date +%s%3N)" \
      --arg summary "$summary" --arg payload "$event" \
      '{event_id: $id, source: "dispatch", source_event_id: $src, topic: $topic, dedupe_key: $id,
        issued_at: $at, payload_summary: $summary, payload: $payload, trace_id: $id}')"
    msg_id="${event_id}:${subject}"
    result="$(cd -- "$repo_root" && isolated_exec "" HOME="${work}/bun-home" TMPDIR="${work}" \
      SMOKE_NATS_URL="nats://127.0.0.1:${nats_port}" SMOKE_SUBJECT="$subject" SMOKE_ENVELOPE="$envelope" \
      SMOKE_MSG_ID="$msg_id" -- bun -e "$publish_js" 2>>"${work}/logs/publish.log")" ||
      fail "the JetStream publish failed: $(tail -n 5 "${work}/logs/publish.log")"
    note "published ${event_id} through JetStream to ${subject} with Nats-Msg-Id ${msg_id}: $(jq -c .ack <<<"$result")"
    jq -e --arg s "$subject" --arg id "$event_id" --arg m "$msg_id" \
      '.ack.stream == "ENVOY_NOTIFICATIONS" and .ack.duplicate == false and .stored.seq == .ack.seq
        and .stored.subject == $s and .stored.event_id == $id and .stored.msg_id == $m' <<<"$result" >/dev/null ||
      fail "the stream did not store the publish as sent: ${result}"
    note "STREAM.MSG.GET ENVOY_NOTIFICATIONS seq $(jq -r .stored.seq <<<"$result"): $(jq -c .stored <<<"$result")"
  else
    local body answer code
    body="$(jq -nc --arg target "$sid" --arg message "$summary" --arg payload "$event" --arg key "smoke-replay-${n}" \
      '{target_session: $target, source: "dispatch", message: $message, payload: $payload, idempotency_key: $key}')"
    answer="$(listener_curl -X POST -H 'Content-Type: application/json' -d "$body" -w '\n%{http_code}' \
      "${envoy_url}/v1/messages/send")" || fail "POST ${envoy_url}/v1/messages/send failed"
    code="${answer##*$'\n'}"
    [[ "$code" == 200 ]] || fail "POST ${envoy_url}/v1/messages/send answered ${code}: ${answer%$'\n'*}"
    event_id="$(jq -r .event_id <<<"${answer%$'\n'*}")"
    note "published ${event_id} through ${envoy_url}/v1/messages/send to ${subject}"
    answer="$(replay_read "$watermark")" || fail "POST ${envoy_url}/v1/replay failed"
    code="${answer##*$'\n'}"
    [[ "$code" == 200 ]] || fail "POST ${envoy_url}/v1/replay answered ${code}: ${answer%$'\n'*}"
    jq -e --arg id "$event_id" 'any(.subjects[0].messages[]; (.data | fromjson | .event_id) == $id)' \
      <<<"${answer%$'\n'*}" >/dev/null ||
      fail "/v1/replay after ${watermark} does not hold ${event_id}: ${answer%$'\n'*}"
    note "/v1/replay after ${watermark} holds it: $(jq -c --arg id "$event_id" '.subjects[0].messages[]
      | select((.data | fromjson | .event_id) == $id) | {seq, subject, time}' <<<"${answer%$'\n'*}")"
  fi
}

# ---- the legs ----------------------------------------------------------------------------------
# break_connection: kills the proxy and shows the session lost its connection and subscription.
break_connection() {
  kill_proxy
  local pid
  for pid in $proxy_children; do
    wait_until 5 ended "$pid" || fail "the proxy's connection process ${pid} survived the kill by 5s"
  done
  note "proxy killed (it and its connection processes: ${proxy_children//$'\n'/ })"
  if [[ "$mode" == local ]]; then
    wait_until 15 disconnected ||
      fail "natsd still holds the session's connection 15s after the proxy died: $(session_connections)"
    note "natsd /connz?subs=1: no connection named omp-${sid} and none subscribed to ${subject}: $(session_connections)"
  else
    note "remote mode: NATS's monitoring is not reachable from here; the break is the proxy's processes ending"
  fi
}

restore_connection() {
  start_proxy
  restored_at=$SECONDS
  note "proxy restarted (pid ${proxy_pid}) on 127.0.0.1:${proxy_port}"
}

# expect_once ID WHEN: exactly one card for ID within two heartbeats of WHEN (a SECONDS value),
# and still exactly one three heartbeats later. The card is the replay's: a live copy carries no
# "while this session's connection was down" line (replayedCard, src/missed-messages.ts).
expect_once() {
  local id=$1 since=$2 remaining
  remaining=$((since + 2 * heartbeat_s - SECONDS))
  ((remaining > 0)) || remaining=0
  wait_until "$remaining" has_card "$id" ||
    fail "no envoy-message entry for ${id} within two heartbeats ($((2 * heartbeat_s))s); cards for it: $(cards "$id")"
  note "envoy-message entry for ${id} after $((SECONDS - since))s; entries for it: $(cards "$id")"
  (($(cards "$id") == 1)) || {
    show_cards "$id"
    fail "$(cards "$id") envoy-message entries carry ${id}, want exactly one"
  }
  sleep $((3 * heartbeat_s))
  (($(cards "$id") == 1)) || {
    show_cards "$id"
    fail "$(cards "$id") envoy-message entries carry ${id} three heartbeats later, want exactly one"
  }
  card_entries "$id" | jq -e '.content | contains("while this session'"'"'s connection was down")' >/dev/null || {
    show_cards "$id"
    fail "the entry for ${id} is not a replayed card: it has no \"while this session's connection was down\" line"
  }
  note "three heartbeats ($((3 * heartbeat_s))s) later still exactly one entry for ${id}, the replay's card:"
  show_cards "$id"
}

# leg_one WITH_PLUGIN N LABEL: register, break, publish, restore; leaves event_id and restored_at.
restored_at=0
leg_one() {
  start_proxy
  note "proxy (pid ${proxy_pid}) on 127.0.0.1:${proxy_port} fronts ${upstream}"
  start_session "$1"
  wait_registered
  if [[ "$mode" == local ]]; then
    wait_until 60 subscribed || fail "natsd shows no connection subscribed to ${subject} within 60s: $(session_connections)"
    note "natsd /connz?subs=1 before the break: $(session_connections)"
  fi
  if [[ "$1" == "$plugin" ]]; then
    wait_until 60 cursor_started || fail "the session wrote no replay cursor for ${subject} within 60s"
    note "replay cursor for ${subject}: $(cursor)"
  else
    sleep $((2 * heartbeat_s))
  fi
  take_watermark
  break_connection
  publish "$2" "$3"
  (($(cards "$event_id") == 0)) || fail "${event_id} reached the session while its connection was down"
  restore_connection
}

begin leg-1-reconnect
prepare_session main
leg_one "$plugin" 1 "leg 1, published while the connection was down"
leg1_event="$event_id"
expect_once "$leg1_event" "$restored_at"
if [[ "$mode" == local ]]; then
  note "natsd /connz?subs=1 after the restart: $(session_connections)"
fi
main_sid="$sid"
main_session_file="$session_file"

begin leg-2-resume
if [[ "$mode" == local ]]; then
  stop_session_gracefully
  wait_until 15 disconnected || fail "natsd still holds the session's connection after omp exited: $(session_connections)"
  note "omp exited at the end of its input; natsd /connz?subs=1 shows no connection for ${sid}: $(session_connections)"
else
  # /v1/messages/send publishes only to a registered session, and an omp that shuts down cleanly
  # unregisters itself, so remote mode ends it with SIGKILL and its registration outlives it.
  take_watermark
  kill_session
  note "omp SIGKILLed; its registration outlives it, so /v1/messages/send still reaches its subject"
fi
publish 2 "leg 2, published while omp was stopped"
leg2_event="$event_id"
start_session "$plugin" "$main_session_file"
[[ "$sid" == "$main_sid" && "$session_file" == "$main_session_file" ]] ||
  fail "the resumed omp is on session ${sid} (${session_file}), not ${main_sid} (${main_session_file})"
wait_registered
returned_at=$SECONDS
expect_once "$leg2_event" "$returned_at"
(($(cards "$leg1_event") == 1)) || {
  show_cards "$leg1_event"
  fail "after the resume $(cards "$leg1_event") entries carry leg 1's ${leg1_event}, want exactly one"
}
note "leg 1's ${leg1_event} is still exactly one entry after the resume"
stop_session_gracefully
kill_proxy

begin control
prepare_session control
leg_one "$control_plugin" 3 "control, published while the connection was down"
control_event="$event_id"
sleep $((5 * heartbeat_s))
(($(cards "$control_event") == 0)) || {
  show_cards "$control_event"
  fail "the released plugin showed ${control_event}; the control does not reproduce the loss"
}
note "five heartbeats ($((5 * heartbeat_s))s) after the restart the control session holds no entry for ${control_event}"
if [[ "$mode" == local ]]; then
  note "natsd /connz?subs=1 after the restart: $(session_connections)"
  wait_until 30 subscribed || fail "the control session did not resubscribe to ${subject} within 30s"
fi
take_watermark
publish 4 "control, published live after the restart"
live_event="$event_id"
live_at=$SECONDS
wait_until $((2 * heartbeat_s)) has_card "$live_event" ||
  fail "the control session showed no entry for its live ${live_event} either: it was not receiving"
(($(cards "$live_event") == 1)) || fail "$(cards "$live_event") entries carry the control's live ${live_event}, want one"
note "the control session showed its live ${live_event} after $((SECONDS - live_at))s, so it was connected and writing cards:"
show_cards "$live_event"
(($(cards "$control_event") == 0)) || fail "the released plugin showed ${control_event} late"
stop_session_gracefully
kill_proxy

passed=1
printf 'SMOKE REPLAY PASS: %s\n' "the plugin under test showed ${leg1_event} (published while its NATS connection was down) and ${leg2_event} (published while omp was stopped, then resumed) exactly once each, still once three heartbeats later; the released plugin (${control_version}) showed nothing for ${control_event}, published the same way, and showed its live ${live_event}"
