#!/usr/bin/env bash
set -euo pipefail

: "${DISPATCH_URL:?DISPATCH_URL is required}"
: "${DISPATCH_USER:?DISPATCH_USER is required}"
# DISPATCH_AGENT_TOKEN is optional: this human-facing smoke sequence does not
# need a bearer caller, but E1 supplies it when exercising agent operations.

readonly dispatch_url="${DISPATCH_URL%/}"
readonly dispatch_user="$DISPATCH_USER"
readonly project_key="E2E${RANDOM}"
readonly issue_key="${project_key}-1"
readonly sse_file="$(mktemp)"
readonly sse_pid_file="${sse_file}.pid"

cleanup() {
  if [[ -f "$sse_pid_file" ]]; then
    local sse_pid
    sse_pid="$(<"$sse_pid_file")"
    if kill -0 "$sse_pid" 2>/dev/null; then
      kill "$sse_pid" 2>/dev/null || true
      wait "$sse_pid" 2>/dev/null || true
    fi
  fi
  rm -f "$sse_pid_file" "$sse_file"
}
trap cleanup EXIT

request() {
  curl -fsS \
    -H "X-Dispatch-User: ${dispatch_user}" \
    -H "Content-Type: application/json" \
    "$@"
}

project="$(request -X POST "${dispatch_url}/api/v1/projects" \
  -d "{\"key\":\"${project_key}\",\"name\":\"Dispatch API E2E\"}")"
[[ "$project" == *"\"key\":\"${project_key}\""* ]] || {
  printf 'project response did not contain %s: %s\n' "$project_key" "$project" >&2
  exit 1
}

issue="$(request -X POST "${dispatch_url}/api/v1/issues" \
  -d "{\"project\":\"${project_key}\",\"title\":\"Dispatch API E2E\",\"spec\":\"# Hello\"}")"
[[ "$issue" == *"\"key\":\"${issue_key}\""* ]] || {
  printf 'issue response did not contain %s: %s\n' "$issue_key" "$issue" >&2
  exit 1
}

# The stream replays the issue creation, then must emit the live message event.
timeout 5s curl -fsSN \
  -H "X-Dispatch-User: ${dispatch_user}" \
  "${dispatch_url}/api/v1/events?since=0" >"$sse_file" &
echo "$!" >"$sse_pid_file"
sleep 0.1

message="$(request -X POST "${dispatch_url}/api/v1/issues/${issue_key}/messages" \
  -d '{"body":"Dispatch API E2E message"}')"
[[ "$message" == *'"body":"Dispatch API E2E message"'* ]] || {
  printf 'message response was unexpected: %s\n' "$message" >&2
  exit 1
}

for _ in $(seq 1 40); do
  if grep -Fq 'event: message.created' "$sse_file" && grep -Fq 'Dispatch API E2E message' "$sse_file"; then
    printf 'PASS: project %s, issue %s, and live message SSE event\n' "$project_key" "$issue_key"
    exit 0
  fi
  sleep 0.1
done
printf 'SSE message event was not observed:\n' >&2
cat "$sse_file" >&2
exit 1
