#!/usr/bin/env bash
# main-bypass-check is the decision half of the bypass check
# (.github/workflows/main-bypass-check.yaml) and performs none of it: given the commits of a push
# to main and, for each commit, the ci.yaml merge-group runs at it and its ci-ok check runs, it
# says whether a commit reached main untested and names the first one. The workflow's step starts
# ci.yaml on that line; this script makes no GitHub call and reads every fact from its
# environment, so a test drives it from fixtures with no network.
#
# What there is to catch: the merge queue tests every change on the tree it lands as, and ci.yaml
# runs on no push (its header says why), so a commit that reached main another way — an admin
# merge, a direct push, a ruleset bypass — was tested by nothing at all. This decision is what
# finds one, and the run the workflow then starts is what tests it.
#
# When a commit counts as tested, both of:
#   1. it is the head of a ci.yaml merge-group run, and
#   2. it carries a ci-ok check run that concluded success.
# Neither alone is enough: a merge-group run that failed or was cancelled leaves no passing ci-ok,
# and a passing ci-ok on a commit that is the head of no merge-group run came from somewhere else.
# A ci-ok posted by the bypass check's own dispatched run never satisfies rule 1, because such a
# run's commit is the head of no merge-group run. That is deliberate, and it starts no second
# dispatch, because the next push carries different commits.
#
# A commit whose own message carries a skip marker is passed over, whatever its checks. The
# markers are GitHub's own set, from its "Skipping workflow runs" page: `[skip ci]`, `[ci skip]`,
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
#   COMMITS  a JSON array of the push's commits, oldest first, each {"id": "<sha>", "message":
#            "<the whole message>"}; any other field is ignored. From
#            `.commits` of the push payload, read from $GITHUB_EVENT_PATH, or from the compare
#            API when GitHub cut that array short (the workflow says when and how).
#   RUNS     a JSON object mapping each commit's sha to the ci.yaml merge-group runs at that
#            commit, each {"conclusion": "success|failure|cancelled|null", "headSha": "…",
#            "workflowName": "…"}; a sha with no such run maps to []. From, over the shas of
#            COMMITS:
#              gh run list --repo "$GITHUB_REPOSITORY" --workflow ci.yaml --event merge_group \
#                --commit "$sha" --json conclusion,headSha,workflowName
#            The `--workflow ci.yaml` is what holds that list to this repository's router; this
#            script reads each run's headSha and puts its workflowName in the reasoning.
#   CI_OK    a JSON object mapping each commit's sha to its ci-ok check runs, each {"name": "…",
#            "conclusion": "success|failure|…|null"}; a sha with no ci-ok maps to []. From, over
#            the same shas:
#              gh api "repos/$GITHUB_REPOSITORY/commits/$sha/check-runs?check_name=ci-ok&filter=all&per_page=100" \
#                --jq '[.check_runs[] | {name, conclusion}]'
#            `filter=all` is required: the default, `latest`, returns one check run per name, and
#            a commit can carry two — a failing one from the run whose job failed, and a passing
#            one from a later run of the same group. `per_page=100` is required too: the default
#            page holds the 30 newest check runs of the commit, and a commit the queue tested
#            more than once can carry more than that.
# RUNS and CI_OK must hold a reading for every commit of COMMITS, so that a gather that read
# nothing is a refusal here rather than a commit silently read as untested.
#
# It prints one line on stdout, `dispatch <sha>`, naming the first commit of the push that was not
# tested; a push whose commits were all tested or all carry a marker prints nothing. The reasoning
# — every commit it read, what was read for it, and what that made of it — goes to stderr, which
# the workflow puts in the job log and the job summary. It exits 0 whenever it could decide, and
# non-zero only on a bad input, which it names.
#
# The decision stops at that first untested commit: every commit after it is read, since the
# workflow gathers both readings for every commit of the push and this script holds them all,
# but none of them is judged and the reasoning names none of them — one dispatched run tests
# main's tip, which contains every commit of the push, so what they carry changes nothing.
#
# Run from anywhere:
#   COMMITS=... RUNS=... CI_OK=... .github/scripts/main-bypass-check.sh
# Its tests are in main-bypass-check.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

fail() {
  echo "::error::$1" >&2
  exit 1
}

for input in COMMITS RUNS CI_OK; do
  if [ -z "${!input:-}" ]; then
    fail "main-bypass-check.sh needs $input in its environment"
  fi
done

jq -e 'type == "array" and all(.[];
  type == "object" and (.id | type) == "string" and (.message | type) == "string")' \
  <<<"$COMMITS" >/dev/null 2>&1 ||
  fail "COMMITS is not a JSON array of the push's commits, each with a string id and a string message: $COMMITS"
bad_shas=$(jq -r '[.[] | select((.id | test("^[0-9a-f]{7,40}$")) | not) | .id] | join(", ")' <<<"$COMMITS")
[ -z "$bad_shas" ] || fail "COMMITS holds an id that is no commit sha: $bad_shas"

for input in RUNS CI_OK; do
  jq -e 'type == "object" and all(.[]; type == "array" and all(.[]; type == "object"))' \
    <<<"${!input}" >/dev/null 2>&1 ||
    fail "$input is not a JSON object mapping a commit sha to an array of what was read for it: ${!input}"
done
unread=$(jq -r --argjson runs "$RUNS" --argjson ci_ok "$CI_OK" \
  '[.[] | .id as $id | select(($runs | has($id) | not) or ($ci_ok | has($id) | not)) | $id] | unique | join(", ")' <<<"$COMMITS")
[ -z "$unread" ] ||
  fail "RUNS and CI_OK need a reading for every commit of COMMITS, and one of them holds none for: $unread"

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

def subject: split("\n") | first // "";

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

[.[]
 | . as $commit
 | ($commit.message | skip_marker) as $marker
 | [$runs[$commit.id][] | select(.headSha == $commit.id)] as $heads
 | $ci_ok[$commit.id] as $checks
 | {commit: $commit, marker: $marker, heads: $heads, checks: $checks,
    tested: (($heads | length) > 0 and ($checks | any(.conclusion == "success")))}]
as $read
| ([$read | to_entries[]
    | select(.value.marker == null and (.value.tested | not)) | .key] | first) as $first
| (if ($read | length) == 0 then
     ["log", "this push carries no commit, so there is nothing to read and nothing is started"]
   else
     ($read[0:(($first // (($read | length) - 1)) + 1)][]
      | if .marker != null then
          ["log", "commit \(.commit.id) (\(.commit.message | subject)) carries the skip marker \(.marker), so no run was ever expected for it: passed over"]
        elif .tested then
          ["log", "commit \(.commit.id) (\(.commit.message | subject)): \(runs_said(.heads)) and \(checks_said(.checks)): tested"]
        else
          ["log", "commit \(.commit.id) (\(.commit.message | subject)): \(runs_said(.heads)) and \(checks_said(.checks)): not tested"]
        end)
   end),
  (if $first == null then
     (if ($read | length) == 0 then
        empty
      else
        ["log", "every commit of this push was tested on the merge queue or carries a skip marker: no run is started"]
      end)
   else
     ["log", "commit \($read[$first].commit.id) reached this branch untested, so ci.yaml is started on this branch by workflow_dispatch: that run tests the branch's tip, which contains it. The \((($read | length) - $first - 1)) commit(s) after it are left unjudged, because that one run tests them too."],
     ["out", "dispatch \($read[$first].commit.id)"]
   end)
| @tsv
JQ
rows=$(jq -r --argjson runs "$RUNS" --argjson ci_ok "$CI_OK" "$program" <<<"$COMMITS")

while IFS=$'\t' read -r kind text; do
  case $kind in
    out) echo "$text" ;;
    log) echo "$text" >&2 ;;
  esac
done <<<"$rows"
