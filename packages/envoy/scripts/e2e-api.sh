#!/usr/bin/env bash
set -euo pipefail

: "${DISPATCH_URL:?DISPATCH_URL is required}"
: "${DISPATCH_USER:?DISPATCH_USER is required}"
: "${DISPATCH_AGENT_TOKEN:?DISPATCH_AGENT_TOKEN is required}"

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

agent_request() {
  curl -fsS \
    -H "Authorization: Bearer ${DISPATCH_AGENT_TOKEN}" \
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
  -d "{\"project\":\"${project_key}\",\"title\":\"Dispatch API E2E\",\"spec\":\"The quick brown fox\"}")"
[[ "$issue" == *"\"key\":\"${issue_key}\""* ]] || {
  printf 'issue response did not contain %s: %s\n' "$issue_key" "$issue" >&2
  exit 1
}

# The stream replays the issue creation, then must emit the live answer event.
timeout 5s curl -fsSN \
  -H "X-Dispatch-User: ${dispatch_user}" \
  "${dispatch_url}/api/v1/events?since=0" >"$sse_file" &
echo "$!" >"$sse_pid_file"
sleep 0.1

ask="$(agent_request -X POST "${dispatch_url}/api/v1/issues/${issue_key}/asks" \
  -d '{"question":"Which colour?","options":[{"label":"brown"},{"label":"red"}],"anchor":{"artifact":"spec","quote":"brown"},"actor":{"kind":"session","id":"session-e2e-api"}}')"
ask_id="$(jq -er '.id' <<<"$ask")"
jq -e '.anchor.quote == "brown" and (.anchor.mark_id | type == "string") and .state == "open"' <<<"$ask" >/dev/null || {
  printf 'anchored ask response was unexpected: %s\n' "$ask" >&2
  exit 1
}

inbox="$(request "${dispatch_url}/api/v1/inbox?project=${project_key}")"
jq -e --arg id "$ask_id" 'length == 1 and .[0].id == $id and .[0].issue.key != "" and .[0].issue.title == "Dispatch API E2E"' <<<"$inbox" >/dev/null || {
  printf 'inbox response was unexpected: %s\n' "$inbox" >&2
  exit 1
}

answer="$(request -X POST "${dispatch_url}/api/v1/asks/${ask_id}/answer" -d '{"selected":["brown"]}')"
jq -e '.state == "answered" and .answer.user != "" and .answer.selected == ["brown"]' <<<"$answer" >/dev/null || {
  printf 'answer response was unexpected: %s\n' "$answer" >&2
  exit 1
}

for _ in $(seq 1 40); do
  if grep -Fq 'event: ask.answered' "$sse_file" && grep -Fq '"state":"answered"' "$sse_file"; then
    printf 'PASS: project %s, anchored ask, inbox, answer, and live ask.answered SSE event\n' "$project_key"
    exit 0
  fi
  sleep 0.1
done
printf 'SSE ask.answered event was not observed:\n' >&2
cat "$sse_file" >&2
exit 1
