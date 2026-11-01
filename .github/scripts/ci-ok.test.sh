#!/usr/bin/env bash
# Tests for `ci-ok.sh`: ci-ok passes only when `changes` and lint succeeded, every job `changes`
# selected succeeded, and every job it did not select succeeded or was skipped.
#
# ci-ok.sh reads only its environment, so each case hands it a `toJSON(needs)` built here and the
# SELECTED_BY and PULL_REQUEST_ONLY its ci.yaml step sets, shrunk to four jobs: typecheck,
# envoy-client and docs, each selected by its own output (on a push `changes` sets docs' output
# false, since docs-pages.yaml builds main's site), and worker-image, which also runs only on a
# pull request. The script under test is the file CI runs.
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

# needs <changes result> <changes outputs> <lint result> [<job>=<result>]...: the toJSON(needs)
# GitHub hands ci-ok, every other job with no outputs.
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
# What `changes` outputs on a push: docs is false there whatever the diff touched.
push_selects='{"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "false"}'

# run_ci_ok <needs> [<event>] [<draft>]: sets out and status. The event defaults to pull_request.
run_ci_ok() {
  set +e
  out=$(NEEDS=$1 EVENT=${2:-pull_request} DRAFT=${3:-false} SELECTED_BY=$selected_by \
    PULL_REQUEST_ONLY=$pull_request_only "$script_dir/ci-ok.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a failed-only re-run left a selected job skipped"
run_ci_ok "$(needs success "$selects" success typecheck=skipped envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names the job, its result and why" \
  "$(contains "$out" "^::error::typecheck ended skipped, but changes selected it (needs.changes.outputs.typecheck is true)")"
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
run_ci_ok "$(needs success "$selects" success typecheck=cancelled envoy-client=skipped worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" '^::error::typecheck ended cancelled, but changes selected it')"

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

echo "case: on a push, changes sets docs' output false, and docs was skipped"
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

echo "case: a draft pull request"
run_ci_ok "$(needs skipped '{}' skipped typecheck=skipped envoy-client=skipped worker-image=skipped docs=skipped)" pull_request true
check "fails" "$(is "$status" 1)"
check "says to mark it ready" \
  "$(contains "$out" '^::error::Draft pull request: no test job runs. Mark it ready for review to start them.$')"
check "and only that" "$(is "$(grep -c '::error::' <<<"$out")" 1)"

echo "case: a needed job SELECTED_BY does not name"
run_ci_ok "$(needs success "$selects" success typecheck=success envoy-client=skipped worker-image=success docs=skipped \
  contracts=skipped)"
check "fails" "$(is "$status" 1)"
check "names it, its result and why" "$(contains "$out" '^::error::contracts ended skipped, and ci-ok does not know what selects it')"

echo "case: a job SELECTED_BY maps that ci-ok does not need"
run_ci_ok "$(needs success "$selects" success typecheck=success worker-image=success docs=skipped)"
check "fails" "$(is "$status" 1)"
check "names it as absent" "$(contains "$out" '^::error::envoy-client is absent from ci-ok.s needs')"
check "prints it" "$(contains "$out" '^envoy-client: absent ')"

summary "ci-ok.sh fails unless every job changes selected succeeded"
