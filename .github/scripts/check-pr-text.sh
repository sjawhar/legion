#!/usr/bin/env bash
# Runs check-private-names.sh over a pull request's title, body and commit messages, which are as
# public as the tree: with this repository's squash settings (the commit-or-PR title, then the
# commit messages), the title or a commit subject becomes main's commit subject and the commit
# messages its body; the release workflows' generated notes list merged titles; and the body and
# every branch commit show on GitHub. A hit is reported as `title:<line>`, `body:<line>` or
# `commits:<line>`, never its text.
#
# It reads the title and body from the pull_request event payload GitHub writes to
# GITHUB_EVENT_PATH, never from the step's environment, which the runner prints in the job log, and
# the commit messages from the pull request's commits route with gh (GH_TOKEN, GITHUB_REPOSITORY).
#
# Usage: GITHUB_EVENT_PATH=<payload> GITHUB_REPOSITORY=<owner>/<repo> .github/scripts/check-pr-text.sh
# CI runs it in the required lint job of pr-and-main.yaml on every pull request push, and in
# pr-title.yaml when a pull request is edited; its tests are in check-private-names.test.sh.
set -euo pipefail

event=${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH must name the pull_request event payload}
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
script_dir=$(cd "$(dirname "$0")" && pwd)
text=$(mktemp -d)
trap 'rm -rf "$text"' EXIT

number=$(jq -er '.pull_request.number' "$event")
jq -r '.pull_request.title' "$event" > "$text/title"
jq -r '.pull_request.body // ""' "$event" > "$text/body"
gh api "repos/$repository/pulls/$number/commits" --paginate --jq '.[].commit.message' > "$text/commits"

# The guard scans these files by their absolute names; the log names them by the part after the
# temporary directory. pipefail makes the guard's exit status the script's.
"$script_dir/check-private-names.sh" "$text/title" "$text/body" "$text/commits" 2>&1 |
  sed "s|$text/||g"
