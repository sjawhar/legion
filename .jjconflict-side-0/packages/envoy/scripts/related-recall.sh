#!/usr/bin/env bash
# Copies one project's pairs of issues closed one into the other, and every other issue they
# compete with, from a Dispatch server into a JSONL file, for TestRelatedRecallOnDuplicates
# (internal/dispatch/api) to score the related-item check on. A pair is a done issue
# whose close reason names one other issue that covers it: "Duplicate of", "Duplicate:", "Folded
# into", "Covered by" or "Superseded by", in any case, followed by a Dispatch issue reference
# (a Dispatch link to KEY, a bare KEY-123 of one of the server's projects, or an /issues/KEY link). The
# close reason is the issue's last top-level message posted at or before its close: closing with a
# reason posts the reason as a message just before the close (envoy-client's
# dispatch_issue_update), and the issue.closed event carries no reason. A reason naming only a pull
# request, several issues or the issue itself is not a pair, and is counted by what it names.
#
# The query is the later-filed issue of a pair as it was filed: its title at its creation and its
# spec at version 1. That is the closed issue when the issue it names came first (forward) and the
# named issue when that came later (reversed). The target is the earlier issue, its title and spec
# as of the query's filing, so a target's spec edited after the close (closers copy the closed
# issue's text over) cannot hold the query's own words.
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
readonly issue_summary="$times"'{key, title, created_at: (.created_at | t), updated_at: (.updated_at | t), closed_at: (.closed_at | t), status, project,
  spec_artifact: .primary_artifact_id,
  versions: ((.primary_artifact_id as $id | .artifacts[]? | select(.id == $id) | [.versions[] | {number, created_at: (.created_at | t)}]) // [])}'
while read -r key; do
  get "/issues/$(uri "$key")" | jq "$issue_summary" >"$workdir/issues/$key.json"
done <"$workdir/keys"

# The server's project keys: a bare KEY-123 names an issue only when KEY is one of them, so a name
# such as UTF-8 is not read as an issue.
projects="$(get /projects | jq -c '[.[].key]')"

# close_reason classifies a done issue's close reason, read from its message threads ($row its
# summary, $key its key, $projects the server's project keys). A wording names what follows it,
# past spaces and opening quotes, brackets or emphasis: an issue reference starting there, or a
# pull request (#123, owner/repo#123, PR 123, a pull request link). A reference that goes on into
# a path or an anchor names something inside an issue, not the issue. The regular expressions use
# no look-around, which some jq implementations lack.
# shellcheck disable=SC2016 # $row, $key and the rest are jq's variables, not shell expansions
readonly close_reason='
def refs($projects):
  [ (match("dispatch:/{2}([A-Za-z][A-Za-z0-9_]*-[0-9]+)([A-Za-z0-9_/#-]?)"; "g"),
     match("https?://[^\\s/]+/issues/([A-Za-z][A-Za-z0-9_]*-[0-9]+)([A-Za-z0-9_/#-]?)"; "g")
      | select((.captures[1].string // "") == "") | {offset, key: (.captures[0].string | ascii_upcase)}),
    (match("[A-Za-z0-9_/:#.-]+"; "g") | (.string | sub("[.:]+$"; "")) as $token
      | select($token | test("^[A-Z][A-Z0-9_]*-[0-9]+$"))
      | select(($token | sub("-[0-9]+$"; "")) as $prefix | any($projects[]; . == $prefix))
      | {offset, key: $token}) ];
$row[0].closed_at as $closed
| ([.[] | .message | select(.body != null and $closed != null and (.created_at | t) <= $closed)]
   | sort_by(.created_at | t) | last | .body // "") as $body
| ($body | refs($projects)) as $refs
| ([$refs[].key | select(. != $key)] | unique) as $others
| [$body | match("duplicate\\s+of|duplicate:|folded\\s+into|covered\\s+by|superseded\\s+by"; "gi")
    | (.offset + .length) as $end
    | ($end + ($body[$end:] | match("^[\\s`*\\[(<\"]*") | .length)) as $at
    | {wording: (.string | ascii_downcase | gsub("\\s+"; " ")),
       issue: (first($refs[] | select(.offset == $at) | .key) // null),
       pull_request: ($body[$at:] | test("^((PR|pull request)\\s*#?[0-9]+|#[0-9]+|[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[0-9]+|https?://github\\.com/[^\\s/]+/[^\\s/]+/pull/[0-9]+)"; "i"))}]
  as $wordings
| (first($wordings[] | select(.issue != null or .pull_request)) // null) as $hit
| {closed: $key, closed_at: $closed, created_at: $row[0].created_at}
  + if ($wordings | length) == 0 then {kind: "none"}
    elif $hit == null then {kind: "no_reference", wording: $wordings[0].wording}
    elif $hit.issue == null then {kind: "pull_request", wording: $hit.wording}
    elif $hit.issue == $key then {kind: "self", wording: $hit.wording}
    elif ($others | length) > 1 then {kind: "several_issues", wording: $hit.wording}
    else {kind: "pair", wording: $hit.wording, survivor: $hit.issue} end'

# Each done issue's close reason, classified.
: >"$workdir/closes.jsonl"
while read -r key; do
  row="$workdir/issues/$key.json"
  [[ "$(jq -r .status "$row")" == "done" ]] || continue
  get "/issues/$(uri "$key")/messages" |
    jq -c --arg key "$key" --argjson projects "$projects" --slurpfile row "$row" "$times$close_reason" >>"$workdir/closes.jsonl"
done <"$workdir/keys"

# One pair per two issues: a pair named by two closes (each issue closed into the other) is
# counted once, by the first close.
jq -sc 'map(select(.kind == "pair")) | group_by([.closed, .survivor] | sort) | map(sort_by(.closed_at) | .[0] + {closes: length})
  | sort_by(.closed_at) | .[]' "$workdir/closes.jsonl" >"$workdir/found.jsonl"

# Each pair kept, with its query and target, or left out with its reason. A named issue outside
# the listing is read once to tell another project's issue from one that does not exist.
: >"$workdir/pairs.jsonl"
while read -r pair; do
  closed="$(jq -r .closed <<<"$pair")"
  survivor="$(jq -r .survivor <<<"$pair")"
  reason=""
  if [[ ! -f "$workdir/issues/$survivor.json" ]]; then
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
  if [[ -n "$reason" ]]; then
    jq -c --arg reason "$reason" '. + {reason: $reason}' <<<"$pair" >>"$workdir/pairs.jsonl"
    continue
  fi
  jq -nc --argjson pair "$pair" --slurpfile closed "$workdir/issues/$closed.json" --slurpfile survivor "$workdir/issues/$survivor.json" '
    if $survivor[0].created_at > $closed[0].created_at
    then {direction: "reversed", query: $survivor[0], target: $closed[0]}
    else {direction: "forward", query: $closed[0], target: $survivor[0]} end
    | $pair + {reason: "", direction, query: .query.key, target: .target.key, query_created_at: .query.created_at}' >>"$workdir/pairs.jsonl"
done <"$workdir/found.jsonl"

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

# The versions each issue needs beyond its latest: version 1 of a kept pair's query, and of a kept
# pair's target the latest created at or before the query's filing.
jq -sc '[.[] | select(.reason == "")]' "$workdir/pairs.jsonl" >"$workdir/kept.json"
: >"$workdir/output.jsonl"
while read -r key; do
  row="$workdir/issues/$key.json"
  needed="$(jq -c --arg key "$key" --slurpfile row "$row" '
    ($row[0].versions) as $versions
    | [.[] | select(.query == $key) | 1]
      + [.[] | select(.target == $key) | .query_created_at as $at
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
  if jq -e --arg key "$key" 'any(.[]; .query == $key or .target == $key)' "$workdir/kept.json" >/dev/null; then
    title_versions="$(titles "$key")"
  fi
  jq -c --rawfile spec "$workdir/spec" --slurpfile versions "$workdir/versions" --argjson titles "$title_versions" '
    {record: "issue", key, title, created_at, updated_at, status, spec: $spec}
    + (if ($versions | length) > 0 then {spec_versions: $versions} else {} end)
    + (if ($titles | length) > 1 then {title_versions: $titles} else {} end)' "$row" >>"$workdir/output.jsonl"
done <"$workdir/keys"
jq -c '.[] | {record: "pair", closed, survivor, wording, direction, query, target, query_created_at}' "$workdir/kept.json" >>"$workdir/output.jsonl"
mv "$workdir/output.jsonl" "$output"

# A chain is a kept pair whose named issue was itself closed into another issue.
jq -rs --argjson issues "$(wc -l <"$workdir/keys")" --slurpfile closes "$workdir/closes.jsonl" '
  def directions: "\(length) (forward \(map(select(.direction == "forward")) | length), reversed \(map(select(.direction == "reversed")) | length))";
  ($closes | map(select(.kind == "pair") | .closed) | unique) as $closed
  | map(select(.reason == "")) as $kept
  | "done issues read: \($closes | length)",
    "close reasons with a wording: \($closes | map(select(.kind != "none")) | length)",
    ($closes | map(select(.kind != "none" and .kind != "pair")) | group_by(.kind)[] | "not a pair, \(.[0].kind): \(length)"),
    "close reasons naming one other issue: \($closes | map(select(.kind == "pair")) | length)",
    "pairs found (two issues, counted once): \(length)",
    (group_by(.wording)[] | "pairs found, \(.[0].wording): \(length)"),
    "pairs kept: \($kept | directions)",
    ($kept | group_by(.wording)[] | "pairs kept, \(.[0].wording): \(directions)"),
    (group_by(.reason)[] | select(.[0].reason != "") | "left out, \(.[0].reason): \(length)"),
    "chains (kept pairs whose named issue was itself closed into another): \($kept | map(select(.survivor as $s | $closed | index($s))) | length)",
    "issues loaded: \($issues)"' "$workdir/pairs.jsonl"
