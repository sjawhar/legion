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
#   GH_RETRY_SECONDS   optional: a refused or failed post is retried twice on gh-read.sh's one
#                      ladder, this many seconds and then twice as many apart (10 by default; the
#                      tests set 0), then fails the step.
#
# Run from anywhere: HEAD_SHA=... CHECK_NAME=... CONCLUSION=... TITLE=... SUMMARY_FILE=...
#   DETAILS_URL=... .github/scripts/post-check-run.sh
# Its tests are in ci-ok-report.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"

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

# GitHub refuses a summary longer than 65535 characters. The body goes to a file rather than to
# gh's stdin: a retried post must send it again, and the three attempts of gh-read.sh's ladder
# share the one stdin a here-string gives them, which nothing rewinds. Measured under bash 5.2
# over that ladder's shape: a refused attempt that read nothing leaves the whole body for the
# next attempt, one that read the body to its end leaves it nothing, and one refused after 40 of
# 121 bytes leaves the other 81 — so the body a retry sent would be whatever the refusal before
# it happened to leave. `--input` names a file, which every attempt opens and reads in full.
body=$(mktemp)
trap 'rm -f "$body"' EXIT
jq -n --arg name "$CHECK_NAME" --arg sha "$HEAD_SHA" --arg conclusion "$CONCLUSION" \
  --arg url "$DETAILS_URL" --arg title "$TITLE" --rawfile summary "$SUMMARY_FILE" \
  '{name: $name, head_sha: $sha, status: "completed", conclusion: $conclusion, details_url: $url,
    output: {title: $title, summary: ($summary | .[0:65000])}}' >"$body"
if posted=$(gh_read gh api --method POST "repos/$GITHUB_REPOSITORY/check-runs" --input "$body" --jq '.html_url'); then
  echo "posted $CHECK_NAME ($CONCLUSION) on $HEAD_SHA: $posted"
  exit 0
fi
echo "::error::GitHub refused the $CHECK_NAME check run on $HEAD_SHA three times"
exit 1
