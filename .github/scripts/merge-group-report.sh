#!/usr/bin/env bash
# The read half of a merge-group follower cancel job (.github/actions/merge-group-cancel): what
# the job the follower follows reported for this run attempt, as the one word
# merge-group-cancels.sh takes as REPORT. It prints that word on stdout and its reasoning on
# stderr, makes no GitHub write, and reads every fact from one artifact list, so a test drives it
# from fixtures with a stand-in gh.
#
# A follower fires on its job's `failure` or `cancelled` result, and that result alone does not
# say the job's own work failed: a job whose tests all passed ends `failure` when its
# ci-ok-report step cannot reach GitHub. Two artifacts of the run tell the cases apart, both
# uploaded by .github/actions/ci-ok-report and both named once in ci-ok.jq:
#
#   * `ci-ok-<job>-<run attempt>-<success|failure|cancelled>`, the job's own result, uploaded
#     before that step posts anything;
#   * `ci-ok-posted-<job>-<run attempt>`, uploaded only after the job's ci-ok post succeeded.
#
# So the answers are:
#
#   * `success` — the job's work passed and its report step alone failed.
#   * `failure` or `cancelled` — the job's work failed and this group's failing ci-ok is posted.
#   * `unposted` — the job's work failed and nothing here shows a ci-ok went up for it: its
#     artifact list or its post was refused three times, or the marker's own upload failed.
#     Either way this run is the only one that can still post this group's verdict, so the act
#     must leave it alive; a marker lost after a post that worked costs at most one more posting
#     of the same failing verdict, by a later job of the run.
#   * `absent` — the job uploaded no report at all. `changes` always gives this: it carries no
#     ci-ok-report step. A job whose upload failed leaves the same absence.
#   * `unknown` — GitHub refused the artifact list three times, so nothing here shows what that
#     job did.
#
# .github/scripts/merge-group-cancels.sh's header says what each of those makes the act do, and
# what it trades; it is the one place that says it.
#
# Its inputs, from the step's env:
#   GH_TOKEN           the job's github.token, with `actions: read` (the artifact list); the
#                      follower's `actions: write` covers it.
#   JOB                the id of the job the follower follows, from its `needs:`.
#   RUN_ATTEMPT        github.run_attempt, the attempt whose reports count.
#   GITHUB_REPOSITORY, GITHUB_RUN_ID, as Actions sets them.
#   GH_RETRY_SECONDS   optional: the retry delay of the artifact list's read (gh-read.sh).
#
# Run from anywhere: JOB=... RUN_ATTEMPT=... .github/scripts/merge-group-report.sh
# Its tests are in merge-group-cancels.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"

for input in GH_TOKEN JOB RUN_ATTEMPT GITHUB_REPOSITORY GITHUB_RUN_ID; do
  if [ -z "${!input:-}" ]; then
    echo "::error::merge-group-report.sh needs $input in its environment"
    exit 1
  fi
done

# The one read: every artifact name of the run, on gh-read.sh's ladder. Three refusals are
# `unknown`, which cancels nothing — an act proves what it cancels — rather than a failed step,
# since the follower's log is where that refusal belongs and the act still reports it.
if ! names=$(gh_read gh api --paginate \
  "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100" \
  --jq '.artifacts[].name'); then
  echo "::warning::the run's artifact list could not be read, so what $JOB's own work did cannot be told here, and this follower cancels nothing" >&2
  echo unknown
  exit 0
fi

# The word and the reasoning, tab-separated as merge-group-cancels.sh's rows are: the word goes
# to stdout, where the step reads it, and the reasoning to stderr, where the job log keeps it
# beside the act.
answer=$(jq -r -n -L "$script_dir" --arg job "$JOB" --arg attempt "$RUN_ATTEMPT" \
  --argjson names "$(jq -R . <<<"$names" | jq -s 'map(select(. != ""))')" '
include "ci-ok";
(reports($names) | map(select(.job == $job and .attempt == $attempt)) | first) as $report
| (posted($names) | any(.job == $job and .attempt == $attempt)) as $posted
| if $report == null then
    ["absent", "\($job) uploaded no report for attempt \($attempt)"]
  elif $report.status == "success" then
    ["success", "\($job) reported success for attempt \($attempt): its own work passed"]
  elif $posted then
    [$report.status, "\($job) reported \($report.status) for attempt \($attempt), and the ci-ok it posted for this group is in the run artifacts beside it"]
  else
    ["unposted", "\($job) reported \($report.status) for attempt \($attempt), and no marker of a posted ci-ok for this group is beside it"]
  end
| @tsv')
IFS=$'\t' read -r report reasoning <<<"$answer"
echo "$reasoning" >&2
echo "$report"
