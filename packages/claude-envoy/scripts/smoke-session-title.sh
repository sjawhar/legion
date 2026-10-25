#!/usr/bin/env bash
# smoke-session-title.sh — the live proof that the name a person gave a Claude Code session is
# what this plugin registers with a real Envoy listener, that a rename replaces it, and that a
# session nobody named keeps the folder-and-short-id fallback (dispatch://INFRA-92).
#
# It runs the committed bundle, not the TypeScript: a real nats-server, a real Envoy listener
# built from this tree, `dist/session-hook.js` for each payload and `dist/envoy-channel.js` as
# the session, read back through the listener's own `GET /v1/sessions`. Its negative control is
# the same first case against the base revision's `dist/envoy-channel.js`, which registers the
# folder name and not the chosen one.
#
# WHAT IS SIMULATED. Claude Code is not installed in a Legion issue pod and cannot be, so the two
# hook payloads are JSON this script writes, shaped as Claude Code's hooks reference documents
# them (https://code.claude.com/docs/en/hooks: a SessionStart hook receives `session_title`, "the
# session's custom title, present when one is set, for example with `--name`, `/rename` …", and
# "UserPromptSubmit hooks also receive `session_title` …"). The run proves that the real hook
# bundle records what Claude Code would hand it and that the real channel server registers it
# with a real listener. It does not prove that Claude Code sends the field; that rests on Claude
# Code's own documentation.
#
# Like smoke-channel.sh and smoke-clear-rebind.sh, this is a manual smoke and never a CI step.
#
#   bash packages/claude-envoy/scripts/smoke-session-title.sh
#
# BASE_REV names the revision the negative control's bundle comes from (default `main@origin`).
set -euo pipefail

readonly base_rev="${BASE_REV:-main@origin}"
readonly heartbeat_ms="${ENVOY_HEARTBEAT_MS:-1000}"
readonly chosen_name="eeny meeny"
readonly renamed="miny moe"
readonly register_timeout=30

package_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly package_dir
repo_root="$(cd "${package_dir}/../.." && pwd)"
readonly repo_root

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  [[ -s "${workdir:-}/server.log" ]] && { printf -- '--- channel server log ---\n'; cat "${workdir}/server.log"; } >&2
  exit 1
}

# nats-server is a `go install` away and lands in GOPATH's bin, which is often off PATH.
if ! command -v nats-server >/dev/null 2>&1; then
  PATH="$(go env GOPATH 2>/dev/null)/bin:${PATH}"
  export PATH
fi
for tool in bun curl jq go nats-server; do
  command -v "$tool" >/dev/null 2>&1 ||
    fail "$tool is required (nats-server: go install github.com/nats-io/nats-server/v2@v2.11.4)"
done

workdir="$(mktemp -d /tmp/claude-session-title.XXXXXX)"
readonly workdir
nats_pid=""
listener_pid=""
server_pid=""
fifo_pid=""

cleanup() {
  [[ -n "$server_pid" ]] && kill "$server_pid" 2>/dev/null
  [[ -n "$fifo_pid" ]] && kill "$fifo_pid" 2>/dev/null
  [[ -n "$listener_pid" ]] && kill "$listener_pid" 2>/dev/null
  [[ -n "$nats_pid" ]] && kill "$nats_pid" 2>/dev/null
  wait 2>/dev/null || true
  rm -rf "$workdir"
  return 0
}
trap cleanup EXIT INT TERM

free_port() {
  bun -e 'const s = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } }); console.log(s.port); s.stop(true)'
}

readonly nats_port="$(free_port)"
readonly listener_port="$(free_port)"
# Dispatch is enabled against a port nothing listens on: the session stamps its title for the
# `dispatch` command without this smoke needing a Dispatch server.
readonly dispatch_port="$(free_port)"
readonly envoy_url="http://127.0.0.1:${listener_port}"

printf 'starting nats-server on %s and an Envoy listener built from this tree on %s\n' \
  "$nats_port" "$listener_port"
nats-server -js -a 127.0.0.1 -p "$nats_port" -sd "${workdir}/nats" >"${workdir}/nats.log" 2>&1 &
nats_pid=$!

(cd "${repo_root}/packages/envoy" && go build -o "${workdir}/envoy-listener" ./cmd/listener) ||
  fail "the Envoy listener did not build"
env PORT="$listener_port" ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID=session-title-smoke \
  NATS_URLS="nats://127.0.0.1:${nats_port}" \
  "${workdir}/envoy-listener" >"${workdir}/listener.log" 2>&1 &
listener_pid=$!
for _ in $(seq 60); do
  [[ "$(curl -fsS --max-time 2 "${envoy_url}/healthz" 2>/dev/null)" == *healthy* ]] && break
  sleep 0.5
done
[[ "$(curl -fsS --max-time 2 "${envoy_url}/healthz" 2>/dev/null)" == *healthy* ]] ||
  fail "the listener was not healthy within 30s: $(cat "${workdir}/listener.log")"

# The session's own scratch world: the envoy.json Claude Code's rule picks
# (`$CLAUDE_CONFIG_DIR/envoy.json`), the plugin state directory the hook records titles in, the
# `dispatch` command's state directory the registration stamps, and a project directory whose
# name is the fallback title's first half.
readonly config_dir="${workdir}/claude-config"
readonly plugin_data="${workdir}/plugin-data"
readonly dispatch_state="${workdir}/dispatch-state"
readonly project_dir="${workdir}/probe-project"
mkdir -p "$config_dir" "$plugin_data" "$dispatch_state" "$project_dir"
jq -n --arg envoy "$envoy_url" --arg nats "nats://127.0.0.1:${nats_port}" \
  --arg dispatch "http://127.0.0.1:${dispatch_port}" \
  '{dispatch: {enabled: true, serverUrl: $dispatch, token: "session-title-smoke"},
    envoy: {enabled: true, url: $envoy, natsUrls: [$nats]}}' >"${config_dir}/envoy.json"

# The channel server runs as Claude Code runs it: no ENVOY_SESSION_ID, because that QA override
# also turns the `dispatch` command's title stamp off, and this smoke asserts the stamp.
run_channel_server() {
  local bundle="$1" sid="$2"
  mkfifo "${workdir}/stdin"
  sleep 600 >"${workdir}/stdin" &
  fifo_pid=$!
  env -i PATH="$PATH" HOME="$workdir" \
    CLAUDE_CONFIG_DIR="$config_dir" CLAUDE_PLUGIN_DATA="$plugin_data" \
    CLAUDE_PROJECT_DIR="$project_dir" CLAUDE_CODE_SESSION_ID="$sid" \
    DISPATCH_STATE_DIR="$dispatch_state" ENVOY_HEARTBEAT_MS="$heartbeat_ms" \
    bun "$bundle" <"${workdir}/stdin" >"${workdir}/server.log" 2>&1 &
  server_pid=$!
}

stop_channel_server() {
  [[ -n "$server_pid" ]] && kill "$server_pid" 2>/dev/null
  [[ -n "$fifo_pid" ]] && kill "$fifo_pid" 2>/dev/null
  # A killed job's status is its signal, which `set -e` would otherwise take for a failure.
  wait "$server_pid" "$fifo_pid" 2>/dev/null || true
  server_pid=""
  fifo_pid=""
  rm -f "${workdir}/stdin"
  return 0
}

# The title the listener holds for the session `$1`, or empty when it holds no such session.
registered_title() {
  curl -fsS --max-time 5 "${envoy_url}/v1/sessions" 2>/dev/null |
    jq -r --arg id "$1" '[.. | objects | select(.session_id? == $id)][0].title // empty'
}

# Waits up to `register_timeout` for the listener's title for the session `$1` to equal `$2`.
await_title() {
  local sid="$1" expected="$2" seen=""
  for _ in $(seq $((register_timeout * 2))); do
    seen="$(registered_title "$sid")"
    [[ "$seen" == "$expected" ]] && return 0
    sleep 0.5
  done
  printf 'the listener holds %q for %s, not %q\n' "$seen" "$sid" "$expected" >&2
  return 1
}

# One hook run, with the environment Claude Code gives a hook and `payload` on its stdin.
run_hook() {
  local mode="$1" payload="$2"
  printf '%s' "$payload" | env -i PATH="$PATH" HOME="$workdir" \
    CLAUDE_CONFIG_DIR="$config_dir" CLAUDE_PLUGIN_DATA="$plugin_data" \
    CLAUDE_PROJECT_DIR="$project_dir" \
    bun "${package_dir}/dist/session-hook.js" "$mode" >"${workdir}/hook-${mode}.out" 2>&1 ||
    fail "the ${mode} hook exited non-zero: $(cat "${workdir}/hook-${mode}.out")"
}

readonly session_id="smoke-title-0001"
readonly short_id="${session_id:0:8}"
readonly fallback="probe-project ${short_id}"

printf '\ncase 1: a SessionStart payload carrying the name a person gave the session\n'
run_hook open-asks "$(jq -nc --arg id "$session_id" --arg dir "$project_dir" --arg title "$chosen_name" \
  '{session_id: $id, cwd: $dir, hook_event_name: "SessionStart", source: "startup", session_title: $title}')"
run_channel_server "${package_dir}/dist/envoy-channel.js" "$session_id"
await_title "$session_id" "$chosen_name" || fail "the chosen name did not reach the listener"
printf 'the listener holds %q for %s\n' "$chosen_name" "$session_id"

printf '\ncase 2: the session is renamed, and the next registration carries the new name\n'
run_hook session-title "$(jq -nc --arg id "$session_id" --arg title "$renamed" \
  '{session_id: $id, hook_event_name: "UserPromptSubmit", session_title: $title}')"
await_title "$session_id" "$renamed" || fail "the rename did not reach the listener"
printf 'the listener holds %q for %s\n' "$renamed" "$session_id"

printf '\ncase 3: the name is taken away, and the folder-and-short-id fallback returns\n'
run_hook session-title "$(jq -nc --arg id "$session_id" \
  '{session_id: $id, hook_event_name: "UserPromptSubmit"}')"
await_title "$session_id" "$fallback" || fail "the fallback did not return"
printf 'the listener holds %q for %s\n' "$fallback" "$session_id"

printf '\nthe dispatch command carries the same name on what this session writes\n'
run_hook session-title "$(jq -nc --arg id "$session_id" --arg title "$renamed" \
  '{session_id: $id, hook_event_name: "UserPromptSubmit", session_title: $title}')"
await_title "$session_id" "$renamed" || fail "the rename did not reach the listener a second time"
readonly stamp="${dispatch_state}/sessions/${session_id}/title"
[[ -f "$stamp" ]] || fail "no title was stamped for the dispatch command at ${stamp}"
[[ "$(cat "$stamp")" == "$renamed" ]] ||
  fail "the dispatch command's title is $(cat "$stamp"), not ${renamed}"
printf '%s holds %q\n' "$stamp" "$renamed"
stop_channel_server

printf '\nnegative control: the same recorded name against %s dist/envoy-channel.js\n' "$base_rev"
readonly control_id="smoke-title-0002"
readonly control_fallback="probe-project ${control_id:0:8}"
run_hook open-asks "$(jq -nc --arg id "$control_id" --arg dir "$project_dir" --arg title "$chosen_name" \
  '{session_id: $id, cwd: $dir, hook_event_name: "SessionStart", source: "startup", session_title: $title}')"
jj -R "$repo_root" file show -r "$base_rev" packages/claude-envoy/dist/envoy-channel.js \
  >"${workdir}/base-envoy-channel.js" ||
  fail "could not read packages/claude-envoy/dist/envoy-channel.js at ${base_rev}"
run_channel_server "${workdir}/base-envoy-channel.js" "$control_id"
await_title "$control_id" "$control_fallback" ||
  fail "the base bundle did not register the fallback it is supposed to"
printf 'the base bundle registered %q and not %q\n' "$control_fallback" "$chosen_name"
stop_channel_server

printf '\nPASS: the chosen name registers, a rename replaces it, an unnamed session keeps the fallback, the dispatch command carries the same name, and the base bundle does none of it\n'
