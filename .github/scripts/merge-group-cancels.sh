#!/usr/bin/env bash
# merge-group-cancels is the decision half of the merge queue's two cancel acts, and performs
# neither of them: given one ci.yaml merge-group run and the repository's ci.yaml merge-group runs,
# it says which runs must be cancelled and, for one of the two acts, whether the asking run must
# end itself. The workflow steps that cancel act on its lines; it makes no GitHub write, and reads
# every fact from its environment, so a test drives it from fixtures with no network.
#
# What there is to cancel: main's queue builds each group on top of the group below it, so when a
# group fails, every group built on it can no longer merge and the queue rebuilds them — while
# their ci.yaml merge-group runs, which nothing cancels, run to the end. That runner time is what
# the acts take back.
#
# The chain. A merge-group run's head branch is
# `gh-readonly-queue/<base>/pr-<pull request number>-<parent sha>`, and that parent sha is the head
# commit of the group below it: the head of that group's own ci.yaml merge-group run, or a commit
# of main when the group sits directly on main. So a step up the chain is taken only through a
# parent sha that is the head of a run this script can resolve, and the first parent that is not
# one ends the walk, because that commit is main's. The runs it can resolve are the runs of RUNS
# and the asking run, which HEAD_SHA and HEAD_BRANCH name on their own: `gh run list` lags seconds
# behind a run that has just started, and a walk that could not step through the asking run's own
# head would miss its direct child and state that absence as proven. A sha is visited once, so
# branch names that loop stop the walk instead of hanging it.
#
# Its inputs, from the step's env:
#   ACT          `failed` or `started`, the act to decide.
#   RUN_ID       the asking run's databaseId.
#   HEAD_SHA     the asking run's head commit.
#   HEAD_BRANCH  the asking run's head branch, a gh-readonly-queue name.
#   RUNS         a JSON array of this repository's ci.yaml merge-group runs, each
#                {"databaseId": <n>, "headSha": "…", "headBranch": "…", "status":
#                "queued|in_progress|completed", "conclusion": "success|failure|cancelled|null",
#                "createdAt": "<RFC3339>"}, in any order. Only `completed` counts as completed:
#                every other status GitHub gives a run (queued, in_progress, waiting, requested,
#                pending) is a run still holding or waiting for runners. From:
#                  gh run list --repo "$GITHUB_REPOSITORY" --workflow ci.yaml --event merge_group \
#                    --json databaseId,headSha,headBranch,status,conclusion,createdAt
#   CI_OK        a JSON object mapping a commit sha to that commit's ci-ok conclusion (success,
#                failure, …; a sha with no ci-ok is absent). The started act needs it; the failed
#                act does not read it and defaults it to {}. From, over the head shas of the runs
#                of RUNS that did not complete `success` — a run that completed `success` is no
#                failed group, and a head left out carries no ci-ok here, which dooms nothing.
#                `filter=all` is required, as it is in main-bypass-check.yaml's read of the same
#                check run: the default, `latest`, returns one check run per name, and a commit
#                can carry two — a failing one from the run whose job failed, and a passing one
#                from a later run of the same group. `per_page=100` is required with it: the
#                default page holds the 30 newest check runs of the commit. A commit that
#                carries both is no failed group, so a `success` among them is the conclusion
#                this map holds:
#                  jq -r '[.[] | select(.conclusion != "success") | .headSha] | unique[]' <<<"$RUNS" |
#                    while read -r sha; do
#                      gh api "repos/$GITHUB_REPOSITORY/commits/$sha/check-runs?check_name=ci-ok&filter=all&per_page=100" \
#                        --jq 'if any(.check_runs[]?; .conclusion == "success") then "success"
#                              else .check_runs[0].conclusion // empty end' |
#                        jq -R --arg sha "$sha" 'select(. != "") | {($sha): .}'
#                    done | jq -s 'add // {}'
#   REPORT       the failed act's whole decision, which the caller reads from this run's
#                artifacts before it asks: the report the job that fired the follower uploaded
#                for this run attempt (.github/actions/ci-ok-report uploads it as
#                `ci-ok-<job>-<run attempt>-<success|failure|cancelled>` before it posts
#                anything), or `absent` when it uploaded none, or `unknown` when GitHub refused
#                the list. The started act does not read it and needs none.
#
# ACT=failed — a job of the asking run reported `failure` or `cancelled` to its follower. That
# result says the job ended badly, not that its own work did: a job whose tests all passed ends
# `failure` when its ci-ok-report step cannot reach GitHub. REPORT is what tells the two apart,
# and the act has one answer for each:
#
#   * `failure` or `cancelled` — the job's own work failed, so this group cannot merge and
#     neither can any group built on it. It cancels every run of RUNS that has not completed
#     and whose chain of parents reaches HEAD_SHA, the nearest descendant first, and then the
#     asking run itself, whose id is the last `cancel` line: cancelling this run first would
#     kill the job that has the descendants to cancel. The same report step posted this group's
#     failing ci-ok before the follower started, so nothing here leaves the group unjudged.
#   * `absent` — the job uploaded no report, so this group carries no ci-ok of its own yet.
#     `cancel-changes` always meets this: `changes` has no ci-ok-report step, and the jobs that
#     post for a run whose `changes` did not succeed (lint, typecheck and test, whose `if:`
#     admits a `changes` that did not succeed) are still running when its follower fires. A job
#     whose artifact upload failed leaves the same absence. It cancels the descendants, which
#     are doomed whichever of the two it was, and never the asking run: force-cancelling it
#     would leave every `if: always()` step skipped and the group with no ci-ok at all, which
#     holds it in the queue until GitHub's 180-minute check timeout. What the lesser cost is:
#     the first of those jobs to report posts this group's failing ci-ok, and the others run on
#     to their own end with nothing left to cancel them — up to `test`'s 80-minute limit, the
#     longest of the three (lint's is 30 and typecheck's is 10). So the act trades up to 80
#     minutes of this run's runner time for the 180 a group with no ci-ok holds the queue. This
#     is the one place that trade is written down; ci.yaml and the action point here.
#   * `success` — the job's work passed and its report step alone failed. Nothing is cancelled
#     at all: this group is no failed group, the groups built on it are testing trees that
#     still land, and a later job of this run finds that `success` among the reports and posts
#     a passing ci-ok.
#   * `unknown` — the caller could not read the reports, so nothing here shows this group
#     failed. Nothing is cancelled, as a head whose ci-ok the started act cannot read dooms
#     nothing: an act proves what it cancels.
#
# A direct child's first step up is through HEAD_SHA itself, which the walk resolves from
# HEAD_SHA and HEAD_BRANCH, so a RUNS that does not hold the asking run yet costs this act no
# descendant. A run that has completed is left alone, since it holds no runner.
#
# ACT=started — the asking run has just started, and it answers two questions. It cancels every run
# of RUNS of the same pull request that has not completed and was created before this run
# (createdAt strictly earlier, the asking run excluded): the queue builds a newer group for a pull
# request when it rebuilds after a change below it, and the older group then tests a tree that will
# not land. The order is createdAt's, never the order the runs started in — a run created earlier
# can start later, and the newer group is still the one that will land. It cancels nothing at all
# when RUNS does not hold the asking run: GitHub's run list lags seconds behind a run that has
# just started, and this act runs in that run's first job, so the list it was given can hold a
# group of the same pull request created after this one while this one is still missing. Without
# this run's own createdAt no listed group can be shown to be the older one, and dating this run
# `now` would make a newer group — the group that will land — look older and cancel it. The cost
# is that one lagging start reclaims no runner, and only a later group of that pull request
# reclaims those older groups: its own start cancels every group of it created before itself.
# When this group is the pull request's last there is no later one — the group merges, or the
# pull request leaves the queue, where dequeued.yaml cancels the removed group's own head and
# never a stale sibling — and the older group then runs to the end.
# Then it prints the asking run's own id as the last `cancel` line when this run's chain of
# parents reaches a group whose head carries a ci-ok of `failure` in CI_OK: that group cannot
# merge, and this group is built on it.
# That half is answered whether or not the list holds the asking run, since the walk reads
# HEAD_SHA and HEAD_BRANCH and no run's createdAt. The commit the walk stops at dooms nothing even
# when its ci-ok failed, because it is a commit of main, and a red run on main is not a failed
# group.
#
# It prints one line per run to force-cancel on stdout, in the order the caller must act on
# them: `cancel <run id>`, the descendants before anything else, and the asking run's own id
# last when the act must end it. The reasoning —
# which run, and why — goes to stderr, so the job log holds the decision beside the act. It exits 0
# whenever it could decide, and non-zero only on a bad input, which it names.
#
# Run from anywhere:
#   ACT=... RUN_ID=... HEAD_SHA=... HEAD_BRANCH=... RUNS=... CI_OK=... REPORT=... \
#     .github/scripts/merge-group-cancels.sh
# Its tests are in merge-group-cancels.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

fail() {
  echo "::error::$1" >&2
  exit 1
}

for input in ACT RUN_ID HEAD_SHA HEAD_BRANCH RUNS; do
  if [ -z "${!input:-}" ]; then
    fail "merge-group-cancels.sh needs $input in its environment"
  fi
done
case $ACT in
  failed | started) ;;
  *) fail "ACT is the act to decide, failed or started, not '$ACT'" ;;
esac
[[ $RUN_ID =~ ^[0-9]+$ ]] || fail "RUN_ID is not a run id: $RUN_ID"
[[ $HEAD_SHA =~ ^[0-9a-f]{7,40}$ ]] || fail "HEAD_SHA is not a commit sha: $HEAD_SHA"
queue_branch='^gh-readonly-queue/.+/pr-[0-9]+-[0-9a-f]{7,40}$'
[[ $HEAD_BRANCH =~ $queue_branch ]] ||
  fail "HEAD_BRANCH is not a gh-readonly-queue/<base>/pr-<n>-<parent sha> branch: $HEAD_BRANCH"

shape='type == "array" and all(.[];
  type == "object"
  and (.databaseId | type) == "number"
  and (.headSha | type) == "string"
  and (.headBranch | type) == "string"
  and (.status | type) == "string"
  and (.createdAt | type) == "string"
  and ((.conclusion | type) == "string" or .conclusion == null))'
jq -e "$shape" <<<"$RUNS" >/dev/null 2>&1 ||
  fail "RUNS is not a JSON array of runs with databaseId, headSha, headBranch, status, conclusion and createdAt: $RUNS"
rfc3339='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?Z$'
bad_times=$(jq -r --arg re "$rfc3339" \
  '[.[] | select((.createdAt | test($re)) | not) | "\(.databaseId) \(.createdAt)"] | join(", ")' <<<"$RUNS")
[ -z "$bad_times" ] || fail "RUNS holds a createdAt that is no RFC3339 UTC time: $bad_times"

ci_ok=${CI_OK:-}
if [ "$ACT" = started ]; then
  [ -n "$ci_ok" ] || fail "the started act reads each ancestor group's ci-ok, so it needs CI_OK in its environment"
fi
[ -n "$ci_ok" ] || ci_ok='{}'
jq -e 'type == "object" and all(.[]; type == "string")' <<<"$ci_ok" >/dev/null 2>&1 ||
  fail "CI_OK is not a JSON object mapping a commit sha to its ci-ok conclusion: $ci_ok"

# The failed act's decision, which the caller reads from the run's artifacts: an unreadable list
# is `unknown` and an unreported job `absent`, both of which the caller says in that word rather
# than by leaving REPORT out, so a caller that forgot it is a failure here and never a silent
# cancel of a healthy group.
report=${REPORT:-}
if [ "$ACT" = failed ]; then
  [ -n "$report" ] ||
    fail "the failed act reads the reporting job's own report, so it needs REPORT in its environment"
  case $report in
    success | failure | cancelled | absent | unknown) ;;
    *) fail "REPORT is the reporting job's own report, one of success, failure, cancelled, absent and unknown, not '$report'" ;;
  esac
fi

# One row per line, tab-separated: `out` and a token for stdout, or `log` and a line of the
# reasoning for stderr. Both are emitted in the order the caller must act on them.
read -r -d '' program <<'JQ' || true
def parsed:
  [((. // "") | capture("^gh-readonly-queue/(?<base>.+)/pr-(?<pr>[0-9]+)-(?<parent>[0-9a-f]+)$"))][0];
def ts: sub("[.][0-9]+Z$"; "Z") | fromdateiso8601;

. as $runs
# Every run a chain step can reach, by its head: the runs of RUNS, and the asking run, which
# HEAD_SHA and HEAD_BRANCH name on their own. `gh run list` lags seconds behind a run that has
# just started, so RUNS can be missing the asking run's own entry, and without this the first
# step of every walk would stop at HEAD_SHA as if it were a commit of main — which hides the
# asking run's direct children from the failed act. A real entry wins, since from_entries takes
# the last of a key. This resolves heads only: which group is older than the asking run is a
# question of createdAt that RUNS alone answers, and the started act reads it there.
| ([{key: $head_sha,
     value: {databaseId: $run_id, headSha: $head_sha, headBranch: $head_branch}}]
   + ($runs | map({key: .headSha, value: .}))
   | from_entries) as $by_sha
# The chain of parents above one run: the runs it is built on, nearest first, and where it stopped.
| def walk($sha; $branch):
    {branch: $branch, seen: [$sha], steps: [], stop: null}
    | until(.stop != null;
        (.branch | parsed) as $parent
        | if $parent == null then
            .stop = {sha: null, why: "branch"}
          elif (.seen | index($parent.parent)) != null then
            .stop = {sha: $parent.parent, why: "cycle"}
          elif ($by_sha | has($parent.parent) | not) then
            .stop = {sha: $parent.parent, why: "main"}
          else
            .steps += [$by_sha[$parent.parent]]
            | .seen += [$parent.parent]
            | .branch = $by_sha[$parent.parent].headBranch
          end);

  if $act == "failed" then
    if $report == "success" then
      ["log", "the job's own report for this run is success: its work passed and its report step alone failed, so this group is no failed group and nothing is cancelled"]
    elif $report == "unknown" then
      ["log", "the job's own report could not be read, so nothing here shows that this group failed and nothing is cancelled"]
    else
      [$runs[]
       | select(.databaseId != $run_id)
       | . as $run
       | (walk(.headSha; .headBranch).steps | map(.headSha) | index($head_sha)) as $step
       | select($step != null)
       | {run: $run, depth: ($step + 1)}]
      | sort_by(.depth, .run.createdAt, .run.databaseId) as $descendants
      | (select($descendants | length == 0)
         | ["log", "no other merge-group run has \($head_sha) in its chain of parents"]),
        ($descendants[]
         | if .run.status == "completed" then
             ["log", "run \(.run.databaseId) (\(.run.headBranch)) is built on \($head_sha), \(.depth) step(s) up, but it has completed (\(.run.conclusion // "no conclusion")): left alone"]
           else
             ["log", "run \(.run.databaseId) (\(.run.headBranch)) is \(.run.status) and its chain of parents reaches \($head_sha) after \(.depth) step(s), so its group can no longer merge: cancel it"],
             ["out", "cancel \(.run.databaseId)"]
           end),
        (if $report == "absent" then
           ["log", "the job uploaded no report, so this group's failing ci-ok is still unposted: run \($run_id) is left running, after the descendants it had to cancel first, so that a job of it can post that ci-ok"]
         else
           ["log", "run \($run_id)'s own remaining jobs are force-cancelled last, after the descendants it had to cancel first"],
           ["out", "cancel \($run_id)"]
         end)
    end
  else
    ([$runs[] | select(.databaseId == $run_id)] | first) as $self
    | ($head_branch | parsed) as $me
    # Which groups of this pull request are older than this run is a question only this run's own
    # createdAt answers, so a list that does not hold the asking run yet cancels nothing: the same
    # lag can leave a group created after this run in the list, and standing this run at the time
    # of the read would make that newer group — the one that will land — look older.
    | (if $self == null then []
       else [$runs[]
             | select(.databaseId != $run_id and .status != "completed")
             | select((.headBranch | parsed) as $parsed | $parsed != null and $parsed.pr == $me.pr)
             | select((.createdAt | ts) < ($self.createdAt | ts))]
       end)
      | sort_by(.createdAt, .databaseId) as $older
    | walk($head_sha; $head_branch) as $chain
    | [$chain.steps[] | select($ci_ok[.headSha] == "failure")] as $doomed
    | (if $self == null then
         ["log", "the run list does not hold the asking run \($run_id) yet, so no group of pull request \($me.pr) can be shown to be older than this one and none is cancelled: the next merge-group run of this pull request cancels them when it starts"]
       elif ($older | length) == 0 then
         ["log", "no group of pull request \($me.pr) created before this run (\($self.createdAt)) is still going"]
       else empty end),
      ($older[]
       | ["log", "run \(.databaseId) (\(.headBranch)) is a group of pull request \($me.pr) created at \(.createdAt), before this run's \($self.createdAt), and is \(.status), so it tests a tree that will not land: cancel it"],
         ["out", "cancel \(.databaseId)"]),
      ($chain.steps[]
       | ["log", "this run is built on \(.headSha), the head of run \(.databaseId) (\(.headBranch)), whose ci-ok is \($ci_ok[.headSha] // "not posted")"]),
      (if $chain.stop.why == "cycle" then
         ["log", "the walk stopped at \($chain.stop.sha): it was visited already, so these branch names form a cycle"]
       elif $chain.stop.why == "main" then
         ["log", "the walk stopped at \($chain.stop.sha): the head of no merge-group run, so it is a commit of main, and its ci-ok (\($ci_ok[$chain.stop.sha] // "not posted")) dooms nothing"]
       else
         ["log", "the walk stopped: \($chain.branch) is no gh-readonly-queue branch"]
       end),
      (if ($doomed | length) > 0 then
         ($doomed[0]
          | ["log", "this run is doomed: its chain of parents reaches \(.headSha), the head of run \(.databaseId), whose ci-ok is failure, so that group cannot merge and this group is built on it"]),
         ["out", "cancel \($run_id)"]
       else
         ["log", "no group in this run's chain of parents carries a failing ci-ok (\($chain.steps | length) group(s) walked): this run is not doomed"]
       end)
  end
| @tsv
JQ
rows=$(jq -r --arg act "$ACT" --argjson run_id "$RUN_ID" --arg head_sha "$HEAD_SHA" \
  --arg head_branch "$HEAD_BRANCH" --argjson ci_ok "$ci_ok" --arg report "$report" \
  "$program" <<<"$RUNS")

while IFS=$'\t' read -r kind text; do
  case $kind in
    out) echo "$text" ;;
    log) echo "$text" >&2 ;;
  esac
done <<<"$rows"
