#!/usr/bin/env bash
set -euo pipefail

readonly daemon_url="${SMOKE_DAEMON_URL:-http://127.0.0.1:${LEGION_DAEMON_PORT:-19370}}"
readonly smoke_dir="${SMOKE_DIR:-/tmp/legion-smoke}"

fail() {
  printf 'CHECKPOINT %s FAILED: %s\n' "$checkpoint" "$*" >&2
  exit 1
}
blocked() {
  printf 'CHECKPOINT %s SKIPPED-BLOCKED: %s\n' "$checkpoint" "$*" >&2
  exit 3
}

require_env() {
  [[ -n "${!1:-}" ]] || fail "$1 is required"
}

state() {
  curl --fail --silent --show-error "${daemon_url}/legion/v1/state"
}
stored_board_scope() {
  local scope_file="${smoke_dir}/board-scope"
  if [[ -r "$scope_file" ]]; then
    printf '%s\n' "$(<"$scope_file")"
  else
    printf '%s\n' "${SMOKE_BOARD_SCOPE:-}"
  fi
}
stored_webhook_mode() {
  local mode_file="${smoke_dir}/webhook-mode"
  if [[ -r "$mode_file" ]]; then
    printf '%s\n' "$(<"$mode_file")"
  else
    printf '%s\n' "${SMOKE_WEBHOOK_MODE:-forward}"
  fi
}

webhook_ingress_block_reason() {
  printf '%s\n' \
    'SMOKE_WEBHOOK_MODE=none: this checkpoint requires live GitHub webhook ingress; use SMOKE_WEBHOOK_MODE=envoy or forward'
}



project_slug() {
  local project="$SMOKE_PROJECT"
  project="${project,,}"
  project="${project//[^a-z0-9]/}"
  printf '%s\n' "$project"
}

issue_key() {
  if [[ -n "${SMOKE_ROOT_ISSUE:-}" ]]; then
    printf '%s\n' "$SMOKE_ROOT_ISSUE"
    return
  fi
  state | jq -er --arg prefix "${SMOKE_REPO}#" '[.issues | keys[] | select(startswith($prefix))] | first'
}

root_key() {
  if [[ -n "${SMOKE_ROOT_ISSUE:-}" ]]; then
    printf '%s\n' "$SMOKE_ROOT_ISSUE"
    return
  fi
  state | jq -er --arg prefix "${SMOKE_REPO}#" '[.trees | to_entries[] | select(.key | startswith($prefix)) | .value.root] | first'
}

issue_number() {
  local key="$1"
  printf '%s\n' "${key##*#}"
}

tree_window() {
  local key="$1"
  local owner="${key%%/*}"
  local rest="${key#*/}"
  local repo="${rest%%#*}"
  local number="${rest#*#}"
  local full
  local digest

  owner="${owner,,}"
  repo="${repo,,}"
  owner="${owner//_/_u}"
  owner="${owner//./_d}"
  owner="${owner//-/_h}"
  repo="${repo//_/_u}"
  repo="${repo//./_d}"
  repo="${repo//-/_h}"
  full="${owner}__${repo}-${number}"
  if ((${#full} <= 160)); then
    printf '%s\n' "$full"
    return
  fi
  digest="$(printf '%s' "$full" | sha256sum | cut -d ' ' -f 1)"
  printf '%s-%s\n' "${full:0:143}" "${digest:0:16}"
}

tree_window_id() {
  state | jq -er --arg key "$1" '.trees[$key].locator.tmuxWindowId'
}

window_id() {
  local name="$1"
  local id
  local window
  local matches=()

  while IFS=' ' read -r id window; do
    [[ "$window" == "$name" ]] && matches+=("$id")
  done < <(tmux list-windows -t "legion-$(project_slug)" -F '#{window_id} #{window_name}')
  [[ "${#matches[@]}" == 1 ]] || fail "expected one tmux window named ${name}, found ${#matches[@]}"
  printf '%s\n' "${matches[0]}"
}

expect_window() {
  local window="$1"
  local windows
  windows="$(tmux list-windows -t "legion-$(project_slug)" -F '#{window_name}')"
  grep -Fxq -- "$window" <<<"$windows" || fail "tmux window ${window} is absent"
}

smoke_pr() {
  if [[ -n "${SMOKE_PR:-}" ]]; then
    printf '%s\n' "$SMOKE_PR"
    return
  fi
  gh pr list -R "$SMOKE_REPO" --state all --json number,headRefName \
    --jq '[.[] | select(.headRefName | test("^legion/issue-")) | .number] | first // empty'
}

checkpoint_one() {
  local issue
  issue="$(issue_key)"
  state | jq -e --arg issue "$issue" '.issues | has($issue)' >/dev/null || fail "daemon state lacks ${issue}"
  expect_window controller
  printf 'CHECKPOINT 1 OK: daemon tracks %s; controller window is live\n' "$issue"
}

checkpoint_two() {
  local root
  root="$(root_key)"
  state | jq -e --arg root "$root" '.admission.active | index($root) != null' >/dev/null ||
    fail "${root} is not admitted"
  expect_window "$(tree_window "$root")"
  printf 'CHECKPOINT 2 OK: %s is active; architect window is live\n' "$root"
}

checkpoint_three() {
  local root
  local issue
  local children
  root="$(root_key)"
  issue="$(gh issue view "$(issue_number "$root")" -R "$SMOKE_REPO" --json labels,body)"
  jq -e '(.body | length > 0) and ([.labels[].name] | index("needs-approval") != null)' >/dev/null <<<"$issue" ||
    fail "root issue lacks a posted spec or needs-approval"
  children="$(gh issue list -R "$SMOKE_REPO" --label legion-child --state all --json number)"
  jq -e 'length > 0' >/dev/null <<<"$children" || fail "no legion-child issue exists"
  printf 'CHECKPOINT 3 OK: posted spec, needs-approval, and legion-child observed\n'
}

checkpoint_four() {
  local root
  root="$(root_key)"
  state | jq -e --arg root "$root" \
    '[.issues | to_entries[] | select(.key != $root and .value.released == true)] | length > 0' >/dev/null ||
    fail "no child wave is released"
  printf 'CHECKPOINT 4 OK: a child wave is released\n'
}

checkpoint_five() {
  local pr
  local view
  pr="$(smoke_pr)"
  [[ "$pr" =~ ^[0-9]+$ ]] || fail "set SMOKE_PR or open a legion/issue-* pull request"
  view="$(gh pr view "$pr" -R "$SMOKE_REPO" --json headRefName,commits)"
  jq -e '(.headRefName | test("^legion/issue-")) and (tostring | test("implementer\\+ses_")) and (tostring | test("Legion-Session:"))' \
    >/dev/null <<<"$view" || fail "PR #${pr} lacks the Legion branch, identity, or session trailer"
  printf 'CHECKPOINT 5 OK: PR #%s carries Legion branch and commit attribution\n' "$pr"
}

checkpoint_six() {
  local root
  local pane
  local verdict_count

  root="$(root_key)"
  require_env SMOKE_VERDICT_FRAGMENT
  require_env SMOKE_RAW_CHECK_FRAGMENT
  state | jq -e '[.prs[] | select(.firstRedEmitted and .greenEmitted)] | length > 0' >/dev/null ||
    fail "no PR recorded both first-red and green emissions"
  pane="$(tmux capture-pane -p -t "$(tree_window_id "$root")")"
  verdict_count="$(grep -Foc "$SMOKE_VERDICT_FRAGMENT" <<<"$pane")"
  [[ "$verdict_count" == "1" ]] || fail "expected one coalesced verdict, found ${verdict_count}"
  [[ "$pane" != *"$SMOKE_RAW_CHECK_FRAGMENT"* ]] || fail "architect transcript contains raw check noise"
  printf 'CHECKPOINT 6 OK: one coalesced verdict and no raw check noise\n'
}

checkpoint_seven() {
  local pr
  local retro
  local files
  local commits
  local commit
  local deletion_at=""
  local reviews

  require_env SMOKE_RETRO_COMMIT
  require_env SMOKE_REVIEWER_LOGIN
  pr="$(smoke_pr)"
  [[ "$pr" =~ ^[0-9]+$ ]] || fail "set SMOKE_PR"
  retro="$(gh api "repos/${SMOKE_REPO}/commits/${SMOKE_RETRO_COMMIT}")"
  jq -e '[.files[].filename | startswith("docs/solutions/")] | any' >/dev/null <<<"$retro" ||
    fail "${SMOKE_RETRO_COMMIT} has no docs/solutions change"
  commits="$(gh api "repos/${SMOKE_REPO}/pulls/${pr}/commits")"
  while IFS= read -r commit; do
    [[ -n "$commit" ]] || continue
    files="$(gh api "repos/${SMOKE_REPO}/commits/${commit}")"
    if jq -e '[.files[] | select(.filename | startswith(".legion/")) | select(.status == "removed")] | length > 0' >/dev/null <<<"$files"; then
      jq -e --arg reviewer "$SMOKE_REVIEWER_LOGIN" '
        .author.login == $reviewer or .committer.login == $reviewer
      ' >/dev/null <<<"$files" || fail "reviewer App did not author or commit the .legion deletion"
      deletion_at="$(jq -r '.commit.committer.date' <<<"$files")"
    fi
  done < <(jq -r '.[].sha' <<<"$commits")
  [[ -n "$deletion_at" ]] || fail "PR #${pr} has no reviewer .legion deletion commit"
  reviews="$(gh api "repos/${SMOKE_REPO}/pulls/${pr}/reviews")"
  jq -e --arg reviewer "$SMOKE_REVIEWER_LOGIN" --arg deletion_at "$deletion_at" '
    any(.[]; .user.login == $reviewer and .state == "APPROVED" and .submitted_at > $deletion_at)
  ' >/dev/null <<<"$reviews" || fail "reviewer approval did not follow the .legion deletion"
  files="$(gh api "repos/${SMOKE_REPO}/pulls/${pr}/files?per_page=100")"
  jq -e '[.[].filename | startswith(".legion/")] | any | not' >/dev/null <<<"$files" ||
    fail "PR #${pr} final diff still contains .legion files"
  gh api "repos/${SMOKE_REPO}/git/ref/heads/main" --jq '.object.sha' >"${smoke_dir}/base-at-merge"
  printf 'CHECKPOINT 7 OK: reviewer deleted .legion before approving and retro is durable\n'
}

checkpoint_eight() {
  local pr
  local pull
  local reviews
  local merge
  local merge_base

  require_env SMOKE_HUMAN_LOGIN
  pr="$(smoke_pr)"
  [[ -r "${smoke_dir}/base-at-merge" ]] || fail "run checkpoint 7 before approving and merging the PR"
  merge_base="$(<"${smoke_dir}/base-at-merge")"
  [[ "$pr" =~ ^[0-9]+$ ]] || fail "set SMOKE_PR"
  pull="$(gh api "repos/${SMOKE_REPO}/pulls/${pr}")"
  jq -e '.state == "closed" and .merged == true and (.merge_commit_sha | type == "string") and .merge_commit_sha != .head.sha' >/dev/null <<<"$pull" ||
    fail "PR #${pr} is not a squash merge"
  reviews="$(gh api "repos/${SMOKE_REPO}/pulls/${pr}/reviews")"
  jq -e --arg human "$SMOKE_HUMAN_LOGIN" --arg head "$(jq -r '.head.sha' <<<"$pull")" '
    any(.[]; .user.login == $human and .state == "APPROVED" and .commit_id == $head)
  ' >/dev/null <<<"$reviews" || fail "current PR head lacks an approving human review"
  merge="$(gh api "repos/${SMOKE_REPO}/git/commits/$(jq -r '.merge_commit_sha' <<<"$pull")")"
  jq -e --arg base "$merge_base" '(.parents | length) == 1 and .parents[0].sha == $base' >/dev/null <<<"$merge" ||
    fail "merge commit is not a single-parent squash onto the recorded base"
  if gh api "repos/${SMOKE_REPO}/contents/.legion" >/dev/null 2>&1; then
    fail "main still contains .legion"
  fi
  printf 'CHECKPOINT 8 OK: human-approved squash merge left main .legion-free\n'
}

checkpoint_nine() {
  local root
  local issue

  require_env SMOKE_ARCHITECT_LOGIN
  require_env SMOKE_SIGNOFF_FRAGMENT
  root="$(root_key)"
  issue="$(gh issue view "$(issue_number "$root")" -R "$SMOKE_REPO" --json state,comments)"
  jq -e '.state == "CLOSED"' >/dev/null <<<"$issue" || fail "root issue is not closed"
  jq -e --arg architect "$SMOKE_ARCHITECT_LOGIN" --arg signoff "$SMOKE_SIGNOFF_FRAGMENT" '
    any(.comments[]; .author.login == $architect and (.body | contains($signoff)))
  ' >/dev/null <<<"$issue" || fail "root lacks an architect-attributed sign-off comment"
  state | jq -e --arg root "$root" '.trees[$root].status == "lingering"' >/dev/null ||
    fail "${root} is not lingering"
  printf 'CHECKPOINT 9 OK: architect signed off and root is lingering\n'
}

checkpoint_ten() {
  local worker_pane
  local architect_pane
  local daemon_log
  local daemon_output
  local after_no_holder
  local after_probe
  local log_offset

  require_env SMOKE_WORKER_WINDOW
  require_env SMOKE_ARCHITECT_WINDOW
  require_env SMOKE_COMMENT_FRAGMENT
  daemon_log="${SMOKE_DAEMON_LOG:-${smoke_dir}/daemon.log}"
  log_offset="${SMOKE_REVIVAL_LOG_OFFSET:-${smoke_dir}/revival.log.offset}"
  [[ -r "$daemon_log" && -r "$log_offset" ]] || fail "run arm-revival immediately before the triggering comment"
  daemon_output="$(tail -c "+$(( $(<"$log_offset") + 1 ))" "$daemon_log")"
  worker_pane="$(tmux capture-pane -p -t "$(window_id "$SMOKE_WORKER_WINDOW")")"
  [[ "$worker_pane" == *"$SMOKE_COMMENT_FRAGMENT"* ]] || fail "worker transcript lacks the published comment"
  after_no_holder="${daemon_output#*no_holder}"
  [[ -n "$after_no_holder" && "$after_no_holder" != "$daemon_output" ]] ||
    fail "daemon log lacks no_holder"
  after_probe="${after_no_holder#*probe}"
  [[ "$after_probe" != "$after_no_holder" && "$after_probe" == *revive* ]] ||
    fail "daemon log lacks ordered no_holder → probe → revive handling"
  architect_pane="$(tmux capture-pane -p -t "$(window_id "$SMOKE_ARCHITECT_WINDOW")")"
  [[ "$architect_pane" != *"$SMOKE_COMMENT_FRAGMENT"* ]] ||
    fail "architect consumed the worker revival comment"
  printf 'CHECKPOINT 10 OK: no_holder → probe → revive reached worker without an architect turn\n'
}

checkpoint_eleven() {
  local issue
  local window
  local windows
  local pane

  require_env SMOKE_RESURRECTION_ISSUE
  require_env SMOKE_CATCHUP_FRAGMENT
  require_env SMOKE_RESURRECTION_ROLE
  require_env SMOKE_RESURRECTION_WORKER_SESSION
  issue="$SMOKE_RESURRECTION_ISSUE"
  [[ "$issue" == "${SMOKE_REPO}#"* ]] || fail "SMOKE_RESURRECTION_ISSUE must belong to ${SMOKE_REPO}"
  window="$(tree_window "$issue")"
  state | jq -e --arg issue "$issue" '.trees[$issue].generation >= 2' >/dev/null ||
    fail "${issue} was not resurrected to a new generation"
  state | jq -e --arg issue "$issue" --arg role "$SMOKE_RESURRECTION_ROLE" --arg session "$SMOKE_RESURRECTION_WORKER_SESSION" '
    any(.roles[]; .issue == $issue and .role == $role and .sessionId == $session)
  ' >/dev/null || fail "resurrection role does not map to the specified worker session"
  windows="$(tmux list-windows -t "legion-$(project_slug)" -F '#{window_name}' | jq -Rsc --arg window "$window" 'split("\n") | map(select(. == $window)) | length')"
  [[ "$windows" == "1" ]] || fail "expected one ${window} tmux window, found ${windows}"
  pane="$(tmux capture-pane -p -t "$(tree_window_id "$issue")")"
  [[ "$pane" == *"$SMOKE_RESURRECTION_WORKER_SESSION"* && "$pane" == *"$SMOKE_CATCHUP_FRAGMENT"* ]] ||
    fail "specified revived worker transcript lacks its catchup-worker payload"
  printf 'CHECKPOINT 11 OK: %s resurrected once and worker received catchup payload\n' "$issue"
}

checkpoint_twelve() {
  local root
  require_env SMOKE_QUEUED_ISSUE
  root="$(root_key)"
  state | jq -e --arg root "$root" --arg promoted "$SMOKE_QUEUED_ISSUE" \
    '(.admission.active | index($root) == null) and (.admission.active | index($promoted) != null)' >/dev/null ||
    fail "closed tree is still active or queued issue was not promoted"
}

if [[ $# -eq 1 && "$1" == "arm-revival" ]]; then
  [[ -r "${smoke_dir}/daemon.log" ]] || {
    printf 'arm-revival: daemon log is unavailable\n' >&2
    exit 1
  }
  wc -c <"${smoke_dir}/daemon.log" >"${smoke_dir}/revival.log.offset"
  printf 'REVIVAL ARMED\n'
  exit 0
fi

[[ $# -eq 1 && "$1" =~ ^[1-9][0-2]?$ ]] || {
  printf 'usage: %s <1-12>\n' "$0" >&2
  exit 2
}

readonly checkpoint="$1"
require_env SMOKE_REPO
require_env SMOKE_PROJECT
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"
command -v grep >/dev/null 2>&1 || fail "grep is required"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"
command -v tail >/dev/null 2>&1 || fail "tail is required"
case "$(stored_webhook_mode)" in
  none)
    case "$checkpoint" in
      5 | 6 | 7 | 9 | 10 | 11)
        blocked "$(webhook_ingress_block_reason)"
        ;;
    esac
    ;;
  forward | envoy)
    ;;
  *)
    fail "recorded SMOKE_WEBHOOK_MODE must be forward, envoy, or none"
    ;;
esac

case "$checkpoint" in
  1 | 2 | 3 | 4)
    [[ -n "${SMOKE_PROJECT_ID:-}" ]] ||
      blocked "SMOKE_PROJECT_ID is required after the sandbox Projects V2 board exists"
    ;;
  7 | 8)
    [[ "${SMOKE_BRANCH_PROTECTION:-}" == "1" ]] ||
      blocked "SMOKE_BRANCH_PROTECTION=1 requires branch protection/ruleset availability"
    ;;
esac


case "$checkpoint" in
  1) checkpoint_one ;;
  2) checkpoint_two ;;
  3) checkpoint_three ;;
  4) checkpoint_four ;;
  5) checkpoint_five ;;
  6) checkpoint_six ;;
  7) checkpoint_seven ;;
  8) checkpoint_eight ;;
  9) checkpoint_nine ;;
  10) checkpoint_ten ;;
  11) checkpoint_eleven ;;
  12) checkpoint_twelve ;;
esac
