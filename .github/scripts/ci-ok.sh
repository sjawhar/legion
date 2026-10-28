#!/usr/bin/env bash
# ci.yaml's last job, ci-ok, is the one check main's ruleset requires of the router, and this
# script is its verdict. It passes only when `changes` succeeded (it selected the jobs from this
# head's diff), lint succeeded (it runs on every change), every job `changes` selected succeeded,
# and every job it did not select succeeded or was skipped.
#
# A selected job must succeed: skipped, cancelled or absent fails. A skipped job reports success,
# and GitHub's "Re-run failed jobs" can leave one skipped while its selector now succeeds: when
# `changes` fails, every job its filters gate is skipped, and a failed-only re-run makes `changes`
# succeed while the jobs it selects stay skipped. ci-ok re-runs too and would pass with nothing
# tested if it accepted a skip from a job `changes` selected.
#
# Its inputs, from the ci-ok step's env:
#   NEEDS              `${{ toJSON(needs) }}`: every needed job's result, and `changes`' outputs.
#   DRAFT              true on a draft pull request, where no test job runs: ci-ok fails saying so.
#   EVENT              `github.event_name`.
#   SELECTED_BY        a JSON object mapping every needed job but `changes` and lint to the
#                      `changes` output its `if:` tests; the job is selected when that output is
#                      'true'. check-ci-filters.sh holds it to ci.yaml.
#   PULL_REQUEST_ONLY  a JSON array of the SELECTED_BY jobs whose `if:` also admits only
#                      pull_request: one is selected only when EVENT is pull_request too.
#   NOT_ON_PUSH        a JSON array of the SELECTED_BY jobs whose `if:` also admits no push (the
#                      docs job: docs-pages.yaml builds main's site): one is selected only when
#                      EVENT is not push. check-ci-filters.sh holds both arrays to ci.yaml.
# A needed job SELECTED_BY does not name fails (ci-ok does not know what selects it), and so does a
# SELECTED_BY job that is absent from NEEDS. It prints every job's result and whether it was
# selected, and an `::error::` for each failure naming the job, its result and why.
#
# Run from anywhere:
#   NEEDS=... DRAFT=... EVENT=... SELECTED_BY=... PULL_REQUEST_ONLY=... NOT_ON_PUSH=... .github/scripts/ci-ok.sh
# CI runs it in ci.yaml's ci-ok job, its tests in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

for input in NEEDS DRAFT EVENT SELECTED_BY PULL_REQUEST_ONLY NOT_ON_PUSH; do
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
for list in PULL_REQUEST_ONLY NOT_ON_PUSH; do
  if ! jq -e --argjson map "$SELECTED_BY" 'type == "array" and all(.[]; type == "string" and (. as $job | $map | has($job)))' \
    <<<"${!list}" >/dev/null 2>&1; then
    echo "::error::$list is not a JSON array of jobs SELECTED_BY names: ${!list}"
    exit 1
  fi
done

# One row per line, tab-separated: `info` and the line to print, or `error` and the ::error:: text.
# Every job is printed in NEEDS order, after `changes` or lint if NEEDS lacks them and before each
# SELECTED_BY job NEEDS lacks.
read -r -d '' program <<'JQ' || true
(.changes.outputs // {}) as $out
| def selection($job):
    $map[$job] as $output
    | ($out[$output] // "unset") as $value
    | ($pr | any(. == $job)) as $pr_only
    | ($not_push | any(. == $job)) as $no_push
    | if $value != "true" then {selected: false, why: "needs.changes.outputs.\($output) is \($value)"}
      elif $pr_only and $event != "pull_request" then
        {selected: false, why: "it runs only on pull_request, and this run is \($event)"}
      elif $no_push and $event == "push" then
        {selected: false, why: "it runs on no push, and this run is push"}
      else {selected: true,
            why: "needs.changes.outputs.\($output) is true\(if $pr_only or $no_push then " on a \($event)" else "" end)"}
      end;
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
          ["error", "\($job) is absent from ci-ok's needs, but SELECTED_BY maps it to needs.changes.outputs.\($map[$job]); add it to ci-ok's needs or drop it from SELECTED_BY"]
        elif $s.selected and $result != "success" then
          ["error", "\($job) ended \($result), but changes selected it (\($s.why)); a selected job must succeed, and a skip here is what a failed-only re-run leaves"]
        elif ($s.selected | not) and $result != "success" and $result != "skipped" then
          ["error", "\($job) ended \($result); a job changes did not select must succeed or be skipped"]
        else empty end
    else
      ["info", "\($job): \($result) (not in SELECTED_BY)"],
      ["error", "\($job) ended \($result), and ci-ok does not know what selects it; add it to ci-ok's SELECTED_BY in ci.yaml"]
    end;
  . as $needs
  | (("changes", "lint") | select(. as $job | $needs | has($job) | not) | rows(.; "absent")),
    ($needs | to_entries[] | rows(.key; .value.result // "absent")),
    ($map | keys_unsorted[] | select(. as $job | $needs | has($job) | not) | rows(.; "absent"))
| @tsv
JQ
rows=$(jq -r --arg event "$EVENT" --argjson map "$SELECTED_BY" --argjson pr "$PULL_REQUEST_ONLY" \
  --argjson not_push "$NOT_ON_PUSH" "$program" <<<"$NEEDS")

errors=()
while IFS=$'\t' read -r level text; do
  case $level in
    info) echo "$text" ;;
    error) errors+=("$text") ;;
  esac
done <<<"$rows"

if [ "$DRAFT" = "true" ]; then
  echo "::error::Draft pull request: no test job runs. Mark it ready for review to start them."
  exit 1
fi
if [ "${#errors[@]}" -gt 0 ]; then
  for error in "${errors[@]}"; do
    echo "::error::$error"
  done
  exit 1
fi
echo "changes and lint succeeded, every job changes selected succeeded, and every other job succeeded or was skipped"
