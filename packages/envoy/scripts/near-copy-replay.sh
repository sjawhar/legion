#!/usr/bin/env bash
# Copies one project's history from a Dispatch server into a JSONL file, for
# TestNearCopyRefusalReplay (internal/dispatch/api) to replay the near-copy refusal over: it walks
# the project forward in time, applies every title, spec, status and parent change up to each
# create, and runs the real gate for that create against the project as it stood then.
#
# This is its own record set, not related-recall.sh's. That harness is run at main as well as at a
# head, and main's reader fails on a record kind it does not know, so neither script may read the
# other's file.
#
# Per issue of the project the file carries its key, its creation time, its primary document's
# id, and the four histories the replay applies:
#
#   status_history   from issue.created (the status it was filed in), issue.closed, and every
#                    issue.updated whose `changed` holds `status`
#   parent_history   from issue.created and every issue.updated whose `changed` holds `parent`
#   title_history    from issue.created and every issue.updated that changed the title
#   ancestors        the `ancestors` of the issue.created payload, which events written before
#                    that field existed do not carry; for those the parent chain as it stands now
#                    is read from the issue rows instead
#
# and the spec versions a replay needs: version 1 of every issue (the text a create was filed
# with, which is what a query replays), the latest version created at or before the window's
# start, and every version created inside the window. A candidate's text at a create is the
# latest of those at or before it.
#
# The queries are every create inside the window, and each pair named with --pair, whose query
# issue is replayed whatever its creation time: the four pairs INFRA-1160's Verification document
# names were filed before the window.
#
# The file is production data: it is written with mode 600, and nothing from it is committed.
# Only GET requests are made, one at a time. stdout gets counts only.
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: DISPATCH_URL=<url> DISPATCH_TOKEN_FILE=<file> near-copy-replay.sh <project> <output.jsonl>
         [--from <RFC3339>] [--to <RFC3339>] [--pair <QUERY>=<TARGET>]...

  --from  the window's start, inclusive (default 2026-10-20T00:00:00Z)
  --to    the window's end, exclusive (default 2026-11-05T00:00:00Z)
  --pair  a known near-copy pair to replay whatever its creation time, repeatable
EOF
  exit 2
}

from=2026-10-20T00:00:00Z
to=2026-11-05T00:00:00Z
pairs=()
project=""
output=""
while (($# > 0)); do
  case "$1" in
    --from)
      from="${2:?--from needs a time}"
      shift 2
      ;;
    --to)
      to="${2:?--to needs a time}"
      shift 2
      ;;
    --pair)
      pairs+=("${2:?--pair needs QUERY=TARGET}")
      shift 2
      ;;
    -*) usage ;;
    *)
      if [[ -z "$project" ]]; then
        project="$1"
      elif [[ -z "$output" ]]; then
        output="$1"
      else
        usage
      fi
      shift
      ;;
  esac
done
[[ -n "$project" && -n "$output" ]] || usage
readonly project output from to
scripts_dir="$(cd "$(dirname "$0")" && pwd)"
readonly scripts_dir
# shellcheck source-path=SCRIPTDIR source=lib/dispatch-export.sh
. "${scripts_dir}/lib/dispatch-export.sh"

# dispatch_export_open checks DISPATCH_URL, DISPATCH_TOKEN_FILE, curl and jq, sets base and
# workdir, takes the EXIT trap that removes workdir, and writes the token header curl reads.
# get, uri, times, list_issue_keys, list_events and markdown come from the same file: this
# export and related-recall.sh read a Dispatch project the same way, from one copy of it.
dispatch_export_open near-copy-replay
readonly base workdir
mkdir "$workdir/issues" "$workdir/events"

pad() { jq -rn --arg value "$1" "$times"'$value | t'; }
window_from="$(pad "$from")"
window_to="$(pad "$to")"
readonly window_from window_to

# Every issue of the project, all statuses.
list_issue_keys "$project" "$workdir/keys"
readonly issues_read="$(wc -l <"$workdir/keys")"

# One file per issue: the fields the replay needs, with its primary document's versions (number
# and time only). is_primary is not set on every artifact row, so the primary document is the one
# primary_artifact_id names.
# shellcheck disable=SC2016 # $id is jq's variable, not a shell expansion
readonly issue_summary="$times"'{key, title, created_at: (.created_at | t), status, parent,
  spec_artifact: .primary_artifact_id,
  versions: ((.primary_artifact_id as $id | .artifacts[]? | select(.id == $id)
              | [.versions[] | {number, created_at: (.created_at | t)}]) // [])}'
read_count=0
while read -r key; do
  get "/issues/$(uri "$key")" | jq "$issue_summary" >"$workdir/issues/$key.json"
  read_count=$((read_count + 1))
  if ((read_count % 100 == 0)); then
    printf 'read %d of %d issues\n' "$read_count" "$issues_read" >&2
  fi
done <"$workdir/keys"

# histories reads an issue's four histories from its events. Each history is the value at each
# point it changed, oldest first, and the first entry is the issue as it was created, so a replay
# that takes the last entry at or before a time always finds one.
#
# `changed` names the fields an issue.updated wrote; an event written before that field existed
# carries none, so such an event is read as a change of every field it can carry, which only adds
# an entry with the value already in force.
# shellcheck disable=SC2016 # the $ names below are jq's variables
readonly histories="$times"'
def changes($field): [.[]
  | select(.type == "issue.created" or .type == "issue.closed"
           or (.type == "issue.updated" and (.payload.changed == null or (.payload.changed | index($field)))))
  | {at: (.created_at | t), value: .payload[$field]}]
  | reduce .[] as $e ([]; if (last | .value) == $e.value then . else . + [$e] end);
{
  status_history: (changes("status") | map({at, status: .value})),
  parent_history: (changes("parent") | map({at, parent: .value})),
  title_history: ([.[] | select(.type == "issue.created" or .type == "issue.updated")
                   | {at: (.created_at | t), value: .payload.title}]
                  | reduce .[] as $e ([]; if (last | .value) == $e.value then . else . + [$e] end)
                  | map({at, title: .value})),
  ancestors: ([.[] | select(.type == "issue.created") | .payload.ancestors] | first)
}'

# The ancestors an issue.created written before that field existed does not carry: the parent
# chain as the issue rows stand now, parent first.
ancestors_now() {
  local key="$1" chain=() parent depth=0
  while ((depth < 32)); do
    parent="$(jq -r '.parent // empty' "$workdir/issues/$key.json" 2>/dev/null || true)"
    [[ -n "$parent" && -r "$workdir/issues/$parent.json" ]] || break
    chain+=("$parent")
    key="$parent"
    depth=$((depth + 1))
  done
  if ((${#chain[@]} == 0)); then
    printf '[]'
  else
    printf '%s\n' "${chain[@]}" | jq -Rsc 'split("\n") | map(select(. != ""))'
  fi
}

event_count=0
while read -r key; do
  list_events "$key" "$workdir/events/$key"
  jq -sc "$histories" "$workdir/events/$key" >"$workdir/events/$key.histories"
  if [[ "$(jq -r '.ancestors // "null"' "$workdir/events/$key.histories")" == "null" ]]; then
    jq -c --argjson ancestors "$(ancestors_now "$key")" '.ancestors = $ancestors' \
      "$workdir/events/$key.histories" >"$workdir/events/$key.fixed"
    mv "$workdir/events/$key.fixed" "$workdir/events/$key.histories"
  fi
  event_count=$((event_count + 1))
  if ((event_count % 100 == 0)); then
    printf 'read the events of %d of %d issues\n' "$event_count" "$issues_read" >&2
  fi
done <"$workdir/keys"

# The queries: every create inside the window, and every --pair's query whatever its time.
printf '%s\n' "${pairs[@]+"${pairs[@]}"}" | sed '/^$/d' | jq -Rsc 'split("\n") | map(select(. != "")
  | split("=") | {record: "pair", query: .[0], target: .[1]})' >"$workdir/pairs.json"
: >"$workdir/queries"
while read -r key; do
  jq -r --arg from "$window_from" --arg to "$window_to" --arg key "$key" \
    'select(.created_at >= $from and .created_at < $to) | $key' "$workdir/issues/$key.json" >>"$workdir/queries"
done <"$workdir/keys"
jq -r '.[] | .query' "$workdir/pairs.json" >>"$workdir/queries"
sort -u "$workdir/queries" -o "$workdir/queries"
readonly query_count="$(wc -l <"$workdir/queries")"

# The versions each issue needs: version 1 (the text it was filed with, which a query replays and
# which is also a candidate's text until its first edit), the latest created at or before the
# window's start, and every version created inside the window.
: >"$workdir/output.jsonl"
version_count=0
while read -r key; do
  row="$workdir/issues/$key.json"
  needed="$(jq -c --arg from "$window_from" --arg to "$window_to" '
    ([.versions[] | select(.created_at <= $from)] | last | if . == null then [] else [.number] end)
    + [.versions[] | select(.created_at > $from and .created_at < $to) | .number]
    + [.versions[] | select(.number == 1) | 1]
    | unique' "$row")"
  : >"$workdir/versions"
  for number in $(jq -r '.[]' <<<"$needed"); do
    markdown "$key" "$number" "$workdir/version"
    jq -c --rawfile markdown "$workdir/version" --argjson number "$number" \
      '{number: $number, created_at: (.versions[] | select(.number == $number) | .created_at), markdown: $markdown}' "$row" >>"$workdir/versions"
    version_count=$((version_count + 1))
  done
  jq -c --slurpfile versions "$workdir/versions" --slurpfile histories "$workdir/events/$key.histories" '
    {record: "issue", key, created_at, spec_artifact,
     status_history: $histories[0].status_history,
     parent_history: $histories[0].parent_history,
     title_history: $histories[0].title_history,
     ancestors: $histories[0].ancestors,
     spec_versions: $versions}' "$row" >>"$workdir/output.jsonl"
done <"$workdir/keys"
while read -r key; do
  jq -nc --arg key "$key" '{record: "query", key: $key}' >>"$workdir/output.jsonl"
done <"$workdir/queries"
jq -c '.[]' "$workdir/pairs.json" >>"$workdir/output.jsonl"
mv "$workdir/output.jsonl" "$output"

printf 'project: %s\n' "$project"
printf 'window: %s to %s\n' "$window_from" "$window_to"
printf 'issues exported: %d\n' "$issues_read"
printf 'creates replayed as queries: %d\n' "$query_count"
printf 'pairs named: %d\n' "$(jq 'length' "$workdir/pairs.json")"
printf 'spec versions exported: %d\n' "$version_count"
printf 'bytes: %d\n' "$(wc -c <"$output")"
printf 'sha256: %s\n' "$(sha256sum "$output" | cut -d' ' -f1)"
