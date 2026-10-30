#!/usr/bin/env bash
# Drives Dispatch's block graph end to end against a running server, as an agent sees it: a block
# reference in a comment and the block's backlinks through the API and `dispatch read`; a deleted
# and restored block; a pinned block reference that changes and is removed; links between the
# blocks of two documents; each block's changed version; every block part of its document; a link
# in a list item; who made each node of every kind and when, who last changed it and when, its
# parent and the parent's version, read by its own reference, as an edge's other end and on the
# `Node:` line of `dispatch read`; the older document references route; the blocks of comments
# and messages: a reply that names a block (A7), a mention that starts at a comment's block (A10),
# the facts of a comment's and a message's block (A11), a block's id kept through a person's edits
# (A13) and a pinned link to a comment's block (A15); and the parts of an ask — its question, each
# option and its answer — each named by its own reference and printed on the option's line by
# `dispatch read` (A6), carrying the facts of every node, a part the ask has dropped included
# (A11), pinned to a revision of the ask's parts (A15), holding a link of its own (R7), refused by
# name where the ask holds no such part (R10) and named by a reply in the ask's thread (R11).
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), and optionally COMPONENT_SOURCE
# (where the component nodes come from: `fixture` seeds the harness's fake GitHub, on
# FAKE_GITHUB_PORT, with a two-component model in acme/r12-arch, replacing every repository it
# served, and adds that repository as an architecture source of PROJECT; `existing` reads the first
# project with a component that has a parent, and writes nothing; `none` proves the server has no
# architecture source; unset, the script reads no component node and says so on one
# `SKIP: R12 component …` line) and DEV_SIGNIN_LOGIN (a login the server's dev sign-in route,
# /auth/_dev/signin, signs in: the local harness has it, a deployed server does not). Editing a
# comment is a person's write alone, so the A13 and A15 comment sections sign that person in with a
# cookie jar and edit the comments the person wrote; with DEV_SIGNIN_LOGIN unset each prints one
# `SKIP: … PATCH /api/v1/comments/{id} is human-only …` line and every other section runs; the same
# holds for the answer part of A6's section, whose answer only a person can write. It writes two
# issues in PROJECT, the second a child of the first, and four asks, three on the child and one on
# the first; it resolves every ask it can and closes both issues, also when a check fails, and it
# removes the source `fixture` added the same way.
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         [COMPONENT_SOURCE=fixture FAKE_GITHUB_PORT=9022] [DEV_SIGNIN_LOGIN=alice] block-graph.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN;
       COMPONENT_SOURCE=existing reads a component the server holds, COMPONENT_SOURCE=none proves
       it holds none, and neither takes FAKE_GITHUB_PORT; unset, no component node is read;
       DEV_SIGNIN_LOGIN, on a server with dev sign-in alone, runs the A13 and A15 comment edits as
       that person; unset, those two sections are skipped)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT
readonly component_source="${COMPONENT_SOURCE-}"
case "$component_source" in
  fixture)
    require_env FAKE_GITHUB_PORT
    [[ "$FAKE_GITHUB_PORT" =~ ^[0-9]+$ ]] || {
      printf 'FAKE_GITHUB_PORT is %s, not a port number\n' "$FAKE_GITHUB_PORT" >&2
      usage
      exit 2
    }
    ;;
  existing | none | "") ;;
  *)
    printf 'COMPONENT_SOURCE is %s; want fixture, existing or none\n' "$component_source" >&2
    usage
    exit 2
    ;;
esac
readonly dev_login="${DEV_SIGNIN_LOGIN-}"
require_commands base64 bun curl jq
read_bearer
# No child process inherits the bearer from here: read_bearer has unset it from the environment,
# curl reads it from a header file, and the CLI gets it in an environment this script builds.

readonly base="${DISPATCH_URL%/}"
# The dashboard's origin, the scheme and host of DISPATCH_URL: a write the person's cookie
# authenticates must name it (enforceCookieOrigin).
base_host="${base#*://}"
readonly origin="${base%%://*}://${base_host%%/*}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
script_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
readonly script_dir
readonly cli_entry="$script_dir/../../../envoy-client/bin/dispatch.ts"
bun_bin="$(command -v bun)"
readonly bun_bin
readonly saved_path="$PATH"
readonly saved_tmpdir="${TMPDIR:-}"
session="block-graph-$(date +%s)-$$"
readonly session

umask 077
work="$(mktemp -d)"
readonly work
mkdir "$work/home" "$work/state"
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

key=""
closed=0
child=""
child_closed=0
ask=""
ask_resolved=0
source_path=""
# close_written ISSUE: closes ISSUE, an issue this script wrote, from cleanup.
close_written() {
  cli issue-update --issue "$1" --status "done" \
    --reason "block-graph.sh stopped before its last check; closing the issue it wrote." \
    >"$work/cleanup" 2>&1 ||
    printf 'block-graph.sh: could not close %s: %s\n' "$1" "$(<"$work/cleanup")" >&2
}
cleanup() {
  local code=$?
  if [[ -n "$ask" && "$ask_resolved" == 0 ]]; then
    cli resolve-ask --ask "$ask" --kind resolved \
      --reason "block-graph.sh stopped before its last check; resolving the ask it wrote." \
      >"$work/cleanup" 2>&1 ||
      printf 'block-graph.sh: could not resolve ask %s: %s\n' "$ask" "$(<"$work/cleanup")" >&2
  fi
  if [[ -n "$child" && "$child_closed" == 0 ]]; then
    close_written "$child"
  fi
  if [[ -n "$key" && "$closed" == 0 ]]; then
    close_written "$key"
  fi
  if [[ -n "$source_path" ]]; then
    send DELETE "$source_path" "$(jq -cn --arg s "$session" '{actor: {kind: "session", id: $s}}')"
    [[ "$status" == 2?? ]] ||
      printf 'block-graph.sh: could not remove the architecture source %s: HTTP %s: %s\n' \
        "$source_path" "$status" "$(body)" >&2
  fi
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}

# The real `dispatch` CLI as an agent session runs it, in an environment holding only what this
# driver names: nothing a calling shell exported (its Dispatch URL, token file or Envoy config)
# reaches it.
cli() {
  (
    set +x
    local name
    local -a exported
    mapfile -t exported < <(compgen -e)
    for name in "${exported[@]}"; do
      unset "$name" 2>/dev/null || true
    done
    export PATH="$saved_path" HOME="$work/home" DISPATCH_STATE_DIR="$work/state"
    if [[ -n "$saved_tmpdir" ]]; then
      export TMPDIR="$saved_tmpdir"
    fi
    export DISPATCH_HOST=omp DISPATCH_SESSION_ID="$session" DISPATCH_URL="$base"
    export DISPATCH_TOKEN="$token"
    exec "$bun_bin" "$cli_entry" "$@"
  )
}

# run SECTION WHAT ARGS...: runs the CLI with ARGS, its output in $work/out; a non-zero exit fails.
run() {
  local section="$1" what="$2"
  shift 2
  cli "$@" >"$work/out" 2>&1 || fail "$section" "$what" "dispatch $1 exited $?: $(<"$work/out")"
}

# posted SECTION WHAT KIND: sets $posted to the id of the KIND (comment or message) the last run
# of `dispatch comment` or `dispatch message` posted.
posted() {
  [[ "$(<"$work/out")" =~ Posted\ $3\ ([0-9a-f-]{36}) ]] ||
    fail "$1" "$2" "no $3 id in: $(<"$work/out")"
  posted="${BASH_REMATCH[1]}"
}

q() { jq -rn --arg v "$1" '$v|@uri'; }

# request AS METHOD PATH [JSON]: sends METHOD PATH as AS: `agent`, with the bearer in $work/auth,
# or `person`, with the session cookie the dev sign-in left in $work/jar and the dashboard's origin,
# which the server requires of every write a cookie authenticates. The answer's body goes to
# $work/body and its HTTP status to $status.
request() {
  local -a auth data=()
  if [[ "$1" == person ]]; then
    auth=(-b "$work/jar" -H "Origin: $origin")
  else
    auth=(-H "@$work/auth")
  fi
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  status="$(curl -sS -X "$2" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$base$3" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# send METHOD PATH [JSON]: request as the agent.
send() { request agent "$@"; }
# person_send METHOD PATH [JSON]: request as the person DEV_SIGNIN_LOGIN signed in.
person_send() { request person "$@"; }

# get PATH: send GET PATH.
get() { send GET "$1"; }
body() { head -c 600 "$work/body"; }

# want_status CODE SECTION WHAT: the last answer's status is CODE.
want_status() {
  [[ "$status" == "$1" ]] || fail "$2" "$3" "HTTP $status, want $1: $(body)"
}

# expect SECTION WHAT WANT JQ-ARGS...: jq -cS JQ-ARGS over the last answer's body prints WANT.
expect() {
  local section="$1" what="$2" want="$3" got
  shift 3
  got="$(jq -cS "$@" "$work/body" 2>&1)" || fail "$section" "$what" "jq: $got; body: $(body)"
  [[ "$got" == "$want" ]] || fail "$section" "$what" "got $got, want $want"
  pass "$section" "$what"
}

# block_ref DOCUMENT ID: a block reference. Fixture ids hold only [A-Za-z0-9_-], and of those
# encodeBlockId writes only `_` encoded, as %5F.
block_ref() { printf '%s#b-%s' "$1" "${2//_/%5F}"; }

# doc_edit SECTION WHAT ARTIFACT OPS: applies OPS through `dispatch doc-edit`; sets $version to the
# version the edit wrote.
doc_edit() {
  run "$1" "$2" doc-edit --issue "$key" --artifact "$3" --ops-json "$4"
  [[ "$(<"$work/out")" =~ Applied\ [0-9]+\ ops\ \(version\ ([0-9]+)\) ]] ||
    fail "$1" "$2" "no version in: $(<"$work/out")"
  version="${BASH_REMATCH[1]}"
}

# upload SECTION NAME FILE [ISSUE]: uploads FILE as the document NAME of ISSUE (default $key)
# through `dispatch artifact`; sets $doc_ref and $doc_id.
upload() {
  run "$1" "upload $2" artifact --issue "${4:-$key}" --name "$2" --path "$3"
  [[ "$(<"$work/out")" =~ (dispatch://[^\);[:space:]]+)\) ]] ||
    fail "$1" "upload $2" "no reference in: $(<"$work/out")"
  doc_ref="${BASH_REMATCH[1]}"
  get "/api/v1/references?to=$(q "$doc_ref")"
  want_status 200 "$1" "read $doc_ref"
  doc_id="$(jq -er '.node.id' "$work/body")" || fail "$1" "read $doc_ref" "no node id: $(body)"
}

# block_ids SECTION SLUG: writes {block's markdown text: block id} for the issue document SLUG to
# $work/ids.json. GET …/blocks gives each block's markdown range; the fixtures are ASCII, so jq's
# character offsets are the server's byte offsets.
block_ids() {
  get "/api/v1/issues/$key/artifacts/$2/text"
  want_status 200 "$1" "read $2's text"
  cp "$work/body" "$work/text.json"
  get "/api/v1/issues/$key/artifacts/$2/blocks"
  want_status 200 "$1" "read $2's blocks"
  jq --slurpfile text "$work/text.json" \
    '$text[0].markdown as $md | map({key: $md[.from:.to], value: .id}) | from_entries' \
    "$work/body" >"$work/ids.json" || fail "$1" "read $2's blocks" "unreadable: $(body)"
}

# id_for SECTION TEXT: sets $block_id to the id of the block whose markdown is TEXT.
id_for() {
  block_id="$(jq -r --arg text "$2" '.[$text] // empty' "$work/ids.json")" ||
    fail "$1" "find the block \"$2\"" "unreadable ids"
  [[ -n "$block_id" ]] || fail "$1" "find the block \"$2\"" "blocks are $(<"$work/ids.json")"
}

# changed_versions SECTION DOCUMENT: writes {block id: changed_version} of every block part of
# DOCUMENT to $work/body.
changed_versions() {
  get "/api/v1/references?to=$(q "$2")&kind=part_of"
  want_status 200 "$1" "read the blocks part of $2"
  jq -cS '[.edges[] | {key: .node.block_id, value: .node.changed_version}] | from_entries' \
    "$work/body" >"$work/versions.json" || fail "$1" "read the blocks part of $2" "$(body)"
  mv "$work/versions.json" "$work/body"
}

callout() { printf ':::callout{#%s kind="note" title=""}\n%s\n:::' "$1" "$2"; }

# Setup: the issue whose spec holds X (`ship_it`) and two more callouts.
printf '%s\n\n%s\n\n%s\n' \
  "$(callout ship_it 'Ship the block graph.')" \
  "$(callout hold_line 'Hold the line on the graph.')" \
  "$(callout third_way 'A third way stays open.')" >"$work/spec.md"
run setup "create the issue" issue --project "$project" --title "Block graph check" \
  --spec-file "$work/spec.md" --force
[[ "$(<"$work/out")" =~ Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
  fail setup "create the issue" "no issue key in: $(<"$work/out")"
key="${BASH_REMATCH[1]}"
pass setup "created $key in $project"
spec="dispatch://$key/spec"
x="$(block_ref "$spec" ship_it)"
[[ "$x" == "dispatch://$key/spec#b-ship%5Fit" ]] || fail setup "X's reference" "got $x"

# A1: a comment citing X is an edge to X alone; a comment citing the whole spec is not.
run A1 "comment citing X" comment --issue "$key" --body "See $x"
[[ "$(<"$work/out")" =~ Posted\ comment\ ([0-9a-f-]{36}) ]] ||
  fail A1 "comment citing X" "no comment id in: $(<"$work/out")"
comment="${BASH_REMATCH[1]}"
comment_ref="dispatch://$key/comment/$comment"
run A1 "comment citing the spec" comment --issue "$key" --body "See $spec"
[[ "$(<"$work/out")" =~ Posted\ comment\ ([0-9a-f-]{36}) ]] ||
  fail A1 "comment citing the spec" "no comment id in: $(<"$work/out")"
spec_comment="${BASH_REMATCH[1]}"

get "/api/v1/references?to=$(q "$x")"
want_status 200 A1 "?to=X answers"
expect A1 "?to=X names block ship_it by its encoded reference" \
  "$(jq -cn --arg r "$x" '["block","ship_it",$r]')" '[.node.kind, .node.block_id, .node.ref]'
# A mention in a comment starts at the comment's block holding it (R7): the source is a node of
# kind block, id comment/<uuid>#b<n>, whose parent is the comment.
expect A1 "?to=X lists block b1 of the comment citing X alone, part of the comment" \
  "$(jq -cn --arg c "$comment" '[["block", "comment/\($c)#b1", $c]]')" \
  '[.edges[] | select(.kind == "mentions") | [.node.kind, .node.id, .node.parent.id]]'

get "/api/v1/references?from=$(q "$comment_ref")"
want_status 200 A1 "?from=<comment> answers"
expect A1 "?from=<comment> has exactly one mentions edge, to block X" \
  "$(jq -cn --arg r "$x" '[["block",$r]]')" \
  '[.edges[] | select(.kind == "mentions") | [.node.kind, .node.ref]]'
expect A1 "?from=<comment> has no edge to the whole document" 0 \
  '[.edges[] | select(.node.kind == "artifact")] | length'

get "/api/v1/references?to=$(q "$spec")"
want_status 200 A1 "?to=<spec> answers"
expect A1 "?to=<spec> lists the comment citing X, from its block b1, with .block.block_id ship_it" \
  '[["block","ship_it"]]' \
  --arg c "$comment" '[.edges[] | select(.node.parent.id == $c) | [.node.kind, .block.block_id]]'
expect A1 "?to=<spec> lists the comment citing the spec, from its block b1, with no block" \
  "$(jq -cn --arg c "$spec_comment" '[["comment/\($c)#b1", null]]')" \
  --arg c "$spec_comment" '[.edges[] | select(.node.parent.id == $c) | [.node.id, .block]]'
spec_id="$(jq -er '.node.id' "$work/body")" || fail A1 "?to=<spec> names the spec" "$(body)"

run A1 "dispatch read --ref X" read --ref "$x"
grep -qF "comment $comment_ref block b1 " "$work/out" ||
  fail A1 "dispatch read --ref X lists block b1 of the comment" "$(<"$work/out")"
pass A1 "dispatch read --ref X lists block b1 of the comment"
grep -q '^Changed in version: [0-9]' "$work/out" ||
  fail A1 "dispatch read --ref X prints Changed in version" "$(<"$work/out")"
pass A1 "dispatch read --ref X prints Changed in version"

# A2: deleting X drops every link to it; restoring X with its id brings each one back. cites.md
# cites only X, so block-links from it to the spec holds the one link to X.
printf 'This document cites %s\n' "$x" >"$work/cites.md"
upload A2 cites.md "$work/cites.md"
cites="$doc_ref"
cites_id="$doc_id"
get "/api/v1/block-links?from=$(q "$cites")&to=$(q "$spec")"
want_status 200 A2 "block-links cites.md -> spec answers"
expect A2 "block-links cites.md -> spec holds the link to X" '["ship_it"]' '[.links[].to.block_id]'
# A comment's block is named by its kind, its id and its comment, a block node of cites.md by its
# document's reference.
x_sources="$(jq -cn --arg c "$comment" --arg d "$cites" '["block comment/\($c)#b1 of \($c)", $d] | sort')"
readonly source_of='.node.document // "\(.node.kind) \(.node.id) of \(.node.parent.id)"'
get "/api/v1/references?to=$(q "$x")"
want_status 200 A2 "?to=X answers"
expect A2 "?to=X lists the comment's block and cites.md's block" "$x_sources" \
  "[.edges[] | select(.kind == \"mentions\") | $source_of] | sort"

doc_edit A2 "delete X" spec '[{"op":"delete","block":"ship_it"}]'
get "/api/v1/references?to=$(q "$x")"
want_status 404 A2 "?to=X of the deleted block"
pass A2 "?to=X of the deleted block is 404"
get "/api/v1/references?from=$(q "$comment_ref")"
want_status 200 A2 "?from=<comment> answers"
expect A2 "?from=<comment> has no edge after the delete" 0 '.edges | length'
get "/api/v1/block-links?from=$(q "$cites")&to=$(q "$spec")"
want_status 200 A2 "block-links answers"
expect A2 "block-links cites.md -> spec has no link after the delete" 0 '.links | length'

doc_edit A2 "restore X with its id" spec \
  "$(jq -cn --arg m "$(callout ship_it 'Ship the block graph.')" \
    '[{op: "insert", before: "block:hold_line", markdown: $m}]')"
get "/api/v1/references?to=$(q "$x")"
want_status 200 A2 "?to=X of the restored block"
expect A2 "?to=X lists the comment's block and cites.md's block again" "$x_sources" \
  "[.edges[] | select(.kind == \"mentions\") | $source_of] | sort"
get "/api/v1/references?from=$(q "$comment_ref")"
want_status 200 A2 "?from=<comment> answers"
expect A2 "?from=<comment> lists its edge to X again" "$(jq -cn --arg r "$x" '[["mentions",$r]]')" \
  '[.edges[] | [.kind, .node.ref]]'
get "/api/v1/block-links?from=$(q "$cites")&to=$(q "$spec")"
want_status 200 A2 "block-links answers"
expect A2 "block-links cites.md -> spec lists the link to X again" '["ship_it"]' \
  '[.links[].to.block_id]'

# Pinned A2 and A15: X pinned at version N reads as changed after a reword, then as removed.
get "/api/v1/issues/$key/artifacts/spec/text"
want_status 200 A2/A15 "read the spec's version"
pinned_version="$(jq -er '.version' "$work/body")" || fail A2/A15 "read the spec's version" "$(body)"
pin="$x@v$pinned_version"
get "/api/v1/references?to=$(q "$x")"
want_status 200 A2/A15 "?to=X answers"
old_text="$(jq -er '.node.text' "$work/body")" || fail A2/A15 "read X's text" "$(body)"
[[ "$old_text" == *"Ship the block graph."* ]] || fail A2/A15 "read X's text" "got $old_text"
new_text="${old_text/Ship the block graph./Ship the block graph today.}"

doc_edit A2/A15 "reword X" spec \
  '[{"op":"replace","find":"Ship the block graph.","with":"Ship the block graph today."}]'
get "/api/v1/references?to=$(q "$pin")"
want_status 200 A2/A15 "?to=X@vN answers after the reword"
expect A2/A15 "?to=X@vN has pin.state changed at version N" \
  "$(jq -cn --argjson n "$pinned_version" '["changed",$n]')" '[.node.pin.state, .node.pin.version]'
expect A2/A15 "?to=X@vN has pin.text the old text" "$(jq -cn --arg t "$old_text" '$t')" \
  '.node.pin.text'
expect A2/A15 "?to=X@vN has the unpinned text the new text" "$(jq -cn --arg t "$new_text" '$t')" \
  '.node.text'

doc_edit A2/A15 "delete X" spec '[{"op":"delete","block":"ship_it"}]'
removed_in="$version"
get "/api/v1/references?to=$(q "$pin")"
want_status 200 A2/A15 "?to=X@vN answers after the delete"
expect A2/A15 "?to=X@vN has pin.state removed with removed_version $removed_in" \
  "[\"removed\",$removed_in]" '[.node.pin.state, .node.pin.removed_version]'
expect A2/A15 "?to=X@vN lists no edge for the removed block" 0 '.edges | length'
run A2/A15 "dispatch read --ref X@vN exits 0" read --ref "$pin"
pass A2/A15 "dispatch read --ref X@vN exits 0"
grep -q "^Removed in version $removed_in" "$work/out" ||
  fail A2/A15 "dispatch read --ref X@vN prints Removed in version" "$(<"$work/out")"
pass A2/A15 "dispatch read --ref X@vN prints Removed in version $removed_in"

# A4: decisions.md's two blocks cite two spec blocks; one spec block cites decisions.md's first
# block. block-links lists exactly those links, each in its own direction.
decisions_slug_ref="dispatch://$key/artifact/decisions-md"
printf '%s\n\n%s\n' \
  "$(callout first_call "We hold the line: $(block_ref "$spec" hold_line)")" \
  "$(callout second_call "We take the third way: $(block_ref "$spec" third_way)")" \
  >"$work/decisions.md"
upload A4 decisions.md "$work/decisions.md"
decisions="$doc_ref"
decisions_id="$doc_id"
[[ "$decisions" == "$decisions_slug_ref" ]] || fail A4 "decisions.md's reference" "got $decisions"
doc_edit A4 "cite decisions.md's first block from the spec" spec \
  "$(jq -cn --arg with "A third way stays open: $(block_ref "$decisions" first_call)" \
    '[{op: "replace", find: "A third way stays open.", with: $with}]')"

block_ids A4 decisions-md
id_for A4 "We hold the line: $(block_ref "$spec" hold_line)"
hold_from="$block_id"
id_for A4 "We take the third way: $(block_ref "$spec" third_way)"
third_from="$block_id"
block_ids A4 spec
id_for A4 "A third way stays open: $(block_ref "$decisions" first_call)"
back_from="$block_id"

get "/api/v1/block-links?from=$(q "$decisions")&to=$(q "$spec")"
want_status 200 A4 "block-links decisions.md -> spec answers"
expect A4 "block-links decisions.md -> spec has exactly the two links" \
  "$(jq -cn --arg a "$hold_from" --arg b "$third_from" '[[$a,"hold_line"],[$b,"third_way"]] | sort')" \
  '[.links[] | [.from.block_id, .to.block_id]] | sort'
get "/api/v1/block-links?from=$(q "$spec")&to=$(q "$decisions")"
want_status 200 A4 "block-links spec -> decisions.md answers"
expect A4 "block-links spec -> decisions.md has exactly the one link" \
  "$(jq -cn --arg a "$back_from" '[[$a,"first_call"]]')" \
  '[.links[] | [.from.block_id, .to.block_id]] | sort'

# A5: an edit to the paragraph in the callout moves that paragraph and the callout alone; an edit
# to Q moves Q alone. Every block's changed version is compared, not one.
printf '%s\n\nParagraph Q stands alone.\n\nParagraph R stands last.\n' \
  "$(callout callout_p 'Paragraph P sits in the callout.')" >"$work/changes.md"
upload A5 changes.md "$work/changes.md"
changes="$doc_ref"
changes_id="$doc_id"
block_ids A5 changes-md
id_for A5 "Paragraph P sits in the callout."
p="$block_id"
id_for A5 "Paragraph Q stands alone."
q_id="$block_id"
id_for A5 "Paragraph R stands last."
r="$block_id"
get "/api/v1/issues/$key/artifacts/changes-md/text"
want_status 200 A5 "read changes.md's version"
first="$(jq -er '.version' "$work/body")" || fail A5 "read changes.md's version" "$(body)"
versions() {
  jq -cSn --arg p "$p" --arg q "$q_id" --arg r "$r" \
    --argjson c "$1" --argjson pv "$2" --argjson qv "$3" --argjson rv "$4" \
    '{callout_p: $c, ($p): $pv, ($q): $qv, ($r): $rv}'
}
changed_versions A5 "$changes"
expect A5 "every block of the new changes.md is at version $first" \
  "$(versions "$first" "$first" "$first" "$first")" '.'

doc_edit A5 "edit paragraph P" changes.md \
  '[{"op":"replace","find":"P sits in the callout.","with":"P sits in the callout, edited."}]'
p_version="$version"
changed_versions A5 "$changes"
expect A5 "after P's edit P and the callout are at $p_version, Q and R at $first" \
  "$(versions "$p_version" "$p_version" "$first" "$first")" '.'

doc_edit A5 "edit paragraph Q" changes.md \
  '[{"op":"replace","find":"Q stands alone.","with":"Q stands alone, edited."}]'
q_version="$version"
changed_versions A5 "$changes"
expect A5 "after Q's edit Q alone moves to $q_version" \
  "$(versions "$p_version" "$p_version" "$q_version" "$first")" '.'

# R6/A10: every block of each document is part of it, and nothing else is.
for document in "spec $spec $spec_id" "cites.md $cites $cites_id" \
  "decisions.md $decisions $decisions_id" "changes.md $changes $changes_id"; do
  read -r name ref id <<<"$document"
  get "/api/v1/artifacts/$id/blocks"
  want_status 200 R6/A10 "read $name's blocks"
  jq -c '[.[].id] | sort' "$work/body" >"$work/listed.json" || fail R6/A10 "read $name's blocks" "$(body)"
  [[ "$(<"$work/listed.json")" != "[]" ]] || fail R6/A10 "$name has blocks" "GET …/blocks is []"
  get "/api/v1/references?to=$(q "$ref")&kind=part_of"
  want_status 200 R6/A10 "?to=$name&kind=part_of answers"
  expect R6/A10 "$name's part_of set equals its /blocks ids" "$(<"$work/listed.json")" \
    '[.edges[] | select(.kind == "part_of") | .node.block_id] | sort'
done

# R7: a mention in a list item starts at the item's paragraph, not at the list.
printf -- '- The first item cites nothing.\n- The second item cites %s\n' "$spec" >"$work/list.md"
upload R7 list.md "$work/list.md"
list="$doc_ref"
block_ids R7 list-md
id_for R7 "The second item cites $spec"
item_paragraph="$block_id"
get "/api/v1/issues/$key/artifacts/list-md/blocks"
want_status 200 R7 "read list.md's blocks"
expect R7 "the second item's text is a paragraph block" '["paragraph"]' \
  --arg id "$item_paragraph" '[.[] | select(.id == $id) | .type]'
get "/api/v1/references?from=$(q "$list")"
want_status 200 R7 "?from=list.md answers"
expect R7 "the mention in the second item starts at the item's paragraph" \
  "$(jq -cn --arg id "$item_paragraph" '[$id]')" \
  --arg spec "$spec" '[.edges[] | select(.kind == "mentions" and .node.ref == $spec) | .block.block_id]'

# R12/A11: every node of every kind carries who made it and when, who last changed it and when,
# its parent and the version of the parent it belongs to: read by its own reference, as the other
# end of an edge, and on the `Node:` line of `dispatch read`. `facts` projects a node's six facts,
# each actor as its kind and id and each time as whether it is set; `same` is what an edge's other
# end shares with the node its own reference reads; `instant` is a timestamp's epoch seconds.
readonly defs='
  def facts: {made_by: [.made_by[] | {kind, id}], made_at: (.made_at != null),
    changed_by: [.changed_by[] | {kind, id}], changed_at: (.changed_at != null),
    parent, parent_version};
  def same: {kind, id, made_by, made_at, changed_by, changed_at, parent, parent_version};
  def instant: capture("^(?<s>[^.Z+]+)(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
    | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | tonumber)
      - (if .z == "Z" then 0
         else (.z[0:1] + "1" | tonumber) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60)
         end);
'
me="$(jq -cn --arg s "$session" '[{kind: "session", id: $s}]')"

# parent_of KIND ID [REF]: a node's parent.
parent_of() {
  jq -cn --arg kind "$1" --arg id "$2" --arg ref "${3-}" \
    '{kind: $kind, id: $id} + (if $ref == "" then {} else {ref: $ref} end)'
}

# wanted MADE_BY CHANGED_BY PARENT VERSION: the facts of a node whose two times are set.
wanted() {
  jq -cnS --argjson made "$1" --argjson changed "$2" --argjson parent "$3" --argjson version "$4" \
    '{made_by: $made, made_at: true, changed_by: $changed, changed_at: true, parent: $parent,
      parent_version: $version}'
}

# node_facts WHAT REF WANT [SECTION]: ?to=REF's node carries the facts WANT; the answer stays in
# $work/body. SECTION names the checks, R12 when absent.
node_facts() {
  local section="${4:-R12}"
  get "/api/v1/references?to=$(q "$2")"
  want_status 200 "$section" "?to=$1 answers"
  expect "$section" "$1 carries its facts" "$3" "$defs .node | facts"
}

# changed_later WHAT: the node node_facts last read was changed after it was made.
changed_later() {
  expect R12 "$1 is later than its making" true \
    "$defs (.node.changed_at | instant) > (.node.made_at | instant)"
}

# node_of SECTION REF: sets $node to the `same` projection of ?to=REF's node.
node_of() {
  get "/api/v1/references?to=$(q "$2")"
  want_status 200 "$1" "?to=$2 answers"
  node="$(jq -cS "$defs .node | same" "$work/body")" || fail "$1" "read ?to=$2's node" "$(body)"
}

# node_line SECTION WHAT PREFIX MIDDLE SUFFIX: the last CLI output holds one `Node:` line, and it
# is PREFIX, then MIDDLE somewhere, and ends with SUFFIX; each part matched as written.
node_line() {
  local line
  line="$(grep '^Node: ' "$work/out")" || fail "$1" "$2" "no Node: line in: $(<"$work/out")"
  [[ "$line" != *$'\n'* ]] || fail "$1" "$2" "more than one Node: line in: $(<"$work/out")"
  [[ "$line" == "$3"*"$4"*"$5" ]] || fail "$1" "$2" "got $line"
  pass "$1" "$2"
}

# Setup beside $key: a child issue, retitled; an ask on it, reworded; a comment on a quote of its
# spec and a reply; a message and a reply; a file and a picture.
printf 'Child spec paragraph for the facts check.\n\nSecond paragraph.\n' >"$work/child.md"
run R12 "create the child issue" issue --project "$project" --title "Block graph facts check" \
  --parent "$key" --spec-file "$work/child.md" --force
[[ "$(<"$work/out")" =~ Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
  fail R12 "create the child issue" "no issue key in: $(<"$work/out")"
child="${BASH_REMATCH[1]}"
pass R12 "created $child under $key"
child_ref="dispatch://$child"
child_spec="$child_ref/spec"
run R12 "retitle $child" issue-update --issue "$child" --title "Block graph facts check, retitled"

recommendation="Should it stay open? Recommendation: no, the check resolves it when it ends."
run R12 "ask on $child" ask --issue "$child" \
  --question "The block graph check reads this ask's facts. $recommendation"
[[ "$(<"$work/out")" =~ Asked\ ([0-9a-f-]{36}) ]] ||
  fail R12 "ask on $child" "no ask id in: $(<"$work/out")"
ask="${BASH_REMATCH[1]}"
ask_ref="$child_ref/ask/$ask"
run R12 "reword the ask" edit-ask --ask "$ask" \
  --question "The block graph check reads this ask's facts, reworded. $recommendation"

get "/api/v1/issues/$child/artifacts/spec/text"
want_status 200 R12 "read $child's spec version"
anchored_version="$(jq -er '.version' "$work/body")" ||
  fail R12 "read $child's spec version" "$(body)"
run R12 "comment on a quote of $child's spec" comment --issue "$child" --artifact spec \
  --quote "Child spec paragraph" --body "Why this paragraph?"
[[ "$(<"$work/out")" =~ Posted\ comment\ ([0-9a-f-]{36}) ]] ||
  fail R12 "comment on a quote of $child's spec" "no comment id in: $(<"$work/out")"
head="${BASH_REMATCH[1]}"
head_ref="$child_ref/comment/$head"
run R12 "reply to the comment" comment --issue "$child" --reply-to "$head" \
  --body "Because the check reads it."
[[ "$(<"$work/out")" =~ Posted\ comment\ ([0-9a-f-]{36}) ]] ||
  fail R12 "reply to the comment" "no comment id in: $(<"$work/out")"
reply_ref="$child_ref/comment/${BASH_REMATCH[1]}"

run R12 "message on $child" message --issue "$child" --body "The block graph check posted this."
[[ "$(<"$work/out")" =~ Posted\ message\ ([0-9a-f-]{36}) ]] ||
  fail R12 "message on $child" "no message id in: $(<"$work/out")"
message="${BASH_REMATCH[1]}"
message_ref="$child_ref/message/$message"
run R12 "reply to the message" message --issue "$child" --in-reply-to "$message_ref" \
  --body "The block graph check replied to its own message."
[[ "$(<"$work/out")" =~ Posted\ message\ ([0-9a-f-]{36}) ]] ||
  fail R12 "reply to the message" "no message id in: $(<"$work/out")"
reply_message_ref="$child_ref/message/${BASH_REMATCH[1]}"

printf 'plain notes\n' >"$work/notes.txt"
upload R12 notes.txt "$work/notes.txt" "$child"
file_ref="$doc_ref"
# A 1x1 PNG.
base64 -d >"$work/pixel.png" <<<'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=='
upload R12 pixel.png "$work/pixel.png" "$child"
image_ref="$doc_ref"

# Each node the setup wrote, read by its own reference.
of_child="$(parent_of issue "$child" "$child_ref")"
node_facts "$child" "$child_ref" \
  "$(wanted "$me" "$me" "$(parent_of issue "$key" "dispatch://$key")" null)"
changed_later "$child's last change, its retitle,"
node_facts "$child's spec" "$child_spec" "$(wanted "$me" "$me" "$of_child" null)"
child_spec_id="$(jq -er '.node.id' "$work/body")" || fail R12 "read $child's spec id" "$(body)"
node_facts "the file" "$file_ref" "$(wanted "$me" "$me" "$of_child" null)"
node_facts "the picture" "$image_ref" "$(wanted "$me" "$me" "$of_child" null)"
node_facts "the ask" "$ask_ref" "$(wanted "$me" "$me" "$of_child" null)"
changed_later "the ask's last change, its rewording,"
node_facts "the anchored comment" "$head_ref" \
  "$(wanted "$me" "$me" "$(parent_of artifact "$child_spec_id" "$child_spec")" "$anchored_version")"
node_facts "the reply" "$reply_ref" \
  "$(wanted "$me" "$me" "$(parent_of comment "$head" "$head_ref")" null)"
node_facts "the message" "$message_ref" "$(wanted "$me" "$me" "$of_child" null)"
node_facts "the reply message" "$reply_message_ref" \
  "$(wanted "$me" "$me" "$(parent_of message "$message" "$message_ref")" null)"

# Blocks where they still exist: X was deleted for good above, and is read by its removed pin.
node_facts "paragraph P of changes.md" "$(block_ref "$changes" "$p")" \
  "$(wanted "$me" "$me" "$(parent_of artifact "$changes_id" "$changes")" "$p_version")"
node_facts "paragraph R of changes.md" "$(block_ref "$changes" "$r")" \
  "$(wanted "$me" "$me" "$(parent_of artifact "$changes_id" "$changes")" "$first")"
expect R12 "paragraph R, never edited, was last changed when it was made" true \
  '.node.made_at == .node.changed_at'
node_facts "the removed pin $pin" "$pin" \
  "$(wanted "$me" "$me" "$(parent_of artifact "$spec_id" "$spec")" "$((removed_in - 1))")"

# The other end of an edge carries the node's own facts.
node_of R12 "$head_ref"
get "/api/v1/references?from=$(q "$reply_ref")"
want_status 200 R12 "?from=<reply> answers"
expect R12 "?from=<reply>'s replies_to node is the comment as its own reference reads it" \
  "[$node]" "$defs"'[.edges[] | select(.kind == "replies_to") | .node | same]'
get "/api/v1/references?from=$(q "$ask_ref")&kind=followed_by"
want_status 200 R12 "?from=<ask>&kind=followed_by answers"
expect R12 "the session following the ask has no recorded facts" \
  "$(jq -cnS --arg s "$session" '[{kind: "session", id: $s, made_by: [], made_at: null,
    changed_by: [], changed_at: null, parent: null, parent_version: null}]')" \
  "$defs"'[.edges[] | select(.kind == "followed_by") | .node | same]'
node_of R12 "$(block_ref "$list" "$item_paragraph")"
get "/api/v1/references?from=$(q "$list")"
want_status 200 R12 "?from=list.md answers"
expect R12 "R7's mention starts at a block carrying the block's own facts" "[$node]" \
  --arg spec "$spec" \
  "$defs"'[.edges[] | select(.kind == "mentions" and .node.ref == $spec) | .block | same]'
node_of R12 "$decisions"
decisions_node="$node"
node_of R12 "$spec"
spec_node="$node"
node_of R12 "$(block_ref "$spec" hold_line)"
hold_node="$node"
get "/api/v1/block-links?from=$(q "$decisions")&to=$(q "$spec")"
want_status 200 R12 "block-links decisions.md -> spec answers"
expect R12 "block-links' from is decisions.md as its own reference reads it" "$decisions_node" \
  "$defs .from | same"
expect R12 "block-links' to is the spec as its own reference reads it" "$spec_node" \
  "$defs .to | same"
expect R12 "every link's two ends carry all six facts" true \
  '[.links[] | .from, .to | has("made_by") and has("made_at") and has("changed_by")
    and has("changed_at") and has("parent") and has("parent_version")] | length == 4 and all'
expect R12 "the link to hold_line ends at the block as its own reference reads it" "[$hold_node]" \
  "$defs"'[.links[] | select(.to.block_id == "hold_line") | .to | same]'

# component_node TIMES: reads component $component of $component_project and its part_of edge to
# its parent $component_parent. TIMES is `recorded` when the import that projected it recorded its
# times, and `unrecorded` when it may predate them.
component_node() {
  local component_ref="dispatch://$component_project/component/$component"
  local component_parent_ref="dispatch://$component_project/component/$component_parent"
  local component_parent_node of_parent
  node_of R12 "$component_parent_ref"
  component_parent_node="$node"
  of_parent="$(parent_of component "$component_project/$component_parent" "$component_parent_ref")"
  get "/api/v1/references?to=$(q "$component_ref")"
  want_status 200 R12 "?to=$component_ref answers"
  if [[ "$1" == recorded ]]; then
    expect R12 "component $component carries its facts, its times recorded at the import" \
      "$(wanted '[]' '[]' "$of_parent" null)" "$defs .node | facts"
  else
    # A component imported before its times were recorded has them null.
    expect R12 "component $component carries its facts" \
      "$(jq -cnS --argjson parent "$of_parent" '{made_by: [], changed_by: [], parent: $parent,
        parent_version: null, times: [true, true]}')" \
      '.node | {made_by, changed_by, parent, parent_version,
        times: [has("made_at"), has("changed_at")]}'
  fi
  get "/api/v1/references?from=$(q "$component_ref")&kind=part_of"
  want_status 200 R12 "?from=$component_ref&kind=part_of answers"
  expect R12 "component $component is part of $component_parent as its own reference reads it" \
    "[$component_parent_node]" "$defs"'[.edges[] | select(.kind == "part_of") | .node | same]'
}

# Components, from the source COMPONENT_SOURCE names.
case "$component_source" in
  fixture)
    arch_repo="acme/r12-arch"
    jq -cn --arg repo "$arch_repo" '{($repo): {contents: "read", installation_id: 367, files: {
      "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n",
      "web.md": "---\ntitle: Web\nparent: platform\n---\nThe web front end.\n"}}}' \
      >"$work/repos.json"
    status="$(curl -sS -X PUT -o "$work/body" -w '%{http_code}' \
      -H 'Content-Type: application/json' --data-binary "@$work/repos.json" \
      "http://127.0.0.1:$FAKE_GITHUB_PORT/__fixture/repos" 2>"$work/curl")" ||
      status="curl failed: $(<"$work/curl")"
    want_status 200 R12 "seed the fake GitHub with $arch_repo"
    source_path="/api/v1/projects/$project/architecture-sources/$arch_repo"
    send PUT "$source_path" \
      "$(jq -cn --arg s "$session" '{branch: "main", actor: {kind: "session", id: $s}}')"
    want_status 200 R12 "add $arch_repo as an architecture source of $project"
    send POST "$source_path/sync"
    want_status 200 R12 "sync $arch_repo"
    expect R12 "the sync of $arch_repo imports its two components" '[null,2]' \
      '[.last_error, .components]'
    component_project="$project"
    component=web
    component_parent=platform
    component_node recorded
    ;;
  existing)
    get "/api/v1/settings/architecture-sources"
    want_status 200 R12 "list the architecture sources"
    projects="$(jq -r '[.[].project] | unique | .[]' "$work/body")" ||
      fail R12 "list the architecture sources" "$(body)"
    [[ -n "$projects" ]] || fail R12 component \
      "this server has no architecture source: COMPONENT_SOURCE=existing needs one"
    mapfile -t source_projects <<<"$projects"
    pick=""
    for component_project in "${source_projects[@]}"; do
      get "/api/v1/projects/$component_project/architecture"
      want_status 200 R12 "read $component_project's architecture"
      pick="$(jq -r '[.components[].id] as $ids
        | first(.components[] | select(.parent != null and (.parent | IN($ids[])))
          | "\(.id) \(.parent)") // empty' "$work/body")" ||
        fail R12 "read $component_project's architecture" "$(body)"
      [[ -z "$pick" ]] || break
    done
    [[ -n "$pick" ]] || fail R12 component "no project with an architecture source has a \
component with a parent: COMPONENT_SOURCE=existing needs one"
    read -r component component_parent <<<"$pick"
    component_node unrecorded
    ;;
  none)
    get "/api/v1/settings/architecture-sources"
    want_status 200 R12 "list the architecture sources"
    jq -e 'length == 0' "$work/body" >/dev/null || fail R12 component \
      "this server has architecture sources, so run with COMPONENT_SOURCE=existing: $(body)"
    pass R12 "component: this server has no architecture source, so it holds no component node"
    ;;
  "")
    printf 'SKIP: R12 component: COMPONENT_SOURCE is unset, so no component node is read; set it to fixture (with FAKE_GITHUB_PORT), existing or none to read one\n'
    ;;
esac

# The CLI prints the facts of the node it reads on its Node: line.
run R12 "dispatch read --ref <anchored comment>" read --ref "$head_ref"
# A session actor holding a service token prints `session <id> (as <service>)`, so each check
# leaves room for that label after the session id.
node_line R12 "dispatch read --ref <anchored comment> prints its facts" \
  "Node: comment $head_ref · made by session $session" " at " \
  " · part of $child_spec (version $anchored_version)"
run R12 "dispatch read --ref <paragraph P>" read --ref "$(block_ref "$changes" "$p")"
node_line R12 "dispatch read --ref <paragraph P> prints its facts" \
  "Node: block $(block_ref "$changes" "$p") · made by session $session" " at " \
  " · part of $changes (version $p_version)"
run R12 "dispatch read --issue $child" read --issue "$child"
node_line R12 "dispatch read --issue $child prints its facts" \
  "Node: issue $child_ref · made by session $session" " · changed by session $session" \
  " · part of dispatch://$key"

# The older references route keeps decisions.md, which cites only blocks of the spec, as a source.
get "/api/v1/artifacts/$spec_id/references"
want_status 200 older-route "GET /artifacts/<spec>/references answers"
expect older-route "referenced_by keeps decisions.md" 1 \
  --arg d "$decisions_id" '[.referenced_by[] | select(.kind == "artifact" and .id == $d)] | length'

# A7: a reply names the block it answers. A comment reply through `--reply-to <comment>#b-b2` is a
# replies_to edge to block b2, listed once, under b2, on ?to=<comment>, and `dispatch read` quotes
# the block; a reply to the whole comment is an edge to the comment and names no block. The same
# for a message reply through `--in-reply-to <message>#b-b2`.
agent_actor="$(jq -cn --arg s "$session" '{kind: "session", id: $s}')"
run A7 "comment X of two paragraphs" comment --issue "$key" --body $'Alpha.\n\nBeta.'
posted A7 "comment X of two paragraphs" comment
cx="$posted"
cx_ref="dispatch://$key/comment/$cx"
cx_b2="$cx_ref#b-b2"
run A7 "reply to X's block b2" comment --issue "$key" --reply-to "$cx_b2" --body "On Beta alone."
posted A7 "reply to X's block b2" comment
block_reply="$posted"
block_reply_ref="dispatch://$key/comment/$block_reply"
run A7 "reply to the whole of X" comment --issue "$key" --reply-to "$cx" --body "On all of X."
posted A7 "reply to the whole of X" comment
plain_reply="$posted"
plain_reply_ref="dispatch://$key/comment/$plain_reply"

get "/api/v1/references?to=$(q "$cx_b2")&kind=replies_to"
want_status 200 A7 "?to=X#b-b2&kind=replies_to answers"
expect A7 "?to=X#b-b2&kind=replies_to is the one reply naming b2" "[\"$block_reply\"]" \
  '[.edges[].node.id]'
get "/api/v1/references?to=$(q "$cx_ref")&kind=replies_to"
want_status 200 A7 "?to=X&kind=replies_to answers"
expect A7 "?to=X&kind=replies_to lists the reply naming b2 once, under b2, and the plain reply" \
  "$(jq -cn --arg b "$block_reply" --arg p "$plain_reply" '[[$b, "b2"], [$p, null]] | sort')" \
  '[.edges[] | [.node.id, .block.block_id]] | sort'
get "/api/v1/references?from=$(q "$plain_reply_ref")&kind=replies_to"
want_status 200 A7 "?from=<plain reply>&kind=replies_to answers"
expect A7 "the plain reply's replies_to edge goes to X itself" "[[\"comment\",\"$cx\"]]" \
  '[.edges[] | [.node.kind, .node.id]]'
get "/api/v1/comments/$block_reply"
want_status 200 A7 "read the reply naming b2"
expect A7 "the reply naming b2 carries reply_block b2 with its text" \
  "$(jq -cn --arg r "$cx_b2" '["b2", $r, "Beta.", false]')" \
  '.comment.reply_block | [.block_id, .ref, .text, .removed]'
get "/api/v1/comments/$plain_reply"
want_status 200 A7 "read the plain reply"
expect A7 "the plain reply's reply_block is null" "[\"$cx\",null]" \
  '[.comment.reply_to, .comment.reply_block]'
run A7 "dispatch read --ref <reply naming b2>" read --ref "$block_reply_ref"
grep -qxF 'Re: block b2: Beta.' "$work/out" ||
  fail A7 "dispatch read --ref <reply naming b2> prints Re: block b2: Beta." "$(<"$work/out")"
pass A7 "dispatch read --ref <reply naming b2> prints Re: block b2: Beta."
run A7 "dispatch read --ref <plain reply>" read --ref "$plain_reply_ref"
if grep -q '^Re: block' "$work/out"; then
  fail A7 "dispatch read --ref <plain reply> quotes no block" "$(<"$work/out")"
fi
pass A7 "dispatch read --ref <plain reply> quotes no block"

run A7 "message M of two paragraphs" message --issue "$key" --body $'Gamma.\n\nDelta.'
posted A7 "message M of two paragraphs" message
mx="$posted"
mx_ref="dispatch://$key/message/$mx"
mx_b2="$mx_ref#b-b2"
run A7 "message reply to M's block b2" message --issue "$key" --in-reply-to "$mx_b2" \
  --body "On Delta alone."
posted A7 "message reply to M's block b2" message
message_block_reply="$posted"
message_block_reply_ref="dispatch://$key/message/$message_block_reply"
run A7 "message reply to the whole of M" message --issue "$key" --in-reply-to "$mx_ref" \
  --body "On all of M."
posted A7 "message reply to the whole of M" message
message_plain_reply="$posted"

get "/api/v1/references?to=$(q "$mx_b2")&kind=replies_to"
want_status 200 A7 "?to=M#b-b2&kind=replies_to answers"
expect A7 "?to=M#b-b2&kind=replies_to is the one message reply naming b2" \
  "[\"$message_block_reply\"]" '[.edges[].node.id]'
get "/api/v1/references?to=$(q "$mx_ref")&kind=replies_to"
want_status 200 A7 "?to=M&kind=replies_to answers"
expect A7 "?to=M&kind=replies_to lists the reply naming b2 once, under b2, and the plain reply" \
  "$(jq -cn --arg b "$message_block_reply" --arg p "$message_plain_reply" \
    '[[$b, "b2", "block"], [$p, null, "message"]] | sort')" \
  '[.edges[] | [.node.id, .block.block_id, .node.parent.kind]] | sort'
get "/api/v1/messages/$mx"
want_status 200 A7 "read M's thread"
expect A7 "the message reply naming b2 carries reply_block b2, the plain one none" \
  "$(jq -cn --arg r "$mx_b2" '[["b2", $r, "Delta.", false], null]')" \
  --arg b "$message_block_reply" --arg p "$message_plain_reply" \
  '[(.replies[] | select(.id == $b) | .reply_block | [.block_id, .ref, .text, .removed]),
    (.replies[] | select(.id == $p) | .reply_block)]'
run A7 "dispatch read --ref <message reply naming b2>" read --ref "$message_block_reply_ref"
grep -qxF 'Re: block b2: Delta.' "$work/out" ||
  fail A7 "dispatch read --ref <message reply naming b2> prints Re: block b2: Delta." \
    "$(<"$work/out")"
pass A7 "dispatch read --ref <message reply naming b2> prints Re: block b2: Delta."

# Negative controls: a block the item does not hold is refused, by name.
send POST "/api/v1/issues/$key/comments" "$(jq -cn --arg x "$cx" --argjson a "$agent_actor" \
  '{body: "On a block X lacks.", reply_to: $x, reply_to_block: "b9", actor: $a}')"
want_status 400 A7 "a reply_to_block of b9"
expect A7 "a reply_to_block of b9 answers 400 INVALID_COMMENT naming b9" true \
  '.code == "INVALID_COMMENT" and (.error | contains("reply_to_block b9 "))'
send POST "/api/v1/issues/$key/messages" "$(jq -cn --arg m "$mx" --argjson a "$agent_actor" \
  '{body: "On a block M lacks.", in_reply_to: $m, in_reply_to_block: "b9", actor: $a}')"
want_status 400 A7 "an in_reply_to_block of b9"
expect A7 "an in_reply_to_block of b9 answers 400 INVALID_MESSAGE naming b9" true \
  '.code == "INVALID_MESSAGE" and (.error | contains("in_reply_to_block b9 "))'

# A10, the comment half: a link in a comment's second paragraph starts at that paragraph's block.
run "A10 comment" "comment citing the spec from its second paragraph" comment --issue "$key" \
  --body "$(printf 'This paragraph cites nothing.\n\nThis one cites %s' "$spec")"
posted "A10 comment" "comment citing the spec from its second paragraph" comment
citing="$posted"
get "/api/v1/references?from=$(q "dispatch://$key/comment/$citing")"
want_status 200 "A10 comment" "?from=<comment> answers"
expect "A10 comment" "?from=<comment> lists its mention of the spec, starting at block b2" \
  "$(jq -cn --arg s "$spec" --arg c "$citing" '[[$s, "b2", $c]]')" \
  '[.edges[] | select(.kind == "mentions") | [.node.ref, .block.block_id, .block.parent.id]]'
get "/api/v1/references?to=$(q "$spec")"
want_status 200 "A10 comment" "?to=<spec> answers"
expect "A10 comment" "?to=<spec> lists block b2 of the comment, part of the comment" \
  "$(jq -cn --arg c "$citing" '[["mentions", "block", "comment/\($c)#b2", "b2"]]')" \
  --arg c "$citing" \
  '[.edges[] | select(.node.parent.id == $c) | [.kind, .node.kind, .node.id, .node.block_id]]'

# A11, comment and message blocks: a block of a comment and of a message carries R12's facts, read
# by its own reference, as an edge's other end and on the `Node:` line. A comment's block is part
# of the comment at the revision its text last changed in; a message's is part of the message, which
# has no versions. A reply naming a block is part of that block.
node_facts "block b2 of comment X" "$cx_b2" \
  "$(wanted "$me" "$me" "$(parent_of comment "$cx" "$cx_ref")" 1)" A11
expect A11 "block b2 of comment X, never edited, was last changed when it was made" true \
  '.node.made_at == .node.changed_at'
node_facts "block b1 of message M" "$mx_ref#b-b1" \
  "$(wanted "$me" "$me" "$(parent_of message "$mx" "$mx_ref")" null)" A11
node_facts "the reply naming b2 of X" "$block_reply_ref" \
  "$(wanted "$me" "$me" "$(parent_of block "comment/$cx#b2" "$cx_b2")" null)" A11
node_facts "the message reply naming b2 of M" "$message_block_reply_ref" \
  "$(wanted "$me" "$me" "$(parent_of block "message/$mx#b2" "$mx_b2")" null)" A11

node_of A11 "$cx_b2"
get "/api/v1/references?from=$(q "$block_reply_ref")&kind=replies_to"
want_status 200 A11 "?from=<reply naming b2>&kind=replies_to answers"
expect A11 "?from=<reply naming b2>'s block end is b2 as its own reference reads it" "[$node]" \
  "$defs"'[.edges[] | select(.node.kind == "block") | .node | same]'
get "/api/v1/references?to=$(q "$cx_ref")&kind=part_of"
want_status 200 A11 "?to=X&kind=part_of answers"
expect A11 "?to=X&kind=part_of lists b2 as its own reference reads it" "[$node]" \
  "$defs"'[.edges[] | select(.node.block_id == "b2") | .node | same]'
node_of A11 "$mx_ref#b-b1"
get "/api/v1/references?to=$(q "$mx_ref")&kind=part_of"
want_status 200 A11 "?to=M&kind=part_of answers"
expect A11 "?to=M&kind=part_of lists b1 as its own reference reads it" "[$node]" \
  "$defs"'[.edges[] | select(.node.block_id == "b1") | .node | same]'

run A11 "dispatch read --ref <X#b-b2>" read --ref "$cx_b2"
grep -qxF "Block b2 of comment $cx (changed in revision 1):" "$work/out" ||
  fail A11 "dispatch read --ref <X#b-b2> names the block and its comment" "$(<"$work/out")"
pass A11 "dispatch read --ref <X#b-b2> names the block and its comment"
node_line A11 "dispatch read --ref <X#b-b2> prints its facts" \
  "Node: block $cx_b2 · made by session $session" " at " " · part of $cx_ref (revision 1)"
run A11 "dispatch read --ref <M#b-b1>" read --ref "$mx_ref#b-b1"
node_line A11 "dispatch read --ref <M#b-b1> prints its facts" \
  "Node: block $mx_ref#b-b1 · made by session $session" " at " " · part of $mx_ref"

# A13 and A15 edit comments a person wrote: PATCH /api/v1/comments/{id} is a person's write, by its
# author alone. DEV_SIGNIN_LOGIN signs the person in once, the session cookie kept in $work/jar.
if [[ -n "$dev_login" ]]; then
  status="$(curl -sS -o "$work/body" -w '%{http_code}' -c "$work/jar" \
    "$base/auth/_dev/signin?login=$(q "$dev_login")" 2>"$work/curl")" ||
    status="curl failed: $(<"$work/curl")"
  want_status 302 A13 "dev sign-in as $dev_login"
  grep -q $'\tdsession\t' "$work/jar" ||
    fail A13 "dev sign-in as $dev_login" "the answer set no session cookie"
  pass A13 "signed in as $dev_login"
fi

# person_comment SECTION WHAT BODY: the person writes a comment of BODY on $key; sets $posted to
# its id.
person_comment() {
  person_send POST "/api/v1/issues/$key/comments" "$(jq -cn --arg b "$3" '{body: $b}')"
  want_status 201 "$1" "$2"
  expect "$1" "$2, as $dev_login" "$(jq -cn --arg l "$dev_login" '["user", $l]')" \
    '[.author.kind, .author.id]'
  posted="$(jq -er '.id' "$work/body")" || fail "$1" "$2" "no comment id: $(body)"
}

# edit_comment SECTION WHAT COMMENT BODY STARTS: the person edits COMMENT to BODY as the dashboard's
# editor saves an edit: GET …/blocks gives the comment's revision, sent as base_revision, and each
# block's text, and every block id the JSON object STARTS names is carried to where that block
# starts in BODY, the first place BODY holds the text STARTS gives it or, given null, the block's
# text as GET …/blocks read it. The fixtures are ASCII, so jq's offsets are UTF-16 offsets. Leaves
# GET …/blocks after the edit in $work/body and the comment's new revision in $revision.
edit_comment() {
  get "/api/v1/comments/$3/blocks"
  want_status 200 "$1" "read comment $3's blocks before: $2"
  jq -ce --arg body "$4" --argjson starts "$5" '
    select(.indexed) | (.blocks | map({key: .id, value: .text}) | from_entries) as $texts
    | {body: $body, base_revision: .revision,
       blocks: ([$starts | to_entries[] | (.value // $texts[.key]) as $start
         | {id: .key, from: ($body | index($start))}] | sort_by(.from))}
    | select(all(.blocks[]; .from != null))' "$work/body" >"$work/edit.json" ||
    fail "$1" "$2" "cannot carry $5 into the new body from: $(body)"
  person_send PATCH "/api/v1/comments/$3" "$(<"$work/edit.json")"
  want_status 200 "$1" "$2"
  expect "$1" "$2: the comment holds the new body" "$(jq -cn --arg b "$4" '$b')" '.body'
  get "/api/v1/comments/$3/blocks"
  want_status 200 "$1" "read comment $3's blocks after: $2"
  revision="$(jq -er 'select(.indexed) | .revision' "$work/body")" ||
    fail "$1" "read comment $3's blocks after: $2" "not indexed: $(body)"
}

# reply_block_is WHAT TEXT REMOVED: the agent's A13 reply names block b2 of the person's comment,
# whose text is TEXT, and REMOVED (true or false) says whether no current revision holds it.
reply_block_is() {
  get "/api/v1/comments/$a13_reply"
  want_status 200 A13 "read the reply after $1"
  expect A13 "after $1 the reply names b2: \"$2\", removed $3" \
    "$(jq -cn --arg t "$2" --argjson r "$3" '["b2", $t, $r]')" \
    '.comment.reply_block | [.block_id, .text, .removed]'
}

# A13: the person's edits keep a cited paragraph's id. An agent's reply names b2 of the person's
# comment; the person moves b2 first, rewords it, splits it, deletes it and writes it back, each
# saved with the block ids carried as the dashboard's editor carries them, and after each the reply
# names b2 with its current text, or its last text and removed. A body holding block HTML is stored
# byte for byte, and block HTML added elsewhere leaves b2's changed revision where it was.
if [[ -z "$dev_login" ]]; then
  printf 'SKIP: A13 PATCH /api/v1/comments/{id} is human-only and this server has no dev sign-in: DEV_SIGNIN_LOGIN is unset; on a server with /auth/_dev/signin, set it to a login to run A13\n'
else
  person_comment A13 "the person writes a comment of three paragraphs" \
    $'First.\n\nSecond.\n\nThird.'
  person_cited="$posted"
  get "/api/v1/comments/$person_cited/blocks"
  want_status 200 A13 "read the comment's blocks"
  expect A13 "GET …/blocks gives revision 1 and each block's id, UTF-16 range and text" \
    '[1,[["b1",0,6,"First."],["b2",8,15,"Second."],["b3",17,23,"Third."]]]' \
    '[.revision, [.blocks[] | [.id, .from, .to, .text]]]'
  send POST "/api/v1/issues/$key/comments" \
    "$(jq -cn --arg c "$person_cited" --argjson a "$agent_actor" \
      '{body: "On the second paragraph.", reply_to: $c, reply_to_block: "b2", actor: $a}')"
  want_status 201 A13 "an agent replies to block b2 of the person's comment"
  a13_reply="$(jq -er '.id' "$work/body")" ||
    fail A13 "an agent replies to block b2 of the person's comment" "no comment id: $(body)"
  reply_block_is "the reply" "Second." false

  edit_comment A13 "move b2 first" "$person_cited" $'Second.\n\nFirst.\n\nThird.' \
    '{"b1": null, "b2": null, "b3": null}'
  expect A13 "the move keeps b2's text and its changed revision 1" '[["b2","Second.",1]]' \
    '[.blocks[] | select(.id == "b2") | [.id, .text, .changed_revision]]'
  reply_block_is "the move" "Second." false

  edit_comment A13 "reword b2" "$person_cited" $'Second, reworded.\n\nFirst.\n\nThird.' \
    '{"b1": null, "b2": "Second, reworded.", "b3": null}'
  expect A13 "the reword changes b2 in revision $revision" "[$revision]" \
    '[.blocks[] | select(.id == "b2") | .changed_revision]'
  reply_block_is "the reword" "Second, reworded." false

  # The split's carried from for b2 is its start, which stays in the first part.
  edit_comment A13 "split b2" "$person_cited" $'Second,\n\nreworded.\n\nFirst.\n\nThird.' \
    '{"b1": null, "b2": "Second,", "b3": null}'
  expect A13 "the split keeps b2 on its first part and gives the second a new id" \
    '[["b2","Second,"],["b4","reworded."],["b1","First."],["b3","Third."]]' \
    '[.blocks[] | [.id, .text]]'
  reply_block_is "the split" "Second," false

  edit_comment A13 "delete b2" "$person_cited" $'reworded.\n\nFirst.\n\nThird.' \
    '{"b1": null, "b3": null, "b4": null}'
  expect A13 "the deletion leaves no block b2" '[]' '[.blocks[] | select(.id == "b2")]'
  reply_block_is "the deletion" "Second," true

  # Written back uncarried, b2's last text takes its id back.
  edit_comment A13 "write b2's text back" "$person_cited" \
    $'Second,\n\nreworded.\n\nFirst.\n\nThird.' '{"b1": null, "b3": null, "b4": null}'
  expect A13 "writing the text back restores b2" \
    '[["b2","Second,"],["b4","reworded."],["b1","First."],["b3","Third."]]' \
    '[.blocks[] | [.id, .text]]'
  b2_changed="$(jq -er '.blocks[] | select(.id == "b2") | .changed_revision' "$work/body")" ||
    fail A13 "read b2's changed revision" "$(body)"
  reply_block_is "the restore" "Second," false

  html_body=$'Second,\n\nreworded.\n\nFirst.\n\nThird.\n\n<div>\nx\n</div>'
  edit_comment A13 "add block HTML after the last paragraph" "$person_cited" "$html_body" \
    '{"b1": null, "b2": null, "b3": null, "b4": null}'
  expect A13 "block HTML added elsewhere keeps b2's changed revision $b2_changed" \
    "[$b2_changed]" '[.blocks[] | select(.id == "b2") | .changed_revision]'
  get "/api/v1/comments/$person_cited"
  want_status 200 A13 "read the comment holding block HTML"
  expect A13 "the edited body holding block HTML reads back byte for byte" \
    "$(jq -cn --arg b "$html_body" '$b')" '.comment.body'
  reply_block_is "block HTML was added" "Second," false

  html_comment_body=$'Above.\n\n<div>\nx\n</div>\n\nBelow.'
  person_comment A13 "the person writes a comment holding block HTML" "$html_comment_body"
  html_comment="$posted"
  get "/api/v1/comments/$html_comment"
  want_status 200 A13 "read the comment holding block HTML"
  expect A13 "a body holding block HTML reads back byte for byte" \
    "$(jq -cn --arg b "$html_comment_body" '$b')" '.comment.body'
  get "/api/v1/comments/$html_comment/blocks"
  want_status 200 A13 "read the blocks of the comment holding block HTML"
  expect A13 "a body holding block HTML splits at blank lines into text blocks" \
    "$(jq -cn '[["text", "Above."], ["text", "<div>\nx\n</div>"], ["text", "Below."]]')" \
    '[.blocks[] | [.type, .text]]'

  # Negative control: a carried id the comment never had is refused, and the body stays.
  get "/api/v1/comments/$person_cited/blocks"
  want_status 200 A13 "read the comment's revision"
  person_send PATCH "/api/v1/comments/$person_cited" \
    "$(jq -c '{body: "Unknown.", base_revision: .revision, blocks: [{id: "b9", from: 0}]}' \
      "$work/body")"
  want_status 400 A13 "a blocks entry with an unknown id"
  expect A13 "a blocks entry with an unknown id answers 400 INVALID_COMMENT naming b9" true \
    '.code == "INVALID_COMMENT" and (.error | contains("\"b9\""))'
  get "/api/v1/comments/$person_cited"
  want_status 200 A13 "read the comment after the refused edit"
  expect A13 "the refused edit leaves the body" "$(jq -cn --arg b "$html_body" '$b')" \
    '.comment.body'
fi

# A15, the comment half: a link pinned to a revision of a comment's block reads the block as that
# revision held it, and says it changed after the person rewords it and was removed after the
# person deletes it. A message never changes, so a pinned message block is no reference.
get "/api/v1/references?to=$(q "$mx_ref#b-b1@v1")"
want_status 400 "A15 comment" "?to=<message block>@v1"
expect "A15 comment" "a pinned message block on ?to= answers 400 INVALID_REFERENCE" \
  '"INVALID_REFERENCE"' '.code'
if [[ -z "$dev_login" ]]; then
  printf 'SKIP: A15 comment PATCH /api/v1/comments/{id} is human-only and this server has no dev sign-in: DEV_SIGNIN_LOGIN is unset; on a server with /auth/_dev/signin, set it to a login to run A15 comment\n'
else
  person_comment "A15 comment" "the person writes comment Y of two paragraphs" $'Alpha.\n\nBeta.'
  y="$posted"
  y_b2="dispatch://$key/comment/$y#b-b2"
  get "/api/v1/comments/$y/blocks"
  want_status 200 "A15 comment" "read Y's revision"
  y_version="$(jq -er '.revision' "$work/body")" || fail "A15 comment" "read Y's revision" "$(body)"
  y_pin="$y_b2@v$y_version"
  run "A15 comment" "an agent's comment links Y's b2 unpinned and pinned" comment --issue "$key" \
    --body "Unpinned $y_b2, pinned $y_pin."
  get "/api/v1/references?to=$(q "$y_pin")"
  want_status 200 "A15 comment" "?to=Y#b-b2@vN answers"
  expect "A15 comment" "?to=Y#b-b2@vN has pin.state unchanged at revision $y_version" \
    "$(jq -cn --argjson n "$y_version" '["unchanged", $n, "Beta."]')" \
    '[.node.pin.state, .node.pin.version, .node.pin.text]'
  expect "A15 comment" "?to=Y#b-b2@vN lists the agent's one mention" 1 \
    '[.edges[] | select(.kind == "mentions")] | length'

  edit_comment "A15 comment" "reword Y's b2" "$y" $'Alpha.\n\nBeta, reworded.' \
    '{"b1": null, "b2": "Beta, reworded."}'
  get "/api/v1/references?to=$(q "$y_pin")"
  want_status 200 "A15 comment" "?to=Y#b-b2@vN answers after the reword"
  expect "A15 comment" "?to=Y#b-b2@vN has pin.state changed with revision $y_version's text" \
    "$(jq -cn --argjson n "$y_version" '["changed", $n, "Beta."]')" \
    '[.node.pin.state, .node.pin.version, .node.pin.text]'
  expect "A15 comment" "?to=Y#b-b2@vN has the unpinned text the new text" '"Beta, reworded."' \
    '.node.text'
  get "/api/v1/references?to=$(q "$y_b2")"
  want_status 200 "A15 comment" "?to=Y#b-b2 answers after the reword"
  expect "A15 comment" "?to=Y#b-b2 reads the new text and no pin" '["Beta, reworded.",null]' \
    '[.node.text, .node.pin]'

  edit_comment "A15 comment" "delete Y's b2" "$y" 'Alpha.' '{"b1": null}'
  y_removed_in="$revision"
  get "/api/v1/references?to=$(q "$y_pin")"
  want_status 200 "A15 comment" "?to=Y#b-b2@vN answers after the deletion"
  expect "A15 comment" "?to=Y#b-b2@vN has pin.state removed with removed_version $y_removed_in" \
    "[\"removed\",$y_removed_in]" '[.node.pin.state, .node.pin.removed_version]'
  run "A15 comment" "dispatch read --ref Y#b-b2@vN" read --ref "$y_pin"
  grep -q "^Removed in revision $y_removed_in\.$" "$work/out" ||
    fail "A15 comment" "dispatch read --ref Y#b-b2@vN prints Removed in revision" \
      "$(<"$work/out")"
  pass "A15 comment" "dispatch read --ref Y#b-b2@vN prints Removed in revision $y_removed_in"
  get "/api/v1/references?to=$(q "$y_b2")"
  want_status 404 "A15 comment" "?to=Y#b-b2 of the deleted block"
  pass "A15 comment" "?to=Y#b-b2 of the deleted block is 404"
fi

# A6: an ask's parts — its question, each option and its answer — are blocks of the graph. Each is
# named by `dispatch://KEY/ask/<id>#b-<part>`, reads its own text and facts, and `dispatch read`
# prints each option's own reference on the option's line with the edges that reach that part under
# it (A6); each part carries R12's facts, one the ask has dropped included (A11); a part pinned to a
# revision reads the text that revision held, says it changed after a reword and was removed after
# the drop (A15); a bare dispatch:// URL in a question, in an option and in an answer starts at that
# part, and the target counts each written link once (R7); a part no revision of the ask holds is
# refused by name (R10); and a reply in the ask's thread names the part it answers (R11). No part id
# is guessed: every one of them comes from the ask's own read or from its part_of edges. Answering is
# a person's write alone (POST /api/v1/asks/{id}/answer is human-only), so the answer part and R7's
# third link run under DEV_SIGNIN_LOGIN; every other check runs on any server.

# ask_parts SECTION WHAT ASK: reads the ask; sets $parts_revision to the revision of its parts,
# $option_parts to its options' part ids in the ask's order, and $answer_part to its answer's part
# id ("" while it holds no answer).
ask_parts() {
  local part
  get "/api/v1/asks/$3"
  want_status 200 "$1" "$2"
  parts_revision="$(jq -er '.ask.revision' "$work/body")" ||
    fail "$1" "$2" "no revision of the ask's parts: $(body)"
  mapfile -t option_parts < <(jq -r '.ask.options[] | .id // ""' "$work/body")
  answer_part="$(jq -r '.ask.answer.id // ""' "$work/body")"
  for part in ${option_parts[@]+"${option_parts[@]}"}; do
    [[ -n "$part" ]] || fail "$1" "$2" "an option carries no part id: $(body)"
  done
}

# part_node SECTION WHAT ASK REF ID TEXT: ?to=REF reads part ID of ASK, a block node of the ask
# carrying TEXT and the revision its text last changed in; the answer stays in $work/body.
part_node() {
  get "/api/v1/references?to=$(q "$4")"
  want_status 200 "$1" "$2"
  expect "$1" "$2" \
    "$(jq -cn --arg a "$3" --arg b "$5" --arg r "$4" --arg t "$6" --argjson v "$parts_revision" \
      '["block", "ask/\($a)#\($b)", $b, $r, $t, "ask", $a, $v]')" \
    '[.node.kind, .node.id, .node.block_id, .node.ref, .node.text, .node.parent.kind,
      .node.parent.id, .node.changed_version]'
}

# The ask the parts of another ask name by a bare URL (R7): nothing edits it, so the three links
# stand while the counts below are read.
run A6 "an ask for three links to name" ask --issue "$key" \
  --question "The block graph check names this ask from the question, an option and the answer of \
another ask, to read that one written link is counted once. $recommendation"
[[ "$(<"$work/out")" =~ Asked\ ([0-9a-f-]{36}) ]] ||
  fail A6 "an ask for three links to name" "no ask id in: $(<"$work/out")"
named_ask="${BASH_REMATCH[1]}"
named_ref="dispatch://$key/ask/$named_ask"

# The ask whose parts A6, A11, A15, R10 and R11 read: a question and two options, each option's
# text its label, an em dash and its description, as the server writes a part's text.
parts_question="Which day does the block graph check ship the ask parts on? Recommendation: Monday, \
so this check reads a part before the week's reviews."
monday="Monday — Ship at the start of the week."
tuesday="Tuesday — Ship once the review lands."
reworded_tuesday="Tuesday — Ship once the review lands and the queue is green."
run A6 "the ask whose parts the check reads" ask --issue "$child" --question "$parts_question" \
  --option "Monday: Ship at the start of the week." \
  --option "Tuesday: Ship once the review lands."
[[ "$(<"$work/out")" =~ Asked\ ([0-9a-f-]{36}) ]] ||
  fail A6 "the ask whose parts the check reads" "no ask id in: $(<"$work/out")"
parts_ask="${BASH_REMATCH[1]}"
parts_ref="$child_ref/ask/$parts_ask"

ask_parts A6 "read the ask's parts" "$parts_ask"
[[ "${#option_parts[@]}" == 2 ]] ||
  fail A6 "the ask's read names each option's part" "its options are ${option_parts[*]-none}"
[[ "$parts_revision" == 1 ]] ||
  fail A6 "the ask's parts are at revision 1" "the read ask's revision is $parts_revision"
monday_part="${option_parts[0]}"
tuesday_part="${option_parts[1]}"
pass A6 "the ask's read names each option's part ($monday_part, $tuesday_part) at revision 1"
monday_ref="$(block_ref "$parts_ref" "$monday_part")"
tuesday_ref="$(block_ref "$parts_ref" "$tuesday_part")"

# Every part of the ask is part_of it, the question among them, and the question's part id is read
# from that list: no read of an ask carries it.
get "/api/v1/references?to=$(q "$parts_ref")&kind=part_of"
want_status 200 A6 "?to=<ask>&kind=part_of answers"
expect A6 "?to=<ask>&kind=part_of lists its two options and one part more, its question" \
  "$(jq -cnS --arg m "$monday_part" --arg t "$tuesday_part" '{parts: 3, options: [$m, $t] | sort}')" \
  --arg m "$monday_part" --arg t "$tuesday_part" \
  '[.edges[] | select(.kind == "part_of") | .node.block_id] as $ids
   | {parts: ($ids | length), options: ($ids | map(select(. == $m or . == $t)) | sort)}'
# No read of an ask names its question's part, so the question is the part its options and its
# answer do not name; the read below is what proves that part holds the question.
question_part="$(jq -er --arg m "$monday_part" --arg t "$tuesday_part" \
  'first(.edges[] | select(.kind == "part_of") | .node.block_id | select(. != $m and . != $t))' \
  "$work/body")" ||
  fail A6 "the question is a part of the ask" "the parts are $(body)"
question_ref="$(block_ref "$parts_ref" "$question_part")"

# Each part is addressable by its own reference.
part_node A6 "the question reads by its own reference" "$parts_ask" "$question_ref" \
  "$question_part" "$parts_question"
part_node A6 "the first option reads by its own reference" "$parts_ask" "$monday_ref" \
  "$monday_part" "$monday"
part_node A6 "the second option reads by its own reference" "$parts_ask" "$tuesday_ref" \
  "$tuesday_part" "$tuesday"

# A6: a comment cites the first option alone; the ask's read prints that option's own reference on
# the option's line, with the edge that reaches the part under it, and the option nothing cites
# carries no such list.
run A6 "a comment citing the first option" comment --issue "$child" \
  --body "We take $monday_ref."
posted A6 "a comment citing the first option" comment
citing="$posted"
citing_ref="$child_ref/comment/$citing"
get "/api/v1/references?to=$(q "$monday_ref")"
want_status 200 A6 "?to=<first option> answers"
expect A6 "?to=<first option> lists the comment's block, part of the comment" \
  "$(jq -cn --arg c "$citing" '[["mentions", "block", "comment/\($c)#b1", $c]]')" \
  '[.edges[] | [.kind, .node.kind, .node.id, .node.parent.id]]'

run A6 "dispatch read --ref <ask>" read --ref "$parts_ref"
grep -qxF -- "- $monday ($monday_ref)" "$work/out" ||
  fail A6 "read shows it on the option" "$(<"$work/out")"
grep -qxF -- "- $tuesday ($tuesday_ref)" "$work/out" ||
  fail A6 "read shows it on the option" "$(<"$work/out")"
pass A6 "read shows it on the option"
mapfile -t under < <(grep -A 2 -xF -- "- $monday ($monday_ref)" "$work/out")
[[ "${#under[@]}" == 3 && "${under[1]}" == "  Referenced by:" &&
  "${under[2]}" == "  - mentions comment $citing_ref block b1 "* ]] ||
  fail A6 "read lists the comment citing the option under that option" \
    "$(printf '%s\n' "${under[@]-}")"
pass A6 "read lists the comment citing the option under that option"
mapfile -t under < <(grep -A 1 -xF -- "- $tuesday ($tuesday_ref)" "$work/out")
[[ "${under[1]-}" != "  Referenced by:" ]] ||
  fail A6 "the option nothing cites lists no reference of its own" "$(printf '%s\n' "${under[@]}")"
pass A6 "the option nothing cites lists no reference of its own"

# The CLI names a part by what it is, and prints the part's own facts on its `Node:` line (A11).
run A6 "dispatch read --ref <first option>" read --ref "$monday_ref"
grep -qxF "Option $monday_part of ask $parts_ask (changed in revision 1):" "$work/out" ||
  fail A6 "dispatch read --ref <first option> names it as the ask's option" "$(<"$work/out")"
pass A6 "dispatch read --ref <first option> names it as the ask's option"
node_line A11 "dispatch read --ref <first option> prints its facts" \
  "Node: block $monday_ref · made by session $session" " at " \
  " · part of $parts_ref (revision 1)"
run A6 "dispatch read --ref <question>" read --ref "$question_ref"
grep -qxF "Question $question_part of ask $parts_ask (changed in revision 1):" "$work/out" ||
  fail A6 "dispatch read --ref <question> names it as the ask's question" "$(<"$work/out")"
pass A6 "dispatch read --ref <question> names it as the ask's question"

# A11: a part carries the six facts of every node, read by its own reference and as the other end
# of the comment's edge.
of_parts_ask="$(parent_of ask "$parts_ask" "$parts_ref")"
asked_facts="$(wanted "$me" "$me" "$of_parts_ask" 1)"
node_facts "the first option" "$monday_ref" "$asked_facts" A11
node_facts "the question" "$question_ref" "$asked_facts" A11
node_of A11 "$monday_ref"
get "/api/v1/references?from=$(q "$citing_ref")&kind=mentions"
want_status 200 A11 "?from=<citing comment>&kind=mentions answers"
expect A11 "the mention's other end is the option as its own reference reads it" "[$node]" \
  "$defs"'[.edges[] | select(.kind == "mentions") | .node | same]'

# R10: a part no revision of the ask holds is refused, naming the part and the ask.
get "/api/v1/references?to=$(q "$(block_ref "$parts_ref" b9)")"
want_status 404 R10 "?to=<ask>#b-b9"
expect R10 "?to=<ask>#b-b9 is 404 NOT_FOUND naming the part and the ask" true \
  --arg a "$parts_ask" '.code == "NOT_FOUND" and (.error | contains("no part \"b9\" in ask \($a)"))'

# R11: a reply in the ask's thread names the part it answers, and every read of it carries that
# part with its text.
run R11 "a reply naming the first option" comment --issue "$child" --reply-to-ask "$monday_ref" \
  --body "Monday works: the check runs at the start of the week."
# A reply under an ask reports `Replied on ask <id> (comment <uuid>; …)`, not a posted comment.
[[ "$(<"$work/out")" =~ Replied\ on\ ask\ $parts_ask\ \(comment\ ([0-9a-f-]{36}) ]] ||
  fail R11 "a reply naming the first option" "no reply id in: $(<"$work/out")"
part_reply="${BASH_REMATCH[1]}"
get "/api/v1/comments/$part_reply"
want_status 200 R11 "read the reply naming the option"
expect R11 "the reply is under the ask and names the option, with the option's text" \
  "$(jq -cn --arg a "$parts_ask" --arg b "$monday_part" --arg r "$monday_ref" --arg t "$monday" \
    '[$a, $b, $r, $t, false]')" \
  '[.comment.ask_id, .comment.reply_block.block_id, .comment.reply_block.ref,
    .comment.reply_block.text, .comment.reply_block.removed]'
get "/api/v1/references?to=$(q "$monday_ref")&kind=replies_to"
want_status 200 R11 "?to=<first option>&kind=replies_to answers"
expect R11 "?to=<first option>&kind=replies_to is the one reply naming it" \
  "[[\"comment\",\"$part_reply\"]]" '[.edges[] | [.node.kind, .node.id]]'
run R11 "dispatch read --ref <reply naming the option>" read --ref "$child_ref/comment/$part_reply"
grep -qxF "Re: block $monday_part: $monday" "$work/out" ||
  fail R11 "dispatch read --ref <reply> quotes the part it answers" "$(<"$work/out")"
pass R11 "dispatch read --ref <reply> quotes the part it answers"

# R7: a bare dispatch:// URL in a question and in an option starts at that part and nowhere else,
# and the target counts each written link once — the question's link is held by the ask's own row
# and by its question part, and counts once.
run "R7 ask" "an ask naming the target from its question and an option" ask --issue "$child" \
  --question "Does $named_ref close before this check ends? Recommendation: it closes with the \
check, which reads how often one link counts." \
  --option "Close with the check: $named_ref closes when the check ends." \
  --option "Leave it open: a reader finds an ask no check owns."
[[ "$(<"$work/out")" =~ Asked\ ([0-9a-f-]{36}) ]] ||
  fail "R7 ask" "an ask naming the target from its question and an option" \
    "no ask id in: $(<"$work/out")"
r7_ask="${BASH_REMATCH[1]}"
r7_ref="$child_ref/ask/$r7_ask"
ask_parts "R7 ask" "read the parts of the ask naming the target" "$r7_ask"
get "/api/v1/references?from=$(q "$r7_ref")&kind=mentions"
want_status 200 "R7 ask" "?from=<ask>&kind=mentions answers"
expect "R7 ask" "the ask's two links start at its question and at the option holding one" \
  "$(jq -cn --arg a "$r7_ask" --arg o "${option_parts[0]}" --arg n "$named_ask" \
    '[["block", "ask/\($a)#b1", "b1", "ask", $n], ["block", "ask/\($a)#\($o)", $o, "ask", $n]]
      | sort')" \
  '[.edges[] | select(.kind == "mentions")
    | [.block.kind, .block.id, .block.block_id, .node.kind, .node.id]] | sort'
get "/api/v1/asks/$named_ask"
want_status 200 "R7 ask" "read the named ask"
expect "R7 ask" "the named ask counts the question's link and the option's once each" 2 \
  '.ask.referenced_by_count'

if [[ -z "$dev_login" ]]; then
  printf 'SKIP: R7 ask POST /api/v1/asks/{id}/answer is human-only and this server has no dev sign-in: DEV_SIGNIN_LOGIN is unset; on a server with /auth/_dev/signin, set it to a login to read the answer part and the third link\n'
  run "R7 ask" "resolve the unanswered ask naming the target" resolve-ask --ask "$r7_ask" \
    --kind resolved --reason "block-graph.sh read this ask's parts; nothing waits on a person."
else
  answer_text="Close with the check, since $named_ref closes when the check ends."
  person_send POST "/api/v1/asks/$r7_ask/answer" \
    "$(jq -cn --arg t "$answer_text" \
      '{selected: ["Close with the check"], text: $t, expected_edited_at: null}')"
  want_status 200 "R7 ask" "answer the ask as $dev_login"
  ask_parts "R7 ask" "read the answered ask's parts" "$r7_ask"
  [[ -n "$answer_part" ]] ||
    fail "R7 ask" "the answered ask's read names its answer's part" "$(body)"
  pass "R7 ask" "the answered ask's read names its answer's part ($answer_part)"
  # The answer's part text is the labels it selected, " - " and the text it was given.
  part_node A6 "the answer reads by its own reference" "$r7_ask" \
    "$(block_ref "$r7_ref" "$answer_part")" "$answer_part" \
    "Close with the check - $answer_text"
  # The answerer made the answer part, where the ask's author made its question and its options.
  node_facts "the answer part" "$(block_ref "$r7_ref" "$answer_part")" \
    "$(wanted "$(jq -cn --arg l "$dev_login" '[{kind: "user", id: $l}]')" \
      "$(jq -cn --arg l "$dev_login" '[{kind: "user", id: $l}]')" \
      "$(parent_of ask "$r7_ask" "$r7_ref")" "$parts_revision")" A11
  get "/api/v1/references?from=$(q "$r7_ref")&kind=mentions"
  want_status 200 "R7 ask" "?from=<answered ask>&kind=mentions answers"
  expect "R7 ask" "the three links start at the question, the option and the answer" \
    "$(jq -cn --arg a "$r7_ask" --arg o "${option_parts[0]}" --arg w "$answer_part" --arg n "$named_ask" \
      '[["ask/\($a)#b1", "b1", $n], ["ask/\($a)#\($o)", $o, $n], ["ask/\($a)#\($w)", $w, $n]]
        | sort')" \
    '[.edges[] | select(.kind == "mentions") | [.block.id, .block.block_id, .node.id]] | sort'
  get "/api/v1/asks/$named_ask"
  want_status 200 "R7 ask" "read the named ask after the answer"
  expect "R7 ask" "the named ask counts the three written links once each" 3 \
    '.ask.referenced_by_count'
fi

# A15, the ask clause: a comment links the second option bare and pinned to the revision it stands
# at; the reword leaves the bare link reading the new text and the pinned one reading the revision
# it names, changed; the drop makes the pinned link read removed and the bare one 404.
tuesday_pin="$tuesday_ref@v1"
run "A15 ask" "a comment linking the second option bare and pinned" comment --issue "$child" \
  --body "Unpinned $tuesday_ref, pinned $tuesday_pin."
get "/api/v1/references?to=$(q "$tuesday_pin")"
want_status 200 "A15 ask" "?to=<second option>@v1 answers"
expect "A15 ask" "?to=<second option>@v1 has pin.state unchanged at revision 1 with its text" \
  "$(jq -cn --arg t "$tuesday" '["unchanged", 1, $t]')" \
  '[.node.pin.state, .node.pin.version, .node.pin.text]'
expect "A15 ask" "?to=<second option>@v1 lists the one mention: the two forms are one target" 1 \
  '[.edges[] | select(.kind == "mentions")] | length'

run "A15 ask" "reword the second option" edit-ask --ask "$parts_ask" \
  --option "Monday: Ship at the start of the week." \
  --option "Tuesday: Ship once the review lands and the queue is green."
ask_parts "A15 ask" "read the ask after the reword" "$parts_ask"
[[ "$parts_revision" == 2 ]] ||
  fail "A15 ask" "the reword is a new revision of the parts" "the revision is $parts_revision"
[[ "${option_parts[1]-}" == "$tuesday_part" ]] ||
  fail "A15 ask" "the reworded option keeps its part id" \
    "it is now ${option_parts[1]-none}, was $tuesday_part"
pass "A15 ask" "the reworded option keeps its part id at revision 2"
get "/api/v1/references?to=$(q "$tuesday_ref")"
want_status 200 "A15 ask" "?to=<second option> answers after the reword"
expect "A15 ask" "?to=<second option> reads the new text and no pin" \
  "$(jq -cn --arg t "$reworded_tuesday" '[$t, null]')" '[.node.text, .node.pin]'
get "/api/v1/references?to=$(q "$tuesday_pin")"
want_status 200 "A15 ask" "?to=<second option>@v1 answers after the reword"
expect "A15 ask" "?to=<second option>@v1 has pin.state changed with revision 1's text" \
  "$(jq -cn --arg t "$tuesday" '["changed", 1, $t]')" \
  '[.node.pin.state, .node.pin.version, .node.pin.text]'
expect "A15 ask" "?to=<second option>@v1 has the unpinned text the new text" \
  "$(jq -cn --arg t "$reworded_tuesday" '$t')" '.node.text'

run "A15 ask" "drop the second option" edit-ask --ask "$parts_ask" \
  --option "Monday: Ship at the start of the week."
ask_parts "A15 ask" "read the ask after the drop" "$parts_ask"
dropped_in="$parts_revision"
[[ "${#option_parts[@]}" == 1 && "${option_parts[0]}" == "$monday_part" ]] ||
  fail "A15 ask" "the drop leaves the first option alone" "its options are ${option_parts[*]-none}"
get "/api/v1/references?to=$(q "$tuesday_pin")"
want_status 200 "A15 ask" "?to=<second option>@v1 answers after the drop"
expect "A15 ask" "?to=<second option>@v1 has pin.state removed with removed_version $dropped_in" \
  "[\"removed\",$dropped_in]" '[.node.pin.state, .node.pin.removed_version]'
expect "A15 ask" "?to=<second option>@v1 lists no edge for the dropped part" 0 '.edges | length'
get "/api/v1/references?to=$(q "$tuesday_ref")"
want_status 404 "A15 ask" "?to=<second option> of the dropped option"
pass "A15 ask" "?to=<second option> of the dropped option is 404"
run "A15 ask" "dispatch read --ref <second option>@v1" read --ref "$tuesday_pin"
grep -qxF "Removed in revision $dropped_in." "$work/out" ||
  fail "A15 ask" "dispatch read --ref <second option>@v1 prints Removed in revision" \
    "$(<"$work/out")"
pass "A15 ask" "dispatch read --ref <second option>@v1 prints Removed in revision $dropped_in"
grep -qxF "Part $tuesday_part of ask $parts_ask:" "$work/out" ||
  fail "A15 ask" "dispatch read --ref <second option>@v1 names a part the ask no longer places" \
    "$(<"$work/out")"
pass "A15 ask" "dispatch read --ref <second option>@v1 names a part the ask no longer places"

# A11 for a part the ask has dropped: it carries the same six facts, part of the ask at the
# revision its text last changed in, the reword's.
node_facts "the dropped option" "$tuesday_pin" "$(wanted "$me" "$me" "$of_parts_ask" 2)" A11

run A6 "resolve the ask whose parts the check read" resolve-ask --ask "$parts_ask" --kind resolved \
  --reason "block-graph.sh read this ask's parts; nothing waits on a person."
run A6 "resolve the ask the parts named" resolve-ask --ask "$named_ask" --kind resolved \
  --reason "block-graph.sh read this ask's parts; nothing waits on a person."

run close "resolve the ask on $child" resolve-ask --ask "$ask" --kind resolved \
  --reason "block-graph.sh read this ask's facts; nothing waits on a person."
ask_resolved=1
run close "close $child" issue-update --issue "$child" --status "done" \
  --reason "block-graph.sh passed every check against this issue."
child_closed=1
pass close "$child is done"
run close "close $key" issue-update --issue "$key" --status "done" \
  --reason "block-graph.sh passed every check against this issue."
closed=1
pass close "$key is done"
