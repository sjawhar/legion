#!/usr/bin/env bash
# Runs check-private-names.sh over a pull request's title, body, branch name and commits (each
# commit's message, author name and email, and committer name and email), and over every issue
# comment, review and review comment on it that a trusted author posted (see "Who counts" below).
# The first four are as public as the tree: with this
# repository's squash settings (the pull request title, then a blank message), the title becomes
# main's commit subject; the release workflows' generated notes list merged titles; the branch
# name keeps showing on the pull request's page after the branch is deleted; and each commit's
# message, author and committer stay public on the pull request's page. A comment, review or review
# comment is public on the pull request's page from the moment it is posted. A hit is reported as
# `title:<line>`, `body:<line>`, `branch:<line>`, `commits:<line>`, `comment:<id>:<line>`,
# `review:<id>:<line>` or `review-comment:<id>:<line>` (the id GitHub gives that issue comment,
# review or review comment), never its text.
#
# It also requires the title to be a conventional commit subject (`type(scope): summary`, the
# scope and a `!` optional): squash merges use the title as main's commit subject, and the release
# workflows version from conventional subjects; this repository's squash merges write an empty
# body, so the title is the only text they read. That failure prints no title text
# either.
#
# It also refuses a GitHub workflow skip instruction in the two texts that carry one. The title is
# the first: this repository squash-merges with the title as the commit subject and an empty
# message (squash_merge_commit_title=PR_TITLE, squash_merge_commit_message=BLANK), so an
# instruction in the title becomes part of main's own commit subject and stops main's runs after
# the merge. The message of the commit the event names as the head is the second: it decides
# whether this pull request's own runs start. GitHub's rule, from its "Skip workflow runs" page, is
# the one applied here:
#
#   1. one of `[skip ci]`, `[ci skip]`, `[no ci]`, `[skip actions]` or `[actions skip]`, in any
#      case, anywhere in the text; or
#   2. a line that is exactly `skip-checks:true` or `skip-checks: true`, in any case and with
#      trailing whitespace allowed, in the last block of the commit message, when two or more empty
#      lines come before that block. A block is a run of lines that are not empty, and a line of
#      nothing but whitespace counts as empty, as git's own trailer reader counts it. GitHub reads
#      a trailer in that one place, so a `skip-checks` line in any earlier block, a last block one
#      empty line below the rest, and a line that starts with the text and goes on, are left alone:
#      GitHub starts the runs for all three. The title is read by rule 1 alone, since a title is
#      one line and carries no trailer block.
#
# The head commit is the only commit read this way: GitHub reads the skip instruction of a pull
# request's head commit and of no other commit in the range. A compare range that lists no commit
# at that sha, which is what a pull request whose head is its base's own commit answers, is logged
# and refuses nothing.
#
# These two refusals print the instruction they matched, and that is the one text this script
# prints: an instruction is nobody's private name, and an author cannot delete one the log will not
# spell. The head commit refusal names the commit by its sha and asks for one more commit, whose
# message becomes the head's; no history has to be rewritten.
#
# It reads the event payload GitHub writes to GITHUB_EVENT_PATH, never the step's environment,
# which the runner prints in the job log. A pull_request or pull_request_review payload carries
# the pull request: its number, and the base and head sha the run checks, which is the head the
# event names rather than whatever head the pull request has by the time a route is read, since a
# push can land while the job runs and gets a run of its own. A payload without one is refused.
# The rest it reads with gh (GH_TOKEN, GITHUB_REPOSITORY). The title, body and branch name come
# from the pull request route as the pull request has them now, not from the payload: ci.yaml's
# lint job checks at a push and can finish long after a later edit's pr-title.yaml run, and both
# post pr-title on the head, so a check of the push's own text would post a pass over the edit's
# refusal. The commits come from the compare route between the event's base and head, which pages
# through every commit and reports their total, so a list that comes back shorter than that total
# is refused loudly here instead of silently scanning part of it; and the issue comments, reviews
# and review comments from their list routes, which report no total, so each is read 100 at a time
# until a page comes back short. There is no total to cross-check those lists against, and none is
# needed: a list that changes while a run reads it is read again by the next push, edit or review
# run and by ci.yaml's lint job's next run (ci.yaml says when it runs), and those runs' checks are
# the ones the merge reads. A NUL byte in any of them, which would make `grep -I` treat the file as
# binary and skip it, is stripped first.
#
# Usage: GITHUB_EVENT_PATH=<payload> GITHUB_REPOSITORY=<owner>/<repo> .github/scripts/check-pr-text.sh
# CI runs it in ci.yaml's lint job at every push to a pull request that is not a draft and when a
# draft is marked ready (lint posts the verdict as pr-title), and in pr-title.yaml when a pull
# request is opened, reopened or edited, when a review on it is submitted or edited, and when the
# merge queue runs its group; its tests are in check-private-names.test.sh.
set -euo pipefail

event=${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH must name the pull request event payload}
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
script_dir=$(cd "$(dirname "$0")" && pwd)
text=$(mktemp -d)
work=$(mktemp -d)
trap 'rm -rf "$text" "$work"' EXIT

pull="$work/pull.json"
if ! jq -e '.pull_request' "$event" > "$pull"; then
  echo "::error::the event payload carries no pull request" >&2
  exit 1
fi
number=$(jq -er '.number' "$pull")
base=$(jq -er '.base.sha' "$pull")
head=$(jq -er '.head.sha' "$pull")
live="$work/live.json"
gh api "repos/$repository/pulls/$number" > "$live"
if ! jq -e '(.title | type == "string") and (.head.ref | type == "string")' "$live" > /dev/null 2>&1; then
  echo "::error::the pull request route answered no title and head branch for #$number" >&2
  exit 1
fi
jq -r '.title' "$live" | tr -d '\000' > "$text/title"
jq -r '.body // ""' "$live" | tr -d '\000' > "$text/body"
jq -r '.head.ref' "$live" | tr -d '\000' > "$text/branch"
# own_status is this script's own refusals: a title that is no conventional commit subject, a title
# or a head commit message that stops GitHub's workflows. The guard's own exit is $status below.
own_status=0
if ! grep -qE '^[a-z]+(\([^)]+\))?!?: .+' "$text/title"; then
  echo "::error::PR title must be a conventional commit subject (type(scope): summary). Squash merges use the PR title as the commit subject, and the release workflows version from conventional subjects — this repository's squash merges write an empty body, so the title is the only text they read."
  own_status=1
fi

# The five strings GitHub stops a push's and a pull request's workflows for, in any case. grep
# answers nothing and exits 1 for a text that holds none, so each read below ends in `|| true`:
# under `set -e` an unguarded failure would end the run.
skip_markers='\[(skip ci|ci skip|no ci|skip actions|actions skip)\]'
title_marker=$(grep -oiE "$skip_markers" "$text/title" | head -n 1 || true)
if [ -n "$title_marker" ]; then
  echo "::error::PR title holds $title_marker, which stops GitHub's workflows. This repository's squash merges use the title as main's commit subject, so that text would stop main's own runs after the merge. Edit the title to one without it."
  own_status=1
fi

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

# skip_checks_trailer <file>: prints the `skip-checks` trailer GitHub reads in that commit message,
# which is a line of the message's last block that is exactly `skip-checks:true` or
# `skip-checks: true` (any case, trailing whitespace allowed) when two or more empty lines come
# before that block. The last block is the run of lines that are not empty holding the message's
# last such line; a line of nothing but whitespace counts as empty. Prints nothing, and exits 0,
# for a message GitHub starts the runs for.
skip_checks_trailer() {
  local -a lines
  local last start i
  mapfile -t lines < "$1"
  last=-1
  for ((i = ${#lines[@]} - 1; i >= 0; i--)); do
    if [ -n "${lines[i]//[[:space:]]/}" ]; then last=$i; break; fi
  done
  if [ "$last" -lt 0 ]; then return 0; fi
  start=$last
  while [ "$start" -gt 0 ] && [ -n "${lines[start - 1]//[[:space:]]/}" ]; do start=$((start - 1)); done
  # start is the block's first line, so the line before it is empty already; the one before that
  # must be empty too, which is GitHub's "preceded by two empty lines".
  if [ "$start" -lt 2 ] || [ -n "${lines[start - 2]//[[:space:]]/}" ]; then return 0; fi
  printf '%s\n' "${lines[@]:start:last - start + 1}" \
    | grep -m 1 -iE '^skip-checks: ?true[[:space:]]*$' || true
}

# The head commit alone: GitHub reads the skip instruction of the commit the event names as the
# head and of no other commit in the range. The entries are counted apart from the message read, so
# a range that lists no commit at that sha is told apart from one whose head commit's message is
# empty.
head_message="$work/head-message"
jq -r --arg head "$head" '.commits[] | select(.sha == $head) | .commit.message' "$compare_json" \
  | tr -d '\000' > "$head_message"
head_entries=$(jq -s --arg head "$head" '[.[].commits[] | select(.sha == $head)] | length' "$compare_json")
if [ "$head_entries" -eq 0 ]; then
  echo "the compare range lists no commit at $head, so no head commit message is read for a text that stops GitHub's workflows"
else
  head_marker=$(grep -oiE "$skip_markers" "$head_message" | head -n 1 || true)
  if [ -z "$head_marker" ]; then head_marker=$(skip_checks_trailer "$head_message"); fi
  if [ -n "$head_marker" ]; then
    echo "::error::the head commit $head holds $head_marker, which stops GitHub's workflows, so no run of this pull request reports. Push one more commit whose message holds no such text: it becomes the head, and no commit has to be rewritten."
    own_status=1
  fi
fi

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
  echo "    Then re-run the newest failed run of each check that reported it on the current head:"
  echo "    lint, which also posts the push's pr-title, and pr-title.yaml's run once for each other"
  echo "    event that is red (\`gh run rerun <run-id> --failed\`)."
  echo "    Never re-run an older run: it checks the commits of the head its own event named, and"
  echo "    its check lands on that head, not the current one."
fi
if [ "$status" -eq 0 ] && [ "$own_status" -ne 0 ]; then
  exit 1
fi
exit "$status"
