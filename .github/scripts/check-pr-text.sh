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
# the commits from the pull request's commits route with gh (GH_TOKEN, GITHUB_REPOSITORY). That
# route returns at most 250 commits even with --paginate, so a pull request past that cap is
# refused loudly here instead of silently scanning a truncated list. A NUL byte in any of the four,
# which would make `grep -I` treat the file as binary and skip it, is stripped first.
#
# Usage: GITHUB_EVENT_PATH=<payload> GITHUB_REPOSITORY=<owner>/<repo> .github/scripts/check-pr-text.sh
# CI runs it in the required lint job of pr-and-main.yaml on every pull request push, and in
# pr-title.yaml when a pull request is edited; its tests are in check-private-names.test.sh.
set -euo pipefail

event=${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH must name the pull_request event payload}
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
script_dir=$(cd "$(dirname "$0")" && pwd)
text=$(mktemp -d)
commits_json=$(mktemp)
trap 'rm -rf "$text" "$commits_json"' EXIT

number=$(jq -er '.pull_request.number' "$event")
expected_commits=$(jq -r '.pull_request.commits // 0' "$event")
jq -r '.pull_request.title' "$event" | tr -d '\000' > "$text/title"
jq -r '.pull_request.body // ""' "$event" | tr -d '\000' > "$text/body"
jq -r '.pull_request.head.ref' "$event" | tr -d '\000' > "$text/branch"

gh api "repos/$repository/pulls/$number/commits" --paginate > "$commits_json"
got_commits=$(jq 'length' "$commits_json")
if [ "$expected_commits" -gt 0 ] && [ "$got_commits" -ne "$expected_commits" ]; then
  echo "::error::pull request reports $expected_commits commits but the commits route returned" \
    "$got_commits (its hard cap is 250); reduce the pull request's commit count so every commit" \
    "can be checked" >&2
  exit 1
fi
jq -r '.[] | .commit.message, .commit.author.name, .commit.author.email, .commit.committer.name, .commit.committer.email' \
  "$commits_json" | tr -d '\000' > "$text/commits"

# The guard scans these files by their absolute names; the log names them by the part after the
# temporary directory. pipefail makes the guard's exit status the script's.
"$script_dir/check-private-names.sh" "$text/title" "$text/body" "$text/branch" "$text/commits" 2>&1 |
  sed "s|$text/||g"
