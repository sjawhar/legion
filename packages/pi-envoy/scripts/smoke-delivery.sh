#!/usr/bin/env bash
# smoke-delivery.sh — real end-to-end delivery smoke test.
# See packages/pi-envoy/scripts/README.md for the full runbook: what this
# proves, prerequisites, env overrides, and exit codes.
set -euo pipefail

readonly envoy_url="${ENVOY_URL:-http://127.0.0.1:9020}"
readonly omp_bin="${OMP_BIN:-omp}"
readonly tui_ready_timeout=30
readonly role_claim_timeout=60
readonly delivery_timeout=90
readonly teardown_timeout=15
readonly curl_connect_timeout=2
readonly curl_max_time=5
readonly plugin_pkg="${HOME}/.omp/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

command -v "$omp_bin" >/dev/null 2>&1 || fail "$omp_bin is not on PATH"
command -v tmux >/dev/null 2>&1 || fail "tmux is required"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"

# The smoke is meaningless without the real installed artifact: fail loudly
# on a missing file, invalid JSON, or a missing version field rather than
# reporting "unknown" and passing anyway.
[[ -f "$plugin_pkg" ]] || fail "installed plugin not found at ${plugin_pkg}"
plugin_version="$(jq -er '.version' "$plugin_pkg" 2>/dev/null)" ||
  fail "could not read .version out of ${plugin_pkg} (invalid JSON or missing field)"
readonly plugin_version

hex="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
readonly hex
readonly session="legion-smoke-${hex}"
readonly role="legion-smoke-${hex}"
readonly message="smoke-delivery-payload-${hex}"

# The trap is installed before anything that allocates a resource (including
# the temp dir itself), so a failure between allocation and use can never
# leak it: cleanup guards each resource on whether it was actually created.
session_created=0
workdir=""
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [[ "$session_created" == 1 ]]; then
    tmux kill-session -t "$session" >/dev/null 2>&1
  fi
  [[ -n "$workdir" ]] && rm -rf "$workdir"
  exit "$rc"
}
trap cleanup EXIT INT TERM

workdir="$(mktemp -d "/tmp/${session}.XXXXXX")"
workdir_basename="$(basename "$workdir")"
readonly workdir_basename
readonly received_file="${workdir}/received.txt"

# GET/POST wrapper with a caller-supplied per-request timeout (bounded
# overall by wall-clock deadlines in the wait_for_* helpers below) plus a
# fixed connect timeout, so a hung or unreachable Envoy cannot hang this
# script under `set -e`. Curl's own stdout is captured and discarded on any
# transport failure — printing both curl's own "no response" %{http_code}
# (which is itself "000") AND a fallback "000" would otherwise double up —
# so exactly one normalized code is ever printed: "000" for a transport
# failure (refused, timed out), otherwise the real HTTP status.
http_status() {
  local request_timeout="$1"
  shift
  local status
  if status="$(curl -sS -o /dev/null -w '%{http_code}' \
    --connect-timeout "$curl_connect_timeout" --max-time "$request_timeout" \
    "$@" 2>/dev/null)"; then
    printf '%s' "$status"
  else
    printf '000'
  fi
}

# True while the role text is still sitting unsubmitted inside the TUI's
# compose box (bounded by its top "╭──" and bottom "╰─" rule, which are also
# drawn — empty — around the busy-state spinner, so their mere presence does
# not mean "unsent").
compose_box_has_prompt() {
  local tail top bottom
  tail="$(tmux capture-pane -t "$session" -p -S -10 2>/dev/null || true)"
  top="$(grep -n '╭──' <<<"$tail" | tail -1 | cut -d: -f1)"
  bottom="$(grep -n '╰─' <<<"$tail" | tail -1 | cut -d: -f1)"
  [[ -n "$top" && -n "$bottom" && "$bottom" -gt "$top" ]] || return 1
  sed -n "${top},${bottom}p" <<<"$tail" | grep -qF "$role"
}

# Every wait_for_* helper below is a wall-clock deadline against bash's
# $SECONDS, not an iteration count: counting fixed-length iterations breaks
# the moment a single check can itself take variable time (a curl call
# bounded by --max-time can take up to that long), silently stretching, say,
# a nominal 60-attempt/1s-sleep loop to several times its intended budget
# under a blackholing endpoint. Each loop recomputes the remaining budget,
# caps any HTTP request's own --max-time to whatever remains, and sleeps at
# most the remaining time before the next check.

# The TUI takes a few seconds to render (MCP connections, recent-session
# scan). Sending keys before it is accepting input drops them silently, so
# this waits for its status line — which names the cwd — before anything is
# typed.
wait_for_tui_ready() {
  local start=$SECONDS remaining
  while true; do
    remaining=$((tui_ready_timeout - (SECONDS - start)))
    ((remaining > 0)) || return 1
    if tmux capture-pane -t "$session" -p 2>/dev/null | grep -qF "$workdir_basename"; then
      return 0
    fi
    remaining=$((tui_ready_timeout - (SECONDS - start)))
    ((remaining > 0)) && sleep "$((remaining < 1 ? remaining : 1))"
  done
}

send_prompt() {
  tmux send-keys -t "$session" "$1"
  sleep 0.5
  tmux send-keys -t "$session" Enter
}

# Polls the role endpoint until it is claimed. A `send-keys ... Enter`
# occasionally races the compose box's own render and is swallowed with no
# visible effect (reproduced live while building this script), so this
# resends Enter for as long as the unsent text is still visibly sitting
# there, instead of waiting out the whole timeout to discover that.
wait_for_role_claim() {
  local start=$SECONDS remaining request_timeout status
  while true; do
    remaining=$((role_claim_timeout - (SECONDS - start)))
    ((remaining > 0)) || return 1
    request_timeout=$((remaining < curl_max_time ? remaining : curl_max_time))
    status="$(http_status "$request_timeout" "${envoy_url}/v1/roles/${role}")"
    [[ "$status" == "200" ]] && return 0
    compose_box_has_prompt && tmux send-keys -t "$session" Enter
    remaining=$((role_claim_timeout - (SECONDS - start)))
    ((remaining > 0)) && sleep "$((remaining < 1 ? remaining : 1))"
  done
}

# The window here spans real Envoy delivery plus the time for a live model
# turn to notice the steered message and write the file, which is why it is
# generous relative to the role-claim and teardown budgets.
wait_for_delivery() {
  local start=$SECONDS remaining received
  while true; do
    remaining=$((delivery_timeout - (SECONDS - start)))
    ((remaining > 0)) || return 1
    if [[ -f "$received_file" ]]; then
      # Command substitution strips trailing newlines, which tolerates the
      # model adding one despite the prompt's explicit no-trailing-newline
      # instruction while still requiring an otherwise exact match.
      received="$(cat -- "$received_file")"
      [[ "$received" == "$message" ]] && return 0
    fi
    remaining=$((delivery_timeout - (SECONDS - start)))
    ((remaining > 0)) && sleep "$((remaining < 1 ? remaining : 1))"
  done
}

# Only 404 counts as "cleared": a transient 500 from a registry read hiccup,
# a transport failure (000), or a lingering 200 are all not evidence of
# teardown. The caller reports last_role_status on failure.
last_role_status=""
wait_for_role_clear() {
  local start=$SECONDS remaining request_timeout
  while true; do
    remaining=$((teardown_timeout - (SECONDS - start)))
    ((remaining > 0)) || return 1
    request_timeout=$((remaining < curl_max_time ? remaining : curl_max_time))
    last_role_status="$(http_status "$request_timeout" "${envoy_url}/v1/roles/${role}")"
    [[ "$last_role_status" == "404" ]] && return 0
    remaining=$((teardown_timeout - (SECONDS - start)))
    ((remaining > 0)) && sleep "$((remaining < 1 ? remaining : 1))"
  done
}

printf 'plugin version: %s (from %s)\n' "$plugin_version" "$plugin_pkg"
printf 'starting tmux session %s (omp TUI) in %s\n' "$session" "$workdir"
tmux new-session -d -s "$session" -x 220 -y 50 -c "$workdir" "$omp_bin"
session_created=1

wait_for_tui_ready || fail "omp TUI in session ${session} did not render within ${tui_ready_timeout}s"

prompt="Call the envoy_role_set tool with role=\"${role}\". Once it succeeds, do not end your turn: wait for an incoming message addressed to that role. When one arrives, run a bash command to write its exact text (no extra formatting, no trailing newline) to a file named received.txt in the current directory, then reply with the single word DONE."
send_prompt "$prompt"

printf 'waiting up to %ss for role %s to be claimed (GET %s/v1/roles/%s)\n' \
  "$role_claim_timeout" "$role" "$envoy_url" "$role"
claim_start=$SECONDS
if ! wait_for_role_claim; then
  fail "role ${role} was not claimed within ${role_claim_timeout}s; pane:
$(tmux capture-pane -t "$session" -p -S -60 2>/dev/null || true)"
fi
claim_elapsed=$((SECONDS - claim_start))
printf 'role claimed after %ss\n' "$claim_elapsed"

# The bare role token is a JetStream subject with no bound stream (500); the
# namespaced notifications.role.<role> topic is what the listener's role
# routing subscribes to.
publish_topic="notifications.role.${role}"
publish_status="$(http_status "$curl_max_time" -X POST -H 'Content-Type: application/json' \
  "${envoy_url}/v1/messages/publish" \
  -d "{\"topic\":\"${publish_topic}\",\"message\":\"${message}\"}")"
[[ "$publish_status" == "200" ]] || fail "publish to ${publish_topic} returned ${publish_status}, want 200"

printf 'waiting up to %ss for delivery into %s\n' "$delivery_timeout" "$received_file"
delivery_start=$SECONDS
if ! wait_for_delivery; then
  fail "message not written to received.txt within ${delivery_timeout}s; pane:
$(tmux capture-pane -t "$session" -p -S -60 2>/dev/null || true)"
fi
delivery_elapsed=$((SECONDS - delivery_start))
printf 'message delivered after %ss\n' "$delivery_elapsed"

# Teardown is part of the contract, not an afterthought: killing the session
# must actually release the role, not leave a stale holder until a heartbeat
# timeout papers over it. The session must genuinely be alive right before
# this kill (otherwise a prior, unrelated exit would let the script "pass" a
# teardown it never caused), and the kill itself must succeed.
printf 'killing tmux session %s and asserting teardown within %ss\n' "$session" "$teardown_timeout"
tmux has-session -t "$session" 2>/dev/null || fail "tmux session ${session} was already gone before the deliberate teardown kill"
tmux kill-session -t "$session" || fail "tmux kill-session -t ${session} failed"
session_created=0
tmux has-session -t "$session" 2>/dev/null && fail "tmux session ${session} is still present after kill-session"

teardown_start=$SECONDS
if ! wait_for_role_clear; then
  fail "role ${role} still resolved (last status ${last_role_status}) ${teardown_timeout}s after tmux kill-session"
fi
teardown_elapsed=$((SECONDS - teardown_start))

printf 'PASS: plugin %s; role %s claimed in %ss (limit %ss); message delivered in %ss (limit %ss); teardown cleared role in %ss (limit %ss) and tmux session is gone\n' \
  "$plugin_version" "$role" "$claim_elapsed" "$role_claim_timeout" "$delivery_elapsed" "$delivery_timeout" "$teardown_elapsed" "$teardown_timeout"
