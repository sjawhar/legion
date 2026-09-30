#!/usr/bin/env bash
# Tests for `security-report.sh`: one case per decision rule of the report-only window (R2–R10),
# the window itself (R1, R11, R12, the window not yet started), and the ways a read can fail
# (R13, R14, a server error).
#
# `gh` is a stub on PATH (the dotfiles scripts/tests/test_docs_pr_gate.py pattern): it logs its
# argv to $STUB_LOG and answers each GitHub API path from the fixture files a case writes under
# its own directory. A fixture named FILE.403 makes that read a 403, FILE.500 a server error.
# Artifacts are real zips built with `zip`, as `gh api …/artifacts/<id>/zip` returns them, and
# their contents are the producers' own output: each zizmor-findings.json is
# .github/scripts/zizmor-findings.sh run over zizmor 1.30.1's findings
# (.github/scripts/testdata/zizmor*.json), and each security-report.json is
# .github/scripts/deps-summary.sh's, from that and the summary of osv-scanner's and govulncheck's
# own output (.github/scripts/testdata/).
#
# Run from anywhere: scripts/security-report.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
report="$script_dir/security-report.sh"
producers="$script_dir/../.github/scripts"
testdata="$producers/testdata"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/../.github/scripts/test-lib.sh"

mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$STUB_LOG"
[ "$1" = api ] || { echo "stub gh: only api is stubbed" >&2; exit 99; }
path=$2
d=$STUB_DIR
page=1
case "$path" in *[?\&]page=*) page=$(sed -E 's/.*[?&]page=([0-9]+).*/\1/' <<<"$path") ;; esac
not_found() { echo '{"message":"Not Found","status":"404"}'; echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
# serve FILE EMPTY: FILE for page 1, EMPTY for later pages or when FILE is absent.
serve() {
  if [ -e "$1.403" ]; then
    echo '{"message":"Resource not accessible by integration","status":"403"}'
    echo "gh: Resource not accessible by integration (HTTP 403)" >&2
    exit 1
  fi
  if [ -e "$1.500" ]; then echo "gh: Server Error (HTTP 500)" >&2; exit 1; fi
  if [ "$page" = 1 ] && [ -e "$1" ]; then cat "$1"; else printf '%s\n' "$2"; fi
}
id_in() { sed -E "s|.*/$1/([0-9]+).*|\\1|" <<<"$path"; }
case "$path" in
  repos/*/actions/workflows\?*) serve "$d/workflows.json" '{"total_count":0,"workflows":[]}' ;;
  repos/*/actions/workflows/*/runs\?*) serve "$d/runs.json" '{"total_count":0,"workflow_runs":[]}' ;;
  repos/*/actions/runs/*/artifacts*) serve "$d/artifacts-run-$(id_in runs).json" '{"total_count":0,"artifacts":[]}' ;;
  repos/*/actions/artifacts/*/zip)
    zip="$d/zip-$(id_in artifacts).zip"
    [ -e "$zip" ] || not_found
    cat "$zip" ;;
  repos/*/actions/artifacts\?*)
    name=$(sed -E 's/.*[?&]name=([^&]+).*/\1/' <<<"$path")
    serve "$d/markers-$name.json" '{"total_count":0,"artifacts":[]}' ;;
  repos/*/pulls\?*) serve "$d/pulls.json" '[]' ;;
  repos/*/pulls/comments\?*) serve "$d/comments.json" '[]' ;;
  repos/*/code-scanning/alerts*) serve "$d/codeql-alerts.json" '[]' ;;
  repos/*/code-scanning/analyses*) serve "$d/codeql-analyses.json" '[]' ;;
  repos/*/secret-scanning/alerts*) serve "$d/secret-alerts.json" '[]' ;;
  repos/*/contents/.github/security-window.json\?ref=main)
    [ -e "$d/window.json" ] || not_found
    printf '{"encoding":"base64","content":"%s"}\n' "$(base64 -w0 "$d/window.json")" ;;
  *) not_found ;;
esac
STUB
chmod +x "$work/bin/gh"

iso() { date -u -d "$1" +%Y-%m-%dT%H:%M:%SZ; }
sha() { printf '%s' "$1" | sha1sum | cut -c1-40; }
# append FILE JQ-PATH OBJECT: appends OBJECT to the array at JQ-PATH in FILE.
append() { jq --argjson x "$3" "$2 += [\$x]" "$1" > "$1.next" && mv "$1.next" "$1"; }

# setup NAME START: a case directory with the Security and CodeQL workflows, report_only true,
# the window's first push run on main (run 1000) at START with a clean report, and two CodeQL
# analyses on main. Sets $d.
setup() {
  d="$work/$1"
  mkdir -p "$d"
  cat > "$d/workflows.json" <<'JSON'
{"total_count": 3, "workflows": [
  {"id": 100, "name": "Tests", "path": ".github/workflows/pr-and-main.yaml"},
  {"id": 101, "name": "Security", "path": ".github/workflows/security.yaml"},
  {"id": 102, "name": "CodeQL", "path": ".github/workflows/codeql.yaml"}]}
JSON
  echo '{"report_only": true}' > "$d/window.json"
  echo '{"total_count": 0, "workflow_runs": []}' > "$d/runs.json"
  cat > "$d/codeql-analyses.json" <<'JSON'
[{"id": 1, "ref": "refs/heads/main", "category": "/language:go", "tool": {"name": "CodeQL"}},
 {"id": 2, "ref": "refs/heads/main", "category": "/language:javascript-typescript", "tool": {"name": "CodeQL"}}]
JSON
  start=$(iso "$2")
  add_run 1000 push main "$start"
  add_report 1000 "$clean_report"
}

# add_run ID EVENT BRANCH CREATED [CONCLUSION]
add_run() {
  append "$d/runs.json" .workflow_runs "$(jq -cn --argjson id "$1" --arg event "$2" --arg branch "$3" \
    --arg created "$4" --arg sha "$(sha "$1")" --arg conclusion "${5:-success}" '{id: $id, event: $event,
    head_branch: $branch, head_sha: $sha, created_at: $created, status: "completed",
    conclusion: $conclusion, pull_requests: []}')"
}

# The findings zizmor 1.30.1 reported over the fixture tree (testdata/zizmor.json, described in
# .github/scripts/zizmor-findings.test.sh), one json-v1 finding each: pick IDENT FILE [NTH [FIXTURE]].
pick() {
  jq -ce --arg ident "$1" --arg file "$2" --argjson n "${3:-0}" '[.[] | select(.ident == $ident and
    ([.locations[] | select(.symbolic.kind == "Primary")][0].symbolic.key.Local.verbatim_path
     | endswith("/" + $file)))][$n]' "$testdata/${4:-zizmor.json}"
}
a_artipacked=$(pick artipacked a.yaml)
a_permissions=$(pick excessive-permissions a.yaml)
a_title=$(pick template-injection a.yaml 0)
a_body=$(pick template-injection a.yaml 1)
a_unpinned=$(pick unpinned-uses a.yaml)
b_artipacked=$(pick artipacked b.yaml)
b_unpinned=$(pick unpinned-uses b.yaml)
z_injection=$(pick template-injection action.yml)
# a_body as a --no-ignores audit reports it once its step carries `# zizmor: ignore[template-injection]`.
a_body_ignored=$(pick template-injection a.yaml 1 zizmor-ignores-no-ignores.json)
pool=$(< "$testdata/zizmor.json")

# audit FINDING…: a json-v1 document holding the given findings, as one zizmor audit writes it.
audit() { jq -cs . <<<"$(printf '%s\n' "$@")"; }

# zizmor_findings HEAD [BASE [NO_IGNORES]]: zizmor-findings.sh's output for a run whose audits
# reported those json-v1 documents: HEAD alone on main, HEAD against BASE on a pull request, whose
# head the workflow also audits with --no-ignores (NO_IGNORES).
zizmor_findings() {
  local dir args
  dir=$(mktemp -d "$work/zizmor.XXXXXX")
  printf '%s' "$1" > "$dir/head.json"
  args=(--head "$dir/head.json" --out "$dir/findings.json")
  if [ $# -ge 2 ]; then printf '%s' "$2" > "$dir/base.json" && args+=(--base "$dir/base.json"); fi
  if [ $# -ge 3 ]; then printf '%s' "$3" > "$dir/no-ignores.json" && args+=(--no-ignores "$dir/no-ignores.json"); fi
  "$producers/zizmor-findings.sh" "${args[@]}"
  cat "$dir/findings.json"
}

# The dependency summaries deps-summary.sh writes from the scanners' own output: nothing found
# (clean), the vulnerable fixture modules (vulnerable: osv 5 with 3 fixable, govulncheck 1
# reachable and 1 informational), and neither scanner completing (error).
mkdir -p "$work/deps"
"$producers/deps-summary.sh" summarize --osv "$testdata/osv-clean.json" \
  --govulncheck "$testdata/govulncheck-clean.json" --out "$work/deps/clean.json"
"$producers/deps-summary.sh" summarize --osv "$testdata/osv.json" --govulncheck "$testdata/govulncheck.json" \
  --out "$work/deps/vulnerable.json"
"$producers/deps-summary.sh" summarize --out "$work/deps/error.json" 2> /dev/null

# security_report ZIZMOR_FINDINGS DEPS: the security-report.json deps-summary.sh report writes for
# a run whose artifacts are that zizmor-findings.json and the DEPS summary (clean, vulnerable, error).
security_report() {
  local dir
  dir=$(mktemp -d "$work/report.XXXXXX")
  printf '%s' "$1" > "$dir/zizmor-findings.json"
  "$producers/deps-summary.sh" report --zizmor "$dir/zizmor-findings.json" --deps "$work/deps/$2.json" \
    --run-id 1 --event push --head 0 --base "" --report-only true --workflows success --dependencies success \
    --out "$dir/security-report.json" > /dev/null
  cat "$dir/security-report.json"
}
clean_report=$(security_report "$(zizmor_findings '[]')" clean)

# zip_artifact ARTIFACT_ID FILENAME JSON: the zip GitHub serves for an artifact.
zip_artifact() {
  local dir="$d/zip-src-$1"
  mkdir -p "$dir"
  printf '%s\n' "$3" > "$dir/$2"
  (cd "$dir" && zip -q -X "$d/zip-$1.zip" "$2")
}

# artifact_json ID NAME RUN_ID CREATED
artifact_json() {
  local branch
  branch=$(jq -r --argjson id "$3" '[.workflow_runs[] | select(.id == $id)][0].head_branch // "main"' "$d/runs.json")
  jq -cn --argjson id "$1" --arg name "$2" --argjson run "$3" --arg created "$4" --arg branch "$branch" \
    --arg sha "$(sha "$3")" '{id: $id, name: $name, expired: false, created_at: $created,
    workflow_run: {id: $run, head_branch: $branch, head_sha: $sha}}'
}

run_created() { jq -r --argjson id "$1" '[.workflow_runs[] | select(.id == $id)][0].created_at' "$d/runs.json"; }

# add_report RUN_ID JSON / add_findings RUN_ID JSON: the run's security-report / zizmor-findings.
add_report() { add_run_artifact "$1" 1 security-report security-report.json "$2"; }
add_findings() { add_run_artifact "$1" 2 zizmor-findings zizmor-findings.json "$2"; }
add_run_artifact() {
  local file="$d/artifacts-run-$1.json" id=$(($1 * 10 + $2))
  [ -e "$file" ] || echo '{"total_count": 0, "artifacts": []}' > "$file"
  append "$file" .artifacts "$(artifact_json "$id" "$3" "$1" "$(run_created "$1")")"
  zip_artifact "$id" "$4" "$5"
}

# add_marker NAME RUN_ID [JSON]: a marker artifact of a run (zizmor-new, zizmor-tool-error or
# dependencies-tool-error); its zip holds JSON (zizmor-findings.json for zizmor-new,
# security-report.json otherwise).
add_marker() {
  local file="$d/markers-$1.json" id file_name=security-report.json
  case "$1" in
    zizmor-new) id=$(($2 * 10 + 3)) file_name=zizmor-findings.json ;;
    dependencies-tool-error) id=$(($2 * 10 + 4)) ;;
    zizmor-tool-error) id=$(($2 * 10 + 5)) ;;
  esac
  [ -e "$file" ] || echo '{"total_count": 0, "artifacts": []}' > "$file"
  append "$file" .artifacts "$(artifact_json "$id" "$1" "$2" "$(run_created "$2")")"
  zip_artifact "$id" "$file_name" "${3:-{\}}"
}

# add_pr NUMBER BRANCH CREATED MERGED
add_pr() {
  [ -e "$d/pulls.json" ] || echo '[]' > "$d/pulls.json"
  append "$d/pulls.json" . "$(jq -cn --argjson n "$1" --arg ref "$2" --arg created "$3" --arg merged "$4" \
    '{number: $n, head: {ref: $ref, sha: "head\($n)"}, created_at: $created, merged_at: $merged,
      updated_at: $merged, merge_commit_sha: "merge\($n)"}')"
}

# pr_with_findings NUMBER BRANCH FIRST_RUN LAST_RUN NEW0 LAST_HEAD LAST_NO_IGNORES: a PR opened 7
# days ago and merged 5 days ago, with two pull_request runs against a base where zizmor found
# nothing. Its first run's audit found NEW0 (a json-v1 document), all of it new, so the run has a
# zizmor-new marker; its last run's head audit found LAST_HEAD and its --no-ignores audit
# LAST_NO_IGNORES.
pr_with_findings() {
  local n="$1" branch="$2" first="$3" last="$4" first_findings last_findings
  add_pr "$n" "$branch" "$(iso '7 days ago')" "$(iso '5 days ago')"
  add_run "$first" pull_request "$branch" "$(iso '6 days ago')"
  first_findings=$(zizmor_findings "$5" '[]' "$5")
  add_marker zizmor-new "$first" "$first_findings"
  add_findings "$first" "$first_findings"
  add_run "$last" pull_request "$branch" "$(iso '5 days ago - 1 hour')"
  last_findings=$(zizmor_findings "$6" '[]' "$7")
  add_findings "$last" "$last_findings"
  if [ "$(jq .new_count <<<"$last_findings")" != 0 ]; then
    add_marker zizmor-new "$last" "$last_findings"
  fi
}

# add_comment ID IN_REPLY_TO BODY
add_comment() {
  [ -e "$d/comments.json" ] || echo '[]' > "$d/comments.json"
  append "$d/comments.json" . "$(jq -cn --argjson id "$1" --argjson reply "$2" --arg body "$3" \
    --arg created "$(iso '4 days ago')" '{id: $id, in_reply_to_id: $reply, body: $body,
    created_at: $created, updated_at: $created, pull_request_url: "https://api.github.com/repos/sjawhar/legion/pulls/9"}')"
}

# run_report [ARG…]: runs the script against $d; output in $output, exit code in $rc.
run_report() {
  rc=0
  output=$(PATH="$work/bin:$PATH" STUB_DIR="$d" STUB_LOG="$d/calls.log" "$report" "$@" 2>&1) || rc=$?
}

has() { grep -qF -- "$1" <<<"$output" && echo true || echo false; }
lacks() { grep -qF -- "$1" <<<"$output" && echo false || echo true; }
closes_on() { date -u -d "@$(($(date -u -d "$1" +%s) + $2 * 86400))" +%Y-%m-%d; }

echo "=== R1. window open: the run table, the closing date, no decision ==="
setup r1 "3 days ago"
add_run 1001 schedule main "$(iso '2 days ago')"
add_report 1001 "$(security_report "$(zizmor_findings "$(audit "$a_title" "$a_body" "$b_artipacked")")" vulnerable)"
add_run 1002 schedule main "$(iso '1 day ago')"
add_report 1002 "$clean_report"
add_run 1003 pull_request some-branch "$(iso '1 day ago')"
add_report 1003 "$(security_report "$(zizmor_findings "$(audit "$a_title")" '[]' "$(audit "$a_title")")" clean)"
echo '[{"number": 1, "state": "open", "secret_type": "example", "secret": "not-a-real-value-123"}]' > "$d/secret-alerts.json"
run_report --repo sjawhar/legion
check "exits 0" "$(is "$rc" 0)"
check "the push run on main has a row" "$(has "| 1000 | push | $(sha 1000 | cut -c1-7) | 0/- | 0/0 | 0/0 | no |")"
check "the first scheduled run's row carries its counts" \
  "$(has "| 1001 | schedule | $(sha 1001 | cut -c1-7) | 3/- | 5/3 | 1/1 | no |")"
check "the second scheduled run has a row" "$(has "| 1002 | schedule |")"
check "a pull request run is not a main row" "$(lacks "| 1003 |")"
check "says when the window closes" "$(has "window closes $(closes_on "$start" 14)")"
check "prints no decision" "$(lacks "DECISION:")"
check "counts secret-scanning alerts without their values" \
  "$( [ "$(has "secret-scanning alerts: 1 open")" = true ] && [ "$(lacks "not-a-real-value-123")" = true ] && echo true || echo false)"
check "lists the CodeQL analyses on main by category" "$(has "CodeQL analyses on main: 2 (/language:go, /language:javascript-typescript)")"

echo "=== R2. rule 1a: the window closed with findings on main ==="
setup r2 "15 days ago"
add_run 1001 schedule main "$(iso '1 day ago')"
add_report 1001 "$(security_report "$(zizmor_findings "$pool")" clean)"
run_report
check "exits 1" "$(is "$rc" 1)"
check "holds zizmor on main's count" "$(has "DECISION: HOLD zizmor — 8 findings on main (fix each, or ignore it with a reason: workflows in .github/zizmor.yml, composite actions inline)")"

echo "=== R3. rule 1b: precision 0.80 over 5 dispositions promotes ==="
setup r3 "15 days ago"
# #11's three findings are gone by its last run. #12 ignores a.yaml's body step inline (its
# --no-ignores audit reports the step with the comment) and fixes b.yaml's checkout.
pr_with_findings 11 feat-a 2001 2002 "$(audit "$a_artipacked" "$a_unpinned" "$a_permissions")" '[]' '[]'
pr_with_findings 12 feat-b 2011 2012 "$(audit "$a_body" "$b_artipacked")" '[]' "$(audit "$a_body_ignored")"
run_report
check "exits 1 (the window closed while report_only is true)" "$(is "$rc" 1)"
check "precision 0.80 (4 fixed / 1 ignored)" "$(has "precision 0.80 (4 fixed / 1 ignored)")"
check "promotes zizmor" "$(has "DECISION: PROMOTE zizmor")"
check "not on rule 1a alone" "$(lacks "DECISION: PROMOTE zizmor (rule 1a alone)")"
check "a PR row counts its dispositions" "$(has "| #12 | 2011 | 2012 | 2 | 1 | 1 | 0 |")"
check "promotes the dependencies with no tool error (rule 2)" "$(has "DECISION: PROMOTE dependencies")"

echo "=== R4. rule 1b: precision 0.40 holds ==="
setup r4 "15 days ago"
pr_with_findings 31 feat-c 3001 3002 \
  "$(audit "$a_artipacked" "$a_body" "$b_unpinned" "$b_artipacked" "$a_permissions")" \
  '[]' "$(audit "$a_artipacked" "$a_body_ignored" "$b_unpinned")"
run_report
check "precision 0.40" "$(has "precision 0.40 (2 fixed / 3 ignored)")"
check "holds zizmor" "$(has "DECISION: HOLD zizmor")"

echo "=== R5. rule 1b with fewer than 5 dispositions: rule 1a alone decides ==="
setup r5 "15 days ago"
# a.yaml's checkout is fixed; its unpinned-uses is still new against the base at the last run.
pr_with_findings 41 feat-d 4001 4002 "$(audit "$a_artipacked" "$a_unpinned")" "$(audit "$a_unpinned")" \
  "$(audit "$a_unpinned")"
run_report
check "a PR row counts a finding still new as merged with findings" "$(has "| #41 | 4001 | 4002 | 2 | 1 | 0 | 1 |")"
check "precision undecided (1 of 5 dispositions)" "$(has "precision undecided (1 of 5 dispositions)")"
check "promotes on rule 1a alone" "$(has "DECISION: PROMOTE zizmor (rule 1a alone)")"

echo "=== R6. the baseline is excluded: a PR whose first run found nothing new ==="
setup r6 "15 days ago"
add_pr 51 feat-f "$(iso '7 days ago')" "$(iso '5 days ago')"
add_run 5001 pull_request feat-f "$(iso '6 days ago')"
add_findings 5001 "$(zizmor_findings "$pool" "$pool" "$pool")"
add_run 5002 pull_request feat-f "$(iso '5 days ago - 1 hour')"
late=$(zizmor_findings "$pool" "$(audit "$z_injection" "$a_artipacked" "$a_permissions" "$a_body" "$a_unpinned" \
  "$b_artipacked" "$b_unpinned")" "$pool")
add_marker zizmor-new 5002 "$late"
add_findings 5002 "$late"
add_pr 52 feat-g "$(iso '7 days ago')" "$(iso '5 days ago')"
add_pr 53 feat-old "$(iso '30 days ago')" "$(iso '20 days ago')"
run_report
check "lists #51 and #52 as having no new findings" "$(has "no new findings: #51, #52")"
check "a PR merged before the window is not counted" "$(lacks "#53")"
check "adds nothing to precision" "$(has "precision undecided (0 of 5 dispositions)")"

echo "=== R7. kill: an audit ignored 3 times ==="
setup r7 "15 days ago"
pr_with_findings 61 feat-h 6001 6002 "$(audit "$a_artipacked")" '[]' "$(audit "$a_artipacked")"
pr_with_findings 62 feat-i 6011 6012 "$(audit "$b_artipacked")" '[]' "$(audit "$b_artipacked")"
pr_with_findings 63 feat-j 6021 6022 "$(audit "$a_artipacked" "$z_injection")" '[]' "$(audit "$a_artipacked")"
run_report
check "names the audit to kill and how" "$(has "KILL: artipacked (ignored 3×, fixed 0×) — workflows: rules.artipacked.ignore in .github/zizmor.yml; composite actions: inline comment")"
check "an audit ignored fewer times is not killed" "$(lacks "KILL: template-injection")"

echo "=== R8. rule 2: tool errors in the window hold the dependency scanners ==="
setup r8 "15 days ago"
add_run 7001 pull_request feat-k "$(iso '5 days ago')"
add_marker dependencies-tool-error 7001
add_run 7002 schedule main "$(iso '4 days ago')"
add_report 7002 "$(security_report "$(zizmor_findings '[]')" error)"
add_marker dependencies-tool-error 7002
add_run 7003 pull_request feat-l "$(iso '20 days ago')"
add_marker dependencies-tool-error 7003
add_run 7004 pull_request feat-m "$(iso '3 days ago')"
add_marker zizmor-tool-error 7004
run_report
check "holds the dependencies on the two runs inside the window" "$(has "DECISION: HOLD dependencies — 2 tool errors in the window")"
check "the scheduled run's row says tool error" "$(has "| 7002 | schedule | $(sha 7002 | cut -c1-7) | 0/- | -/- | -/- | yes |")"
check "a zizmor-only tool error is not counted (still 2)" "$(lacks "3 tool errors")"

echo "=== #81. a dependency tool error on a PR's first run leaves its zizmor result counted ==="
setup r81 "15 days ago"
pr_with_findings 81 feat-n 8101 8102 "$(audit "$a_artipacked")" '[]' '[]'
add_marker dependencies-tool-error 8101
run_report
check "the PR's finding counts as fixed" "$(has "| #81 | 8101 | 8102 | 1 | 1 | 0 | 0 |")"
check "precision counts the disposition" "$(has "precision undecided (1 of 5 dispositions)")"
setup r81z "15 days ago"
add_pr 82 feat-o "$(iso '7 days ago')" "$(iso '5 days ago')"
add_run 8201 pull_request feat-o "$(iso '6 days ago')"
add_marker zizmor-tool-error 8201
add_run 8202 pull_request feat-o "$(iso '6 days ago + 1 hour')"
add_marker zizmor-new 8202 "$(zizmor_findings "$(audit "$a_artipacked")" '[]' "$(audit "$a_artipacked")")"
add_run 8203 pull_request feat-o "$(iso '5 days ago - 1 hour')"
add_findings 8203 "$(zizmor_findings '[]' '[]' '[]')"
run_report
check "a zizmor tool error drops that run: the next run is the first" "$(has "| #82 | 8202 | 8203 | 1 | 1 | 0 | 0 |")"

echo "=== #97. two findings of one audit in one file: each is ignored or fixed on its own ==="
setup r97 "15 days ago"
# a.yaml's title step is rewritten; its body step is ignored inline.
pr_with_findings 97 feat-q 9701 9702 "$(audit "$a_title" "$a_body")" '[]' "$(audit "$a_body_ignored")"
run_report
check "one fixed, one ignored" "$(has "| #97 | 9701 | 9702 | 2 | 1 | 1 | 0 |")"

echo "=== R9. rule 3: a fixed CodeQL alert adds the pull_request trigger ==="
setup r9 "15 days ago"
jq -n --arg fixed "$(iso '5 days ago')" --arg old "$(iso '20 days ago')" '[
  {number: 1, state: "fixed", fixed_at: $fixed, rule: {id: "go/path-injection", security_severity_level: "high"},
   most_recent_instance: {location: {path: "packages/envoy/internal/secret-location.go", start_line: 7}}},
  {number: 2, state: "open", fixed_at: null, rule: {id: "js/xss", security_severity_level: "medium"},
   most_recent_instance: {location: {path: "packages/dispatch/src/secret-location.ts", start_line: 9}}},
  {number: 3, state: "fixed", fixed_at: $old, rule: {id: "go/path-injection", security_severity_level: "high"}}]' \
  > "$d/codeql-alerts.json"
run_report
check "promotes the pull_request trigger on the one alert fixed in the window" \
  "$(has "DECISION: PROMOTE codeql pull_request trigger (1 fixed alert)")"
check "counts alerts by rule, severity and state" "$(has "| go/path-injection | high | fixed | 2 |")"
check "publishes no location" "$(lacks "secret-location")"
setup r9-none "15 days ago"
echo '[{"number": 2, "state": "open", "fixed_at": null, "rule": {"id": "js/xss", "security_severity_level": "medium"}}]' > "$d/codeql-alerts.json"
run_report
check "with no fixed alert, holds codeql" "$(has "HOLD codeql")"
setup r9-blocked "15 days ago"
touch "$d/codeql-alerts.json.403" "$d/codeql-analyses.json.403"
run_report
check "a 403 reads BLOCKED with the reason" "$(has "BLOCKED (caller lacks security-events read; the scheduled Security run has it)")"
check "and the decision says codeql: BLOCKED" "$(has "codeql: BLOCKED")"

echo "=== R10. rule 4: a rubric row with 3 not-a-defect and 0 fixed is narrowed ==="
setup r10 "15 days ago"
add_comment 1 null "Security[prompt]: the issue body reaches the planner's instructions unfenced"
add_comment 2 1 "Accepted: not a defect — the body is fenced two lines up"
add_comment 3 null "Security[prompt]: model output is interpolated into a tool prompt"
add_comment 4 3 "Accepted: not a defect — the tool takes no instructions"
add_comment 5 null "Security[prompt]: a PR comment reaches the reviewer prompt"
add_comment 6 5 "Accepted: not a defect — comments are quoted"
add_comment 7 null "Security[supply-chain]: actions/checkout is unpinned"
add_comment 8 7 "Accepted: fixed in abc1234"
add_comment 9 null "Security[supply-chain]: a new dependency floats"
add_comment 10 9 "Accepted: fixed in def5678"
add_comment 11 null "Security[supply-chain]: the base image tag floats"
add_comment 12 11 "Accepted: not a defect — the digest is pinned in the next line"
add_comment 13 null "An ordinary review comment"
run_report
check "narrows the prompt row" "$(has "NARROW: prompt row (3 not a defect, 0 fixed)")"
check "keeps the supply-chain row" "$(has "keep: supply-chain row (1 not a defect, 2 fixed)")"

echo "=== R11. report_only false: the report prints, with no decision ==="
setup r11 "15 days ago"
echo '{"report_only": false}' > "$d/window.json"
run_report
check "exits 0" "$(is "$rc" 0)"
check "prints the run table" "$(has "| 1000 | push |")"
check "prints no decision" "$(lacks "DECISION:")"

echo "=== R12. --decision force with the window open ==="
setup r12 "3 days ago"
run_report --decision force
check "exits 1" "$(is "$rc" 1)"
check "prints the decision under FORCED" "$( [ "$(has "FORCED")" = true ] && [ "$(has "DECISION: PROMOTE zizmor")" = true ] && echo true || echo false)"
run_report --decision none
check "--decision none with the window open exits 0" "$(is "$rc" 0)"

echo "=== R13. no Security workflow ==="
setup r13 "3 days ago"
echo '{"total_count": 1, "workflows": [{"id": 100, "name": "Tests", "path": ".github/workflows/pr-and-main.yaml"}]}' > "$d/workflows.json"
run_report --repo sjawhar/legion
check "exits 2" "$(is "$rc" 2)"
check "says so" "$(has "no Security workflow on sjawhar/legion")"

echo "=== R14. secret-scanning alerts refused: a BLOCKED row, the exit code unchanged ==="
setup r14 "3 days ago"
touch "$d/secret-alerts.json.403"
run_report
check "exits 0, as with the alerts readable" "$(is "$rc" 0)"
check "the row reads BLOCKED with the reason" "$(has "secret-scanning alerts: BLOCKED (caller lacks secret_scanning read; run as an admin)")"

echo "=== the window has not started: no push run on main yet ==="
setup not-started "3 days ago"
echo '{"total_count": 0, "workflow_runs": []}' > "$d/runs.json"
run_report
check "exits 0" "$(is "$rc" 0)"
check "says the window has not started" "$(has "window has not started")"

echo "=== a server error fails loudly ==="
setup server-error "15 days ago"
touch "$d/pulls.json.500"
run_report
check "exits 2" "$(is "$rc" 2)"
check "names the call that failed" "$(has "pulls?state=closed")"

echo "=== usage ==="
setup usage "3 days ago"
run_report --decision maybe
check "an unknown --decision exits 2" "$(is "$rc" 2)"

summary "security-report.sh computes the report-only window's decision"
