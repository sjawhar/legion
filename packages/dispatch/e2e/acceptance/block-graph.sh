#!/usr/bin/env bash
# Drives Dispatch's block graph end to end against a running server, as an agent sees it: a block
# reference in a comment and the block's backlinks through the API and `dispatch read`; a deleted
# and restored block; a pinned block reference that changes and is removed; links between the
# blocks of two documents; each block's changed version; every block part of its document; a link
# in a list item; the older document references route.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key). It writes one issue in PROJECT and
# closes it, also when a check fails. Each passing check prints `PASS: <section> <what>`; the first
# failing one prints `FAIL: <section> <what>: <observed>` and the script exits 1.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE block-graph.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN)
USAGE
}

for variable in DISPATCH_URL PROJECT; do
  if [[ -z "${!variable:-}" ]]; then
    printf 'required environment variable %s is unset\n' "$variable" >&2
    usage
    exit 2
  fi
done
for command in bun curl jq; do
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
# No child process inherits the bearer from here: curl reads it from a header file, and the CLI
# gets it in an environment this script builds.
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE

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
cleanup() {
  local code=$?
  if [[ -n "$key" && "$closed" == 0 ]]; then
    cli issue-update --issue "$key" --status "done" \
      --reason "block-graph.sh stopped before its last check; closing the issue it wrote." \
      >"$work/cleanup" 2>&1 ||
      printf 'block-graph.sh: could not close %s: %s\n' "$key" "$(<"$work/cleanup")" >&2
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

# get PATH: the answer's body goes to $work/body and its HTTP status to $status.
get() {
  status="$(curl -sS -o "$work/body" -w '%{http_code}' -H "@$work/auth" "$base$1" 2>"$work/curl")" ||
    status="curl failed: $(<"$work/curl")"
}
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

# upload SECTION NAME FILE: uploads FILE as the issue document NAME through `dispatch artifact`;
# sets $doc_ref and $doc_id.
upload() {
  run "$1" "upload $2" artifact --issue "$key" --name "$2" --path "$3"
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

# The older references route keeps decisions.md, which cites only blocks of the spec, as a source.
get "/api/v1/artifacts/$spec_id/references"
want_status 200 older-route "GET /artifacts/<spec>/references answers"
expect older-route "referenced_by keeps decisions.md" 1 \
  --arg d "$decisions_id" '[.referenced_by[] | select(.kind == "artifact" and .id == $d)] | length'

run close "close $key" issue-update --issue "$key" --status "done" \
  --reason "block-graph.sh passed every check against this issue."
closed=1
pass close "$key is done"
