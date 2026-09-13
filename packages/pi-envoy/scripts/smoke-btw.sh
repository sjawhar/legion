#!/usr/bin/env bash
# smoke-btw.sh — real end-to-end targeted Dispatch delivery smoke.
# See packages/pi-envoy/scripts/README.md for prerequisites and usage.
set -euo pipefail

readonly envoy_url="${ENVOY_URL:-http://127.0.0.1:9020}"
readonly dispatch_url="${DISPATCH_URL:-http://127.0.0.1:8766}"
readonly dispatch_issue="${DISPATCH_SMOKE_ISSUE:?set DISPATCH_SMOKE_ISSUE to an open issue key}"
readonly mode="${MODE:-btw}"
readonly omp_bin="${OMP_BIN:-omp}"
readonly session_timeout=60
readonly answer_timeout=90
readonly teardown_timeout=15
readonly plugin_pkg="${HOME}/.omp/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json"
readonly envoy_config="${HOME}/.config/opencode/envoy.json"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# The bearer the installed plugin resolves: DISPATCH_TOKEN_FILE, then DISPATCH_TOKEN, then
# dispatch.token in ~/.config/opencode/envoy.json.
resolve_dispatch_bearer() {
  if [[ -n "${DISPATCH_TOKEN_FILE:-}" ]]; then
    local token
    token="$(tr -d '[:space:]' <"$DISPATCH_TOKEN_FILE" 2>/dev/null)" ||
      fail "DISPATCH_TOKEN_FILE names ${DISPATCH_TOKEN_FILE}, which could not be read"
    [[ -n "$token" ]] || fail "DISPATCH_TOKEN_FILE names ${DISPATCH_TOKEN_FILE}, which is empty"
    printf '%s' "$token"
    return
  fi
  if [[ -n "${DISPATCH_TOKEN:-}" ]]; then
    printf '%s' "$DISPATCH_TOKEN"
    return
  fi
  [[ -f "$envoy_config" ]] ||
    fail "no Dispatch credential: set DISPATCH_AUTH_HEADER, DISPATCH_TOKEN(_FILE), or dispatch.token in ${envoy_config}"
  jq -er '.dispatch.token // empty' "$envoy_config" 2>/dev/null ||
    fail "${envoy_config} has no dispatch.token; set DISPATCH_AUTH_HEADER or DISPATCH_TOKEN(_FILE)"
}

write_dispatch_header_file() {
  local output="$1"
  (umask 077 && printf '%s\n' "$dispatch_auth_header" >"$output")
}

skip() {
  printf 'SKIP: %s\n' "$*"
  exit 0
}

case "$mode" in
  btw|steer) ;;
  *) fail "MODE must be btw or steer, got ${mode}" ;;
esac

command -v "$omp_bin" >/dev/null 2>&1 || fail "$omp_bin is not on PATH"
command -v tmux >/dev/null 2>&1 || fail "tmux is required"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"
[[ -f "$plugin_pkg" ]] || fail "installed plugin not found at ${plugin_pkg}"
plugin_version="$(jq -er '.version' "$plugin_pkg" 2>/dev/null)" ||
  fail "could not read .version out of ${plugin_pkg}"
readonly plugin_version

# Who asks. A human header (browser cookie or trusted identity header) makes the smoke card
# human-authored, exactly as the dashboard would. Without one, the script is an agent: it
# authenticates with the plugin's Dispatch bearer and names itself in `actor` as
# SMOKE_ACTOR_SESSION (default smoke-btw-<hex>), so the card is visibly agent-authored.
hex="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
readonly hex
bearer=""
actor_session=""
if [[ -n "${DISPATCH_AUTH_HEADER:-}" ]]; then
  readonly dispatch_auth_header="$DISPATCH_AUTH_HEADER"
  readonly asker="human (${dispatch_auth_header%%:*} header)"
else
  bearer="$(resolve_dispatch_bearer)"
  readonly dispatch_auth_header="Authorization: Bearer ${bearer}"
  actor_session="${SMOKE_ACTOR_SESSION:-smoke-btw-${hex}}"
  readonly asker="agent session ${actor_session} (bearer)"
fi
readonly bearer actor_session

readonly session="legion-smoke-${hex}"
readonly message="smoke-${mode}-payload-${hex}"
workdir=""
dispatch_header_file=""
session_created=0

cleanup() {
  if (( session_created )); then tmux kill-session -t "$session" 2>/dev/null || true; fi
  if [[ -n "$workdir" ]]; then rm -rf "$workdir"; fi
}
trap cleanup EXIT INT TERM

http_status() {
  local output="$1"
  shift
  local status
  if status="$(curl --connect-timeout 2 --max-time 5 -sS -o "$output" -w '%{http_code}' "$@" 2>/dev/null)"; then
    printf '%s' "$status"
  else
    printf '000'
  fi
}

wait_for_session() {
  local deadline=$((SECONDS + session_timeout)) response session_id
  while (( SECONDS < deadline )); do
    response="$(curl --connect-timeout 2 --max-time 5 -fsS "${envoy_url}/v1/sessions?dir=${workdir}" 2>/dev/null || true)"
    session_id="$(jq -er --arg dir "$workdir" '.[] | select(.dir == $dir) | .session_id' <<<"$response" 2>/dev/null || true)"
    if [[ -n "$session_id" ]]; then printf '%s\n' "$session_id"; return 0; fi
    sleep 1
  done
  return 1
}

wait_for_reply() {
  local message_id="$1" deadline=$((SECONDS + answer_timeout)) output status
  output="$(mktemp)"
  while (( SECONDS < deadline )); do
    status="$(http_status "$output" -H "@${dispatch_header_file}" "${dispatch_url}/api/v1/issues/${dispatch_issue}/messages/${message_id}")"
    if [[ "$status" == "200" ]] && jq -e '.replies | length > 0' "$output" >/dev/null 2>&1; then
      rm -f "$output"
      return 0
    fi
    sleep 1
  done
  printf 'last reply response (%s): %s\n' "$status" "$(cat "$output")" >&2
  rm -f "$output"
  return 1
}

wait_for_session_clear() {
  local session_id="$1" deadline=$((SECONDS + teardown_timeout)) response
  while (( SECONDS < deadline )); do
    response="$(curl --connect-timeout 2 --max-time 5 -fsS "${envoy_url}/v1/sessions?dir=${workdir}" 2>/dev/null || true)"
    if ! jq -e --arg id "$session_id" '.[] | select(.session_id == $id)' <<<"$response" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# The primary turn must be mid-sleep when the targeted message lands: that is what
# proves BTW does not wait for a turn boundary. The prompt asks for one bash
# command that creates a sentinel file and then sleeps, so the file exists exactly
# while the tool call is blocked in the sleep. Pane text would not do: the model
# may narrate the command before running it, and omp's bash `sleep` is an
# in-process builtin with no OS process to look for.
wait_for_sleep_running() {
  local deadline=$((SECONDS + session_timeout))
  while (( SECONDS < deadline )); do
    [[ -e "$sleep_sentinel" ]] && return 0
    sleep 1
  done
  return 1
}

workdir="$(mktemp -d "/tmp/${session}.XXXXXX")"
dispatch_header_file="${workdir}/dispatch-headers"
write_dispatch_header_file "$dispatch_header_file"
printf 'plugin version: %s (from %s)\n' "$plugin_version" "$plugin_pkg"
printf 'asking as %s\n' "$asker"
printf 'starting isolated OMP session %s in %s\n' "$session" "$workdir"
# The target must answer on the same Dispatch this script asks. An existing tmux
# server gives a new session its own global environment, not this shell's, so
# the coordinates go in with -e: the URL as is, the bearer through a 0600 file
# (never as argv, which /proc exposes). Human-header mode has no bearer to
# forward; the spawned session then relies on its own configured dispatch.token.
launch_env=(-e "DISPATCH_URL=${dispatch_url}")
if [[ -n "$bearer" ]]; then
  token_file="${workdir}/dispatch-token"
  (umask 077 && printf '%s' "$bearer" >"$token_file")
  launch_env+=(-e "DISPATCH_TOKEN_FILE=${token_file}")
fi
# The prompt is omp's initial message, not typed keystrokes: the TUI drops keys
# sent before its editor is up, and `send-keys` into a pane whose process is
# still initialising is not reliable. This session is the only target in the
# POST below. Steer may be queued until the sleep reaches its next tool
# boundary; BTW replies through its side turn.
readonly sleep_sentinel="${workdir}/sleep-started"
readonly prompt="Run exactly this one bash command first, before anything else: touch ${sleep_sentinel} && sleep 45. When a Dispatch message arrives, follow its reply_with instruction exactly; do not message any other session."
tmux new-session -d -s "$session" -x 220 -y 50 -c "$workdir" "${launch_env[@]}" "$omp_bin" "$prompt"
session_created=1

printf 'waiting up to %ss for this OMP session to register with Envoy\n' "$session_timeout"
target_session="$(wait_for_session)" || fail "session ${session} did not register under ${workdir}"
printf 'registered session: %s\n' "$target_session"

printf 'waiting up to %ss for the session to start its sleep\n' "$session_timeout"
wait_for_sleep_running || fail "session ${session} never started the sleep: $(tmux capture-pane -t "$session" -p | tail -5)"

request_file="$(mktemp)"
response_file="$(mktemp)"
trap 'rm -f "$request_file" "$response_file"; cleanup' EXIT INT TERM
jq -nc --arg body "$message" --arg target "session:${target_session}" --arg delivery "$mode" --arg actor "$actor_session" \
  '{body: $body, target: $target, delivery: $delivery} + (if $actor == "" then {} else {actor: {kind: "session", id: $actor}} end)' >"$request_file"
status="$(http_status "$response_file" -H "@${dispatch_header_file}" -X POST -H 'Content-Type: application/json' \
  --data-binary "@${request_file}" "${dispatch_url}/api/v1/issues/${dispatch_issue}/messages")"
if [[ "$status" == "404" ]]; then
  skip "Dispatch does not expose the targeted message route for ${dispatch_issue}; use a server that supports targeted messages before running the live smoke"
fi
if [[ "$status" == "400" && -n "$bearer" ]] && jq -e '.code == "ACTOR_KIND"' "$response_file" >/dev/null 2>&1; then
  skip "this Dispatch server requires a human-authenticated targeted message; use a server that accepts bearer-authored targets or set DISPATCH_AUTH_HEADER"
fi
[[ "$status" == "201" ]] || fail "targeted ${mode} POST returned ${status}: $(cat "$response_file")"
message_id="$(jq -er '.id' "$response_file")" || fail "targeted ${mode} POST had no message id: $(cat "$response_file")"
response_target="$(jq -er '.target' "$response_file")" || fail "targeted ${mode} POST had no target: $(cat "$response_file")"
[[ "$response_target" == "session:${target_session}" ]] || fail "Dispatch targeted ${response_target}, not this session ${target_session}"

printf 'waiting up to %ss for Dispatch reply to message %s\n' "$answer_timeout" "$message_id"
wait_for_reply "$message_id" || fail "no ${mode} reply arrived for ${message_id}"

printf 'killing isolated OMP session %s and checking Envoy teardown\n' "$session"
tmux kill-session -t "$session" || fail "tmux kill-session -t ${session} failed"
session_created=0
wait_for_session_clear "$target_session" || fail "session ${target_session} remained live after ${teardown_timeout}s"
printf 'PASS: %s delivery from Dispatch (asked as %s) reached only %s and replied on %s (plugin %s)\n' \
  "$mode" "$asker" "$target_session" "$dispatch_issue" "$plugin_version"
