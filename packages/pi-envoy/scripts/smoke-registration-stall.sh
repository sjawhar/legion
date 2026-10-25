#!/usr/bin/env bash
# smoke-registration-stall.sh — manual live smoke for the registration-stall fix (INFRA-246).
#
# No unit test can show a real pane's listener record surviving the listener's own 300 s expiry,
# so this drives a real headless `omp` pane against a real Envoy listener and watches the live
# registry. Two arms, each with a negative control that must fail the way the bug failed:
#
#   --arm stall    a pane whose registration post is wedged forever is detected, bounded, keeps
#                  refreshing past one record lifetime, and says so exactly once.
#   --arm recover  a pane unregistered while its process runs re-registers itself, with no human
#                  and no write from this script beyond the one delete.
#
# The two numbers every budget below is measured in — the pane's refresh interval and the
# listener's record lifetime — are read at run time out of the files that define them
# (packages/envoy-client/src/envoy-settings.ts and packages/envoy/internal/session/registry.go,
# the same two .github/scripts/check-heartbeat-margin.sh parses), never copied here: a copy would
# go stale the moment either moves and this smoke would measure against a budget the tree no
# longer has.
#
# Manual only, like scripts/smoke-delivery.sh and scripts/smoke-btw.sh: it needs a live listener
# and a configured model provider, and is never wired into CI. See scripts/README.md.
set -euo pipefail

readonly wedge_at=3
readonly poll_s=15
readonly fast_poll_s=5
readonly curl_connect_timeout=5
readonly curl_max_time=20

refuse() {
  printf 'smoke-registration-stall: %s\n' "$1" >&2
  exit 2
}

note() {
  printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$1"
}

# The operator supplies the listener: this repository is public and names no deployment.
envoy_url="${ENVOY_URL:-}"
[[ -n "$envoy_url" ]] ||
  refuse "ENVOY_URL is required: set it to the base URL of the live Envoy listener to smoke"
token_file="${ENVOY_TOKEN_FILE:-}"
[[ -n "$token_file" ]] ||
  refuse "ENVOY_TOKEN_FILE is required: set it to a readable file holding the listener bearer token"
[[ -r "$token_file" ]] ||
  refuse "ENVOY_TOKEN_FILE (${token_file}) is not a readable file"
omp_bin="${OMP_BIN:-/opt/omp/bin/omp}"
[[ -x "$omp_bin" ]] ||
  refuse "OMP_BIN (${omp_bin}) is not an executable omp binary"
readonly envoy_url token_file omp_bin

for required in curl jq; do
  command -v "$required" >/dev/null 2>&1 || refuse "${required} is required"
done

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "${script_dir}/../../.." && pwd)"
readonly repo_root

# The refresh interval and the record lifetime, out of the tree rather than out of this file.
settings_file="${repo_root}/packages/envoy-client/src/envoy-settings.ts"
registry_file="${repo_root}/packages/envoy/internal/session/registry.go"
readonly settings_file registry_file
[[ -r "$settings_file" ]] ||
  refuse "${settings_file} is not readable: this smoke reads DEFAULT_HEARTBEAT_MS from it"
[[ -r "$registry_file" ]] ||
  refuse "${registry_file} is not readable: this smoke reads OpenSessionRegistry's default ttl from it"
default_heartbeat_ms="$(sed -nE 's/^export const DEFAULT_HEARTBEAT_MS = ([0-9_]+);.*$/\1/p' \
  "$settings_file" | tr -d _)"
[[ "$default_heartbeat_ms" =~ ^[0-9]+$ ]] ||
  refuse "${settings_file} declares no single plain-decimal DEFAULT_HEARTBEAT_MS this smoke can read"
readonly default_heartbeat_ms
record_ttl="$(sed -nE 's/^[[:space:]]*opts := sessionRegistryOpts\{.*ttl: ([0-9_]+) \* time\.(Second|Minute).*$/\1 \2/p' \
  "$registry_file" | tr -d _)"
[[ "$record_ttl" =~ ^[0-9]+\ (Second|Minute)$ ]] ||
  refuse "${registry_file} declares no single OpenSessionRegistry default ttl in <n> * time.Second|Minute form this smoke can read"
if [[ "$record_ttl" == *Minute ]]; then
  record_lifetime_s=$((${record_ttl% *} * 60))
else
  record_lifetime_s="${record_ttl% *}"
fi
readonly record_ttl record_lifetime_s

arm=""
control=0
plugin=""
heartbeat_ms=""
base_rev=""
while (($# > 0)); do
  case "$1" in
    --arm)
      arm="${2:-}"
      shift 2 || refuse "--arm needs stall or recover"
      ;;
    --control)
      control=1
      shift
      ;;
    --plugin)
      plugin="${2:-}"
      shift 2 || refuse "--plugin needs a path"
      ;;
    --heartbeat-ms)
      heartbeat_ms="${2:-}"
      shift 2 || refuse "--heartbeat-ms needs a number"
      ;;
    --base-rev)
      base_rev="${2:-}"
      shift 2 || refuse "--base-rev needs a revision"
      ;;
    *)
      refuse "unknown argument: $1"
      ;;
  esac
done

[[ "$arm" == "stall" || "$arm" == "recover" ]] ||
  refuse "--arm stall|recover is required"
[[ -z "$heartbeat_ms" || "$heartbeat_ms" =~ ^[0-9]+$ ]] ||
  refuse "--heartbeat-ms must be a whole number of milliseconds"
if [[ "$arm" == "stall" && "$control" == 1 ]]; then
  [[ -n "$base_rev" ]] ||
    refuse "--arm stall --control needs --base-rev <rev>: the control is the plugin as it was then"
  [[ -z "$plugin" ]] ||
    refuse "--plugin and --base-rev are exclusive: the stall control runs the plugin at --base-rev"
  command -v jj >/dev/null 2>&1 || refuse "jj is required to materialise --base-rev"
else
  [[ -z "$base_rev" ]] || refuse "--base-rev applies only to --arm stall --control"
fi
[[ -n "$plugin" ]] || plugin="${repo_root}/packages/pi-envoy/extensions/envoy.ts"
readonly arm control heartbeat_ms base_rev

# What the pane's refresh interval will be: DEFAULT_HEARTBEAT_MS as the tree declares it, unless
# the operator shortened it, which is what every "one refresh interval" budget below is measured
# in.
refresh_ms="${heartbeat_ms:-$default_heartbeat_ms}"
readonly refresh_ms
readonly refresh_s=$(((refresh_ms + 999) / 1000))
note "read from the tree: refresh interval ${default_heartbeat_ms} ms (DEFAULT_HEARTBEAT_MS, ${settings_file}), record lifetime ${record_lifetime_s}s (${record_ttl}, OpenSessionRegistry's default ttl, ${registry_file}); this run refreshes every ${refresh_s}s"

arm_label="$arm"
((control == 0)) || arm_label="${arm}, control"
readonly arm_label

listener_gets=0
listener_deletes=0

verdict_pass() {
  printf 'SMOKE %s PASS: %s\n' "$arm_label" "$1"
  exit 0
}

verdict_fail() {
  printf 'SMOKE %s FAIL: %s\n' "$arm_label" "$1"
  exit 1
}

scratch=""
base_plugin=""
pane_pid=""
pane_fd=""
cleanup() {
  local status=$?
  if [[ -n "$pane_pid" ]]; then
    kill -KILL "$pane_pid" 2>/dev/null || true
    wait "$pane_pid" 2>/dev/null || true
  fi
  [[ -z "$pane_fd" ]] || exec {pane_fd}>&- || true
  [[ -z "$base_plugin" ]] || rm -f "$base_plugin"
  [[ -z "$scratch" ]] || rm -rf "$scratch"
  return "$status"
}
trap cleanup EXIT INT TERM

scratch="$(mktemp -d "/tmp/smoke-registration-stall-${arm}.XXXXXX")"
readonly fixture_log="${scratch}/fixture.log"
readonly pane_log="${scratch}/pane.log"
readonly sessions_json="${scratch}/sessions.json"
readonly header_file="${scratch}/auth-header"
: >"$fixture_log"
# The bearer goes through a 0600 file, never argv, which /proc exposes to every local user.
(
  umask 077
  printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" >"$header_file"
)

if [[ "$arm" == "stall" && "$control" == 1 ]]; then
  # The base plugin only resolves from the directory it shipped in: it imports `../src/...` and
  # `./envoy-message-renderer`. The dot keeps it out of every glob, the tsconfig include list and
  # biome's walk; the trap removes it on every exit path.
  base_plugin="${repo_root}/packages/pi-envoy/extensions/.smoke-base-$$.ts"
  jj -R "$repo_root" file show -r "$base_rev" packages/pi-envoy/extensions/envoy.ts >"$base_plugin" ||
    refuse "jj file show -r ${base_rev} packages/pi-envoy/extensions/envoy.ts failed"
  [[ -s "$base_plugin" ]] || refuse "the plugin at ${base_rev} materialised empty"
  plugin="$base_plugin"
fi
readonly plugin
[[ -r "$plugin" ]] || refuse "plugin (${plugin}) is not readable"

# Every listener call this script makes goes through these two, which keep their result in
# `http_code` rather than on stdout: a command substitution would run them in a subshell and lose
# the counters, and the recover arm's claim that nothing but one delete wrote is an assertion
# about this script's own traffic, not a hope.
http_code=""
list_sessions() {
  listener_gets=$((listener_gets + 1))
  http_code="$(curl -sS -o "$sessions_json" -w '%{http_code}' \
    --connect-timeout "$curl_connect_timeout" --max-time "$curl_max_time" \
    -H "@${header_file}" "${envoy_url}/v1/sessions" 2>/dev/null)" || http_code="000"
}

delete_session() {
  listener_deletes=$((listener_deletes + 1))
  http_code="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE \
    --connect-timeout "$curl_connect_timeout" --max-time "$curl_max_time" \
    -H "@${header_file}" "${envoy_url}/v1/sessions/$1" 2>/dev/null)" || http_code="000"
}

# This run's own row among the listener's hundreds of live ones, left in `row_json` (empty when it
# is not listed): `dir` is the scratch directory the pane was launched with, which no other
# session can be running in.
row_json=""
own_row() {
  row_json=""
  list_sessions
  if [[ "$http_code" != "200" ]]; then
    note "GET /v1/sessions returned ${http_code}"
    return 0
  fi
  row_json="$(jq -c --arg dir "$scratch" 'map(select(.dir == $dir)) | first // empty' "$sessions_json")"
}

last_registration() {
  awk '$1 == "registration" && NF == 2 { n = $2 } END { print n + 0 }' "$fixture_log"
}

stall_lines() {
  grep -c 'reconcile pass stalled' "$fixture_log" || true
}

pane_alive() {
  [[ -n "$pane_pid" ]] && kill -0 "$pane_pid" 2>/dev/null
}

pane_tail() {
  tail -n 5 "$pane_log" 2>/dev/null | tr '\n' ' '
}

# One real headless pane, loading the fixture ahead of the plugin so the fixture's `fetch` wrapper
# is the one the plugin captures. Its stdin is a fifo this script holds open rather than a `sleep`
# pipeline, so the pane is one process this script can signal exactly: the recover control needs a
# SIGKILL that reaches `omp` itself, since a pane that exits cleanly deletes its own record and
# there would then be nothing for the control to delete.
start_pane() {
  local wedge="$1"
  local fifo="${scratch}/pane.stdin"
  mkfifo "$fifo"
  exec {pane_fd}<>"$fifo"
  printf '{"type":"prompt","id":"p1","message":"Reply with the single word ok."}\n' >&"$pane_fd"
  local -a pane_env=(
    "ENVOY_URL=${envoy_url}"
    "ENVOY_TOKEN_FILE=${token_file}"
    "STALL_FIXTURE_LOG=${fixture_log}"
    "STALL_FIXTURE_WEDGE_AT=${wedge}"
  )
  [[ -z "$heartbeat_ms" ]] || pane_env+=("ENVOY_HEARTBEAT_MS=${heartbeat_ms}")
  env "${pane_env[@]}" "$omp_bin" --mode rpc --no-ui \
    --cwd "$scratch" --session-dir "$scratch" --no-skills --no-rules --no-extensions --no-tools \
    -e "${script_dir}/stall-fixture.ts" -e "$plugin" \
    <"$fifo" >"$pane_log" 2>&1 &
  pane_pid=$!
  note "pane started (pid ${pane_pid}, wedge ${wedge}, refresh ${refresh_s}s) in ${scratch}"
}

# Bounded wait for this run's row to appear in `row_json`.
wait_for_row() {
  local deadline=$((SECONDS + $1))
  while ((SECONDS < deadline)); do
    own_row
    [[ -z "$row_json" ]] || return 0
    pane_alive || {
      note "pane exited early: $(pane_tail)"
      return 1
    }
    sleep "$fast_poll_s"
  done
  return 1
}

row_field() {
  jq -r --arg field "$2" '.[$field]' <<<"$1"
}

age_s() {
  printf '%s' "$((($(date +%s%3N) - $1) / 1000))"
}

run_stall() {
  start_pane "$wedge_at"
  wait_for_row 120 ||
    verdict_fail "the pane never registered under ${scratch} within 120s: $(pane_tail)"
  local row="$row_json"
  local session_id
  session_id="$(row_field "$row" session_id)"
  note "registered as ${session_id}"

  note "waiting for the fixture to wedge registration ${wedge_at} (every ${refresh_s}s)"
  local wedge_deadline=$((SECONDS + 8 * refresh_s + 180))
  until grep -q "^registration ${wedge_at} wedged\$" "$fixture_log"; do
    ((SECONDS < wedge_deadline)) ||
      verdict_fail "registration ${wedge_at} was never reached; the log holds $(last_registration) registrations"
    pane_alive || verdict_fail "the pane exited before registration ${wedge_at}: $(pane_tail)"
    sleep 2
  done
  local wedged_at
  wedged_at="$(date +%s)"
  own_row
  row="$row_json"
  [[ -n "$row" ]] || verdict_fail "the session was already unlisted when registration ${wedge_at} wedged"
  local wedged_last_seen
  wedged_last_seen="$(row_field "$row" last_seen)"
  note "registration ${wedge_at} is wedged; last_seen at the wedge is ${wedged_last_seen}"

  # One record lifetime past the wedged post plus a margin: before that, a listener record that is
  # merely not yet expired proves nothing either way.
  local margin=60
  ((control == 0)) || margin=90
  local target=$((wedged_at + record_lifetime_s + margin))
  local gone_at=0
  note "watching until $(date -u -d "@${target}" +%H:%M:%SZ) (${record_lifetime_s}s record lifetime + ${margin}s)"
  while (($(date +%s) < target)); do
    sleep "$poll_s"
    own_row
    row="$row_json"
    local regs stalls
    regs="$(last_registration)"
    stalls="$(stall_lines)"
    if [[ -z "$row" ]]; then
      note "not listed; registrations=${regs} stall lines=${stalls} ($(( $(date +%s) - wedged_at ))s past the wedge)"
      ((control == 1)) ||
        verdict_fail "the session left the registry $(($(date +%s) - wedged_at))s after the wedged post; it must stay listed"
      ((gone_at != 0)) || gone_at="$(date +%s)"
      continue
    fi
    ((gone_at == 0)) ||
      verdict_fail "the control's session returned to the registry after disappearing; only the fixed plugin refreshes itself"
    local seen
    seen="$(row_field "$row" last_seen)"
    note "listed; last_seen=${seen} (age $(age_s "$seen")s) registrations=${regs} stall lines=${stalls}"
    pane_alive || verdict_fail "the pane exited during the watch: $(pane_tail)"
  done

  own_row
  row="$row_json"
  local regs stalls last_seen
  regs="$(last_registration)"
  stalls="$(stall_lines)"
  if ((control == 1)); then
    [[ -z "$row" ]] ||
      verdict_fail "the base-rev session is still listed $(($(date +%s) - wedged_at))s after the wedged post (last_seen $(row_field "$row" last_seen)); the control must lapse"
    ((regs <= wedge_at - 1)) ||
      verdict_fail "the base-rev pane posted registration ${regs} after the wedged ${wedge_at}; its wedge did not hold"
    verdict_pass "the plugin at ${base_rev} left ${session_id} wedged at registration ${wedge_at}, posted nothing after it (last accepted ${regs}), and the record was gone $((gone_at - wedged_at))s after the wedged post and never returned (watched to $(($(date +%s) - wedged_at))s)"
  fi

  [[ -n "$row" ]] ||
    verdict_fail "the session is not listed $(($(date +%s) - wedged_at))s after the wedged post; expected it to outlive the ${record_lifetime_s}s record lifetime"
  last_seen="$(row_field "$row" last_seen)"
  ((last_seen > wedged_last_seen)) ||
    verdict_fail "last_seen did not advance past the wedged post: ${wedged_last_seen} at the wedge, ${last_seen} now"
  ((regs > wedge_at)) ||
    verdict_fail "no registration past the wedged ${wedge_at} was posted (last accepted ${regs}); the refresh did not resume"
  ((stalls == 1)) ||
    verdict_fail "expected exactly one stall line in the pane's log, saw ${stalls}"
  verdict_pass "${session_id} stayed listed $(($(date +%s) - wedged_at))s after registration ${wedge_at} wedged — past the ${record_lifetime_s}s record lifetime — last_seen advanced ${wedged_last_seen} → ${last_seen}, the fixture logged ${regs} accepted registrations (past the wedged ${wedge_at}, so the refresh kept going), and the pane said the stall exactly once"
}

run_recover() {
  start_pane 0
  wait_for_row 120 ||
    verdict_fail "the pane never registered under ${scratch} within 120s: $(pane_tail)"
  local row="$row_json"
  local session_id before_seen regs_before
  session_id="$(row_field "$row" session_id)"
  before_seen="$(row_field "$row" last_seen)"
  regs_before="$(last_registration)"
  note "registered as ${session_id} (last_seen ${before_seen}, ${regs_before} registrations posted)"

  if ((control == 1)); then
    # A pane that exits cleanly deletes its own record, so the control's pane is SIGKILLed: its
    # row outlives it, which is the row the delete then removes.
    note "SIGKILLing the pane so its record outlives it"
    kill -KILL "$pane_pid" 2>/dev/null || verdict_fail "could not SIGKILL the pane (pid ${pane_pid})"
    wait "$pane_pid" 2>/dev/null || true
    if pane_alive; then verdict_fail "the pane is still running after SIGKILL"; fi
    pane_pid=""
    own_row
    row="$row_json"
    [[ -n "$row" ]] ||
      verdict_fail "the killed pane's record was already gone before the delete; nothing to control"
    note "the dead pane's record is still listed"
  fi

  delete_session "$session_id"
  [[ "$http_code" == "200" ]] ||
    verdict_fail "DELETE /v1/sessions/${session_id} returned ${http_code}, want 200"
  local deleted_at
  deleted_at="$(date +%s)"
  note "deleted ${session_id}; confirming it is unlisted"
  local gone=0 confirm_deadline=$((SECONDS + 30))
  while ((SECONDS < confirm_deadline)); do
    own_row
    row="$row_json"
    if [[ -z "$row" ]]; then
      gone=1
      break
    fi
    sleep 2
  done
  ((gone == 1)) ||
    verdict_fail "${session_id} was still listed 30s after the delete; the delete did not take"
  note "unlisted $(($(date +%s) - deleted_at))s after the delete"

  # One refresh interval is what the pane's own heartbeat needs; the margin covers the round trip.
  local budget=$((refresh_s + 45))
  local deadline=$((SECONDS + budget))
  if ((control == 1)); then
    note "watching ${budget}s: a record with no process behind it must stay gone"
    while ((SECONDS < deadline)); do
      sleep "$fast_poll_s"
      own_row
      row="$row_json"
      [[ -z "$row" ]] ||
        verdict_fail "${session_id} came back $(($(date +%s) - deleted_at))s after the delete although its process was already dead; the return in the main arm would not be the pane's own refresh"
      note "still gone ($(($(date +%s) - deleted_at))s)"
    done
    verdict_pass "${session_id}'s process was SIGKILLed, its record deleted, and it stayed gone for ${budget}s (one ${refresh_s}s refresh interval and a margin) — so a return in the main arm is the live pane's own refresh, not the listener restoring anything; this run made ${listener_gets} list polls and ${listener_deletes} delete, and no other request"
  fi

  note "watching up to ${budget}s for the live pane to re-register itself"
  while ((SECONDS < deadline)); do
    sleep "$fast_poll_s"
    own_row
    row="$row_json"
    if [[ -n "$row" ]]; then
      local back_id back_seen
      back_id="$(row_field "$row" session_id)"
      back_seen="$(row_field "$row" last_seen)"
      [[ "$back_id" == "$session_id" ]] ||
        verdict_fail "a different session (${back_id}) now holds ${scratch}, not ${session_id}"
      ((back_seen > before_seen)) ||
        verdict_fail "the returned row's last_seen ${back_seen} did not advance past ${before_seen}"
      local regs_after
      regs_after="$(last_registration)"
      ((regs_after > regs_before)) ||
        verdict_fail "the fixture logged no registration after the delete (${regs_before} before, ${regs_after} after); the row did not come back from the pane's own post"
      note "back after $(($(date +%s) - deleted_at))s with last_seen=${back_seen}"
      verdict_pass "${session_id} was deleted from the registry while its process ran and re-registered itself $(($(date +%s) - deleted_at))s later (budget ${budget}s: one ${refresh_s}s refresh interval and a margin), last_seen ${before_seen} → ${back_seen} and the pane's own registration count ${regs_before} → ${regs_after}, with no human and no envoy_subscribe: the only requests this run made to the listener are ${listener_gets} GET /v1/sessions polls and the single DELETE /v1/sessions/${session_id}"
    fi
    pane_alive || verdict_fail "the pane exited before re-registering: $(pane_tail)"
    note "not back yet ($(($(date +%s) - deleted_at))s)"
  done
  verdict_fail "${session_id} did not re-register within ${budget}s of the delete (one ${refresh_s}s refresh interval and a margin); the pane is still running: $(pane_tail)"
}

note "arm ${arm_label}: listener ${envoy_url}, omp ${omp_bin}, plugin ${plugin}"
if [[ "$arm" == "stall" ]]; then
  run_stall
else
  run_recover
fi
