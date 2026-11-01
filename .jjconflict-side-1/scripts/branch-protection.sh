#!/usr/bin/env bash
set -euo pipefail

readonly repository="sjawhar/legion"
readonly branch="main"
readonly endpoint="repos/${repository}/branches/${branch}/protection"
readonly ci_app_id="${CI_APP_ID:-15368}"
readonly reviewer_app_id="${REVIEWER_APP_ID:-}"

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_numeric() {
  [[ "$2" =~ ^[0-9]+$ ]] || die "$1 must be a numeric GitHub App ID"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

require_command gh
require_command jq
require_numeric CI_APP_ID "$ci_app_id"
[[ -n "$reviewer_app_id" ]] || die "REVIEWER_APP_ID is required"
require_numeric REVIEWER_APP_ID "$reviewer_app_id"

printf '%s\n' "Prepared branch-protection update for ${repository}:${branch}:"
printf '%s\n' "  - required CI check from GitHub Actions (App ID ${ci_app_id})"
printf '%s\n' "  - required tester and architect checks from reviewer App ID ${reviewer_app_id}"
printf '%s\n' "  - at least one pull-request approval"
printf '%s\n' "No request has been sent. Read docs/solutions/github/branch-protection-required-checks.md first."

if [[ "${APPLY_BRANCH_PROTECTION:-}" != "1" ]]; then
  printf '%s\n' "Dry run. To apply manually: APPLY_BRANCH_PROTECTION=1 REVIEWER_APP_ID=<id> $0"
  exit 0
fi

admin="$(gh api "repos/${repository}" --jq '.permissions.admin // false')" || die "cannot read repository permissions"
[[ "$admin" == "true" ]] || die "the active GitHub credential needs repository Administration: write"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

protected=false
if gh api -i "$endpoint" >"${tmpdir}/protection" 2>&1; then
  protected=true
elif ! grep -Eq '^HTTP/[0-9.]+ 404' "${tmpdir}/protection"; then
  cat "${tmpdir}/protection" >&2
  die "cannot determine whether ${repository}:${branch} is protected"
fi

required_checks="$(jq -nc \
  --argjson ci "$ci_app_id" \
  --argjson reviewer "$reviewer_app_id" \
  '[
    {context: "CI", app_id: $ci},
    {context: "tester", app_id: $reviewer},
    {context: "architect", app_id: $reviewer}
  ]')"

if [[ "$protected" == "false" ]]; then
  payload="$(jq -nc --argjson checks "$required_checks" '{
    required_status_checks: {strict: true, checks: $checks},
    enforce_admins: false,
    required_pull_request_reviews: {
      dismissal_restrictions: {users: [], teams: [], apps: []},
      dismiss_stale_reviews: false,
      require_code_owner_reviews: false,
      required_approving_review_count: 1,
      require_last_push_approval: false,
      bypass_pull_request_allowances: {users: [], teams: [], apps: []}
    },
    restrictions: null,
    required_linear_history: false,
    allow_force_pushes: false,
    allow_deletions: false,
    block_creations: false,
    required_conversation_resolution: false,
    lock_branch: false,
    allow_fork_syncing: false
  }')"
  gh api --method PUT -H 'Accept: application/vnd.github+json' "$endpoint" --input - <<<"$payload"
  exit 0
fi

if current_checks="$(gh api -H 'Accept: application/vnd.github+json' "${endpoint}/required_status_checks" 2>"${tmpdir}/checks")"; then
  :
elif grep -Eq '404|Not Found' "${tmpdir}/checks"; then
  current_checks='{}'
else
  cat "${tmpdir}/checks" >&2
  die "cannot read existing required status checks"
fi

checks_payload="$(jq -nc --argjson current "$current_checks" --argjson required "$required_checks" '{
  strict: ($current.strict // true),
  checks: (
    ($current.checks // [])
    | map(select(.context != "CI" and .context != "tester" and .context != "architect"))
    | . + $required
    | unique_by([.context, .app_id])
  )
}')"
gh api --method PATCH -H 'Accept: application/vnd.github+json' "${endpoint}/required_status_checks" --input - <<<"$checks_payload"

if current_reviews="$(gh api -H 'Accept: application/vnd.github+json' "${endpoint}/required_pull_request_reviews" 2>"${tmpdir}/reviews")"; then
  existing_count="$(jq -r '.required_approving_review_count // 0' <<<"$current_reviews")"
elif grep -Eq '404|Not Found' "${tmpdir}/reviews"; then
  existing_count=0
else
  cat "${tmpdir}/reviews" >&2
  die "cannot read existing pull-request review protection"
fi

if ((existing_count < 1)); then
  existing_count=1
fi
review_payload="$(jq -nc --argjson count "$existing_count" '{required_approving_review_count: $count}')"
gh api --method PATCH -H 'Accept: application/vnd.github+json' "${endpoint}/required_pull_request_reviews" --input - <<<"$review_payload"

printf '%s\n' "Applied required-check and pull-request-review protection to ${repository}:${branch}."
