#!/usr/bin/env bash
# The tests one Go build tag adds to one package, as the anchored alternation `go test -run` takes.
#
# A build tag adds files to a package; it subtracts none. So `go test -tags memory ./pkg` runs
# every test of `pkg`, the untagged ones the untagged job already ran included. A tag step that
# wants only what the tag adds has to name those tests, and this script derives that name list
# from the source, so nothing is kept by hand:
#
#   go test -tags memory -count=1 \
#     -run "$(.github/scripts/go-tagged-tests.sh memory packages/envoy/cmd/dispatch)" \
#     ./packages/envoy/cmd/dispatch
#
# It reads the source rather than running `go test -list`. A listing would cost a second compile
# of the package inside every tag job, and it would take two of them — the tagged listing and the
# untagged one — to subtract one from the other and be left with what the tag adds.
#
# Arguments: a build tag, then one directory. It reads the `*_test.go` files directly in that
# directory and no deeper, each one's `//go:build` line, which Go requires in the leading comment
# block before the `package` clause. Legacy `// +build` lines are not read and need not be: no
# file under packages/ carries one.
#
# A file contributes its tests when its constraint is the bare tag — the line is exactly
# `//go:build <tag>`, whitespace aside. A file with no `//go:build` line, or with one that does
# not mention the tag, contributes nothing and is no error: that is every ordinary test of the
# package, and leaving them out is the point.
#
# What it prints: one line on standard output, `^(A|B|C)$`, the names sorted, and nothing else.
# A test is a top-level `func Test…(`, `func Fuzz…(` or `func Example…(` of a contributing file,
# in the package or in its `_test` package. A method (`func (h *harness) TestFoo(`) is not one.
# `TestMain` is excluded: it is the package's entry point and `-run` never selects it.
#
# Every refusal exits non-zero with its reason on standard error, and prints nothing at all on
# standard output. It refuses:
#
#   1. any argument count but two, or a directory that does not exist;
#   2. a constraint that mentions the tag in any shape but the bare one — a conjunction
#      (`memory && linux`), a disjunction, a negation (`!memory`), a parenthesised form. Every
#      file carrying one of the six tags in use has the bare form today, so a future
#      `//go:build memory && linux` is a loud failure here and never a silent omission from a
#      `-run` that would then skip that file's tests while the job still reported green;
#   3. finding no test. A renamed tag or a moved file is then loud: the calling step assigns this
#      output and fails under `bash -e`, instead of running `-run ""`, which matches every test —
#      the very thing this script exists to stop.
#
# Bash and what a GitHub runner already has (awk, sort): no extra tooling.
#
# Run from anywhere: .github/scripts/go-tagged-tests.sh <tag> <dir>, the directory as your shell
# reads it, relative to your working directory.
# ci.yaml's tag steps run it, and its tests run in ci.yaml's test job.
set -euo pipefail
shopt -s nullglob

fail() { # fail MESSAGE...: on standard error, and nothing on standard output
  echo "go-tagged-tests.sh: $*" >&2
  exit 1
}

if [ $# -ne 2 ]; then
  echo "go-tagged-tests.sh: takes a build tag and one directory, got $#: $*" >&2
  echo "go-tagged-tests.sh: usage: go-tagged-tests.sh <tag> <dir>" >&2
  exit 1
fi
tag=$1
dir=$2

[ -d "$dir" ] || fail "'$dir' is not a directory"

# The constraint of FILE's `//go:build` line, trimmed, or nothing when it has none. The directive
# binds only in the leading comment block, so the `package` clause ends the search.
constraint_of() { # constraint_of FILE
  awk '
    /^[[:space:]]*\/\/go:build/ {
      sub(/^[[:space:]]*\/\/go:build/, "")
      sub(/^[[:space:]]+/, "")
      sub(/[[:space:]]+$/, "")
      print
      exit
    }
    /^[[:space:]]*package[[:space:]]/ { exit }
  ' "$1"
}

# Whether CONSTRAINT names TAG as one of its terms. Every character a build tag cannot hold is a
# separator, so `memory && linux`, `!memory` and `(memory || x)` all name `memory`, while
# `memoryfoo` and `inmemory` name nothing.
mentions_tag() { # mentions_tag CONSTRAINT TAG
  local -a terms
  local term
  read -ra terms <<<"${1//[^[:alnum:]_.]/ }"
  for term in "${terms[@]}"; do
    [ "$term" = "$2" ] && return 0
  done
  return 1
}

tagged=()
for file in "$dir"/*_test.go; do
  constraint=$(constraint_of "$file")
  if [ "$constraint" = "$tag" ]; then
    tagged+=("$file")
  elif [ -n "$constraint" ] && mentions_tag "$constraint" "$tag"; then
    fail "$file: '//go:build $constraint' names '$tag' in a shape this script will not read." \
      "Only the bare '//go:build $tag' contributes its tests, so this file would be left out of" \
      "a -run that claims to cover the tag. Put the tag's tests in a file constrained by the tag" \
      "alone, or teach this script the shape."
  fi
done

if [ ${#tagged[@]} -eq 0 ]; then
  fail "no *_test.go file in '$dir' is constrained by '//go:build $tag' alone"
fi

# $2 of a `func Name(…)` line, less the arguments: a top-level test, never a method, since a
# method's `func` is followed by its receiver's `(`. go runs a `func TestXxx(` whose Xxx does not
# start with a lower-case letter, so `func Test(` and `func Test_one(` are tests and
# `func Testify(` is an ordinary function; naming one in a -run would be a dead alternative.
names=$(awk '
  /^func[[:space:]]+(Test|Fuzz|Example)([^a-z][A-Za-z0-9_]*)?[[:space:]]*\(/ {
    name = $0
    sub(/^func[[:space:]]+/, "", name)
    sub(/[[:space:]]*\(.*$/, "", name)
    if (name != "TestMain") { print name }
  }
' "${tagged[@]}" | LC_ALL=C sort -u)

if [ -z "$names" ]; then
  fail "the ${#tagged[@]} file(s) in '$dir' constrained by '//go:build $tag' declare no Test," \
    "Fuzz or Example function"
fi

printf '^(%s)$\n' "${names//$'\n'/|}"
