#!/usr/bin/env bash
# Tests for `find-pr-payload.sh`: the payload file ci.yaml's `pr-title` job hands to
# check-pr-text.sh, which is the event's own payload at every event that carries a pull request
# and, on a merge group, the queued pull request read from the queue branch the group runs on.
#
# Each case writes the payload Actions would write and runs the script with a stand-in `gh` first
# on PATH: it answers the pull request route with a fixture and records every route it was asked
# for, so a case can show that a refused head_ref reads nothing. The script under test is the file
# CI runs, and the fixture carries the real shapes: a `gh-readonly-queue/<base>/pr-<n>-<40 hex>`
# branch, and a pull request whose head is the pull request's own, not the group's commit.
#
# Run from anywhere: .github/scripts/find-pr-payload.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/temp"

# The head of the queued pull request, and the group's own commit, which the script never reads:
# the queue builds the group on top of the base, so the two differ.
pull_head=365e2766d4bd1f3a2e9c0b7a84f15d6c3e9a2b10
group_head=114285918850b1d4c6e8f0a2b4c6d8e0f2a4b6c8

cat > "$work/pull-request.json" <<JSON
{"number": 2847, "title": "fix(ci): post pr-title from lint on a merge group",
 "head": {"sha": "$pull_head"}, "base": {"sha": "7f3b9c2d5e8a1b4c6d9e0f2a3b5c7d8e9f0a1b2c"}}
JSON

cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
set -euo pipefail
echo "\$*" >> "$work/gh-calls"
case "\$*" in
  "api repos/o/r/pulls/2847") cat "$work/pull-request.json" ;;
  *) echo "unexpected gh \$*" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"

# merge_group_event <head_ref>: the payload Actions writes for a queued group.
merge_group_event() {
  jq -n --arg ref "$1" --arg sha "$group_head" '{merge_group: {head_ref: $ref, head_sha: $sha}}' \
    > "$work/event.json"
}

# find_payload <event name>: runs the script over $work/event.json; sets out (stdout, the path),
# err (stderr) and status. Every case runs it under `timeout`, so a lookup that hung would fail
# its case instead of hanging the job.
find_payload() {
  rm -f "$work/gh-calls" "$work/temp/pull-request.json" "$work/temp/pull-request-event.json"
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r RUNNER_TEMP="$work/temp" \
    GITHUB_EVENT_NAME="$1" GITHUB_EVENT_PATH="$work/event.json" \
    timeout 60 "$script_dir/find-pr-payload.sh" 2> "$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
}

gh_calls() { cat "$work/gh-calls" 2> /dev/null || true; }

echo "case: a pull_request event carries the pull request, so its own payload comes back"
jq -n '{action: "opened", number: 2847, pull_request: {number: 2847}}' > "$work/event.json"
find_payload pull_request
check "it answers" "$(is "$status" 0)"
check "the path is the event payload Actions wrote" "$(is "$out" "$work/event.json")"
check "it reads no route" "$(is "$(gh_calls)" "")"
check "it says nothing else" "$(is "$err" "")"

echo "case: a well-formed queue ref gives the pull request it names, in check-pr-text.sh's shape"
merge_group_event "refs/heads/gh-readonly-queue/main/pr-2847-$pull_head"
find_payload merge_group
check "it answers" "$(is "$status" 0)"
check "the path is the payload it wrote under RUNNER_TEMP" \
  "$(is "$out" "$work/temp/pull-request-event.json")"
check "it read the pull request the branch names" "$(is "$(gh_calls)" "api repos/o/r/pulls/2847")"
check "the file holds the pull request" \
  "$(is "$(jq -r '.pull_request.head.sha' "$work/temp/pull-request-event.json")" "$pull_head")"
check "and its number, as a number" \
  "$(is "$(jq -r '.number | tostring + " " + type' "$work/temp/pull-request-event.json")" "2847 number")"
check "the number is the branch's, not the group's commit" \
  "$(is "$(contains "$(cat "$work/temp/pull-request-event.json")" "$group_head")" false)"
# GitHub has sent merge_group.head_ref both with and without the refs/heads/ prefix.
merge_group_event "gh-readonly-queue/main/pr-2847-$pull_head"
find_payload merge_group
check "a branch name without the refs/heads/ prefix is the same ref" "$(is "$status" 0)"
check "it reads the same pull request" "$(is "$(gh_calls)" "api repos/o/r/pulls/2847")"

echo "case: a ref that names no pull request is refused and named"
merge_group_event refs/heads/main
find_payload merge_group
check "it refuses" "$(is "$status" 1)"
check "it names the ref and the shape it wanted" \
  "$(contains "$err" "^::error::merge_group.head_ref 'refs/heads/main' names no queued pull request (want refs/heads/gh-readonly-queue/<base>/pr-<number>-<40 hex>)\$")"
check "it reads no route" "$(is "$(gh_calls)" "")"
check "it writes no payload" "$(is "$(test -e "$work/temp/pull-request-event.json" && echo yes || echo no)" no)"

echo "case: a ref whose sha is not 40 hex characters is refused, not trimmed to a number"
short=${pull_head:0:39}
merge_group_event "refs/heads/gh-readonly-queue/main/pr-2847-$short"
find_payload merge_group
check "it refuses" "$(is "$status" 1)"
check "it names the ref" \
  "$(contains "$err" "^::error::merge_group.head_ref 'refs/heads/gh-readonly-queue/main/pr-2847-$short' names no queued pull request ")"
check "it reads no route" "$(is "$(gh_calls)" "")"

summary "the pull request payload the pr-title job checks a pull request's text against"
