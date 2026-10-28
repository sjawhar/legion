#!/usr/bin/env bash
# Tests for `check-ci-filters.sh`: every ci.yaml filter must match every file its job reads, the
# Go closures recomputed with go list, the table of go commands must say what ci.yaml says, a
# claimed go command the check does not reduce counts as reading its whole module, ci-ok's
# SELECTED_BY, PULL_REQUEST_ONLY and NOT_ON_PUSH must say what each needed job's if: tests, and a
# PUSH_WORKFLOWS workflow's on.push.paths must equal its filter.
#
# Each case builds a miniature repository under a temporary directory - a go.work binding one
# module, a ci.yaml whose `changes` job gates one job, and a ci_job_reads.py describing it - and
# symlinks the real script, its engine (ci_filter_coverage.py) and the path model it imports
# (workflow_paths.py) into it, so the code under test is the code CI runs. It needs go and a
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
# request, and ci-ok needs both. The site filter gates no job: site.yaml, which PUSH_WORKFLOWS
# pairs with it, runs on a push whose paths are that filter flattened. Echoes the fixture root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/mod/cmd/app" "$root/mod/lib" \
    "$root/mod/other" "$root/fixtures" "$root/docs"
  for linked in check-ci-filters.sh ci_filter_coverage.py workflow_paths.py; do
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
jobs:
  changes:
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
    if: needs.changes.outputs.app == 'true'
    runs-on: ubuntu-24.04
    steps:
      - name: Test the app
        run: go test ./cmd/app ./lib
        working-directory: mod
  image:
    needs: changes
    if: github.event_name == 'pull_request' && needs.changes.outputs.image == 'true'
    runs-on: ubuntu-24.04
    steps:
      - name: Build
        run: go build ./...
  ci-ok:
    needs: [changes, app, image]
    if: ${{ !cancelled() }}
    runs-on: ubuntu-24.04
    steps:
      - name: Condense
        env:
          SELECTED_BY: '{"app": "app", "image": "image"}'
          PULL_REQUEST_ONLY: '["image"]'
          NOT_ON_PUSH: '[]'
        run: .github/scripts/ci-ok.sh
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

echo "case: a go command that cannot run fails rather than reading an empty closure"
root=$(fixture broken-workspace)
printf 'go 1.21\n\nuse ./missing\n' > "$root/go.work"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the closure is unknown" "$(contains "$out" 'so the closure of .* is unknown')"

echo "case: ci-ok's SELECTED_BY misses a job ci-ok needs"
root=$(fixture ci-ok-missing)
replace "$root/.github/workflows/ci.yaml" "SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "SELECTED_BY: '{\"image\": \"image\"}'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the output to map it to" \
  "$(contains "$out" "ci-ok's SELECTED_BY does not map app, which ci-ok needs, so ci-ok.sh fails it; add \"app\": \"app\"")"

echo "case: ci-ok's SELECTED_BY maps a job to an output its if: does not test"
root=$(fixture ci-ok-wrong)
replace "$root/.github/workflows/ci.yaml" "SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "SELECTED_BY: '{\"app\": \"image\", \"image\": \"image\"}'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the wrong output and the right one" \
  "$(contains "$out" "ci-ok's SELECTED_BY maps app to image, but its if: tests needs.changes.outputs.app")"

echo "case: ci-ok's PULL_REQUEST_ONLY misses a job whose if: admits only pull_request"
root=$(fixture ci-ok-pull-request-only)
replace "$root/.github/workflows/ci.yaml" "PULL_REQUEST_ONLY: '[\"image\"]'" "PULL_REQUEST_ONLY: '[]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job" "$(contains "$out" "ci-ok's PULL_REQUEST_ONLY lacks image, whose if: admits only pull_request")"

echo "case: ci-ok's NOT_ON_PUSH lists a job whose if: admits a push"
root=$(fixture ci-ok-not-on-push-wrong)
replace "$root/.github/workflows/ci.yaml" "NOT_ON_PUSH: '[]'" "NOT_ON_PUSH: '[\"app\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the term its if: lacks" \
  "$(contains "$out" "ci-ok's NOT_ON_PUSH lists app, whose if: has no github.event_name != 'push' term")"

echo "case: ci-ok's NOT_ON_PUSH misses a job whose if: admits no push"
root=$(fixture ci-ok-not-on-push-missing)
replace "$root/.github/workflows/ci.yaml" "if: needs.changes.outputs.app == 'true'" \
  "if: github.event_name != 'push' && needs.changes.outputs.app == 'true'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job" "$(contains "$out" "ci-ok's NOT_ON_PUSH lacks app, whose if: admits no push, so ci-ok.sh fails its skip on a push")"

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
  "$(contains "$out" 'job app step "Test the app" runs `GOFLAGS=-tags=probe go test ./cmd/app ./lib`, which this check does not reduce (it starts with `GOFLAGS=-tags=probe`, not go), so it counts as reading every Go file of module mod; filter app must select it on any Go change')"

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
  "$(contains "$out" 'build.sh, which job app runs, runs `go build ./cmd/app`, which this check does not reduce (its line is `go build ./cmd/app ./other`, not the text alone), so it counts as reading every Go file of module mod')"
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

summary "check-ci-filters.sh holds each filter to its job's Go closure, its rules and its table"
