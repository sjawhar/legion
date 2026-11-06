#!/usr/bin/env bash
# The last step of ci.yaml's `ci-ok` job: it sets every other ci-ok check run on this head to the
# conclusion that job just reached, so exactly one verdict stands at the end of every attempt.
#
# Why there can be another one at all. ci-ok-report.sh posts a failing ci-ok from the job whose
# result decides a merge-group run, with POST /repos/.../check-runs, and such a check run joins
# the Actions App's first check suite at that commit rather than the posting run's: measured on
# pull request #2860 at 57182d42, where CI's ci-ok landed in the suite of "Rebuild Claude Envoy
# plugin" (114301219205) rather than in CI's own (114301219822). Two things follow. A posted
# ci-ok is not the ci-ok job's own check run, so GitHub never replaces it on a re-run: a run that
# failed, was posted a failing ci-ok and was then re-run green would keep that failing check run
# on the head for ever, and main's ruleset reads every check run of the required name. And it can
# sit in another workflow's check suite, where no re-run of this workflow reaches it at all.
# PATCHing each of them to this job's conclusion closes both: whatever a head carries under the
# name ci-ok, it says what this attempt decided.
#
# What it leaves alone: this job's own check run (CHECK_RUN_ID, `job.check_run_id`), which GitHub
# concludes itself; a check run that already carries this conclusion, since a PATCH that changes
# nothing is still a write; and one that is not completed, which belongs to a run still deciding
# — its own job writes its conclusion, and completing it here would overwrite a verdict that is
# still being made.
#
# A refusal leaves a warning and never fails the job. The job's conclusion is the verdict, and
# failing this step would turn a passing one red, which blocks the head this step exists to
# unblock; a check run it could not reach stands as it was, and the next attempt tries again.
#
# Its inputs, from the step's env:
#   GH_TOKEN           the job's github.token, with `checks: write` (the read and the PATCH).
#   GITHUB_REPOSITORY  owner/repo, as Actions sets it.
#   HEAD_SHA           the commit this run's ci-ok is on: a pull request's head or github.sha.
#   CHECK_RUN_ID       this job's own check run (`job.check_run_id`), the one to leave alone.
#   CONCLUSION         this job's own conclusion (`job.status`): success, failure or cancelled.
#   GITHUB_SERVER_URL, GITHUB_RUN_ID, as Actions sets them: the PATCHed runs link to this job.
#   GH_RETRY_SECONDS   optional: gh-read.sh's knob, the tests' way to retry without waiting.
#
# `check_name=ci-ok&filter=all` is the read: the default filter, `latest`, returns one check run
# per name, and a head can carry several — this job's own, one an earlier attempt's job left, and
# one ci-ok-report.sh posted — and `per_page=100` with --paginate reads them all, where the
# default page holds the 30 newest check runs of the commit.
#
# Run from anywhere: GH_TOKEN=... GITHUB_REPOSITORY=o/r HEAD_SHA=... CHECK_RUN_ID=... \
#   CONCLUSION=... .github/scripts/ci-ok-reconcile.sh
# Its tests are in ci-ok-reconcile.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"

for input in GH_TOKEN GITHUB_REPOSITORY HEAD_SHA CHECK_RUN_ID CONCLUSION GITHUB_SERVER_URL GITHUB_RUN_ID; do
  if [ -z "${!input:-}" ]; then
    echo "::error::ci-ok-reconcile.sh needs $input in its environment"
    exit 1
  fi
done
case $CONCLUSION in
  success | failure | cancelled) ;;
  *)
    echo "::error::ci-ok-reconcile.sh takes job.status, which is success, failure or cancelled, not '$CONCLUSION'"
    exit 1
    ;;
esac

if ! runs=$(gh_read gh api --paginate \
  "repos/$GITHUB_REPOSITORY/commits/$HEAD_SHA/check-runs?check_name=ci-ok&filter=all&per_page=100" \
  --jq '.check_runs[] | "\(.id) \(.status) \(.conclusion)"'); then
  echo "::warning::GitHub refused this head's ci-ok check runs three times, so any ci-ok beside this job's own stands as it was; this job's own conclusion ($CONCLUSION) is this attempt's verdict"
  exit 0
fi

body=$(mktemp)
trap 'rm -f "$body"' EXIT
jq -n --arg conclusion "$CONCLUSION" \
  --arg title "ci-ok is $CONCLUSION on this head: the ci-ok job of run $GITHUB_RUN_ID decided it" \
  --arg summary "This check run was posted by a job of a run rather than concluded by one, so no re-run updates it. The ci-ok job of [run $GITHUB_RUN_ID]($GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID) set it to that run's own conclusion, so one verdict stands on this head." \
  --arg url "$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/job/$CHECK_RUN_ID" \
  '{conclusion: $conclusion, details_url: $url, output: {title: $title, summary: $summary}}' >"$body"

reconciled=0
while read -r id status was; do
  [ -n "$id" ] || continue
  if [ "$id" = "$CHECK_RUN_ID" ]; then
    echo "ci-ok check run $id is this job's own ($CONCLUSION)"
    continue
  fi
  if [ "$status" != completed ]; then
    echo "ci-ok check run $id is $status, so the run deciding it writes its conclusion"
    continue
  fi
  if [ "$was" = "$CONCLUSION" ]; then
    echo "ci-ok check run $id already concluded $was"
    continue
  fi
  if gh_read gh api --silent --method PATCH "repos/$GITHUB_REPOSITORY/check-runs/$id" --input "$body"; then
    echo "set ci-ok check run $id from $was to $CONCLUSION"
    reconciled=$((reconciled + 1))
  else
    echo "::warning::GitHub refused the PATCH of ci-ok check run $id three times, so it stands at $was while this attempt's verdict is $CONCLUSION"
  fi
done <<<"$runs"
echo "ci-ok on $HEAD_SHA is $CONCLUSION; $reconciled other check run(s) of that name were set to it"
