#!/usr/bin/env bash
# The live proof, on GitHub itself, of what moving Legion's handoffs out of git does to a branch's
# required checks (INFRA-177: A8, A9's GitHub half, and the plan's D13). On sjawhar/legion-smoke,
# whose main requires the check `gate` (the `gate` job of .github/workflows/fail-on-demand.yml,
# which runs on every push and pull request), as the implement App this shell's gh acts as:
#   - negative-control: a code commit gets a `gate` check run; a `.legion/`-only commit above it
#     whose message ends with two empty lines and `skip-checks: true` - the handoff push of the
#     release before INFRA-177 - and the pull request opened on it get none within 10 minutes;
#   - import-removal: the commit `legion handoffs import` makes on that branch, removing all of
#     `.legion/` with GitHub's Git Data API and no trailer, moved there by a non-forced ref update,
#     gets a `gate` check run;
#   - empty-root-pull-request (D13): a branch whose only commit above main is empty and carries no
#     trailer - the commit a root whose code all lands through its children pushes - is opened as a
#     pull request by GitHub, and its head gets a `gate` check run.
# The removal makes the very calls handoffimport makes (packages/daemon/internal/handoffimport/
# handoffimport.go: branch.head, branch.removalSet, branch.removalCommit and branch.move), driven
# here with `gh api` so that no token leaves gh: the branch's ref, its head commit, the head's root
# tree, `.legion/`'s recursive tree, a tree on the root tree deleting each path under `.legion/`
# (sha null), a commit of it on the head, and PATCH of the ref with force false. Every commit is
# written the same way, through the Git Data API, so the run needs no clone.
#
# Run it from a checkout, with gh acting for the smoke repository's owner as an App installed there
# with contents and pull-request write (the implement App), and jq, as
#   bash scripts/e2e/handoff-cutover-github.sh
# Each check prints `== <name>`, what it observed and `ok <name>`; the first that fails ends the run
# non-zero, naming it. What each check holds is whether GitHub started the required check on a
# head: a `gate` check run on it, in any status. A skipped push gets none at all, while a started
# one is queued the moment it is pushed, and on the smoke account's Actions queue its result can
# take 20 to 40 minutes to come (each run's status when it is seen is printed). Each head is polled
# every 15 seconds for at most HANDOFF_CUTOVER_WAIT seconds (default 600), and the negative
# control's head is watched that whole window. A run takes about 12 minutes. Its branches are
# handoff-cutover/<run>/{negative,root} and its pull requests are titled with the run id; on any
# exit the workflow runs on its branches that have not finished are cancelled, so they hold no
# runner of the shared queue, both pull requests are closed and both branches deleted.
# HANDOFF_CUTOVER_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is unset,
# kept and printed) keeps the transcript and every GitHub answer the checks judged. No token is
# printed or exported: gh holds it.
set -euo pipefail
# A refusal inside a command substitution (the helpers below run in many) ends the run, not only
# the substitution.
shopt -s inherit_errexit

root=$(cd "$(dirname "$0")/../.." && pwd)
repo=sjawhar/legion-smoke
wait_seconds=${HANDOFF_CUTOVER_WAIT:-600}
run=$(date -u +%Y%m%d%H%M%S)-$$
key=HCUT${run//-/}
negative_branch=handoff-cutover/$run/negative
root_branch=handoff-cutover/$run/root
evidence=${HANDOFF_CUTOVER_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-handoff-cutover-evidence.XXXXXXXX")}
mkdir -p "$evidence"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

check=setup
started=$SECONDS
check_started=$SECONDS
begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }

# The run's own branches and pull requests: on any exit their unfinished workflow runs are
# cancelled, the pull requests closed and the branches deleted, each step best effort and each
# failure named.
branches=()
pulls=()
cleanup() {
  local status=$? number branch id runs
  # GitHub lists a push's workflow runs a few seconds after the push, so a run that ends right after
  # one waits for them before it reads what to cancel.
  [ "${#branches[@]}" -eq 0 ] || sleep 10
  for branch in "${branches[@]}"; do
    runs=$(gh api "repos/$repo/actions/runs?branch=$branch" --jq '.workflow_runs[] | select(.status != "completed") | .id' 2>/dev/null) || runs=
    for id in $runs; do
      gh api -X POST "repos/$repo/actions/runs/$id/cancel" >/dev/null 2>&1 &&
        echo "   cleanup: cancelled workflow run $id on $branch" ||
        echo "   cleanup: could not cancel workflow run $id on $branch" >&2
    done
  done
  for number in "${pulls[@]}"; do
    gh api -X PATCH "repos/$repo/pulls/$number" -f state=closed >/dev/null 2>&1 &&
      echo "   cleanup: closed pull request #$number" ||
      echo "   cleanup: could not close pull request #$number on $repo; close it by hand" >&2
  done
  for branch in "${branches[@]}"; do
    gh api -X DELETE "repos/$repo/git/refs/heads/$branch" >/dev/null 2>&1 &&
      echo "   cleanup: deleted branch $branch" ||
      echo "   cleanup: could not delete branch $branch on $repo; delete it by hand" >&2
  done
  if [ "$status" -eq 0 ]; then
    echo "handoff cutover github e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "handoff cutover github e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}
trap cleanup EXIT

# github METHOD PATH [JSON] sends one REST call through gh, the JSON body (when given) on stdin, and
# prints GitHub's answer; a refusal fails the check with GitHub's own words.
github() {
  local method=$1 path=$2 body=${3:-} answer
  if [ -n "$body" ]; then
    answer=$(gh api -X "$method" "$path" --input - <<<"$body" 2>&1) || fail "GitHub refused $method $path: $answer"
  else
    answer=$(gh api -X "$method" "$path" 2>&1) || fail "GitHub refused $method $path: $answer"
  fi
  printf '%s' "$answer"
}

# commit_on PARENT MESSAGE TREE_JSON writes a tree of TREE_JSON's entries on PARENT's tree (none:
# PARENT's tree itself) and a commit of it with MESSAGE whose one parent is PARENT, and prints the
# commit's sha.
commit_on() {
  local parent=$1 message=$2 entries=$3 tree
  tree=$(github GET "repos/$repo/git/commits/$parent" | jq -r .tree.sha)
  if [ "$entries" != "[]" ]; then
    tree=$(github POST "repos/$repo/git/trees" "$(jq -n --arg base "$tree" --argjson tree "$entries" '{base_tree: $base, tree: $tree}')" | jq -r .sha)
  fi
  github POST "repos/$repo/git/commits" "$(jq -n --arg m "$message" --arg t "$tree" --arg p "$parent" '{message: $m, tree: $t, parents: [$p]}')" | jq -r .sha
}

# create_branch BRANCH SHA creates the run's branch at SHA, as a push of it does, and records it for
# the cleanup.
create_branch() {
  github POST "repos/$repo/git/refs" "$(jq -n --arg ref "refs/heads/$1" --arg sha "$2" '{ref: $ref, sha: $sha}')" >/dev/null
  branches+=("$1")
}

# open_pull BRANCH TITLE opens a pull request from BRANCH into main, records it for the cleanup, and
# leaves its number in pull_number; GitHub's refusal fails the check with its words. It runs in the
# run's own shell, never in a command substitution, whose subshell would keep the record from the
# cleanup.
pull_number=
open_pull() {
  pull_number=$(github POST "repos/$repo/pulls" "$(jq -n --arg head "$1" --arg title "$2" \
    --arg body "INFRA-177's handoff cutover proof, run $run (scripts/e2e/handoff-cutover-github.sh). It is closed and its branch deleted when the run ends; nothing here is for review." \
    '{head: $head, base: "main", title: $title, body: $body}')" | jq -r .number)
  pulls+=("$pull_number")
}

# gate_runs SHA prints each `gate` check run on SHA as `<status>/<conclusion> <url>`, and saves
# GitHub's answer in the evidence directory.
gate_runs() {
  local answer
  answer=$(github GET "repos/$repo/commits/$1/check-runs?check_name=gate")
  printf '%s\n' "$answer" >"$evidence/check-runs-$1.json"
  jq -r '.check_runs[] | "\(.status)/\(.conclusion // "none") \(.html_url)"' <<<"$answer"
}

# await_gate SHA WHAT waits up to wait_seconds for a `gate` check run on SHA, in any status: GitHub
# started the required check on that head.
await_gate() {
  local sha=$1 what=$2 since=$SECONDS runs
  while :; do
    runs=$(gate_runs "$sha")
    if [ -n "$runs" ]; then
      note "$what $sha: GitHub started gate after $((SECONDS - since))s: $(head -1 <<<"$runs")"
      return 0
    fi
    ((SECONDS - since < wait_seconds)) || fail "$what $sha has no gate check run after ${wait_seconds}s"
    sleep 15
  done
}

# assert_no_gate SHA WHAT watches SHA for the whole wait_seconds window and fails at the first
# `gate` check run on it, in any status, or the first workflow run GitHub made for it.
assert_no_gate() {
  local sha=$1 what=$2 since=$SECONDS runs workflows
  while ((SECONDS - since < wait_seconds)); do
    runs=$(gate_runs "$sha")
    [ -z "$runs" ] || fail "$what $sha got a gate check run after $((SECONDS - since))s: $runs"
    workflows=$(github GET "repos/$repo/actions/runs?head_sha=$sha" | jq -r '.workflow_runs[] | "\(.name) \(.event) \(.status)"')
    [ -z "$workflows" ] || fail "$what $sha got workflow runs after $((SECONDS - since))s: $(tr '\n' ';' <<<"$workflows")"
    sleep 15
  done
  note "$what $sha: no gate check run and no workflow run in ${wait_seconds}s; check runs on it: $(github GET "repos/$repo/commits/$sha/check-runs" | jq -r '.total_count')"
}

begin prerequisites
for tool in gh jq; do command -v "$tool" >/dev/null || fail "$tool is not on PATH"; done
viewer=$(gh api graphql -f query='{viewer{login}}' --jq .data.viewer.login 2>&1) || fail "gh answers no viewer: $viewer"
case $viewer in *"[bot]") ;; *) fail "gh acts as $viewer, not a GitHub App: run it where gh acts as the implement App" ;; esac
permissions=$(github GET "repos/$repo" | jq -c .permissions)
note "gh acts as $viewer; GET repos/$repo .permissions: $permissions"
if ! jq -e '.push == true' <<<"$permissions" >/dev/null; then
  # GitHub fills a repository's permissions for a user's token only: for an App installation token
  # every one reads false. The installation's repositories list is that token's own answer, and the
  # first write below (the branch) is the push itself, whose refusal fails the run by GitHub's words.
  installed=$(gh api --paginate installation/repositories --jq '.repositories[].full_name' 2>&1) ||
    fail "$repo shows no push permission and gh is no App installation that lists it: $installed"
  grep -qxF "$repo" <<<"$installed" || fail "$repo shows no push permission and $viewer's installation does not list it"
  note "an App installation token, for which GitHub fills no permissions; its installation lists $repo, and the first branch write is the push"
fi
rules=$(github GET "repos/$repo/rules/branches/main")
printf '%s\n' "$rules" >"$evidence/rules-main.json"
jq -e '[.[] | select(.type == "required_status_checks") | .parameters.required_status_checks[].context] | index("gate") != null' <<<"$rules" >/dev/null ||
  fail "$repo main does not require the check gate: $(jq -c '[.[] | {type, parameters}]' <<<"$rules")"
note "GET repos/$repo/rules/branches/main: required_status_checks $(jq -c '[.[] | select(.type == "required_status_checks") | .parameters.required_status_checks[]]' <<<"$rules")"
base=$(github GET "repos/$repo/git/ref/heads/main" | jq -r .object.sha)
note "run $run, issue key $key, main at $base, branches $negative_branch and $root_branch, evidence $evidence"
pass

begin negative-control
code=$(commit_on "$base" "handoff cutover proof $run: a code change" \
  "$(jq -n --arg path "handoff-cutover/$run.txt" --arg c "run $run" '[{path: $path, mode: "100644", type: "blob", content: $c}]')")
create_branch "$negative_branch" "$code"
note "pushed $negative_branch at the code commit $code (handoff-cutover/$run.txt)"
await_gate "$code" "the code commit"
handoff_message="plan: record handoff ($key)"$'\n\n\n'"skip-checks: true"
handoff=$(commit_on "$code" "$handoff_message" \
  "$(jq -n --arg path ".legion/$key/plan.json" --arg c "{\"schemaVersion\": 1, \"phase\": \"plan\", \"issue\": \"$key\"}" '[{path: $path, mode: "100644", type: "blob", content: $c}]')")
files=$(github GET "repos/$repo/commits/$handoff" | jq -r '.files[].filename')
[ "$files" = ".legion/$key/plan.json" ] || fail "the handoff commit $handoff changes $(tr '\n' ' ' <<<"$files"), want .legion/$key/plan.json alone"
tail=$(github GET "repos/$repo/git/commits/$handoff" | jq -r '.message | sub("\n+$"; "")' | tail -3 | tr '\n' '|')
[ "$tail" = "||skip-checks: true|" ] || fail "the handoff commit $handoff's message ends '$tail', want two empty lines then skip-checks: true"
github PATCH "repos/$repo/git/refs/heads/$negative_branch" "$(jq -n --arg sha "$handoff" '{sha: $sha, force: false}')" >/dev/null
note "pushed the .legion/-only commit $handoff above it (files: $files; message ends with two empty lines and skip-checks: true)"
open_pull "$negative_branch" "Handoff cutover proof $run: a branch whose head is a handoff push"
negative_pull=$pull_number
note "opened pull request #$negative_pull on $negative_branch at $handoff"
assert_no_gate "$handoff" "the handoff push"
pass

begin import-removal
# branch.head: the branch's ref.
head=$(github GET "repos/$repo/git/ref/heads/$negative_branch" | jq -r .object.sha)
[ "$head" = "$handoff" ] || fail "$negative_branch is at $head, not the handoff commit $handoff"
# branch.removalSet: the head's root tree, its .legion entry, and every non-tree path below it.
root_tree=$(github GET "repos/$repo/git/commits/$head" | jq -r .tree.sha)
legion_tree=$(github GET "repos/$repo/git/trees/$root_tree" | jq -r '.tree[] | select(.path == ".legion" and .type == "tree") | .sha')
[ -n "$legion_tree" ] || fail "the tree of $head has no .legion/ directory"
below=$(github GET "repos/$repo/git/trees/$legion_tree?recursive=1")
jq -e '.truncated == false' <<<"$below" >/dev/null || fail "GitHub truncated its list of .legion/ at $head"
deletions=$(jq -c '[.tree[] | select(.type != "tree") | {path: (".legion/" + .path), mode, type, sha: null}]' <<<"$below")
[ "$deletions" != "[]" ] || fail "GitHub listed nothing under .legion/ at $head"
# branch.removalCommit: the tree on the root tree without those paths, and its commit on the head,
# with no trailer and no bracket keyword.
removal_tree=$(github POST "repos/$repo/git/trees" "$(jq -n --arg base "$root_tree" --argjson tree "$deletions" '{base_tree: $base, tree: $tree}')" | jq -r .sha)
removal_message="chore(legion): $key's handoffs are on its Dispatch issue now"
removal=$(github POST "repos/$repo/git/commits" "$(jq -n --arg m "$removal_message" --arg t "$removal_tree" --arg p "$head" '{message: $m, tree: $t, parents: [$p]}')" | jq -r .sha)
# branch.move: the non-forced ref update.
github PATCH "repos/$repo/git/refs/heads/$negative_branch" "$(jq -n --arg sha "$removal" '{sha: $sha, force: false}')" >/dev/null
note "removed .legion/ as handoffimport does: tree $removal_tree on $root_tree deleting $(jq -r '[.[].path] | join(", ")' <<<"$deletions") (sha null), commit $removal \"$removal_message\" on $head, PATCH refs/heads/$negative_branch force false"
if gh api "repos/$repo/contents/.legion?ref=$removal" >/dev/null 2>&1; then
  fail "the removal commit $removal still carries .legion/"
fi
# GitHub moves the pull request's head behind the ref update, a few seconds later.
for _ in $(seq 1 24); do
  pr_head=$(github GET "repos/$repo/pulls/$negative_pull" | jq -r .head.sha)
  [ "$pr_head" != "$removal" ] || break
  sleep 5
done
[ "$pr_head" = "$removal" ] || fail "pull request #$negative_pull's head is $pr_head, not the removal commit $removal, 120s after the ref update"
note "GET repos/$repo/contents/.legion?ref=$removal is 404; pull request #$negative_pull's head is $removal"
await_gate "$removal" "the removal commit"
pass

begin empty-root-pull-request
empty=$(commit_on "$base" "$key: the root of its children; no code of its own" "[]")
[ "$(github GET "repos/$repo/git/commits/$empty" | jq -r .tree.sha)" = "$(github GET "repos/$repo/git/commits/$base" | jq -r .tree.sha)" ] ||
  fail "the empty commit $empty's tree is not main's"
create_branch "$root_branch" "$empty"
note "pushed $root_branch at the empty commit $empty (main's tree, no trailer) above main $base"
answer=$(gh api -X POST "repos/$repo/pulls" --input - 2>&1 <<<"$(jq -n --arg head "$root_branch" --arg title "Handoff cutover proof $run: a root with no code of its own" \
  --arg body "INFRA-177's handoff cutover proof, run $run (scripts/e2e/handoff-cutover-github.sh): D13, a pull request whose only commit is empty. It is closed and its branch deleted when the run ends." \
  '{head: $head, base: "main", title: $title, body: $body}')") ||
  fail "GitHub refused to open a pull request whose only commit is empty (D13 does not hold; report it to the architect, do not work around it): $answer"
root_pull=$(jq -r .number <<<"$answer")
pulls+=("$root_pull")
jq -e --arg sha "$empty" '.head.sha == $sha and .commits == 1 and .changed_files == 0' <<<"$(github GET "repos/$repo/pulls/$root_pull")" >/dev/null ||
  fail "pull request #$root_pull is not the one empty commit $empty: $(github GET "repos/$repo/pulls/$root_pull" | jq -c '{head: .head.sha, commits, changed_files}')"
note "GitHub opened pull request #$root_pull at $empty: 1 commit, 0 changed files"
await_gate "$empty" "the empty root commit"
pass
