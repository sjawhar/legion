#!/usr/bin/env bash
# Posts ci-ok, the check main's ruleset requires of ci.yaml's router, from the last step of the
# last job of a run to finish. ci.yaml has no ci-ok job: one would wait for a runner of its own
# after every other job ended, in the queue behind every job the account started meanwhile, which
# on 2026-11-02, with the account's runners full all day, took a median 39 minutes a run and up to
# 111.
#
# Every job of ci.yaml but `changes`, and the job of each workflow ci.yaml calls, ends with the
# step .github/actions/ci-ok-report, which first uploads the job's result as an artifact of the
# run named ci-ok-<job>-<run attempt>-<success|failure|cancelled>, then runs this script. The
# script lists the run's artifacts and posts ci-ok only when every job that runs has reported:
# lint, and every job `changes` selected (ci-ok.jq, from CI_OK_SELECTED_BY and
# CI_OK_PULL_REQUEST_ONLY in ci.yaml's env, the map check-ci-filters.sh holds to the jobs' `if:`s).
# Otherwise it says which jobs it waits on and exits 0: each of those reports later, and the last
# of them finds every report.
#
# It does not wait on a merge_group run whose verdict is already decided: one where `changes`
# failed or was cancelled, or where the reporting job's own report (its artifact, uploaded
# before the list is read) is a failure or a cancellation. ci.yaml's merge-group followers fire
# on `failure` or `cancelled` alike — `cancelled` is what a job its own `timeout-minutes` cut
# off reports — and the follower of a job whose own report says so then force-cancels the rest
# of that run, so no later report can post at all: the failing ci-ok must be posted by that job
# itself, before the cancel, or the group gets no ci-ok at all and sits in the queue until
# GitHub's 180-minute check timeout takes it out. The artifact this step uploads is the very
# signal the follower reads (.github/actions/merge-group-cancel), so the two never disagree
# about which job's work failed. `changes` is the one decided case with no force-cancel behind
# it: it uploads no report, so its follower cancels the groups built on this one and leaves
# this run alive, and the first of lint, typecheck and test to report posts the verdict below.
# The `cancelled` half also admits a merge_group run a person or another workflow cancelled
# gracefully, where this job's own `job.status` is `cancelled` and its `always()` steps still
# run: that run is over and will post no ci-ok later either, so the failing one posted now takes
# the pull request out of the queue at once rather than at that same timeout. A force-cancelled
# run reaches none of this: its steps are left skipped, so this script does not run there at
# all. A decided run reads the jobs route (below) and records every still
# unreported job at its live status: a job GitHub shows completed at its conclusion, one still
# queued or running at that status (`queued`, `in_progress`), one the route does not list at all
# as `unlisted`. None of those is `success`, so ci-ok.sh's verdict is failure, which is right: a
# selected job that did not succeed fails ci-ok, and the run is about to be thrown away. A
# refused read leaves those jobs unreported and the script waits as usual.
#
# The report is uploaded before the list is read, and GitHub serves an artifact once its upload
# finishes, so of two jobs that finish together the second to read the list finds both reports
# and posts; both may post, and the later check run is the one GitHub and the Legion daemon read.
# A job reports its own `job.status` (`always()`), so a failed, timed-out or cancelled job
# reports too. On a pull-request or dispatched run, a re-run attempt's report replaces that job's
# earlier one, and a job a failed-only re-run did not run keeps its report from the attempt that
# ran it; during a whole re-run, a job not yet re-run keeps its earlier attempt's report at this
# same head. A merge-group run is never re-run: every job of such a re-run attempt skips, by the
# term every job of ci.yaml carries (the comment above its `changes` job says which), so the
# attempt runs this script nowhere,
# posts nothing, and the first attempt's ci-ok stands. A job `changes` did not
# select and that never reported was skipped. A selected job that never reports (one a
# failed-only re-run left skipped, one cancelled before it started, one that ended before its
# first step) stands at the conclusion GitHub lists for it once it is one of the last two (below),
# and otherwise leaves ci-ok unposted by this run, which GitHub's merge box shows as expected and
# the daemon as missing, never as passed.
#
# GH_RETRY_SECONDS is the tests' knob for the retry delay (below).
#
# The verdict is ci-ok.sh's, over the NEEDS this script builds: `changes`' result and outputs as
# `toJSON(needs.changes)` gave them to the reporting job, and every job's reported result.
#
# Its inputs, from the ci-ok-report action's env:
#   GH_TOKEN           the job's github.token, with `actions: read` (the artifact list) and
#                      `checks: write` (the post).
#   JOB                the reporting job's id in ci.yaml, for the log.
#   CHANGES            `toJSON(needs.changes)` of the reporting job.
#   EVENT              `github.event_name`.
#   HEAD_SHA           the commit ci-ok is for (post-check-run.sh).
#   GITHUB_REPOSITORY, GITHUB_RUN_ID, GITHUB_SERVER_URL, as Actions sets them.
#   CI_OK_WORKFLOW     optional: the router's file, .github/workflows/ci.yaml beside this script
#                      by default (the copy of the commit the run checked out).
#
# Run from anywhere: JOB=... CHANGES=... EVENT=... HEAD_SHA=... .github/scripts/ci-ok-report.sh
# Its tests are in ci-ok-report.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
workflow=${CI_OK_WORKFLOW:-$script_dir/../workflows/ci.yaml}
for input in GH_TOKEN JOB CHANGES EVENT HEAD_SHA GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_SERVER_URL; do
  if [ -z "${!input:-}" ]; then
    echo "::error::ci-ok-report.sh needs $input in its environment"
    exit 1
  fi
done
if ! jq -e 'type == "object" and (.result | type == "string")' <<<"$CHANGES" >/dev/null 2>&1; then
  echo "::error::CHANGES is not toJSON(needs.changes): $CHANGES"
  exit 1
fi
selected_by=$(yq -r '.env.CI_OK_SELECTED_BY // ""' "$workflow")
pull_request_only=$(yq -r '.env.CI_OK_PULL_REQUEST_ONLY // ""' "$workflow")
if ! jq -e 'type == "object"' <<<"$selected_by" >/dev/null 2>&1 \
  || ! jq -e 'type == "array"' <<<"$pull_request_only" >/dev/null 2>&1; then
  echo "::error::$workflow's env holds no JSON CI_OK_SELECTED_BY object and CI_OK_PULL_REQUEST_ONLY array"
  exit 1
fi

# Every artifact name of the run, one per line; a failed read is retried twice, GH_RETRY_SECONDS
# (10 by default, as for post-check-run.sh) and then twice as many apart, then fails the step with
# nothing posted.
names=
for attempt in 1 2 3; do
  if names=$(gh api --paginate "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100" \
    --jq '.artifacts[].name'); then
    break
  fi
  if [ "$attempt" -eq 3 ]; then
    echo "::error::GitHub refused the run's artifact list three times, so this job cannot tell whether it is the last"
    exit 1
  fi
  sleep $((attempt * ${GH_RETRY_SECONDS:-10}))
done

read -r -d '' program <<'JQ' || true
include "ci-ok";
($changes.outputs // {}) as $out
| ($names
   | map(capture("^ci-ok-(?<job>.+)-(?<attempt>[0-9]+)-(?<status>success|failure|cancelled)$") // empty)
   | group_by(.job)
   | map(max_by(.attempt | tonumber) | {key: .job, value: .status})
   | from_entries) as $reports
| (["lint"] + [$map | keys_unsorted[] | select(selection(.; $map; $out; $pr; $event).selected)]) as $runs
| {pending: [$runs[] | select($reports[.] == null)],
   needs: ({changes: $changes, lint: {result: ($reports.lint // "absent")}}
           + ([$map | keys_unsorted[] | {key: ., value: {result: ($reports[.] // "skipped")}}] | from_entries)
           + ([$reports | to_entries[] | select(.key as $job | $job != "lint" and $job != "changes" and ($map | has($job) | not))
               | {key, value: {result: .value}}] | from_entries))}
JQ
decision=$(jq -n -L "$script_dir" --argjson changes "$CHANGES" --arg event "$EVENT" \
  --argjson map "$selected_by" --argjson pr "$pull_request_only" \
  --argjson names "$(jq -R . <<<"$names" | jq -s 'map(select(. != ""))')" "$program")

# A merge_group run whose verdict is already decided: `changes` failed or was cancelled (its
# own time limit cuts it off as `cancelled`, and `cancel-changes` fires on that result
# just as it does on a failure), or this job's own report (the artifact it uploaded before the
# list was read) is a failure or a cancellation — the result a job its `timeout-minutes` cut
# off carries, and one ci.yaml's follower fires on just as it does on a failure. In the two
# cases where this job's own report decided it, that follower force-cancels the rest of the
# run, so a job that has not reported never will, and this job posts the verdict now rather
# than waiting for a report the cancel prevents. In the two `changes` cases it does not:
# `changes` uploads no report, so its follower cancels the groups built on this one and leaves
# this run alive — and the verdict is posted now all the same, by the first of lint, typecheck
# and test to report, rather than by the last of them (merge-group-cancels.sh's header says
# what that trades). The line below
# names which of the four it was, so the posted summary says what decided the run.
decided=false
decided_by="the last job of the run to report"
if [ "$EVENT" = merge_group ]; then
  reason=$(jq -r --arg job "$JOB" '
    if .needs[$job].result == "failure" then
      "whose failure decides this merge-group run before its follower force-cancels the rest"
    elif .needs[$job].result == "cancelled" then
      "whose cancellation decides this merge-group run before its follower force-cancels the rest"
    elif .needs.changes.result == "failure" then
      "reporting after changes failed, which decides this merge-group run: the changes follower cancels the groups built on it and leaves this run to post this verdict"
    elif .needs.changes.result == "cancelled" then
      "reporting after changes was cancelled, which decides this merge-group run: the changes follower cancels the groups built on it and leaves this run to post this verdict"
    else "" end' <<<"$decision")
  if [ -n "$reason" ]; then
    decided=true
    decided_by="$reason"
  fi
fi

# A job that ended before its first step ran (a service container that never started, a runner
# lost) never reports, so a run whose other jobs all reported would never get ci-ok. When one or
# two jobs are left unreported, or the verdict is decided and this run will decide nothing more,
# read the run's jobs once (the latest attempt's, as GitHub lists them): each such job GitHub
# shows completed stands at its conclusion, a called workflow's jobs (`<job> / <its job>`)
# together. Fewer reads than every job reading the list, which the token's hourly limit could not
# carry; a job that ends unreported as the run's very last leaves ci-ok unposted, and re-running
# it posts ci-ok. A failed read leaves the jobs unreported. On a decided run a job that is not
# completed stands at the status GitHub shows it at (`queued`, `in_progress`), and one the route
# does not list at all at `unlisted`: neither is `success`, so ci-ok.sh fails it.
unreported=$(jq '.pending | length' <<<"$decision")
if [ "$unreported" -ge 1 ] && { [ "$unreported" -le 2 ] || [ "$decided" = true ]; } \
  && jobs=$(gh api --paginate "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/jobs?per_page=100" \
    --jq '.jobs[] | {name, status, conclusion}'); then
  decision=$(jq --argjson jobs "$(jq -s . <<<"$jobs")" --argjson decided "$decided" '
    def mine($job): [$jobs[] | select(.name == $job or (.name | startswith($job + " / ")))];
    def ended($job):
      mine($job) as $mine
      | if ($mine | length) > 0 and all($mine[]; .status == "completed") then
          [$mine[].conclusion]
          | if any(. == "success" or . == "skipped" or . == "cancelled" | not) then "failure"
            elif any(. == "cancelled") then "cancelled"
            elif any(. == "success") then "success"
            else "skipped" end
        else null end;
    def live($job):
      [mine($job)[] | select(.status != "completed") | .status] | first // "unlisted";
    reduce .pending[] as $job (.;
      (ended($job) // (if $decided then live($job) else null end)) as $result
      | if $result == null then . else (.pending -= [$job]) | (.needs[$job].result = $result) end)' \
    <<<"$decision")
fi

pending=$(jq -r '.pending | join(", ")' <<<"$decision")
if [ -n "$pending" ]; then
  echo "$JOB reported; ci-ok waits on $pending, and the last of them to finish posts it"
  exit 0
fi

summary=$(mktemp)
trap 'rm -f "$summary"' EXIT
status=0
verdict=$(NEEDS=$(jq -c .needs <<<"$decision") EVENT=$EVENT SELECTED_BY=$selected_by \
  PULL_REQUEST_ONLY=$pull_request_only "$script_dir/ci-ok.sh" 2>&1) || status=$?
echo "$verdict"
if [ "$status" -eq 0 ]; then
  conclusion=success
  title="Every job changes selected succeeded"
else
  conclusion=failure
  title=$(grep -m1 '^::error::' <<<"$verdict" | sed 's/^::error:://' | cut -c1-200 || true)
  title=${title:-ci-ok.sh failed}
fi
{
  echo "Decided by $JOB, $decided_by ([run]($GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID))."
  echo
  echo '```text'
  echo "$verdict"
  echo '```'
} >"$summary"
CHECK_NAME=ci-ok CONCLUSION=$conclusion TITLE=$title SUMMARY_FILE=$summary \
  DETAILS_URL=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID \
  "$script_dir/post-check-run.sh"
