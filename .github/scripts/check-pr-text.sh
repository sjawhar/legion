#!/usr/bin/env bash
# Runs check-private-names.sh over a pull request's title, body, branch name and commits (each
# commit's message, author name and email, and committer name and email), and over every issue
# comment, review and review comment on it that a trusted author posted (see "Who counts" below).
# The first four are as public as the tree: with this
# repository's squash settings (the commit-or-PR title, then the commit messages), the title or a
# commit subject becomes main's commit subject and the commit messages its body; the release
# workflows' generated notes list merged titles; the branch name keeps showing on the pull
# request's page after the branch is deleted; and a commit's author or committer identity reaches
# main through a squash's auto-generated Co-authored-by trailer. A comment, review or review
# comment is public on the pull request's page from the moment it is posted. A hit is reported as
# `title:<line>`, `body:<line>`, `branch:<line>`, `commits:<line>`, `comment:<id>:<line>`,
# `review:<id>:<line>` or `review-comment:<id>:<line>` (the id GitHub gives that issue comment,
# review or review comment), never its text.
#
# It reads the event payload GitHub writes to GITHUB_EVENT_PATH, never the step's environment,
# which the runner prints in the job log. A pull_request, pull_request_review or
# pull_request_review_comment payload carries the pull request: its number, title, body, branch
# name, and the base and head sha the run checks, which is the head the event names rather than
# whatever head the pull request has by the time a route is read, since a push can land while the
# job runs and gets a run of its own. An issue_comment payload names the pull request only as an
# issue, so the rest is read from the pulls route, and a comment on an issue that is no pull
# request is refused. The rest it reads with gh (GH_TOKEN, GITHUB_REPOSITORY): the commits from the
# compare route between that base and head, which pages through every commit and reports their
# total, so a list that comes back shorter than that total is refused loudly here instead of
# silently scanning part of it; and the issue comments, reviews and review comments from their
# list routes, which report no total, so each is read 100 at a time until a page comes back short.
# There is no total to cross-check those lists against, and none is needed: posting, editing or
# deleting one starts its own run, which reads after the change, so a list that shifted while one
# run read it is read again by the run its change started. A NUL byte in any of them, which would
# make `grep -I` treat the file as binary and skip it, is stripped first.
#
# Usage: GITHUB_EVENT_PATH=<payload> GITHUB_REPOSITORY=<owner>/<repo> .github/scripts/check-pr-text.sh
# CI runs it in the required lint job of pr-and-main.yaml on every pull request push, and in
# pr-title.yaml when a pull request is edited or a comment, review or review comment on it is
# posted, edited or deleted; its tests are in check-private-names.test.sh.
set -euo pipefail

event=${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH must name the pull request event payload}
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
script_dir=$(cd "$(dirname "$0")" && pwd)
text=$(mktemp -d)
work=$(mktemp -d)
trap 'rm -rf "$text" "$work"' EXIT

pull="$work/pull.json"
if jq -e 'has("pull_request")' "$event" > /dev/null; then
  jq '.pull_request' "$event" > "$pull"
else
  issue=$(jq -r '.issue | select(.pull_request) | .number' "$event")
  if ! [[ $issue =~ ^[0-9]+$ ]]; then
    echo "::error::the event payload names no pull request: it has no pull_request, and no issue" \
      "that is one" >&2
    exit 1
  fi
  gh api "repos/$repository/pulls/$issue" > "$pull"
fi
number=$(jq -er '.number' "$pull")
base=$(jq -er '.base.sha' "$pull")
head=$(jq -er '.head.sha' "$pull")
jq -r '.title' "$pull" | tr -d '\000' > "$text/title"
jq -r '.body // ""' "$pull" | tr -d '\000' > "$text/body"
jq -r '.head.ref' "$pull" | tr -d '\000' > "$text/branch"

# GitHub limits this route to 250 commits for a call with no paging parameter: `?per_page=100` is
# that parameter, and asks for the largest page GitHub serves; `--paginate` then reads every page
# it creates, printing one JSON object per page, each repeating total_commits.
compare_json="$work/compare.json"
gh api "repos/$repository/compare/$base...$head?per_page=100" --paginate > "$compare_json"
total_commits=$(jq -s '.[0].total_commits // empty' "$compare_json")
if ! [[ $total_commits =~ ^[0-9]+$ ]]; then
  echo "::error::the compare route's answer for $base...$head names no whole-number" \
    "total_commits, so the commit list cannot be checked against it" >&2
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

# Who counts: an issue comment, review or review comment is scanned only when an App installed on
# this repository posted it (it posts as a Bot, which only such an App or github-actions can do;
# Legion's role Apps carry the CONTRIBUTOR association here, so association alone would drop them)
# or its author is the repository's owner, a member of its organization or a collaborator. On a
# public repository anyone signed in can comment or review, and every run reports the required
# check; a stranger's text skipped this way can neither fail the check nor clear one that failed.
# Each skipped item is printed as `skipped <kind>:<id>: author <login> (<association>) is not
# trusted`, with no text.
trusted='.user.type == "Bot" or (.author_association // "" | IN("OWNER", "MEMBER", "COLLABORATOR"))'

# read_list <kind> <route>: writes the body of every trusted item the list route answers to
# $text/<kind>/<id>, reading 100 items a page until a page holds fewer. An answer that is not a
# list, or an item whose id is not a whole number, is refused rather than scanned in part.
read_list() {
  local kind=$1 route=$2 page=1 id trust author body
  mkdir "$text/$kind"
  while :; do
    gh api "repos/$repository/$route?per_page=100&page=$page" > "$work/answer"
    if ! jq -e 'type == "array"' "$work/answer" > /dev/null 2>&1; then
      echo "::error::page $page of the $route route is not a list, so its ${kind}s cannot be" \
        "checked" >&2
      exit 1
    fi
    jq -r ".[] | [(.id | tojson), ($trusted | tostring),
        \"\(.user.login // \"<none>\") (\(.author_association // \"<none>\"))\",
        (.body // \"\" | explode | map(select(. != 0)) | implode | @base64)] | @tsv" \
      "$work/answer" > "$work/items"
    while IFS=$'\t' read -r id trust author body; do
      if ! [[ $id =~ ^[0-9]+$ ]]; then
        echo "::error::page $page of the $route route lists a $kind whose id is not a whole" \
          "number, so it cannot be reported" >&2
        exit 1
      fi
      if [ "$trust" = true ]; then
        base64 -d <<< "$body" > "$text/$kind/$id"
      else
        echo "skipped $kind:$id: author $author is not trusted"
      fi
    done < "$work/items"
    if [ "$(wc -l < "$work/items")" -lt 100 ]; then break; fi
    page=$((page + 1))
  done
}
# Each kind a hit is reported under, beside its route; the reads, the scan and the report below
# all take the kinds from here.
lists=(
  comment "issues/$number/comments"
  review "pulls/$number/reviews"
  review-comment "pulls/$number/comments"
)
kinds=()
for ((i = 0; i < ${#lists[@]}; i += 2)); do
  kinds+=("${lists[i]}")
  read_list "${lists[i]}" "${lists[i + 1]}"
done

# The guard scans these by their absolute names; the log names each by the part after the
# temporary directory, and an item as <kind>:<id>.
kind_pattern="($(IFS='|'; echo "${kinds[*]}"))"
status=0
"$script_dir/check-private-names.sh" "$text/title" "$text/body" "$text/branch" "$text/commits" \
  "${kinds[@]/#/$text/}" 2>&1 | sed -E -e "s|$text/||g" -e "s#(^|[:=])$kind_pattern/([0-9]+)#\\1\\2:\\3#g" \
  > "$work/report" || status=$?
cat "$work/report"
if [ "$status" -ne 0 ] && grep -q -E "(^|[:=])$kind_pattern:[0-9]+:" "$work/report"; then
  echo "  - a comment, review or review comment reported above is public on the pull request's page"
  echo "    now: delete each comment and review comment, since an edit keeps the old text in its"
  echo "    edit history, which anyone who can read the repository can open; a submitted review"
  echo "    cannot be deleted, so edit its body and delete the old revision from that history."
  echo "    pr-title.yaml checks the pull request again on each deletion and edit."
fi
exit "$status"
