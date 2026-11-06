#!/usr/bin/env bash
# Tests for `go-tagged-tests.sh`: for a build tag and a directory, the tests that tag adds.
#
# Each case builds a miniature package under a temporary directory and runs the real script over
# it, so the script under test is the file CI runs. One case runs it over this repository's own
# tree, so the fixtures cannot quietly drift from the source they stand for.
#
# Run from anywhere: .github/scripts/go-tagged-tests.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../.." && pwd)
lister="$script_dir/go-tagged-tests.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: an empty package directory. Echoes its path.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root"
  echo "$root"
}

# gofile <path> <constraint> <declaration>...: a Go file carrying that `//go:build` constraint
# (empty for a file with no constraint at all), then one line per declaration.
gofile() {
  local path=$1 constraint=$2
  shift 2
  : >"$path"
  if [ -n "$constraint" ]; then
    printf '//go:build %s\n\n' "$constraint" >>"$path"
  fi
  printf 'package main\n\n' >>"$path"
  printf '%s\n' "$@" >>"$path"
}

# run_lister <argument>...: sets out, err and status, keeping the two streams apart, because what
# a refusal must print on standard output is nothing.
run_lister() {
  set +e
  out=$("$lister" "$@" 2>"$work/stderr")
  status=$?
  set -e
  err=$(cat "$work/stderr")
}

echo "case: the bare form, over several files"
root=$(fixture bare)
gofile "$root/alpha_test.go" demo \
  'func TestAlpha(t *testing.T) {}' \
  'func TestBeta(t *testing.T) {}' \
  'func (h *harness) TestMethodIsNoTest(t *testing.T) {}'
gofile "$root/gamma_test.go" '  demo  ' \
  'func TestGamma(t *testing.T) {}'
run_lister demo "$root"
check "exits 0" "$(is "$status" 0)"
check "prints the anchored alternation of exactly their tests" \
  "$(is "$out" '^(TestAlpha|TestBeta|TestGamma)$')"
check "says nothing on standard error" "$(is "$err" "")"

echo "case: the names come back sorted"
root=$(fixture sorted)
gofile "$root/out_of_order_test.go" demo \
  'func TestZulu(t *testing.T) {}' \
  'func TestAlpha(t *testing.T) {}' \
  'func TestMike(t *testing.T) {}'
run_lister demo "$root"
check "exits 0" "$(is "$status" 0)"
check "sorted, not in file order" "$(is "$out" '^(TestAlpha|TestMike|TestZulu)$')"

echo "case: TestMain is excluded, and a name that only begins with it is not"
root=$(fixture testmain)
gofile "$root/main_test.go" demo \
  'func TestMain(m *testing.M) {}' \
  'func TestMainlineStaysIn(t *testing.T) {}'
run_lister demo "$root"
check "exits 0" "$(is "$status" 0)"
check "the entry point is gone, the test is not" "$(is "$out" '^(TestMainlineStaysIn)$')"

echo "case: go's own rule for what the name after Test may start with"
# go runs a TestXxx whose Xxx does not start with a lower-case letter, so Testify is an ordinary
# function and naming it in a -run would be a dead alternative.
root=$(fixture namerule)
gofile "$root/name_rule_test.go" demo \
  'func Test(t *testing.T) {}' \
  'func Test_one(t *testing.T) {}' \
  'func Testify(t *testing.T) {}' \
  'func TestifyHelper(t *testing.T) {}'
run_lister demo "$root"
check "exits 0" "$(is "$status" 0)"
check "the bare name and the underscore one are tests, the lower-case ones are not" \
  "$(is "$out" '^(Test|Test_one)$')"

echo "case: a fuzz target and an example of a tagged file"
root=$(fixture fuzz)
gofile "$root/fuzz_test.go" demo \
  'func FuzzTheParser(f *testing.F) {}' \
  'func ExampleTheClient() {}' \
  'func TestThePlainTest(t *testing.T) {}'
run_lister demo "$root"
check "exits 0" "$(is "$status" 0)"
check "all three kinds are named" \
  "$(is "$out" '^(ExampleTheClient|FuzzTheParser|TestThePlainTest)$')"

echo "case: a non-test file carrying the tag contributes nothing"
# Real: packages/envoy/internal/testnats/image.go carries //go:build natslive and declares no test.
root=$(fixture nontestfile)
gofile "$root/image.go" demo 'func TestLookingButNotInATestFile(t *testing.T) {}'
gofile "$root/image_test.go" demo 'func TestFromTheTestFile(t *testing.T) {}'
run_lister demo "$root"
check "exits 0" "$(is "$status" 0)"
check "only the _test.go file is read" "$(is "$out" '^(TestFromTheTestFile)$')"

echo "case: another tag, and no constraint at all, each contribute nothing"
root=$(fixture othertag)
gofile "$root/other_test.go" othertag 'func TestOfAnotherTag(t *testing.T) {}'
gofile "$root/plain_test.go" '' 'func TestWithNoConstraint(t *testing.T) {}'
gofile "$root/wanted_test.go" demo 'func TestOfTheTag(t *testing.T) {}'
run_lister demo "$root"
check "exits 0" "$(is "$status" 0)"
check "neither is named, and neither is an error" "$(is "$out" '^(TestOfTheTag)$')"

echo "case: a tag no file carries"
root=$(fixture notag)
gofile "$root/plain_test.go" '' 'func TestWithNoConstraint(t *testing.T) {}'
run_lister demo "$root"
check "exits non-zero" "$(is "$((status != 0))" 1)"
check "prints nothing on standard output" "$(is "$out" "")"
check "says no file carries it" \
  "$(contains "$err" "no \*_test.go file in '$root' is constrained by '//go:build demo' alone")"

echo "case: a tagged file that declares no test"
root=$(fixture notests)
gofile "$root/helpers_test.go" demo 'func newHarness(t *testing.T) {}'
run_lister demo "$root"
check "exits non-zero" "$(is "$((status != 0))" 1)"
check "prints nothing on standard output" "$(is "$out" "")"
check "says the tagged files declare none" \
  "$(contains "$err" "declare no Test, Fuzz or Example function")"

echo "case: a constraint shape it refuses"
shape_number=0
for shape in 'demo && linux' '!demo' 'demo || linux' '(demo || othertag) && linux'; do
  shape_number=$((shape_number + 1))
  root=$(fixture "shape$shape_number")
  gofile "$root/ok_test.go" demo 'func TestOfTheTag(t *testing.T) {}'
  gofile "$root/compound_test.go" "$shape" 'func TestOfTheCompound(t *testing.T) {}'
  run_lister demo "$root"
  check "'$shape' exits non-zero" "$(is "$((status != 0))" 1)"
  check "'$shape' prints nothing on standard output" "$(is "$out" "")"
  check "'$shape' names the file" "$(contains "$err" "$root/compound_test.go")"
  check "'$shape' names the constraint line" "$(contains "$err" "//go:build $shape")"
done

echo "case: a constraint that merely begins with the tag is no refusal"
root=$(fixture neighbour)
gofile "$root/neighbour_test.go" demonstration 'func TestOfTheOtherTag(t *testing.T) {}'
gofile "$root/wanted_test.go" demo 'func TestOfTheTag(t *testing.T) {}'
run_lister demo "$root"
check "exits 0" "$(is "$status" 0)"
check "'demonstration' is another tag, not this one" "$(is "$out" '^(TestOfTheTag)$')"

echo "case: a subdirectory does not contribute"
root=$(fixture subdir)
gofile "$root/top_test.go" demo 'func TestAtTheTop(t *testing.T) {}'
mkdir -p "$root/sub"
gofile "$root/sub/deep_test.go" demo 'func TestInTheSubdirectory(t *testing.T) {}'
run_lister demo "$root"
check "exits 0" "$(is "$status" 0)"
check "one directory is read, not a tree" "$(is "$out" '^(TestAtTheTop)$')"

echo "case: the wrong number of arguments"
root=$(fixture arguments)
gofile "$root/wanted_test.go" demo 'func TestOfTheTag(t *testing.T) {}'
run_lister
check "none exits non-zero" "$(is "$((status != 0))" 1)"
check "none prints nothing on standard output" "$(is "$out" "")"
check "none says what it takes" "$(contains "$err" 'takes a build tag and one directory, got 0')"
run_lister demo
check "one exits non-zero" "$(is "$((status != 0))" 1)"
check "one says what it takes" "$(contains "$err" 'takes a build tag and one directory, got 1')"
run_lister demo "$root" extra
check "three exits non-zero" "$(is "$((status != 0))" 1)"
check "three says what it takes" "$(contains "$err" 'takes a build tag and one directory, got 3')"

echo "case: a directory that does not exist"
run_lister demo "$work/no-such-directory"
check "exits non-zero" "$(is "$((status != 0))" 1)"
check "prints nothing on standard output" "$(is "$out" "")"
check "names the path" "$(contains "$err" "'$work/no-such-directory' is not a directory")"

echo "case: this repository's own tree"
# packages/envoy/cmd/listener/server_change_test.go is the one serverchange file today, and
# TestServerChange the one test it declares.
run_lister serverchange "$repo_root/packages/envoy/cmd/listener"
check "exits 0" "$(is "$status" 0)"
check "the listener's serverchange test" "$(is "$out" '^(TestServerChange)$')"

summary "go-tagged-tests.sh"
