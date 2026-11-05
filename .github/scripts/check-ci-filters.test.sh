#!/usr/bin/env bash
# Tests for `check-ci-filters.sh`: every ci.yaml filter must match every file its job reads, the
# Go closures recomputed with go list (with -race for a -race command), the table of go commands
# must say what ci.yaml says, a claimed go command the check does not reduce (another form, a
# step that is more than the go line, a shell, a container, a BASH_ENV) and a step off its exact
# entry's pinned script count as reading every Go file of the repository and every file under a
# Go module's directory, a why that cites a step must name a workflow file that still holds it,
# CI_OK_SELECTED_BY must map every job changes gates, it and CI_OK_PULL_REQUEST_ONLY must say what
# each job's if: tests, every job but changes and those CI_OK_EXEMPT names must end with the ci-ok
# report step and no job may be named for a required check, and a PUSH_WORKFLOWS workflow's
# on.push.paths must equal its filter, no `if:` of ci.yaml may test for a push event or compare
# github.ref with a branch ref, every job a merge-group run runs must have a follower cancel job
# carrying the one follower condition, every job's if: must hold the term that skips it on a
# re-run attempt of a merge-group run, and ci-caches.yaml's save of a cache family must carry
# the key and the paths ci.yaml's restore of that family names, its own restores being lookups.
#
# Each case builds a miniature repository under a temporary directory - a go.work binding one
# module, a ci.yaml whose `changes` job gates one job, and a ci_job_reads.py describing it - and
# symlinks the real script, its engine (ci_filter_coverage.py), the rules of ci.yaml it runs
# (ci_workflow_rules.py) and the path model they import (workflow_paths.py) into it, so the code
# under test is the code CI runs. It needs go and a
# python3 with PyYAML.
#
# Run from anywhere: .github/scripts/check-ci-filters.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes. The app job runs `go test ./cmd/app ./lib`, whose closure is
# cmd/app and lib (cmd/app imports lib); lib's test reads fixtures/data.json through `..`, which
# RULES records; mod/other is a package nothing imports. The image job runs only on a pull
# request, and both end with the ci-ok report step. The site filter gates no job: site.yaml, which
# PUSH_WORKFLOWS pairs with it, runs on a push whose paths are that filter flattened. Echoes the
# fixture root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/mod/cmd/app" "$root/mod/lib" \
    "$root/mod/other" "$root/fixtures" "$root/docs"
  for linked in check-ci-filters.sh ci_filter_coverage.py ci_workflow_rules.py workflow_paths.py; do
    ln -s "$script_dir/$linked" "$root/.github/scripts/$linked"
  done
  printf 'go 1.21\n\nuse ./mod\n' > "$root/go.work"
  printf 'module example.com/m\n\ngo 1.21\n' > "$root/mod/go.mod"
  printf 'package main\n\nimport "example.com/m/lib"\n\nfunc main() { lib.Hello() }\n' > "$root/mod/cmd/app/main.go"
  printf 'package lib\n\nfunc Hello() {}\n' > "$root/mod/lib/lib.go"
  cat > "$root/mod/lib/lib_test.go" <<'GO'
package lib

import (
	"os"
	"testing"
)

func TestData(t *testing.T) {
	if _, err := os.ReadFile("../../fixtures/data.json"); err != nil {
		t.Fatal(err)
	}
}
GO
  printf 'package other\n' > "$root/mod/other/other.go"
  echo '{}' > "$root/fixtures/data.json"
  echo 'unrelated' > "$root/docs/readme.md"
  cat > "$root/.github/scripts/ci_job_reads.py" <<'PY'
from ci_filter_coverage import Read, at, gate, run

RULES = {"mod/lib/lib_test.go": [Read("fixtures/data.json", "lib_test.go reads ../../fixtures/data.json")]}
GATES = [
    gate("app", "app", runs=[
        run("mod", ["./cmd/app", "./lib"], at("app", "Test the app") + ": go test ./cmd/app ./lib", step="Test the app",
            text="go test ./cmd/app ./lib"),
    ], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")]),
]
EXCLUDED = {"image": "an image build another check proves"}
PUSH_WORKFLOWS = {"site": ".github/workflows/site.yaml"}
PY
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: [pull_request]
env:
  CI_OK_SELECTED_BY: '{"app": "app", "image": "image"}'
  CI_OK_PULL_REQUEST_ONLY: '["image"]'
  CI_OK_EXEMPT: '["cancel-changes", "cancel-app"]'
jobs:
  changes:
    if: (github.event_name != 'merge_group' || github.run_attempt == '1')
    runs-on: ubuntu-24.04
    outputs:
      app: ${{ steps.filter.outputs.app }}
      image: ${{ steps.filter.outputs.image }}
    steps:
      - uses: dorny/paths-filter@v4
        id: filter
        with:
          predicate-quantifier: some-with-excludes
          filters: |
            workspace: &workspace
              - 'go.work'
              - 'mod/go.mod'
            app:
              - '.github/workflows/ci.yaml'
              - *workspace
              - 'mod/cmd/app/*'
              - 'mod/lib/*'
              - 'fixtures/**'
            image:
              - 'docs/**'
            site:
              - *workspace
              - 'docs/**'
  app:
    needs: changes
    if: needs.changes.outputs.app == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')
    runs-on: ubuntu-24.04
    steps:
      - name: Test the app
        run: go test ./cmd/app ./lib
        working-directory: mod
      - name: Report ci-ok when this is the run's last job
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          changes: ${{ toJSON(needs.changes) }}
          job-status: ${{ job.status }}
  image:
    needs: changes
    if: github.event_name == 'pull_request' && needs.changes.outputs.image == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')
    runs-on: ubuntu-24.04
    steps:
      - name: Build
        run: go build ./...
      - name: Report ci-ok when this is the run's last job
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          changes: ${{ toJSON(needs.changes) }}
          job-status: ${{ job.status }}
  cancel-changes:
    needs: changes
    if: ${{ !cancelled() && github.event_name == 'merge_group' && github.run_attempt == '1' && (contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled')) }}
    runs-on: ubuntu-24.04
    permissions:
      actions: write
    steps:
      - uses: ./.github/actions/merge-group-cancel
        with:
          needs: ${{ toJSON(needs) }}
  cancel-app:
    needs: app
    if: ${{ !cancelled() && github.event_name == 'merge_group' && github.run_attempt == '1' && (contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled')) }}
    runs-on: ubuntu-24.04
    permissions:
      actions: write
    steps:
      - uses: ./.github/actions/merge-group-cancel
        with:
          needs: ${{ toJSON(needs) }}
YAML
  cat > "$root/.github/workflows/site.yaml" <<'YAML'
name: Site
on:
  push:
    branches: [main]
    paths:
      - 'go.work'
      - 'mod/go.mod'
      - 'docs/**'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - run: echo build
YAML
  git -C "$root" init -q
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-ci-filters.sh" 2>&1)
  status=$?
  set -e
}

# replace <file> <old> <new>: one literal replacement, failing the case loudly if <old> is absent.
replace() {
  local file=$1 old=$2 new=$3 text
  text=$(<"$file")
  if [[ $text != *"$old"* ]]; then
    echo "  FAIL: $file has no '$old' to replace" >&2
    return 1
  fi
  printf '%s\n' "${text/"$old"/"$new"}" > "$file"
}

echo "case: the filter matches the job's closure, its test's read and its module files"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the job's files" "$(contains "$out" '^app: 7 files, 0 uncovered (filter app)$')"
check "holds the push workflow's paths to its filter" \
  "$(contains "$out" '^.github/workflows/site.yaml: on.push.paths equals filter site (3 patterns)$')"
check "says what it checked" "$(contains "$out" 'every ci.yaml filter matches every file its job reads (1 gates)')"

echo "case: an import of a package outside the filter"
root=$(fixture new-import)
printf 'package lib\n\nimport _ "example.com/m/other"\n\nfunc Hello() {}\n' > "$root/mod/lib/lib.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the filter, the file and the go command" \
  "$(contains "$out" '::error file=mod/other/other.go::app reads mod/other/other.go, which its filter app does not match.*go test ./cmd/app ./lib (go list -deps)')"
check "counts it" "$(contains "$out" '^app: 8 files, 1 uncovered (filter app)$')"

echo "case: a file a test reads through .. that the filter leaves out"
root=$(fixture rule-read)
replace "$root/.github/workflows/ci.yaml" "              - 'fixtures/**'" "              - 'docs/**'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file and the rule's why" \
  "$(contains "$out" '::error file=fixtures/data.json::app reads fixtures/data.json.*lib_test.go reads ../../fixtures/data.json')"

echo "case: a rule applies only when go list compiles its file under the job's tags"
root=$(fixture rule-untagged)
replace "$root/.github/workflows/ci.yaml" "              - 'fixtures/**'" "              - 'docs/**'"
replace "$root/mod/lib/lib_test.go" "package lib" $'//go:build special\n\npackage lib'
run_check "$root"
check "passes while the test is built out" "$(is "$status" 0)"
replace "$root/.github/scripts/ci_job_reads.py" 'step="Test the app",' 'step="Test the app", tags="special",'
replace "$root/.github/scripts/ci_job_reads.py" 'text="go test ./cmd/app ./lib"' 'text="go test -tags special ./cmd/app ./lib"'
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test -tags special ./cmd/app ./lib"
run_check "$root"
check "fails once the job's tags compile it" "$(is "$status" 1)"
check "and names its read" "$(contains "$out" '::error file=fixtures/data.json::')"

echo "case: a go command ci.yaml runs that the table does not list"
root=$(fixture unlisted-command)
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          go test ./cmd/app ./lib\n          # go build ./... in a comment is no command\n          go vet ./...'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the step and the command" \
  "$(contains "$out" 'job app step "Test the app" runs `go vet ./...`, which no Run in ci_job_reads.py lists')"
check "and passes over the comment" "$(is "$(contains "$out" 'go build ./... in a comment')" false)"

echo "case: a Run whose pinned text the step no longer runs"
root=$(fixture stale-pin)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test ./..."
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the stale Run" "$(contains "$out" 'job app step "Test the app" no longer runs `go test ./cmd/app ./lib`')"
check "and the command nothing claims" "$(contains "$out" 'runs `go test ./...`, which no Run in ci_job_reads.py lists')"

echo "case: a gate whose filter the changes job does not define"
root=$(fixture no-filter)
replace "$root/.github/workflows/ci.yaml" "            app:" "            application:"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the filter" "$(contains "$out" 'app has no filter app in the changes job')"

echo "case: a rule whose source file is gone"
root=$(fixture stale-rule)
rm "$root/mod/lib/lib_test.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the rule" "$(contains "$out" 'RULES names mod/lib/lib_test.go, which is not in the repository')"

echo "case: a job gated on a filter the table does not know"
root=$(fixture unknown-gate)
replace "$root/.github/scripts/ci_job_reads.py" 'EXCLUDED = {"image": "an image build another check proves"}' 'EXCLUDED = {}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the filter" "$(contains "$out" 'job image is gated on filter image, which ci_job_reads.py neither lists in GATES nor EXCLUDES')"

echo "case: under some-with-excludes a ! pattern excludes"
root=$(fixture excludes)
replace "$root/.github/workflows/ci.yaml" "              - 'mod/cmd/app/*'" $'              - \'mod/**\'\n              - \'!mod/other/**\''
run_check "$root"
check "passes while nothing it excludes is read" "$(is "$status" 0)"
printf 'package lib\n\nimport _ "example.com/m/other"\n\nfunc Hello() {}\n' > "$root/mod/lib/lib.go"
run_check "$root"
check "fails once the job reads a file it excludes" "$(is "$status" 1)"
check "and names it" "$(contains "$out" '::error file=mod/other/other.go::app reads mod/other/other.go')"

echo "case: under some a ! pattern matches every file outside it"
root=$(fixture some-negation)
replace "$root/.github/workflows/ci.yaml" "          predicate-quantifier: some-with-excludes" "          predicate-quantifier: some"
replace "$root/.github/workflows/ci.yaml" "              - 'fixtures/**'" $'              - \'fixtures/**\'\n              - \'!docs/**\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the pattern" "$(contains "$out" 'filter app has !docs/\*\*, a `!` pattern under predicate-quantifier some')"

echo "case: a declared read and a filter pattern that match no file of the repository"
root=$(fixture dead-pattern)
replace "$root/.github/scripts/ci_job_reads.py" 'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")]' \
  'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition"), Read("packages/contracts/fixtures/delivery-live/**", "lib_test.go reads the live fixtures")]'
run_check "$root"
check "fails on the gate's dead read" "$(is "$status" 1)"
check "names the read and the gate it is in" \
  "$(contains "$out" "::error file=.github/scripts/ci_job_reads.py::gate app reads packages/contracts/fixtures/delivery-live/\*\*, which matches no file in the repository")"
replace "$root/.github/workflows/ci.yaml" "              - 'fixtures/**'" \
  $'              - \'fixtures/**\'\n              - \'packages/contracts/fixtures/delivery-live/**\''
run_check "$root"
check "fails on the filter's dead pattern too" "$(is "$status" 1)"
check "names the pattern and the filter it is in" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml::filter app has packages/contracts/fixtures/delivery-live/\*\*, which matches no file in the repository")"

echo "case: a RULES row's dead read fails, and a ! pattern that matches nothing does not"
root=$(fixture dead-pattern-rules)
replace "$root/.github/scripts/ci_job_reads.py" \
  'RULES = {"mod/lib/lib_test.go": [Read("fixtures/data.json", "lib_test.go reads ../../fixtures/data.json")]}' \
  'RULES = {"mod/lib/lib_test.go": [Read("fixtures/data.json", "lib_test.go reads ../../fixtures/data.json"), Read("fixtures/gone/**", "lib_test.go reads a directory that is not here")]}'
run_check "$root"
check "fails on the RULES row's dead read" "$(is "$status" 1)"
check "names the read and the row it is in" \
  "$(contains "$out" "::error file=.github/scripts/ci_job_reads.py::the RULES row for mod/lib/lib_test.go reads fixtures/gone/\*\*, which matches no file in the repository")"

root=$(fixture dead-pattern-exempt)
replace "$root/.github/workflows/ci.yaml" "              - 'fixtures/**'" \
  $'              - \'fixtures/**\'\n              - \'!fixtures/gone/**\''
run_check "$root"
check "a ! pattern matching nothing is no dead pattern" "$(is "$status" 0)"

echo "case: a go command that cannot run fails rather than reading an empty closure"
root=$(fixture broken-workspace)
printf 'go 1.21\n\nuse ./missing\n' > "$root/go.work"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the closure is unknown" "$(contains "$out" 'so the closure of .* is unknown')"

echo "case: CI_OK_SELECTED_BY misses a job changes gates"
root=$(fixture ci-ok-missing)
replace "$root/.github/workflows/ci.yaml" "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "CI_OK_SELECTED_BY: '{\"image\": \"image\"}'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the output to map it to" \
  "$(contains "$out" "CI_OK_SELECTED_BY does not map app, whose if: tests needs.changes.outputs.app, so ci-ok-report.sh never waits for its report and ci-ok.sh fails any it finds; add \"app\": \"app\"")"

echo "case: CI_OK_SELECTED_BY maps a job to an output its if: does not test"
root=$(fixture ci-ok-wrong)
replace "$root/.github/workflows/ci.yaml" "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "CI_OK_SELECTED_BY: '{\"app\": \"image\", \"image\": \"image\"}'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the wrong output and the right one" \
  "$(contains "$out" "CI_OK_SELECTED_BY maps app to image, but its if: tests needs.changes.outputs.app")"

echo "case: CI_OK_SELECTED_BY maps a job changes does not gate"
root=$(fixture ci-ok-extra)
replace "$root/.github/workflows/ci.yaml" "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\", \"gone\": \"gone\"}'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" "CI_OK_SELECTED_BY maps gone, which is no job whose if: tests an output of changes; drop it")"

echo "case: CI_OK_PULL_REQUEST_ONLY misses a job whose if: admits only pull_request"
root=$(fixture ci-ok-pull-request-only)
replace "$root/.github/workflows/ci.yaml" "CI_OK_PULL_REQUEST_ONLY: '[\"image\"]'" "CI_OK_PULL_REQUEST_ONLY: '[]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job" "$(contains "$out" "CI_OK_PULL_REQUEST_ONLY lacks image, whose if: admits only pull_request")"

echo "case: a job whose last step is not the ci-ok report"
root=$(fixture ci-ok-unreported)
replace "$root/.github/workflows/ci.yaml" "        if: always()" "        if: success()"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and what its last step must be" \
  "$(contains "$out" "job app's last step is not \`uses: ./.github/actions/ci-ok-report\` with \`if: always()\`")"

# cancel_job <root>: appends a merge-group-only job that exists to stop a run the queue has
# thrown away. It reports nothing to ci-ok, as a job of CI_OK_EXEMPT must not.
cancel_job() {
  cat >> "$1/.github/workflows/ci.yaml" <<'YAML'
  cancel:
    if: github.event_name == 'merge_group' && github.run_attempt == '1'
    runs-on: ubuntu-24.04
    permissions:
      actions: write
    steps:
      - name: Cancel the rest of the run
        run: gh run cancel "$GITHUB_RUN_ID"
YAML
}

echo "case: a job CI_OK_EXEMPT names passes without the ci-ok report step"
root=$(fixture ci-ok-exempt)
cancel_job "$root"
replace "$root/.github/workflows/ci.yaml" "CI_OK_EXEMPT: '[\"cancel-changes\", \"cancel-app\"]'" \
  "CI_OK_EXEMPT: '[\"cancel-changes\", \"cancel-app\", \"cancel\"]'"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it checked" "$(contains "$out" 'every ci.yaml filter matches every file its job reads (1 gates)')"

echo "case: a job CI_OK_EXEMPT does not name still needs the ci-ok report step"
root=$(fixture ci-ok-exempt-absent)
cancel_job "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and what its last step must be" \
  "$(contains "$out" "job cancel's last step is not \`uses: ./.github/actions/ci-ok-report\` with \`if: always()\`")"

echo "case: a job a merge-group run runs with no follower, and a follower of a job it never runs"
root=$(fixture follower-set)
replace "$root/.github/workflows/ci.yaml" $'  cancel-app:\n    needs: app' $'  cancel-app:\n    needs: image'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job left without one" \
  "$(contains "$out" "job app can run on a merge group and has no follower, so nothing cancels the groups built on this one when it fails")"
check "names the follower that could never fire" \
  "$(contains "$out" "job cancel-app follows image, which is no job a merge-group run of this file runs outside the cancel jobs")"

echo "case: a follower whose condition is not the one every follower carries"
root=$(fixture follower-if)
replace "$root/.github/workflows/ci.yaml" \
  "&& (contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled')) }}" \
  "&& (needs.changes.result == 'failure' || needs.changes.result == 'cancelled') }}"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the follower and the one text" \
  "$(contains "$out" "job cancel-changes's if: is not the one every follower carries")"

echo "case: a job with no term holding it back on a re-run attempt of a merge-group run"
root=$(fixture rerun-guard)
replace "$root/.github/workflows/ci.yaml" \
  "    if: needs.changes.outputs.app == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')" \
  "    if: needs.changes.outputs.app == 'true'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the term it lacks" \
  "$(contains "$out" "job app's if: has no \`(github.event_name != 'merge_group' || github.run_attempt == '1')\`")"
check "says why the changes job's own skip is not enough" \
  "$(contains "$out" 'gh run rerun --failed` does not re-run the `changes` that succeeded')"

echo "case: a merge-group-only job with no attempt-1 term"
root=$(fixture rerun-guard-cancel)
cancel_job "$root"
replace "$root/.github/workflows/ci.yaml" \
  "    if: github.event_name == 'merge_group' && github.run_attempt == '1'" \
  "    if: github.event_name == 'merge_group'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the term it lacks" \
  "$(contains "$out" "job cancel runs on merge_group and its if: has no \`github.run_attempt == '1'\`")"

echo "case: a job named for a required check"
root=$(fixture ci-ok-job)
replace "$root/.github/workflows/ci.yaml" $'\n  image:\n' $'\n  image:\n    name: ci-ok\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the check" \
  "$(contains "$out" "job image is named ci-ok, a check main's ruleset requires")"

echo "case: a push workflow's paths miss a pattern of its filter"
root=$(fixture push-paths-missing)
replace "$root/.github/workflows/site.yaml" "      - 'mod/go.mod'"$'\n' ""
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the workflow, the pattern and the filter" \
  "$(contains "$out" "::error file=.github/workflows/site.yaml::.github/workflows/site.yaml's on.push.paths lacks mod/go.mod, which .github/workflows/ci.yaml's filter site has")"
check "reports the lists differ" "$(contains "$out" '^.github/workflows/site.yaml: on.push.paths differs from filter site')"

echo "case: a push workflow's paths name a pattern its filter lacks"
root=$(fixture push-paths-extra)
replace "$root/.github/workflows/site.yaml" "      - 'docs/**'" $'      - \'docs/**\'\n      - \'fixtures/**\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the pattern" "$(contains "$out" "site.yaml's on.push.paths has fixtures/\*\*, which .github/workflows/ci.yaml's filter site lacks")"

echo "case: a push workflow PUSH_WORKFLOWS names is gone"
root=$(fixture push-workflow-gone)
rm "$root/.github/workflows/site.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'PUSH_WORKFLOWS names .github/workflows/site.yaml for filter site, which is not in the repository')"

# A job that calls a reusable workflow keeps its gate in ci.yaml and gives its steps away, so a
# why cites the step in the file that holds it. ci.yaml's docs job does that with docs-site.yaml.
echo "case: a why cites a step of another workflow file"
root=$(fixture cited-step-elsewhere)
replace "$root/.github/workflows/ci.yaml" "              - 'fixtures/**'" \
  $'              - \'fixtures/**\'\n              - \'.github/workflows/site.yaml\''
replace "$root/.github/scripts/ci_job_reads.py" \
  '    ], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")]),' \
  $'    ], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition"),\n              Read(".github/workflows/site.yaml", at("build", "echo build", ".github/workflows/site.yaml"))]),'
run_check "$root"
check "passes while that file holds the cited step" "$(is "$status" 0)"
replace "$root/.github/workflows/site.yaml" "      - run: echo build" "      - run: echo publish"
run_check "$root"
check "fails once the step is gone" "$(is "$status" 1)"
check "names the step, its job and the file that does not have it" \
  "$(contains "$out" 'a why cites step "echo build" of job build, which .github/workflows/site.yaml does not have')"

echo "case: a package added to a go command the table claims, outside the job's filter"
root=$(fixture appended-package)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test ./cmd/app ./lib ./other"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the package the job now also tests" "$(contains "$out" 'other')"

echo "case: a package added to a claimed go command by its import path, outside the job's filter"
root=$(fixture appended-import-path)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test ./cmd/app ./lib example.com/m/other"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the package the job now also tests" "$(contains "$out" 'other')"

echo "case: a package added to a claimed go command on a continuation line, outside the job's filter"
root=$(fixture appended-continuation)
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          go test ./cmd/app ./lib \\\n            ./other'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the package the job now also tests" "$(contains "$out" 'other')"

echo "case: a -tags appended after a claimed go command's packages compiles a file outside the job's filter"
root=$(fixture appended-tags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test ./cmd/app ./lib -tags probe"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the tags the line builds with" "$(contains "$out" 'it builds with -tags probe, the Run with (none)')"
check "and names the command" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib -tags probe`, which is not the Run')"

echo "case: a Run whose tags or mode disagree with the line it claims"
root=$(fixture run-disagrees)
replace "$root/.github/scripts/ci_job_reads.py" 'text="go test ./cmd/app ./lib"),' 'text="go test ./cmd/app ./lib", tags="probe", mode="build"),'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the tags" "$(contains "$out" 'it builds with -tags (none), the Run with probe')"
check "and the mode" "$(contains "$out" 'its go command is a test run, the Run a build run')"

echo "case: GOFLAGS set in front of a claimed go command compiles a file outside the job's filter"
root=$(fixture inline-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: GOFLAGS=-tags=probe go test ./cmd/app ./lib"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file that closure reads outside the filter" "$(contains "$out" 'other')"

echo "case: GOFLAGS in a claimed go command's step env compiles a file outside the job's filter"
root=$(fixture env-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" $'        working-directory: mod\n        env:\n          GOFLAGS: -tags=probe'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file that closure reads outside the filter" "$(contains "$out" 'other')"

echo "case: a go command this check does not reduce passes when its filter selects its whole module"
root=$(fixture unreduced-whole-module)
replace "$root/.github/workflows/ci.yaml" "              - 'mod/cmd/app/*'" "              - 'mod/**'"
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: GOFLAGS=-tags=probe go test ./cmd/app ./lib"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "counts every Go file of the module" "$(contains "$out" '^app: 8 files, 0 uncovered (filter app)$')"
replace "$root/.github/workflows/ci.yaml" "              - 'mod/**'" "              - 'mod/cmd/app/*'"
run_check "$root"
check "fails once its filter misses one of them" "$(is "$status" 1)"
check "names the command, why it is not reduced and the filter" \
  "$(contains "$out" 'job app step "Test the app" runs `GOFLAGS=-tags=probe go test ./cmd/app ./lib`, which this check does not reduce (it starts with `GOFLAGS=-tags=probe`, not go), so it counts as reading every Go file of the repository and every file under a Go module.s directory; filter app must select them all on any Go change')"

echo "case: a script's go command is held to its pinned text, the whole line"
root=$(fixture script-line)
printf '#!/bin/sh\ngo build ./cmd/app\n' > "$root/build.sh"
replace "$root/.github/scripts/ci_job_reads.py" 'text="go test ./cmd/app ./lib"),' \
  'text="go test ./cmd/app ./lib"), run("mod", ["./cmd/app"], "build.sh: go build ./cmd/app", mode="build", source="build.sh", text="go build ./cmd/app"),'
run_check "$root"
check "passes while the line is the text alone" "$(is "$status" 0)"
printf '#!/bin/sh\ngo build ./cmd/app ./other\n' > "$root/build.sh"
run_check "$root"
check "fails once the line holds more" "$(is "$status" 1)"
check "names the script's command and its line" \
  "$(contains "$out" 'build.sh, which job app runs, runs `go build ./cmd/app`, which this check does not reduce (its line is `go build ./cmd/app ./other`, not the text alone), so it counts as reading every Go file of the repository')"
check "and the file the module adds" "$(contains "$out" '::error file=mod/other/other.go::')"

echo "case: a claimed simple go command whose step runs it in another directory"
root=$(fixture moved-working-directory)
mkdir -p "$root/alt/cmd/app" "$root/alt/lib"
printf 'package main\n\nfunc main() {}\n' > "$root/alt/cmd/app/main.go"
printf 'package lib\n' > "$root/alt/lib/lib.go"
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" "        working-directory: alt"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the directory the step runs in" "$(contains "$out" 'alt')"

echo "case: an earlier step writes GOFLAGS to GITHUB_ENV before a claimed simple go command"
root=$(fixture github-env-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "      - name: Test the app" $'      - name: Set the Go flags\n        run: echo "GOFLAGS=-tags=probe" >> "$GITHUB_ENV"\n      - name: Test the app'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file that closure reads outside the filter, or the GITHUB_ENV write" "$(contains "$out" 'other\|GITHUB_ENV')"

echo "case: go env -w sets GOFLAGS before a claimed simple go command"
root=$(fixture go-env-w-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          go env -w GOFLAGS=-tags=probe\n          go test ./cmd/app ./lib'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file that closure reads outside the filter, or the go env write" "$(contains "$out" 'other\|go env')"

echo "case: a claimed command that cds into another module of the workspace before it runs"
root=$(fixture cd-other-module)
mkdir -p "$root/mod2/cmd/app" "$root/mod2/lib"
printf 'module example.com/m2\n\ngo 1.21\n' > "$root/mod2/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$root/mod2/cmd/app/main.go"
printf 'package lib\n' > "$root/mod2/lib/lib.go"
printf 'go 1.21\n\nuse (\n\t./mod\n\t./mod2\n)\n' > "$root/go.work"
replace "$root/.github/workflows/ci.yaml" "              - 'mod/go.mod'" $'              - \'mod/go.mod\'\n              - \'mod2/go.mod\''
replace "$root/.github/workflows/site.yaml" "      - 'mod/go.mod'" $'      - \'mod/go.mod\'\n      - \'mod2/go.mod\''
replace "$root/.github/workflows/ci.yaml" "              - 'mod/cmd/app/*'" "              - 'mod/**'"
run_check "$root"
check "passes while the command runs in its own module" "$(is "$status" 0)"
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: cd ../mod2 && go test ./cmd/app ./lib"
run_check "$root"
check "fails once it runs in the other module, whose files its filter misses" "$(is "$status" 1)"
check "names a file of that module" "$(contains "$out" 'mod2/')"

echo "case: an unreduced go command reads every file under every Go module's directory, testdata included"
root=$(fixture unreduced-module-tree)
mkdir -p "$root/mod2/x/testdata"
printf 'module example.com/m2\n\ngo 1.21\n' > "$root/mod2/go.mod"
printf 'package x\n' > "$root/mod2/x/x.go"
echo 'data' > "$root/mod2/x/testdata/data.txt"
printf 'go 1.21\n\nuse (\n\t./mod\n\t./mod2\n)\n' > "$root/go.work"
replace "$root/.github/workflows/ci.yaml" "              - 'mod/cmd/app/*'" \
  $'              - \'**/*.go\'\n              - \'**/go.mod\'\n              - \'**/go.sum\'\n              - \'**/go.work\'\n              - \'**/go.work.sum\''
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: GOFLAGS=-tags=probe go test ./cmd/app ./lib"
run_check "$root"
check "fails though its filter selects every Go and module file" "$(is "$status" 1)"
check "names the other module's testdata file" \
  "$(contains "$out" '::error file=mod2/x/testdata/data.txt::app reads mod2/x/testdata/data.txt, which its filter app does not match')"
check "and the command that reads it" \
  "$(contains "$out" 'runs `GOFLAGS=-tags=probe go test ./cmd/app ./lib`, which this check does not reduce (it starts with `GOFLAGS=-tags=probe`, not go), so it counts as reading every Go file of the repository and every file under a Go module.s directory')"

echo "case: a cd on its own line before a simple go command in the same run"
root=$(fixture cd-own-line)
mkdir -p "$root/mod2/cmd/app" "$root/mod2/lib"
printf 'module example.com/m2\n\ngo 1.21\n' > "$root/mod2/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$root/mod2/cmd/app/main.go"
printf 'package lib\n' > "$root/mod2/lib/lib.go"
printf 'go 1.21\n\nuse (\n\t./mod\n\t./mod2\n)\n' > "$root/go.work"
replace "$root/.github/workflows/ci.yaml" "              - 'mod/go.mod'" $'              - \'mod/go.mod\'\n              - \'mod2/go.mod\''
replace "$root/.github/workflows/site.yaml" "      - 'mod/go.mod'" $'      - \'mod/go.mod\'\n      - \'mod2/go.mod\''
replace "$root/.github/workflows/ci.yaml" "              - 'mod/cmd/app/*'" "              - 'mod/**'"
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          cd ../mod2\n          go test ./cmd/app ./lib'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and that it is not its step's whole script" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (it is not its step.s whole run: script, comments and blank lines aside)')"
check "and a file of the module it cds into" "$(contains "$out" '::error file=mod2/')"

echo "case: an export of GOFLAGS on its own line before a simple go command in the same run"
root=$(fixture export-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          export GOFLAGS=-tags=probe\n          go test ./cmd/app ./lib'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and that it is not its step's whole script" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (it is not its step.s whole run: script')"
check "and the file the tags compile in" "$(contains "$out" '::error file=mod/other/other.go::')"

echo "case: a step shell that sets GOFLAGS around a simple go command"
root=$(fixture step-shell)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" $'        working-directory: mod\n        shell: env GOFLAGS=-tags=probe bash -e {0}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and the step's shell" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (step "Test the app" runs under shell: `env GOFLAGS=-tags=probe bash -e {0}`)')"
check "and the file the tags compile in" "$(contains "$out" '::error file=mod/other/other.go::')"

echo "case: a job defaults.run.shell around a simple go command"
root=$(fixture job-default-shell)
replace "$root/.github/workflows/ci.yaml" $'    runs-on: ubuntu-24.04\n    steps:\n      - name: Test the app' \
  $'    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: bash --noprofile --norc -eo pipefail {0}\n    steps:\n      - name: Test the app'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and the job's default shell" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (the job.s defaults.run.shell is `bash --noprofile --norc -eo pipefail {0}`)')"
check "and a file it falls to" "$(contains "$out" '::error file=mod/other/other.go::')"

echo "case: a job container around a simple go command"
root=$(fixture job-container)
replace "$root/.github/workflows/ci.yaml" $'    runs-on: ubuntu-24.04\n    steps:\n      - name: Test the app' \
  $'    runs-on: ubuntu-24.04\n    container: golang:1.21\n    steps:\n      - name: Test the app'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and the container" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (the job runs in a container:)')"
check "and a file it falls to" "$(contains "$out" '::error file=mod/other/other.go::')"

echo "case: a BASH_ENV in a simple go command's env"
root=$(fixture bash-env)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
printf 'export GOFLAGS=-tags=probe\n' > "$root/mod/goflags.sh"
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" $'        working-directory: mod\n        env:\n          BASH_ENV: ./goflags.sh'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and the BASH_ENV" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (step "Test the app".s env sets BASH_ENV)')"
check "and the file the tags compile in" "$(contains "$out" '::error file=mod/other/other.go::')"

echo "case: a -race go command's closure is the one go list -race lists"
root=$(fixture race-closure)
cat > "$root/mod/cmd/app/race_test.go" <<'GO'
//go:build race

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test -race ./cmd/app ./lib"
replace "$root/.github/scripts/ci_job_reads.py" 'text="go test ./cmd/app ./lib"),' 'text="go test -race ./cmd/app ./lib", race=True),'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file the race build compiles in" \
  "$(contains "$out" '::error file=mod/other/other.go::app reads mod/other/other.go, which its filter app does not match.*(go list -deps)')"

echo "case: a -race go command claimed by a Run without race"
root=$(fixture race-mismatch)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test -race ./cmd/app ./lib"
replace "$root/.github/scripts/ci_job_reads.py" 'text="go test ./cmd/app ./lib"),' 'text="go test -race ./cmd/app ./lib"),'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and the race mismatch" \
  "$(contains "$out" 'job app step "Test the app" runs `go test -race ./cmd/app ./lib`, which is not the Run.s (./cmd/app ./lib): it runs with -race, the Run without')"

# exact_step_table <root>: the app gate pins its step's whole script (a loop over the packages) and
# lists the Run that stands for what the loop compiles.
exact_step_table() {
  cat > "$1/.github/scripts/ci_job_reads.py" <<'PY'
from ci_filter_coverage import Read, at, exact, gate, run

RULES = {"mod/lib/lib_test.go": [Read("fixtures/data.json", "lib_test.go reads ../../fixtures/data.json")]}
GATES = [
    gate("app", "app", exact=[
        exact("Test the app", "mod", 'for pkg in ./cmd/app ./lib; do\n  go test -count=1 "$pkg"\ndone', [
            run("mod", ["./cmd/app", "./lib"], at("app", "Test the app") + " (its pinned script): go test of each package of the loop"),
        ]),
    ], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")]),
]
EXCLUDED = {"image": "an image build another check proves"}
PUSH_WORKFLOWS = {"site": ".github/workflows/site.yaml"}
PY
  replace "$1/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" \
    $'        run: |\n          for pkg in ./cmd/app ./lib; do\n            go test -count=1 "$pkg"\n          done'
}

echo "case: a step pinned whole by an exact entry is held to its runs' closure"
root=$(fixture exact-step)
exact_step_table "$root"
run_check "$root"
check "passes while the step holds the pinned script" "$(is "$status" 0)"
check "counts the runs' closure" "$(contains "$out" '^app: 7 files, 0 uncovered (filter app)$')"
printf 'package lib\n\nimport _ "example.com/m/other"\n\nfunc Hello() {}\n' > "$root/mod/lib/lib.go"
run_check "$root"
check "fails once the closure reads a file outside the filter" "$(is "$status" 1)"
check "and names it with the exact entry's run" \
  "$(contains "$out" '::error file=mod/other/other.go::app reads mod/other/other.go.*"Test the app" (its pinned script): go test of each package of the loop (go list -deps)')"

echo "case: a step one character off its pinned script falls to everything"
root=$(fixture exact-step-changed)
exact_step_table "$root"
replace "$root/.github/workflows/ci.yaml" "-count=1" "-count=2"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step and why it falls" \
  "$(contains "$out" 'job app step "Test the app".s pinned script, which this check does not reduce (the step.s script is not the pinned text), so it counts as reading every Go file of the repository and every file under a Go module.s directory; filter app must select them all on any Go change')"
check "and a file it falls to" "$(contains "$out" '::error file=mod/other/other.go::')"

for odd in cr nbsp; do
  echo "case: a hidden command after a ${odd} that Python reads as a comment and bash runs"
  root=$(fixture "hidden-$odd")
  # YAML's own escapes, as a committer would write them: \r and \_ (no-break space).
  if [ "$odd" = cr ]; then char='\r' code=U+000D; else char='\_' code=U+00A0; fi
  replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" \
    "        run: \"go test ./cmd/app ./lib\\n${char}# || go build ./other\""
  run_check "$root"
  check "fails" "$(is "$status" 1)"
  check "names the character" "$(contains "$out" "its step.s script holds the character $code")"
  check "and a file it falls to" "$(contains "$out" '::error file=mod/other/other.go::')"
done

# HOME, XDG_CONFIG_HOME and PATH reach go's env file or binary; PROBE_SETTING stands for any other
# name outside ENV_ALLOWED.
for name in XDG_CONFIG_HOME HOME PATH PROBE_SETTING; do
  echo "case: $name, outside ENV_ALLOWED, in a simple go command's env"
  root=$(fixture "env-$name")
  cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
  mkdir -p "$root/mod/goenv/go"
  printf 'GOFLAGS=-tags=probe\n' > "$root/mod/goenv/go/env"
  replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" $'        working-directory: mod\n        env:\n          '"$name"': ./goenv'
  run_check "$root"
  check "fails" "$(is "$status" 1)"
  check "names the command and $name" \
    "$(contains "$out" "job app step \"Test the app\" runs \`go test ./cmd/app ./lib\`, which this check does not reduce (step \"Test the app\".s env sets $name)")"
  check "and the file the tags compile in" "$(contains "$out" '::error file=mod/other/other.go::')"
done

echo "case: a step of ci.yaml gated on a push, an event the workflow does not run on"
root=$(fixture push-gated-step)
replace "$root/.github/workflows/ci.yaml" "      - name: Test the app" \
  $'      - name: Test the app\n        if: github.event_name == \'push\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the step and the expression" \
  "$(contains "$out" "job app step \"Test the app\" has \`if: github.event_name == 'push'\`, which tests \`github.event_name == 'push'\`")"
check "says where such work belongs instead" \
  "$(contains "$out" 'belongs in .github/workflows/ci-caches.yaml')"

echo "case: a job of ci.yaml gated on main's ref"
root=$(fixture push-gated-job)
replace "$root/.github/workflows/ci.yaml" "    if: needs.changes.outputs.app == 'true' &&" \
  "    if: needs.changes.outputs.app == 'true' && github.ref != 'refs/heads/main' &&"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the comparison" \
  "$(contains "$out" "job app has \`if: needs.changes.outputs.app == 'true' && github.ref != 'refs/heads/main' && (github.event_name != 'merge_group' || github.run_attempt == '1')\`, which tests \`github.ref != 'refs/heads/main'\`")"

echo "case: github.ref_name, and github.ref outside such a comparison, are no push gate"
root=$(fixture push-gate-free)
replace "$root/.github/workflows/ci.yaml" "    if: needs.changes.outputs.app == 'true' &&" \
  "    if: needs.changes.outputs.app == 'true' && github.ref_name != 'main' && !startsWith(github.ref, 'refs/heads/gh-readonly-queue/') &&"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# cache_workflow <root>: the shape .github/workflows/ci-caches.yaml and ci.yaml have here - a fill
# job that looks the family's key up, builds only on a miss and saves the entry, and a restore of
# that same key and those same paths in the job that reads it.
cache_workflow() {
  cat > "$1/.github/workflows/ci-caches.yaml" <<'YAML'
name: CI caches
on:
  push:
    branches: [main]
jobs:
  fill:
    runs-on: ubuntu-24.04
    steps:
      - id: go-app
        uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-app-${{ runner.os }}
          lookup-only: true
      - if: steps.go-app.outputs.cache-hit != 'true'
        run: go test -count=1 ./cmd/app ./lib
        working-directory: mod
      - if: steps.go-app.outputs.cache-hit != 'true'
        uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-app-${{ runner.os }}
YAML
  replace "$1/.github/workflows/ci.yaml" "      - name: Test the app" \
    $'      - id: go-cache\n        uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: |\n            ~/go/pkg/mod\n            ~/.cache/go-build\n          key: go-app-${{ runner.os }}\n          restore-keys: ${{ format(\'go-app-{0}-\', runner.os) }}\n      - name: Test the app'
}

echo "case: a family ci-caches.yaml saves under the key and paths ci.yaml restores"
root=$(fixture cache-round-trip)
cache_workflow "$root"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a restore key that drifted from the key ci-caches.yaml saves"
root=$(fixture cache-key-drift)
cache_workflow "$root"
replace "$root/.github/workflows/ci.yaml" 'key: go-app-${{ runner.os }}' 'key: go-app-${{ runner.os }}-v2'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the family and both keys" \
  "$(contains "$out" 'job fill saves family go-app under key .*, but job app of .github/workflows/ci.yaml restores it under .*-v2')"

echo "case: a restore path the save of that family does not name"
root=$(fixture cache-path-drift)
cache_workflow "$root"
replace "$root/.github/workflows/ci.yaml" $'            ~/.cache/go-build\n' $'            ~/.cache/go-build\n            ~/.cache/legion\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names both path lists" \
  "$(contains "$out" 'job fill saves family go-app from ~/go/pkg/mod, ~/.cache/go-build, but job app of .github/workflows/ci.yaml restores it into ~/go/pkg/mod, ~/.cache/go-build, ~/.cache/legion')"

echo "case: a family ci-caches.yaml fills that ci.yaml restores nowhere"
root=$(fixture cache-unrestored)
cache_workflow "$root"
replace "$root/.github/workflows/ci.yaml" 'key: go-app-${{ runner.os }}' 'key: go-other-${{ runner.os }}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the family nothing restores" \
  "$(contains "$out" 'job fill saves cache family go-app, which no actions/cache/restore of .github/workflows/ci.yaml reads')"

echo "case: a lookup of ci-caches.yaml that downloads the entry"
root=$(fixture cache-lookup-only)
cache_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" "          lookup-only: true"$'\n' ""
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the family" \
  "$(contains "$out" "job fill's actions/cache/restore of family go-app is not \`lookup-only: true\`")"

echo "case: a lookup of ci-caches.yaml with restore-keys"
root=$(fixture cache-lookup-prefix)
cache_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" "          lookup-only: true" $'          lookup-only: true\n          restore-keys: go-app-'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the family" \
  "$(contains "$out" "job fill's actions/cache/restore of family go-app has restore-keys")"

summary "check-ci-filters.sh holds each filter to its job's Go closure, its rules and its table"
