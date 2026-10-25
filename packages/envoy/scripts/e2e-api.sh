#!/usr/bin/env bash
set -euo pipefail

: "${DISPATCH_URL:?DISPATCH_URL is required}"
: "${DISPATCH_USER:?DISPATCH_USER is required}"
: "${DISPATCH_AGENT_TOKEN:?DISPATCH_AGENT_TOKEN is required}"

readonly dispatch_url="${DISPATCH_URL%/}"
readonly dispatch_user="$DISPATCH_USER"
# DISPATCH_AGENT_TOKEN may list several values while the token rotates; the first is the current one.
read -r agent_token _ <<<"$DISPATCH_AGENT_TOKEN"
readonly agent_token
readonly project_key="E2E${RANDOM}"
readonly issue_key="${project_key}-1"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly script_dir
root="$(cd "$script_dir/../../.." && pwd -P)"
readonly root
events_file="$(mktemp)"
readonly events_file
events_pid=

# shellcheck disable=SC2329 # cleanup runs through the EXIT trap below
cleanup() {
  if [ -n "${events_pid:-}" ] && kill -0 "$events_pid" 2>/dev/null; then
    kill "$events_pid" 2>/dev/null || true
    wait "$events_pid" 2>/dev/null || true
  fi
  rm -f "$events_file"
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
    -H "Authorization: Bearer ${agent_token}" \
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

# The event feed replays the issue creation, then must emit the live answer event.
bun "$root/scripts/e2e/lib/dispatch-live-events.ts" \
  --url "$dispatch_url" --user "$dispatch_user" --since 0 --until '"state":"answered"' --timeout 5 \
  >"$events_file" &
events_pid=$!
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

answer="$(request -X POST "${dispatch_url}/api/v1/asks/${ask_id}/answer" -d '{"selected":["brown"],"expected_edited_at":null}')"
jq -e '.state == "answered" and .answer.user != "" and .answer.selected == ["brown"]' <<<"$answer" >/dev/null || {
  printf 'answer response was unexpected: %s\n' "$answer" >&2
  exit 1
}

for _ in $(seq 1 40); do
  if grep -Fq '"state":"answered"' "$events_file"; then
    if wait "$events_pid"; then
      events_pid=
      printf 'PASS: project %s, anchored ask, inbox, answer, and live ask.answered event\n' "$project_key"
      exit 0
    fi
    break
  fi
  sleep 0.1
done
printf 'live ask.answered event was not observed:\n' >&2
cat "$events_file" >&2
exit 1
