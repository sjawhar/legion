#!/usr/bin/env bash
# Posts the failing ci-ok of a merge-group run from the job whose own result decides the group, in
# that job's last step, the moment it ends. Every other verdict is ci.yaml's `ci-ok` job's: that
# job needs every job of the run, fails when ci-ok.sh's verdict over their results is failure, and
# its own check run is the ci-ok main's ruleset reads.
#
# Why the verdict is a job and this post is not. A check run POSTed with
# POST /repos/.../check-runs joins the Actions App's first check suite at that commit rather than
# the posting run's, which is another workflow's as often as not and a suite the merge queue then
# does not read: post-check-run.sh's header holds the measurement. A job's own check run is in
# its run's own suite by construction, so the verdict is a job. What that shape costs is the
# verdict job's wait for a runner of its own, which it begins only once the last of its needs has
# ended; docs/solutions/github/join-a-job-to-the-ci-router.md holds every reading of that wait.
# The verdict is a job all the same, because one that lands late is right while one filed in
# another workflow's suite blocks the head until a person re-runs it.
#
# What this early post buys is the one thing that job cannot: the ci-ok job waits for every job of
# the run, so on a merge-group run a failure has already lost it would post the failing verdict
# only once every other job had ended. Posted from here, the queue reads the verdict at the first
# failure, takes the pull request out and rebuilds the groups above it without waiting for the
# rest of the run.
#
# The post carries the external_id ci-ok-post:<this run's id>, which is the marker
# ci-ok-reconcile.sh looks for: that step rewrites a ci-ok check run whose external_id carries
# that prefix or is empty, the post of the release before the marker, and leaves alone one that
# carries a UUID, a job's own check run. post-check-run.sh's header says what that field holds in
# each case.
#
# So it posts on a merge_group run and nowhere else, and there only in one of four cases: this
# job's own result is a failure or a cancellation (`cancelled` is what a job its own
# `timeout-minutes` cut off carries, and it decides the group just as a failure does), or
# `changes` failed or was cancelled (its own time limit cuts it off as `cancelled`), which leaves
# the jobs it selects unrun and the group untested. On every other run, and on a merge-group run
# nothing has decided yet, this script prints what it saw and exits 0: the ci-ok job posts that
# run's verdict. A pull request needs no early post, since no queue holds it and its run is
# cancelled by the next push.
#
# A decided run is read once before the post, so the check's summary names every job's result: the
# run's jobs route (the latest attempt's, as GitHub lists them), where a job GitHub shows
# completed stands at its conclusion, a called workflow's jobs (`<job> / <its job>`) together, one
# still queued or running at that status (`queued`, `in_progress`), and one the route does not
# list at all as `unlisted`. The reporting job is the one exception: the route shows it
# `in_progress`, because this step is still running, so its own JOB_STATUS stands for it. Nothing
# but a success is a success there, so ci-ok.sh's verdict is failure, which is right: a selected
# job that did not succeed fails ci-ok, and the group this run tests cannot merge.
#
# A jobs read GitHub refuses three times records every job as `unread`, which is no success
# either, and the verdict goes up all the same: it is failing whatever those jobs go on to do, and
# waiting for a read this job cannot make would leave the group with no ci-ok at all until
# GitHub's 180-minute check timeout took its pull request out of the queue. Every read here, and
# the post, is retried on gh-read.sh's one ladder, whose GH_RETRY_SECONDS is the tests' knob for
# the delay; three refusals of the post fail the step with nothing posted, and the run's other
# jobs run on — the ci-ok job then posts the verdict at the end of the run, unless
# .github/workflows/dequeued.yaml force-cancels the run first, which leaves every `if: always()`
# step skipped.
#
# The verdict is ci-ok.sh's, over the NEEDS this script builds: `changes`' result and outputs as
# `toJSON(needs.changes)` gave them to the reporting job, and every other job's result as above.
#
# Its inputs, from the ci-ok-report action's env:
#   GH_TOKEN           the job's github.token, with `actions: read` (the run's jobs) and
#                      `checks: write` (the post).
#   JOB                the reporting job's id in ci.yaml; a called workflow's job passes the id of
#                      the ci.yaml job that calls it.
#   JOB_STATUS         that job's `job.status`: success, failure or cancelled.
#   CHANGES            `toJSON(needs.changes)` of the reporting job.
#   EVENT              `github.event_name`.
#   HEAD_SHA           the commit ci-ok is for (post-check-run.sh).
#   GITHUB_REPOSITORY, GITHUB_RUN_ID, GITHUB_SERVER_URL, as Actions sets them.
#   CI_OK_WORKFLOW     optional: the router's file, .github/workflows/ci.yaml beside this script
#                      by default (the copy of the commit the run checked out). CI_OK_SELECTED_BY,
#                      CI_OK_PULL_REQUEST_ONLY and CI_OK_ALWAYS_RUN are read from it and handed
#                      to ci-ok.sh, which judges a job by the class its name is in.
#
# Run from anywhere: JOB=... JOB_STATUS=... CHANGES=... EVENT=... HEAD_SHA=... \
#   .github/scripts/ci-ok-report.sh
# Its tests are in ci-ok-report.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
workflow=${CI_OK_WORKFLOW:-$script_dir/../workflows/ci.yaml}
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"
for input in GH_TOKEN JOB JOB_STATUS CHANGES EVENT HEAD_SHA GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_SERVER_URL; do
  if [ -z "${!input:-}" ]; then
    echo "::error::ci-ok-report.sh needs $input in its environment"
    exit 1
  fi
done
if ! jq -e 'type == "object" and (.result | type == "string")' <<<"$CHANGES" >/dev/null 2>&1; then
  echo "::error::CHANGES is not toJSON(needs.changes): $CHANGES"
  exit 1
fi

# Which of the four cases decided this merge-group run, if any: the two lines below name the job
# whose result decided it and which case it was, so the posted title and summary both say what
# decided it. A job's own result is read from JOB_STATUS rather than from the jobs route, which
# shows this job `in_progress` while this step runs.
decided_job=$JOB
decided_by=
if [ "$EVENT" = merge_group ]; then
  case $JOB_STATUS in
    failure) decided_by="whose failure decides this merge-group run, posted from this job rather than from the ci-ok job at the end of the run" ;;
    cancelled) decided_by="whose cancellation decides this merge-group run, posted from this job rather than from the ci-ok job at the end of the run" ;;
  esac
  if [ -z "$decided_by" ]; then
    case $(jq -r .result <<<"$CHANGES") in
      failure)
        decided_job=changes
        decided_by="reporting after changes failed, which decides this merge-group run: the verdict goes up from this job rather than from the ci-ok job at the end of the run"
        ;;
      cancelled)
        decided_job=changes
        decided_by="reporting after changes was cancelled, which decides this merge-group run: the verdict goes up from this job rather than from the ci-ok job at the end of the run"
        ;;
    esac
  fi
fi
if [ -z "$decided_by" ]; then
  echo "$JOB ended $JOB_STATUS on a $EVENT run, which this job decides nothing of; the ci-ok job posts this run's verdict"
  exit 0
fi

selected_by=$(yq -r '.env.CI_OK_SELECTED_BY // ""' "$workflow")
pull_request_only=$(yq -r '.env.CI_OK_PULL_REQUEST_ONLY // ""' "$workflow")
always_run=$(yq -r '.env.CI_OK_ALWAYS_RUN // ""' "$workflow")
if ! jq -e 'type == "object"' <<<"$selected_by" >/dev/null 2>&1 \
  || ! jq -e 'type == "array"' <<<"$pull_request_only" >/dev/null 2>&1 \
  || ! jq -e 'type == "array"' <<<"$always_run" >/dev/null 2>&1; then
  echo "::error::$workflow's env holds no JSON CI_OK_SELECTED_BY object and CI_OK_PULL_REQUEST_ONLY and CI_OK_ALWAYS_RUN arrays"
  exit 1
fi

read -r -d '' program <<'JQ' || true
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
def live($job): [mine($job)[] | select(.status != "completed") | .status] | first // "unlisted";
{changes: $changes}
+ ([$always[] | select(. != "changes") | {key: ., value: {result: (ended(.) // live(.))}}] | from_entries)
+ ([$map | keys_unsorted[] | {key: ., value: {result: (ended(.) // live(.))}}] | from_entries)
JQ
if jobs=$(gh_read gh api --paginate \
  "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/jobs?per_page=100" \
  --jq '.jobs[] | {name, status, conclusion}'); then
  needs=$(jq -n --argjson jobs "$(jq -s . <<<"$jobs")" --argjson changes "$CHANGES" \
    --argjson map "$selected_by" --argjson always "$always_run" "$program")
else
  echo "::warning::GitHub refused this run's job list three times, and this merge-group run is already decided: every other job is read as unread, because this run's verdict is failing whatever those jobs go on to do and a group with no ci-ok at all holds the queue for 180 minutes"
  needs=$(jq -n --argjson changes "$CHANGES" --argjson map "$selected_by" --argjson always "$always_run" \
    '{changes: $changes}
     + ([$always[] | select(. != "changes") | {key: ., value: {result: "unread"}}] | from_entries)
     + ([$map | keys_unsorted[] | {key: ., value: {result: "unread"}}] | from_entries)')
fi
# This job's own result, which the jobs route cannot give while this step is running.
needs=$(jq --arg job "$JOB" --arg status "$JOB_STATUS" '.[$job].result = $status' <<<"$needs")

summary=$(mktemp)
trap 'rm -f "$summary"' EXIT
status=0
verdict=$(NEEDS=$needs EVENT=$EVENT SELECTED_BY=$selected_by ALWAYS_RUN=$always_run \
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
# The title names the job whose result decided the run, which is the one thing a person reading
# the check beside the merge box needs and the summary below says at length: on such a run most
# jobs never ran, so the verdict alone reads as if ci-ok judged a whole run.
title=$(cut -c1-200 <<<"$decided_job decided this merge-group run: $title")
{
  echo "Decided by $JOB, $decided_by ([run]($GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID))."
  echo
  echo '```text'
  echo "$verdict"
  echo '```'
} >"$summary"
CHECK_NAME=ci-ok CONCLUSION=$conclusion TITLE=$title SUMMARY_FILE=$summary \
  DETAILS_URL=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID \
  EXTERNAL_ID=ci-ok-post:$GITHUB_RUN_ID \
  "$script_dir/post-check-run.sh"
