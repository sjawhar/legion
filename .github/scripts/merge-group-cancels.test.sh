#!/usr/bin/env bash
# Tests for `merge-group-cancels.sh`: which of ci.yaml's merge-group runs the cancel act of a run
# that has just started must cancel, which is every group of its own pull request the queue has
# replaced.
#
# merge-group-cancels.sh reads only its environment, so each case hands it the RUNS `gh run list`
# would give for a queue of its own. The fixtures carry the real shapes:
# `gh-readonly-queue/main/pr-<n>-<parent sha>` branches and gh's RFC3339 times. The script under
# test is the file CI runs, and every case runs it under `timeout`, so a decision that hung would
# fail its case instead of hanging the job.
#
# Run from anywhere: .github/scripts/merge-group-cancels.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# main's tip and an older commit of main: the parent sha of a group the queue built on main.
main_tip=04cec161128c5a1cdada47ba40626f02b1fdbe38
main_older=7f3b9c2d5e8a1b4c6d9e0f2a3b5c7d8e9f0a1b2c

# queue_branch <pull request> <parent sha>: the head branch of that group's run.
queue_branch() { echo "gh-readonly-queue/main/pr-$1-$2"; }

# mg_run <id> <pull request> <parent sha> <status> <createdAt> [<runStartedAt>]: one run of
# `gh run list --workflow ci.yaml --event merge_group`. runStartedAt is a field gh also carries
# and this script never reads; a case passes it to show a run that started late.
mg_run() {
  jq -n --argjson id "$1" --arg branch "$(queue_branch "$2" "$3")" \
    --arg status "$4" --arg created "$5" --arg started "${6:-}" \
    '{
       databaseId: $id,
       headBranch: $branch,
       status: $status,
       createdAt: $created,
     }
     + (if $started == "" then {} else {runStartedAt: $started} end)'
}

# runs <run>...: the RUNS array, in the order given (the script must not depend on it).
runs() { printf '%s\n' "$@" | jq -s '.'; }

# run_cancels <run id> <head branch> <runs>: sets out (stdout, the cancels), err (stderr, the
# reasoning) and status.
run_cancels() {
  set +e
  out=$(RUN_ID=$1 HEAD_BRANCH=$2 RUNS=$3 timeout 60 "$script_dir/merge-group-cancels.sh" 2>"$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
}

echo "case: an older group of the same pull request is cancelled by the newer group's start"
# The older group's run was created first and started last: the queue rebuilt pull request 2700
# above a group that had changed, and the older group's runners were handed out late.
rebuilt=$(runs \
  "$(mg_run 41871000100 2700 "$main_older" in_progress 2026-11-04T19:00:00Z 2026-11-04T19:06:30Z)" \
  "$(mg_run 41871000200 2700 "$main_tip" in_progress 2026-11-04T19:05:00Z 2026-11-04T19:05:10Z)" \
  "$(mg_run 41871000300 2701 "$main_older" in_progress 2026-11-04T18:50:00Z)")
run_cancels 41871000200 "$(queue_branch 2700 "$main_tip")" "$rebuilt"
check "decides" "$(is "$status" 0)"
check "cancels the older group of this pull request and nothing else" \
  "$(is "$out" 'cancel 41871000100')"
check "orders by createdAt, although that run started after this one" \
  "$(contains "$err" "^run 41871000100 (gh-readonly-queue/main/pr-2700-$main_older) is a group of pull request 2700 created at 2026-11-04T19:00:00Z, before this run's 2026-11-04T19:05:00Z, and is in_progress")"
check "leaves another pull request's older group alone" "$(is "$(contains "$out" '41871000300')" false)"

echo "case: a bad input is named and nothing is decided"
run_cancels 41860898444 refs/heads/main "$rebuilt"
check "a head branch that is no queue branch fails" "$(is "$status" 1)"
check "names it" "$(contains "$err" '^::error::HEAD_BRANCH is not a gh-readonly-queue/<base>/pr-<n>-<parent sha> branch: refs/heads/main$')"
run_cancels 41860898444 "$(queue_branch 2589 "$main_tip")" '[{"databaseId": 1}]'
check "a RUNS entry without the fields gh gives fails" "$(is "$status" 1)"
check "names it" "$(contains "$err" '^::error::RUNS is not a JSON array of runs with databaseId')"

echo "case: the act cancels nothing when the list does not hold the asking run yet"
run_cancels 41999999999 "$(queue_branch 2700 "$main_tip")" "$rebuilt"
check "it decides rather than failing" "$(is "$status" 0)"
check "it cancels nothing" "$(is "$out" "")"
check "it says why, and what reclaims those groups instead" \
  "$(contains "$err" "^the run list does not hold the asking run 41999999999 yet, so no group of pull request 2700 can be shown to be older than this one and none is cancelled: the next merge-group run of this pull request cancels them when it starts\$")"

echo "case: the stand-in must not cancel a newer group of the same pull request"
# `gh run list` can already hold a group created after the asking run while it still does not
# hold the asking run itself: the same index lag that leaves the asking run missing. Dating the
# asking run `now`, as a stand-in entry in RUNS once did, then makes the newer group look older,
# and the act cancels the group that will land.
lagging=$(runs "$(mg_run 41874000200 2700 "$main_older" in_progress 2026-11-04T19:05:10Z)")
run_cancels 41874000100 "$(queue_branch 2700 "$main_older")" "$lagging"
check "it decides rather than failing" "$(is "$status" 0)"
check "it cancels no group it cannot prove is older than itself" "$(is "$out" "")"

summary "the merge-group cancel decision"
