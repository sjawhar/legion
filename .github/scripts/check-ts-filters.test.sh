#!/usr/bin/env bash
# Tests for `check-ts-filters.sh`: every file a gate's tsc program compiles must be selected by
# that gate's filter, the files outside the repository and under node_modules dropped and a
# relative path resolved against the repository root, with a program that cannot be listed, a
# pair naming a filter the changes job does not define, a pair naming a path the repository does
# not hold, and a covered filter with a `!` pattern each failing rather than passing silently.
#
# Each case builds a miniature repository under a temporary directory - a ci.yaml whose changes
# job defines two filters through an anchor, a package and a library it compiles, and a
# ci_ts_programs.py pairing them - and symlinks the real script, the engine it borrows the
# changes job's filters from (ci_filter_coverage.py) and the path model both import
# (workflow_paths.py) into it, so the code under test is the code CI runs. The programs are
# listed by a stand-in bunx on the fixture's PATH, which prints the list the fixture wrote for
# the project: a case's input is then the file list itself, and the harness needs no Bun, no
# node_modules and no tsc. It needs a python3 with PyYAML.
#
# Run from anywhere: .github/scripts/check-ts-filters.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes. The app job is gated on the filter app, which names its own
# package and, through the shared anchor, the library it compiles; pkg/app/listed is what the
# stand-in bunx answers for that project, and it holds a file of each kind the script must drop
# or keep. Echoes the fixture root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/bin" "$root/pkg/app/src" "$root/pkg/lib" \
    "$root/other"
  for linked in check-ts-filters.sh ci_filter_coverage.py workflow_paths.py; do
    ln -s "$script_dir/$linked" "$root/.github/scripts/$linked"
  done
  echo '{}' > "$root/pkg/app/tsconfig.json"
  printf 'export const app = 1;\n' > "$root/pkg/app/src/main.ts"
  printf 'export const lib = 1;\n' > "$root/pkg/lib/lib.ts"
  printf 'export const other = 1;\n' > "$root/other/thing.ts"
  # A file of the package, a file of the library, a path tsc printed relative to the repository
  # root, a file under node_modules and a file outside the repository: the last two are no part
  # of what a filter must select, and the three before them are.
  cat > "$root/pkg/app/listed" <<'LIST'
pkg/app/src/main.ts
pkg/lib/lib.ts
./pkg/app/src/generated.ts
node_modules/typescript/lib/lib.es5.d.ts
/usr/lib/node_modules/typescript/lib/lib.dom.d.ts
LIST
  cat > "$root/bin/bunx" <<'SH'
#!/usr/bin/env bash
# A stand-in bunx for the fixtures: `bunx tsc -p <project> --noEmit --listFilesOnly` prints the
# lines of <project>/listed, each made absolute against the fixture root unless it already starts
# with / or ./, as tsc prints absolute paths and the script must read either. With a <project>/fails
# file it prints that file and exits 2, as tsc does on a project it cannot read.
set -euo pipefail
if [ "$1" != tsc ] || [ "$2" != -p ]; then
  echo "stand-in bunx: unexpected argv: $*" >&2
  exit 64
fi
project=$3
if [ -f "$project/fails" ]; then
  cat "$project/fails" >&2
  exit 2
fi
while IFS= read -r line; do
  case $line in
    /* | ./*) echo "$line" ;;
    *) echo "$PWD/$line" ;;
  esac
done < "$project/listed"
SH
  chmod +x "$root/bin/bunx"
  cat > "$root/.github/scripts/ci_ts_programs.py" <<'PY'
PROGRAMS = [("app", "pkg/app", "app: Typecheck the app")]
PY
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: [pull_request]
jobs:
  changes:
    runs-on: ubuntu-24.04
    steps:
      - uses: dorny/paths-filter@v4
        id: filter
        with:
          predicate-quantifier: some-with-excludes
          filters: |
            library: &library
              - 'pkg/lib/**'
            app:
              - '.github/workflows/ci.yaml'
              - *library
              - 'pkg/app/**'
            other:
              - 'other/**'
YAML
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$(PATH="$root/bin:$PATH" "$root/.github/scripts/check-ts-filters.sh" 2>&1)
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

echo "case: the filter selects every file of the program, node_modules and the files outside the repository dropped"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the three files it kept" "$(contains "$out" '^pkg/app: 3 files, 0 uncovered (filter app)$')"
check "says what it checked" "$(contains "$out" 'every changes filter selects every file its job.s typecheck compiles (1 programs, 1 filters)')"

echo "case: a file of another package the filter does not select"
root=$(fixture uncovered-file)
replace "$root/.github/workflows/ci.yaml" "              - 'pkg/lib/**'" "              - 'other/**'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the file and the program it came from" \
  "$(contains "$out" '::error file=pkg/lib/lib.ts::filter app does not select pkg/lib/lib.ts, which the tsc program of pkg/app compiles (app: Typecheck the app)')"
check "says the filter must select it or no job starts" \
  "$(contains "$out" 'the filter must select it, or the change that breaks that program starts no job')"
check "counts it" "$(contains "$out" '^pkg/app: 3 files, 1 uncovered (filter app)$')"

echo "case: a program tsc cannot list"
root=$(fixture unlistable-program)
printf 'error TS5083: cannot read file tsconfig.json\n' > "$root/pkg/app/fails"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and what it printed" \
  "$(contains "$out" '::error file=pkg/app::`bunx tsc -p pkg/app --noEmit --listFilesOnly` failed (exit 2), so the files its program holds are unknown: error / TS5083')"

echo "case: a pair naming a filter the changes job does not define"
root=$(fixture unknown-filter)
replace "$root/.github/scripts/ci_ts_programs.py" '("app", "pkg/app"' '("application", "pkg/app"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter and the program it holds to nothing" \
  "$(contains "$out" '::error file=.github/scripts/ci_ts_programs.py::application is no filter of the changes job, so the program pkg/app')"

echo "case: a pair naming a path the repository does not hold"
root=$(fixture missing-project)
replace "$root/.github/scripts/ci_ts_programs.py" '"pkg/app"' '"pkg/gone"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the path and the filter it leaves holding no program" \
  "$(contains "$out" '::error file=.github/scripts/ci_ts_programs.py::pkg/gone (app: Typecheck the app) is no path of this repository, so filter app is held to no program')"

echo "case: a ! pattern in a covered filter"
root=$(fixture negated-pattern)
replace "$root/.github/workflows/ci.yaml" "              - 'pkg/app/**'" $'              - \'pkg/app/**\'\n              - \'!pkg/app/src/generated.ts\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the pattern and the filter it is in" \
  "$(contains "$out" '::error file=.github/workflows/ci.yaml::filter app has !pkg/app/src/generated.ts, a `!` pattern')"
check "and reads the filter no further" "$(is "$(contains "$out" '^pkg/app: ')" false)"

summary "check-ts-filters.sh holds each Bun gate's filter to every file its tsc program compiles"
