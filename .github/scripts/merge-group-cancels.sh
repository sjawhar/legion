#!/usr/bin/env bash
# merge-group-cancels is the decision half of the merge queue's cancel act, and performs it not
# at all: given one ci.yaml merge-group run and the repository's ci.yaml merge-group runs, it says
# which runs must be cancelled. The workflow step that cancels acts on its lines; it makes no
# GitHub write, and reads every fact from its environment, so a test drives it from fixtures with
# no network.
#
# What there is to cancel: the queue builds a newer group for a pull request when it rebuilds
# after a change below it, and the older group then tests a tree that will not land while its
# ci.yaml merge-group run, which nothing cancels, runs to the end. That runner time is what the
# act takes back. A group that can no longer merge because the group below it failed is the
# queue's own to rebuild, and ci.yaml's `cancel-started` says so.
#
# Its inputs, from the step's env:
#   RUN_ID       the asking run's databaseId.
#   HEAD_BRANCH  the asking run's head branch, a gh-readonly-queue name:
#                `gh-readonly-queue/<base>/pr-<pull request number>-<parent sha>`, which is where
#                the pull request this group tests is read from.
#   RUNS         a JSON array of this repository's ci.yaml merge-group runs, each
#                {"databaseId": <n>, "headBranch": "…", "status":
#                "queued|in_progress|completed", "createdAt": "<RFC3339>"}, in any order. Only
#                `completed` counts as completed: every other status GitHub gives a run (queued,
#                in_progress, waiting, requested, pending) is a run still holding or waiting for
#                runners. From:
#                  gh run list --repo "$GITHUB_REPOSITORY" --workflow ci.yaml --event merge_group \
#                    --json databaseId,headBranch,status,createdAt
#
# The act: it cancels every run of RUNS of the same pull request that has not completed and was
# created before this run (createdAt strictly earlier, the asking run excluded). The order is
# createdAt's, never the order the runs started in — a run created earlier can start later, and
# the newer group is still the one that will land. It cancels nothing at all when RUNS does not
# hold the asking run: GitHub's run list lags seconds behind a run that has just started, and this
# act runs in that run's first job, so the list it was given can hold a group of the same pull
# request created after this one while this one is still missing. Without this run's own createdAt
# no listed group can be shown to be the older one, and dating this run `now` would make a newer
# group — the group that will land — look older and cancel it. The cost is that one lagging start
# reclaims no runner, and only a later group of that pull request reclaims those older groups: its
# own start cancels every group of it created before itself. When this group is the pull request's
# last there is no later one — the group merges, or the pull request leaves the queue, where
# dequeued.yaml cancels the removed group's own head and never a stale sibling — and the older
# group then runs to the end.
#
# It prints one line per run to force-cancel on stdout, in the order the caller must act on
# them: `cancel <run id>`. The reasoning — which run, and why — goes to stderr, so the job log
# holds the decision beside the act. It exits 0 whenever it could decide, and non-zero only on a
# bad input, which it names.
#
# Run from anywhere:
#   RUN_ID=... HEAD_BRANCH=... RUNS=... .github/scripts/merge-group-cancels.sh
# Its tests are in merge-group-cancels.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

fail() {
  echo "::error::$1" >&2
  exit 1
}

for input in RUN_ID HEAD_BRANCH RUNS; do
  if [ -z "${!input:-}" ]; then
    fail "merge-group-cancels.sh needs $input in its environment"
  fi
done
[[ $RUN_ID =~ ^[0-9]+$ ]] || fail "RUN_ID is not a run id: $RUN_ID"
queue_branch='^gh-readonly-queue/.+/pr-[0-9]+-[0-9a-f]{7,40}$'
[[ $HEAD_BRANCH =~ $queue_branch ]] ||
  fail "HEAD_BRANCH is not a gh-readonly-queue/<base>/pr-<n>-<parent sha> branch: $HEAD_BRANCH"

shape='type == "array" and all(.[];
  type == "object"
  and (.databaseId | type) == "number"
  and (.headBranch | type) == "string"
  and (.status | type) == "string"
  and (.createdAt | type) == "string")'
jq -e "$shape" <<<"$RUNS" >/dev/null 2>&1 ||
  fail "RUNS is not a JSON array of runs with databaseId, headBranch, status and createdAt: $RUNS"
rfc3339='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?Z$'
bad_times=$(jq -r --arg re "$rfc3339" \
  '[.[] | select((.createdAt | test($re)) | not) | "\(.databaseId) \(.createdAt)"] | join(", ")' <<<"$RUNS")
[ -z "$bad_times" ] || fail "RUNS holds a createdAt that is no RFC3339 UTC time: $bad_times"

# One row per line, tab-separated: `out` and a token for stdout, or `log` and a line of the
# reasoning for stderr. Both are emitted in the order the caller must act on them.
read -r -d '' program <<'JQ' || true
def parsed:
  [((. // "") | capture("^gh-readonly-queue/(?<base>.+)/pr-(?<pr>[0-9]+)-(?<parent>[0-9a-f]+)$"))][0];
def ts: sub("[.][0-9]+Z$"; "Z") | fromdateiso8601;

. as $runs
| ([$runs[] | select(.databaseId == $run_id)] | first) as $self
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
| (if $self == null then
     ["log", "the run list does not hold the asking run \($run_id) yet, so no group of pull request \($me.pr) can be shown to be older than this one and none is cancelled: the next merge-group run of this pull request cancels them when it starts"]
   elif ($older | length) == 0 then
     ["log", "no group of pull request \($me.pr) created before this run (\($self.createdAt)) is still going"]
   else empty end),
  ($older[]
   | ["log", "run \(.databaseId) (\(.headBranch)) is a group of pull request \($me.pr) created at \(.createdAt), before this run's \($self.createdAt), and is \(.status), so it tests a tree that will not land: cancel it"],
     ["out", "cancel \(.databaseId)"])
| @tsv
JQ
rows=$(jq -r --argjson run_id "$RUN_ID" --arg head_branch "$HEAD_BRANCH" "$program" <<<"$RUNS")

while IFS=$'\t' read -r kind text; do
  case $kind in
    out) echo "$text" ;;
    log) echo "$text" >&2 ;;
  esac
done <<<"$rows"
