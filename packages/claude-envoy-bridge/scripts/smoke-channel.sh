#!/usr/bin/env bash
# smoke-channel.sh — real Claude Code channel delivery smoke.
# See README.md for prerequisites, configuration, and why this is not CI.
set -euo pipefail

readonly envoy_url="${ENVOY_URL:-http://127.0.0.1:9020}"
readonly nats_url="${ENVOY_NATS_URL:?set ENVOY_NATS_URL to the live Envoy NATS listener}"
readonly claude_bin="${CLAUDE_BIN:-claude}"
readonly channel_entry="${CLAUDE_CHANNEL_ENTRY:?set CLAUDE_CHANNEL_ENTRY, e.g. plugin:claude-envoy-bridge@<marketplace>}"
# `--channels` only works for an entry the organization allowlists; on this fleet
# that is the `legion-plugins` marketplace. Everything else (a dev marketplace, a
# bare `server:` entry) needs the development bypass. Always overridable.
if [[ -n "${CLAUDE_CHANNEL_FLAG:-}" ]]; then
  readonly channel_flag="$CLAUDE_CHANNEL_FLAG"
elif [[ "$channel_entry" == *"@legion-plugins" ]]; then
  readonly channel_flag="--channels"
else
  readonly channel_flag="--dangerously-load-development-channels"
fi
readonly plugin_dir="${CLAUDE_PLUGIN_DIR:-}"
readonly mcp_config="${CLAUDE_MCP_CONFIG:-}"
readonly startup_timeout=60
readonly session_timeout=45
readonly delivery_timeout=90
readonly teardown_timeout=15

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  [[ -n "${tmux_session:-}" ]] && tmux capture-pane -pt "$tmux_session" >&2 || true
  if [[ -n "${claude_output:-}" && -f "$claude_output" ]]; then
    printf -- '--- claude pane output (%s) ---\n' "$claude_output" >&2
    cat "$claude_output" >&2 || true
  fi
  exit 1
}

command -v "$claude_bin" >/dev/null 2>&1 || fail "$claude_bin is not on PATH"
command -v tmux >/dev/null 2>&1 || fail "tmux is required"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"

readonly nonce="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
readonly target_session="claude-channel-smoke-${nonce}"
readonly message="channel-smoke-payload-${nonce}"
workdir="$(mktemp -d "/tmp/${target_session}.XXXXXX")"
readonly workdir
readonly received_file="${workdir}/received.txt"
readonly claude_output="${workdir}/claude-output.txt"
readonly delivery_instruction="Envoy channel smoke: use Bash to write exactly ${message}, with no trailing newline, to ${received_file}; then reply CHANNEL_SMOKE_DONE."
tmux_session=""

cleanup() {
  [[ -n "$tmux_session" ]] && tmux kill-session -t "$tmux_session" 2>/dev/null || true
  rm -rf "$workdir"
}
trap cleanup EXIT INT TERM

# Claude Code shows up to three dialogs before its prompt, in an order that
# depends on what this machine has already consented to: folder trust, the
# development-channel warning, and first-use MCP server consent. Poll the pane
# and answer whichever is showing until the prompt appears.
answer_startup_dialogs() {
  local deadline=$((SECONDS + startup_timeout)) pane
  while (( SECONDS < deadline )); do
    pane="$(tmux capture-pane -pt "$tmux_session" 2>/dev/null || true)"
    if [[ "$pane" == *"Yes, I trust this folder"* ]]; then
      tmux send-keys -t "$tmux_session" Down Enter
    elif [[ "$pane" == *"I am using this for local development"* ]]; then
      tmux send-keys -t "$tmux_session" Enter
    elif [[ "$pane" == *"New MCP server found"* ]]; then
      tmux send-keys -t "$tmux_session" Enter
    elif [[ "$pane" == *"shift+tab to cycle"* || "$pane" == *"? for shortcuts"* ]]; then
      # The permission-mode status line is drawn only once the prompt is live.
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_session() {
  local deadline=$((SECONDS + session_timeout)) response
  while (( SECONDS < deadline )); do
    response="$(curl --connect-timeout 2 --max-time 5 -fsS "${envoy_url}/v1/sessions?dir=${workdir}" 2>/dev/null || true)"
    if jq -e --arg id "$target_session" '.[] | select(.session_id == $id)' <<<"$response" >/dev/null 2>&1; then
      session_row="$(jq -c --arg id "$target_session" '.[] | select(.session_id == $id) | {session_id, dir, capabilities}' <<<"$response")"
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_delivery() {
  local deadline=$((SECONDS + delivery_timeout))
  while (( SECONDS < deadline )); do
    if [[ -f "$received_file" ]] && [[ "$(cat "$received_file")" == "$message" ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_teardown() {
  local deadline=$((SECONDS + teardown_timeout)) response
  while (( SECONDS < deadline )); do
    response="$(curl --connect-timeout 2 --max-time 5 -fsS "${envoy_url}/v1/sessions?dir=${workdir}" 2>/dev/null || true)"
    if ! jq -e --arg id "$target_session" '.[] | select(.session_id == $id)' <<<"$response" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# The channel server gets a known direct Envoy route through the QA override.
# The smoke starts an interactive Claude Code session: channel notifications are
# meant to wake an already-open session, while -p has no interactive consent or
# plan-approval surfaces.
tmux_session="${target_session}"
session_row=""
claude_command=("$claude_bin")
[[ -z "$plugin_dir" ]] || claude_command+=(--plugin-dir "$plugin_dir")
[[ -z "$mcp_config" ]] || claude_command+=(--mcp-config "$mcp_config")
tmux new-session -d -s "$tmux_session" -x 220 -y 50 -c "$workdir" \
  env ENVOY_NATS_URL="$nats_url" ENVOY_URL="$envoy_url" ENVOY_SESSION_ID="$target_session" \
  CLAUDE_PROJECT_DIR="$workdir" CLAUDE_PLUGIN_DATA="${workdir}/plugin-data" \
  "${claude_command[@]}" "$channel_flag" "$channel_entry"
tmux set-option -t "$tmux_session" remain-on-exit on
# Everything Claude prints, dialogs included, lands in a file the failure path dumps.
tmux pipe-pane -t "$tmux_session" -o "cat >> '${claude_output}'"
answer_startup_dialogs || fail "Claude did not reach its prompt within ${startup_timeout}s"

printf 'starting interactive Claude channel smoke %s with %s %s\n' "$target_session" "$channel_flag" "$channel_entry"
wait_for_session || fail "channel session did not register with Envoy within ${session_timeout}s"
printf 'registered Envoy channel session %s\n' "$session_row"

response_file="$(mktemp)"
trap 'rm -f "$response_file"; cleanup' EXIT INT TERM
status="$(curl --connect-timeout 2 --max-time 5 -sS -o "$response_file" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' "${envoy_url}/v1/messages/send" \
  --data "$(jq -nc --arg target "$target_session" --arg body "$delivery_instruction" '{source:"human", target_session:$target, message:$body, idempotency_key:("channel-smoke-" + $target)}')" 2>/dev/null || true)"
[[ "$status" == "200" ]] || fail "Envoy direct send returned ${status}: $(cat "$response_file")"
printf 'sent direct Envoy event; waiting up to %ss for Claude to consume the channel notification\n' "$delivery_timeout"
wait_for_delivery || fail "Claude did not write the exact channel payload within ${delivery_timeout}s"

printf 'delivery observed; terminating Claude session and checking deregistration\n'
tmux kill-session -t "$tmux_session" || fail "tmux kill-session failed"
tmux_session=""
wait_for_teardown || fail "Envoy session remained registered after ${teardown_timeout}s"
printf 'PASS: Envoy event arrived through Claude Code channel notifications and the session deregistered — %s\n' "$session_row"
