#!/usr/bin/env bash
# Tests for `zizmor-findings.sh`: a finding is new only when the head carries more copies of its
# fingerprint than the base does, and the fingerprint survives the edits a pull request makes
# around a finding without touching it (rows, step indices, the text of an enclosing job, an
# inline ignore comment, the directory zizmor was run from).
#
# Fixtures are zizmor 1.30.1 json-v1 findings in the shape it writes (measured 2026-09-30): a
# Hidden location for the enclosing step first, then the Primary location that carries the route.
# testdata/zizmor*.json are zizmor 1.30.1's own output (`--format=json-v1 .`, offline) over a
# scratch tree whose files their `feature` texts quote: workflow a.yaml (an unpinned checkout and
# two steps echoing the pull request's title and body), workflow b.yaml (an unpinned checkout),
# composite action z (echoes the issue title) and a zizmor.yml with the `"*": hash-pin` policy.
# zizmor.json is that tree; zizmor-ignores.json adds `# zizmor: ignore[template-injection]` to
# a.yaml's body step and `rules.artipacked.ignore: [b.yaml]`, and zizmor-ignores-no-ignores.json
# audits that tree with --no-ignores.
#
# Run from anywhere: .github/scripts/zizmor-findings.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fingerprint="$script_dir/zizmor-findings.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# finding IDENT PATH ROUTE ROW FEATURE: one finding. ROUTE is a JSON array of {"Key": …} and
# {"Index": …} entries; ROW is the Primary location's zero-based row.
finding() {
  jq -cn --arg ident "$1" --arg path "$2" --argjson route "$3" --argjson row "$4" --arg feature "$5" '{
    ident: $ident,
    desc: ("the " + $ident + " audit fired"),
    url: ("https://docs.zizmor.sh/audits/#" + $ident),
    determinations: {confidence: "High", severity: "High", persona: "Regular"},
    locations: [
      {
        symbolic: {
          key: {Local: {verbatim_path: $path}},
          annotation: "enclosing step",
          route: {route: ($route | if length > 0 then .[0:-1] else . end)},
          feature_kind: "Normal",
          kind: "Hidden"
        },
        concrete: {
          location: {start_point: {row: ($row + 40), column: 0}, end_point: {row: ($row + 41), column: 0}},
          feature: "a hidden span that is not the finding"
        }
      },
      {
        symbolic: {
          key: {Local: {verbatim_path: $path}},
          annotation: "the finding",
          route: {route: $route},
          feature_kind: "Normal",
          kind: "Primary"
        },
        concrete: {
          location: {start_point: {row: $row, column: 6}, end_point: {row: ($row + 1), column: 0}},
          feature: $feature
        }
      }
    ]
  }'
}

# array FINDING…: a json-v1 document holding the given findings.
array() { jq -cs '.' <<<"$(printf '%s\n' "$@")"; }

# shift_rows N DOC: the same document with every row moved down by N.
shift_rows() {
  jq -c --argjson n "$1" '[.[] | .locations |= [.[] | .concrete.location.start_point.row += $n
    | .concrete.location.end_point.row += $n]]' <<<"$2"
}

# with_path PREFIX DOC: every verbatim_path rewritten to PREFIX followed by its `.github/…` part.
with_path() {
  jq -c --arg p "$1" '[.[] | .locations |= [.[] | .symbolic.key.Local.verbatim_path |=
    ($p + (. | sub("^.*?(?=\\.github/)"; "")))]]' <<<"$2"
}

# run NAME HEAD [BASE] [--annotate]: runs the script over the two documents; leaves the output
# at $work/NAME.out.json, its stdout at $work/NAME.stdout and its exit code in $rc.
run() {
  local name="$1" head="$2" base="${3:-}"
  shift 2
  if [ $# -gt 0 ]; then shift; fi
  printf '%s' "$head" > "$work/$name.head.json"
  local args=(--head "$work/$name.head.json" --out "$work/$name.out.json" "$@")
  if [ -n "$base" ]; then
    printf '%s' "$base" > "$work/$name.base.json"
    args+=(--base "$work/$name.base.json")
  fi
  rc=0
  "$fingerprint" "${args[@]}" > "$work/$name.stdout" 2> "$work/$name.stderr" || rc=$?
}

out() { jq -r "$2" "$work/$1.out.json" 2>/dev/null || echo "unreadable"; }

steps='[{"Key":"jobs"},{"Key":"build"},{"Key":"steps"},{"Index":2}]'
checkout_step='[{"Key":"jobs"},{"Key":"build"},{"Key":"steps"},{"Index":0},{"Key":"uses"}]'
job='[{"Key":"jobs"},{"Key":"build"}]'
wf='[]'
x=./.github/workflows/x.yaml
y=./.github/actions/y/action.yml

injection=$(finding template-injection "$x" "$(jq -c '. + [{"Key":"run"}]' <<<"$steps")" 20 \
  $'|\n  echo "${{ github.event.pull_request.title }}"\n')
unpinned=$(finding unpinned-uses "$x" "$checkout_step" 11 "uses: actions/checkout@v5")
permissions=$(finding excessive-permissions "$x" "$job" 8 $'build:\n    runs-on: ubuntu-24.04\n    steps:\n')
workflow_level=$(finding excessive-permissions "$x" "$wf" 0 $'name: X\non: push\n')
action_env=$(finding github-env "$y" '[{"Key":"runs"},{"Key":"steps"},{"Index":0},{"Key":"run"}]' 15 \
  $'run: |\n  echo "X=1" >> "$GITHUB_ENV"\n')
baseline=$(array "$injection" "$unpinned" "$permissions" "$workflow_level" "$action_env")

echo "=== 1. base and head identical: nothing is new ==="
run identical "$baseline" "$baseline"
check "exits 0" "$(is "$rc" 0)"
check "new is []" "$(is "$(out identical '.new | tojson')" '[]')"
check "new_count is 0" "$(is "$(out identical .new_count)" 0)"
check "head_count is the fixture size (5)" "$(is "$(out identical .head_count)" 5)"
check "base lists the 5 base findings" "$(is "$(out identical '.base | length')" 5)"

echo "=== 2. one finding fixed, one added at a new step: exactly that one is new ==="
added=$(finding unpinned-uses "$x" '[{"Key":"jobs"},{"Key":"lint"},{"Key":"steps"},{"Index":0},{"Key":"uses"}]' 30 \
  "uses: actions/setup-go@v5")
head2=$(array "$unpinned" "$permissions" "$workflow_level" "$action_env" "$added")
run one-new "$head2" "$baseline"
check "exits 0" "$(is "$rc" 0)"
check "new has exactly one entry" "$(is "$(out one-new '.new | length')" 1)"
check "new_count is 1" "$(is "$(out one-new .new_count)" 1)"
check "the new entry is the added unpinned-uses at line 31" \
  "$(is "$(out one-new '.new[0] | "\(.ident) \(.path) \(.line) \(.route)"')" \
    "unpinned-uses .github/workflows/x.yaml 31 jobs/lint/steps/uses")"
# shellcheck disable=SC2016 # $f is a jq variable
check "its fingerprint is absent from base" \
  "$(is "$(out one-new '.new[0].fingerprint as $f | [.base[].fingerprint] | index($f)')" null)"
check "each entry carries severity and confidence" \
  "$(is "$(out one-new '.new[0] | "\(.severity)/\(.confidence)"')" "High/High")"

echo "=== 3. rows shifted, a step index moved, a job's span rewritten: nothing is new ==="
moved_injection=$(finding template-injection "$x" \
  '[{"Key":"jobs"},{"Key":"build"},{"Key":"steps"},{"Index":3},{"Key":"run"}]' 20 \
  $'|\n  echo "${{ github.event.pull_request.title }}"\n')
rewritten_job=$(finding excessive-permissions "$x" "$job" 8 \
  $'build:\n    - run: echo inserted above\n    runs-on: ubuntu-24.04\n    steps:\n')
head3=$(shift_rows 3 "$(array "$moved_injection" "$unpinned" "$rewritten_job" "$workflow_level" "$action_env")")
run shifted "$head3" "$baseline"
check "new is []" "$(is "$(out shifted '.new | tojson')" '[]')"
check "lines follow the shifted rows (the injection reads line 24)" \
  "$(is "$(out shifted '[.head[] | select(.ident == "template-injection")][0].line')" 24)"

echo "=== 4. no --base: new, new_count and base are null ==="
run no-base "$baseline"
check "exits 0" "$(is "$rc" 0)"
check "new is null" "$(is "$(out no-base '.new')" null)"
check "new_count is null" "$(is "$(out no-base '.new_count')" null)"
check "base is null" "$(is "$(out no-base '.base')" null)"
check "ignored is null" "$(is "$(out no-base '.ignored')" null)"
check "head_count is the fixture size (5)" "$(is "$(out no-base .head_count)" 5)"
check "prints no annotation" "$(is "$(wc -l < "$work/no-base.stdout")" 0)"

echo "=== 5. --annotate: one warning per new finding, repository-relative, none for the baseline ==="
run annotate "$head2" "$baseline" --annotate
annotations=$(grep -c '^::warning ' "$work/annotate.stdout" || true)
check "exactly one ::warning line" "$(is "$annotations" 1)"
check "it names the file, line, audit and description" "$(contains "$(cat "$work/annotate.stdout")" \
  '^::warning file=.github/workflows/x.yaml,line=31,title=zizmor unpinned-uses::the unpinned-uses audit fired$')"
check "no annotation names a baseline finding" \
  "$(is "$(grep -c 'template-injection\|excessive-permissions\|github-env' "$work/annotate.stdout" || true)" 0)"
run annotate-quiet "$head2" "$baseline"
check "without --annotate, stdout is empty" "$(is "$(wc -l < "$work/annotate-quiet.stdout")" 0)"

echo "=== 6. base audited from an absolute path, head from its root: nothing is new ==="
run prefixes "$baseline" "$(with_path /tmp/base/ "$baseline")"
check "new is []" "$(is "$(out prefixes '.new | tojson')" '[]')"
check "paths are repository-relative on both sides" \
  "$(is "$(out prefixes '[.head[].path, .base[].path] | map(select(startswith(".github/") | not)) | length')" 0)"

echo "=== 7. the difference is a multiset ==="
run repeated "$(array "$injection" "$unpinned" "$permissions" "$workflow_level" "$action_env" "$injection")" "$baseline"
check "a second copy of an existing finding is one new entry" "$(is "$(out repeated '.new | length')" 1)"
check "the new entry is the repeated template-injection" "$(is "$(out repeated '.new[0].ident')" template-injection)"
doubled=$(array "$injection" "$injection" "$unpinned" "$permissions" "$workflow_level" "$action_env")
run one-removed "$(array "$injection" "$unpinned" "$permissions" "$workflow_level" "$action_env")" "$doubled"
check "removing one of two copies adds nothing" "$(is "$(out one-removed '.new | tojson')" '[]')"

echo "=== 8. an inline ignore added to a step leaves its finding's fingerprint unchanged ==="
# zizmor 1.30.1's own output (testdata/): the tree, and the same tree with an inline
# `# zizmor: ignore[template-injection]` on one step audited with --no-ignores, where that
# finding's feature text carries the comment.
testdata="$script_dir/testdata"
run inline-comment "$(< "$testdata/zizmor-ignores-no-ignores.json")" "$(< "$testdata/zizmor.json")"
check "the commented finding is in the head" \
  "$(is "$(out inline-comment '[.head[] | select(.ident == "template-injection")] | length')" 3)"
check "nothing is new against the tree without the comment" "$(is "$(out inline-comment '.new | tojson')" '[]')"

echo "=== 9. --no-ignores: the findings zizmor's ignores suppressed, one entry each ==="
# The tree with an inline ignore on one of a.yaml's two template-injection steps and a
# rules.artipacked.ignore entry for b.yaml, audited normally and with --no-ignores.
run ignores "$(< "$testdata/zizmor-ignores.json")" "$(< "$testdata/zizmor.json")" \
  --no-ignores "$testdata/zizmor-ignores-no-ignores.json"
check "exits 0" "$(is "$rc" 0)"
check "ignored holds the inline-ignored step and the config-ignored file, nothing else" \
  "$(is "$(out ignores '[.ignored[] | "\(.ident) \(.path) \(.line)"] | sort | join(", ")')" \
    "artipacked .github/workflows/b.yaml 10, template-injection .github/workflows/a.yaml 14")"
# shellcheck disable=SC2016 # $base is a jq variable
check "each ignored fingerprint is the one that finding had before the ignore" \
  "$(is "$(out ignores '[.base[].fingerprint] as $base | [.ignored[].fingerprint | select(. as $f | $base | index($f) | not)] | length')" 0)"
check "the other template-injection in a.yaml is not ignored" \
  "$(is "$(out ignores '[.ignored[] | select(.ident == "template-injection")] | length')" 1)"
check "nothing is new" "$(is "$(out ignores '.new | tojson')" '[]')"
check "head_count counts only what zizmor reported (6)" "$(is "$(out ignores .head_count)" 6)"

echo "=== usage errors exit 2 ==="
rc=0
"$fingerprint" --head "$work/identical.head.json" > /dev/null 2> "$work/usage.stderr" || rc=$?
check "missing --out exits 2" "$(is "$rc" 2)"
rc=0
"$fingerprint" --head "$work/missing.json" --out "$work/x.json" > /dev/null 2> "$work/usage.stderr" || rc=$?
check "an unreadable --head exits 2" "$(is "$rc" 2)"

summary "zizmor-findings.sh fingerprints findings against the base"
