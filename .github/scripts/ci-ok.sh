#!/usr/bin/env bash
# ci-ok is one of the two checks main's ruleset requires of ci.yaml's router, and this script is
# its verdict. ci.yaml's `ci-ok` job runs it over `toJSON(needs)`, needing every job of the file
# but itself and those CI_OK_EXEMPT names, and fails when this script does, so the job's own
# check run carries the verdict — and a job's check run is in its run's own check suite, where a
# check run POSTed with POST /repos/.../check-runs joins the first suite at that commit, which is
# another workflow's as often as not (post-check-run.sh's header holds the measurement). It
# passes only when `changes` succeeded (it selected the jobs from this head's diff), every job
# ALWAYS_RUN names succeeded (each of those runs on every change), every job `changes` selected
# succeeded, and every job it did not select succeeded or was skipped.
#
# A selected job must succeed: skipped, cancelled or absent fails, and the error says which of
# the three it was, because they are not one fault. A skip means the job's `if:` did not admit
# the run although `changes` chose it — the two disagree, and whoever meets the failure has one
# of them to fix — and a verdict that accepted it would pass with nothing tested. A cancellation
# is the job cut off instead: its own `timeout-minutes` (a job its limit cut off reports
# `cancelled`, measured on probe run 41869007039), a cancelled run, or a force-cancel, none of
# them the filter's doing, so that error sends the author to the job's log and its limit rather
# than to its `if:`.
#
# A re-run of only the failed jobs leaves no such skip: `gh run rerun <id> --failed` was
# measured twice on pull request #2734, and each re-run kept 30 of the run's 31 jobs, ran only
# the failed job again, and ci-ok passed both times. A merge_group run of ci.yaml is never
# re-run at all: every job of a re-run attempt of a merge-group run but the `ci-ok` and
# `pr-title` jobs skips, each held back by a term of its own `if:` (the comment above ci.yaml's
# `changes` job says which), deliberately, because a group is tested once, and a pull request
# the queue removed is tested again when it is added again. Those two jobs run on such an
# attempt, because each is named for a required check and a skipped job's check run is a passing
# required check; a step of each fails the attempt before this script judges anything, so the
# run of skips it leaves never reaches this verdict.
#
# Its inputs, from the ci-ok job's step and from ci-ok-report.sh:
#   NEEDS              `changes`' result and outputs, as `toJSON(needs.changes)` gives them, and
#                      every other job's result, in the shape `toJSON(needs)` would give it.
#   EVENT              `github.event_name`.
#   SELECTED_BY        a JSON object mapping every job but those ALWAYS_RUN names to the
#                      `changes` output its `if:` tests; the job is selected when that output is
#                      'true' (ci-ok.jq). check-ci-filters.sh holds it to ci.yaml.
#   PULL_REQUEST_ONLY  a JSON array of the SELECTED_BY jobs whose `if:` also admits only
#                      pull_request: one is selected only when EVENT is pull_request too.
#                      check-ci-filters.sh holds it to ci.yaml. (A job an event must hold back
#                      otherwise needs no list: `changes` sets its output false there, as it does
#                      for the nine slow suites on a pull request without the label full-ci.)
#   ALWAYS_RUN         a JSON array of the jobs no `changes` output gates, `changes` among them:
#                      each runs on every change, so ci-ok passes only when it succeeded, and a
#                      skip, a cancellation or an absence fails. ci.yaml's env names them in
#                      CI_OK_ALWAYS_RUN, and check-ci-filters.sh holds every job of that file to
#                      exactly one of the three classes this list, SELECTED_BY and CI_OK_EXEMPT
#                      make.
# A job of NEEDS that neither ALWAYS_RUN nor SELECTED_BY names fails (ci-ok does not know what to
# make of its result), and so does a job either of them names that is absent from NEEDS. It
# prints every job's result and whether it was selected, and an `::error::` for each failure
# naming the job, its result and why.
#
# Run from anywhere:
#   NEEDS=... EVENT=... SELECTED_BY=... PULL_REQUEST_ONLY=... ALWAYS_RUN=... .github/scripts/ci-ok.sh
# ci.yaml's ci-ok job runs it over every job's result, and ci-ok-report.sh over the results it
# reads for the failing ci-ok it posts at once on a decided merge-group run; its tests are in
# ci-ok.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
for input in NEEDS EVENT SELECTED_BY PULL_REQUEST_ONLY ALWAYS_RUN; do
  if [ -z "${!input:-}" ]; then
    echo "::error::ci-ok.sh needs $input in its environment"
    exit 1
  fi
done
if ! jq -e 'type == "object"' <<<"$NEEDS" >/dev/null 2>&1; then
  echo "::error::NEEDS is not a JSON object: $NEEDS"
  exit 1
fi
if ! jq -e 'type == "object" and all(.[]; type == "string")' <<<"$SELECTED_BY" >/dev/null 2>&1; then
  echo "::error::SELECTED_BY is not a JSON object of job to output name: $SELECTED_BY"
  exit 1
fi
if ! jq -e --argjson map "$SELECTED_BY" 'type == "array" and all(.[]; type == "string" and (. as $job | $map | has($job)))' \
  <<<"$PULL_REQUEST_ONLY" >/dev/null 2>&1; then
  echo "::error::PULL_REQUEST_ONLY is not a JSON array of jobs SELECTED_BY names: $PULL_REQUEST_ONLY"
  exit 1
fi
if ! jq -e 'type == "array" and all(.[]; type == "string") and any(. == "changes")' \
  <<<"$ALWAYS_RUN" >/dev/null 2>&1; then
  echo "::error::ALWAYS_RUN is not a JSON array of job names naming changes, whose outputs every other job is judged by: $ALWAYS_RUN"
  exit 1
fi

# One row per line, tab-separated: `info` and the line to print, or `error` and the ::error:: text.
# Every job is printed in NEEDS order, after each ALWAYS_RUN job NEEDS lacks and before each
# SELECTED_BY job it lacks. `cut_off` is why a selected job that did not succeed failed: the
# three results it can carry have three causes, and only the skip is the filter's.
read -r -d '' program <<'JQ' || true
include "ci-ok";
(.changes.outputs // {}) as $out
| def selection($job): selection($job; $map; $out; $pr; $event);
  def cut_off($result):
    if $result == "skipped" then
      "a skip here means the job's if: did not admit the run although changes chose it, a disagreement between the two the author must fix"
    elif $result == "cancelled" then
      "a cancellation here is the job cut off rather than filtered out — its own timeout-minutes, a cancelled run or a force-cancel — so the job's log and its timeout-minutes say which, not its filter"
    elif $result == "failure" then
      "this one failed: its log says what broke"
    else
      "a \($result) is no conclusion at all, so this verdict was reached before the job ended"
    end;
  def rows($job; $result):
    if $job == "changes" then
      ["info", "changes: \($result)"],
      (select($result != "success")
       | ["error", "changes ended \($result); it selects the jobs from this head's diff, so ci-ok passes only when it succeeded"])
    elif ($always | any(. == $job)) then
      ["info", "\($job): \($result)"],
      (select($result != "success")
       | ["error", "\($job) ended \($result); \($job) runs on every change, so ci-ok passes only when it succeeded"])
    elif ($map | has($job)) then
      selection($job) as $s
      | ["info", "\($job): \($result) (\(if $s.selected then "selected" else "not selected" end): \($s.why))"],
        if $result == "absent" then
          ["error", "\($job) is absent from NEEDS, but SELECTED_BY maps it to needs.changes.outputs.\($map[$job]); the ci-ok job's toJSON(needs) and ci-ok-report.sh both give every SELECTED_BY job a result, so this is a defect of whichever of them built NEEDS"]
        elif $s.selected and $result != "success" then
          ["error", "\($job) ended \($result), but changes selected it (\($s.why)); a selected job must succeed, and \(cut_off($result))"]
        elif ($s.selected | not) and $result != "success" and $result != "skipped" then
          ["error", "\($job) ended \($result); a job changes did not select must succeed or be skipped"]
        else empty end
    else
      ["info", "\($job): \($result) (in none of ci-ok's three classes)"],
      ["error", "\($job) ended \($result), and ci-ok does not know what selects it; map it in CI_OK_SELECTED_BY in ci.yaml's env when a changes output gates it, name it in CI_OK_ALWAYS_RUN when it runs on every change, or name it in CI_OK_EXEMPT when it is outside ci-ok's accounting"]
    end;
  . as $needs
  | ($always[] | select(. as $job | $needs | has($job) | not) | rows(.; "absent")),
    ($needs | to_entries[] | rows(.key; .value.result // "absent")),
    ($map | keys_unsorted[] | select(. as $job | $needs | has($job) | not) | rows(.; "absent"))
| @tsv
JQ
rows=$(jq -L "$script_dir" -r --arg event "$EVENT" --argjson map "$SELECTED_BY" --argjson pr "$PULL_REQUEST_ONLY" \
  --argjson always "$ALWAYS_RUN" \
  "$program" <<<"$NEEDS")

errors=()
while IFS=$'\t' read -r level text; do
  case $level in
    info) echo "$text" ;;
    error) errors+=("$text") ;;
  esac
done <<<"$rows"

if [ "${#errors[@]}" -gt 0 ]; then
  for error in "${errors[@]}"; do
    echo "::error::$error"
  done
  exit 1
fi
# The pass line names the jobs it held to success, as the errors above name theirs: ALWAYS_RUN in
# the file's order, "a, b and c".
always_run_text=$(jq -r 'if length > 1 then (.[:-1] | join(", ")) + " and " + .[-1] else .[0] end' <<<"$ALWAYS_RUN")
echo "$always_run_text succeeded, every job changes selected succeeded, and every other job succeeded or was skipped"
