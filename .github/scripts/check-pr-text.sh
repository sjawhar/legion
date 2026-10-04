#!/usr/bin/env bash
# Runs check-private-names.sh over a pull request's title, body, branch name and commits (each
# commit's message, author name and email, and committer name and email), which are as public as
# the tree: with this repository's squash settings (the commit-or-PR title, then the commit
# messages), the title or a commit subject becomes main's commit subject and the commit messages
# its body; the release workflows' generated notes list merged titles; the branch name keeps
# showing on the pull request's page after the branch is deleted; and a commit's author or
# committer identity reaches main through a squash's auto-generated Co-authored-by trailer. A hit
# is reported as `title:<line>`, `body:<line>`, `branch:<line>` or `commits:<line>`, never its text.
#
# It reads the title, body and branch name from the pull_request event payload GitHub writes to
# GITHUB_EVENT_PATH, never from the step's environment, which the runner prints in the job log, and
# the commits from the compare route with gh (GH_TOKEN, GITHUB_REPOSITORY), for the payload's base
# and head sha: the head this run checks, not whatever head the pull request has by the time the
# route is read, since a push can land while the job runs and gets a run of its own. The route
# pages through every commit and reports their total, so a list that comes back shorter than that
# total is refused loudly here instead of silently scanning part of it. A NUL byte in any of the
# four, which would make `grep -I` treat the file as binary and skip it, is stripped first.
#
# Usage: GITHUB_EVENT_PATH=<payload> GITHUB_REPOSITORY=<owner>/<repo> .github/scripts/check-pr-text.sh
# CI runs it in the required lint job of pr-and-main.yaml on every pull request push, and in
# pr-title.yaml when a pull request is edited; its tests are in check-private-names.test.sh.
set -euo pipefail

event=${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH must name the pull_request event payload}
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
script_dir=$(cd "$(dirname "$0")" && pwd)
text=$(mktemp -d)
compare_json=$(mktemp)
trap 'rm -rf "$text" "$compare_json"' EXIT

base=$(jq -er '.pull_request.base.sha' "$event")
head=$(jq -er '.pull_request.head.sha' "$event")
jq -r '.pull_request.title' "$event" | tr -d '\000' > "$text/title"
jq -r '.pull_request.body // ""' "$event" | tr -d '\000' > "$text/body"
jq -r '.pull_request.head.ref' "$event" | tr -d '\000' > "$text/branch"

# GitHub limits this route to 250 commits for a call with no paging parameter: `?per_page=100` is
# that parameter, and asks for the largest page GitHub serves; `--paginate` then reads every page
# it creates, printing one JSON object per page, each repeating total_commits.
gh api "repos/$repository/compare/$base...$head?per_page=100" --paginate > "$compare_json"
total_commits=$(jq -s '.[0].total_commits // empty' "$compare_json")
if [ -z "$total_commits" ]; then
  echo "::error::the compare route's answer for $base...$head names no total_commits, so the" \
    "commit list cannot be checked against it" >&2
  exit 1
fi
listed_commits=$(jq -s '[.[].commits[]] | length' "$compare_json")
if [ "$listed_commits" -ne "$total_commits" ]; then
  echo "::error::the compare route reports $total_commits commits from the pull request's base to" \
    "$head but listed $listed_commits, so not every commit can be checked" >&2
  exit 1
fi
jq -r '.commits[] | .commit.message, .commit.author.name, .commit.author.email, .commit.committer.name, .commit.committer.email' \
  "$compare_json" | tr -d '\000' > "$text/commits"

# The guard scans these files by their absolute names; the log names them by the part after the
# temporary directory. pipefail makes the guard's exit status the script's.
"$script_dir/check-private-names.sh" "$text/title" "$text/body" "$text/branch" "$text/commits" 2>&1 |
  sed "s|$text/||g"
