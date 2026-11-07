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

# GitHub refuses a summary longer than 65535 characters. The body goes to a file rather than down
# this script's standard input: the post retries, and an attempt that had consumed that input
# would leave the attempt after it posting an empty body.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
jq -n --arg name "$CHECK_NAME" --arg sha "$HEAD_SHA" --arg conclusion "$CONCLUSION" \
  --arg url "$DETAILS_URL" --arg title "$TITLE" --rawfile summary "$SUMMARY_FILE" \
  '{name: $name, head_sha: $sha, status: "completed", conclusion: $conclusion, details_url: $url,
    output: {title: $title, summary: ($summary | .[0:65000])}}' > "$work/body.json"
# The post retries: .github/scripts/ci-retry.sh makes five bounded attempts, the last starting
# 150 s after the first, and an exhausted post fails the step. The loop that stood here was a
# second copy of that helper, which is what dispatch://INFRA-1545's R2 refuses. gh writes the
# posted check's URL into a file through `sh -c`, so that each attempt starts that file over,
# for the reason ci-ok-report.sh gives where it does the same.
#
# 30 s per attempt: the bound that file derives there for a JSON call on one REST route, about
# 3 s of work, which this post is no larger than.
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/ci-retry.sh
. "$script_dir/ci-retry.sh"
# shellcheck disable=SC2016 # $1 to $4 are sh -c's own arguments, expanded by that shell
if retry_fetch "the $CHECK_NAME check run" 30 -- \
  sh -c 'gh api --method POST "$1" --input "$2" --jq "$3" > "$4"' gh-api \
  "repos/$GITHUB_REPOSITORY/check-runs" "$work/body.json" '.html_url' "$work/posted"; then
  echo "posted $CHECK_NAME ($CONCLUSION) on $HEAD_SHA: $(<"$work/posted")"
  exit 0
fi
echo "::error::GitHub refused the $CHECK_NAME check run on $HEAD_SHA five times"
exit 1
