#!/usr/bin/env bash
# Posts one completed check run on a commit as this workflow's GITHUB_TOKEN, which is the GitHub
# Actions App (integration 15368, the source main's ruleset names for its required checks): the
# way ci.yaml reports ci-ok (ci-ok-report.sh) and its lint job reports pr-title, each from a step
# of a job that already holds a runner rather than from a job of its own, which would wait in the
# account's runner queue first. The job needs `checks: write`.
#
# Its inputs, from the step's env:
#   GH_TOKEN           the job's github.token.
#   GITHUB_REPOSITORY  owner/repo, as Actions sets it.
#   HEAD_SHA           the commit the check is for: a pull request's head (not the merge commit a
#                      pull_request run checks out, which no ruleset reads) or the pushed commit.
#   CHECK_NAME         the check's name, exactly as the ruleset requires it.
#   CONCLUSION         success or failure.
#   TITLE              one line, shown beside the check.
#   SUMMARY_FILE       a file whose text is the check's summary (markdown), cut to GitHub's limit.
#   DETAILS_URL        where the check links: the job that decided it.
#   GH_RETRY_SECONDS   optional: a refused or failed post is retried twice, this many seconds
#                      and then twice as many apart (10 by default; the tests set 0), then fails
#                      the step.
#
# Run from anywhere: HEAD_SHA=... CHECK_NAME=... CONCLUSION=... TITLE=... SUMMARY_FILE=...
#   DETAILS_URL=... .github/scripts/post-check-run.sh
# Its tests are in ci-ok-report.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

for input in GH_TOKEN GITHUB_REPOSITORY HEAD_SHA CHECK_NAME CONCLUSION TITLE SUMMARY_FILE DETAILS_URL; do
  if [ -z "${!input:-}" ]; then
    echo "::error::post-check-run.sh needs $input in its environment"
    exit 1
  fi
done
case $CONCLUSION in
  success | failure) ;;
  *)
    echo "::error::post-check-run.sh posts success or failure, not '$CONCLUSION'"
    exit 1
    ;;
esac

# GitHub refuses a summary longer than 65535 characters.
body=$(jq -n --arg name "$CHECK_NAME" --arg sha "$HEAD_SHA" --arg conclusion "$CONCLUSION" \
  --arg url "$DETAILS_URL" --arg title "$TITLE" --rawfile summary "$SUMMARY_FILE" \
  '{name: $name, head_sha: $sha, status: "completed", conclusion: $conclusion, details_url: $url,
    output: {title: $title, summary: ($summary | .[0:65000])}}')
for attempt in 1 2 3; do
  if posted=$(gh api --method POST "repos/$GITHUB_REPOSITORY/check-runs" --input - --jq '.html_url' <<<"$body"); then
    echo "posted $CHECK_NAME ($CONCLUSION) on $HEAD_SHA: $posted"
    exit 0
  fi
  if [ "$attempt" -lt 3 ]; then
    sleep $((attempt * ${GH_RETRY_SECONDS:-10}))
  fi
done
echo "::error::GitHub refused the $CHECK_NAME check run on $HEAD_SHA three times"
exit 1
