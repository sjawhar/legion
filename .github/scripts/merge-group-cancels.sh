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
# parent sha that is the head of a run in RUNS, and the first parent that is not one ends the walk,
# because that commit is main's. A sha is visited once, so branch names that loop stop the walk
# instead of hanging it.
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
#   CI_OK        a JSON object mapping a commit sha to that commit's ci-ok check-run conclusion
#                (success, failure, …; a sha with no ci-ok is absent). The started act needs it;
#                the failed act does not read it and defaults it to {}. From, over the head shas
#                of the runs of RUNS that did not complete `success` — a run that completed
#                `success` is no failed group, and a head left out carries no ci-ok here, which
#                dooms nothing:
#                  jq -r '[.[] | select(.conclusion != "success") | .headSha] | unique[]' <<<"$RUNS" |
#                    while read -r sha; do
#                      gh api "repos/$GITHUB_REPOSITORY/commits/$sha/check-runs?check_name=ci-ok" \
#                        --jq '.check_runs[0].conclusion // empty' |
#                        jq -R --arg sha "$sha" 'select(. != "") | {($sha): .}'
#                    done | jq -s 'add // {}'
#
# ACT=failed — a selected job of the asking run has failed, so its group cannot merge and neither
# can any group built on it. It cancels every run of RUNS that has not completed and whose chain of
# parents reaches HEAD_SHA, the nearest descendant first, and then prints `self`. A run that has
# completed is left alone, since it holds no runner. The asking run is never in the cancel list: it
# is the `self` line, and that line is last because cancelling this run first would kill the job
# that has the descendants to cancel.
#
# ACT=started — the asking run has just started, and it answers two questions. It cancels every run
# of RUNS of the same pull request that has not completed and was created before this run
# (createdAt strictly earlier, the asking run excluded): the queue builds a newer group for a pull
# request when it rebuilds after a change below it, and the older group then tests a tree that will
# not land. The order is createdAt's, never the order the runs started in — a run created earlier
# can start later, and the newer group is still the one that will land. Then it prints
# `self-doomed` when this run's chain of parents reaches a group whose head carries a ci-ok of
# `failure` in CI_OK: that group cannot merge, and this group is built on it. The commit the walk
# stops at dooms nothing even when its ci-ok failed, because it is a commit of main, and a red run
# on main is not a failed group.
#
# It prints one token per line on stdout, in the order the caller must act on them: `cancel <run
# id>` for each other run, descendants before anything else, then `self` as the last line of the
# failed act, or `self-doomed` as the last line of the started act when it applies. The reasoning —
# which run, and why — goes to stderr, so the job log holds the decision beside the act. It exits 0
# whenever it could decide, and non-zero only on a bad input, which it names.
#
# Run from anywhere:
#   ACT=... RUN_ID=... HEAD_SHA=... HEAD_BRANCH=... RUNS=... CI_OK=... \
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
  # The act orders by the asking run's createdAt, so that run must be in the list. GitHub's run
  # list can lag seconds behind a run that has just started, and this act runs in that run's first
  # job, so the list it read may not hold it yet. A run that has just started is the newest
  # merge-group run there is, so standing it at the time of this read orders the rest rightly:
  # every group of its pull request the list holds was created before it.
  if ! jq -e --argjson id "$RUN_ID" 'any(.[]; .databaseId == $id)' <<<"$RUNS" >/dev/null 2>&1; then
    echo "the run list does not hold the asking run $RUN_ID yet, so it stands as the newest merge-group run, created now" >&2
    RUNS=$(jq -c --argjson id "$RUN_ID" --arg sha "$HEAD_SHA" --arg branch "$HEAD_BRANCH" \
      --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      '. + [{databaseId: $id, headSha: $sha, headBranch: $branch, status: "in_progress",
             conclusion: null, createdAt: $now}]' <<<"$RUNS")
  fi
fi
[ -n "$ci_ok" ] || ci_ok='{}'
jq -e 'type == "object" and all(.[]; type == "string")' <<<"$ci_ok" >/dev/null 2>&1 ||
  fail "CI_OK is not a JSON object mapping a commit sha to its ci-ok conclusion: $ci_ok"

# One row per line, tab-separated: `out` and a token for stdout, or `log` and a line of the
# reasoning for stderr. Both are emitted in the order the caller must act on them.
read -r -d '' program <<'JQ' || true
def parsed:
  [((. // "") | capture("^gh-readonly-queue/(?<base>.+)/pr-(?<pr>[0-9]+)-(?<parent>[0-9a-f]+)$"))][0];
def ts: sub("[.][0-9]+Z$"; "Z") | fromdateiso8601;

. as $runs
| ($runs | map({key: .headSha, value: .}) | from_entries) as $by_sha
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
      ["log", "run \($run_id)'s own remaining jobs are force-cancelled last, after the descendants it had to cancel first"],
      ["out", "self"]
  else
    ($runs[] | select(.databaseId == $run_id)) as $self
    | ($head_branch | parsed) as $me
    | [$runs[]
       | select(.databaseId != $run_id and .status != "completed")
       | select((.headBranch | parsed) as $parsed | $parsed != null and $parsed.pr == $me.pr)
       | select((.createdAt | ts) < ($self.createdAt | ts))]
      | sort_by(.createdAt, .databaseId) as $older
    | walk($head_sha; $head_branch) as $chain
    | [$chain.steps[] | select($ci_ok[.headSha] == "failure")] as $doomed
    | (select($older | length == 0)
       | ["log", "no group of pull request \($me.pr) created before this run (\($self.createdAt)) is still going"]),
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
         ["out", "self-doomed"]
       else
         ["log", "no group in this run's chain of parents carries a failing ci-ok (\($chain.steps | length) group(s) walked): this run is not doomed"]
       end)
  end
| @tsv
JQ
rows=$(jq -r --arg act "$ACT" --argjson run_id "$RUN_ID" --arg head_sha "$HEAD_SHA" \
  --arg head_branch "$HEAD_BRANCH" --argjson ci_ok "$ci_ok" "$program" <<<"$RUNS")

while IFS=$'\t' read -r kind text; do
  case $kind in
    out) echo "$text" ;;
    log) echo "$text" >&2 ;;
  esac
done <<<"$rows"
