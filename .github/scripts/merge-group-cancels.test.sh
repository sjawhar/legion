#!/usr/bin/env bash
# Tests for `merge-group-cancels.sh`: which of ci.yaml's merge-group runs each cancel act must
# cancel, and whether the asking run must end itself.
#
# merge-group-cancels.sh reads only its environment, so each case hands it the RUNS `gh run list`
# would give for a queue of its own and the CI_OK map of those groups' heads. The fixtures carry
# the real shapes: 40-character shas, `gh-readonly-queue/main/pr-<n>-<parent sha>` branches, and
# gh's RFC3339 times. The script under test is the file CI runs, and every case runs it under
# `timeout`, so a chain walk that looped would fail its case instead of hanging the job.
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

# main's tip and an older commit of main: the head of no merge-group run, where a walk stops.
main_tip=04cec161128c5a1cdada47ba40626f02b1fdbe38
main_older=7f3b9c2d5e8a1b4c6d9e0f2a3b5c7d8e9f0a1b2c
# The three stacked groups measured on this repository: pr-2589 on main, pr-2720 on it, pr-2734 on
# that one.
head_2589=feadc17dfa5a6898bbb8e237f12c630663469b05
head_2720=2b801fb42ce6576cee41d366e05ec3e1ac3acaf7
head_2734=9c1f0b6e2a4d8f7c3b5a1e9d0c4f8a2b6d3e7f10

# queue_branch <pull request> <parent sha>: the head branch of that group's run.
queue_branch() { echo "gh-readonly-queue/main/pr-$1-$2"; }

# mg_run <id> <pull request> <parent sha> <head sha> <status> <conclusion> <createdAt>
#   [<runStartedAt>]: one run of `gh run list --workflow ci.yaml --event merge_group`. An empty
# conclusion is null, as gh gives it for a run that has not completed. runStartedAt is a field gh
# also carries and this script never reads; a case passes it to show a run that started late.
mg_run() {
  jq -n --argjson id "$1" --arg branch "$(queue_branch "$2" "$3")" --arg head "$4" \
    --arg status "$5" --arg conclusion "$6" --arg created "$7" --arg started "${8:-}" \
    '{
       databaseId: $id,
       headSha: $head,
       headBranch: $branch,
       status: $status,
       conclusion: (if $conclusion == "" then null else $conclusion end),
       createdAt: $created,
     }
     + (if $started == "" then {} else {runStartedAt: $started} end)'
}

# runs <run>...: the RUNS array, in the order given (the script must not depend on it).
runs() { printf '%s\n' "$@" | jq -s '.'; }

# run_cancels <act> <run id> <head sha> <head branch> <runs> [<ci-ok map>] [<report>]: sets out
# (stdout, the cancels), err (stderr, the reasoning) and status. The report is the failed act's
# only reader; it defaults to `failure`, the answer a job whose own work failed gives.
run_cancels() {
  set +e
  out=$(ACT=$1 RUN_ID=$2 HEAD_SHA=$3 HEAD_BRANCH=$4 RUNS=$5 CI_OK=${6:-} REPORT=${7-failure} \
    timeout 60 "$script_dir/merge-group-cancels.sh" 2>"$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
}

echo "case: a chain with a failed link: every descendant is cancelled, in chain order, this run last"
stack=$(runs \
  "$(mg_run 41865489886 2734 "$head_2720" "$head_2734" queued "" 2026-11-04T18:09:44Z)" \
  "$(mg_run 41860898444 2589 "$main_tip" "$head_2589" in_progress "" 2026-11-04T17:58:02Z)" \
  "$(mg_run 41861688656 2720 "$head_2589" "$head_2720" in_progress "" 2026-11-04T18:03:17Z)" \
  "$(mg_run 41870000001 2800 "$main_older" 92a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5 in_progress "" 2026-11-04T18:04:00Z)")
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$stack"
check "decides" "$(is "$status" 0)"
check "cancels the nearest descendant, then the one above it, and ends with this run" \
  "$(is "$out" "$(printf 'cancel 41861688656\ncancel 41865489886\ncancel 41860898444')")"
check "says why the group one step up is cancelled" \
  "$(contains "$err" "^run 41861688656 (gh-readonly-queue/main/pr-2720-$head_2589) is in_progress and its chain of parents reaches $head_2589 after 1 step(s)")"
check "says why the group two steps up is cancelled" \
  "$(contains "$err" "^run 41865489886 (gh-readonly-queue/main/pr-2734-$head_2720) is queued and its chain of parents reaches $head_2589 after 2 step(s)")"
check "says this run is cancelled last and why" \
  "$(contains "$err" "^run 41860898444's own remaining jobs are force-cancelled last, after the descendants it had to cancel first\$")"
check "leaves a group of another chain alone" "$(is "$(contains "$out" '41870000001')" false)"

echo "case: a chain without a failed link: a started run whose ancestors all pass is not doomed"
ci_ok_green=$(jq -n --arg a "$head_2589" --arg b "$head_2720" --arg m "$main_tip" \
  '{($a): "success", ($b): "success", ($m): "success"}')
run_cancels started 41865489886 "$head_2734" "$(queue_branch 2734 "$head_2720")" "$stack" "$ci_ok_green"
check "decides" "$(is "$status" 0)"
check "prints no token at all" "$(is "$out" "")"
check "walked the group one step up" \
  "$(contains "$err" "^this run is built on $head_2720, the head of run 41861688656 (gh-readonly-queue/main/pr-2720-$head_2589), whose ci-ok is success\$")"
check "walked the group two steps up" \
  "$(contains "$err" "^this run is built on $head_2589, the head of run 41860898444 (gh-readonly-queue/main/pr-2589-$main_tip), whose ci-ok is success\$")"
check "stopped at main's tip" \
  "$(contains "$err" "^the walk stopped at $main_tip: the head of no merge-group run")"
check "says it is not doomed" \
  "$(contains "$err" '^no group in this run'"'"'s chain of parents carries a failing ci-ok (2 group(s) walked): this run is not doomed$')"

echo "case: an older group of the same pull request is cancelled by the newer group's start"
pr2700_old=6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192
pr2700_new=5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7081
pr2701_head=708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3
# The older group's run was created first and started last: the queue rebuilt pull request 2700
# above a group that had changed, and the older group's runners were handed out late.
rebuilt=$(runs \
  "$(mg_run 41871000100 2700 "$main_older" "$pr2700_old" in_progress "" 2026-11-04T19:00:00Z 2026-11-04T19:06:30Z)" \
  "$(mg_run 41871000200 2700 "$main_tip" "$pr2700_new" in_progress "" 2026-11-04T19:05:00Z 2026-11-04T19:05:10Z)" \
  "$(mg_run 41871000300 2701 "$main_older" "$pr2701_head" in_progress "" 2026-11-04T18:50:00Z)")
run_cancels started 41871000200 "$pr2700_new" "$(queue_branch 2700 "$main_tip")" "$rebuilt" '{}'
check "decides" "$(is "$status" 0)"
check "cancels the older group of this pull request and nothing else" \
  "$(is "$out" 'cancel 41871000100')"
check "orders by createdAt, although that run started after this one" \
  "$(contains "$err" "^run 41871000100 (gh-readonly-queue/main/pr-2700-$main_older) is a group of pull request 2700 created at 2026-11-04T19:00:00Z, before this run's 2026-11-04T19:05:00Z, and is in_progress")"
check "leaves another pull request's older group alone" "$(is "$(contains "$out" '41871000300')" false)"
check "is not doomed, since no ancestor group carries a failing ci-ok" \
  "$(contains "$err" 'this run is not doomed$')"

echo "case: queued and running descendants are cancelled, a completed one is not"
mixed=$(runs \
  "$(mg_run 41860898444 2589 "$main_tip" "$head_2589" in_progress "" 2026-11-04T17:58:02Z)" \
  "$(mg_run 41861688656 2720 "$head_2589" "$head_2720" in_progress "" 2026-11-04T18:03:17Z)" \
  "$(mg_run 41865489886 2734 "$head_2720" "$head_2734" completed success 2026-11-04T18:09:44Z)" \
  "$(mg_run 41869000004 2740 "$head_2734" 8192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4 queued "" 2026-11-04T18:12:00Z)")
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$mixed"
check "decides" "$(is "$status" 0)"
check "cancels the running one and the queued one, and ends with this run" \
  "$(is "$out" "$(printf 'cancel 41861688656\ncancel 41869000004\ncancel 41860898444')")"
check "says the completed descendant is left alone" \
  "$(contains "$err" "^run 41865489886 (gh-readonly-queue/main/pr-2734-$head_2720) is built on $head_2589, 2 step(s) up, but it has completed (success): left alone\$")"

echo "case: a pull request removed from the queue and added again within seconds"
dequeued_head=3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f
requeued_head=4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f70
requeued=$(runs \
  "$(mg_run 41872000100 2750 "$main_tip" "$dequeued_head" in_progress "" 2026-11-04T20:00:00Z)" \
  "$(mg_run 41872000200 2750 "$main_tip" "$requeued_head" in_progress "" 2026-11-04T20:00:04Z)")
run_cancels failed 41872000100 "$dequeued_head" "$(queue_branch 2750 "$main_tip")" "$requeued"
check "decides" "$(is "$status" 0)"
check "cancels only itself: the new group is built on main, not on the old group" \
  "$(is "$out" 'cancel 41872000100')"
check "says no run is built on the failed group's head" \
  "$(contains "$err" "^no other merge-group run has $dequeued_head in its chain of parents\$")"

echo "case: the walk stops at a commit of main, and a failing ci-ok there dooms nothing"
red_main=$(jq -n --arg m "$main_tip" '{($m): "failure"}')
on_main=$(runs "$(mg_run 41860898444 2589 "$main_tip" "$head_2589" in_progress "" 2026-11-04T17:58:02Z)")
run_cancels started 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$on_main" "$red_main"
check "decides" "$(is "$status" 0)"
check "prints no token: nothing to cancel and nothing doomed" "$(is "$out" "")"
check "says the stop commit's failing ci-ok dooms nothing" \
  "$(contains "$err" "^the walk stopped at $main_tip: the head of no merge-group run, so it is a commit of main, and its ci-ok (failure) dooms nothing\$")"
# The control: the same failing ci-ok on a sha that is the head of a merge-group run does doom.
group_2500=a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6
below=$(runs \
  "$(mg_run 41859000500 2500 "$main_tip" "$group_2500" completed failure 2026-11-04T17:50:00Z)" \
  "$(mg_run 41860898444 2589 "$group_2500" "$head_2589" in_progress "" 2026-11-04T17:58:02Z)")
red_group=$(jq -n --arg g "$group_2500" --arg m "$main_tip" '{($g): "failure", ($m): "failure"}')
run_cancels started 41860898444 "$head_2589" "$(queue_branch 2589 "$group_2500")" "$below" "$red_group"
check "the same conclusion on a group's head dooms this run" "$(is "$out" 'cancel 41860898444')"
check "names the group that dooms it" \
  "$(contains "$err" "^this run is doomed: its chain of parents reaches $group_2500, the head of run 41859000500, whose ci-ok is failure")"

echo "case: a cycle in the branch names does not hang the walk"
cycle_a=1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d
cycle_b=2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e
cycle=$(runs \
  "$(mg_run 41873000100 2810 "$cycle_b" "$cycle_a" in_progress "" 2026-11-04T21:00:00Z)" \
  "$(mg_run 41873000200 2811 "$cycle_a" "$cycle_b" in_progress "" 2026-11-04T21:01:00Z)")
run_cancels failed 41873000100 "$cycle_a" "$(queue_branch 2810 "$cycle_b")" "$cycle"
check "the failed act decides rather than hanging" "$(is "$status" 0)"
check "cancels the run the cycle made a descendant, then itself" \
  "$(is "$out" "$(printf 'cancel 41873000200\ncancel 41873000100')")"
run_cancels started 41873000100 "$cycle_a" "$(queue_branch 2810 "$cycle_b")" "$cycle" '{}'
check "the started act decides rather than hanging" "$(is "$status" 0)"
check "says the branch names form a cycle" \
  "$(contains "$err" "^the walk stopped at $cycle_a: it was visited already, so these branch names form a cycle\$")"

echo "case: a bad input is named and nothing is decided"
run_cancels rebased 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$stack"
check "an act that is neither failed nor started fails" "$(is "$status" 1)"
check "names it" "$(contains "$err" "^::error::ACT is the act to decide, failed or started, not 'rebased'\$")"
run_cancels failed 41860898444 "$head_2589" refs/heads/main "$stack"
check "a head branch that is no queue branch fails" "$(is "$status" 1)"
check "names it" "$(contains "$err" '^::error::HEAD_BRANCH is not a gh-readonly-queue/<base>/pr-<n>-<parent sha> branch: refs/heads/main$')"
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" '[{"databaseId": 1}]'
check "a RUNS entry without the fields gh gives fails" "$(is "$status" 1)"
check "names it" "$(contains "$err" '^::error::RUNS is not a JSON array of runs with databaseId')"
run_cancels started 41865489886 "$head_2734" "$(queue_branch 2734 "$head_2720")" "$stack"
check "the started act without CI_OK fails" "$(is "$status" 1)"
check "names it" "$(contains "$err" "^::error::the started act reads each ancestor group's ci-ok, so it needs CI_OK in its environment\$")"
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$stack" '' ''
check "the failed act without REPORT fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$err" "^::error::the failed act reads the reporting job's own report, so it needs REPORT in its environment\$")"
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$stack" '' passed
check "a REPORT that is none of the five answers fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$err" "^::error::REPORT is the reporting job's own report, one of success, failure, cancelled, absent and unknown, not 'passed'\$")"

echo "case: the started act cancels nothing when the list does not hold the asking run yet"
run_cancels started 41999999999 "$head_2734" "$(queue_branch 2734 "$head_2720")" "$stack" '{}'
check "it decides rather than failing" "$(is "$status" 0)"
check "it cancels nothing" "$(is "$out" "")"
check "it says why, and what reclaims those groups instead" \
  "$(contains "$err" "^the run list does not hold the asking run 41999999999 yet, so no group of pull request 2734 can be shown to be older than this one and none is cancelled: the next merge-group run of this pull request cancels them when it starts\$")"
check "it still walks the chain and answers the doomed half" \
  "$(contains "$err" 'this run is not doomed$')"

echo "case: the stand-in must not cancel a newer group of the same pull request"
# `gh run list` can already hold a group created after the asking run while it still does not
# hold the asking run itself: the same index lag that leaves the asking run missing. Dating the
# asking run `now`, as a stand-in entry in RUNS once did, then makes the newer group look older,
# and the act cancels the group that will land.
lagging=$(runs "$(mg_run 41874000200 2700 "$main_older" "$pr2700_new" in_progress "" 2026-11-04T19:05:10Z)")
run_cancels started 41874000100 "$pr2700_old" "$(queue_branch 2700 "$main_older")" "$lagging" '{}'
check "it decides rather than failing" "$(is "$status" 0)"
check "it cancels no group it cannot prove is older than itself" "$(is "$out" "")"

echo "case: every cancel job of ci.yaml can run when the job it needs did not succeed"
# GitHub gives a job `if:` that names no status-check function an implicit `success()`
# (docs.github.com, "Evaluate expressions", Status check functions), false once any job in
# `needs:` failed or was cancelled. A follower whose condition reads the result of the job it
# needs — `contains(needs.*.result, 'failure')` and its `cancelled` twin — with no `always()`
# or `!cancelled()` is therefore skipped in exactly the runs it exists for, so the act below is
# never asked for a decision at all.
# release.yaml's header states the same rule for this repository and names the run it cost
# (38822866387); check-release-credentials.sh rule 5 pins it there and reads no other workflow.
dead_followers=$(yq -r '
  .jobs | to_entries
  | map(select((.value."if" // "") | test("needs\.[A-Za-z0-9_*-]+\.result")))
  | map(select(((.value."if" // "") | test("always\(\)|cancelled\(\)|failure\(\)|success\(\)")) | not))
  | map(.key) | join(" ")' "$script_dir/../workflows/ci.yaml" 2>/dev/null)
check "no job of ci.yaml reads a need's result behind an implicit success()" \
  "$(is "$dead_followers" "")"

echo "case: the failed act finds its descendants when the list does not hold the asking run"
# `gh run list` lags behind a run that has just started, which is why the started act above
# refuses to guess. The failed act walks a candidate's chain of parents through the runs it was
# given, so without the asking run's own entry it reaches no parent, misses its direct child and
# states the absence as proven. HEAD_SHA and HEAD_BRANCH name this run, so the walk needs no
# entry for it. `cancel-changes` is the most exposed caller: `changes` is the run's first job, so
# its follower fires inside that lag window.
lagging_child=$(runs \
  "$(mg_run 41865489886 2734 "$head_2589" "$head_2734" in_progress "" 2026-11-04T18:09:44Z)")
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$lagging_child"
check "it decides rather than failing" "$(is "$status" 0)"
check "it cancels the descendant, then itself" \
  "$(is "$out" "$(printf 'cancel 41865489886\ncancel 41860898444')")"

echo "case: the report of the job that fired the follower decides what the failed act cancels"
# A follower fires on its job's `failure` or `cancelled` result, which a job whose tests all
# passed carries too when its ci-ok-report step cannot reach GitHub. The report that step
# uploaded before it posted anything tells the two apart, and .github/actions/merge-group-cancel
# reads it out of the run's artifacts for this script.
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$stack" '' success
check "a report of success cancels nothing at all" "$(is "$out" "")"
check "says the job's work passed and its report step alone failed" \
  "$(contains "$err" "^the job's own report for this run is success: its work passed and its report step alone failed, so this group is no failed group and nothing is cancelled\$")"
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$stack" '' unknown
check "a report GitHub refused cancels nothing either" "$(is "$out" "")"
check "says nothing here shows this group failed" \
  "$(contains "$err" "^the job's own report could not be read, so nothing here shows that this group failed and nothing is cancelled\$")"
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$stack" '' absent
check "a job that uploaded no report, as the changes job never does, cancels the descendants" \
  "$(is "$out" "$(printf 'cancel 41861688656\ncancel 41865489886')")"
check "and never this run, which stays alive so a job of it can post this group's ci-ok" \
  "$(contains "$err" "^the job uploaded no report, so this group's failing ci-ok is still unposted: run 41860898444 is left running, after the descendants it had to cancel first, so that a job of it can post that ci-ok\$")"
run_cancels failed 41860898444 "$head_2589" "$(queue_branch 2589 "$main_tip")" "$stack" '' cancelled
check "a report of cancelled, which a job its own timeout cut off leaves, ends this run too" \
  "$(is "$out" "$(printf 'cancel 41861688656\ncancel 41865489886\ncancel 41860898444')")"

summary "the merge-group cancel decision"
