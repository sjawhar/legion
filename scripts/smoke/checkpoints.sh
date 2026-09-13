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

# shellcheck source=scripts/smoke/dispatch-config.sh
source "$(dirname "${BASH_SOURCE[0]}")/dispatch-config.sh"

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
dispatch_request() {
  local path="$1"
  resolve_dispatch_config
  curl --fail --silent --show-error \
    -H "Authorization: Bearer ${DISPATCH_TOKEN}" \
    "${DISPATCH_URL%/}/api/v1/${path}"
}

dispatch_issue() {
  dispatch_request "issues/$1"
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

# The design-gate policy `up.sh` wrote into the rig's `legion.yaml` and recorded beside it. `off`
# (the default) means the root architect was told to add no approval step; `root-issues` means a
# human approves the root's spec document between checkpoints 3 and 4.
stored_design_gate() {
  local gate_file="${smoke_dir}/design-gate"
  if [[ -r "$gate_file" ]]; then
    printf '%s\n' "$(<"$gate_file")"
  else
    printf '%s\n' "${SMOKE_DESIGN_GATE:-off}"
  fi
}

# Where the rig's Dispatch issue events come from, as `up.sh` recorded it (`SMOKE_DISPATCH_INGRESS`):
# `shared` (the default) — events arrive only through the `envoy` webhook mode's bridge; `rig` — a
# scratch Dispatch server publishes straight into the rig NATS, whatever the webhook mode. An
# unrecorded rig (a scratch directory up.sh never populated) is `shared` unless the variable says
# otherwise, so an old rig keeps today's gating.
stored_dispatch_ingress() {
  local ingress_file="${smoke_dir}/dispatch-ingress"
  if [[ -r "$ingress_file" ]]; then
    printf '%s\n' "$(<"$ingress_file")"
  else
    printf '%s\n' "${SMOKE_DISPATCH_INGRESS:-shared}"
  fi
}

webhook_ingress_block_reason() {
  printf '%s\n' \
    'SMOKE_WEBHOOK_MODE=none: this checkpoint requires live GitHub webhook ingress; use SMOKE_WEBHOOK_MODE=envoy or forward'
}

# Checkpoints 1-4 and 12 read daemon state the root issue only reaches once the daemon has
# ingested its Dispatch issue events (`state.issues`; resync.ts healStatusDrift and
# reportRootAnomalies skip keys it never saw). With the recorded Dispatch ingress `shared`, only
# up.sh's envoy-mode bridge relays those events into the rig NATS: none mode has no feed at all,
# and forward mode (`gh webhook forward`) carries GitHub events only -- up.sh creates the root
# issue over HTTP and the daemon never admits it -- so under either recorded mode these are
# blocked, never reported as a false FAILED. With `rig` ingress a scratch Dispatch publishes into
# the rig NATS directly, so the webhook mode says nothing about these checkpoints and the block is
# skipped (the dispatch below prints which record let it through).
dispatch_ingress_block_reason() {
  printf 'SMOKE_WEBHOOK_MODE=%s with SMOKE_DISPATCH_INGRESS=shared: this checkpoint requires Dispatch issue-event ingress; no Dispatch issue event reaches the rig NATS, so the daemon never admits the root issue; use SMOKE_WEBHOOK_MODE=envoy, or SMOKE_DISPATCH_INGRESS=rig when a scratch Dispatch publishes into the rig NATS\n' "$1"
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

# The pane pid and every process below it, breadth-first from one `ps` snapshot, one pid per line.
# Checkpoint 14 walks descendants because every pane runs `legion worker-shim -- <launch prefix>
# <omp>`: an identity `omp_launch_prefix` injects (`env LEGION_TREE=… secrets … -- mise x … -- omp`)
# sits on the OMP process and its children, never on the pane's own pid, so a pane-pid-only read
# would pass the very rig the check exists to fail (LEGION-88).
descendant_pids() {
  local snapshot
  snapshot="$(ps -eo pid=,ppid=)"
  local -a queue=("$1")
  local pid child
  while ((${#queue[@]} > 0)); do
    pid="${queue[0]}"
    queue=("${queue[@]:1}")
    printf '%s\n' "$pid"
    while IFS= read -r child; do
      [[ -n "$child" ]] && queue+=("$child")
    done < <(awk -v parent="$pid" '$2 == parent { print $1 }' <<<"$snapshot")
  done
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

# The Dispatch lifecycle statuses at or past `todo`: a child in one of them has been released by
# its architect (`release_wave`), so it is the one status set both `architect_parked` (none may
# be) and `checkpoint_four` (one must be) read. `triage`, `icebox`, and `backlog` are not released.
readonly released_statuses='["todo", "in_progress", "testing", "needs_review", "retro", "done"]'

# The phase-worker role claimed on the root itself in daemon state (a single-issue tree: any role
# other than the architect, e.g. planner or implementer), or nothing when only the architect is.
root_phase_worker() {
  local root="$1"
  local daemon_state="$2"
  jq -r --arg root "$root" '
    [.roles | to_entries[] | select(.value.issue == $root and .value.role != "architect") | .value.role]
    | first // empty
  ' <<<"$daemon_state"
}

# How the tree moved past the gate: a Dispatch child issue under the root (the decomposed path),
# or a phase-worker role claim on the root itself (`root_phase_worker`). Prints which one was
# observed, or fails naming the root when neither is.
tree_progress() {
  local root="$1"
  local daemon_state="$2"
  local children
  local worker
  children="$(dispatch_children "$root")"
  if jq -e --arg root "$root" 'any(.[]; .parent == $root)' >/dev/null <<<"$children"; then
    printf 'a child issue\n'
    return
  fi
  worker="$(root_phase_worker "$root" "$daemon_state")"
  if [[ -n "$worker" ]]; then
    printf 'a %s phase worker claimed on the root (single-issue tree)\n' "$worker"
    return
  fi
  fail "${root} has neither a Dispatch child issue nor a phase-worker role claim on the root: the tree has not moved past the gate"
}

# The architect parked on the gate: the human has not approved yet, so nothing may have moved past
# it. A child issue may exist in `triage` or `backlog` (the skill allows decomposing before the
# approval request) but none may be `todo` or later, and no phase-worker role (anything but the
# architect) may be claimed on the root or on any child in daemon state. Fails naming what moved.
architect_parked() {
  local root="$1"
  local daemon_state="$2"
  local children
  local moved
  children="$(dispatch_children "$root")"
  moved="$(jq -r --arg root "$root" --argjson released "$released_statuses" '
    [.[] | select(.parent == $root and (.status | IN($released[]))) | "\(.key) (\(.status))"]
    | first // empty
  ' <<<"$children")"
  [[ -z "$moved" ]] ||
    fail "${root}'s architect moved before approval: child issue ${moved} was released while the spec document is still awaiting approval"
  moved="$(jq -r --arg root "$root" --argjson children "$children" '
    ([$children[] | .key] + [$root]) as $tree |
    [.roles | to_entries[] | select((.value.issue | IN($tree[])) and .value.role != "architect") | "\(.value.role) on \(.value.issue)"]
    | first // empty
  ' <<<"$daemon_state")"
  [[ -z "$moved" ]] ||
    fail "${root}'s architect moved before approval: a ${moved} phase worker is claimed while the spec document is still awaiting approval"
}

# The daemon's design gate is a human's approval of the root spec document at a version. What this
# checkpoint proves depends on the policy the rig recorded (`stored_design_gate`):
# - `root-issues`: before the human acts, the architect must have registered the document
#   (`gates[root].artifactId`) at its current version (`latestVersion`) and Dispatch must show an
#   open approval request on it (`approval.state == "awaiting"`, from `dispatch_request_approval`).
# - `off`: the architect was told in its system prompt that the gate is off, so it must have
#   registered no gate and requested no approval — nothing waits in anyone's inbox.
# Under `root-issues` the checkpoint proves the architect asked and WAITED: with the document still
# awaiting approval, no child issue may be released and no phase worker may be claimed
# (`architect_parked`) — a compliant architect is parked on `design-approved` at this moment, and
# on a single-issue tree there is nothing else to observe. Under `off` it proves the tree moved
# past the (absent) gate — a child issue, or a phase worker on the root itself (`tree_progress`).
# Either way the spec is posted as the root's primary `spec.md`. No `Approve` ask is involved.
checkpoint_three() {
  local root
  local design_gate
  local daemon_state
  local gate_artifact
  local gate_version
  local artifacts
  local progress

  root="$(dispatch_root_key)"
  design_gate="$(stored_design_gate)"
  daemon_state="$(state)"
  artifacts="$(dispatch_artifacts "$root")"
  jq -e '
    any(.[]; .name == "spec.md" and .primary == true and (.versions | type == "array" and length > 0))
  ' >/dev/null <<<"$artifacts" || fail "${root} lacks a posted primary spec.md artifact"
  case "$design_gate" in
    root-issues)
      gate_artifact="$(jq -er --arg root "$root" '.gates[$root].artifactId' <<<"$daemon_state")" ||
        fail "${root} has no registered design gate"
      gate_version="$(jq -er --arg root "$root" '.gates[$root].latestVersion' <<<"$daemon_state")" ||
        fail "${root}'s registered design gate records no latestVersion"
      jq -e --arg id "$gate_artifact" '
        any(.[]; .id == $id and .name == "spec.md" and .primary == true)
      ' >/dev/null <<<"$artifacts" ||
        fail "${root}'s registered design gate ${gate_artifact} is not its posted primary spec.md artifact"
      jq -e --arg id "$gate_artifact" --argjson version "$gate_version" '
        any(.[]; .id == $id and ([.versions[].number] | max) == $version)
      ' >/dev/null <<<"$artifacts" ||
        fail "${root}'s registered design gate is not at the spec document's current version ${gate_version}"
      jq -e --arg id "$gate_artifact" '
        any(.[]; .id == $id and .approval.state == "awaiting")
      ' >/dev/null <<<"$artifacts" ||
        fail "${root} has no open approval request on its registered spec document (approval.state must be awaiting; a Dispatch server without document approval never reports one)"
      architect_parked "$root" "$daemon_state"
      printf 'CHECKPOINT 3 OK: posted spec artifact awaiting approval, registered design gate, and the architect parked on it (nothing released, no phase worker)\n'
      ;;
    off)
      jq -e --arg root "$root" '.gates | has($root) | not' >/dev/null <<<"$daemon_state" ||
        fail "${root} registered a design gate although the rig runs with gates.design: off (the architect ignored its Design gate policy line)"
      jq -e '
        any(.[]; .name == "spec.md" and .primary == true and (.approval.state // "draft") == "awaiting") | not
      ' >/dev/null <<<"$artifacts" ||
        fail "${root} has an open approval request on its spec document although the rig runs with gates.design: off (a question is waiting in a human's inbox)"
      progress="$(tree_progress "$root" "$daemon_state")"
      printf 'CHECKPOINT 3 OK: posted spec artifact, no design gate or approval request (gates.design: off), and %s observed\n' "$progress"
      ;;
    *)
      fail "recorded design-gate policy '${design_gate}' is neither off nor root-issues"
      ;;
  esac
}

# Under `root-issues`, between checkpoints 3 and 4 a human approves the root spec document (the
# document header's Approve, or the approval ask with Approve); this checkpoint proves the approval
# opened the gate (`approvedVersion == latestVersion`) and the tree then moved. Under `off` there
# is no gate to check; only the move itself is observed.
#
# LEGION-57: a released child is owned by a sub-architect running as a phase worker inside its
# parent's tree -- never a tree or admission entry of its own (the reducer ignores a child's `todo`
# while an ancestor tree is live; the first `spawn_worker` of the child's architect is what starts
# it and writes its `in_progress`). So the move is either a child Dispatch reports at `in_progress`
# or later holding an architect role claim whose locator is a worker pane, or -- only while no
# child of the root is released (`todo` or later on Dispatch), a single-issue tree -- a phase-worker
# role claim on the root itself (`root_phase_worker`). A released child without that claim fails
# whatever runs on the root: a phase worker on the root does not own a child, and the window after
# `release_wave` and before `spawn_worker` is not yet the fixed state. Either way, no child of the
# root may be a tree or admission entry. Dispatch is the surface for the child's status; the
# daemon's persisted state is the surface for admission and role claims. A worker claim's locator
# is a worker pane by construction (only `launchWorker` writes a locator onto a role claim; the
# root architect's own claim never carries one), and the tree-absence assertion rules out the
# child holding a root pane of its own.
checkpoint_four() {
  local root
  local design_gate
  local daemon_state
  local children
  local offender
  local owned
  local unowned
  local pending
  local released

  root="$(dispatch_root_key)"
  design_gate="$(stored_design_gate)"
  daemon_state="$(state)"
  if [[ "$design_gate" == root-issues ]]; then
    jq -e --arg root "$root" '
      .gates[$root] | (.approvedVersion != null) and (.approvedVersion == .latestVersion)
    ' >/dev/null <<<"$daemon_state" ||
      fail "daemon has not recorded the spec approval for ${root} (gates[${root}].approvedVersion must equal latestVersion)"
  fi
  children="$(dispatch_children "$root")"
  offender="$(jq -r --arg root "$root" --argjson children "$children" '
    . as $state |
    [ $children[] | select(.parent == $root) | .key as $child |
      select(
        $state.trees[$child] != null or
          (($state.admission.queue // []) | index($child)) != null or
          (($state.admission.active // []) | index($child)) != null
      ) | $child ] | first // empty
  ' <<<"$daemon_state")"
  [[ -z "$offender" ]] || fail "child ${offender} is admitted as a tree of its own (LEGION-57)"
  # `in_progress` is what the first sub-architect spawn writes, and every later status passes
  # through it; `todo` is released but not yet spawned -- not yet the fixed state.
  owned="$(jq -r --arg root "$root" --argjson children "$children" '
    . as $state |
    [ $children[] | select(.parent == $root) | . as $child |
      select(
        ($child.status == "in_progress" or $child.status == "testing" or
          $child.status == "needs_review" or $child.status == "retro" or $child.status == "done") and
          any($state.roles[]?;
            has("issue") and .issue == $child.key and .role == "architect" and
              (.locator.tmuxPaneId | type == "string" and length > 0))
      ) | "\(.key) \(.status)" ] | first // empty
  ' <<<"$daemon_state")"
  if [[ -n "$owned" ]]; then
    released="released child ${owned%% *} has no tree or admission entry of its own; its architect runs as a worker pane of ${root} and Dispatch reports it ${owned##* }"
  else
    # A released child the sub-architect claim already covers, still `todo` on Dispatch: the
    # `in_progress` PATCH in flight during `spawn_worker`, or a failed one parked in the daemon's
    # `pendingStatusWrites` for resync. Named as such -- the claim is not missing.
    pending="$(jq -r --arg root "$root" --argjson children "$children" '
      . as $state |
      [ $children[] | select(.parent == $root and .status == "todo") | .key as $child |
        select(any($state.roles[]?;
          has("issue") and .issue == $child and .role == "architect" and
            (.locator.tmuxPaneId | type == "string" and length > 0))) | $child ] | first // empty
    ' <<<"$daemon_state")"
    [[ -z "$pending" ]] ||
      fail "released child ${pending} has a sub-architect worker pane on ${root} but Dispatch still reports it todo (the in_progress PATCH is in flight, or failed and is parked in the daemon's pendingStatusWrites for resync)"
    unowned="$(jq -r --arg root "$root" --argjson released "$released_statuses" '
      [ .[] | select(.parent == $root and (.status | IN($released[]))) | "\(.key) (\(.status))" ]
      | first // empty
    ' <<<"$children")"
    [[ -z "$unowned" ]] ||
      fail "released child ${unowned} holds no sub-architect role claim with a worker pane on ${root} (LEGION-57: the parent architect's spawn_worker for its architect is what starts a child; a phase worker on the root does not own it)"
    released="$(root_phase_worker "$root" "$daemon_state")"
    [[ -n "$released" ]] ||
      fail "neither a released child holds a sub-architect role claim with a worker pane nor a phase worker is claimed on ${root}"
    released="a ${released} phase worker claimed on the root (single-issue tree)"
  fi
  if [[ "$design_gate" == root-issues ]]; then
    printf 'CHECKPOINT 4 OK: spec approval recorded on the gate; %s\n' "$released"
  else
    printf 'CHECKPOINT 4 OK: %s\n' "$released"
  fi
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

# Spec LEGION-6 acceptance 2 and 4, and LEGION-74: no recorded Legion process — the private tmux
# server itself, the controller pane, every tree root pane, every worker pane — carries a bearer, a
# boot secret, or either GitHub App private key (the daemon's own, for token minting; a pane must
# never see them) on its argv or in its environment; the private server's global environment has
# none; and the default tmux server hosts no legion-<slug> session. SMOKE_CANARY_ENV is a
# space-separated list of further names the operator planted in the daemon's environment
# (e.g. FOO_SECRET=canary) to prove the allow-list drops what it does not name.
checkpoint_thirteen() {
  local slug socket server_pid pane pid entry name pattern joined
  local -a pids=()
  local -a names=(DISPATCH_TOKEN LEGION_BOOT_TOKEN LEGION_CONTROLLER_SECRET GH_AGENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64)
  local -a canaries=()
  read -r -a canaries <<<"${SMOKE_CANARY_ENV:-}"
  names+=("${canaries[@]}")
  pattern="$(IFS='|'; printf '%s' "${names[*]}")"
  joined="$(IFS=' '; printf '%s' "${names[*]}")"
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
    for name in "${names[@]}"; do
      if entry="$(tr '\0' '\n' <"/proc/${pid}/environ" | grep -m1 "^${name}=")"; then
        fail "pid ${pid} environ carries ${entry%%=*}=… (no Legion pane may carry it)"
      fi
      if entry="$(tr '\0' '\n' <"/proc/${pid}/cmdline" | grep -m1 "^${name}=")"; then
        fail "pid ${pid} cmdline carries ${entry%%=*}=…"
      fi
    done
  done
  if entry="$(legion_tmux show-environment -g | grep -m1 -E "^(${pattern})=")"; then
    fail "private tmux server global environment carries ${entry%%=*}"
  fi
  if tmux has-session -t "$socket" 2>/dev/null; then
    fail "default tmux server still hosts a ${socket} session"
  fi
  printf 'CHECKPOINT 13 OK: %d processes on %s carry none of %s; default server hosts no %s\n' \
    "${#pids[@]}" "$socket" "$joined" "$socket"
}

# LEGION-88: a rig prints RIG READY and passes checkpoint 1 with a controller that is alive but has
# never claimed its role -- the shape both LEGION-72 reproductions took, when a rig daemon built
# before LEGION-74's allow-list handed the launching worker pane's LEGION_TREE to its controller
# pane and the extension refused the both-markers session. Two facts, in every webhook mode: the
# controller's role claim is in daemon state, and every recorded pane's process tree carries only
# the Legion identity the daemon set for it -- the controller none of LEGION_TREE, LEGION_ISSUE,
# LEGION_GENERATION, LEGION_WORKSPACE; a root or worker pane a LEGION_TREE/LEGION_ISSUE equal to the
# tree and issue the daemon recorded for it (a sub-architect on a child has LEGION_TREE the root
# and LEGION_ISSUE the child, mirroring the daemon's liveAncestorTree) -- and the private server's
# global table names no LEGION_*. Descendants are walked (see descendant_pids). Absence never fails
# a root or worker pane: a worker's own subprocess may legitimately run under `env -u LEGION_TREE …`
# (docs/solutions/legion/worker-pane-shell-gotchas.md), and the pane pid itself always carries both
# from its -e pairs. An observed value is compared, never printed; the recorded key is.
checkpoint_fourteen() {
  local slug socket token entry pane kind tree issue pid p environ name
  local panes=0 processes=0
  slug="$(project_slug)"
  socket="legion-${slug}"
  token="legion-${slug}-controller"
  state | jq -e --arg token "$token" '
    .roles[$token] | type == "object" and .role == "controller" and (.sessionId | type == "string" and length > 0)
  ' >/dev/null ||
    fail "daemon state has no controller role claim (${token}): the controller pane never registered — attach with \`tmux -L ${socket} attach -t ${socket}\` and read its pane"
  while IFS=$'\t' read -r pane kind tree issue; do
    [[ -n "$pane" ]] || continue
    panes=$((panes + 1))
    pid="$(legion_tmux display-message -p -t "$pane" '#{pane_pid}')" || fail "recorded pane ${pane} is absent from ${socket}"
    [[ -r "/proc/${pid}/environ" ]] || fail "cannot read /proc/${pid}"
    while IFS= read -r p; do
      [[ -n "$p" ]] || continue
      # A child that exited between the snapshot and this read proves nothing either way.
      [[ -d "/proc/${p}" ]] || continue
      [[ -r "/proc/${p}/environ" ]] || fail "cannot read /proc/${p} (under recorded pane ${pane})"
      environ="$(tr '\0' '\n' <"/proc/${p}/environ")"
      processes=$((processes + 1))
      if [[ "$kind" == controller ]]; then
        for name in LEGION_TREE LEGION_ISSUE LEGION_GENERATION LEGION_WORKSPACE; do
          if grep -q "^${name}=" <<<"$environ"; then
            fail "controller pane ${pane} (pid ${p}) carries ${name}: a Legion identity the daemon did not set for it — the rig launcher inherited it from an outer Legion pane, or the launch prefix injected it"
          fi
        done
      else
        if entry="$(grep -m1 '^LEGION_TREE=' <<<"$environ")" && [[ "${entry#*=}" != "$tree" ]]; then
          fail "pane ${pane} (pid ${p}, ${kind} ${issue}) carries a LEGION_TREE that differs from the tree the daemon recorded for it (${tree})"
        fi
        if entry="$(grep -m1 '^LEGION_ISSUE=' <<<"$environ")" && [[ "${entry#*=}" != "$issue" ]]; then
          fail "pane ${pane} (pid ${p}, ${kind} ${issue}) carries a LEGION_ISSUE that differs from the issue the daemon recorded for it (${issue})"
        fi
      fi
    done < <(descendant_pids "$pid")
  done < <(state | jq -r '
    . as $s
    | def tree_of(k): if $s.trees[k] then k elif ($s.issues[k].parent? // null) then tree_of($s.issues[k].parent) else k end;
    [ {pane: .controllerLocator.tmuxPaneId?, kind: "controller", tree: "", issue: ""},
      (.trees | to_entries[] | {pane: .value.locator.tmuxPaneId?, kind: "root", tree: .key, issue: .key}),
      (.roles | to_entries[] | select(.value | has("issue"))
        | {pane: .value.locator.tmuxPaneId?, kind: "worker", tree: tree_of(.value.issue), issue: .value.issue}) ]
    | map(select(.pane != null))
    | .[] | [.pane, .kind, .tree, .issue] | @tsv')
  ((panes > 0)) || fail "daemon state records no pane to inspect"
  if entry="$(legion_tmux show-environment -g | grep -m1 -E '^LEGION_[A-Za-z0-9_]*=')"; then
    fail "private tmux server global environment names ${entry%%=*}"
  fi
  printf 'CHECKPOINT 14 OK: controller claim %s present; %d processes under %d recorded panes carry only the Legion identity the daemon set; private server global environment names no LEGION_*\n' \
    "$token" "$processes" "$panes"
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

[[ $# -eq 1 && "$1" =~ ^([1-9]|1[0-4])$ ]] || {
  printf 'usage: %s <1-14>\n' "$0" >&2
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
dispatch_ingress="$(stored_dispatch_ingress)"
readonly dispatch_ingress
case "$dispatch_ingress" in
  shared | rig) ;;
  *)
    fail "recorded SMOKE_DISPATCH_INGRESS must be shared or rig"
    ;;
esac
# Whether the recorded webhook mode decides checkpoints 1-4 and 12: only when the rig's Dispatch
# issue events have no other way in (`shared`). Under `rig` the scratch Dispatch publishes into
# the rig NATS itself, so the same webhook mode blocks nothing for them.
dispatch_ingress_gate() {
  if [[ "$dispatch_ingress" == rig ]]; then
    printf 'CHECKPOINT %s: SMOKE_WEBHOOK_MODE=%s does not block this checkpoint; the recorded SMOKE_DISPATCH_INGRESS=rig says a scratch Dispatch publishes issue events into the rig NATS directly\n' "$checkpoint" "$webhook_mode" >&2
    return
  fi
  blocked "$(dispatch_ingress_block_reason "$webhook_mode")"
}
case "$webhook_mode" in
  none)
    case "$checkpoint" in
      1 | 2 | 3 | 4 | 12)
        dispatch_ingress_gate
        ;;
      5 | 6 | 7 | 9 | 10 | 11)
        blocked "$(webhook_ingress_block_reason)"
        ;;
    esac
    ;;
  forward)
    case "$checkpoint" in
      1 | 2 | 3 | 4 | 12)
        dispatch_ingress_gate
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
  14) checkpoint_fourteen ;;
esac
