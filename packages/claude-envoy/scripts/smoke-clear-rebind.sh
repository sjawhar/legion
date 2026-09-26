#!/usr/bin/env bash
# smoke-clear-rebind.sh — real Claude Code `/clear` handoff smoke: after `/clear`, the
# registry entry of the new session id must list its own direct subject and never the old one.
# See README.md "Manual smoke" for prerequisites; like smoke-channel.sh, never a CI step.
set -euo pipefail

readonly envoy_url="${ENVOY_URL:-http://127.0.0.1:9020}"
readonly nats_url="${ENVOY_NATS_URL:?set ENVOY_NATS_URL to the live Envoy NATS listener}"
readonly claude_bin="${CLAUDE_BIN:-claude}"
plugin_dir="${CLAUDE_PLUGIN_DIR:-$(dirname "${BASH_SOURCE[0]}")/..}"
plugin_dir="$(cd "$plugin_dir" && pwd)"
readonly plugin_dir
readonly heartbeat_ms="${ENVOY_HEARTBEAT_MS:-5000}"
readonly startup_timeout=60
readonly registration_timeout=45
readonly handoff_timeout=60

command -v "$claude_bin" >/dev/null 2>&1 || { echo "FAIL: $claude_bin is not on PATH" >&2; exit 1; }
for tool in tmux curl jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: $tool is required" >&2; exit 1; }
done

workdir="$(mktemp -d /tmp/claude-clear-rebind.XXXXXX)"
readonly workdir
readonly claude_output="${workdir}/claude-output.txt"
tmux_session="claude-clear-rebind-${workdir##*.}"
session_ids=()
claude_pid=""
server_pid=""

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  [[ -n "$tmux_session" ]] && tmux capture-pane -pt "$tmux_session" >&2 || true
  if [[ -f "$claude_output" ]]; then
    printf -- '--- claude pane output (%s) ---\n' "$claude_output" >&2
    cat "$claude_output" >&2 || true
  fi
  exit 1
}

cleanup() {
  [[ -n "$tmux_session" ]] && tmux kill-session -t "$tmux_session" 2>/dev/null || true
  # The channel server deregisters as it exits, and can outlive Claude; a registration
  # still in flight then must land before the entries are removed below.
  local deadline=$((SECONDS + 10)) pid
  for pid in "$claude_pid" "$server_pid"; do
    while [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null && (( SECONDS < deadline )); do
      sleep 0.2
    done
  done
  # A deregistered session keeps its interest entry until the listener reaps it; an
  # unsubscribe with no topics removes the entry now.
  for id in "${session_ids[@]}"; do
    curl --connect-timeout 2 --max-time 5 -fsS -o /dev/null -X POST -H 'Content-Type: application/json' \
      "${envoy_url}/v1/interests/unsubscribe" --data "{\"session_id\":\"${id}\",\"topics\":[]}" || true
  done
  # Claude Code keeps a transcript directory per project path, named after it.
  rm -rf "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects/${workdir//[^A-Za-z0-9]/-}" "$workdir"
}
trap cleanup EXIT INT TERM

# The registry rows this Claude session owns: it registers with its project directory.
entries() {
  local rows
  rows="$(curl --connect-timeout 2 --max-time 5 -fsS "${envoy_url}/v1/sessions")" \
    || fail "the Envoy listener at ${envoy_url} did not answer /v1/sessions"
  jq -c --arg dir "$workdir" '[.[] | select(.dir == $dir) | {session_id, topics}]' <<<"$rows"
}

# Polls the entries until the jq filter prints something within `timeout` seconds; prints it.
await_entries() {
  local deadline=$((SECONDS + $1)) out
  shift
  while (( SECONDS < deadline )); do
    out="$(entries | jq -cr "$@")"
    [[ -n "$out" ]] && { printf '%s\n' "$out"; return 0; }
    sleep 1
  done
  return 1
}

answer_startup_dialogs() {
  local deadline=$((SECONDS + startup_timeout)) pane
  while (( SECONDS < deadline )); do
    pane="$(tmux capture-pane -pt "$tmux_session" 2>/dev/null || true)"
    if [[ "$pane" == *"Yes, I trust this folder"* ]]; then
      tmux send-keys -t "$tmux_session" Down Enter
    elif [[ "$pane" == *"New MCP server found"* ]]; then
      tmux send-keys -t "$tmux_session" Enter
    elif [[ "$pane" == *"shift+tab to cycle"* || "$pane" == *"? for shortcuts"* ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# User settings stay out (`--setting-sources project,local`) so an installed copy of the
# plugin does not start a second channel server in this session; `--plugin-dir` loads the
# build under test with its SessionStart hook, which writes the handoff file `/clear` needs.
tmux new-session -d -s "$tmux_session" -x 220 -y 50 -c "$workdir" \
  env ENVOY_NATS_URL="$nats_url" ENVOY_URL="$envoy_url" ENVOY_HEARTBEAT_MS="$heartbeat_ms" \
  CLAUDE_PROJECT_DIR="$workdir" \
  "$claude_bin" --setting-sources project,local --plugin-dir "$plugin_dir"
tmux set-option -t "$tmux_session" remain-on-exit on
claude_pid="$(tmux display-message -p -t "$tmux_session" '#{pane_pid}')"
tmux pipe-pane -t "$tmux_session" -o "cat >> '${claude_output}'"
answer_startup_dialogs || fail "Claude did not reach its prompt within ${startup_timeout}s"

before="$(await_entries "$registration_timeout" 'if length == 1 then .[0].session_id else empty end')" \
  || fail "no single registry entry for ${workdir} within ${registration_timeout}s: $(entries)"
session_ids+=("$before")
server_pid="$(pgrep -P "$claude_pid" -f 'dist/envoy-channel.js' | head -1 || true)"
printf 'registered %s\n' "$before"

tmux send-keys -t "$tmux_session" "/clear" Enter
cleared_at=$SECONDS
# shellcheck disable=SC2016 # $old is a jq variable
after_id="$(await_entries "$handoff_timeout" --arg old "$before" \
  'if length == 1 and .[0].session_id != $old then .[0].session_id else empty end')" \
  || fail "the entry did not move off ${before} within ${handoff_timeout}s: $(entries)"
session_ids+=("$after_id")
moved=$((SECONDS - cleared_at))
printf 'the entry moved %ss after /clear (heartbeat every %sms)\n' "$moved" "$heartbeat_ms"

# Checked at once and again two heartbeats later: a later registration must not bring it back.
check_entry() {
  local entry
  entry="$(entries | jq -c --arg id "$after_id" '.[] | select(.session_id == $id)')"
  printf '%s: %s\n' "$1" "$entry"
  jq -e --arg own "notifications.agent.${after_id}" '(.topics // []) | index($own)' <<<"$entry" >/dev/null \
    || fail "the new entry does not list its own direct subject notifications.agent.${after_id}"
  if jq -e --arg old "notifications.agent.${before}" '(.topics // []) | index($old)' <<<"$entry" >/dev/null; then
    fail "the new entry still lists the pre-/clear direct subject notifications.agent.${before}"
  fi
}
check_entry "after /clear"
# The server polls the handoff file; waiting for the heartbeat instead shows up here once the
# heartbeat is long (ENVOY_HEARTBEAT_MS=60000 makes this a real check of the poll).
move_limit=$((heartbeat_ms / 1000 - 1))
(( move_limit > 10 )) && move_limit=10
(( move_limit < 2 )) && move_limit=2
(( moved <= move_limit )) \
  || fail "the entry moved ${moved}s after /clear; the handoff poll should move it within ${move_limit}s"
sleep $((2 * heartbeat_ms / 1000 + 1))
check_entry "two heartbeats later"
printf 'PASS: /clear moved the entry %s -> %s without the old direct subject (plugin %s)\n' \
  "$before" "$after_id" "$plugin_dir"
