#!/usr/bin/env bash
# The recorded form of this repository's security settings, and the one way to apply them.
#
#   scripts/security-settings.sh                                   secret scanning + push protection
#   scripts/security-settings.sh --private-vulnerability-reporting private vulnerability reporting
#   scripts/security-settings.sh --help                            this text
#
# Each mode reads the current state first (security_and_analysis, private vulnerability reporting
# and CodeQL default setup) and prints it. Then it says there is nothing to change, or prints the
# exact command it would send (a dry run, the default), or, with APPLY_SECURITY_SETTINGS=1, sends
# it and refuses unless a readback shows the requested state. The dry run is where the commands
# are written down: the settings asks on AGENTC-1305 carry its output, and
# docs/solutions/github/security-settings.md points to it rather than copying it.
#
# Push protection blocks a push that carries a supported credential. By default anyone with write
# access can bypass a block by giving a reason, which GitHub records as a secret-scanning alert and
# in the audit log. Delegated bypass is the setting that narrows who may bypass; this script
# reports its state and never changes it.
#
# Only a repository admin reads or writes security_and_analysis and enables private vulnerability
# reporting. An agent session's gh acts as the owner's GitHub App, which reads
# security_and_analysis as null and whose writes GitHub refuses with 403, so the first mode stops
# before any write when it reads null; the App can read private vulnerability reporting, so the
# second mode still prints its state and the command.
#
# SECURITY_SETTINGS_REPOSITORY overrides the repository (default sjawhar/legion).
set -euo pipefail

readonly repository="${SECURITY_SETTINGS_REPOSITORY:-sjawhar/legion}"
readonly patch_body='{"security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"}}}'
readonly patch_command="gh api --method PATCH repos/${repository} --input - <<<'${patch_body}' --jq .security_and_analysis"
readonly put_command="gh api --method PUT repos/${repository}/private-vulnerability-reporting"

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

mode=secret-scanning
case "${1:-}" in
  "") ;;
  --private-vulnerability-reporting) mode=private-vulnerability-reporting ;;
  -h | --help)
    awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
    exit 0
    ;;
  *) die "unknown argument: $1 (see $0 --help)" ;;
esac
[[ $# -le 1 ]] || die "one argument at most (see $0 --help)"

require_command gh
require_command jq

# The security_and_analysis object, or nothing when the caller cannot read it (gh prints nothing
# for a null).
read_security_and_analysis() {
  gh api "repos/${repository}" --jq .security_and_analysis || die "cannot read repos/${repository}"
}

read_private_vulnerability_reporting() {
  local enabled
  enabled=$(gh api "repos/${repository}/private-vulnerability-reporting" --jq .enabled) ||
    die "cannot read repos/${repository}/private-vulnerability-reporting"
  case "$enabled" in
    true) echo enabled ;;
    false) echo disabled ;;
    *) die "repos/${repository}/private-vulnerability-reporting answered enabled=${enabled:-<nothing>}" ;;
  esac
}

# CodeQL default setup's state; only an admin can read it, and a 403 is reported as such.
read_default_setup() {
  local err state
  err=$(mktemp)
  if state=$(gh api "repos/${repository}/code-scanning/default-setup" --jq .state 2>"$err"); then
    rm -f "$err"
    echo "$state"
  elif grep -q 'HTTP 403' "$err"; then
    rm -f "$err"
    echo "unknown (not an admin)"
  else
    printf 'error: cannot read repos/%s/code-scanning/default-setup: %s\n' "$repository" "$(<"$err")" >&2
    rm -f "$err"
    exit 1
  fi
}

status_of() { # status_of SECURITY_AND_ANALYSIS KEY
  if [[ -z "$1" ]]; then
    echo "unknown (not an admin)"
  else
    jq -r --arg key "$2" '.[$key].status // "not reported"' <<<"$1"
  fi
}

security_and_analysis=$(read_security_and_analysis)
if [[ "$mode" == secret-scanning && -z "$security_and_analysis" ]]; then
  die "cannot read security_and_analysis for ${repository}: the active GitHub credential is not a repository admin. An agent session's gh acts as the GitHub App, which reads it as null and whose PATCH GitHub refuses with 403. Run this from a shell where \`gh auth status\` shows a repository admin."
fi
private_vulnerability_reporting=$(read_private_vulnerability_reporting)
default_setup=$(read_default_setup)

secret_scanning=$(status_of "$security_and_analysis" secret_scanning)
push_protection=$(status_of "$security_and_analysis" secret_scanning_push_protection)
delegated_bypass=$(status_of "$security_and_analysis" secret_scanning_delegated_bypass)
non_provider=$(status_of "$security_and_analysis" secret_scanning_non_provider_patterns)

printf '%s\n' "Security settings for ${repository}:" \
  "  secret scanning: ${secret_scanning}" \
  "  push protection: ${push_protection}" \
  "  push protection delegated bypass: ${delegated_bypass}" \
  "  secret scanning non-provider patterns: ${non_provider}" \
  "  private vulnerability reporting: ${private_vulnerability_reporting}" \
  "  CodeQL default setup: ${default_setup}"

if [[ "$default_setup" == configured ]]; then
  printf '%s\n' "warning: CodeQL default setup is configured; GitHub refuses the advanced-setup analyses .github/workflows/codeql.yaml uploads while it is. Turn it off as an admin: gh api --method PATCH repos/${repository}/code-scanning/default-setup -f state=not-configured"
fi

# The modes' one flow. A mode defines requested (true when the state it asks for holds), send (its
# one request), refresh (re-reads that state) and readback (prints it), then calls apply_mode with
# the state it asks for, the command a dry run prints, and its flag.
apply_mode() { # apply_mode WHAT COMMAND [FLAG]
  local what=$1 command=$2 flag=${3:-}
  if requested; then
    printf 'nothing to change: %s\n' "$what"
    return
  fi
  if [[ "${APPLY_SECURITY_SETTINGS:-}" != 1 ]]; then
    printf '%s\n' "Dry run: no request sent. As a repository admin, run APPLY_SECURITY_SETTINGS=1 $0${flag:+ $flag}, or:" \
      "  ${command}"
    return
  fi
  send
  refresh
  printf '%s\n' "Applied; readback:"
  readback
  requested || die "the readback does not show ${what}"
}

if [[ "$mode" == secret-scanning ]]; then
  requested() { [[ "$secret_scanning" == enabled && "$push_protection" == enabled ]]; }
  send() { gh api --method PATCH "repos/${repository}" --input - <<<"$patch_body" --jq .security_and_analysis >/dev/null; }
  refresh() {
    security_and_analysis=$(read_security_and_analysis)
    secret_scanning=$(status_of "$security_and_analysis" secret_scanning)
    push_protection=$(status_of "$security_and_analysis" secret_scanning_push_protection)
  }
  readback() { printf '%s\n' "  secret scanning: ${secret_scanning}" "  push protection: ${push_protection}"; }
  apply_mode "secret scanning and push protection enabled" "$patch_command"
else
  requested() { [[ "$private_vulnerability_reporting" == enabled ]]; }
  send() { gh api --method PUT "repos/${repository}/private-vulnerability-reporting"; }
  refresh() { private_vulnerability_reporting=$(read_private_vulnerability_reporting); }
  readback() { printf '%s\n' "  private vulnerability reporting: ${private_vulnerability_reporting}"; }
  apply_mode "private vulnerability reporting enabled" "$put_command" --private-vulnerability-reporting
fi
