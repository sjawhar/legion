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
# The report is uploaded before the list is read, and GitHub serves an artifact once its upload
# finishes, so of two jobs that finish together the second to read the list finds both reports
# and posts; both may post, and the later check run is the one GitHub and the Legion daemon read.
# A job reports its own `job.status` (`always()`), so a failed, timed-out or cancelled job
# reports too. A re-run attempt's report replaces that job's earlier one, and a job a failed-only
# re-run did not run keeps its report from the attempt that ran it. A job `changes` did not
# select and that never reported was skipped; a selected job that never reports (one a
# failed-only re-run left skipped, or one cancelled before it started) leaves ci-ok unposted by
# this run, which GitHub's merge box shows as expected and the daemon as missing, never as passed.
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
  title=$(grep -m1 '^::error::' <<<"$verdict" | sed 's/^::error:://' | cut -c1-200)
  title=${title:-ci-ok.sh failed}
fi
{
  echo "Decided by $JOB, the last job of the run to report ([run]($GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID))."
  echo
  echo '```text'
  echo "$verdict"
  echo '```'
} >"$summary"
CHECK_NAME=ci-ok CONCLUSION=$conclusion TITLE=$title SUMMARY_FILE=$summary \
  DETAILS_URL=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID \
  "$script_dir/post-check-run.sh"
