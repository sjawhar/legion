#!/usr/bin/env bash
# Copies one project's duplicate-closed issues, and every other issue they compete with, from a
# Dispatch server into a JSONL file, for TestRelatedRecallOnDuplicates (internal/dispatch/api) to
# score the related-item check on (INFRA-568). A duplicate is a done issue whose messages carry
# `Duplicate of dispatch://<KEY>`: closing with a reason posts the reason as a message just before
# the close (envoy-client's dispatch_issue_update), and the dispatch-first skill words that reason
# so. Each text is taken as it stood when the duplicate was filed: the duplicate's title at its
# creation and spec at version 1, the survivor's title and spec as of the duplicate's creation, so
# a survivor's spec edited after the close (closers copy the duplicate's text over) cannot hold
# the duplicate's own words.
#
# The file is production data: it is written with mode 600, and nothing from it is committed.
# Only GET requests are made, one at a time. stdout gets counts only.
set -euo pipefail

if (($# != 2)); then
  printf 'usage: DISPATCH_URL=<url> DISPATCH_TOKEN_FILE=<file> %s <project> <output.jsonl>\n' "$0" >&2
  exit 2
fi
readonly project="$1"
readonly output="$2"
: "${DISPATCH_URL:?DISPATCH_URL must name the Dispatch server to read}"
: "${DISPATCH_TOKEN_FILE:?DISPATCH_TOKEN_FILE must name a file holding a Dispatch token}"
[[ -r "$DISPATCH_TOKEN_FILE" ]] || {
  printf 'DISPATCH_TOKEN_FILE is not readable: %s\n' "$DISPATCH_TOKEN_FILE" >&2
  exit 2
}
for command in curl jq; do
  command -v "$command" >/dev/null || {
    printf '%s is required\n' "$command" >&2
    exit 1
  }
done

umask 077
readonly base="${DISPATCH_URL%/}/api/v1"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/related-recall.XXXXXX")"
readonly workdir
trap 'rm -rf "$workdir"' EXIT
mkdir "$workdir/issues"
# The token reaches curl through a header file, so it never sits on a command line.
printf 'Authorization: Bearer %s\n' "$(tr -d '\r\n' <"$DISPATCH_TOKEN_FILE")" >"$workdir/auth"

# get prints the JSON body of GET <path> under /api/v1. --fail-with-body makes any non-2xx an
# error; 404 alone is reported as status 4 so a caller can tell a missing issue from a failure.
get() {
  local status
  status="$(curl --silent --show-error --retry 3 --retry-all-errors --max-time 60 \
    -H "@$workdir/auth" -o "$workdir/body" -w '%{http_code}' "${base}$1")"
  case "$status" in
    2??) cat "$workdir/body" ;;
    404) return 4 ;;
    *)
      printf 'GET %s answered %s\n' "$1" "$status" >&2
      return 1
      ;;
  esac
}

uri() { jq -rn --arg value "$1" '$value | @uri'; }

# Every issue of the project, all statuses, one page at a time.
: >"$workdir/keys"
offset=0
while :; do
  page="$(get "/issues?project=$(uri "$project")&limit=250&offset=${offset}")"
  jq -r '.issues[].key' <<<"$page" >>"$workdir/keys"
  count="$(jq '.issues | length' <<<"$page")"
  total="$(jq '.total' <<<"$page")"
  offset=$((offset + count))
  if ((count == 0 || offset >= total)); then
    break
  fi
done
if [[ ! -s "$workdir/keys" ]]; then
  printf 'project %s has no issues\n' "$project" >&2
  exit 1
fi

# Dispatch writes times with as many fraction digits as they need; padded to six, two times
# compare as text, which every comparison below relies on.
readonly times='def t: if . == null then . else sub("Z$"; "") | split(".") | .[0] + "." + ((.[1] // "") + "000000")[0:6] + "Z" end;'

# One file per issue: its row, with its primary document's versions (number and time only).
# shellcheck disable=SC2016 # $id is jq's variable, not a shell expansion
readonly issue_summary="$times"'{key, title, created_at: (.created_at | t), updated_at: (.updated_at | t), status, project,
  spec_artifact: .primary_artifact_id,
  versions: ((.primary_artifact_id as $id | .artifacts[]? | select(.id == $id) | [.versions[] | {number, created_at: (.created_at | t)}]) // [])}'
while read -r key; do
  get "/issues/$(uri "$key")" | jq "$issue_summary" >"$workdir/issues/$key.json"
done <"$workdir/keys"

# Each done issue's duplicate target: the latest message or reply naming one.
: >"$workdir/duplicates.jsonl"
while read -r key; do
  [[ "$(jq -r .status "$workdir/issues/$key.json")" == "done" ]] || continue
  get "/issues/$(uri "$key")/messages" | jq -c --arg key "$key" "$times"'
    [.[] | .message, (.replies // [])[] | select(.body != null)
      | {created_at: (.created_at | t), match: (.body | capture("Duplicate of dispatch://(?<k>[A-Za-z][A-Za-z0-9_]*-[0-9]+)"))}]
    | sort_by(.created_at) | last
    | select(. != null) | {duplicate: $key, survivor: (.match.k | ascii_upcase)}' >>"$workdir/duplicates.jsonl"
done <"$workdir/keys"

# Each pair kept or left out with its reason. A survivor outside the listing is read once to tell
# another project's issue from one that does not exist.
: >"$workdir/pairs.jsonl"
while read -r pair; do
  duplicate="$(jq -r .duplicate <<<"$pair")"
  survivor="$(jq -r .survivor <<<"$pair")"
  filed="$(jq -r .created_at "$workdir/issues/$duplicate.json")"
  reason=""
  if [[ "$survivor" == "$duplicate" ]]; then
    reason=self
  elif [[ -f "$workdir/issues/$survivor.json" ]]; then
    if [[ "$(jq -r .created_at "$workdir/issues/$survivor.json")" > "$filed" ]]; then
      reason=survivor_filed_later
    fi
  else
    status=0
    survivor_row="$(get "/issues/$(uri "$survivor")")" || status=$?
    case "$status" in
      0) reason=other_project ;;
      4) reason=survivor_not_found ;;
      *) exit 1 ;;
    esac
    # An issue of this project the listing did not hold would be a listing fault, not a pair to
    # score; it is counted apart so it cannot pass as another project's.
    if [[ "$status" == 0 && "$(jq -r .project <<<"$survivor_row")" == "$project" ]]; then
      reason=survivor_not_listed
    fi
  fi
  jq -c --arg filed "$filed" --arg reason "$reason" '. + {duplicate_created_at: $filed, reason: $reason}' <<<"$pair" >>"$workdir/pairs.jsonl"
done <"$workdir/duplicates.jsonl"

# markdown writes one version of an issue's primary document to a file, byte for byte.
markdown() {
  get "/artifacts/$(uri "$(jq -r .spec_artifact "$workdir/issues/$1.json")")/versions/$2" | jq -j .markdown >"$3"
}

# titles prints an issue's titles over time, [{created_at, title}], from its issue.created and
# issue.updated events, each of which carries the whole issue.
titles() {
  local after=0 page
  : >"$workdir/events"
  while :; do
    page="$(get "/issues/$(uri "$1")/events?after=${after}&limit=200")"
    jq -c '.[]' <<<"$page" >>"$workdir/events"
    (("$(jq length <<<"$page")" == 200)) || break
    after="$(jq '.[-1].seq' <<<"$page")"
  done
  jq -sc "$times"'[.[] | select(.type == "issue.created" or .type == "issue.updated") | {created_at: (.created_at | t), title: .payload.title}]
    | reduce .[] as $e ([]; if (last | .title) == $e.title then . else . + [$e] end)' "$workdir/events"
}

# The versions each issue needs beyond its latest: version 1 of a kept pair's duplicate, and of a
# kept pair's survivor the latest created at or before the duplicate's filing.
jq -sc '[.[] | select(.reason == "")]' "$workdir/pairs.jsonl" >"$workdir/kept.json"
: >"$workdir/output.jsonl"
while read -r key; do
  row="$workdir/issues/$key.json"
  needed="$(jq -c --arg key "$key" --slurpfile row "$row" '
    ($row[0].versions) as $versions
    | [.[] | select(.duplicate == $key) | 1]
      + [.[] | select(.survivor == $key) | .duplicate_created_at as $at
          | [$versions[] | select(.created_at <= $at)] | last | .number // empty]
    | unique | map(select(. as $n | $versions | any(.number == $n)))' "$workdir/kept.json")"
  latest="$(jq '[.versions[].number] | max // 0' "$row")"
  : >"$workdir/spec"
  if ((latest > 0)); then
    markdown "$key" "$latest" "$workdir/spec"
  fi
  : >"$workdir/versions"
  for number in $(jq -r '.[]' <<<"$needed"); do
    if ((number == latest)); then
      cp "$workdir/spec" "$workdir/version"
    else
      markdown "$key" "$number" "$workdir/version"
    fi
    jq -c --rawfile markdown "$workdir/version" --argjson number "$number" \
      '{number: $number, created_at: (.versions[] | select(.number == $number) | .created_at), markdown: $markdown}' "$row" >>"$workdir/versions"
  done
  title_versions='[]'
  if jq -e --arg key "$key" 'any(.[]; .duplicate == $key or .survivor == $key)' "$workdir/kept.json" >/dev/null; then
    title_versions="$(titles "$key")"
  fi
  jq -c --rawfile spec "$workdir/spec" --slurpfile versions "$workdir/versions" --argjson titles "$title_versions" '
    {record: "issue", key, title, created_at, updated_at, status, spec: $spec}
    + (if ($versions | length) > 0 then {spec_versions: $versions} else {} end)
    + (if ($titles | length) > 1 then {title_versions: $titles} else {} end)' "$row" >>"$workdir/output.jsonl"
done <"$workdir/keys"
jq -c '.[] | {record: "pair", duplicate, survivor, duplicate_created_at}' "$workdir/kept.json" >>"$workdir/output.jsonl"
mv "$workdir/output.jsonl" "$output"

# A chain is a kept pair whose survivor was itself closed as a duplicate of a third issue.
jq -rs --argjson issues "$(wc -l <"$workdir/keys")" '
  (map(.duplicate) | unique) as $duplicates
  | "pairs found: \(length)",
    "pairs kept: \(map(select(.reason == "")) | length)",
    (group_by(.reason)[] | select(.[0].reason != "") | "left out, \(.[0].reason): \(length)"),
    "chains (kept pairs whose survivor was itself closed as a duplicate): \(map(select(.reason == "" and (.survivor as $s | $duplicates | index($s)))) | length)",
    "issues loaded: \($issues)"' "$workdir/pairs.jsonl"
