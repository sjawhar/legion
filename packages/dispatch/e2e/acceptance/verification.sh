#!/usr/bin/env bash
# Drives Dispatch's verification rules end to end against a running server, as an agent sees them:
# a `plan_task` and a `check` that serve a spec's `requirement`, and the `serves` and `needs` edges
# `GET /api/v1/references` lists for them; every attribute a `check` carries, read back by
# `dispatch read --ref`; the refusals a pointer earns — a block no document holds, a block of an
# issue in another tree, a `needs` naming a block outside its own plan — and a `when` the schema
# does not know; a requirement's id through a move; `dispatch result` and `dispatch results`, with
# an unknown check and a pass that counts a skipped case refused; the one write a closed issue
# takes, a result for an `after_merge` check, beside the two it still refuses; the coverage flags
# `dispatch read --issue` prints, cleared by a check of the issue and by a check of a child, and
# back when that check is deleted, with the missing plan task named only once a plan document
# exists; the gap an approval request names and the covered spec that earns none; `dispatch
# plan-approve`, refused on a spec, and `dispatch request-approval`, refused on a plan; and the one
# request that names a spec and its Verification document together; a person's review and the CLI
# pin the fetched version's UTF-8 bytes, even after the latest version changes; and, with a
# repository mapped, the newest result of a check read whole — all forty characters of its commit,
# so a commit that only reads like the head is told apart from it — beside a result at a commit
# that repository does not hold refused.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), and optionally FAKE_GITHUB_PORT (the
# harness's fake GitHub, packages/dispatch/e2e/fake-github.ts, which the server reads as
# DISPATCH_GITHUB_API_BASE: the run gives it one repository holding one pull request and one
# commit beside its head, replacing every repository it served, so a result recorded at that pull
# request's head reads `current`, one at a commit the repository holds is stored, and one at a
# commit it does not hold is refused `400 RESULT_COMMIT_UNKNOWN`; unset, one `SKIP: currency …`
# line stands in for the section, which no other way of running has a pull request's head to
# read) and DEV_SIGNIN_LOGIN (a login the server's dev sign-in route, /auth/_dev/signin,
# signs in: the local harness has it, a deployed server does not). Recording a review is a person's
# write alone, so the two review sections sign that person in with a cookie jar; with
# DEV_SIGNIN_LOGIN unset each prints one `SKIP: … POST /api/v1/artifacts/{id}/reviews needs a
# person's credential holding the approve scope …` line and every other section runs.
#
# The run reads PROJECT's repository mappings once, in its setup, since every result it records
# names a commit the repository of its issue must hold. It reads each mapping under the name its
# repository is called today: a mapping may carry `renamed_to`, the full `owner/name` the
# repository was renamed to, and Dispatch canonicalises every repository through it, so that is
# the name Dispatch asks GitHub about and the name this run must drive. The rename rule is one
# step, so a mapping's own name is the canonical one only where it names no rename. Where PROJECT
# maps no default repository, its issues resolve to none, nothing is ever asked about them, and
# the commits are the synthetic ones below, the currency section mapping a repository of its own
# for the one check that needs a pull request. Where PROJECT already maps one, every issue this
# script writes resolves to that repository, so the setup reads the repository's default-branch
# head from GitHub with gh and every result is recorded at that head: no check stands down. A
# repository gh cannot read fails the run, naming it. Where FAKE_GITHUB_PORT is set the setup
# gives the fake, once and for the whole run, the one repository the run drives — the mapped one,
# or the currency section's own — with that head, a look-alike of it and a pull request, since a
# server reading the fake asks the fake and not GitHub; a server reading the real GitHub needs no
# such seeding, and a fake-backed run proves this script's fixture and its count rather than a
# server talking to GitHub.
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
       person; unset, those two sections are skipped;
       a PROJECT that maps a default repository needs gh, which reads that repository's
       default-branch head: every result is recorded at it)
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
require_commands bun curl jq sha256sum cut
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
# The wanted status is held under any name but `status`: that one holds the answer's own status,
# which `request` writes and `want_status` reads, and a local of that name here would take both
# writes and leave every status check comparing a value with itself.
refused_result() {
  local want_code="$5" want_status_code="$6"
  send POST "/api/v1/issues/$3/results" "$4"
  want_status "$want_status_code" "$1" "$2 is refused $want_status_code"
  expect "$1" "$2 is refused $want_code" "\"$want_code\"" '.code'
}

# The commits the run records at, where PROJECT maps no default repository: no issue this script
# writes resolves to a repository then, so no result's commit is ever asked about, and these are
# the script's own. Where one is mapped, the setup below replaces all three with that
# repository's default-branch head, which GitHub holds, and a look-alike of that head, which only
# the fake GitHub the setup seeds holds.
commit_one=1111111111111111111111111111111111111111
commit_two=2222222222222222222222222222222222222222
# The shape of the incident this rule came of: a commit whose first twelve characters are the
# head's, so a line printing twelve reads exactly like the head, and whose other twenty-eight are
# not. GitHub holds no such commit; the fake GitHub the setup seeds is given it, so the result at
# it is stored rather than refused, and the currency section that records it runs against that
# fake alone.
commit_lookalike=2222222222223333333333333333333333333333
# A commit no repository holds, which the record route must refuse.
readonly commit_unknown=4444444444444444444444444444444444444444

# --- setup ---------------------------------------------------------------------------------------

# PROJECT's default repository, read once and read by every section that records a result, under
# the name it is called today: where the mapping carries `renamed_to`, that is the repository's
# canonical name, the one Dispatch asks GitHub about, and GitHub answers 404 for the installation
# of a name a repository was renamed away from. The rename rule is one step, so the mapping's own
# name stands only where it names no rename. With one mapped, every issue of the project resolves
# to it, so Dispatch asks GitHub whether it holds the commit each result names and refuses the
# ones it does not. This run therefore records every result at that repository's own
# default-branch head, which gh reads from GitHub below: no check stands down, and a repository
# gh cannot read fails the run by name rather than skipping it.
get "/api/v1/settings/repo-projects"
want_status 200 setup "the repository mappings answer"
default_repo="$(jq -r --arg p "$project" \
  'first(.[] | select(.project == $p and .default) | (.renamed_to // .repo)) // ""' "$work/body")" ||
  fail setup "read the repository mappings" "$(body)"
readonly default_repo

if [[ -n "$default_repo" ]]; then
  command -v gh >/dev/null ||
    fail setup "read $default_repo's default-branch head" "gh is not on PATH, and every result this run records names that head"
  branch="$(gh api "repos/$default_repo" --jq .default_branch 2>"$work/gh")" && [[ -n "$branch" ]] ||
    fail setup "read $default_repo's default branch with gh" "$(<"$work/gh")"
  readonly branch
  head_commit="$(gh api "repos/$default_repo/commits/$branch" --jq .sha 2>"$work/gh")" ||
    fail setup "read $default_repo's $branch head with gh" "$(<"$work/gh")"
  [[ "$head_commit" =~ ^[0-9a-f]{40}$ ]] ||
    fail setup "read $default_repo's $branch head with gh" "gh answered '$head_commit', which is no commit"
  readonly head_commit
  commit_one="$head_commit"
  commit_two="$head_commit"
  # The look-alike of that head: its first twelve characters, then its other twenty-eight with
  # every character replaced, so the two read alike only where a line cut to twelve shows them.
  commit_lookalike="${head_commit:0:12}$(printf '%s' "${head_commit:12}" |
    tr '0123456789abcdef' '89abcdef01234567')"
  pass setup "read $default_repo's $branch head $head_commit with gh"
fi
readonly commit_one commit_two commit_lookalike

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

# The repository the run drives on the fake: the one PROJECT maps, where it maps one, since every
# issue this script writes then resolves to that repository, and otherwise one of this run's own,
# which the currency section maps as PROJECT's default.
currency_repo="${default_repo:-acme/verification-$$}"
readonly currency_repo

# A server reading the harness's fake GitHub asks the fake, not GitHub, whether that repository
# holds a result's commit, so the fake is given the repository here, once and for the whole run:
# the head (the one gh named, where PROJECT maps a repository, and this script's own otherwise),
# the look-alike beside it, and pull request 7 of the root issue, which is what makes a result at
# that head read `current` in the currency section below. A server reading the real GitHub needs
# no seeding of any kind — it reads the same repository gh did — and a fake-backed run proves
# this script's fixture and its count rather than a server talking to GitHub.
if [[ -n "$fake_github_port" ]]; then
  curl -sS -o /dev/null -X PUT -H 'Content-Type: application/json' --data-binary "$(
    jq -cn --arg r "$currency_repo" --arg k "$root" --arg sha "$commit_two" \
      --arg look "$commit_lookalike" \
      '{($r): {contents: "read", pull_requests: "read", installation_id: 256,
               commit_pulls: {($sha): [7]}, commits: [$look],
               pulls: {"7": {head_sha: $sha, state: "open", title: "\($k) the verification check"}}}}'
  )" "http://127.0.0.1:$fake_github_port/__fixture/repos" 2>"$work/curl" ||
    fail setup "seed the fake GitHub with $currency_repo" "$(<"$work/curl")"
  seeded_github=1
  pass setup "seeded the fake GitHub with $currency_repo, its head $commit_two, $commit_lookalike and pull request 7"
fi

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

# --- result: recording one, and the two refusals a result's own fields earn ----------------------

record result "record a pass for c1" "$root" --check c1 --role tester --verdict pass \
  --commit "$commit_one" --surface cli --run "bash e2e/acceptance/verification.sh" \
  --observed "every check passed" --negative-control "Deleted the check and the flag came back."
grep -qF "Pull request body line:" "$work/out" ||
  fail result "the record prints the pull-request body line" "$(<"$work/out")"
pass result "the record prints the pull-request body line"

run result "read the results of $root" results --issue "$root"
grep -qE "^- c1 \(before_merge\): pass at ${commit_one} by tester, " "$work/out" ||
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
    get "/api/v1/artifacts/$artifact/versions/1"
    want_status 200 review "$both's $document version 1 answers"
    jq -j .markdown "$work/body" >"$work/approved-$document"
    h1="$(sha256sum "$work/approved-$document" | cut -d' ' -f1)"
    expect review "$document version read names its own bytes" "\"$h1\"" '.sha256'
    person_send GET "/api/v1/artifacts/$artifact/reviews"
    want_status 200 review "$both's $document reviews answer"
    expect review "$both's $document carries one approved person's review" \
      '[["approved","person",1]]' '[.[] | [.state, .kind, .version]]'
    expect review "$both's $document review names the sha256 of the bytes approved" \
      "\"$h1\"" '.[0].content_sha256'
    expect review "$both's $document review was hashed at approval" '"insert"' '.[0].content_sha256_source'
  done

  artifact_id review "$both" spec.md
  h1="$(sha256sum "$work/approved-spec.md" | cut -d' ' -f1)"
  cp "$work/approved-spec.md" "$work/revised-spec.md"
  printf '\nA new line the person has not approved.\n' >>"$work/revised-spec.md"
  upload review "$both" spec.md "$work/revised-spec.md"
  get "/api/v1/artifacts/$artifact/versions/2"
  want_status 200 review "$both's spec version 2 answers"
  h2="$(jq -j .markdown "$work/body" | sha256sum | cut -d' ' -f1)"
  [[ "$h2" != "$h1" ]] || fail review "different version bytes have different hashes" "$h1 = $h2"
  expect review "version 2 read names its own bytes" "\"$h2\"" '.sha256'
  person_send GET "/api/v1/artifacts/$artifact/reviews"
  want_status 200 review "the person's review still answers after the edit"
  expect review "the review does not follow the latest bytes" "\"$h1\"" '.[0].content_sha256'
  get "/api/v1/artifacts/$artifact"
  want_status 200 review "the changed document answers"
  expect review "an unapproved edit leaves the approval stale" '"stale"' '.approval.state'
  expect review "the stale approval still names the approved bytes" "\"$h1\"" '.approval.content_sha256'
  run review "read the approved spec version through the CLI" doc-read --issue "$both" --version 1
  grep -qF "Content: sha256:$h1" "$work/out" || fail review "the CLI names version 1's bytes" "$(<"$work/out")"
  grep -qF "content sha256:$h1" "$work/out" || fail review "the CLI names the approval's bytes" "$(<"$work/out")"
  pass review "the CLI's approved-version content and approval hashes match"
  run review "read the changed spec version through the CLI" doc-read --issue "$both" --version 2
  grep -qF "Content: sha256:$h2" "$work/out" || fail review "the CLI names version 2's bytes" "$(<"$work/out")"
  pass review "the CLI's latest-version content differs from its stale approval"

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

# --- currency: the newest result, printed whole, and a commit the repository does not hold ------

if [[ -z "$fake_github_port" ]]; then
  skip currency "a result at the pull request's head reads current: FAKE_GITHUB_PORT is unset; only a server reading the harness's fake GitHub can be told a pull request's head"
else
  if [[ -z "$default_repo" ]]; then
    # PROJECT maps no repository of its own, so this section maps the one the setup gave the fake
    # — that repository, the head, the look-alike and pull request 7 — and every issue this run
    # wrote resolves to it from here on. Where PROJECT maps one, it is already this section's, so
    # nothing is mapped here.
    send PUT "/api/v1/settings/repo-projects/$currency_repo" \
      "$(jq -cn --arg s "$session" --arg p "$project" \
        '{actor: {kind: "session", id: $s}, project: $p, default: true}')"
    want_status 200 currency "map $currency_repo as $project's default repository"
    mapped="$currency_repo"
    pass currency "mapped $currency_repo as $project's default repository"
  fi

  # The incident, driven: a result at a commit that reads like the head, and then the head's
  # own results, the last of which is the one the read must name.
  record currency "record a pass at the commit that reads like the head" "$root" --check c1 \
    --role tester --verdict pass --commit "$commit_lookalike" --surface cli \
    --run "bash e2e/acceptance/verification.sh"
  record currency "record a pass at the pull request's head" "$root" --check c1 --role tester \
    --verdict pass --commit "$commit_two" --surface cli --run "bash e2e/acceptance/verification.sh"
  record currency "record the newest pass at the pull request's head" "$root" --check c1 \
    --role tester --verdict pass --commit "$commit_two" --surface cli \
    --run "bash e2e/acceptance/verification.sh" --observed "the last of the three results"
  run currency "read the results of $root" results --issue "$root"
  grep -qE "^- c1 \(before_merge\): pass at ${commit_two} by tester, current\." "$work/out" ||
    fail currency "the newest result reads current" "$(<"$work/out")"
  pass currency "the newest result reads current and prints all forty characters of the pull request's head, so the look-alike's own characters cannot stand in for it"

  refused_result currency "a result at a commit $currency_repo does not hold" "$root" \
    "$(jq -cn --arg s "$session" --arg c "$commit_unknown" \
      '{actor: {kind: "session", id: $s}, check: "c1", role: "tester", verdict: "pass", commit: $c}')" \
    RESULT_COMMIT_UNKNOWN 400
fi

# --- close: the issues the run wrote -------------------------------------------------------------

close_issue close "$root"
close_issue close "$other"
close_issue close "$both"
