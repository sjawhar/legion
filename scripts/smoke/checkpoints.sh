#!/usr/bin/env bash
set -euo pipefail

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

# The daemon's own persisted LegionState -- `GET /legion/v1/state` only ever returns `{project}`
# (see LegionDaemonApi.State.response and its handler), so the tree/role/gate/admission fields
# every checkpoint below reads have never been servable over that endpoint. `state_dir` in the
# generated `legion.yaml` is `${smoke_dir}/daemon` (see up.sh's `write_daemon_config`), and the
# daemon persists to `state.json` inside it -- local to this rig, so reading it directly needs no
# network round trip at all.
state() {
  local state_file="${smoke_dir}/daemon/state.json"
  [[ -r "$state_file" ]] || fail "daemon state file is missing: ${state_file}"
  cat "$state_file"
}
require_dispatch_token() {
  [[ -n "${DISPATCH_TOKEN:-}" ]] ||
    fail 'DISPATCH_TOKEN is required; run: secrets DISPATCH_TOKEN -- bash scripts/smoke/checkpoints.sh <n>'
}
dispatch_request() {
  local path="$1"
  require_env DISPATCH_URL
  require_dispatch_token
  curl --fail --silent --show-error \
    -H "Authorization: Bearer ${DISPATCH_TOKEN}" \
    "${DISPATCH_URL%/}/api/v1/${path}"
}

dispatch_issue() {
  dispatch_request "issues/$1"
}

dispatch_asks() {
  dispatch_request "issues/$1/asks?state=all"
}

dispatch_artifacts() {
  dispatch_request "issues/$1/artifacts"
}

dispatch_children() {
  local root="$1"
  local project="${root%%-*}"
  dispatch_request "issues?project=${project}&parent=${root}"
}
# The mode up.sh recorded for this rig, or an explicit export for a scratch directory up.sh never
# populated. Never a guess: forward now gates checkpoints 1-4 and 12, so a guessed mode would block
# them on a fact nobody recorded and send the operator after a setting that does not exist.
stored_webhook_mode() {
  local mode_file="${smoke_dir}/webhook-mode"
  if [[ -r "$mode_file" ]]; then
    printf '%s\n' "$(<"$mode_file")"
  elif [[ -n "${SMOKE_WEBHOOK_MODE:-}" ]]; then
    printf '%s\n' "$SMOKE_WEBHOOK_MODE"
  else
    fail "no recorded webhook mode at ${mode_file}; run up.sh, or export SMOKE_WEBHOOK_MODE=envoy|forward|none"
  fi
}

webhook_ingress_block_reason() {
  printf '%s\n' \
    'SMOKE_WEBHOOK_MODE=none: this checkpoint requires live GitHub webhook ingress; use SMOKE_WEBHOOK_MODE=envoy or forward'
}

# Checkpoints 1-4 and 12 read daemon state the root issue only reaches once the daemon has
# ingested its Dispatch issue events (`state.issues`; resync.ts healStatusDrift and
# reportRootAnomalies skip keys it never saw). Only up.sh's envoy-mode bridge relays those events
# into the rig NATS: none mode has no feed at all, and forward mode (`gh webhook forward`) carries
# GitHub events only -- up.sh creates the root issue over HTTP and the daemon never admits it -- so
# under either recorded mode these are blocked, never reported as a false FAILED.
dispatch_ingress_block_reason() {
  printf 'SMOKE_WEBHOOK_MODE=%s: this checkpoint requires Dispatch issue-event ingress; no Dispatch issue event reaches the rig NATS, so the daemon never admits the root issue; use SMOKE_WEBHOOK_MODE=envoy\n' "$1"
}



project_slug() {
  local project="$SMOKE_PROJECT"
  project="${project,,}"
  project="${project//[^a-z0-9]/}"
  printf '%s\n' "$project"
}

# Every Legion pane lives on the daemon's private tmux server, socket `legion-<slug>` (the same
# string as its session name); the default server never sees one.
legion_tmux() {
  tmux -L "legion-$(project_slug)" "$@"
}

# The root Dispatch issue for this exercise: `up.sh` creates it once and records its key at
# `${smoke_dir}/root-issue` for every later checkpoints.sh invocation to read (it never guesses
# by picking "the first parentless issue" -- LEGSMOKE is a shared project, and other concurrent
# rigs' own root issues would make that guess pick the wrong one). SMOKE_ROOT_ISSUE always
# overrides both, for pointing a single checkpoint at a specific exercise's root by hand.
smoke_root_issue() {
  if [[ -n "${SMOKE_ROOT_ISSUE:-}" ]]; then
    printf '%s\n' "$SMOKE_ROOT_ISSUE"
    return
  fi
  local root_file="${smoke_dir}/root-issue"
  [[ -s "$root_file" ]] ||
    fail "SMOKE_ROOT_ISSUE is unset and ${root_file} is missing or empty; run up.sh (it records the root issue there) or set SMOKE_ROOT_ISSUE"
  printf '%s\n' "$(<"$root_file")"
}

root_key() {
  smoke_root_issue
}
dispatch_root_key() {
  smoke_root_issue
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
  done < <(legion_tmux list-windows -t "legion-$(project_slug)" -F '#{window_id} #{window_name}')
  [[ "${#matches[@]}" == 1 ]] || fail "expected one tmux window named ${name}, found ${#matches[@]}"
  printf '%s\n' "${matches[0]}"
}

expect_window() {
  local window="$1"
  local windows
  windows="$(legion_tmux list-windows -t "legion-$(project_slug)" -F '#{window_name}')"
  grep -Fxq -- "$window" <<<"$windows" || fail "tmux window ${window} is absent"
}
expect_recorded_window() {
  local recorded_window_id="$1"
  local windows

  windows="$(legion_tmux list-windows -t "legion-$(project_slug)" -F '#{window_id}')"
  grep -Fxq -- "$recorded_window_id" <<<"$windows" ||
    fail "recorded tmux window ${recorded_window_id} is absent"
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
  local dispatch_issue_state
  local daemon_state
  local controller_window

  issue="$(dispatch_root_key)"
  dispatch_issue_state="$(dispatch_issue "$issue")"
  jq -e '
    .status as $status |
    ["icebox", "backlog", "todo", "in_progress", "testing", "needs_review", "retro", "done"] |
    index($status) != null
  ' >/dev/null <<<"$dispatch_issue_state" || fail "Dispatch issue ${issue} has not progressed past triage"
  daemon_state="$(state)"
  jq -e --arg issue "$issue" '.issues | has($issue)' >/dev/null <<<"$daemon_state" ||
    fail "daemon state lacks ${issue}"
  jq -e '
    .controllerLocator |
    (.tmuxWindowId | type == "string" and length > 0) and
    (.tmuxPaneId | type == "string" and length > 0)
  ' >/dev/null <<<"$daemon_state" || fail "daemon state lacks a controller window/pane locator"
  controller_window="$(jq -er '.controllerLocator.tmuxWindowId' <<<"$daemon_state")"
  expect_recorded_window "$controller_window"
  printf 'CHECKPOINT 1 OK: Dispatch tracks %s past triage; controller locator is live\n' "$issue"
}

checkpoint_two() {
  local root
  local dispatch_issue_state
  local daemon_state
  local architect_window

  root="$(dispatch_root_key)"
  dispatch_issue_state="$(dispatch_issue "$root")"
  jq -e '.status == "in_progress"' >/dev/null <<<"$dispatch_issue_state" ||
    fail "Dispatch issue ${root} is not in_progress"
  daemon_state="$(state)"
  jq -e --arg root "$root" '.admission.active | index($root) != null' >/dev/null <<<"$daemon_state" ||
    fail "${root} is not admitted"
  jq -e --arg root "$root" '
    .trees[$root].locator |
    (.tmuxWindowId | type == "string" and length > 0) and
    (.tmuxPaneId | type == "string" and length > 0)
  ' >/dev/null <<<"$daemon_state" || fail "${root} lacks an architect window/pane locator"
  architect_window="$(jq -er --arg root "$root" '.trees[$root].locator.tmuxWindowId' <<<"$daemon_state")"
  expect_recorded_window "$architect_window"
  printf 'CHECKPOINT 2 OK: Dispatch reports %s in_progress; architect locator is live\n' "$root"
}

# The rig runs with `gates.design: off` (up.sh): the daemon approves the gate the moment the
# architect registers it and closes the ask on Dispatch, so a smoke exercise never waits on a
# human. This checkpoint proves that whole path — the gate is registered and daemon-approved, and
# the ask the architect opened is `resolved` rather than sitting open in someone's inbox.
checkpoint_three() {
  local root
  local daemon_state
  local design_ask_id
  local asks
  local artifacts
  local children

  root="$(dispatch_root_key)"
  daemon_state="$(state)"
  design_ask_id="$(jq -er --arg root "$root" '.gates[$root].designAskId' <<<"$daemon_state")" ||
    fail "${root} has no registered design-gate ask"
  jq -e --arg root "$root" '.gates[$root].designApproved == "gate-off"' >/dev/null <<<"$daemon_state" ||
    fail "${root} design gate is registered but the daemon did not approve it (gates.design is not off?)"
  asks="$(dispatch_asks "$root")"
  jq -e --arg ask "$design_ask_id" '
    any(.[]; .id == $ask and .state == "resolved" and any(.options[]?; .label == "Approve"))
  ' >/dev/null <<<"$asks" || fail "${root} design-gate ask ${design_ask_id} is not resolved on Dispatch"
  artifacts="$(dispatch_artifacts "$root")"
  jq -e '
    any(.[]; .name == "spec.md" and .primary == true and (.versions | type == "array" and length > 0))
  ' >/dev/null <<<"$artifacts" || fail "${root} lacks a posted primary spec.md artifact"
  children="$(dispatch_children "$root")"
  jq -e --arg root "$root" 'any(.[]; .parent == $root)' >/dev/null <<<"$children" ||
    fail "${root} has no Dispatch child issue"
  printf 'CHECKPOINT 3 OK: posted spec artifact, daemon-approved design gate (ask resolved), and child issue observed\n'
}

checkpoint_four() {
  local root
  local daemon_state

  root="$(dispatch_root_key)"
  daemon_state="$(state)"
  jq -e --arg root "$root" '
    [
      .issues[$root].children[]? as $child |
      .issues[$child].status as $status |
      .trees[$child].status as $tree_status |
      select(
        ($status == "todo" or $status == "in_progress" or $status == "testing" or
          $status == "needs_review" or $status == "retro" or $status == "done") and
          ((.admission.active | index($child)) != null or $tree_status == "queued" or
            $tree_status == "active")
      )
    ] | length > 0
  ' >/dev/null <<<"$daemon_state" || fail "no child is released into admission or an active tree"
  printf 'CHECKPOINT 4 OK: a released child is tracked by admission or tree state\n'
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
  state | jq -e '[.prs[] | select(.verdict == "green")] | length > 0' >/dev/null ||
    fail "no PR recorded a green CI verdict"
  pane="$(legion_tmux capture-pane -p -t "$(tree_window_id "$root")")"
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
  local dispatch_issue_state
  local children

  root="$(dispatch_root_key)"
  dispatch_issue_state="$(dispatch_issue "$root")"
  jq -e '.status == "done"' >/dev/null <<<"$dispatch_issue_state" || fail "Dispatch issue ${root} is not done"
  children="$(dispatch_children "$root")"
  jq -e 'all(.[]; .status == "done")' >/dev/null <<<"$children" ||
    fail "${root} has a child issue that is not done"
  printf 'CHECKPOINT 9 OK: root issue and all its children are done\n'
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
  worker_pane="$(legion_tmux capture-pane -p -t "$(window_id "$SMOKE_WORKER_WINDOW")")"
  [[ "$worker_pane" == *"$SMOKE_COMMENT_FRAGMENT"* ]] || fail "worker transcript lacks the published comment"
  after_no_holder="${daemon_output#*no_holder}"
  [[ -n "$after_no_holder" && "$after_no_holder" != "$daemon_output" ]] ||
    fail "daemon log lacks no_holder"
  after_probe="${after_no_holder#*probe}"
  [[ "$after_probe" != "$after_no_holder" && "$after_probe" == *revive* ]] ||
    fail "daemon log lacks ordered no_holder → probe → revive handling"
  architect_pane="$(legion_tmux capture-pane -p -t "$(window_id "$SMOKE_ARCHITECT_WINDOW")")"
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
  windows="$(legion_tmux list-windows -t "legion-$(project_slug)" -F '#{window_name}' | jq -Rsc --arg window "$window" 'split("\n") | map(select(. == $window)) | length')"
  [[ "$windows" == "1" ]] || fail "expected one ${window} tmux window, found ${windows}"
  pane="$(legion_tmux capture-pane -p -t "$(tree_window_id "$issue")")"
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

# Spec LEGION-6 acceptance 2 and 4: no recorded Legion process — the private tmux server itself,
# the controller pane, every tree root pane, every worker pane — carries a bearer or boot secret on
# its argv or in its environment; the private server's global environment has none; and the default
# tmux server hosts no legion-<slug> session.
checkpoint_thirteen() {
  local slug socket server_pid pane pid entry name
  local -a pids=()
  slug="$(project_slug)"
  socket="legion-${slug}"
  server_pid="$(legion_tmux display-message -p '#{pid}')" || fail "private tmux server ${socket} is not running"
  pids+=("$server_pid")
  while IFS= read -r pane; do
    [[ -n "$pane" ]] || continue
    pid="$(legion_tmux display-message -p -t "$pane" '#{pane_pid}')" || fail "recorded pane ${pane} is absent from ${socket}"
    pids+=("$pid")
  done < <(state | jq -r '
    [ .controllerLocator.tmuxPaneId?,
      (.trees[]? | .locator.tmuxPaneId?),
      (.roles[]? | select(has("issue")) | .locator.tmuxPaneId?) ]
    | map(select(. != null)) | .[]')
  ((${#pids[@]} > 1)) || fail "daemon state records no pane to inspect"
  for pid in "${pids[@]}"; do
    [[ -r "/proc/${pid}/environ" && -r "/proc/${pid}/cmdline" ]] || fail "cannot read /proc/${pid}"
    for name in DISPATCH_TOKEN LEGION_BOOT_TOKEN LEGION_CONTROLLER_SECRET; do
      if entry="$(tr '\0' '\n' <"/proc/${pid}/environ" | grep -m1 "^${name}=")"; then
        fail "pid ${pid} environ carries ${entry%%=*}=… (expected only ${name}_FILE)"
      fi
      if entry="$(tr '\0' '\n' <"/proc/${pid}/cmdline" | grep -m1 "^${name}=")"; then
        fail "pid ${pid} cmdline carries ${entry%%=*}=…"
      fi
    done
  done
  if entry="$(legion_tmux show-environment -g | grep -m1 -E '^(DISPATCH_TOKEN|LEGION_BOOT_TOKEN|LEGION_CONTROLLER_SECRET)=')"; then
    fail "private tmux server global environment carries ${entry%%=*}"
  fi
  if tmux has-session -t "$socket" 2>/dev/null; then
    fail "default tmux server still hosts a ${socket} session"
  fi
  printf 'CHECKPOINT 13 OK: %d processes on %s carry no bearer or boot secret; default server hosts no %s\n' \
    "${#pids[@]}" "$socket" "$socket"
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

[[ $# -eq 1 && "$1" =~ ^([1-9]|1[0-3])$ ]] || {
  printf 'usage: %s <1-13>\n' "$0" >&2
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
webhook_mode="$(stored_webhook_mode)"
readonly webhook_mode
case "$webhook_mode" in
  none)
    case "$checkpoint" in
      1 | 2 | 3 | 4 | 12)
        blocked "$(dispatch_ingress_block_reason "$webhook_mode")"
        ;;
      5 | 6 | 7 | 9 | 10 | 11)
        blocked "$(webhook_ingress_block_reason)"
        ;;
    esac
    ;;
  forward)
    case "$checkpoint" in
      1 | 2 | 3 | 4 | 12)
        blocked "$(dispatch_ingress_block_reason "$webhook_mode")"
        ;;
    esac
    ;;
  envoy)
    ;;
  *)
    fail "recorded SMOKE_WEBHOOK_MODE must be forward, envoy, or none"
    ;;
esac

case "$checkpoint" in
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
  13) checkpoint_thirteen ;;
esac
