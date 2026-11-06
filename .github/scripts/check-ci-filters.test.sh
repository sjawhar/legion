#!/usr/bin/env bash
# Tests for `check-ci-filters.sh`: every ci.yaml filter must match every file its job reads, the
# Go closures recomputed with go list (with -race for a -race command), the table of go commands
# must say what ci.yaml says, a claimed go command the check does not reduce (another form, a
# step that is more than the go line, a shell, a container, a BASH_ENV) and a step off its exact
# entry's pinned script count as reading every Go file of the repository and every file under a
# Go module's directory, a why that cites a step must name a workflow file that still holds it,
# CI_OK_SELECTED_BY must map every job changes gates, it and CI_OK_PULL_REQUEST_ONLY must say what
# each job's if: tests, every job but changes and those CI_OK_EXEMPT names must end with the ci-ok
# report step, the ci-ok job must need every job but itself and the exempt ones, each check
# main's ruleset requires must be the ci.yaml job of that name and no other job may carry one,
# and a PUSH_WORKFLOWS workflow's
# on.push.paths must equal its filter, no `if:` of ci.yaml may test for a push event or compare
# github.ref with a branch ref, every job's if: but those two jobs' must hold the term that skips
# it on a re-run attempt of a merge-group run and theirs must not, ci-caches.yaml's save of a
# cache family must carry
# the key and the paths ci.yaml's restore of that family names, its own restores being lookups,
# ci_job_reads.py's WORKSPACES must name exactly the workspaces of the root package.json, and no
# workflow but ci.yaml may run on a merge group.
#
# Each case builds a miniature repository under a temporary directory - a go.work binding one
# module, a ci.yaml whose `changes` job gates one job, a root package.json with one workspace, and
# a ci_job_reads.py describing them - and symlinks the real script, its engine
# (ci_filter_coverage.py), the rules of ci.yaml it runs
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
# request, and both end with the ci-ok report step. pr-title and ci-ok are the two jobs named for
# a check main's ruleset requires: CI_OK_EXEMPT holds both outside ci-ok's accounting, so neither
# carries the report step and neither may carry the term that skips a job on a re-run attempt of
# a merge-group run. The site filter gates no job: site.yaml, which
# PUSH_WORKFLOWS pairs with it, runs on a push whose paths are that filter flattened. The root
# package.json has the one workspace packages/widget, which WORKSPACES names, since rule 13 holds
# those two lists to each other; no job here runs Bun, so that manifest is nothing the app gate
# reads. Echoes the fixture root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/mod/cmd/app" "$root/mod/lib" \
    "$root/mod/other" "$root/fixtures" "$root/docs" "$root/packages/widget"
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
  printf '{\n  "name": "fixture",\n  "workspaces": ["packages/widget"]\n}\n' > "$root/package.json"
  printf '{\n  "name": "widget"\n}\n' > "$root/packages/widget/package.json"
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
WORKSPACES = ["packages/widget"]
PY
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: [pull_request]
env:
  CI_OK_SELECTED_BY: '{"app": "app", "image": "image"}'
  CI_OK_PULL_REQUEST_ONLY: '["image"]'
  CI_OK_EXEMPT: '["ci-ok", "pr-title"]'
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
      - name: Report ci-ok when this job decides a merge-group run
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
      - name: Report ci-ok when this job decides a merge-group run
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          changes: ${{ toJSON(needs.changes) }}
          job-status: ${{ job.status }}
  pr-title:
    if: github.event_name == 'pull_request' || github.event_name == 'merge_group'
    runs-on: ubuntu-24.04
    steps:
      - name: Check the pull request's text
        run: echo text
  ci-ok:
    needs: [changes, app, image]
    if: ${{ always() }}
    runs-on: ubuntu-24.04
    steps:
      - name: Judge every job of this run
        run: echo verdict
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
  "$(contains "$out" "CI_OK_SELECTED_BY does not map app, whose if: tests needs.changes.outputs.app, so ci-ok.sh does not know what selects it and fails its result; add \"app\": \"app\"")"

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
replace "$root/.github/workflows/ci.yaml" "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" \
  "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\", \"cancel\"]'"
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

echo "case: a job named for the other required check"
root=$(fixture pr-title-job)
replace "$root/.github/workflows/ci.yaml" $'\n  image:\n' $'\n  image:\n    name: pr-title\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the check" \
  "$(contains "$out" "job image is named pr-title, a check main's ruleset requires, and is not the pr-title job")"

echo "case: a job named for a required check renamed away from the check"
root=$(fixture pr-title-renamed)
replace "$root/.github/workflows/ci.yaml" $'\n  pr-title:\n' $'\n  pr-title:\n    name: PR title\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the check would be reported by nothing" \
  "$(contains "$out" "job pr-title carries \`name: PR title\`, so its check run is called PR title and pr-title, a check main's ruleset requires, is reported by nothing")"

echo "case: a job named for a required check holding the re-run term"
root=$(fixture pr-title-rerun-guard)
replace "$root/.github/workflows/ci.yaml" \
  "    if: github.event_name == 'pull_request' || github.event_name == 'merge_group'" \
  "    if: (github.event_name != 'merge_group' || github.run_attempt == '1')"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says a skipped required check passes" \
  "$(contains "$out" "job pr-title is named for a check main's ruleset requires and its if: holds a term that skips it on a re-run attempt of a merge-group run")"

echo "case: the verdict job holding the re-run term"
root=$(fixture ci-ok-rerun-guard)
replace "$root/.github/workflows/ci.yaml" "    if: \${{ always() }}" \
  "    if: \${{ always() && (github.event_name != 'merge_group' || github.run_attempt == '1') }}"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the verdict and what a skip would hand the group" \
  "$(contains "$out" "job ci-ok is named for a check main's ruleset requires and its if: holds a term that skips it on a re-run attempt of a merge-group run: a skipped job's check run is a passing required check, so that attempt would merge on ci-ok concluded by nothing")"

echo "case: a job CI_OK_EXEMPT does not name the pr-title job"
root=$(fixture pr-title-not-exempt)
replace "$root/.github/workflows/ci.yaml" "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" "CI_OK_EXEMPT: '[\"ci-ok\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "asks it for the report step no exempt job carries" \
  "$(contains "$out" "job pr-title's last step is not \`uses: ./.github/actions/ci-ok-report\`")"
check "and asks the verdict to need it" \
  "$(contains "$out" "job ci-ok does not need pr-title, so it reaches the verdict while that job is still running and says nothing about it; add pr-title to its needs:")"

echo "case: the file has no pr-title job"
root=$(fixture pr-title-absent)
replace "$root/.github/workflows/ci.yaml" $'  pr-title:\n    if: github.event_name == \'pull_request\' || github.event_name == \'merge_group\'\n    runs-on: ubuntu-24.04\n    steps:\n      - name: Check the pull request\'s text\n        run: echo text\n' ""
replace "$root/.github/workflows/ci.yaml" "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" "CI_OK_EXEMPT: '[\"ci-ok\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the check nothing would carry" \
  "$(contains "$out" "this file has no pr-title job, a check main's ruleset requires")"

echo "case: the ci-ok job does not need a job of the file"
root=$(fixture ci-ok-needs-missing)
replace "$root/.github/workflows/ci.yaml" "    needs: [changes, app, image]" "    needs: [changes, app]"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job it would judge nothing of" \
  "$(contains "$out" "job ci-ok does not need image, so it reaches the verdict while that job is still running and says nothing about it; add image to its needs:")"

echo "case: the ci-ok job needs a job CI_OK_EXEMPT names"
root=$(fixture ci-ok-needs-exempt)
cancel_job "$root"
replace "$root/.github/workflows/ci.yaml" "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" \
  "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\", \"cancel\"]'"
replace "$root/.github/workflows/ci.yaml" "    needs: [changes, app, image]" "    needs: [changes, app, image, cancel]"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exempt job and why the verdict cannot read it" \
  "$(contains "$out" "job ci-ok needs cancel, but CI_OK_EXEMPT holds it outside ci-ok's accounting, and ci-ok.sh fails a result for a job CI_OK_SELECTED_BY does not map; drop it from its needs:")"

echo "case: CI_OK_EXEMPT does not name the ci-ok job"
root=$(fixture ci-ok-not-exempt)
replace "$root/.github/workflows/ci.yaml" "  CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" "  CI_OK_EXEMPT: '[\"pr-title\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the verdict reports nothing to itself" \
  "$(contains "$out" "CI_OK_EXEMPT does not name ci-ok, which reports no verdict to itself")"
check "and asks it for the report step no exempt job carries" \
  "$(contains "$out" "job ci-ok's last step is not \`uses: ./.github/actions/ci-ok-report\`")"

echo "case: the file has no ci-ok job"
# The job is the last of the fixture, and `replace` reads the file without its trailing newline,
# so the block to cut starts at the newline before it and ends at its last line.
root=$(fixture ci-ok-absent)
replace "$root/.github/workflows/ci.yaml" $'\n  ci-ok:\n    needs: [changes, app, image]\n    if: ${{ always() }}\n    runs-on: ubuntu-24.04\n    steps:\n      - name: Judge every job of this run\n        run: echo verdict' ""
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the check nothing would carry" \
  "$(contains "$out" "this file has no ci-ok job, the check main's ruleset requires")"

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
WORKSPACES = ["packages/widget"]
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

echo "case: a workspace of the root manifest that WORKSPACES does not name"
root=$(fixture workspace-untabled)
replace "$root/.github/scripts/ci_job_reads.py" 'WORKSPACES = ["packages/widget"]' 'WORKSPACES = []'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the workspace and the table" \
  "$(contains "$out" "::error file=package.json::workspaces names packages/widget, which .github/scripts/ci_job_reads.py's WORKSPACES does not")"

echo "case: a WORKSPACES entry the root manifest does not list"
root=$(fixture workspace-unlisted)
replace "$root/.github/scripts/ci_job_reads.py" 'WORKSPACES = ["packages/widget"]' \
  'WORKSPACES = ["packages/widget", "packages/gone"]'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the entry and the manifest" \
  "$(contains "$out" "::error file=.github/scripts/ci_job_reads.py::WORKSPACES names packages/gone, which the root manifest's workspaces does not")"

echo "case: every filter of a job that compiles or reads another package's files selects them"
# Rule 14 holds the root manifest's workspaces equal to WORKSPACES, which makes every Bun job
# declare a read of each workspace's manifest. It says nothing about a workspace's sources, so a
# package can be compiled by a job whose filter never selects it, and a test can read a file of
# another package that no filter of its job names. Rule 1 does not see either, because for the
# Bun gates the declared reads are hand-kept beside the filter and the two go short together.
# Each pair below was measured rather than reasoned: COMPILERS with
# `bunx tsc --listFilesOnly` in the package, or `bun build --sourcemap=external` of the entry
# the job runs, and READS by following the relative path the named test resolves. A filter that
# gates such a job and does not select what it compiles or reads leaves the change that breaks
# it starting no job at all, on the pull request and again on the merge group that lands it.
#
# This list is hand-kept, like the reads it guards: it holds the pairs somebody has measured,
# and nothing derives it, so a job added later is not held until its pair is written here.
uncovered=$(PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 - "$script_dir/../workflows/ci.yaml" <<'PY'
import sys, yaml, workflow_paths as wp
from pathlib import Path

SOURCE = "packages/broker-client/src/token-client.ts"

# Each job's own tsc program, or the runtime closure of the entry it runs, holds the seven
# sources of packages/broker-client, which packages/envoy-client/src/broker-credential.ts:21-22
# imports and whose exports map every entry to ./src/<module>.ts.
COMPILERS = (
    ("claude-envoy", "claude_envoy"),
    ("envoy-client", "envoy_client"),
    ("envoy-session-stall", "envoy_session_stall"),
    ("pi-envoy", "pi_envoy"),
    ("pi-legion", "pi_legion"),
    ("pi-shared", "pi_shared"),
)

# A file of another package that the job's own tests compile, read by relative path, or run as a
# program, with the test that reaches it. The last class is the one no derivation sees: a spawn
# carries no import, so `bunx tsc --listFilesOnly` of the job's own program lists none of it
# (measured: zero claude-envoy files in packages/pi-legion's program), and check-ts-filters.sh
# cannot hold the filter to it.
READS = (
    ("envoy_client", "packages/envoy/internal/brokerclient/testdata/wire.json",
     "packages/broker-client/src/__tests__/wire.test.ts:25-26"),
    ("contracts", "packages/envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json",
     "packages/contracts/src/dispatch-block-ref.test.ts:98-102"),
    ("contracts", "packages/daemon/internal/intake/testdata/dispatch/server-started.json",
     "packages/contracts/src/dispatch-api.test.ts:293-300"),
    ("dispatch_ui", "packages/envoy/internal/dispatch/pmdoc/schema/block-node-types.json",
     "packages/proof-editor/tests/ask-concurrent-edit.test.ts:269-275"),
    ("claude_envoy", "packages/envoy-client/test/stall-proxy.ts",
     "packages/claude-envoy/tests/envoy-channel-stall.test.ts:9-11"),
    ("pi_legion", "packages/dispatch/e2e/harness-processes.ts", "packages/pi-legion's tsc program"),
    ("pi_legion", "packages/dispatch/e2e/harness-broker.ts", "packages/pi-legion's tsc program"),
    ("pi_legion", "packages/dispatch/e2e/harness-real-broker.ts", "packages/pi-legion's tsc program"),
    ("pi_legion", "packages/dispatch/e2e/harness-signin.ts", "packages/pi-legion's tsc program"),
    ("pi_legion", "packages/claude-envoy/scripts/build.ts",
     "packages/pi-legion/src/skills-guard.test.ts:24,275, which spawns that builder and asserts an "
     "exact file list over what it writes"),
    ("pi_legion", "packages/claude-envoy/bin/envoy-channel.ts",
     "that builder's BUNDLE_ENTRYPOINTS (packages/claude-envoy/scripts/build.ts:41-46), bundled into "
     "the tree the same test asserts over"),
    ("pi_legion", "packages/envoy-client/bin/dispatch.ts",
     "the same BUNDLE_ENTRYPOINTS entry, reached from another package again"),
)

doc = yaml.safe_load(Path(sys.argv[1]).read_text())
step = next(s for s in doc["jobs"]["changes"]["steps"] if s.get("id") == "filter")
filters = yaml.safe_load(step["with"]["filters"])


def flat(entry):
    return [entry] if isinstance(entry, str) else [p for x in entry for p in flat(x)]


def covers(name, path):
    return wp.Filter(flat(filters[name])).covers(path)


bad = [f"{job} ({name}) compiles {SOURCE}" for job, name in COMPILERS if not covers(name, SOURCE)]
bad += [f"{name} does not select {path}, which {who} reads"
        for name, path, who in READS if not covers(name, path)]
print("; ".join(bad))
PY
)
check "no filter gates a job that compiles or reads files it does not select" \
  "$(is "$uncovered" "")"

# Rule 13 reads the root manifest as check-bun-workspace-manifests.sh reads it, at every shape:
# a list it cannot read is named rather than taken for an empty one, which would hold WORKSPACES
# to nothing, and the two spellings of a workspace are one workspace.
echo "case: a root manifest rule 13 cannot read"
root=$(fixture workspace-manifest-gone)
rm "$root/package.json"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names what the table is then held to" \
  "$(contains "$out" "::error file=package.json::the root manifest is missing, so .github/scripts/ci_job_reads.py's WORKSPACES is held to nothing")"

echo "case: a root manifest that is not valid JSON"
root=$(fixture workspace-manifest-unreadable)
replace "$root/package.json" '"workspaces": ["packages/widget"]' '"workspaces": ["packages/widget",'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the manifest and what it could not read" \
  "$(contains "$out" "::error file=package.json::the root manifest is not valid JSON (")"

echo "case: a workspaces that is not a list"
root=$(fixture workspace-not-a-list)
replace "$root/package.json" '"workspaces": ["packages/widget"]' '"workspaces": "packages/widget"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names what workspaces must be for the table to be held to it" \
  "$(contains "$out" "::error file=package.json::workspaces must be an array of workspace paths for .github/scripts/ci_job_reads.py's WORKSPACES to be held to it")"

echo "case: a workspaces holding an entry that is no workspace path"
root=$(fixture workspace-entry-unreadable)
replace "$root/package.json" '["packages/widget"]' '["packages/widget", 7]'
run_check "$root"
check "fails on a non-string entry" "$(is "$status" 1)"
check "names what workspaces must be" \
  "$(contains "$out" "::error file=package.json::workspaces must be an array of workspace paths")"
replace "$root/package.json" '["packages/widget", 7]' '["packages/widget", ""]'
run_check "$root"
check "fails on an empty entry too" "$(is "$status" 1)"
check "and names the same thing" \
  "$(contains "$out" "::error file=package.json::workspaces must be an array of workspace paths")"

echo "case: an empty workspaces array against a table that names none"
root=$(fixture workspace-none)
replace "$root/package.json" '["packages/widget"]' '[]'
replace "$root/.github/scripts/ci_job_reads.py" 'WORKSPACES = ["packages/widget"]' 'WORKSPACES = []'
run_check "$root"
check "passes: a root declaring no workspace agrees with a table naming none" "$(is "$status" 0)"

echo "case: a trailing slash on either side names the same workspace"
root=$(fixture workspace-trailing-slash)
replace "$root/package.json" '["packages/widget"]' '["packages/widget/"]'
run_check "$root"
check "passes with the slash in the manifest" "$(is "$status" 0)"
replace "$root/package.json" '["packages/widget/"]' '["packages/widget"]'
replace "$root/.github/scripts/ci_job_reads.py" 'WORKSPACES = ["packages/widget"]' 'WORKSPACES = ["packages/widget/"]'
run_check "$root"
check "and with it in the table" "$(is "$status" 0)"

echo "case: the merge-group trigger belongs to ci.yaml alone"
# ci-ok-report.sh POSTs the failing ci-ok of a merge-group run the reporting job's own result
# decides, and a POSTed check run joins the Actions App's first check suite at that commit
# rather than the posting run's, so the queue reads that POST only while ci.yaml is the one
# workflow a merge group starts.
root=$(fixture merge-group-only)
replace "$root/.github/workflows/ci.yaml" "on: [pull_request]" $'on:\n  pull_request:\n  merge_group:'
run_check "$root"
check "passes with ci.yaml itself on merge_group" "$(is "$status" 0)"
cat > "$root/.github/workflows/scan.yaml" <<'YAML'
name: Scan
on:
  pull_request:
  merge_group:
jobs:
  scan:
    runs-on: ubuntu-24.04
    steps:
      - run: echo scan
YAML
run_check "$root"
check "fails once a second workflow runs on it" "$(is "$status" 1)"
check "naming the file it found and the one workflow that may" \
  "$(contains "$out" "::error file=.github/workflows/scan.yaml::.github/workflows/scan.yaml runs on merge_group, which only .github/workflows/ci.yaml may")"
check "the early POST a second suite breaks" \
  "$(contains "$out" "a POSTed check run joins the Actions App.s first check suite at that commit rather than the posting run.s")"
check "and the measurement behind it" \
  "$(contains "$out" 'measured on pull request #2847, which the queue refused to enqueue')"
mv "$root/.github/workflows/scan.yaml" "$root/.github/workflows/scan.yml"
run_check "$root"
check "and reads the .yml spelling GitHub reads too" \
  "$(contains "$out" "::error file=.github/workflows/scan.yml::")"

summary "check-ci-filters.sh holds each filter to its job's Go closure, its rules and its table"
