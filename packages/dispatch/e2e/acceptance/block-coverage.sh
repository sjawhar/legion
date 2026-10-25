#!/usr/bin/env bash
# Measures how much of a Dispatch server's documents the block graph covers, reading only. For
# every document it can list (each project's documents and each issue's documents), a document is
# not indexed when GET /api/v1/artifacts/{id}/blocks lists blocks while
# GET /api/v1/references?to=<document>&kind=part_of lists none: its blocks are not yet graph
# nodes. Every other document counts as indexed, one with no blocks included, since it owes none.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed). PROJECT, when set, limits the count to that project. It prints
# `documents=<n> indexed=<n> not_indexed=<n>`, then each not-indexed document's reference on a line
# of its own, and exits 1 when a read fails.
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token [PROJECT=CORE] block-coverage.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN)
USAGE
}

if [[ -z "${DISPATCH_URL:-}" ]]; then
  printf 'required environment variable DISPATCH_URL is unset\n' >&2
  usage
  exit 2
fi
for command in curl jq; do
  command -v "$command" >/dev/null || {
    printf 'required command %s is unavailable\n' "$command" >&2
    exit 2
  }
done

if [[ -n "${DISPATCH_TOKEN:-}" ]]; then
  token="$DISPATCH_TOKEN"
elif [[ -n "${DISPATCH_TOKEN_FILE:-}" ]]; then
  [[ -r "$DISPATCH_TOKEN_FILE" ]] || {
    printf 'DISPATCH_TOKEN_FILE names %s, which cannot be read\n' "$DISPATCH_TOKEN_FILE" >&2
    exit 2
  }
  token="$(<"$DISPATCH_TOKEN_FILE")"
  token="${token#"${token%%[![:space:]]*}"}"
  token="${token%"${token##*[![:space:]]}"}"
else
  printf 'set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE to an agent bearer\n' >&2
  usage
  exit 2
fi
[[ -n "$token" ]] || {
  printf 'the agent bearer is empty\n' >&2
  exit 2
}
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE

readonly base="${DISPATCH_URL%/}"

umask 077
work="$(mktemp -d)"
readonly work
trap 'rm -rf "$work"' EXIT
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

q() { jq -rn --arg v "$1" '$v|@uri'; }

# get PATH OUT: writes the answer to PATH into OUT; any status but 200 stops the count.
get() {
  local status
  status="$(curl -sS -o "$2" -w '%{http_code}' -H "@$work/auth" "$base$1" 2>"$work/curl")" ||
    status="curl failed: $(<"$work/curl")"
  if [[ "$status" != 200 ]]; then
    printf 'GET %s: HTTP %s: %s\n' "$1" "$status" "$(head -c 600 "$2" 2>/dev/null)" >&2
    exit 1
  fi
}

# Each listed document as `<id> <reference>`: an issue's primary document is dispatch://KEY/spec,
# another issue document dispatch://KEY/artifact/<slug>, an unlinked project document
# dispatch://PROJECT/artifact/<slug>. Files have no blocks and are not counted.
documents_of() {
  jq -r '.[] | select(.kind == "doc") |
    "\(.id) " + (if .issue_key != null and .primary then "dispatch://\(.issue_key)/spec"
      elif .issue_key != null then "dispatch://\(.issue_key)/artifact/\(.slug)"
      elif .project != null then "dispatch://\(.project)/artifact/\(.slug)"
      else "dispatch://agent/\(.session_id)/artifact/\(.slug)" end)' "$1" >>"$work/documents"
}

: >"$work/documents"
if [[ -n "${PROJECT:-}" ]]; then
  printf '%s\n' "$PROJECT" >"$work/projects"
else
  get /api/v1/projects "$work/body"
  jq -r '.[].key' "$work/body" >"$work/projects"
fi
while read -r project; do
  get "/api/v1/projects/$(q "$project")/artifacts" "$work/body"
  documents_of "$work/body"
  get "/api/v1/issues?project=$(q "$project")" "$work/body"
  jq -r '.[].key' "$work/body" >"$work/issues"
  while read -r issue; do
    get "/api/v1/issues/$(q "$issue")/artifacts" "$work/body"
    documents_of "$work/body"
  done <"$work/issues"
done <"$work/projects"

# A project's list also holds its issues' documents, so each document is counted once.
sort -u "$work/documents" -o "$work/documents"
documents=0
not_indexed=()
while read -r id ref; do
  documents=$((documents + 1))
  get "/api/v1/artifacts/$id/blocks" "$work/body"
  blocks="$(jq 'length' "$work/body")"
  get "/api/v1/references?to=$(q "$ref")&kind=part_of" "$work/body"
  parts="$(jq '[.edges[] | select(.kind == "part_of")] | length' "$work/body")"
  if ((blocks > 0 && parts == 0)); then
    not_indexed+=("$ref")
  fi
done <"$work/documents"

printf 'documents=%d indexed=%d not_indexed=%d\n' \
  "$documents" "$((documents - ${#not_indexed[@]}))" "${#not_indexed[@]}"
if ((${#not_indexed[@]} > 0)); then
  printf '%s\n' "${not_indexed[@]}"
fi
