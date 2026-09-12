#!/usr/bin/env bash
# Drives the model-gated OMP acceptance scenarios against a running native Dispatch stack.
set -euo pipefail

readonly mode="${1:-E4}"
readonly role="acceptance-r"
readonly answer_option="Yes"

usage() {
  cat >&2 <<'USAGE'
Usage: OMP_BIN=/path/to/omp DISPATCH_URL=http://127.0.0.1:8773 \
  DISPATCH_TOKEN=token ISSUE_KEY=CORE-1 omp-roundtrip.sh E4|E5

E4 asks "Ship it?", answers it as X-Dispatch-User: sjawhar, and requires the
answer steer in the OMP transcript within five seconds.
E5 sets ISSUE_KEY's route to role:acceptance-r, claims that role from OMP,
posts a human message, and requires that delivery in the transcript within
five seconds.

Optional: ENVOY_URL (default http://127.0.0.1:9020) and ENVOY_NATS_URL
(default nats://envoy-nats:4222).
USAGE
}

require() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    printf 'required environment variable %s is unset\n' "$name" >&2
    usage
    exit 2
  fi
}

for variable in OMP_BIN DISPATCH_URL DISPATCH_TOKEN ISSUE_KEY; do
  require "$variable"
done

case "$mode" in
  E4 | E5) ;;
  *)
    printf 'unknown scenario %q\n' "$mode" >&2
    usage
    exit 2
    ;;
esac

for command in bun curl jq tmux; do
  command -v "$command" >/dev/null || {
    printf 'required command %s is unavailable\n' "$command" >&2
    exit 2
  }
done
[[ -x "$OMP_BIN" ]] || {
  printf 'OMP_BIN is not executable: %s\n' "$OMP_BIN" >&2
  exit 2
}

readonly script_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
readonly repo_root="$(cd -- "$script_dir/../../../.." && pwd)"
readonly pi_envoy_dir="$repo_root/packages/pi-envoy"
readonly extension="$pi_envoy_dir/dist/envoy.js"
readonly session_dir="$(mktemp -d "/tmp/dispatch-omp-${mode,,}-sessions.XXXXXX")"
readonly work_dir="$(mktemp -d "/tmp/dispatch-omp-${mode,,}-work.XXXXXX")"
readonly tmux_session="dispatch-i1-${mode,,}-$$"

export DISPATCH_URL DISPATCH_TOKEN
export ENVOY_URL="${ENVOY_URL:-http://127.0.0.1:9020}"
export ENVOY_NATS_URL="${ENVOY_NATS_URL:-nats://envoy-nats:4222}"

cleanup() {
  if tmux has-session -t "$tmux_session" 2>/dev/null; then
    tmux kill-session -t "$tmux_session"
  fi
}
trap cleanup EXIT

show_diagnostics() {
  printf '\nOMP transcript directory: %s\nOMP work directory: %s\n' "$session_dir" "$work_dir" >&2
  printf 'tmux pane (%s):\n' "$tmux_session" >&2
  tmux capture-pane -pt "$tmux_session" -S -100 2>/dev/null >&2 || true
}

session_files() {
  shopt -s nullglob globstar
  printf '%s\n' "$session_dir"/**/*.jsonl
}

await_session_file() {
  local deadline=$((SECONDS + 10))
  local file
  while ((SECONDS < deadline)); do
    while IFS= read -r file; do
      [[ -f "$file" ]] && {
        printf '%s\n' "$file"
        return 0
      }
    done < <(session_files)
    sleep 0.1
  done
  printf 'OMP did not create a persisted .jsonl session within 10 seconds\n' >&2
  show_diagnostics
  return 1
}

await_steer() {
  local needle="$1"
  local deadline=$((SECONDS + 5))
  local file
  while ((SECONDS < deadline)); do
    while IFS= read -r file; do
      if grep -Fq -- "$needle" "$file"; then
        printf 'Observed steer %q in %s\n' "$needle" "$file"
        return 0
      fi
    done < <(session_files)
    sleep 0.1
  done
  printf 'No steer containing %q appeared in the OMP .jsonl within 5 seconds\n' "$needle" >&2
  show_diagnostics
  return 1
}

human_get_issue() {
  curl --fail-with-body --silent --show-error \
    -H 'X-Dispatch-User: sjawhar' \
    "$DISPATCH_URL/api/v1/issues/$ISSUE_KEY"
}

await_new_ask() {
  local previous_ids="$1"
  local deadline=$((SECONDS + 30))
  local candidate
  while ((SECONDS < deadline)); do
    while IFS= read -r candidate; do
      [[ -n "$candidate" ]] || continue
      if ! printf '%s\n' "$previous_ids" | grep -Fqx -- "$candidate"; then
        printf '%s\n' "$candidate"
        return 0
      fi
    done < <(human_get_issue | jq -r '.open_asks[]? | select(.state == "open" and .question == "Ship it?") | .id')
    sleep 0.1
  done
  printf 'OMP did not create a new open "Ship it?" ask within 30 seconds\n' >&2
  show_diagnostics
  return 1
}

await_role_claim() {
  local session_id="$1"
  local deadline=$((SECONDS + 30))
  local holder
  while ((SECONDS < deadline)); do
    holder="$(curl --silent --show-error --fail "$ENVOY_URL/v1/roles/$role" 2>/dev/null | jq -r '.session_id // empty' || true)"
    if [[ "$holder" == "$session_id" ]]; then
      return 0
    fi
    sleep 0.1
  done
  printf 'OMP session %s did not claim role %s within 30 seconds\n' "$session_id" "$role" >&2
  show_diagnostics
  return 1
}

(
  cd "$pi_envoy_dir"
  bun run build
)
[[ -r "$extension" ]] || {
  printf 'expected built Pi Envoy extension is missing: %s\n' "$extension" >&2
  exit 1
}

previous_ask_ids=""
if [[ "$mode" == E4 ]]; then
  previous_ask_ids="$(human_get_issue | jq -r '.open_asks[]? | .id')"
  prompt="Call dispatch_ask on issue $ISSUE_KEY asking 'Ship it?' with options Yes/No, then wait."
else
  curl --fail-with-body --silent --show-error \
    -X PATCH \
    -H 'Content-Type: application/json' \
    -H 'X-Dispatch-User: sjawhar' \
    --data "{\"route\":\"role:$role\"}" \
    "$DISPATCH_URL/api/v1/issues/$ISSUE_KEY" >/dev/null
  prompt="Call envoy_role_set with role $role, then wait."
fi
readonly previous_ask_ids

readonly prompt
readonly -a omp_args=(
  "$OMP_BIN"
  -p
  --auto-approve
  --max-time=90
  --model anthropic/claude-fable-5-1
  --session-dir "$session_dir"
  --extension "$extension"
  "$prompt"
)
printf -v tmux_command '%q ' "${omp_args[@]}"
tmux new-session -d -s "$tmux_session" -c "$work_dir" "$tmux_command"

readonly transcript="$(await_session_file)"
if [[ "$mode" == E4 ]]; then
  readonly ask_id="$(await_new_ask "$previous_ask_ids")"
  curl --fail-with-body --silent --show-error \
    -X POST \
    -H 'Content-Type: application/json' \
    -H 'X-Dispatch-User: sjawhar' \
    --data "{\"selected\":[\"$answer_option\"],\"expected_edited_at\":null}" \
    "$DISPATCH_URL/api/v1/asks/$ask_id/answer" >/dev/null
  await_steer 'ask.answered'
  printf 'E4 passed: ask %s answered as %s; transcript %s\n' "$ask_id" "$answer_option" "$transcript"
else
  session_id="${transcript##*_}"
  session_id="${session_id%.jsonl}"
  readonly session_id
  await_role_claim "$session_id"
  readonly message="I1 E5 routed human message $tmux_session"
  curl --fail-with-body --silent --show-error \
    -X POST \
    -H 'Content-Type: application/json' \
    -H 'X-Dispatch-User: sjawhar' \
    --data "{\"body\":\"$message\"}" \
    "$DISPATCH_URL/api/v1/issues/$ISSUE_KEY/messages" >/dev/null
  await_steer "$message"
  printf 'E5 passed: role %s received message; transcript %s\n' "$role" "$transcript"
fi
