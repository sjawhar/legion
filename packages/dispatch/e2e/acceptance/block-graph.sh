#!/usr/bin/env bash
# Drives Dispatch's block graph end to end against a running server, as an agent sees it: a block
# reference in a comment and the block's backlinks through the API and `dispatch read`; a deleted
# and restored block; a pinned block reference that changes and is removed; links between the
# blocks of two documents; each block's changed version; every block part of its document; a link
# in a list item; who made each node of every kind and when, who last changed it and when, its
# parent and the parent's version, read by its own reference, as an edge's other end and on the
# `Node:` line of `dispatch read`; the older document references route.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), COMPONENT_SOURCE (where the component
# nodes come from: `fixture` seeds the harness's fake GitHub, on FAKE_GITHUB_PORT, with a
# two-component model in acme/r12-arch, replacing every repository it served, and adds that
# repository as an architecture source of PROJECT; `existing` reads the first project with a
# component that has a parent, and writes nothing; `none` proves the server has no architecture
# source). It writes two issues in PROJECT, the second a child of the first holding an ask, and
# resolves the ask and closes both, also when a check fails; it removes the source `fixture` added
# the same way. Each passing check prints `PASS: <section> <what>`; the first failing one prints
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
         COMPONENT_SOURCE=fixture FAKE_GITHUB_PORT=9022 block-graph.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN;
       COMPONENT_SOURCE=existing reads a component the server holds, COMPONENT_SOURCE=none proves
       it holds none, and neither takes FAKE_GITHUB_PORT)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT COMPONENT_SOURCE
# shellcheck disable=SC2153 # COMPONENT_SOURCE is the caller's input, checked by name above
readonly component_source="$COMPONENT_SOURCE"
case "$component_source" in
  fixture)
    require_env FAKE_GITHUB_PORT
    [[ "$FAKE_GITHUB_PORT" =~ ^[0-9]+$ ]] || {
      printf 'FAKE_GITHUB_PORT is %s, not a port number\n' "$FAKE_GITHUB_PORT" >&2
      usage
      exit 2
    }
    ;;
  existing | none) ;;
  *)
    printf 'COMPONENT_SOURCE is %s; want fixture, existing or none\n' "$component_source" >&2
    usage
    exit 2
    ;;
esac
require_commands base64 bun curl jq
read_bearer
# No child process inherits the bearer from here: read_bearer has unset it from the environment,
# curl reads it from a header file, and the CLI gets it in an environment this script builds.

readonly base="${DISPATCH_URL%/}"
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
cleanup() {
  local code=$?
  if [[ -n "$ask" && "$ask_resolved" == 0 ]]; then
    cli resolve-ask --ask "$ask" --kind resolved \
      --reason "block-graph.sh stopped before its last check; resolving the ask it wrote." \
      >"$work/cleanup" 2>&1 ||
      printf 'block-graph.sh: could not resolve ask %s: %s\n' "$ask" "$(<"$work/cleanup")" >&2
  fi
  if [[ -n "$child" && "$child_closed" == 0 ]]; then
    cli issue-update --issue "$child" --status "done" \
      --reason "block-graph.sh stopped before its last check; closing the issue it wrote." \
      >"$work/cleanup" 2>&1 ||
      printf 'block-graph.sh: could not close %s: %s\n' "$child" "$(<"$work/cleanup")" >&2
  fi
  if [[ -n "$key" && "$closed" == 0 ]]; then
    cli issue-update --issue "$key" --status "done" \
      --reason "block-graph.sh stopped before its last check; closing the issue it wrote." \
      >"$work/cleanup" 2>&1 ||
      printf 'block-graph.sh: could not close %s: %s\n' "$key" "$(<"$work/cleanup")" >&2
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

q() { jq -rn --arg v "$1" '$v|@uri'; }

# send METHOD PATH [JSON]: the answer's body goes to $work/body and its HTTP status to $status.
send() {
  local -a data=()
  [[ $# -lt 3 ]] || data=(-H 'Content-Type: application/json' --data-binary "$3")
  status="$(curl -sS -X "$1" -o "$work/body" -w '%{http_code}' -H "@$work/auth" "${data[@]}" \
    "$base$2" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

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
expect A1 "?to=X lists the comment citing X alone" "[\"$comment\"]" \
  '[.edges[] | select(.kind == "mentions") | .node.id]'

get "/api/v1/references?from=$(q "$comment_ref")"
want_status 200 A1 "?from=<comment> answers"
expect A1 "?from=<comment> has exactly one mentions edge, to block X" \
  "$(jq -cn --arg r "$x" '[["block",$r]]')" \
  '[.edges[] | select(.kind == "mentions") | [.node.kind, .node.ref]]'
expect A1 "?from=<comment> has no edge to the whole document" 0 \
  '[.edges[] | select(.node.kind == "artifact")] | length'

get "/api/v1/references?to=$(q "$spec")"
want_status 200 A1 "?to=<spec> answers"
expect A1 "?to=<spec> lists the comment citing X with .block.block_id ship_it" '["ship_it"]' \
  --arg c "$comment" '[.edges[] | select(.node.id == $c) | .block.block_id]'
expect A1 "?to=<spec> lists the comment citing the spec with no block" '[null]' \
  --arg c "$spec_comment" '[.edges[] | select(.node.id == $c) | .block]'
spec_id="$(jq -er '.node.id' "$work/body")" || fail A1 "?to=<spec> names the spec" "$(body)"

run A1 "dispatch read --ref X" read --ref "$x"
grep -qF "$comment_ref" "$work/out" ||
  fail A1 "dispatch read --ref X lists the comment" "$(<"$work/out")"
pass A1 "dispatch read --ref X lists the comment"
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
# A comment node is named by its own reference, a block node of cites.md by its document's.
x_sources="$(jq -cn --arg c "$comment_ref" --arg d "$cites" '[$c, $d] | sort')"
get "/api/v1/references?to=$(q "$x")"
want_status 200 A2 "?to=X answers"
expect A2 "?to=X lists the comment and cites.md's block" "$x_sources" \
  '[.edges[] | select(.kind == "mentions") | .node.document // .node.ref] | sort'

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
expect A2 "?to=X lists the comment and cites.md's block again" "$x_sources" \
  '[.edges[] | select(.kind == "mentions") | .node.document // .node.ref] | sort'
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

# node_facts WHAT REF WANT: ?to=REF's node carries the facts WANT; the answer stays in $work/body.
node_facts() {
  get "/api/v1/references?to=$(q "$2")"
  want_status 200 R12 "?to=$1 answers"
  expect R12 "$1 carries its facts" "$3" "$defs .node | facts"
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
    ;;
  none)
    get "/api/v1/settings/architecture-sources"
    want_status 200 R12 "list the architecture sources"
    jq -e 'length == 0' "$work/body" >/dev/null || fail R12 component \
      "this server has architecture sources, so run with COMPONENT_SOURCE=existing: $(body)"
    pass R12 "component: this server has no architecture source, so it holds no component node"
    ;;
esac
if [[ "$component_source" != none ]]; then
  component_ref="dispatch://$component_project/component/$component"
  component_parent_ref="dispatch://$component_project/component/$component_parent"
  node_of R12 "$component_parent_ref"
  component_parent_node="$node"
  of_parent="$(parent_of component "$component_project/$component_parent" "$component_parent_ref")"
  get "/api/v1/references?to=$(q "$component_ref")"
  want_status 200 R12 "?to=$component_ref answers"
  if [[ "$component_source" == fixture ]]; then
    expect R12 "component $component carries its facts, its times recorded at the import" \
      "$(jq -cnS --argjson parent "$of_parent" '{made_by: [], made_at: true, changed_by: [],
        changed_at: true, parent: $parent, parent_version: null}')" "$defs .node | facts"
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
fi

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
