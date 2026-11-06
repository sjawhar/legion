#!/usr/bin/env bash
# Tests for `ci-ok.sh`: ci-ok passes only when every job the always-run list names succeeded,
# every job `changes` selected succeeded, and every job it did not select succeeded or was
# skipped.
#
# ci-ok.sh reads only its environment, so each case hands it the NEEDS ci.yaml's ci-ok job builds
# with `toJSON(needs)`, which ci-ok-report.sh builds for its early post too,
# and the SELECTED_BY, PULL_REQUEST_ONLY and ALWAYS_RUN of ci.yaml's env, shrunk to four selected
# jobs: typecheck,
# envoy-client and docs, each selected by its own output, and worker-image, which also runs only
# on a pull request. The always-run list is ci.yaml's own `changes` and lint, and a third job,
# smoke-gate, for the cases that prove the list is read rather than hard-coded. Some cases pass
# another event than pull_request, which is what
# PULL_REQUEST_ONLY turns on; the script takes the event as a string and knows no workflow's
# triggers. The script under test is the file CI runs.
#
# Run from anywhere: .github/scripts/ci-ok.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

selected_by='{"typecheck": "typecheck", "envoy-client": "envoy_client", "worker-image": "worker_image", "docs": "docs"}'
pull_request_only='["worker-image"]'
always_run='["changes", "lint"]'
# The same list with a third always-run job, which ci-ok.sh must judge as it judges lint.
three_always='["changes", "lint", "smoke-gate"]'

# needs <changes result> <changes outputs> <lint result> [<job>=<result>]...: the NEEDS
# ci-ok-report.sh hands ci-ok.sh, every other job with no outputs.
needs() {
  local changes=$1 outputs=$2 lint=$3 pair doc
  shift 3
  doc=$(jq -n --arg c "$changes" --argjson o "$outputs" --arg l "$lint" \
    '{changes: {result: $c, outputs: $o}, lint: {result: $l, outputs: {}}}')
  for pair in "$@"; do
    doc=$(jq --arg j "${pair%%=*}" --arg r "${pair#*=}" '.[$j] = {result: $r, outputs: {}}' <<<"$doc")
  done
  echo "$doc"
}

# The outputs of a `changes` that selected typecheck and worker-image and not envoy-client or docs.
selects='{"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "false"}'
# The same with docs selected too.
selects_docs='{"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "true"}'
# The same outputs read on another event, docs unselected: ci-ok.sh takes EVENT as given.
push_selects='{"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "false"}'

# run_ci_ok <needs> [<event>] [<always run>]: sets out and status. The event defaults to
# pull_request and the always-run list to ci.yaml's own two jobs.
run_ci_ok() {
  set +e
  out=$(NEEDS=$1 EVENT=${2:-pull_request} SELECTED_BY=$selected_by \
    PULL_REQUEST_ONLY=$pull_request_only ALWAYS_RUN=${3:-$always_run} "$script_dir/ci-ok.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a job changes selected ended skipped"
run_ci_ok "$(needs success "$selects" success typecheck=skipped envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names the job, its result and why" \
  "$(contains "$out" "^::error::typecheck ended skipped, but changes selected it (needs.changes.outputs.typecheck is true)")"
check "says what the skip means" \
  "$(contains "$out" "a selected job must succeed, and a skip here means the job's if: did not admit the run although changes chose it, a disagreement between the two the author must fix\$")"
check "prints its selection" "$(contains "$out" '^typecheck: skipped (selected: needs.changes.outputs.typecheck is true)$')"

echo "case: every selected job succeeded and the unselected ones were skipped"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped)"
check "passes" "$(is "$status" 0)"
check "prints every job's result" "$(contains "$out" '^changes: success$')"
check "and lint's" "$(contains "$out" '^lint: success$')"
check "and the selected job's selection" "$(contains "$out" '^typecheck: success (selected: needs.changes.outputs.typecheck is true)$')"
check "and the unselected job's" \
  "$(contains "$out" '^envoy-client: skipped (not selected: needs.changes.outputs.envoy_client is false)$')"
check "says why it passed" "$(contains "$out" 'changes and lint succeeded, every job changes selected succeeded')"
check "reports no error" "$(is "$(contains "$out" '::error::')" false)"

echo "case: a job changes did not select, skipped, beside a selected one that succeeded"
run_ci_ok "$(needs success '{"typecheck": "true", "envoy_client": "false", "worker_image": "false", "docs": "false"}' success \
  typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)"
check "passes" "$(is "$status" 0)"
check "prints worker-image unselected" \
  "$(contains "$out" '^worker-image: skipped (not selected: needs.changes.outputs.worker_image is false)$')"

echo "case: a selected job was cancelled"
# A selected job reads `cancelled` when its own `timeout-minutes` cut it off, when the run was
# cancelled, or when a force-cancel ended it: none of the three is its filter's doing, so the
# error sends the author to the job's log and its limit rather than to its `if:`.
run_ci_ok "$(needs success "$selects" success typecheck=cancelled envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" '^::error::typecheck ended cancelled, but changes selected it')"
check "says it was cut off, and where to look" \
  "$(contains "$out" "a cancellation here is the job cut off rather than filtered out — its own timeout-minutes, a cancelled run or a force-cancel — so the job's log and its timeout-minutes say which, not its filter\$")"
check "does not call it a disagreement between the filter and the if:" \
  "$(is "$(contains "$out" 'did not admit the run although changes chose it')" false)"

echo "case: an unselected job failed"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=failure worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" '^::error::envoy-client ended failure; a job changes did not select must succeed or be skipped')"

echo "case: worker-image's output is true on a push, where its if: admits only pull_request"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)" push
check "passes" "$(is "$status" 0)"
check "prints why it is not selected" \
  "$(contains "$out" '^worker-image: skipped (not selected: it runs only on pull_request, and this run is push)$')"

echo "case: worker-image's output is true on a pull request, and it was skipped"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" "^::error::worker-image ended skipped, but changes selected it (needs.changes.outputs.worker_image is true on a pull_request)")"

echo "case: changes' docs output is false and docs was skipped, on an event that is not a pull request"
run_ci_ok "$(needs success "$push_selects" success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)" push
check "passes" "$(is "$status" 0)"
check "prints why it is not selected" \
  "$(contains "$out" '^docs: skipped (not selected: needs.changes.outputs.docs is false)$')"
check "reports no error" "$(is "$(contains "$out" '::error::')" false)"

echo "case: docs' output is true on a pull request, and it was skipped"
run_ci_ok "$(needs success "$selects_docs" success typecheck=success envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" "^::error::docs ended skipped, but changes selected it (needs.changes.outputs.docs is true)")"

echo "case: docs' output is true on a merge group, and it was skipped"
run_ci_ok "$(needs success "$selects_docs" success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)" merge_group
check "fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" "^::error::docs ended skipped, but changes selected it (needs.changes.outputs.docs is true)")"

echo "case: changes failed"
run_ci_ok "$(needs failure '{}' success typecheck=success envoy-client=skipped worker-image=skipped docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it and why" "$(contains "$out" '^::error::changes ended failure; it selects the jobs from this head')"
check "prints the jobs it could not select" "$(contains "$out" '^envoy-client: skipped (not selected: needs.changes.outputs.envoy_client is unset)$')"

echo "case: lint was skipped"
run_ci_ok "$(needs success "$selects" skipped typecheck=success envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it and why" "$(contains "$out" '^::error::lint ended skipped; lint runs on every change')"

echo "case: a job SELECTED_BY does not name"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped \
  contracts=skipped)"
check "fails" "$(is "$status" 1)"
check "names it, its result and why" "$(contains "$out" '^::error::contracts ended skipped, and ci-ok does not know what selects it')"
check "names the three classes it could join" \
  "$(contains "$out" "map it in CI_OK_SELECTED_BY in ci.yaml's env when a changes output gates it, name it in CI_OK_ALWAYS_RUN when it runs on every change, or name it in CI_OK_EXEMPT when it is outside ci-ok's accounting\$")"

echo "case: a job SELECTED_BY maps is absent from NEEDS"
run_ci_ok "$(needs success "$selects" success typecheck=success worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it as absent" "$(contains "$out" '^::error::envoy-client is absent from NEEDS')"
check "prints it" "$(contains "$out" '^envoy-client: absent ')"

echo "case: a job changes selected failed"
run_ci_ok "$(needs success "$selects" success typecheck=failure envoy-client=skipped worker-image=success docs=skipped)"
check "fails, so the ci-ok job's own check run carries the failure" "$(is "$status" 1)"
check "names the job, its result and why" \
  "$(contains "$out" '^::error::typecheck ended failure, but changes selected it (needs.changes.outputs.typecheck is true)')"
check "prints every other job's result" "$(contains "$out" '^worker-image: success (selected')"
check "says the job failed" \
  "$(contains "$out" 'a selected job must succeed, and this one failed: its log says what broke$')"

echo "case: a job the always-run list names succeeded"
# The class is ci.yaml's CI_OK_ALWAYS_RUN, not two names in the script: a third job of the list
# is judged as lint is, and the pass line names it with the other two.
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped \
  smoke-gate=success)" pull_request "$three_always"
check "passes" "$(is "$status" 0)"
check "prints its result with no selection" "$(contains "$out" '^smoke-gate: success$')"
check "names it in the pass line" \
  "$(contains "$out" '^changes, lint and smoke-gate succeeded, every job changes selected succeeded')"

echo "case: a job the always-run list names was skipped"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped \
  smoke-gate=skipped)" pull_request "$three_always"
check "fails" "$(is "$status" 1)"
check "names it and why, as it does lint" \
  "$(contains "$out" '^::error::smoke-gate ended skipped; smoke-gate runs on every change, so ci-ok passes only when it succeeded$')"

echo "case: a job the always-run list names is absent from NEEDS"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped)" \
  pull_request "$three_always"
check "fails" "$(is "$status" 1)"
check "prints it absent" "$(contains "$out" '^smoke-gate: absent$')"
check "names it and why" \
  "$(contains "$out" '^::error::smoke-gate ended absent; smoke-gate runs on every change')"

echo "case: a job the always-run list does not name, which ci-ok.sh judged by name before"
# smoke-gate is in no class while the list is ci.yaml's two: the verdict fails it with the three
# remedies rather than holding it to success.
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped \
  smoke-gate=success)"
check "fails" "$(is "$status" 1)"
check "names it and the three classes it could join" \
  "$(contains "$out" '^::error::smoke-gate ended success, and ci-ok does not know what selects it; map it in CI_OK_SELECTED_BY')"

summary "ci-ok.sh fails unless every job changes selected succeeded"
