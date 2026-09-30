#!/usr/bin/env bash
# Tests for `security-settings.sh`: it refuses a caller that cannot read the repository's
# security settings before any write, prints the same commands the settings asks carry, writes
# only with APPLY_SECURITY_SETTINGS=1, and trusts a write only once a readback shows it.
#
# `gh` is a stub on PATH (the dotfiles scripts/tests/test_docs_pr_gate.py pattern): it logs its
# argv to $STUB_LOG and answers from the fixture files a case writes under $STUB_DIR.
#
# Run from anywhere: scripts/security-settings.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
settings="$script_dir/security-settings.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/../.github/scripts/test-lib.sh"

mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
# Answers `gh api` from $STUB_DIR: repo.json, pvr.json, default-setup.json (default-setup.403
# makes that read a 403). A PATCH merges its body into repo.json unless $STUB_DIR/ignore-writes
# exists; a PUT enables pvr.json. --jq is applied as gh applies it (a null prints nothing).
set -euo pipefail
printf '%s\n' "$*" >> "$STUB_LOG"
[ "$1" = api ] || { echo "stub gh: only api is stubbed" >&2; exit 99; }
shift
method=GET path="" jq_expr=""
while [ $# -gt 0 ]; do
  case "$1" in
    --method|-X) method=$2; shift 2 ;;
    --jq|-q) jq_expr=$2; shift 2 ;;
    --input) shift 2 ;;
    -*) shift ;;
    *) path=$1; shift ;;
  esac
done
forbidden() { echo '{"message":"Resource not accessible by integration","status":"403"}'; echo "gh: Resource not accessible by integration (HTTP 403)" >&2; exit 1; }
case "$method $path" in
  "GET repos/sjawhar/legion") body=$(cat "$STUB_DIR/repo.json") ;;
  "PATCH repos/sjawhar/legion")
    cat > "$STUB_DIR/patch-body.json"
    if [ ! -e "$STUB_DIR/ignore-writes" ]; then
      jq -s '.[0] * .[1]' "$STUB_DIR/repo.json" "$STUB_DIR/patch-body.json" > "$STUB_DIR/repo.next"
      mv "$STUB_DIR/repo.next" "$STUB_DIR/repo.json"
    fi
    body=$(cat "$STUB_DIR/repo.json") ;;
  "GET repos/sjawhar/legion/private-vulnerability-reporting") body=$(cat "$STUB_DIR/pvr.json") ;;
  "PUT repos/sjawhar/legion/private-vulnerability-reporting")
    [ -e "$STUB_DIR/ignore-writes" ] || echo '{"enabled":true}' > "$STUB_DIR/pvr.json"
    exit 0 ;;
  "GET repos/sjawhar/legion/code-scanning/default-setup")
    [ -e "$STUB_DIR/default-setup.403" ] && forbidden
    body=$(cat "$STUB_DIR/default-setup.json") ;;
  *) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
esac
if [ -n "$jq_expr" ]; then
  out=$(jq -r "$jq_expr" <<<"$body")
  [ "$out" = null ] && out=""
  printf '%s\n' "$out"
else
  printf '%s\n' "$body"
fi
STUB
chmod +x "$work/bin/gh"

patch_body='{"security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"}}}'
patch_command="gh api --method PATCH repos/sjawhar/legion --input - <<<'$patch_body' --jq .security_and_analysis"
put_command="gh api --method PUT repos/sjawhar/legion/private-vulnerability-reporting"

# fixture NAME SECRET_SCANNING PUSH_PROTECTION BYPASS PVR DEFAULT_SETUP: a stub state. A status
# of `null` makes security_and_analysis read null (a caller that is not an admin); DEFAULT_SETUP
# `403` makes that read refused.
fixture() {
  local dir="$work/$1"
  mkdir -p "$dir"
  if [ "$2" = null ]; then
    echo '{"full_name":"sjawhar/legion","security_and_analysis":null}' > "$dir/repo.json"
  else
    jq -n --arg ss "$2" --arg pp "$3" --arg bypass "$4" '{full_name: "sjawhar/legion",
      security_and_analysis: {secret_scanning: {status: $ss}, secret_scanning_push_protection: {status: $pp},
        secret_scanning_delegated_bypass: {status: $bypass},
        secret_scanning_non_provider_patterns: {status: "disabled"}}}' > "$dir/repo.json"
  fi
  echo "{\"enabled\":$5}" > "$dir/pvr.json"
  if [ "$6" = 403 ]; then
    touch "$dir/default-setup.403"
  else
    echo "{\"state\":\"$6\",\"languages\":[]}" > "$dir/default-setup.json"
  fi
}

# settings NAME [ARG…]: runs the script against fixture NAME with the stub; output in $output,
# exit code in $rc, the stub's argv log in $work/NAME/calls.log. Extra environment through
# `APPLY=1 settings …`.
settings() {
  local name="$1"
  shift
  rc=0
  output=$(PATH="$work/bin:$PATH" STUB_DIR="$work/$name" STUB_LOG="$work/$name/calls.log" \
    APPLY_SECURITY_SETTINGS="${APPLY:-}" "$settings" "$@" 2>&1) || rc=$?
  touch "$work/$name/calls.log"
}

has() { grep -qF -- "$2" <<<"$1" && echo true || echo false; }
# count FILE [--] TEXT: how many lines of FILE contain TEXT (fixed string).
count() {
  local file="$1"
  shift
  if [ "$1" = -- ]; then shift; fi
  grep -cF -- "$1" "$file" || true
}

# after FILE TEXT REGEX: how many lines matching REGEX follow the first line containing TEXT (0
# when no line contains TEXT).
after() { awk -v text="$2" -v re="$3" 'seen && $0 ~ re { n++ } index($0, text) { seen = 1 } END { print n + 0 }' "$1"; }

echo "=== 1. security_and_analysis reads null: refused, naming the admin, before any write ==="
fixture not-admin null null null false 403
settings not-admin
check "exits 1" "$(is "$rc" 1)"
check "says it cannot read security_and_analysis" "$(has "$output" "cannot read security_and_analysis")"
check "names the missing admin" "$(has "$output" "admin")"
check "sends no write" "$(is "$(count "$work/not-admin/calls.log" -- "--method")" 0)"
APPLY=1 settings not-admin
check "APPLY_SECURITY_SETTINGS=1 exits 1 the same way" "$(is "$rc" 1)"
check "and still sends no write" "$(is "$(count "$work/not-admin/calls.log" -- "--method")" 0)"

echo "=== 2. everything enabled: nothing to change ==="
fixture enabled enabled enabled disabled true not-configured
settings enabled
check "exits 0" "$(is "$rc" 0)"
check "says nothing to change" "$(has "$output" "nothing to change")"
check "sends no write" "$(is "$(count "$work/enabled/calls.log" -- "--method")" 0)"
check "prints each status" "$(has "$output" "push protection: enabled")"

echo "=== 3. push protection disabled, no APPLY: a dry run with the exact command ==="
fixture dry enabled disabled disabled false not-configured
settings dry
check "exits 0" "$(is "$rc" 0)"
check "says Dry run" "$(has "$output" "Dry run")"
check "prints the settings ask's PATCH command verbatim" "$(has "$output" "$patch_command")"
check "sends no write" "$(is "$(count "$work/dry/calls.log" -- "--method")" 0)"

echo "=== 4. APPLY_SECURITY_SETTINGS=1: one PATCH with the ask's body, then a readback ==="
fixture apply enabled disabled disabled false not-configured
APPLY=1 settings apply
check "exits 0" "$(is "$rc" 0)"
check "sends exactly one PATCH, as the ask's command does" \
  "$(is "$(count "$work/apply/calls.log" "api --method PATCH repos/sjawhar/legion --input - --jq .security_and_analysis")" 1)"
check "sends no other write" "$(is "$(count "$work/apply/calls.log" -- "--method")" 1)"
check "the PATCH body is the ask's body" \
  "$(is "$(jq -cS . "$work/apply/patch-body.json" 2>/dev/null || echo "no PATCH body")" "$(jq -cS . <<<"$patch_body")")"
check "reads security_and_analysis back after the PATCH" \
  "$(is "$(after "$work/apply/calls.log" "--method PATCH" '^api repos/sjawhar/legion ')" 1)"
check "reports the applied state" "$(has "$output" "push protection: enabled")"

echo "=== 5. --private-vulnerability-reporting with APPLY: one PUT, then a readback ==="
fixture pvr enabled enabled disabled false not-configured
APPLY=1 settings pvr --private-vulnerability-reporting
check "exits 0" "$(is "$rc" 0)"
check "sends exactly one PUT, the ask's command" \
  "$(is "$(count "$work/pvr/calls.log" "api --method PUT repos/sjawhar/legion/private-vulnerability-reporting")" 1)"
check "sends no other write" "$(is "$(count "$work/pvr/calls.log" -- "--method")" 1)"
check "reads the status back after the PUT" \
  "$(is "$(after "$work/pvr/calls.log" "--method PUT" '^api repos/sjawhar/legion/private-vulnerability-reporting')" 1)"
check "reports it enabled" "$(has "$output" "private vulnerability reporting: enabled")"

echo "=== 6. CodeQL default setup configured: warned, naming codeql.yaml ==="
fixture default-setup enabled enabled disabled true configured
settings default-setup
check "exits 0" "$(is "$rc" 0)"
check "says default setup is configured" "$(has "$output" "default setup is configured")"
check "names codeql.yaml" "$(has "$output" "codeql.yaml")"

echo "=== 7. delegated bypass enabled: reported, never warned against ==="
fixture bypass enabled enabled enabled true not-configured
settings bypass
check "exits 0" "$(is "$rc" 0)"
check "reports it" "$(has "$output" "push protection delegated bypass: enabled")"
check "does not warn against the setting that narrows who may bypass" "$(is "$(has "$output" "warning:")" false)"

echo "=== 8. --private-vulnerability-reporting as a caller that is not an admin: a dry run ==="
fixture pvr-app null null null false 403
settings pvr-app --private-vulnerability-reporting
check "exits 0 (the App reads private vulnerability reporting)" "$(is "$rc" 0)"
check "prints it disabled" "$(has "$output" "private vulnerability reporting: disabled")"
check "prints the ask's PUT command" "$(has "$output" "$put_command")"
check "reads default setup as unknown (not an admin)" "$(has "$output" "unknown (not an admin)")"
check "sends no write" "$(is "$(count "$work/pvr-app/calls.log" -- "--method")" 0)"

echo "=== 9. a write the readback does not show: refused ==="
fixture ignored enabled disabled disabled false not-configured
touch "$work/ignored/ignore-writes"
APPLY=1 settings ignored
check "exits 1" "$(is "$rc" 1)"
check "names the readback" "$(has "$output" "readback")"

echo "=== 10. --help prints the whole header comment and none of the code ==="
fixture help enabled enabled disabled true not-configured
settings help --help
header=$(awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$settings")
check "exits 0" "$(is "$rc" 0)"
check "prints every header line, the last included" "$(is "$output" "$header")"
check "and no line of the code" "$(is "$(has "$output" "set -euo pipefail")" false)"
check "reads nothing from GitHub" "$(is "$(count "$work/help/calls.log" -- "api")" 0)"

summary "security-settings.sh reads, plans and applies the security settings"
