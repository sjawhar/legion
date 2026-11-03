#!/usr/bin/env bash
# Tests for `ci-ok-report.sh` and the `post-check-run.sh` it posts with: a job posts ci-ok only
# once lint and every job `changes` selected have reported, a re-run's report replaces the
# earlier attempt's, the verdict is ci-ok.sh's, and a list GitHub refuses posts nothing.
#
# Each case writes the run's artifact names, which are the jobs' reports, and the `changes` the
# reporting job saw, and runs the real scripts with a stand-in `gh` first on PATH: it answers the
# artifact list with those names and records each check run posted. The map is ci-ok.test.sh's
# four jobs, in a ci.yaml of the case's own (CI_OK_WORKFLOW); one case reads the real ci.yaml.
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
  *"/actions/runs/41/artifacts?per_page=100"*)
    if [ -e "$work/list-fails" ]; then echo "HTTP 502" >&2; exit 1; fi
    cat "$work/artifacts" ;;
  *"--method POST repos/o/r/check-runs"*)
    n=\$(find "$work" -maxdepth 1 -name 'posted.*.json' | wc -l)
    cat > "$work/posted.\$n.json"
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
YAML

# What `changes` gave the reporting job: typecheck and worker-image selected.
selects='{"result": "success", "outputs": {"typecheck": "true", "envoy_client": "false", "worker_image": "true", "docs": "false"}}'

# report <artifact name>...: the run's artifacts, which other jobs' uploads leave beside the reports.
report() {
  printf '%s\n' pi-envoy-pr "$@" > "$work/artifacts"
  rm -f "$work"/posted.*.json "$work/list-fails" "$work/post-fails"
}

# run_report <changes> [<event>] [<workflow>]: sets out and status.
run_report() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
    GITHUB_SERVER_URL=https://github.com GH_RETRY_SECONDS=0 JOB=typecheck CHANGES=$1 \
    EVENT=${2:-pull_request} HEAD_SHA=abc123 CI_OK_WORKFLOW=${3:-$work/ci.yaml} \
    "$script_dir/ci-ok-report.sh" 2>&1)
  status=$?
  set -e
}

posted() { find "$work" -maxdepth 1 -name 'posted.*.json' | wc -l | tr -d ' '; }
field() { jq -r "$1" "$work/posted.0.json"; }

echo "case: a job changes selected has not reported"
report ci-ok-lint-1-success ci-ok-typecheck-1-success
run_report "$selects"
check "exits 0" "$(is "$status" 0)"
check "posts nothing" "$(is "$(posted)" 0)"
check "names the job it waits on" "$(contains "$out" 'ci-ok waits on worker-image, and the last of them to finish posts it')"

echo "case: lint has not reported"
report ci-ok-typecheck-1-success ci-ok-worker-image-1-success
run_report "$selects"
check "posts nothing" "$(is "$(posted)" 0)"
check "waits on lint" "$(contains "$out" 'ci-ok waits on lint,')"

echo "case: lint and every selected job reported success"
report ci-ok-lint-1-success ci-ok-typecheck-1-success ci-ok-worker-image-1-success
run_report "$selects"
check "exits 0" "$(is "$status" 0)"
check "posts one check run" "$(is "$(posted)" 1)"
check "named ci-ok" "$(is "$(field .name)" ci-ok)"
check "on the head" "$(is "$(field .head_sha)" abc123)"
check "completed" "$(is "$(field .status)" completed)"
check "passing" "$(is "$(field .conclusion)" success)"
check "linking the run" "$(is "$(field .details_url)" https://github.com/o/r/actions/runs/41)"
check "summarising ci-ok.sh's verdict" "$(contains "$(field .output.summary)" '^envoy-client: skipped (not selected')"

echo "case: a selected job reported failure"
report ci-ok-lint-1-success ci-ok-typecheck-1-failure ci-ok-worker-image-1-success
run_report "$selects"
check "exits 0" "$(is "$status" 0)"
check "posts a failing ci-ok" "$(is "$(field .conclusion)" failure)"
check "titled with the first error" \
  "$(contains "$(field .output.title)" '^typecheck ended failure, but changes selected it')"

echo "case: a re-run attempt's report replaces the earlier one"
report ci-ok-lint-1-success ci-ok-typecheck-1-failure ci-ok-worker-image-1-success ci-ok-typecheck-2-success
run_report "$selects"
check "passes" "$(is "$(field .conclusion)" success)"

echo "case: an older attempt's report does not replace a newer one"
report ci-ok-lint-1-success ci-ok-typecheck-2-failure ci-ok-worker-image-1-success ci-ok-typecheck-1-success
run_report "$selects"
check "fails" "$(is "$(field .conclusion)" failure)"

echo "case: a cancelled job reported"
report ci-ok-lint-1-success ci-ok-typecheck-1-cancelled ci-ok-worker-image-1-success
run_report "$selects"
check "fails" "$(is "$(field .conclusion)" failure)"

echo "case: a job changes did not select reported failure"
report ci-ok-lint-1-success ci-ok-typecheck-1-success ci-ok-worker-image-1-success ci-ok-envoy-client-1-failure
run_report "$selects"
check "fails" "$(is "$(field .conclusion)" failure)"

echo "case: a job the map does not name reported"
report ci-ok-lint-1-success ci-ok-typecheck-1-success ci-ok-worker-image-1-success ci-ok-contracts-1-success
run_report "$selects"
check "fails" "$(is "$(field .conclusion)" failure)"
check "says ci-ok does not know what selects it" \
  "$(contains "$(field .output.summary)" 'contracts ended success, and ci-ok does not know what selects it')"

echo "case: a push, where worker-image's if: admits only pull_request"
report ci-ok-lint-1-success ci-ok-typecheck-1-success
run_report "$selects" push
check "does not wait on worker-image" "$(is "$(posted)" 1)"
check "passes" "$(is "$(field .conclusion)" success)"

echo "case: changes failed"
report ci-ok-lint-1-success
run_report '{"result": "failure", "outputs": {}}'
check "posts once lint reported" "$(is "$(posted)" 1)"
check "fails" "$(is "$(field .conclusion)" failure)"
check "titled with changes' failure" "$(contains "$(field .output.title)" '^changes ended failure')"

echo "case: GitHub refuses the artifact list"
report ci-ok-lint-1-success ci-ok-typecheck-1-success ci-ok-worker-image-1-success
touch "$work/list-fails"
run_report "$selects"
check "fails the step" "$(is "$status" 1)"
check "posts nothing" "$(is "$(posted)" 0)"
check "says why" "$(contains "$out" "GitHub refused the run's artifact list three times")"

echo "case: GitHub refuses the post"
report ci-ok-lint-1-success ci-ok-typecheck-1-success ci-ok-worker-image-1-success
touch "$work/post-fails"
run_report "$selects"
check "fails the step" "$(is "$status" 1)"
check "tried three times" "$(is "$(posted)" 3)"
check "says so" "$(contains "$out" 'GitHub refused the ci-ok check run on abc123 three times')"

echo "case: CHANGES is not toJSON(needs.changes)"
report ci-ok-lint-1-success
run_report '{}'
check "fails" "$(is "$status" 1)"
check "posts nothing" "$(is "$(posted)" 0)"

echo "case: the real ci.yaml's map, with nothing selected"
report ci-ok-lint-1-success
run_report '{"result": "success", "outputs": {}}' pull_request "$script_dir/../workflows/ci.yaml"
check "reads it and posts" "$(is "$(posted)" 1)"
check "passes" "$(is "$(field .conclusion)" success)"

summary "ci-ok-report.sh posts ci-ok only once every job that runs has reported"
