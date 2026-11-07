#!/usr/bin/env bash
# The last step of ci.yaml's `ci-ok` job: it sets every other ci-ok check run on this head to the
# conclusion that job just reached, so exactly one verdict stands at the end of every attempt.
#
# Why there can be another one at all. ci-ok-report.sh posts a failing ci-ok from the job whose
# result decides a merge-group run, with POST /repos/.../check-runs, and such a check run joins
# the Actions App's first check suite at that commit rather than the posting run's
# (post-check-run.sh's header holds the measurement). Two things follow. A posted ci-ok is not
# the ci-ok job's own check run, so GitHub never replaces it on a re-run: a run that failed, was
# posted a failing ci-ok and was then re-run green would keep that failing check run on the head
# for ever. And it can sit in another workflow's check suite, where no re-run of this workflow
# reaches it at all. A re-run's own job check run needs nothing from this step, measured on the
# k28 probe of INFRA-1066 (2026-11-06, head ee30dee4): attempt 1's failing ci-ok job check run
# (127883400181) stays on the head under filter=all, but the default `latest` filter returns
# attempt 2's alone (127889647834) and GitHub's statusCheckRollup reads SUCCESS with one ci-ok
# context, so a later attempt of the same job supersedes its own earlier one. PATCHing each
# posted ci-ok to this job's conclusion closes both cases: whatever a head carries under the name
# ci-ok that a job of a run posted, it says what this attempt decided.
#
# Why it is not every other ci-ok on the head. One of them can be another workflow run's own
# ci-ok job check run: main-bypass-check.yaml starts this workflow with `gh workflow run ci.yaml
# --ref main`, so that dispatched run's github.sha is main's tip, and the queue landed that tip
# carrying its own merge group's green ci-ok. GitHub will not let this step rewrite it, which the
# k28 probe of INFRA-1066 measured on 2026-11-06: an unfiltered copy of this step, run under
# GITHUB_TOKEN against another run's completed ci-ok job check run (127883380092), was refused
# three times with "Check run status and conclusions can only be updated internally by GitHub
# Actions". So the rewrite is impossible rather than merely wrong, and what an unfiltered step
# costs is the whole retry ladder against a call that cannot succeed, under this job's
# timeout-minutes, and a warning saying a verdict stands that was never this step's to change.
#
# The external_id is what separates the two, by what wrote the check run rather than by what it
# says, and post-check-run.sh's header gives its three shapes. This step PATCHes a
# `ci-ok-post:<run id>` prefix and an absent marker (`-` in the read below) alike, and sends a
# prefix back, so a later attempt's read of this head still finds one; it leaves a UUID alone,
# which is a job's own check run and which GitHub refuses to let this token PATCH in any case.
# The absent ones are the posts of the release before the marker existed, and they sit on open
# heads today: 126296045537 on pull request #2726 at 4a878981 and 127791633790 on #2860 at
# 57182d42, both `success`, both with an empty external_id, measured on 2026-11-06. No title is
# tested beside the field: every job's own ci-ok check run carries a UUID, so the external_id
# alone decides which is which.
#
# What it leaves alone: this job's own check run (CHECK_RUN_ID, `job.check_run_id`), which GitHub
# concludes itself; a check run that already carries this conclusion, since a PATCH that changes
# nothing is still a write; one that is not completed, which belongs to a run still deciding
# — its own job writes its conclusion, and completing it here would overwrite a verdict that is
# still being made; and one whose external_id is a UUID, which no job of a run posted, so GitHub
# answers a PATCH of it with that refusal and the check run is in any case the business of the
# run whose job GitHub concluded it for.
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
# per name, and a head can carry several — this job's own, one an earlier attempt's job left, one
# ci-ok-report.sh posted, and another run's own — and `per_page=100` with --paginate reads them
# all, where the default page holds the 30 newest check runs of the commit. Each line carries the
# check run's external_id as a fourth field, where a check run with none reads as `-` rather than
# as the empty string GitHub returns for it: the loop reads the line with `read -r`, which a
# field that is a word on every line keeps lined up whatever a check run carries, and `-` is the
# word the loop reads as a post of the release before the marker.
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
  --jq '.check_runs[] | "\(.id) \(.status) \(.conclusion) \(.external_id // "" | if . == "" then "-" else . end)"'); then
  echo "::warning::GitHub refused this head's ci-ok check runs three times, so any ci-ok beside this job's own stands as it was; this job's own conclusion ($CONCLUSION) is this attempt's verdict"
  exit 0
fi

body=$(mktemp)
trap 'rm -f "$body"' EXIT
# external_id goes with the PATCH, so the check run still carries a ci-ok-post: marker when the
# next attempt reads this head; it names this run, as the title, summary and link of the same
# body already do.
jq -n --arg conclusion "$CONCLUSION" \
  --arg title "ci-ok is $CONCLUSION on this head: the ci-ok job of run $GITHUB_RUN_ID decided it" \
  --arg summary "This check run was posted by a job of a run rather than concluded by one, so no re-run updates it. The ci-ok job of [run $GITHUB_RUN_ID]($GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID) set it to that run's own conclusion, so one verdict stands on this head." \
  --arg url "$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/job/$CHECK_RUN_ID" \
  --arg external_id "ci-ok-post:$GITHUB_RUN_ID" \
  '{conclusion: $conclusion, details_url: $url, external_id: $external_id,
    output: {title: $title, summary: $summary}}' >"$body"

reconciled=0
while read -r id status was marker; do
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
  case $marker in
    ci-ok-post:* | -) ;;
    *)
      echo "ci-ok check run $id carries the external_id $marker, which is neither a ci-ok-post: marker nor absent, so it is a job's own ci-ok check run, which the run whose job it is concludes itself"
      continue
      ;;
  esac
  if gh_read gh api --silent --method PATCH "repos/$GITHUB_REPOSITORY/check-runs/$id" --input "$body"; then
    echo "set ci-ok check run $id from $was to $CONCLUSION"
    reconciled=$((reconciled + 1))
  else
    echo "::warning::GitHub refused the PATCH of ci-ok check run $id three times, so it stands at $was while this attempt's verdict is $CONCLUSION"
  fi
done <<<"$runs"
echo "ci-ok on $HEAD_SHA is $CONCLUSION; $reconciled other check run(s) of that name were set to it"
