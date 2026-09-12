#!/usr/bin/env bash
# smoke-btw.sh — real end-to-end targeted Dispatch delivery smoke.
# See packages/pi-envoy/scripts/README.md for prerequisites and usage.
set -euo pipefail

readonly envoy_url="${ENVOY_URL:-http://127.0.0.1:9020}"
readonly dispatch_url="${DISPATCH_URL:-http://127.0.0.1:8766}"
readonly dispatch_issue="${DISPATCH_SMOKE_ISSUE:?set DISPATCH_SMOKE_ISSUE to an open issue key}"
readonly dispatch_auth_header="${DISPATCH_AUTH_HEADER:?set DISPATCH_AUTH_HEADER to one human-authenticated HTTP header}"
readonly mode="${MODE:-btw}"
readonly omp_bin="${OMP_BIN:-omp}"
readonly session_timeout=60
readonly answer_timeout=90
readonly teardown_timeout=15
readonly plugin_pkg="${HOME}/.omp/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
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

hex="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
readonly hex
readonly session="legion-smoke-${hex}"
readonly message="smoke-${mode}-payload-${hex}"
workdir=""
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
    status="$(http_status "$output" -H "$dispatch_auth_header" "${dispatch_url}/api/v1/issues/${dispatch_issue}/messages/${message_id}")"
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

workdir="$(mktemp -d "/tmp/${session}.XXXXXX")"
printf 'plugin version: %s (from %s)\n' "$plugin_version" "$plugin_pkg"
printf 'starting isolated OMP session %s in %s\n' "$session" "$workdir"
tmux new-session -d -s "$session" -x 220 -y 50 -c "$workdir" "$omp_bin"
session_created=1

# This session is the only target in the POST below. Steer may be queued until
# the sleep reaches its next tool boundary; BTW replies through its side turn.
tmux send-keys -t "$session" "Run bash sleep 45. When a Dispatch message arrives, follow its reply_with instruction exactly; do not message any other session." Enter
printf 'waiting up to %ss for this OMP session to register with Envoy\n' "$session_timeout"
target_session="$(wait_for_session)" || fail "session ${session} did not register under ${workdir}"
printf 'registered session: %s\n' "$target_session"

request_file="$(mktemp)"
response_file="$(mktemp)"
trap 'rm -f "$request_file" "$response_file"; cleanup' EXIT INT TERM
jq -nc --arg body "$message" --arg target "session:${target_session}" --arg delivery "$mode" \
  '{body: $body, target: $target, delivery: $delivery}' >"$request_file"
status="$(http_status "$response_file" -X POST -H "$dispatch_auth_header" -H 'Content-Type: application/json' \
  --data-binary "@${request_file}" "${dispatch_url}/api/v1/issues/${dispatch_issue}/messages")"
if [[ "$status" == "404" ]]; then
  skip "Dispatch does not expose this targeted message route for ${dispatch_issue}; deploy this lane before running the live smoke"
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
printf 'PASS: %s delivery from Dispatch reached only %s and replied on %s (plugin %s)\n' \
  "$mode" "$target_session" "$dispatch_issue" "$plugin_version"
