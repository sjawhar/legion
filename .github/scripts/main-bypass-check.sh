#!/usr/bin/env bash
# main-bypass-check is the decision half of the bypass check
# (.github/workflows/main-bypass-check.yaml) and performs none of it: given the head of a push to
# main, the ci.yaml merge-group runs at that head and its ci-ok check runs, it says whether the
# head reached main untested. The workflow's step starts ci.yaml on that line; this script makes
# no GitHub call and reads every fact from its environment, so a test drives it from fixtures
# with no network.
#
# What there is to catch: the merge queue tests every change on the tree it lands as, and ci.yaml
# runs on no push (its header says why), so a commit that reached main another way — an admin
# merge, a direct push, a ruleset bypass — was tested by nothing at all. This decision is what
# finds one, and the run the workflow then starts is what tests it.
#
# Why the push's head alone answers for the whole push: a ci.yaml merge-group run at the head
# tested the tree that head names, which holds every commit under it, so nothing a push whose
# head the queue tested carries went untested. And the queue lands a group by pushing that
# group's head onto main, every commit it lands being the head of a group it tested, so a push
# that came from the queue has such a head. A push carrying anything the queue never saw
# therefore arrives with a head that is the head of no passing merge-group run, which is what
# this reads.
#
# When the head counts as tested, both of:
#   1. it is the head of a ci.yaml merge-group run, and
#   2. it carries a ci-ok check run that concluded success.
# Neither alone is enough: a merge-group run that failed or was cancelled leaves no passing ci-ok,
# and a passing ci-ok on a commit that is the head of no merge-group run came from somewhere else.
# A ci-ok posted by the bypass check's own dispatched run never satisfies rule 1, because such a
# run's commit is the head of no merge-group run. That is deliberate, and it starts no second
# dispatch, because this check runs on a push alone and the run it dispatches pushes nothing.
#
# A head whose own message carries a skip marker is passed over, whatever its checks. GitHub
# reads those markers on the push's head too, and starts no workflow at all for such a push, this
# one included, so a marked head rarely reaches this decision; the rule is kept here because the
# only thing it can do wrong is start the most expensive run in this repository for a commit that
# asked for no CI at all. The markers are GitHub's own set, from its "Skipping workflow runs"
# page: `[skip ci]`, `[ci skip]`,
# `[no ci]`, `[skip actions]` and `[actions skip]` anywhere in the message, and the trailer
# `skip-checks:true` or `skip-checks: true` at the end of it. The page's rule for that trailer,
# in its words: "The trailers section should be included at the end of your commit message and be
# preceded by two empty lines. If you already have other trailers in your commit message,
# `skip-checks` should be last." The rule this script implements, in its own: the words count
# when they are the message's last non-empty line and that line ends the message's last block of
# non-empty lines, where every line of the block is a trailer — `token: value`, the token made of
# letters, digits and hyphens, or a folded continuation, which begins with a space or a tab and
# is never the block's own first line, since the line such a first line would continue is above
# the empty line and no part of the block — and an empty line comes before the block, which is
# therefore never the subject's own line. So the
# trailers a commit already carries (`Co-authored-by:`, `Omp-Session:`, `Signed-off-by:`) may
# stand above `skip-checks`, as the page says they may, and that commit is still passed over: one
# empty line before the block is enough, because git's own cleanup, which the same page names,
# folds the two a person writes into one before the message reaches GitHub.
# The words anywhere else — quoted in a body, written into the subject, followed by another
# paragraph, or in a last block that also holds a line of prose — are no trailer here: GitHub
# started every workflow for such a commit, so it reached main past the queue like any other
# commit and is read here. The prose case is the one shape where this rule is narrower than
# git's own parsing, which reads a last block of prose and trailers as trailers when one of them
# is git-generated (measured with `git interpret-trailers --parse`, git 2.47.3: a block of prose
# and `skip-checks: true` parses as no trailer at all, and the same block with a
# `Signed-off-by:` line parses as two); that costs one dispatched run on a message shape
# nothing in this repository writes, and misses no untested commit.
# This repository's release, rebuild and changelog-fold pushes put a bracket marker in the commit
# subject, so the whole message is read, subject and body. The spelling is GitHub's, letter for
# letter: a marker in another letter case reads here as no marker, which costs one dispatched run
# and misses nothing.
#
# Its inputs, from the step's env:
#   HEAD_SHA      the sha of the push's head, from `github.event.after`.
#   HEAD_MESSAGE  that commit's whole message, from `github.event.head_commit.message`: read for
#                 the skip marker, and its subject quoted in the reasoning. A payload that
#                 carries no head_commit leaves it empty, which holds no marker, so the head is
#                 read by its runs and checks like any other — the side that starts the run.
#   RUNS          a JSON array of the ci.yaml merge-group runs at HEAD_SHA, each {"conclusion":
#                 "success|failure|cancelled|null", "headSha": "…", "workflowName": "…"}; a head
#                 with no such run is []. From:
#                   gh run list --repo "$GITHUB_REPOSITORY" --workflow ci.yaml --event merge_group \
#                     --commit "$HEAD_SHA" --json conclusion,headSha,workflowName
#                 The `--workflow ci.yaml` is what holds that list to this repository's router;
#                 this script reads each run's headSha and puts its workflowName in the reasoning.
#   CI_OK         a JSON array of HEAD_SHA's ci-ok check runs, each {"name": "…", "conclusion":
#                 "success|failure|…|null"}; a head with no ci-ok is []. From:
#                   gh api "repos/$GITHUB_REPOSITORY/commits/$HEAD_SHA/check-runs?check_name=ci-ok&filter=all&per_page=100" \
#                     --jq '[.check_runs[] | {name, conclusion}]'
#                 `filter=all` is required: the default, `latest`, returns one check run per name,
#                 and a commit can carry two — a failing one from the run whose job failed, and a
#                 passing one from a later run of the same group. `per_page=100` is required too:
#                 the default page holds the 30 newest check runs of the commit, and a commit the
#                 queue tested more than once can carry more than that.
#
# What it reads is the push's head and nothing else: no other commit of the push, no merge-group
# run or ci-ok check run at one, and no compare route for a payload GitHub cut at its documented
# 2048-commit cap. The head answers for all of them, so the check costs two API calls per push
# however many commits the push carries.
#
# It prints one line on stdout, `dispatch <sha>`, naming the head when it reached main untested;
# a head the queue tested, or one carrying a skip marker, prints nothing. The reasoning — what
# was read of the head and what that made of it — goes to stderr, which the workflow puts in the
# job log and the job summary. It exits 0 whenever it could decide, and non-zero only on a bad
# input, which it names.
#
# Run from anywhere:
#   HEAD_SHA=... HEAD_MESSAGE=... RUNS=... CI_OK=... .github/scripts/main-bypass-check.sh
# Its tests are in main-bypass-check.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

fail() {
  echo "::error::$1" >&2
  exit 1
}

for input in HEAD_SHA RUNS CI_OK; do
  if [ -z "${!input:-}" ]; then
    fail "main-bypass-check.sh needs $input in its environment"
  fi
done
# HEAD_MESSAGE is the one input that may be empty: a payload with no head_commit carries no
# message, and no message carries no marker.
HEAD_MESSAGE=${HEAD_MESSAGE:-}

[[ $HEAD_SHA =~ ^[0-9a-f]{7,40}$ ]] || fail "HEAD_SHA is no commit sha: $HEAD_SHA"

for input in RUNS CI_OK; do
  jq -e 'type == "array" and all(.[]; type == "object")' <<<"${!input}" >/dev/null 2>&1 ||
    fail "$input is not a JSON array of what was read at the push's head: ${!input}"
done

# One row per line, tab-separated: `out` and the token for stdout, or `log` and a line of the
# reasoning for stderr, in the order the caller must act on them.
read -r -d '' program <<'JQ' || true
def markers: ["[skip ci]", "[ci skip]", "[no ci]", "[skip actions]", "[actions skip]"];

# The skip marker this message carries, or null: one of GitHub's five bracket markers anywhere in
# it, or the skip-checks trailer. The trailer is that line and only that line when it is the
# message's last non-empty one and it ends the message's last block of non-empty lines, every
# line of which is a trailer, with an empty line before the block (the header says why, and
# quotes GitHub's rule). A block that starts at the first line is the subject's own and is no
# trailer section, and neither is one whose own first line is a folded continuation: the line
# such a first line would continue is above the empty line and no part of the block, which is
# how git reads it too. Trailing blanks and a carriage return, which a message from a Windows
# client can carry, are not part of a line.
def trailer_line: test("^[A-Za-z0-9-]+:") or test("^[ \t]");
def skip_trailer:
  (split("\n") | map(sub("[[:space:]]+$"; ""))) as $lines
  | ([$lines | to_entries[] | select(.value != "") | .key] | last) as $last
  # The block's first line: the one after the last empty line above the last non-empty one.
  | ((([$lines[0:($last // 0)] | to_entries[] | select(.value == "") | .key] | last) // -1) + 1) as $start
  | if $last == null or $last == 0 or $start == 0 then null
    elif ($lines[$start:($last + 1)] | all(trailer_line) | not) then null
    elif ($lines[$start] | test("^[ \t]")) then null
    elif $lines[$last] == "skip-checks:true" or $lines[$last] == "skip-checks: true" then $lines[$last]
    else null
    end;
def skip_marker:
  . as $message
  | [markers[] | . as $marker | select($message | contains($marker))]
    + [$message | skip_trailer | select(. != null)]
  | first;

# A payload that carried no message leaves nothing to quote, and the reasoning says so rather
# than printing an empty pair of brackets.
def subject:
  (split("\n") | first // "")
  | if . == "" then "no commit message in the push payload" else . end;

def runs_said($heads):
  if ($heads | length) == 0 then
    "it is the head of no ci.yaml merge-group run"
  else
    "it is the head of \($heads | length) ci.yaml merge-group run(s) (\($heads
      | map("\(.workflowName // "no workflow name") \(.conclusion // "not finished")")
      | join(", ")))"
  end;

def checks_said($checks):
  if ($checks | length) == 0 then
    "it carries no ci-ok check run"
  else
    "its \($checks | length) ci-ok check run(s) concluded \($checks
      | map(.conclusion // "not finished") | join(", "))"
  end;

($message | skip_marker) as $marker
| [$runs[] | select(.headSha == $sha)] as $heads
| (($heads | length) > 0 and ($ci_ok | any(.conclusion == "success"))) as $tested
| if $marker != null then
    ["log", "commit \($sha) (\($message | subject)) carries the skip marker \($marker), so no run was ever expected for it: passed over"],
    ["log", "this push's head carries a skip marker, so no run is started"]
  elif $tested then
    ["log", "commit \($sha) (\($message | subject)): \(runs_said($heads)) and \(checks_said($ci_ok)): tested"],
    ["log", "this push's head was tested on the merge queue: no run is started"]
  else
    ["log", "commit \($sha) (\($message | subject)): \(runs_said($heads)) and \(checks_said($ci_ok)): not tested"],
    ["log", "commit \($sha) reached this branch untested, so ci.yaml is started on this branch by workflow_dispatch: that run tests the branch's tip, which contains it."],
    ["out", "dispatch \($sha)"]
  end
| @tsv
JQ
rows=$(jq -rn --arg sha "$HEAD_SHA" --arg message "$HEAD_MESSAGE" \
  --argjson runs "$RUNS" --argjson ci_ok "$CI_OK" "$program")

while IFS=$'\t' read -r kind text; do
  case $kind in
    out) echo "$text" ;;
    log) echo "$text" >&2 ;;
  esac
done <<<"$rows"
