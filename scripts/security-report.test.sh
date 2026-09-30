#!/usr/bin/env bash
# Tests for `security-report.sh`: one case per decision rule of the report-only window (R2–R10),
# the window itself (R1, R11, R12, the window not yet started), the ways a read can fail (R13, R14,
# a server error, an artifact missing, expired or not in its producer's shape), and paging.
#
# `gh` is a stub on PATH (the dotfiles scripts/tests/test_docs_pr_gate.py pattern): it logs its
# argv to $STUB_LOG, which the paging case reads, and answers each GitHub API path from the
# fixture files a case writes under its own directory (FILE for a list's first page, FILE.pageN
# for page N). A fixture named FILE.403 makes that read a 403, FILE.500 a server error.
# Artifacts are real zips built with `zip`, as `gh api …/artifacts/<id>/zip` returns them, and
# their contents are the producers' own output: each zizmor-findings.json is
# .github/scripts/zizmor-findings.sh run over zizmor 1.30.1's findings
# (.github/scripts/testdata/zizmor*.json), and each security-report.json is
# .github/scripts/security-run.sh's, from that and the summary of osv-scanner's and govulncheck's
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
# serve FILE EMPTY: FILE for page 1, FILE.pageN for page N, EMPTY for a page with no such file.
serve() {
  if [ -e "$1.403" ]; then
    echo '{"message":"Resource not accessible by integration","status":"403"}'
    echo "gh: Resource not accessible by integration (HTTP 403)" >&2
    exit 1
  fi
  if [ -e "$1.500" ]; then echo "gh: Server Error (HTTP 500)" >&2; exit 1; fi
  local file="$1"
  if [ "$page" != 1 ]; then file="$1.page$page"; fi
  if [ -e "$file" ]; then cat "$file"; else printf '%s\n' "$2"; fi
}
id_in() { sed -E "s|.*/$1/([0-9]+).*|\\1|" <<<"$path"; }
case "$path" in
  repos/*/actions/workflows\?*) serve "$d/workflows.json" '{"total_count":0,"workflows":[]}' ;;
  repos/*/actions/workflows/*/runs\?*created=*)
    # A created=FROM..TO listing, as GitHub filters it (both ends inclusive), from every run in
    # runs.json; one page holds them all here.
    range=$(sed -E 's/.*[?&]created=([^&]+).*/\1/' <<<"$path")
    if [ "$page" != 1 ]; then echo '{"total_count":0,"workflow_runs":[]}'; exit 0; fi
    jq --arg from "${range%%..*}" --arg to "${range##*..}" \
      '.workflow_runs |= map(select(.created_at >= $from and .created_at <= $to)) | .total_count = (.workflow_runs | length)' \
      "$d/runs.json" ;;
  repos/*/actions/workflows/*/runs\?*)
    # runs.capped.json, when a case writes one, is what GitHub's 1,000-result cap leaves of an
    # unfiltered-by-date listing.
    if [ -e "$d/runs.capped.json" ]; then serve "$d/runs.capped.json" '{"total_count":0,"workflow_runs":[]}'
    else serve "$d/runs.json" '{"total_count":0,"workflow_runs":[]}'; fi ;;
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
  repos/*/collaborators/*/permission)
    # permission-LOGIN holds that login's role; anyone else reads as GitHub reads an outsider on a
    # public repository.
    login=$(sed -E 's|.*/collaborators/([^/]+)/permission|\1|' <<<"$path")
    role=$(cat "$d/permission-$login" 2> /dev/null || echo read)
    jq -cn --arg login "$login" --arg role "$role" \
      '{permission: (if $role == "maintain" then "write" else $role end), role_name: $role, user: {login: $login}}' ;;
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

# setup NAME START: a case directory with the Security and CodeQL workflows (Security created a
# day before START), both checks report-only, the window's first run on main (run 1000, a push) at
# START with a clean report, and two CodeQL analyses on main. Sets $d.
setup() {
  d="$work/$1"
  mkdir -p "$d"
  jq -n --arg created "$(date -u -d "@$(($(date -u -d "$2" +%s) - 86400))" +%Y-%m-%dT%H:%M:%S.000Z)" \
    '{total_count: 3, workflows: [
      {id: 100, name: "Tests", path: ".github/workflows/pr-and-main.yaml"},
      {id: 101, name: "Security", path: ".github/workflows/security.yaml", created_at: $created},
      {id: 102, name: "CodeQL", path: ".github/workflows/codeql.yaml"}]}' > "$d/workflows.json"
  echo '{"report_only": {"zizmor": true, "dependencies": true}}' > "$d/window.json"
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

# The dependency summaries security-run.sh writes from the scanners' own output: nothing found
# (clean), the vulnerable fixture modules (vulnerable: osv 5 with 3 fixable, govulncheck 1
# reachable and 1 informational), and neither scanner completing (error).
mkdir -p "$work/deps"
"$producers/security-run.sh" summarize --osv "$testdata/osv-clean.json" \
  --govulncheck "$testdata/govulncheck-clean.json" --out "$work/deps/clean.json"
"$producers/security-run.sh" summarize --osv "$testdata/osv.json" --govulncheck "$testdata/govulncheck.json" \
  --out "$work/deps/vulnerable.json"
"$producers/security-run.sh" summarize --out "$work/deps/error.json" 2> /dev/null

# security_report ZIZMOR_FINDINGS DEPS: the security-report.json security-run.sh report writes for
# a run whose artifacts are that zizmor-findings.json and the DEPS summary (clean, vulnerable, error).
security_report() {
  local dir
  dir=$(mktemp -d "$work/report.XXXXXX")
  printf '%s' "$1" > "$dir/zizmor-findings.json"
  "$producers/security-run.sh" report --zizmor "$dir/zizmor-findings.json" --deps "$work/deps/$2.json" \
    --run-id 1 --event push --head 0 --base "" --report-only-zizmor true --report-only-dependencies true \
    --workflows success --dependencies success --out "$dir/security-report.json" > /dev/null
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

# add_comment ID IN_REPLY_TO BODY [LOGIN]: a review comment by LOGIN (default legion-review[bot],
# a GitHub App); a login ending in [bot] is a Bot, any other a User.
add_comment() {
  local login=${4:-legion-review[bot]}
  [ -e "$d/comments.json" ] || echo '[]' > "$d/comments.json"
  append "$d/comments.json" . "$(jq -cn --argjson id "$1" --argjson reply "$2" --arg body "$3" --arg login "$login" \
    --arg created "$(iso '4 days ago')" '{id: $id, in_reply_to_id: $reply, body: $body,
    user: {login: $login, type: (if ($login | endswith("[bot]")) then "Bot" else "User" end)},
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
check "prints each check's flag" "$(has "report_only: zizmor true, dependencies true")"
check "and no note on a window file in its shape" "$(lacks "note:")"
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
check "asks for no required check while zizmor holds (rule 5)" "$(lacks "NEXT: ask a repository admin")"

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
check "asks for the required check once zizmor promotes (rule 5)" \
  "$(has "NEXT: ask a repository admin to require the check \`security\` (rule 5)")"
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

echo "=== rule 2's precondition: the dependency gate must pass on main ==="
setup r2-main "15 days ago"
add_run 1001 schedule main "$(iso '1 day ago')"
add_report 1001 "$(security_report "$(zizmor_findings '[]')" vulnerable)"
run_report
check "names main's count of each finding the gate blocks on" \
  "$(has "dependencies on main: 3 osv-scanner findings with a fix, 1 reachable govulncheck finding (run 1001, schedule)")"
check "holds the dependencies on them, naming both counts" \
  "$(has "DECISION: HOLD dependencies — main has 3 osv-scanner findings with a fix and 1 reachable govulncheck finding; the gate would fail every pull request")"
setup r2-main-both "15 days ago"
add_run 7101 pull_request feat-p "$(iso '5 days ago')"
add_marker dependencies-tool-error 7101
add_run 1001 schedule main "$(iso '1 day ago')"
add_report 1001 "$(security_report "$(zizmor_findings '[]')" vulnerable)"
run_report
check "a tool error and findings on main: the hold names both" \
  "$(has "DECISION: HOLD dependencies — 1 tool errors in the window; main has 3 osv-scanner findings with a fix and 1 reachable govulncheck finding")"
setup r2-main-error "15 days ago"
add_run 1001 schedule main "$(iso '1 day ago')"
add_report 1001 "$(security_report "$(zizmor_findings '[]')" error)"
run_report
check "a dependency tool error on main's newest run: rule 2 reads the run before it" \
  "$(has "dependencies on main: 0 osv-scanner findings with a fix, 0 reachable govulncheck findings (run 1000, push)")"
check "and promotes on its zeros" "$(has "DECISION: PROMOTE dependencies")"

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

echo "=== strict artifacts: a missing or malformed artifact fails the report, never a number ==="
# fails_loudly LABEL MESSAGE: the report exited 2 with MESSAGE, and printed no decision.
fails_loudly() {
  check "$1: exits 2" "$(is "$rc" 2)"
  check "$1: says so" "$(has "$2")"
  check "$1: prints no decision" "$(lacks "DECISION:")"
  check "$1: no traceback" "$(lacks "Traceback")"
}
one_new=$(zizmor_findings "$(audit "$a_artipacked")" '[]' "$(audit "$a_artipacked")")
# pr_first_run NUMBER BRANCH RUN MARKER_JSON: a PR merged 5 days ago whose first run's zizmor-new
# marker holds MARKER_JSON and whose zizmor-findings artifact is $one_new.
pr_first_run() {
  add_pr "$1" "$2" "$(iso '7 days ago')" "$(iso '5 days ago')"
  add_run "$3" pull_request "$2" "$(iso '6 days ago')"
  add_marker zizmor-new "$3" "$4"
  add_findings "$3" "$one_new"
}

setup strict-a "15 days ago"
pr_first_run 95 feat-r 9501 "$one_new"
add_run 9502 pull_request feat-r "$(iso '5 days ago - 1 hour')"
run_report
fails_loudly "A. a PR's last run with no zizmor-findings artifact" "run 9502 has no zizmor-findings artifact"

setup strict-a-error "15 days ago"
pr_first_run 94 feat-u 9401 "$one_new"
add_run 9402 pull_request feat-u "$(iso '5 days ago - 1 hour')"
add_findings 9402 '{"tool_error": true, "head_count": null, "new_count": null}'
run_report
fails_loudly "a PR's last run recording a tool error with no zizmor-tool-error marker" \
  "run 9402's zizmor-findings records a tool error, but the run has no zizmor-tool-error marker"

setup strict-b "15 days ago"
pr_first_run 96 feat-s 9601 "$(jq -c 'del(.new)' <<<"$one_new")"
add_run 9602 pull_request feat-s "$(iso '5 days ago - 1 hour')"
add_findings 9602 "$(zizmor_findings '[]' '[]' '[]')"
run_report
fails_loudly "B. a zizmor-new marker with no new key" "run 9601's zizmor-new artifact has no new list of findings"

setup strict-b-empty "15 days ago"
pr_first_run 93 feat-v 9301 "$(zizmor_findings "$pool" "$pool" "$pool")"
add_run 9302 pull_request feat-v "$(iso '5 days ago - 1 hour')"
add_findings 9302 "$(zizmor_findings '[]' '[]' '[]')"
run_report
fails_loudly "a zizmor-new marker listing nothing new" \
  "run 9301's zizmor-new marker lists no new finding; the Security workflow uploads it only for new findings"

setup strict-c "15 days ago"
add_run 1001 schedule main "$(iso '1 day ago')"
add_report 1001 "$(jq -c '.zizmor.count = .zizmor.head_count | del(.zizmor.head_count)' \
  <<<"$(security_report "$(zizmor_findings "$pool")" clean)")"
run_report
fails_loudly "C. a main report with head_count renamed" \
  "run 1001's security-report.json has a zizmor half without head_count"

setup strict-report "3 days ago"
add_run 1001 schedule main "$(iso '1 day ago')"
run_report
fails_loudly "a main run with no security-report artifact" "run 1001 has no security-report artifact"

setup strict-expired "15 days ago"
pr_with_findings 98 feat-t 9801 9802 "$(audit "$a_artipacked")" '[]' '[]'
jq '.artifacts |= map(if .workflow_run.id == 9801 then .expired = true else . end)' "$d/markers-zizmor-new.json" \
  > "$d/markers.next" && mv "$d/markers.next" "$d/markers-zizmor-new.json"
run_report
fails_loudly "an expired zizmor-new marker (exit 2, not the decision's 1)" \
  "artifact 98013 (zizmor-new) of run 9801 has expired; its zizmor-findings.json can no longer be read"

setup strict-tool-error "15 days ago"
add_run 1001 schedule main "$(iso '1 day ago')"
add_report 1001 "$(security_report '{"tool_error": true, "head_count": null, "new_count": null}' clean)"
run_report
check "a zizmor tool error on main's newest run: its row reads the tool error" \
  "$(has "| 1001 | schedule | $(sha 1001 | cut -c1-7) | -/- | 0/0 | 0/0 | yes |")"
check "and rule 1a reads the run before it" "$(has "zizmor on main: 0 findings (run 1000, push)")"

echo "=== paging: a list is read to its short page, and merged pull requests stop at the window ==="
setup paging "15 days ago"
# 101 dependency tool-error markers inside the window: a full first page and one on the second.
# markers FIRST COUNT: COUNT markers of runs FIRST… created 5 days ago, as one page of the listing.
markers() {
  jq -n --argjson first "$1" --argjson count "$2" --arg created "$(iso '5 days ago')" '{total_count: 101,
    artifacts: [range($first; $first + $count) | {id: (50000 + .), name: "dependencies-tool-error",
      expired: false, created_at: $created, workflow_run: {id: (60000 + .), head_branch: "main", head_sha: "0"}}]}'
}
markers 0 100 > "$d/markers-dependencies-tool-error.json"
markers 100 1 > "$d/markers-dependencies-tool-error.json.page2"
# Closed pull requests, newest update first: 100 closed unmerged inside the window, then #700
# merged inside it followed by 99 last updated before the window opened.
# closed FIRST COUNT UPDATED MERGED: COUNT pull requests #FIRST… updated at UPDATED, merged at MERGED.
closed() {
  jq -n --argjson first "$1" --argjson count "$2" --arg updated "$3" --argjson merged "$4" '[range($first; $first + $count)
    | {number: ., head: {ref: "branch-\(.)", sha: "0"}, created_at: $updated, updated_at: $updated, merged_at: $merged}]'
}
closed 1 100 "$(iso '3 days ago')" null > "$d/pulls.json"
jq -s add <(closed 700 1 "$(iso '4 days ago')" "\"$(iso '4 days ago')\"") \
  <(closed 800 99 "$(iso '20 days ago')" "\"$(iso '20 days ago')\"") > "$d/pulls.json.page2"
run_report
requested() { grep -cF -- "$1" "$d/calls.log" || true; }
check "every tool-error marker is counted, the second page's too" \
  "$(has "## Dependency scanner tool errors in the window: 101")"
check "the marker listing's second page is read" "$(is "$(requested "name=dependencies-tool-error&per_page=100&page=2")" 1)"
check "and its short second page ends it: no third page" \
  "$(is "$(requested "name=dependencies-tool-error&per_page=100&page=3")" 0)"
check "the pull request merged on the second page is counted" "$(has "no new findings: #700")"
check "one merged before the window is not" "$(lacks "#800")"
check "the closed pull requests' second page is read" "$(is "$(requested "pulls?state=closed&base=main&sort=updated&direction=desc&per_page=100&page=2")" 1)"
check "a page ending before the window ends the listing: no third page" \
  "$(is "$(requested "pulls?state=closed&base=main&sort=updated&direction=desc&per_page=100&page=3")" 0)"

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

echo "=== rule 4 counts writers and GitHub Apps only: anyone else's threads and replies are ignored, and named ==="
setup r10-authors "15 days ago"
echo write > "$d/permission-maintainer"
add_comment 1 null "Security[supply-chain]: a floating tag" outsider
add_comment 2 1 "Accepted: not a defect — pinned" outsider
add_comment 3 null "Security[supply-chain]: another floating tag" outsider
add_comment 4 3 "Accepted: not a defect — pinned" outsider
add_comment 5 null "Security[supply-chain]: a third floating tag" outsider
add_comment 6 5 "Accepted: not a defect — pinned" outsider
add_comment 7 null "Security[sandbox]: the pod mounts the Docker socket"
add_comment 8 7 "Accepted: not a defect — it is read-only" outsider
add_comment 9 null "Security[secret]: the token reaches the log" maintainer
add_comment 10 9 "Accepted: fixed in 1234567" maintainer
run_report
check "a CONTRIBUTOR human's three not-a-defect threads narrow nothing" "$(lacks "supply-chain row")"
check "each of them is named as ignored" \
  "$(has "ignored: Security[supply-chain] thread 5 by outsider (not a collaborator with write access, nor a GitHub App)")"
check "their Accepted: reply on an App's thread leaves it unanswered" \
  "$(has "keep: sandbox row (0 not a defect, 0 fixed, 1 unanswered)")"
check "and that reply is named" \
  "$(has "ignored: Accepted: reply 8 by outsider on thread 7 (not a collaborator with write access, nor a GitHub App)")"
check "a human collaborator with write access counts" "$(has "keep: secret row (0 not a defect, 1 fixed)")"
check "each human's permission is read once, and no App's" \
  "$( [ "$(grep -cF "collaborators/outsider/permission" "$d/calls.log")" = 1 ] &&
    [ "$(grep -cF "collaborators/maintainer/permission" "$d/calls.log")" = 1 ] &&
    [ "$(grep -c "collaborators/legion-review" "$d/calls.log" || true)" = 0 ] && echo true || echo false)"
check "so every rubric row is kept" "$(has "DECISION: keep every rubric row")"

echo "=== R11. every check promoted: the report prints, with no decision ==="
setup r11 "15 days ago"
echo '{"report_only": {"zizmor": false, "dependencies": false}}' > "$d/window.json"
run_report
check "exits 0" "$(is "$rc" 0)"
check "prints the run table" "$(has "| 1000 | push |")"
check "prints no decision" "$(lacks "DECISION:")"
check "prints each check's flag" "$(has "report_only: zizmor false, dependencies false")"
setup r11-bare "15 days ago"
echo '{"report_only": false}' > "$d/window.json"
run_report
check "a bare false (the file's first form) promotes both checks: exits 0" "$(is "$rc" 0)"
check "and reads false for each" "$(has "report_only: zizmor false, dependencies false")"
check "and names the bare boolean" "$(has "note: main's .github/security-window.json: report_only is one boolean")"

echo "=== per-check flags: each check's promotion is decided on its own ==="
setup promoted-zizmor "15 days ago"
echo '{"report_only": {"zizmor": false, "dependencies": true}}' > "$d/window.json"
run_report
check "zizmor promoted, dependencies still report-only after the window: exits 1" "$(is "$rc" 1)"
check "zizmor reads as already blocking" "$(has "DECISION: zizmor already blocking (report_only.zizmor is false)")"
check "zizmor is not decided again" "$(lacks "DECISION: PROMOTE zizmor")"
check "no rule 5 request for a check already promoted" "$(lacks "NEXT:")"
check "dependencies is still decided" "$(has "DECISION: PROMOTE dependencies")"
setup promoted-dependencies "15 days ago"
echo '{"report_only": {"zizmor": true, "dependencies": false}}' > "$d/window.json"
run_report
check "dependencies promoted, zizmor still report-only after the window: exits 1" "$(is "$rc" 1)"
check "zizmor is decided" "$(has "DECISION: PROMOTE zizmor")"
check "dependencies reads as already blocking" \
  "$(has "DECISION: dependencies already blocking (report_only.dependencies is false)")"
check "dependencies is not decided again" "$(lacks "DECISION: PROMOTE dependencies")"
setup promoted-open "3 days ago"
echo '{"report_only": {"zizmor": false, "dependencies": true}}' > "$d/window.json"
run_report
check "one check still report-only inside the window: exits 0, no decision" \
  "$( [ "$rc" = 0 ] && [ "$(lacks "DECISION:")" = true ] && echo true || echo false)"

echo "=== main's window file not in its shape: each check it does not set false reads as report-only, named ==="
# window_case NAME CONTENT|- ZIZMOR DEPENDENCIES NOTE: main's window file holds CONTENT (- for no
# file); the report reads the flags ZIZMOR and DEPENDENCIES through security-run.sh and prints NOTE.
window_case() {
  setup "window-$1" "15 days ago"
  if [ "$2" = - ]; then rm "$d/window.json"; else printf '%s\n' "$2" > "$d/window.json"; fi
  run_report
  check "$1: reads zizmor $3, dependencies $4" "$(has "report_only: zizmor $3, dependencies $4")"
  check "$1: names it" "$(has "note: main's .github/security-window.json: $5")"
  check "$1: exits 1 (a check is still report-only after the window)" "$(is "$rc" 1)"
}
window_case missing - true true "missing; every check reads as report-only"
window_case not-json '{"report_only":' true true "not JSON"
window_case not-an-object '[false]' true true "not an object with a report_only key; every check reads as report-only"
window_case no-report-only '{"report-only": false}' true true "not an object with a report_only key"
window_case string '{"report_only": "false"}' true true \
  'report_only is "false", not an object of checks; every check reads as report-only'
window_case missing-key '{"report_only": {"zizmor": false}}' false true \
  "report_only has no dependencies key; dependencies reads as report-only"
window_case non-boolean '{"report_only": {"zizmor": "false", "dependencies": false}}' true false \
  'report_only.zizmor is "false", not true or false; zizmor reads as report-only'
window_case unknown-key '{"report_only": {"zizmor": true, "dependencies": true, "codeql": false}}' true true \
  'report_only."codeql" names no check (zizmor, dependencies); ignored'

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

echo "=== the window has not started: no Security run on main yet ==="
setup not-started "3 days ago"
echo '{"total_count": 0, "workflow_runs": []}' > "$d/runs.json"
run_report
check "exits 0" "$(is "$rc" 0)"
check "says the window has not started" "$(has "window has not started")"

echo "=== the window's start: the first Security run on main, which GitHub's 1,000-run cap cannot move ==="
setup capped "15 days ago"
add_run 1001 schedule main "$(iso '10 days ago')"
add_report 1001 "$clean_report"
# Past 1,000 runs, a workflow's run listing filtered by branch no longer reaches its oldest: the
# first push run on main (1000) has dropped off it.
jq '.workflow_runs |= map(select(.id != 1000)) | .total_count = (.workflow_runs | length)' "$d/runs.json" \
  > "$d/runs.capped.json"
run_report
check "opens the window at the first run on main, not at the oldest run the capped listing holds" \
  "$(has "window: $start to")"
check "so the window has closed and the decision is due: exits 1" "$(is "$rc" 1)"
setup first-dispatch "15 days ago"
add_run 999 workflow_dispatch main "$(iso '15 days ago - 1 hour')"
add_report 999 "$clean_report"
run_report
check "a dispatched run on main before the first push opens the window" \
  "$(has "window: $(iso '15 days ago - 1 hour' | cut -c1-13)")"

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
