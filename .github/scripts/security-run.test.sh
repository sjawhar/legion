#!/usr/bin/env bash
# Tests for `security-run.sh`: the base a run is judged against and the flags it reads there, the
# dependency scanners' counts from their real output, a half that fails closed on any output not in
# its scanner's shape, the security report assembled from a run's two artifacts, and enforcement.
#
# testdata/ holds the scanners' own output, measured 2026-09-30:
#   - osv.json: osv-scanner v2.6.0 `scan source --recursive --format json` over a go.mod requiring
#     github.com/aws/aws-sdk-go v1.55.5 and golang.org/x/text v0.3.7: five vulnerabilities, the
#     three of x/text with a fixed version and the two of aws-sdk-go with none;
#     osv-clean.json: the same over a go.mod with no requirements.
#   - govulncheck.json: govulncheck v1.8.0 `-format json ./...` over a module calling
#     golang.org/x/text v0.3.7's language.ParseAcceptLanguage: GO-2022-1059 reachable (its
#     findings reach the symbol), GO-2026-5970 informational (module-level only);
#     govulncheck-clean.json: the same over a module with no requirements. Both keep every record
#     but the osv entries no finding names (the Go standard library's, a few hundred KB).
#   - zizmor*.json: see zizmor-findings.test.sh.
#
# Run from anywhere: .github/scripts/security-run.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
run_script="$script_dir/security-run.sh"
testdata="$script_dir/testdata"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# summarize NAME [ARG…]: runs `security-run.sh summarize ARG… --out $work/NAME.json`; its stderr is
# in $work/NAME.stderr and its exit code in $rc.
summarize() {
  local name="$1"
  shift
  rc=0
  "$run_script" summarize "$@" --out "$work/$name.json" > /dev/null 2> "$work/$name.stderr" || rc=$?
}

# out NAME JQ: JQ applied to $work/NAME.json, compact.
out() { jq -c "$2" "$work/$1.json" 2>/dev/null || echo "unreadable"; }
stderr_of() { cat "$work/$1.stderr"; }

osv_error='{"total":null,"with_fix":null,"tool_error":true}'
govulncheck_error='{"reachable":null,"informational":null,"tool_error":true}'

echo "=== 1. the scanners' real output: counts per half ==="
summarize real --osv "$testdata/osv.json" --govulncheck "$testdata/govulncheck.json" \
  --govulncheck "$testdata/govulncheck-clean.json"
check "exits 0" "$(is "$rc" 0)"
check "osv: 5 vulnerabilities, 3 with a fixed version" \
  "$(is "$(out real .osv)" '{"total":5,"with_fix":3,"tool_error":false}')"
check "govulncheck: 1 reachable, 1 informational across the two modules" \
  "$(is "$(out real .govulncheck)" '{"reachable":1,"informational":1,"tool_error":false}')"
summarize two-modules --osv "$testdata/osv.json" --govulncheck "$testdata/govulncheck.json" \
  --govulncheck "$testdata/govulncheck.json"
check "one entry per module and OSV id: the same findings in two modules count twice" \
  "$(is "$(out two-modules .govulncheck)" '{"reachable":2,"informational":2,"tool_error":false}')"

echo "=== 2. clean output: zeros, no tool error ==="
summarize clean --osv "$testdata/osv-clean.json" --govulncheck "$testdata/govulncheck-clean.json"
check "osv: 0 of 0" "$(is "$(out clean .osv)" '{"total":0,"with_fix":0,"tool_error":false}')"
check "govulncheck: 0 and 0" "$(is "$(out clean .govulncheck)" '{"reachable":0,"informational":0,"tool_error":false}')"

echo "=== 3. osv-scanner output not in its shape is a tool error, never zero ==="
echo '{}' > "$work/in-osv-empty-object.json"
echo '{"results": null}' > "$work/in-osv-null-results.json"
jq '.results[].packages[] |= with_entries(if .key == "vulnerabilities" then .key = "vulns" else . end)' \
  "$testdata/osv.json" > "$work/in-osv-renamed.json"
echo 'not json' > "$work/in-osv-garbage.json"
for drift in empty-object null-results renamed garbage; do
  summarize "osv-$drift" --osv "$work/in-osv-$drift.json" --govulncheck "$testdata/govulncheck.json"
  check "$drift: exits 0" "$(is "$rc" 0)"
  check "$drift: osv records a tool error" "$(is "$(out "osv-$drift" .osv)" "$osv_error")"
  check "$drift: govulncheck keeps its counts" \
    "$(is "$(out "osv-$drift" .govulncheck)" '{"reachable":1,"informational":1,"tool_error":false}')"
  check "$drift: says why, naming the file" "$(contains "$(stderr_of "osv-$drift")" "in-osv-$drift.json")"
done
check "the renamed key's reason names the missing array" \
  "$(contains "$(stderr_of osv-renamed)" "vulnerabilities")"
summarize osv-absent --govulncheck "$testdata/govulncheck.json"
check "no --osv (osv-scanner did not complete): a tool error" "$(is "$(out osv-absent .osv)" "$osv_error")"

echo "=== 4. govulncheck output not in its shape is a tool error, never zero ==="
jq 'with_entries(if .key == "finding" then .key = "Finding" else . end)' "$testdata/govulncheck.json" \
  > "$work/in-gv-renamed.json"
: > "$work/in-gv-empty.json"
jq 'if has("config") then empty else . end' "$testdata/govulncheck.json" > "$work/in-gv-no-config.json"
jq 'if has("finding") then .finding.trace = [] else . end' "$testdata/govulncheck.json" > "$work/in-gv-no-trace.json"
for drift in renamed empty no-config no-trace; do
  summarize "gv-$drift" --osv "$testdata/osv.json" --govulncheck "$testdata/govulncheck-clean.json" \
    --govulncheck "$work/in-gv-$drift.json"
  check "$drift: exits 0" "$(is "$rc" 0)"
  check "$drift: govulncheck records a tool error" "$(is "$(out "gv-$drift" .govulncheck)" "$govulncheck_error")"
  check "$drift: osv keeps its counts" "$(is "$(out "gv-$drift" .osv.total)" 5)"
  check "$drift: says why, naming the file" "$(contains "$(stderr_of "gv-$drift")" "in-gv-$drift.json")"
done
check "the renamed record's reason names its key" "$(contains "$(stderr_of gv-renamed)" "Finding")"
summarize gv-absent --osv "$testdata/osv.json"
check "no --govulncheck (govulncheck failed in a module): a tool error" \
  "$(is "$(out gv-absent .govulncheck)" "$govulncheck_error")"

echo "=== 5. usage errors exit 2 ==="
rc=0
"$run_script" summarize --osv "$testdata/osv.json" > /dev/null 2>&1 || rc=$?
check "summarize without --out exits 2" "$(is "$rc" 2)"
rc=0
"$run_script" summarize --out "$work/x.json" --nope > /dev/null 2>&1 || rc=$?
check "an unknown flag exits 2" "$(is "$rc" 2)"
rc=0
"$run_script" > /dev/null 2>&1 || rc=$?
check "no subcommand exits 2" "$(is "$rc" 2)"

# --- report ------------------------------------------------------------------------------------
# The zizmor findings of a pull request run, built by zizmor-findings.sh from zizmor's own output.
"$script_dir/zizmor-findings.sh" --head "$testdata/zizmor-ignores.json" --base "$testdata/zizmor.json" \
  --no-ignores "$testdata/zizmor-ignores-no-ignores.json" --out "$work/zizmor-findings.json" > /dev/null
summarize deps --osv "$testdata/osv.json" --govulncheck "$testdata/govulncheck.json" \
  --govulncheck "$testdata/govulncheck-clean.json"

# report NAME ZIZMOR DEPS [ARG…]: runs `security-run.sh report` for run 42, a pull request with
# both checks report-only, into $work/NAME.json; its stdout (the step summary) in $work/NAME.md,
# stderr in $work/NAME.stderr.
report() {
  local name="$1" zizmor="$2" deps="$3"
  shift 3
  rc=0
  "$run_script" report --zizmor "$zizmor" --deps "$deps" --run-id 42 --event pull_request \
    --head abc123 --base def456 --report-only-zizmor true --report-only-dependencies true \
    --workflows success --dependencies success --out "$work/$name.json" "$@" \
    > "$work/$name.md" 2> "$work/$name.stderr" || rc=$?
}

echo "=== 6. report: the run's security report from its two artifacts ==="
report full "$work/zizmor-findings.json" "$work/deps.json"
check "exits 0" "$(is "$rc" 0)"
check "the run's identity and each check's flag" \
  "$(is "$(out full '[.run_id, .event, .head_sha, .base_sha, .report_only]')" '[42,"pull_request","abc123","def456",{"zizmor":true,"dependencies":true}]')"
check "zizmor: head count, new count and the count per audit" \
  "$(is "$(out full .zizmor)" '{"head_count":6,"new_count":0,"by_audit":{"artipacked":1,"excessive-permissions":1,"template-injection":2,"unpinned-uses":2},"tool_error":false}')"
check "osv and govulncheck as summarized" \
  "$(is "$(out full '[.osv, .govulncheck]')" '[{"total":5,"with_fix":3,"tool_error":false},{"reachable":1,"informational":1,"tool_error":false}]')"
check "no tool error" "$(is "$(out full .tool_error)" false)"
check "the scanner jobs' results" "$(is "$(out full .gate)" '{"workflows":"success","dependencies":"success"}')"
check "the step summary's rows" "$(is "$(cat "$work/full.md")" "## Security (report_only: zizmor true, dependencies true)

| check | result |
| --- | --- |
| zizmor | 6 findings on this tree, 0 new against the base |
| zizmor by audit | artipacked 1, excessive-permissions 1, template-injection 2, unpinned-uses 2 |
| osv-scanner | 5 vulnerabilities, 3 with a fixed version |
| govulncheck | 1 reachable, 1 informational |")"

echo "=== 7. report: no base, no flags, and a run with no base to compare ==="
"$script_dir/zizmor-findings.sh" --head "$testdata/zizmor.json" --out "$work/zizmor-push.json"
report push "$work/zizmor-push.json" "$work/deps.json" --event push --base "" --report-only-zizmor "" \
  --report-only-dependencies ""
check "base_sha and both flags are null" \
  "$(is "$(out push '[.base_sha, .report_only]')" '[null,{"zizmor":null,"dependencies":null}]')"
check "new_count is null" "$(is "$(out push .zizmor.new_count)" null)"
check "the summary says there is no base and the flags are unknown" \
  "$( [ "$(contains "$(cat "$work/push.md")" '^| zizmor | 8 findings on this tree, no base to compare |$')" = true ] &&
    [ "$(contains "$(cat "$work/push.md")" '^## Security (report_only: zizmor unknown, dependencies unknown)$')" = true ] && echo true || echo false)"
report mixed "$work/zizmor-findings.json" "$work/deps.json" --report-only-zizmor false
check "one check promoted: each flag is recorded on its own" \
  "$(is "$(out mixed .report_only)" '{"zizmor":false,"dependencies":true}')"
check "and the summary names both" \
  "$(contains "$(cat "$work/mixed.md")" '^## Security (report_only: zizmor false, dependencies true)$')"

echo "=== 8. report: a tool error or a missing or malformed artifact is that half's tool error ==="
echo '{"tool_error": true, "head_count": null, "new_count": null}' > "$work/in-zizmor-error.json"
report zizmor-error "$work/in-zizmor-error.json" "$work/deps.json"
check "zizmor's recorded tool error" \
  "$(is "$(out zizmor-error .zizmor)" '{"head_count":null,"new_count":null,"by_audit":null,"tool_error":true}')"
check "is the report's tool error" "$(is "$(out zizmor-error .tool_error)" true)"
check "and reads 'tool error' in the summary" "$(contains "$(cat "$work/zizmor-error.md")" '^| zizmor | tool error |$')"
report zizmor-missing "$work/nowhere.json" "$work/deps.json"
check "a missing zizmor-findings.json is a zizmor tool error" "$(is "$(out zizmor-missing .zizmor.tool_error)" true)"
check "and says so" "$(contains "$(stderr_of zizmor-missing)" "nowhere.json")"
jq '.head_total = .head_count | del(.head_count)' "$work/zizmor-findings.json" > "$work/in-zizmor-renamed.json"
report zizmor-renamed "$work/in-zizmor-renamed.json" "$work/deps.json"
check "a renamed head_count is a zizmor tool error" "$(is "$(out zizmor-renamed .zizmor.tool_error)" true)"
check "the dependency halves are unaffected" "$(is "$(out zizmor-renamed '[.osv.tool_error, .govulncheck.tool_error]')" '[false,false]')"
report deps-missing "$work/zizmor-findings.json" "$work/nowhere.json"
check "a missing deps-summary.json is a tool error of both dependency halves" \
  "$(is "$(out deps-missing '[.osv, .govulncheck]')" "[$osv_error,$govulncheck_error]")"
check "is the report's tool error" "$(is "$(out deps-missing .tool_error)" true)"
jq '.osv = {count: 5, with_fix: 3, tool_error: false}' "$work/deps.json" > "$work/in-deps-renamed.json" || true
report deps-renamed "$work/zizmor-findings.json" "$work/in-deps-renamed.json"
check "an osv half with total renamed is an osv tool error" "$(is "$(out deps-renamed .osv)" "$osv_error")"
check "govulncheck's half stands" "$(is "$(out deps-renamed .govulncheck.reachable)" 1)"
summarize deps-osv-error --govulncheck "$testdata/govulncheck.json"
report deps-error "$work/zizmor-findings.json" "$work/deps-osv-error.json"
check "a recorded osv tool error reads 'tool error' in the summary" \
  "$(contains "$(cat "$work/deps-error.md")" '^| osv-scanner | tool error |$')"

echo "=== 9. report: usage errors exit 2 ==="
report bad-run "$work/zizmor-findings.json" "$work/deps.json" --run-id x
check "a run id that is not a number exits 2" "$(is "$rc" 2)"
report bad-flag "$work/zizmor-findings.json" "$work/deps.json" --report-only-dependencies maybe
check "a flag other than true, false or empty exits 2" "$(is "$rc" 2)"

# --- enforce -----------------------------------------------------------------------------------
# enforce NAME: runs `security-run.sh enforce` on $work/NAME.json (a report written above); output
# in $work/NAME.enforce, exit code in $rc.
enforce() {
  rc=0
  "$run_script" enforce "$work/$1.json" > "$work/$1.enforce" 2>&1 || rc=$?
}

echo "=== 10. enforce: the security job fails only for a promoted check whose gate failed ==="
report open-failed "$work/zizmor-findings.json" "$work/deps.json" --workflows failure --dependencies failure
enforce open-failed
check "both checks report-only, both gates failed: exits 0" "$(is "$rc" 0)"
check "and says neither is enforced" \
  "$(contains "$(cat "$work/open-failed.enforce")" "^zizmor: report-only (report_only.zizmor is true); not enforced$")"
report zizmor-failed "$work/zizmor-findings.json" "$work/deps.json" --report-only-zizmor false \
  --workflows failure --dependencies failure
enforce zizmor-failed
check "zizmor promoted and its workflows job failed: exits 1" "$(is "$rc" 1)"
check "naming the check and the job" \
  "$(contains "$(cat "$work/zizmor-failed.enforce")" "zizmor is blocking (report_only.zizmor is false) and its workflows job concluded failure")"
check "the report-only dependency gate's failure is not enforced" \
  "$(contains "$(cat "$work/zizmor-failed.enforce")" "^dependencies: report-only")"
report zizmor-passed "$work/zizmor-findings.json" "$work/deps.json" --report-only-zizmor false \
  --workflows success --dependencies failure
enforce zizmor-passed
check "zizmor promoted and its job succeeded, dependencies report-only and failed: exits 0" "$(is "$rc" 0)"
report dependencies-skipped "$work/zizmor-findings.json" "$work/deps.json" --report-only-dependencies false \
  --dependencies skipped
enforce dependencies-skipped
check "a promoted check whose job did not run is not a pass: exits 1" "$(is "$rc" 1)"
report flags-unknown "$work/zizmor-findings.json" "$work/deps.json" --report-only-zizmor "" \
  --report-only-dependencies "" --workflows failure --dependencies failure
enforce flags-unknown
check "flags unknown (the window job did not run): report-only, exits 0" "$(is "$rc" 0)"
rc=0
"$run_script" enforce "$work/nowhere.json" > /dev/null 2>&1 || rc=$?
check "a missing report exits 2" "$(is "$rc" 2)"
jq 'del(.gate)' "$work/zizmor-failed.json" > "$work/no-gate.json"
enforce no-gate
check "a report without its gate results exits 2" "$(is "$rc" 2)"

echo "=== 11. window-flags: the window job's reading of the file, one output line per check ==="
# run_flags ARG…: runs `security-run.sh window-flags ARG…` from the current directory; stdout in
# $flags_out, stderr in $flags_err, exit code in $rc.
run_flags() {
  rc=0
  flags_out=$("$run_script" window-flags "$@" 2> "$work/flags.err") || rc=$?
  flags_err=$(< "$work/flags.err")
}
mkdir -p "$work/flags"
echo '{"report_only": {"zizmor": true, "dependencies": false}}' > "$work/flags/split.json"
run_flags "$work/flags/split.json"
check "exits 0" "$(is "$rc" 0)"
check "prints one GITHUB_OUTPUT line per check" "$(is "$flags_out" "zizmor=true
dependencies=false")"
check "and no note" "$(is "$flags_err" "")"
echo '{"report_only": true}' > "$work/flags/bare.json"
run_flags "$work/flags/bare.json"
check "a bare boolean is that value for each check" "$(is "$flags_out" "zizmor=true
dependencies=true")"
check "and is named on stderr" "$(contains "$flags_err" "report_only is one boolean")"
run_flags "$work/flags/nowhere.json"
check "no file: exits 0, every check report-only" "$( [ "$rc" = 0 ] && [ "$flags_out" = "zizmor=true
dependencies=true" ] && echo true || echo false)"
check "and names the file" "$(contains "$flags_err" "nowhere.json: missing")"
echo '{"report_only": {"zizmor": 0, "dependencies": false}}' > "$work/flags/number.json"
run_flags "$work/flags/number.json"
check "a number is not false: that check stays report-only" "$(is "$flags_out" "zizmor=true
dependencies=false")"
run_flags "$script_dir/../security-window.json"
check "the checked-in window file is in the per-check shape: no note" "$(is "$flags_err" "")"
rc=0
"$run_script" window-flags > /dev/null 2>&1 || rc=$?
check "window-flags without a file exits 2" "$(is "$rc" 2)"

echo "=== 12. the base: a pull request is judged against its merge commit's first parent, and reads its flags there ==="
# A real repository. Main promotes zizmor (A1); a pull request branched from A1 sets it back to
# report-only (P); main moves on (A2), so the pull request's recorded base (A1) is stale; GitHub's
# merge commit M joins A2 and P, as refs/pull/<n>/merge does. The run checks M out at depth 1 from
# an origin, as actions/checkout does, so HEAD is shallow and its parents are not local.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
g() { git -c user.name=test -c user.email=test@example.invalid -c commit.gpgsign=false -c init.defaultBranch=main "$@"; }
src="$work/git/src"
mkdir -p "$src/.github"
g -C "$src" init -q
echo '{"report_only": {"zizmor": false, "dependencies": true}}' > "$src/.github/security-window.json"
echo 1 > "$src/other.txt"
g -C "$src" add -A
g -C "$src" commit -qm "A1: main promotes zizmor"
a1=$(g -C "$src" rev-parse HEAD)
g -C "$src" checkout -qb pr
echo '{"report_only": {"zizmor": true, "dependencies": true}}' > "$src/.github/security-window.json"
g -C "$src" commit -qam "P: the pull request sets zizmor back to report-only"
g -C "$src" checkout -q main
echo 2 > "$src/other.txt"
g -C "$src" commit -qam "A2: main moves on"
a2=$(g -C "$src" rev-parse HEAD)
g -C "$src" checkout -q --detach main
g -C "$src" merge -q --no-ff pr -m "M: GitHub's merge commit"
g -C "$src" branch pull-merge HEAD
g clone -q --bare "$src" "$work/git/origin.git"
g clone -q --depth 1 --branch pull-merge "file://$work/git/origin.git" "$work/git/clone"
g clone -q --depth 1 --branch main "file://$work/git/origin.git" "$work/git/main"
# base_commit DIR ARG…: `security-run.sh base-commit ARG…` run in DIR; its output in $base_out,
# exit code in $rc.
base_commit() {
  local dir=$1
  shift
  rc=0
  base_out=$(cd "$dir" && "$run_script" base-commit "$@" 2>&1) || rc=$?
}
base_commit "$work/git/clone" pull_request
check "a pull request's base is its merge commit's first parent (main's tip), not its recorded base" \
  "$( [ "$rc" = 0 ] && [ "$base_out" = "$a2" ] && [ "$base_out" != "$a1" ] && echo true || echo false)"
base_commit "$work/git/clone" merge_group "$a1"
check "a merge group's base is the event's base_sha" "$(is "$base_out" "$a1")"
base_commit "$work/git/clone" push
check "a push, a schedule or a dispatch has no base: prints nothing" \
  "$( [ "$rc" = 0 ] && [ -z "$base_out" ] && echo true || echo false)"
base_commit "$work/git/main" pull_request
check "a pull_request run whose HEAD is not a merge commit fails, naming it" \
  "$( [ "$rc" = 2 ] && [ "$(contains "$base_out" "not a merge commit")" = true ] && echo true || echo false)"
base_commit "$work/git/clone" merge_group
check "merge_group without its base_sha exits 2" "$(is "$rc" 2)"
cd "$work/git/clone"
run_flags .github/security-window.json --at "$a2"
check "a pull request that sets zizmor back to report-only over a promoted base still reads the base's false" \
  "$( [ "$rc" = 0 ] && [ "$flags_out" = "zizmor=false
dependencies=true" ] && [ -z "$flags_err" ] && echo true || echo false)"
run_flags .github/security-window.json
check "the tree itself says true: the base read is what keeps zizmor blocking" "$(is "$flags_out" "zizmor=true
dependencies=true")"
run_flags .github/nowhere.json --at "$a2"
check "a base with no window file: every check report-only, named with the commit" \
  "$( [ "$flags_out" = "zizmor=true
dependencies=true" ] && [ "$(contains "$flags_err" ".github/nowhere.json at ${a2:0:12}: missing")" = true ] &&
  echo true || echo false)"
cd - > /dev/null
unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM

summary "security-run.sh reads the base and its flags, summarizes the dependency scanners, and assembles and enforces the security report"
