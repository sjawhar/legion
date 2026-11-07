#!/usr/bin/env bash
# Tests for `go-test-times.py`: the reporting tool the envoy-go measurement is read through
# (dispatch://INFRA-1075). Every acceptance figure of that work — each leaf test's seconds, each
# package's seconds, and which tests the head lost against the baseline — is this script's
# output, so its leaf rule, its Elapsed attribution and its comparison are held here.
#
# Each case writes a miniature `go test -json` stream under a temporary directory and runs the
# real script over it.
#
# Run from anywhere: .github/scripts/go-test-times.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$script_dir/go-test-times.py"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# event <action> <package> <test> <elapsed>: one go test -json line. An empty test is a
# package-level event.
event() {
  if [ -z "$3" ]; then
    printf '{"Action":"%s","Package":"%s","Elapsed":%s}\n' "$1" "$2" "$4"
  else
    printf '{"Action":"%s","Package":"%s","Test":"%s","Elapsed":%s}\n' "$1" "$2" "$3" "$4"
  fi
}

run() {
  set +e
  out=$(python3 "$script" "$@" 2>&1)
  status=$?
  set -e
}

# ---------------------------------------------------------------- the leaf rule and the times
#
# TestParent has two subtests, so it is no leaf and its 9 s do not appear in the leaf table; its
# subtests do. TestAlone has no subtest and is a leaf at 2.5 s. TestParent/Deep/Deeper shows that
# the rule is "no test names me as its prefix", at any depth, not "has no slash".
stream="$work/one.json"
{
  event run pkg/a TestParent 0
  event run pkg/a TestParent/Quick 0
  event pass pkg/a TestParent/Quick 0.01
  event run pkg/a TestParent/Deep 0
  event run pkg/a TestParent/Deep/Deeper 0
  event pass pkg/a TestParent/Deep/Deeper 3.5
  event pass pkg/a TestParent/Deep 3.5
  event pass pkg/a TestParent 9
  event pass pkg/a TestAlone 2.5
  event pass pkg/a "" 9.6
  event pass pkg/b TestFast 0.2
  event pass pkg/b "" 70
  printf 'ok  \tpkg/b\t70.0s\n'
} > "$stream"

run "$stream"
check "it reads the stream" "$(is "$status" 0)"
check "it counts both packages" "$(contains "$out" 'packages: 2, summing to 79.6 s')"
check "it counts the leaf tests, the parent left out" "$(contains "$out" 'leaf tests: 4')"
check "a leaf test shows its own Elapsed" "$(contains "$out" '2.50  pkg/a TestAlone')"
check "the deepest subtest is a leaf" "$(contains "$out" '3.50  pkg/a TestParent/Deep/Deeper')"
check "an intermediate parent is no leaf" "$(is "$(grep -c 'pkg/a TestParent/Deep  ' <<<"$out")" 0)"
check "the parent test is no leaf" "$(is "$(grep -c 'pkg/a TestParent  ' <<<"$out")" 0)"
check "a package shows its own Elapsed" "$(contains "$out" '70.0  pkg/b')"
check "a line that is not JSON is passed over" "$(is "$status" 0)"

# ---------------------------------------------------------------- the bars
run --bars "$stream"
check "--bars reads the stream" "$(is "$status" 0)"
check "--bars counts the packages over 60 s" "$(contains "$out" 'over the 60 s package bar: 1')"
check "--bars counts the leaf tests over 1 s" "$(contains "$out" 'over the 1 s leaf-test bar: 2')"
check "--bars shows the package over the bar" "$(contains "$out" '70.0  pkg/b')"
check "--bars leaves out a package under the bar" "$(is "$(grep -c '9.6  pkg/a' <<<"$out")" 0)"
check "--bars leaves out a leaf under the bar" "$(is "$(grep -c 'pkg/b TestFast' <<<"$out")" 0)"

run --bars --package-bar 5 --leaf-bar 3 "$stream"
check "the bars are the caller's" "$(contains "$out" 'over the 5 s package bar: 2')"
check "the leaf bar is the caller's" "$(contains "$out" 'over the 3 s leaf-test bar: 1')"

# ---------------------------------------------------------------- a directory and a zip
mkdir -p "$work/dir"
cp "$stream" "$work/dir/unit-api.json"
event pass pkg/c TestInTheOtherFile 1.5 > "$work/dir/natslive.json"
event pass pkg/c "" 1.6 >> "$work/dir/natslive.json"
run "$work/dir"
check "a directory reads every .json under it" "$(contains "$out" 'packages: 3, summing to 81.2 s')"

# python3's own zipfile, so the case needs no `zip` binary on the runner.
(cd "$work/dir" && python3 -m zipfile -c "$work/artifact.zip" unit-api.json natslive.json)
run "$work/artifact.zip"
check "a .zip reads every .json in it" "$(contains "$out" 'packages: 3, summing to 81.2 s')"

# ---------------------------------------------------------------- the comparison
base="$work/base.json"
head="$work/head.json"
{
  event pass pkg/a TestKept 1
  event pass pkg/a TestBroken 1
  event pass pkg/a TestNowSkipped 1
  event pass pkg/a TestRemoved 1
  event skip pkg/a TestAlreadySkipped 0
  event pass pkg/a "" 4
} > "$base"
{
  event pass pkg/a TestKept 1
  event fail pkg/a TestBroken 1
  event skip pkg/a TestNowSkipped 0
  event skip pkg/a TestAlreadySkipped 0
  event pass pkg/a TestAddedOnMain 1
  event pass pkg/a "" 3
} > "$head"

run --compare "$base" "$head"
check "a disagreement exits 1" "$(is "$status" 1)"
check "a test that stopped passing is named" "$(contains "$out" 'passed at the baseline and does not pass at the head: 3')"
check "a failing test is named with its action" "$(contains "$out" 'pkg/a TestBroken  \[fail\]')"
check "a removed test is named as absent" "$(contains "$out" 'pkg/a TestRemoved  \[absent\]')"
check "a newly skipped test is counted once" "$(contains "$out" 'skips at the head and did not at the baseline: 1')"
check "a test skipped at both ends is no finding" "$(is "$(grep -c 'TestAlreadySkipped' <<<"$out")" 0)"
check "a test only the head holds is attributed, not counted against the change" \
  "$(contains "$out" "at the head only (another change's, or renamed): 1")"
check "the head-only test is named" "$(contains "$out" 'pkg/a TestAddedOnMain')"

run --compare "$base" "$base"
check "two equal streams exit 0" "$(is "$status" 0)"
check "two equal streams find no regression" "$(contains "$out" 'does not pass at the head: 0')"

# ---------------------------------------------------------------- refusals
run --compare "$base" "$head" "$stream"
check "--compare with another path is refused" "$(is "$status" 2)"
check "the refusal says why" "$(contains "$out" 'pass no other path')"

run "$work/absent.json"
check "a path that does not exist is refused" "$(is "$status" 1)"
check "the refusal names the path" "$(contains "$out" 'does not exist')"

printf 'not a stream\n' > "$work/empty.json"
run "$work/empty.json"
check "a stream with no result is refused" "$(is "$status" 1)"
check "that refusal says so" "$(contains "$out" 'no test or package result')"

run
check "no path at all is refused" "$(is "$status" 2)"

summary "go-test-times.py reads a go test -json stream, ranks it against the bars, and compares two"
