#!/usr/bin/env bash
# Tests for `workflow_paths.py`, the path model the three path checks import: a path glob as a
# regex (picomatch's `**`, `*`, `?` and `[...]`, with `{` and `}` as literal characters), a GitHub
# `paths` list where the last match wins and `!` negates, a dorny/paths-filter filter under its
# predicate quantifier, a workflow's events, the patterns the model refuses to read rather than
# guess, and the walk of the repository's files.
#
# Only what the model does. What each checker makes of it — its verdicts, its reading of a
# workflow — stays in that checker's own suite: check-ci-filters.test.sh,
# check-image-trigger-paths.test.sh and check-ci-images.test.sh, each of which symlinks this same
# module rather than reimplementing it.
#
# The module is symlinked into a temporary directory and imported the way the checks import it
# (PYTHONPATH, no bytecode), so the code under test is the file CI runs. It needs a python3 with
# PyYAML, which the events cases parse their workflow with.
#
# Run from anywhere: .github/scripts/workflow_paths.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

ln -s "$script_dir/workflow_paths.py" "$work/workflow_paths.py"

# py <statements> [argument ...]: runs the statements with the model as `wp` and the arguments in
# sys.argv[1:], and echoes what they print.
py() {
  local statements=$1
  shift
  PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$work" python3 -c "import sys
import workflow_paths as wp
$statements" "$@"
}

# matches <pattern> <path>: true/false for check — whether glob_regex(<pattern>) matches the whole
# path, which is how Filter and PathsFilter ask it.
matches() {
  py 'print(str(wp.glob_regex(sys.argv[1]).fullmatch(sys.argv[2]) is not None).lower())' "$1" "$2"
}

# covers <path> <pattern>...: true/false — whether a GitHub `paths` list of those patterns covers
# the path. No pattern is the empty list, not the absent filter.
covers() {
  local path=$1
  shift
  py 'print(str(wp.Filter(sys.argv[2:]).covers(sys.argv[1])).lower())' "$path" "$@"
}

# ignore_covers <path> <pattern>...: the same for a `paths-ignore` list, where a path no pattern
# names is the covered one.
ignore_covers() {
  local path=$1
  shift
  py 'print(str(wp.Filter(sys.argv[2:], ignore=True).covers(sys.argv[1])).lower())' "$path" "$@"
}

# filter_covers <quantifier> <path> <pattern>...: true/false — whether a dorny/paths-filter filter
# of those patterns, under that predicate-quantifier, matches the path.
filter_covers() {
  local quantifier=$1 path=$2
  shift 2
  py 'print(str(wp.PathsFilter(sys.argv[3:], sys.argv[1]).covers(sys.argv[2])).lower())' \
    "$quantifier" "$path" "$@"
}

# refused_quantifier <name>: what PathsFilter raises for a quantifier it does not model.
refused_quantifier() {
  py 'try:
    wp.PathsFilter(["mod/**"], sys.argv[1])
except ValueError as refusal:
    print(refusal)
else:
    print("no refusal")' "$1"
}

# triggers <workflow>: the event names events() reads from that YAML document, space separated.
triggers() {
  py 'import yaml
print(" ".join(str(name) for name in wp.events(yaml.safe_load(sys.argv[1]))))' "$1"
}

# unread <pattern>...: the patterns unread_patterns refuses, space separated.
unread() { py 'print(" ".join(wp.unread_patterns(sys.argv[1:])))' "$@"; }

# unread_filter <pattern>...: the patterns unread_filter_patterns refuses, space separated.
unread_filter() { py 'print(" ".join(wp.unread_filter_patterns(sys.argv[1:])))' "$@"; }

# walked: every file walk() finds under the working directory, sorted, space separated.
walked() { py 'print(" ".join(sorted(wp.walk())))'; }

echo "case: ** spans directories, * and ? stay inside one"
check "**/ matches no directory at all" "$(matches '**/Dockerfile' 'Dockerfile')"
check "**/ matches one" "$(matches '**/Dockerfile' 'docker/Dockerfile')"
check "**/ matches several" "$(matches '**/Dockerfile' 'packages/app/docker/Dockerfile')"
check "** at the end of a pattern spans directories" \
  "$(matches 'packages/app/**' 'packages/app/src/deep/nested.ts')"
check "a bare ** matches every path" "$(matches '**' 'packages/app/src/index.ts')"
check "* matches a whole path segment" "$(matches 'packages/*' 'packages/app')"
check "and part of one" "$(matches 'docker/*.yaml' 'docker/compose.yaml')"
check "but stays inside it" "$(is "$(matches 'packages/*' 'packages/app/src')" false)"
check "even with the rest of the pattern behind it" \
  "$(is "$(matches 'docker/*.yaml' 'docker/base/compose.yaml')" false)"
check "? is one character inside a segment" "$(matches 'package.jso?' 'package.json')"
check "not zero" "$(is "$(matches 'package.jso?' 'package.jso')" false)"
check "and not a /" "$(is "$(matches 'docker?Dockerfile' 'docker/Dockerfile')" false)"
check "a pattern matches a path whole, not its head" \
  "$(is "$(matches 'packages/app' 'packages/app/src/index.ts')" false)"

echo "case: a [...] class is the regex's, and an unclosed [ is a literal"
check "a class matches one character of itself" "$(matches 'v[0-9].yaml' 'v2.yaml')"
check "and only one" "$(is "$(matches 'v[0-9].yaml' 'v12.yaml')" false)"
check "an unclosed [ is the character itself" "$(matches 'docker/a[b' 'docker/a[b')"

echo "case: a brace is a literal character, not an expansion"
check "{a,b} matches the text it is" "$(matches '{docker/**,.dockerignore}' '{docker/**,.dockerignore}')"
check "and no branch of it" "$(is "$(matches '{docker,.dockerignore}' 'docker')" false)"
check "a closing brace alone is a literal too" "$(matches 'docker/}' 'docker/}')"

echo "case: a GitHub paths list, where the last match wins and ! negates"
check "a path a pattern matches is covered" "$(covers 'packages/app/src/index.ts' 'packages/app/**')"
check "a path no pattern matches is not" "$(is "$(covers 'docs/readme.md' 'packages/app/**')" false)"
check "a later ! pattern takes the match back" \
  "$(is "$(covers 'packages/app/src/index.ts' 'packages/app/**' '!packages/app/src/**')" false)"
check "an earlier one does not: the last match wins" \
  "$(covers 'packages/app/src/index.ts' '!packages/app/src/**' 'packages/app/**')"
check "a ! pattern the path does not match leaves it covered" \
  "$(covers 'packages/app/src/index.ts' 'packages/app/**' '!docs/**')"
check "an empty list covers nothing" "$(is "$(covers 'docs/readme.md')" false)"

echo "case: an event with no path filter covers everything"
check "no list at all is everything" "$(py 'print(str(wp.Filter(None).everything).lower())')"
check "and it covers a path no pattern names" \
  "$(py 'print(str(wp.Filter(None).covers(sys.argv[1])).lower())' 'docs/readme.md')"
check "an empty list is not everything" "$(is "$(py 'print(str(wp.Filter([]).everything).lower())')" false)"

echo "case: a paths-ignore list covers what it does not name"
check "a path a pattern names is left out" "$(is "$(ignore_covers 'docs/readme.md' 'docs/**')" false)"
check "every other path is covered" "$(ignore_covers 'packages/app/src/index.ts' 'docs/**')"
check "a ! pattern in it takes a path back in" \
  "$(ignore_covers 'docs/api/readme.md' 'docs/**' '!docs/api/**')"

echo "case: a dorny/paths-filter filter under the default quantifier, some"
check "a pattern that matches the path matches" "$(filter_covers some 'mod/lib/lib.go' 'mod/**')"
check "a path no pattern matches does not" "$(is "$(filter_covers some 'docs/readme.md' 'mod/**')" false)"
check "a ! pattern matches every file outside it" \
  "$(filter_covers some 'docs/readme.md' '!mod/other/**')"
check "and no file inside it" "$(is "$(filter_covers some 'mod/other/other.go' '!mod/other/**')" false)"
check "so beside an include it excludes nothing" \
  "$(filter_covers some 'mod/other/other.go' 'mod/**' '!mod/other/**')"

echo "case: the same filter under some-with-excludes, where a ! pattern excludes"
check "an included path matches" \
  "$(filter_covers some-with-excludes 'mod/lib/lib.go' 'mod/**' '!mod/other/**')"
check "an excluded one does not" \
  "$(is "$(filter_covers some-with-excludes 'mod/other/other.go' 'mod/**' '!mod/other/**')" false)"
check "the exclusion holds wherever it stands in the list" \
  "$(is "$(filter_covers some-with-excludes 'mod/other/other.go' '!mod/other/**' 'mod/**')" false)"
check "a path no include matches does not match" \
  "$(is "$(filter_covers some-with-excludes 'docs/readme.md' 'mod/**' '!mod/other/**')" false)"
check "nor does one only a ! pattern matches" \
  "$(is "$(filter_covers some-with-excludes 'mod/other/other.go' '!mod/other/**')" false)"

echo "case: a predicate-quantifier the model does not read is refused, not guessed"
check "every is refused" "$(is "$(refused_quantifier every)" 'predicate-quantifier every is not modelled')"
check "and so is a name that is no quantifier" \
  "$(is "$(refused_quantifier all)" 'predicate-quantifier all is not modelled')"

echo "case: events() reads the workflow's on:, which YAML 1.1 gives as the boolean true"
check "a bare on: key is that true" "$(is "$(triggers 'on:
  push:
    paths: [docker/**]
  workflow_dispatch:
')" 'push workflow_dispatch')"
check "a quoted \"on\" key is the string" "$(is "$(triggers '"on":
  push:
')" 'push')"
check "one event named as a string" "$(is "$(triggers 'on: push')" 'push')"
check "a list of events" "$(is "$(triggers 'on: [push, pull_request]')" 'push pull_request')"
check "a document with no on: at all" "$(is "$(triggers 'jobs: {}')" '')"
check "and each event keeps its own block" "$(is "$(py 'import yaml
print(" ".join(wp.events(yaml.safe_load(sys.argv[1]))["push"]["paths"]))' 'on:
  push:
    paths: [docker/**, .dockerignore]
')" 'docker/** .dockerignore')"

echo "case: a pattern GitHub reads another way is refused, not guessed"
check "? is refused" "$(is "$(unread 'package.json?')" 'package.json?')"
check "+ is refused" "$(is "$(unread 'docs/a+')" 'docs/a+')"
check "a backslash is refused" "$(is "$(unread 'docs/a\b')" 'docs/a\b')"
check "a brace is refused" "$(is "$(unread '{docker/**,.dockerignore}')" '{docker/**,.dockerignore}')"
check "a closing brace alone is refused" "$(is "$(unread 'docker/}')" 'docker/}')"
check "the patterns it does read are kept" \
  "$(is "$(unread 'packages/app/**' 'docker/*' 'v[0-9].yaml' '!docs/**')" '')"
check "and it names each refused pattern of a list" \
  "$(is "$(unread 'packages/app/**' 'package.json?' 'docker/}')" 'package.json? docker/}')"

echo "case: a paths-filter pattern keeps picomatch's ?, and only a brace is refused"
check "? is kept" "$(is "$(unread_filter 'package.jso?')" '')"
check "+ is kept" "$(is "$(unread_filter 'docs/a+')" '')"
check "a backslash is kept" "$(is "$(unread_filter 'docs/a\b')" '')"
check "a brace picomatch expands is refused" "$(is "$(unread_filter '{a,b}/**')" '{a,b}/**')"
check "a closing brace alone is refused" "$(is "$(unread_filter 'docker/}')" 'docker/}')"

echo "case: walk() lists every file of the tree, version control and dependencies apart"
root="$work/tree"
mkdir -p "$root/packages/app/src" "$root/.git/objects" "$root/node_modules/dep" "$root/elsewhere"
echo '{}' > "$root/package.json"
echo 'export {};' > "$root/packages/app/src/index.ts"
echo 'object' > "$root/.git/objects/one"
echo 'module.exports = {};' > "$root/node_modules/dep/index.js"
echo 'linked' > "$root/elsewhere/file.txt"
ln -s ../../elsewhere "$root/packages/app/linked"
check "every file, by its path from the working directory, with no skipped directory and no symlinked one" \
  "$(is "$(cd "$root" && walked)" 'elsewhere/file.txt package.json packages/app/src/index.ts')"

summary "workflow_paths.py, the path model the CI path checks import"
