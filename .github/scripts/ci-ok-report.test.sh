#!/usr/bin/env bash
# Tests for `ci-ok-report.sh` and the `post-check-run.sh` it posts with: the failing ci-ok of a
# merge-group run goes up from the job whose own result decides it, the moment that job ends,
# naming that job in the title and every other job at the result the jobs route shows; every
# other run posts nothing, because ci.yaml's ci-ok job carries the verdict there; and a jobs list
# GitHub refuses still posts the verdict, where a refused post fails the step.
#
# Each case writes the run's jobs as the jobs route lists them and the `changes` and `job.status`
# the reporting job saw, and runs the real scripts with a stand-in `gh` first on PATH: it answers
# the jobs read and records each check run posted. The map is ci-ok.test.sh's four jobs and the
# always-run list ci.yaml's own two, in a
# ci.yaml of the case's own (CI_OK_WORKFLOW); one case reads the real ci.yaml.
#
# Run from anywhere: .github/scripts/ci-ok-report.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

mkdir "$work/bin"
cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
set -euo pipefail
case "\$*" in
  *"/actions/runs/41/jobs?per_page=100"*)
    if [ ! -e "$work/jobs" ]; then echo "HTTP 502" >&2; exit 1; fi
    cat "$work/jobs" ;;
  *"--method POST repos/o/r/check-runs"*)
    # post-check-run.sh sends the body as a file, so that a retried post can send it again.
    n=\$(find "$work" -maxdepth 1 -name 'posted.*.json' | wc -l)
    body=
    while [ \$# -gt 0 ]; do
      case \$1 in --input) body=\$2 ;; esac
      shift
    done
    cp "\$body" "$work/posted.\$n.json"
    if [ -e "$work/post-fails" ]; then echo "HTTP 403" >&2; exit 1; fi
    echo "https://github.com/o/r/runs/\$n" ;;
  *) echo "unexpected gh \$*" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"

cat > "$work/ci.yaml" <<'YAML'
env:
  CI_OK_SELECTED_BY: |
    {"typecheck": "typecheck", "envoy-client": "envoy_client", "worker-image": "worker_image", "docs": "docs"}
  CI_OK_PULL_REQUEST_ONLY: '["worker-image"]'
  CI_OK_ALWAYS_RUN: '["changes", "lint"]'
YAML

# What `changes` gave the reporting job on a merge group: typecheck and envoy-client selected,
# worker-image held back by its pull_request-only `if:`.
selects='{"result": "success", "outputs": {"typecheck": "true", "envoy_client": "true", "worker_image": "false", "docs": "false"}}'

# jobs <name>=<status>[/<conclusion>]...: the run's jobs as the jobs route lists them, one JSON
# object per line, as `gh api --jq '.jobs[] | …'` prints them. The run's first case writes none,
# which is the route refusing the read.
jobs() {
  local pair
  rm -f "$work"/posted.*.json "$work/post-fails" "$work/jobs"
  for pair in "$@"; do
    jq -nc --arg name "${pair%%=*}" --arg status "${pair#*=}" \
      '{name: $name, status: ($status | split("/")[0]), conclusion: ($status | split("/")[1])}' \
      >> "$work/jobs"
  done
}

# run_report <changes> <event> <job status> [<workflow>]: sets out and status.
run_report() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
    GITHUB_SERVER_URL=https://github.com GH_RETRY_SECONDS=0 JOB=typecheck CHANGES=$1 \
    EVENT=$2 JOB_STATUS=$3 HEAD_SHA=abc123 CI_OK_WORKFLOW=${4:-$work/ci.yaml} \
    "$script_dir/ci-ok-report.sh" 2>&1)
  status=$?
  set -e
}

posted() { find "$work" -maxdepth 1 -name 'posted.*.json' | wc -l | tr -d ' '; }
field() { jq -r "$1" "$work/posted.0.json"; }

echo "case: a merge_group run whose reporting job failed posts without waiting"
jobs typecheck=in_progress lint=in_progress envoy-client=queued
run_report "$selects" merge_group failure
check "exits 0" "$(is "$status" 0)"
check "posts one check run" "$(is "$(posted)" 1)"
check "named ci-ok" "$(is "$(field .name)" ci-ok)"
check "on the head" "$(is "$(field .head_sha)" abc123)"
check "completed" "$(is "$(field .status)" completed)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "linking the run" "$(is "$(field .details_url)" https://github.com/o/r/actions/runs/41)"
check "marked as a ci-ok a job posted, which ci-ok-reconcile.sh reads" \
  "$(is "$(field .external_id)" ci-ok-post:41)"
check "records the reporting job at its own job.status, not the status the route shows it at" \
  "$(contains "$(field .output.summary)" '^typecheck: failure (selected')"
check "records a running job at its live status" \
  "$(contains "$(field .output.summary)" '^lint: in_progress$')"
check "records a queued job at its live status" \
  "$(contains "$(field .output.summary)" '^envoy-client: queued (selected')"
check "records a job the route does not list as unlisted" \
  "$(contains "$(field .output.summary)" '^docs: unlisted (not selected')"
check "says the failing job decided it" \
  "$(contains "$(field .output.summary)" 'Decided by typecheck, whose failure decides this merge-group run')"
check "says the verdict is not the ci-ok job's" \
  "$(contains "$(field .output.summary)" 'rather than from the ci-ok job at the end of the run')"
check "titles it with the job whose result decided the run" \
  "$(contains "$(field .output.title)" '^typecheck decided this merge-group run: ')"

echo "case: a merge_group run whose reporting job its timeout-minutes cancelled"
# `cancelled` is what a job cut off by its `timeout-minutes` reports (measured on probe run
# 41869007039), and it decides the group as a failure does. The queue takes the pull request out
# on the verdict this job posts and dequeued.yaml force-cancels the run, and a force-cancel leaves
# every `if: always()` step skipped, so no later job of the run reports and the ci-ok job never
# runs: this reporting job is the run's last chance to post ci-ok, and a group that gets none sits
# in the queue until GitHub's 180-minute check timeout removes it.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
run_report "$selects" merge_group cancelled
check "exits 0" "$(is "$status" 0)"
check "posts one check run rather than waiting" "$(is "$(posted)" 1)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "says the cancellation decided it" \
  "$(contains "$(field .output.summary)" 'whose cancellation decides this merge-group run')"

echo "case: a merge_group run whose changes failed"
jobs typecheck=in_progress lint=in_progress
run_report '{"result": "failure", "outputs": {}}' merge_group success
check "posts although this job succeeded" "$(is "$(posted)" 1)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "titled with changes, the job whose result decided the run" \
  "$(contains "$(field .output.title)" '^changes decided this merge-group run: ')"
check "titled with changes' failure" "$(contains "$(field .output.title)" 'changes ended failure')"

echo "case: a merge_group run whose changes its own time limit cancelled"
# `changes` carries no report step and reports nothing of its own, and its time limit cuts it off
# as `cancelled`, which decides the group as a failure does.
jobs typecheck=in_progress lint=in_progress
run_report '{"result": "cancelled", "outputs": {}}' merge_group success
check "posts one check run rather than waiting" "$(is "$(posted)" 1)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "says the cancelled changes decided it" \
  "$(contains "$(field .output.summary)" 'reporting after changes was cancelled, which decides this merge-group run')"

echo "case: a merge_group run this job decides nothing of"
jobs typecheck=in_progress lint=in_progress envoy-client=queued
run_report "$selects" merge_group success
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "leaves the verdict to the ci-ok job" \
  "$(contains "$out" 'typecheck ended success on a merge_group run, which this job decides nothing of; the ci-ok job posts this run.s verdict')"

echo "case: the same failure on a pull_request run"
# No queue holds a pull request, and its run is cancelled by the next push, so nothing is posted
# early there: the ci-ok job fails on this job's result at the end of the run.
jobs typecheck=in_progress lint=in_progress envoy-client=queued
run_report "$selects" pull_request failure
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "reads no job list either" "$(is "$(contains "$out" 'HTTP 502')" false)"

echo "case: the same failure on a dispatched run"
jobs typecheck=in_progress
run_report "$selects" workflow_dispatch failure
check "posts nothing" "$(is "$(posted)" 0)"

echo "case: a decided merge_group run whose jobs route GitHub refuses posts its verdict all the same"
# The jobs route is read three times before the script gives up. On a decided merge-group run
# nothing this job waits for can help: the queue takes the pull request out on this verdict and
# dequeued.yaml force-cancels the run, and a force-cancel leaves every `if: always()` step
# skipped. A run that waited for a read it could not make would carry no ci-ok at all and hold the
# queue until GitHub's 180-minute check timeout dropped its pull request. So every other job is
# recorded `unread`, which ci-ok.sh fails on, and the verdict goes up: nothing worse can come of
# posting a failing verdict on a group that is about to be thrown away.
jobs
run_report "$selects" merge_group failure
check "exits 0" "$(is "$status" 0)"
check "posts its failing ci-ok rather than waiting on a read it cannot make" "$(is "$(posted)" 1)"
check "failing" "$(is "$(field .conclusion)" failure)"
check "records every other job as unread" "$(contains "$(field .output.summary)" '^lint: unread$')"
check "and the reporting job at its own status" \
  "$(contains "$(field .output.summary)" '^typecheck: failure (selected')"
check "warns why it posted on an unread run" \
  "$(contains "$out" "::warning::GitHub refused this run's job list three times")"

echo "case: GitHub refuses the post"
jobs typecheck=in_progress lint=in_progress envoy-client=queued
touch "$work/post-fails"
run_report "$selects" merge_group failure
check "fails the step" "$(is "$status" 1)"
check "tried three times" "$(is "$(posted)" 3)"
check "says so" "$(contains "$out" 'GitHub refused the ci-ok check run on abc123 three times')"

echo "case: CHANGES is not toJSON(needs.changes)"
jobs typecheck=in_progress
run_report '{}' merge_group failure
check "fails" "$(is "$status" 1)"
check "posts nothing" "$(is "$(posted)" 0)"

echo "case: the real ci.yaml's map, with nothing selected"
jobs typecheck=in_progress
run_report '{"result": "success", "outputs": {}}' merge_group failure "$script_dir/../workflows/ci.yaml"
check "reads it and posts" "$(is "$(posted)" 1)"
check "failing on the job that decided the run" "$(is "$(field .conclusion)" failure)"

echo "case: post-check-run.sh called with no marker"
# The marker's prefix is what tells a posted check run from a job's own check run of the same
# name, whose own external_id is a UUID, and ci-ok-reconcile.sh PATCHes only the check runs
# carrying it, so a post that set none would leave a check run no later attempt could reconcile.
jobs
: > "$work/summary.md"
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GH_RETRY_SECONDS=0 \
  HEAD_SHA=abc123 CHECK_NAME=pr-title CONCLUSION=success TITLE='the title is conventional' \
  SUMMARY_FILE="$work/summary.md" DETAILS_URL=https://github.com/o/r/actions/runs/41 \
  "$script_dir/post-check-run.sh" 2>&1)
status=$?
set -e
check "fails the step" "$(is "$status" 1)"
check "posts nothing" "$(is "$(posted)" 0)"
check "names the input" \
  "$(contains "$out" 'post-check-run.sh needs EXTERNAL_ID in its environment')"

echo "case: post-check-run.sh posts the marker it was given"
# As ci.yaml's lint job calls it for pr-title, the other check run posted this way.
jobs
: > "$work/summary.md"
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GH_RETRY_SECONDS=0 \
  HEAD_SHA=abc123 CHECK_NAME=pr-title CONCLUSION=success TITLE='the title is conventional' \
  SUMMARY_FILE="$work/summary.md" DETAILS_URL=https://github.com/o/r/actions/runs/41 \
  EXTERNAL_ID=pr-title:41 "$script_dir/post-check-run.sh" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "posts one check run" "$(is "$(posted)" 1)"
check "carrying that external_id" "$(is "$(field .external_id)" pr-title:41)"

summary "ci-ok-report.sh posts the failing ci-ok of a merge-group run one job's result decided"
