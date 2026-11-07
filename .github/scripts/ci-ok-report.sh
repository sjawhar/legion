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
# re-run did not run keeps its report from the attempt that ran it; during a whole re-run, a job
# not yet re-run keeps its earlier attempt's report at this same head. A job `changes` did not
# select and that never reported was skipped. A selected job that never reports (one a
# failed-only re-run left skipped, one cancelled before it started, one that ended before its
# first step) stands at the conclusion GitHub lists for it once it is one of the last two (below),
# and otherwise leaves ci-ok unposted by this run, which GitHub's merge box shows as expected and
# the daemon as missing, never as passed.
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

# Every artifact name of the run, one per line. The read retries: .github/scripts/ci-retry.sh
# makes five bounded attempts, the last starting 150 s after the first, and an exhausted read
# fails the step with nothing posted. The loop that stood here was a second copy of that helper,
# which is what dispatch://INFRA-1545's R2 refuses: one file holds the attempts for all of CI.
#
# gh writes into a file rather than into a variable, and the redirection sits inside the command
# the helper runs so that every attempt starts that file over: gh copies an error answer's body
# to standard output as it copies a good one's, and a refused attempt's leftovers ahead of the
# retry's answer would leave the read holding both. `sh -c` given one command execs it, so the
# attempt's bound still reaches gh itself.
#
# 30 s per attempt: this is a JSON read of one REST route, and the whole step that holds both of
# these reads — the artifact write, the upload and the two gh calls — measured at most 9 s over
# 378 observations in the 20 most recent successful ci.yaml push runs on main, with a median of
# 3 s. So one read takes about 3 s, and 30 s is ten times that; the step's two reads are the
# dominant adder in every job's budget, which is why the bound is the call's own size rather
# than a round number above it (dispatch://INFRA-1545).
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# shellcheck source=.github/scripts/ci-retry.sh
. "$script_dir/ci-retry.sh"
# shellcheck disable=SC2016 # $1 to $3 are sh -c's own arguments, expanded by that shell
if ! retry_fetch "the run's artifact list" 30 -- sh -c 'gh api --paginate "$1" --jq "$2" > "$3"' \
  gh-api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100" \
  '.artifacts[].name' "$work/artifacts"; then
  echo "::error::GitHub refused the run's artifact list five times, so this job cannot tell whether it is the last"
  exit 1
fi
names=$(<"$work/artifacts")

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

# A job that ended before its first step ran (a service container that never started, a runner
# lost) never reports, so a run whose other jobs all reported would never get ci-ok. When one or
# two jobs are left unreported, read the run's jobs once (the latest attempt's, as GitHub lists
# them): each such job GitHub shows completed stands at its conclusion, a called workflow's jobs
# (`<job> / <its job>`) together. Fewer reads than every job reading the list, which the token's
# hourly limit could not carry; a job that ends unreported as the run's very last leaves ci-ok
# unposted, and re-running it posts ci-ok. This read retries like the one above and is bounded as
# it is; a read that fails every attempt leaves the jobs unreported.
unreported=$(jq '.pending | length' <<<"$decision")
# shellcheck disable=SC2016 # $1 to $3 are sh -c's own arguments, expanded by that shell
if [ "$unreported" -ge 1 ] && [ "$unreported" -le 2 ] \
  && retry_fetch "the run's job list" 30 -- sh -c 'gh api --paginate "$1" --jq "$2" > "$3"' \
    gh-api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/jobs?per_page=100" \
    '.jobs[] | {name, status, conclusion}' "$work/jobs"; then
  decision=$(jq --argjson jobs "$(jq -s . "$work/jobs")" '
    def ended($job):
      [$jobs[] | select(.name == $job or (.name | startswith($job + " / ")))] as $mine
      | if ($mine | length) > 0 and all($mine[]; .status == "completed") then
          [$mine[].conclusion]
          | if any(. == "success" or . == "skipped" or . == "cancelled" | not) then "failure"
            elif any(. == "cancelled") then "cancelled"
            elif any(. == "success") then "success"
            else "skipped" end
        else null end;
    reduce .pending[] as $job (.;
      ended($job) as $result
      | if $result == null then . else (.pending -= [$job]) | (.needs[$job].result = $result) end)' \
    <<<"$decision")
fi

pending=$(jq -r '.pending | join(", ")' <<<"$decision")
if [ -n "$pending" ]; then
  echo "$JOB reported; ci-ok waits on $pending, and the last of them to finish posts it"
  exit 0
fi

# Under $work, so the one EXIT trap above removes it: a second `trap … EXIT` would replace that
# one and leave the directory behind.
summary="$work/summary"
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
  echo "Decided by $JOB, the last job of the run to report ([run]($GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID))."
  echo
  echo '```text'
  echo "$verdict"
  echo '```'
} >"$summary"
CHECK_NAME=ci-ok CONCLUSION=$conclusion TITLE=$title SUMMARY_FILE=$summary \
  DETAILS_URL=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID \
  "$script_dir/post-check-run.sh"
