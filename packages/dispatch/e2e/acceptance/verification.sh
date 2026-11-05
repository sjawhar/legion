#!/usr/bin/env bash
# Drives Dispatch's verification rules end to end against a running server, as an agent sees them:
# a `plan_task` and a `check` that serve a spec's `requirement`, and the `serves` and `needs` edges
# `GET /api/v1/references` lists for them; every attribute a `check` carries, read back by
# `dispatch read --ref`; the refusals a pointer earns — a block no document holds, a block of an
# issue in another tree, a `needs` naming a block outside its own plan — and a `when` the schema
# does not know; a requirement's id through a move; `dispatch result` and `dispatch results`, with
# an unknown check, a pass that counts a skipped case and a before-merge pass naming no negative
# control refused; the exact passing commands in the daemon's implementer and tester prompts,
# accepted with their negative control and refused without it; the one write a closed issue
# takes, a result for an `after_merge` check, beside the two it still refuses; the coverage flags
# `dispatch read --issue` prints, cleared by a check of the issue and by a check of a child, and
# back when that check is deleted, with the missing plan task named only once a plan document
# exists; the gap an approval request names and the covered spec that earns none; `dispatch
# plan-approve`, refused on a spec, and `dispatch request-approval`, refused on a plan; and the one
# request that names a spec and its Verification document together.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), and optionally FAKE_GITHUB_PORT (the
# harness's fake GitHub, packages/dispatch/e2e/fake-github.ts, which the server reads as
# DISPATCH_GITHUB_API_BASE: the currency section seeds it with one repository holding one pull
# request, replacing every repository it served, maps that repository as PROJECT's default and
# removes the mapping again, so a result recorded at that pull request's head reads `current`;
# unset, or with PROJECT already naming a default repository, one `SKIP: currency …` line stands in
# for the section) and DEV_SIGNIN_LOGIN (a login the server's dev sign-in route, /auth/_dev/signin,
# signs in: the local harness has it, a deployed server does not). Recording a review is a person's
# write alone, so the two review sections sign that person in with a cookie jar; with
# DEV_SIGNIN_LOGIN unset each prints one `SKIP: … POST /api/v1/artifacts/{id}/reviews needs a
# person's credential holding the approve scope …` line and every other section runs.
#
# It writes four issues in PROJECT — a root, a child of it, a sibling in another tree and one more
# whose spec and Verification document are approved together — with a spec, a Verification document
# and, on the root, a plan; it closes every one of them, also when a check fails, and an approval
# request it left open is withdrawn with the issue that holds it. Nothing it writes is read by a
# second run, so it may be run again at once.
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script. A negative check — `grep -q … ||
# fail` and the like — is written as the AND list it reads as, and is safe for that same reason:
# its failing command is not the last one, so `set -e` leaves the `|| fail` to decide.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         [FAKE_GITHUB_PORT=9022] [DEV_SIGNIN_LOGIN=alice] verification.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN;
       FAKE_GITHUB_PORT, on a server reading the harness's fake GitHub alone, runs the currency
       section, which replaces every repository that fake served; unset, it is skipped;
       DEV_SIGNIN_LOGIN, on a server with dev sign-in alone, records the two human reviews as that
       person; unset, those two sections are skipped)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT
readonly fake_github_port="${FAKE_GITHUB_PORT-}"
if [[ -n "$fake_github_port" ]]; then
  [[ "$fake_github_port" =~ ^[0-9]+$ ]] || {
    printf 'FAKE_GITHUB_PORT is %s, not a port number\n' "$fake_github_port" >&2
    usage
    exit 2
  }
fi
readonly dev_login="${DEV_SIGNIN_LOGIN-}"
require_commands bun curl jq
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
session="verification-$(date +%s)-$$"
readonly session

umask 077
work="$(mktemp -d)"
readonly work
mkdir "$work/home" "$work/state"
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

# The issues the run writes, each cleared from the list once it is closed, and the repository
# mapping the currency section adds.
written=()
closed=()
mapped=""
seeded_github=0

# close_written ISSUE: closes ISSUE, an issue this script wrote, from cleanup.
close_written() {
  cli issue-update --issue "$1" --status "done" \
    --reason "verification.sh stopped before its last check; closing the issue it wrote." \
    >"$work/cleanup" 2>&1 ||
    printf 'verification.sh: could not close %s: %s\n' "$1" "$(<"$work/cleanup")" >&2
}
cleanup() {
  local code=$? issue index
  for index in "${!written[@]}"; do
    issue="${written[$index]}"
    [[ "${closed[$index]}" == 1 ]] || close_written "$issue"
  done
  if [[ -n "$mapped" ]]; then
    send DELETE "/api/v1/settings/repo-projects/$mapped" \
      "$(jq -cn --arg s "$session" '{actor: {kind: "session", id: $s}}')"
    [[ "$status" == 2?? ]] ||
      printf 'verification.sh: could not remove the repository mapping %s: HTTP %s: %s\n' \
        "$mapped" "$status" "$(body)" >&2
  fi
  if [[ "$seeded_github" == 1 ]]; then
    curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' --data-binary '{}' \
      "http://127.0.0.1:$fake_github_port/__fixture/repos" 2>"$work/curl" ||
      printf 'verification.sh: could not empty the fake GitHub: %s\n' "$(<"$work/curl")" >&2
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
skip() { printf 'SKIP: %s %s\n' "$1" "$2"; }

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

# issue SECTION TITLE FILE [PARENT]: creates an issue with the spec FILE; sets $key to its key and
# records it for cleanup.
issue() {
  local -a parent=()
  [[ $# -lt 4 ]] || parent=(--parent "$4")
  run "$1" "create \"$2\"" issue --project "$project" --title "$2" --spec-file "$3" \
    "${parent[@]}" --force
  [[ "$(<"$work/out")" =~ Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
    fail "$1" "create \"$2\"" "no issue key in: $(<"$work/out")"
  key="${BASH_REMATCH[1]}"
  written+=("$key")
  closed+=(0)
  pass "$1" "created $key in $project"
}

# close_issue SECTION ISSUE: closes ISSUE and marks it closed for cleanup.
close_issue() {
  local index
  run "$1" "close $2" issue-update --issue "$2" --status "done" \
    --reason "The verification check closes the issue it wrote."
  for index in "${!written[@]}"; do
    [[ "${written[$index]}" != "$2" ]] || closed[index]=1
  done
  pass "$1" "closed $2"
}

# upload SECTION ISSUE NAME FILE: uploads FILE as the document NAME of ISSUE.
upload() {
  run "$1" "upload $3 to $2" artifact --issue "$2" --name "$3" --path "$4"
  pass "$1" "uploaded $3 to $2"
}

# document_body ISSUE NAME FILE: the JSON an artifact POST takes for FILE under NAME.
document_body() {
  jq -cn --arg a "$session" --arg n "$2" --rawfile c "$3" \
    '{actor: {kind: "session", id: $a}, name: $n, content: $c}'
}

# refusal SECTION WHAT SUBSTRING: the last answer's `error` holds SUBSTRING. The message is read
# out of the JSON, so the comparison is the words a reader sees, not their escaping on the wire.
refusal() {
  local message
  message="$(jq -er '.error' "$work/body")" || fail "$1" "$2 names $3" "no error in: $(body)"
  [[ "$message" == *"$3"* ]] || fail "$1" "$2 names $3" "the refusal reads $message"
  pass "$1" "$2 names $3"
}

# refused_document SECTION WHAT ISSUE NAME FILE CODE SUBSTRING: posting FILE as ISSUE's document
# NAME is refused 400 CODE, and the refusal holds SUBSTRING.
refused_document() {
  send POST "/api/v1/issues/$3/artifacts" "$(document_body "$3" "$4" "$5")"
  want_status 400 "$1" "$2"
  expect "$1" "$2 is refused $6" "\"$6\"" '.code'
  refusal "$1" "$2" "$7"
}

# artifact_id SECTION ISSUE NAME: sets $artifact to the id of ISSUE's document NAME.
artifact_id() {
  get "/api/v1/issues/$2/artifacts"
  want_status 200 "$1" "read $2's documents"
  artifact="$(jq -er --arg n "$3" 'map(select(.name == $n)) | .[0].id' "$work/body")" ||
    fail "$1" "find $2's $3" "$(body)"
}

# version_of SECTION ISSUE SLUG: sets $version to the current version of ISSUE's document SLUG.
version_of() {
  get "/api/v1/issues/$2/artifacts/$3/text"
  want_status 200 "$1" "read $2's $3 version"
  version="$(jq -er '.version' "$work/body")" || fail "$1" "read $2's $3 version" "$(body)"
}

# edit SECTION WHAT ISSUE ARTIFACT OPS: applies OPS through `dispatch doc-edit`.
edit() {
  run "$1" "$2" doc-edit --issue "$3" --artifact "$4" --ops-json "$5"
  pass "$1" "$2"
}

# refused_edit SECTION WHAT ISSUE ARTIFACT OPS SUBSTRING: the edit is refused and names SUBSTRING.
refused_edit() {
  cli doc-edit --issue "$3" --artifact "$4" --ops-json "$5" >"$work/out" 2>&1 &&
    fail "$1" "$2" "the edit was accepted: $(<"$work/out")"
  grep -qF "$6" "$work/out" || fail "$1" "$2" "$(<"$work/out")"
  pass "$1" "$2"
}

# gaps SECTION WHAT ISSUE WANT: the `Coverage:` lines `dispatch read --issue ISSUE` prints, in
# order, as a JSON array, equal WANT.
gaps() {
  local got
  run "$1" "read $3" read --issue "$3"
  got="$(sed -n 's/^Coverage: //p' "$work/out" |
    jq -Rsc 'split("\n") | map(select(. != ""))')" ||
    fail "$1" "$2" "unreadable: $(<"$work/out")"
  [[ "$got" == "$4" ]] || fail "$1" "$2" "got $got, want $4"
  pass "$1" "$2"
}

# record SECTION WHAT ISSUE ARGS...: records a result through `dispatch result`.
record() {
  local section="$1" what="$2" issue="$3"
  shift 3
  run "$section" "$what" result --issue "$issue" "$@"
  pass "$section" "$what"
}

# refused_result SECTION WHAT ISSUE JSON CODE STATUS: POST …/results is refused STATUS CODE.
refused_result() {
  local want_code="$5" status="$6"
  send POST "/api/v1/issues/$3/results" "$4"
  want_status "$status" "$1" "$2 is refused $status"
  expect "$1" "$2 is refused $want_code" "\"$want_code\"" '.code'
}

readonly commit_one=1111111111111111111111111111111111111111
readonly commit_two=2222222222222222222222222222222222222222

# --- setup ---------------------------------------------------------------------------------------

# The root's spec marks three requirements: r1 is served by a check and a plan task, r2 by a check
# alone, and r3 by nothing until a child's check serves it.
cat >"$work/root-spec.md" <<'SPEC'
The verification check's root spec.

:::requirement{#r1}
A check and a plan task both serve this requirement.
:::

:::requirement{#r2}
A check alone serves this requirement.
:::

:::requirement{#r3}
Only a child issue's check serves this requirement.
:::
SPEC
issue setup "Verification rules" "$work/root-spec.md"
root="$key"
readonly root
readonly root_spec="dispatch://$root/spec"
readonly root_verification="dispatch://$root/artifact/verification-md"
readonly root_plan="dispatch://$root/artifact/plan-md"

printf 'The verification check'"'"'s child spec.\n' >"$work/child-spec.md"
issue setup "Verification rules, the child" "$work/child-spec.md" "$root"
child="$key"
readonly child

cat >"$work/other-spec.md" <<'SPEC'
The verification check's sibling spec, in another tree.

:::requirement{#o1}
One check serves this requirement, so the spec has no gap.
:::
SPEC
issue setup "Verification rules, another tree" "$work/other-spec.md"
other="$key"
readonly other

cat >"$work/both-spec.md" <<'SPEC'
The verification check's spec, approved beside its Verification document.

:::requirement{#b1}
One check serves this requirement too.
:::
SPEC
issue setup "Verification rules, approved with its proof" "$work/both-spec.md"
both="$key"
readonly both

# --- check: a check's attributes, and the `when` the schema does not know ------------------------

cat >"$work/root-verification.md" <<VERIFICATION
How $root is proved.

:::check{#c1 serves="$root_spec#b-r1" surface="cli" run="bash e2e/acceptance/verification.sh" when="before_merge" negative_control="Delete the check and the requirement reads uncovered."}
The first requirement is proved.
:::

:::check{#cnone}
A check that serves no requirement.
:::
VERIFICATION
upload check "$root" verification.md "$work/root-verification.md"

run check "read the check by its reference" read --ref "$root_verification#b-c1"
want_lines="$(
  cat <<LINES
Block: c1
Type: check
Document: $root_verification
serves: $root_spec#b-r1
surface: cli
run: bash e2e/acceptance/verification.sh
when: before_merge
negative_control: Delete the check and the requirement reads uncovered.
LINES
)"
[[ "$(sed -n '1,8p' "$work/out")" == "$want_lines" ]] ||
  fail check "the check's head carries every attribute" "$(sed -n '1,8p' "$work/out")"
pass check "the check's head carries every attribute, in the order its type declares them"

get "/api/v1/references?to=$(q "$root_spec#b-r1")"
want_status 200 check "?to=r1 answers"
expect check "?to=r1 lists the serves edge from the check" \
  "$(jq -cn --arg k "$root" '[["serves", "\($k)/verification-md#c1"]]')" \
  '[.edges[] | select(.kind == "serves") | [.kind, .node.id]]'

get "/api/v1/references?from=$(q "$root_verification#b-cnone")"
want_status 200 check "?from=the check that serves nothing answers"
expect check "the check that serves nothing writes no serves edge" 0 \
  '[.edges[] | select(.kind == "serves")] | length'

printf ':::check{#cwhen when="sometimes"}\nA check whose when the schema does not know.\n:::\n' \
  >"$work/bad-when.md"
refused_document check 'a check with when="sometimes"' "$root" bad-when.md "$work/bad-when.md" \
  INVALID_MARKDOWN 'attribute "when": must be one of before_merge, after_merge, default_branch, got "sometimes"'

# --- pointer: what a `serves` may and may not name -----------------------------------------------

printf ':::check{#cmissing serves="%s#b-nothing"}\nA check serving a block no document holds.\n:::\n' \
  "$root_spec" >"$work/missing-serves.md"
refused_document pointer 'a check serving a block no document holds' "$root" missing-serves.md \
  "$work/missing-serves.md" INVALID_MARKDOWN \
  "attribute \"serves\" points at $root_spec#b-nothing, which no document holds"

printf ':::check{#cother serves="dispatch://%s/spec#b-o1"}\nA check serving another tree.\n:::\n' \
  "$other" >"$work/other-serves.md"
refused_document pointer "a check serving a requirement of $other" "$root" other-serves.md \
  "$work/other-serves.md" INVALID_MARKDOWN \
  "attribute \"serves\" points at dispatch://$other/spec#b-o1, a block of $other; it points only at a block of $root"

edit pointer "a check serving r2, a requirement of its own issue, saves" "$root" verification.md \
  "$(jq -cn --arg m ":::check{#c2 serves=\"$root_spec#b-r2\" surface=\"api\" run=\"curl the coverage route\"}
The second requirement is proved.
:::" '[{op: "insert", after: "end", markdown: $m}]')"
get "/api/v1/references?to=$(q "$root_spec#b-r2")"
want_status 200 pointer "?to=r2 answers"
expect pointer "?to=r2 lists the serves edge from the saved check" \
  "$(jq -cn --arg k "$root" '[["serves", "\($k)/verification-md#c2"]]')" \
  '[.edges[] | select(.kind == "serves") | [.kind, .node.id]]'

# --- coverage-plan: a requirement with a check and no plan task, before any plan document --------

get "/api/v1/issues/$root/coverage"
want_status 200 coverage-plan "the coverage route answers"
expect coverage-plan "$root has no plan document yet" false '.has_plan'
gaps coverage-plan "a requirement with a check and no plan task carries no flag" "$root" \
  '["r3 has no check"]'

# --- plan: a plan task that serves, one that waits on another, and what `needs` may not name -----

cat >"$work/root-plan.md" <<PLAN
How $root is built.

:::plan_task{#t1 serves="$root_spec#b-r1"}
The first task serves the first requirement.
:::

:::plan_task{#t2 needs="$root_plan#b-t1"}
The second task waits on the first.
:::

:::plan_task{#t3}
A task that serves no requirement.
:::
PLAN
upload plan "$root" plan.md "$work/root-plan.md"

get "/api/v1/references?to=$(q "$root_spec#b-r1")"
want_status 200 plan "?to=r1 answers"
expect plan "?to=r1 lists the serves edge from the plan task beside the check's" \
  "$(jq -cn --arg k "$root" '[["serves", "\($k)/plan-md#t1"], ["serves", "\($k)/verification-md#c1"]] | sort')" \
  '[.edges[] | select(.kind == "serves") | [.kind, .node.id]] | sort'
get "/api/v1/references?from=$(q "$root_plan#b-t3")"
want_status 200 plan "?from=the task that serves nothing answers"
expect plan "the plan task that serves nothing writes no serves edge" 0 \
  '[.edges[] | select(.kind == "serves")] | length'

run plan "read the waiting task by its reference" read --ref "$root_plan#b-t2"
grep -qxF "needs: $root_plan#b-t1" "$work/out" ||
  fail plan "the waiting task's head carries its needs" "$(<"$work/out")"
pass plan "the waiting task's head carries needs: $root_plan#b-t1"
get "/api/v1/references?to=$(q "$root_plan#b-t1")"
want_status 200 plan "?to=the first task answers"
expect plan "?to=the first task lists the needs edge from the second" \
  "$(jq -cn --arg k "$root" '[["needs", "\($k)/plan-md#t2"]]')" \
  '[.edges[] | select(.kind == "needs") | [.kind, .node.id]]'

refused_edit plan "a needs naming a requirement, which is no plan task, is refused" "$root" plan.md \
  "$(jq -cn --arg m ":::plan_task{#t4 needs=\"$root_spec#b-r1\"}
A task waiting on a requirement.
:::" '[{op: "insert", after: "end", markdown: $m}]')" \
  "attribute \"needs\" points at $root_spec#b-r1, a \"requirement\" block; it points only at plan_task"

printf ':::plan_task{#u1}\nA task of another document.\n:::\n\n:::plan_task{#u2 needs="%s#b-t1"}\nA task waiting on another plan'"'"'s task.\n:::\n' \
  "$root_plan" >"$work/outside-needs.md"
refused_document plan "a needs naming a task of another plan" "$root" outside-needs.md \
  "$work/outside-needs.md" INVALID_MARKDOWN \
  "attribute \"needs\" points at $root_plan#b-t1, a block of $root/plan-md; it points only at a block of its own document"

# --- coverage-task: the missing plan task is named once the plan document exists -----------------

gaps coverage-task "a requirement with a check and no plan task is flagged once plan.md exists" \
  "$root" '["r2 has no plan task","r3 has no check","r3 has no plan task"]'

# --- coverage-child: a child's check clears the root's flag, and its deletion brings it back -----

cat >"$work/child-verification.md" <<VERIFICATION
How $child proves what $root states.

:::check{#c3 serves="$root_spec#b-r3"}
The child's check proves the root's third requirement.
:::

:::check{#cafter when="after_merge" surface="api" run="curl the production route"}
The production check runs after the merge.
:::

:::check{#cbefore surface="cli" run="bun test"}
The pre-merge check runs before it.
:::
VERIFICATION
upload coverage-child "$child" verification.md "$work/child-verification.md"
gaps coverage-child "a check of the child clears the root's flag" "$root" \
  '["r2 has no plan task","r3 has no plan task"]'

edit coverage-child "delete the child's check" "$child" verification.md \
  '[{"op":"delete","block":"c3"}]'
gaps coverage-child "deleting the child's check brings the root's flag back" "$root" \
  '["r2 has no plan task","r3 has no check","r3 has no plan task"]'

# --- move: a requirement's id survives the block moving ------------------------------------------

edit move "move the third requirement above the first" "$root" spec \
  '[{"op":"move","block":"r3","before":"block:r1"}]'
get "/api/v1/issues/$root/coverage"
want_status 200 move "the coverage route answers after the move"
expect move "the requirements keep their ids in the new order" '["r3","r1","r2"]' \
  '[.requirements[].block_id]'
get "/api/v1/references?to=$(q "$root_spec#b-r3")"
want_status 200 move "?to=r3 answers after the move"
expect move "r3 is read by the id it had before the move" '"r3"' '.node.block_id'
get "/api/v1/references?to=$(q "$root_spec#b-r1")"
want_status 200 move "?to=r1 answers after the move"
expect move "r1 keeps both edges that serve it through the move" 2 \
  '[.edges[] | select(.kind == "serves")] | length'

# --- result: recording one, and the refusals a result's fields earn -------------------------------

record result "record a pass for c1" "$root" --check c1 --role tester --verdict pass \
  --commit "$commit_one" --surface cli --run "bash e2e/acceptance/verification.sh" \
  --observed "every check passed" --negative-control "Deleted the check and the flag came back."
grep -qF "Pull request body line:" "$work/out" ||
  fail result "the record prints the pull-request body line" "$(<"$work/out")"
pass result "the record prints the pull-request body line"

run result "read the results of $root" results --issue "$root"
grep -qE "^- c1 \(before_merge\): pass at ${commit_one:0:12} by tester, " "$work/out" ||
  fail result "the results read shows the result under c1" "$(<"$work/out")"
pass result "the results read shows the pass under c1"
grep -qF -- "- cnone (before_merge): no result yet." "$work/out" ||
  fail result "a check with no result says so" "$(<"$work/out")"
pass result "a check nobody has run reads no result yet"

refused_result result "a check id no Verification document in reach holds" "$root" \
  "$(jq -cn --arg s "$session" --arg c "$commit_one" \
    '{actor: {kind: "session", id: $s}, check: "no-such-check", role: "tester", verdict: "pass", commit: $c}')" \
  UNKNOWN_CHECK 400
refused_result result "a pass counting a skipped case" "$root" \
  "$(jq -cn --arg s "$session" --arg c "$commit_one" \
    '{actor: {kind: "session", id: $s}, check: "c1", role: "tester", verdict: "pass", commit: $c, skipped: 1}')" \
  RESULT_CONTRADICTS 400
refused_result result "a before-merge pass naming no negative control" "$root" \
  "$(jq -cn --arg s "$session" --arg c "$commit_one" \
    '{actor: {kind: "session", id: $s}, check: "c1", role: "tester", verdict: "pass", commit: $c}')" \
  NEGATIVE_CONTROL_REQUIRED 400

# --- prompt-example: the command the daemon's prompts show is the one the server takes ------------

for prompt_file in implementer.md tester.md; do
  mapfile -t prompt_commands < <(grep -o '`dispatch result --issue <issue>[^`]*`' \
    "$script_dir/../../../daemon/internal/prompts/roles/$prompt_file")
  [[ "${#prompt_commands[@]}" == 1 ]] ||
    fail prompt-example "$prompt_file holds one result command" "found ${#prompt_commands[@]}"
  prompt_command="${prompt_commands[0]}"
  prompt_command="${prompt_command#\`}"
  prompt_command="${prompt_command%\`}"
  prompt_command="${prompt_command//<issue>/$root}"
  prompt_command="${prompt_command//<block id>/c1}"
  prompt_command="${prompt_command//<head sha>/$commit_one}"
  # Split only the unquoted CLI words; keep the quoted negative-control text as one argument,
  # verbatim. No shell evaluates the prompt's text.
  prompt_pattern='^dispatch (.*) --negative-control "([^"]+)"$'
  [[ "$prompt_command" =~ $prompt_pattern ]] ||
    fail prompt-example "$prompt_file's command names its negative control" "$prompt_command"
  prompt_negative_control="${BASH_REMATCH[2]}"
  read -r -a prompt_args <<<"${BASH_REMATCH[1]}"
  run prompt-example "$prompt_file's command" "${prompt_args[@]}" \
    --negative-control "$prompt_negative_control"
  grep -qF "Pull request body line:" "$work/out" ||
    fail prompt-example "$prompt_file's command is accepted" "$(<"$work/out")"
  pass prompt-example "$prompt_file's command is accepted"
  if cli "${prompt_args[@]}" >"$work/out" 2>&1; then
    fail prompt-example "$prompt_file's command without its negative control is refused" "$(<"$work/out")"
  fi
  grep -qF "NEGATIVE_CONTROL_REQUIRED" "$work/out" ||
    fail prompt-example "$prompt_file's command without its negative control is refused" "$(<"$work/out")"
  pass prompt-example "$prompt_file's command without its negative control is refused"
done

# --- closed: the one write a closed issue takes --------------------------------------------------

close_issue closed "$child"
record closed "record a pass for the after-merge check on the closed $child" "$child" \
  --check cafter --role tester --verdict pass --commit "$commit_two" --surface api \
  --run "curl the production route" --observed "the production route answered"
refused_result closed "a result for a before-merge check on a closed issue" "$child" \
  "$(jq -cn --arg s "$session" --arg c "$commit_two" \
    '{actor: {kind: "session", id: $s}, check: "cbefore", role: "tester", verdict: "pass", commit: $c}')" \
  ISSUE_CLOSED 409

printf 'A note the closed issue must refuse.\n' >"$work/note.md"
send POST "/api/v1/issues/$child/artifacts" "$(document_body "$child" note.md "$work/note.md")"
want_status 409 closed "a document write on the closed issue"
expect closed "a document write on the closed issue is refused ISSUE_CLOSED" '"ISSUE_CLOSED"' '.code'

# --- approval: the gap a request names, the plan review, and what each refuses --------------------

run approval "request approval of $root's spec" request-approval --issue "$root" \
  --summary "The verification check asks for this spec, which still has a gap."
grep -qF "Coverage of spec.md: r3 has no check." "$work/out" ||
  fail approval "the request names the spec's gap" "$(<"$work/out")"
pass approval "the request on $root's spec names r3 has no check"

cat >"$work/other-verification.md" <<VERIFICATION
How $other is proved.

:::check{#oc1 serves="dispatch://$other/spec#b-o1" surface="api" run="curl the coverage route"}
The sibling's one requirement is proved.
:::
VERIFICATION
upload approval "$other" verification.md "$work/other-verification.md"
run approval "request approval of $other's covered spec" request-approval --issue "$other" \
  --summary "The verification check asks for this spec, every requirement of which has a check."
grep -qF "Coverage of " "$work/out" &&
  fail approval "a covered spec gets no gap line" "$(<"$work/out")"
pass approval "the request on $other's covered spec names no gap"

version_of approval "$root" plan-md
run approval "approve the plan at version $version" plan-approve --issue "$root" --version "$version"
grep -qF "The plan review approved plan.md" "$work/out" ||
  fail approval "the plan review approves the plan" "$(<"$work/out")"
pass approval "the plan review approved $root's plan at version $version"
artifact_id approval "$root" plan.md
get "/api/v1/artifacts/$artifact/reviews"
want_status 200 approval "the plan's reviews answer"
expect approval "the plan carries one approved plan review" \
  "$(jq -cn --argjson v "$version" '[["approved", "plan_review", $v]]')" \
  '[.[] | [.state, .kind, .version]]'

artifact_id approval "$root" spec.md
send POST "/api/v1/artifacts/$artifact/plan-reviews" \
  "$(jq -cn --arg s "$session" '{actor: {kind: "session", id: $s}, version: 1}')"
want_status 409 approval "a plan review of a spec"
expect approval "a plan review of a spec is refused NOT_PLAN_DOCUMENT" '"NOT_PLAN_DOCUMENT"' '.code'

artifact_id approval "$root" plan.md
send POST "/api/v1/artifacts/$artifact/approval-requests" \
  "$(jq -cn --arg s "$session" '{actor: {kind: "session", id: $s}, summary: "Approve the plan."}')"
want_status 409 approval "an approval request on a plan"
expect approval "an approval request on a plan is refused PLAN_APPROVED_BY_REVIEW" \
  '"PLAN_APPROVED_BY_REVIEW"' '.code'

# --- review: one request over two documents, and one over the spec alone -------------------------

cat >"$work/both-verification.md" <<VERIFICATION
How $both is proved.

:::check{#bc1 serves="dispatch://$both/spec#b-b1" surface="browser" run="bun run e2e"}
The requirement approved beside this document is proved.
:::
VERIFICATION
upload review "$both" verification.md "$work/both-verification.md"
run review "request approval of $both's spec and its Verification document" request-approval \
  --issue "$both" --include-verification \
  --summary "The verification check asks for this spec and its proof together."
grep -qF 'Approve spec.md (version 1) and verification.md (version 1)?' "$work/out" ||
  fail review "one request names both documents" "$(<"$work/out")"
pass review "one request's question names spec.md and verification.md at their versions"

if [[ -z "$dev_login" ]]; then
  skip review "a person's approval records a review on each document: DEV_SIGNIN_LOGIN is unset; POST /api/v1/artifacts/{id}/reviews needs a person's credential holding the approve scope, and only a server with dev sign-in mints that person's cookie"
  skip review "a request naming the spec alone records nothing on the Verification document: DEV_SIGNIN_LOGIN is unset"
else
  curl -sS -o /dev/null -c "$work/jar" "$base/auth/_dev/signin?login=$(q "$dev_login")" \
    2>"$work/curl" || fail review "sign $dev_login in" "$(<"$work/curl")"
  [[ -s "$work/jar" ]] || fail review "sign $dev_login in" "the dev sign-in set no cookie"
  pass review "signed $dev_login in at the dev sign-in route"

  artifact_id review "$both" spec.md
  person_send POST "/api/v1/artifacts/$artifact/reviews" '{"state":"approved"}'
  want_status 201 review "$dev_login approves the spec of $both"
  pass review "$dev_login approved $both's spec, answering the request that named both"
  for document in spec.md verification.md; do
    artifact_id review "$both" "$document"
    get "/api/v1/artifacts/$artifact/reviews"
    want_status 200 review "$both's $document reviews answer"
    expect review "$both's $document carries one approved person's review" \
      '[["approved","person",1]]' '[.[] | [.state, .kind, .version]]'
  done

  artifact_id review "$other" spec.md
  person_send POST "/api/v1/artifacts/$artifact/reviews" '{"state":"approved"}'
  want_status 201 review "$dev_login approves the spec of $other"
  pass review "$dev_login approved $other's spec, answering the request that named it alone"
  get "/api/v1/artifacts/$artifact/reviews"
  want_status 200 review "$other's spec reviews answer"
  expect review "$other's spec carries the approved review" '[["approved","person",1]]' \
    '[.[] | [.state, .kind, .version]]'
  artifact_id review "$other" verification.md
  get "/api/v1/artifacts/$artifact/reviews"
  want_status 200 review "$other's verification.md reviews answer"
  expect review "a request naming the spec alone records nothing on the Verification document" \
    '[]' '[.[] | [.state, .kind, .version]]'
fi

# --- currency: a result at the head of the issue's pull request reads current --------------------

currency_repo="acme/verification-$$"
if [[ -z "$fake_github_port" ]]; then
  skip currency "a result at the pull request's head reads current: FAKE_GITHUB_PORT is unset; only a server reading the harness's fake GitHub can be told a pull request's head"
else
  get "/api/v1/settings/repo-projects"
  want_status 200 currency "the repository mappings answer"
  mapped_already="$(jq -r --arg p "$project" \
    '[.[] | select(.project == $p and .default)] | length' "$work/body")" ||
    fail currency "read the repository mappings" "$(body)"
  if [[ "$mapped_already" != 0 ]]; then
    skip currency "a result at the pull request's head reads current: $project already names a default repository, which this section would replace"
  else
    curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' --data-binary "$(
      jq -cn --arg r "$currency_repo" --arg k "$root" --arg sha "$commit_two" \
        '{($r): {contents: "read", pull_requests: "read", installation_id: 256,
                 commit_pulls: {($sha): [7]},
                 pulls: {"7": {head_sha: $sha, state: "open", title: "\($k) the verification check"}}}}'
    )" "http://127.0.0.1:$fake_github_port/__fixture/repos" 2>"$work/curl" ||
      fail currency "seed the fake GitHub" "$(<"$work/curl")"
    seeded_github=1
    pass currency "seeded the fake GitHub with $currency_repo and its pull request 7"

    send PUT "/api/v1/settings/repo-projects/$currency_repo" \
      "$(jq -cn --arg s "$session" --arg p "$project" \
        '{actor: {kind: "session", id: $s}, project: $p, default: true}')"
    want_status 200 currency "map $currency_repo as $project's default repository"
    mapped="$currency_repo"
    pass currency "mapped $currency_repo as $project's default repository"

    record currency "record a pass at the pull request's head" "$root" --check c1 --role tester \
      --verdict pass --commit "$commit_two" --surface cli --run "bash e2e/acceptance/verification.sh" \
      --negative-control "Moved the head; the result read older."
    run currency "read the results of $root" results --issue "$root"
    grep -qE "^- c1 \(before_merge\): pass at ${commit_two:0:12} by tester, current\." "$work/out" ||
      fail currency "the newest result reads current" "$(<"$work/out")"
    pass currency "the result at the pull request's head reads current"
  fi
fi

# --- close: the issues the run wrote -------------------------------------------------------------

close_issue close "$root"
close_issue close "$other"
close_issue close "$both"
