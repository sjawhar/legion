#!/usr/bin/env bash
# Runs check-private-names.sh over a pull request's title and body. A squash merge makes the title
# main's commit subject and can carry the body into its message, and the release workflows'
# generated notes list merged titles, so both are as public as the tree. A hit is reported as
# `title:<line>` or `body:<line>`, never its text.
#
# Usage: PR_TITLE=<title> PR_BODY=<body> .github/scripts/check-pr-text.sh
# CI runs it in pr-title.yaml, which re-runs when a pull request is edited; its tests are in
# check-private-names.test.sh.
set -euo pipefail

: "${PR_TITLE?PR_TITLE must name the pull request title}"
script_dir=$(cd "$(dirname "$0")" && pwd)
text=$(mktemp -d)
trap 'rm -rf "$text"' EXIT

printf '%s\n' "$PR_TITLE" > "$text/title"
printf '%s\n' "${PR_BODY-}" > "$text/body"
# The guard scans these files by their absolute names; the log names them by the part after the
# temporary directory. pipefail makes the guard's exit status the script's.
"$script_dir/check-private-names.sh" "$text/title" "$text/body" 2>&1 | sed "s|$text/||g"
