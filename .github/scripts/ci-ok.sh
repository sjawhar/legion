#!/usr/bin/env bash
# ci-ok is the one check main's ruleset requires of ci.yaml's router, and this script is its
# verdict. No job of ci.yaml is ci-ok: the last job of a run to finish posts it from its own last
# step (ci-ok-report.sh), so the verdict never waits for a runner of its own. It passes only when
# `changes` succeeded (it selected the jobs from this head's diff), lint succeeded (it runs on
# every change), every job `changes` selected succeeded, and every job it did not select
# succeeded or was skipped.
#
# A selected job must succeed: skipped, cancelled or absent fails. A skip there means the job's
# `if:` did not admit the run although `changes` chose it — the two disagree, and whoever meets
# the failure has one of them to fix — and a verdict that accepted it would pass with nothing
# tested. A re-run of only the failed jobs does not leave one: `gh run rerun <id> --failed` was
# measured twice on pull request #2734, and each re-run kept 30 of the run's 31 jobs, ran only
# the failed job again, and ci-ok passed both times. A merge_group run of ci.yaml is never
# re-run at all: every job of a re-run attempt of a merge-group run skips, each held back by a
# term of its own `if:` (the comment above ci.yaml's `changes` job says which), deliberately,
# because a group is tested once, and a pull request the queue removed is tested again when it
# is added again.
#
# Its inputs, from ci-ok-report.sh:
#   NEEDS              `changes`' result and outputs, as `toJSON(needs.changes)` gives them, and
#                      every other job's result, in the shape `toJSON(needs)` would give it.
#   EVENT              `github.event_name`.
#   SELECTED_BY        a JSON object mapping every job but `changes` and lint to the `changes`
#                      output its `if:` tests; the job is selected when that output is 'true'
#                      (ci-ok.jq). check-ci-filters.sh holds it to ci.yaml.
#   PULL_REQUEST_ONLY  a JSON array of the SELECTED_BY jobs whose `if:` also admits only
#                      pull_request: one is selected only when EVENT is pull_request too.
#                      check-ci-filters.sh holds it to ci.yaml. (A job an event must hold back
#                      otherwise needs no list: `changes` sets its output false there, as it does
#                      for the nine slow suites on a pull request without the label full-ci.)
# A job of NEEDS that SELECTED_BY does not name fails (ci-ok does not know what selects it), and
# so does a SELECTED_BY job that is absent from NEEDS. It prints every job's result and whether it
# was selected, and an `::error::` for each failure naming the job, its result and why.
#
# Run from anywhere:
#   NEEDS=... EVENT=... SELECTED_BY=... PULL_REQUEST_ONLY=... .github/scripts/ci-ok.sh
# ci-ok-report.sh runs it in the last step of every job of ci.yaml but `changes`; its tests are in
# ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
for input in NEEDS EVENT SELECTED_BY PULL_REQUEST_ONLY; do
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

# One row per line, tab-separated: `info` and the line to print, or `error` and the ::error:: text.
# Every job is printed in NEEDS order, after `changes` or lint if NEEDS lacks them and before each
# SELECTED_BY job NEEDS lacks.
read -r -d '' program <<'JQ' || true
include "ci-ok";
(.changes.outputs // {}) as $out
| def selection($job): selection($job; $map; $out; $pr; $event);
  def rows($job; $result):
    if $job == "changes" then
      ["info", "changes: \($result)"],
      (select($result != "success")
       | ["error", "changes ended \($result); it selects the jobs from this head's diff, so ci-ok passes only when it succeeded"])
    elif $job == "lint" then
      ["info", "lint: \($result)"],
      (select($result != "success")
       | ["error", "lint ended \($result); lint runs on every change, so ci-ok passes only when it succeeded"])
    elif ($map | has($job)) then
      selection($job) as $s
      | ["info", "\($job): \($result) (\(if $s.selected then "selected" else "not selected" end): \($s.why))"],
        if $result == "absent" then
          ["error", "\($job) is absent from NEEDS, but SELECTED_BY maps it to needs.changes.outputs.\($map[$job]); ci-ok-report.sh gives every SELECTED_BY job a result, so this is its defect"]
        elif $s.selected and $result != "success" then
          ["error", "\($job) ended \($result), but changes selected it (\($s.why)); a selected job must succeed, and a skip here means the job's if: did not admit the run although changes chose it, a disagreement between the two the author must fix"]
        elif ($s.selected | not) and $result != "success" and $result != "skipped" then
          ["error", "\($job) ended \($result); a job changes did not select must succeed or be skipped"]
        else empty end
    else
      ["info", "\($job): \($result) (not in SELECTED_BY)"],
      ["error", "\($job) ended \($result), and ci-ok does not know what selects it; add it to CI_OK_SELECTED_BY in ci.yaml's env"]
    end;
  . as $needs
  | (("changes", "lint") | select(. as $job | $needs | has($job) | not) | rows(.; "absent")),
    ($needs | to_entries[] | rows(.key; .value.result // "absent")),
    ($map | keys_unsorted[] | select(. as $job | $needs | has($job) | not) | rows(.; "absent"))
| @tsv
JQ
rows=$(jq -L "$script_dir" -r --arg event "$EVENT" --argjson map "$SELECTED_BY" --argjson pr "$PULL_REQUEST_ONLY" \
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
echo "changes and lint succeeded, every job changes selected succeeded, and every other job succeeded or was skipped"
