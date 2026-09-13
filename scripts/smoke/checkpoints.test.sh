#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly checkpoints_script="${project_root}/scripts/smoke/checkpoints.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin="${temporary_dir}/bin"
readonly smoke_dir="${temporary_dir}/smoke"
readonly curl_log="${temporary_dir}/curl.log"
export CURL_LOG="$curl_log"
export TMUX_LOG="${temporary_dir}/tmux.log"
output_file="${temporary_dir}/output"
trap 'rm -rf "$temporary_dir"' EXIT

mkdir -p "$fake_bin" "$smoke_dir" "${smoke_dir}/daemon"
# Checkpoints 1-4 need Dispatch issue-event ingress, so their fixture-backed OK cases run with
# envoy recorded; the none-mode and forward-mode blocks further down prove the mode gating itself.
printf 'envoy\n' >"${smoke_dir}/webhook-mode"
# The daemon state every checkpoint reads. `write_state` takes the `gates` record so the
# design-gate runs below can vary it; the default carries no gate, matching the rig's default
# `gates.design: off` (recorded in `design-gate` exactly as `up.sh` records it). LEGSMOKE-2 is
# LEGSMOKE-1's released child under LEGION-57's model: no tree or admission entry of its own, an
# architect (sub-architect) role claim whose locator is a worker pane, Dispatch status
# `in_progress` (what the first sub-architect spawn writes).
write_state() {
  printf '%s' '{"issues":{"LEGSMOKE-1":{"status":"in_progress","children":["LEGSMOKE-2"]},"LEGSMOKE-2":{"parent":"LEGSMOKE-1","status":"in_progress","children":[]},"LEGSMOKE-99":{"status":"in_progress","children":[]}},"trees":{"LEGSMOKE-1":{"root":"LEGSMOKE-1","status":"active","locator":{"tmuxWindowId":"@2","tmuxPaneId":"%2"}}},"controllerLocator":{"tmuxWindowId":"@1","tmuxPaneId":"%1"},"roles":{"legion-exampleorg24-legsmoke-2-architect":{"issue":"LEGSMOKE-2","role":"architect","locator":{"tmuxWindowId":"@3","tmuxPaneId":"%3"}},"legion-exampleorg24-legsmoke-2-tester":{"issue":"LEGSMOKE-2","role":"tester","locator":{"tmuxWindowId":"@3","tmuxPaneId":"%4"}}},"admission":{"active":["LEGSMOKE-1"],"queue":[]},"gates":'"$1"'}' >"${smoke_dir}/daemon/state.json"
}
# The same rig as a single-issue tree: LEGSMOKE-1 has no child, and the architect spawned a
# planner on the root itself (`roles` carries a non-architect claim with issue == root). `$1` is
# the gates record, `$2` the role claimed on the root (default planner; pass "architect" to model
# a tree that has not moved past the gate at all).
write_single_issue_state() {
  local role="${2:-planner}"
  printf '%s' '{"issues":{"LEGSMOKE-1":{"status":"in_progress","children":[]},"LEGSMOKE-99":{"status":"in_progress","children":[]}},"trees":{"LEGSMOKE-1":{"root":"LEGSMOKE-1","status":"active","locator":{"tmuxWindowId":"@2","tmuxPaneId":"%2"}}},"controllerLocator":{"tmuxWindowId":"@1","tmuxPaneId":"%1"},"roles":{"legion-exampleorg24-legsmoke-1-architect":{"issue":"LEGSMOKE-1","role":"architect","sessionId":"ses_arch"},"legion-exampleorg24-legsmoke-1-'"$role"'":{"issue":"LEGSMOKE-1","role":"'"$role"'","sessionId":"ses_'"$role"'","locator":{"tmuxWindowId":"@2","tmuxPaneId":"%5"}}},"admission":{"active":["LEGSMOKE-1"]},"gates":'"$1"'}' >"${smoke_dir}/daemon/state.json"
}
# The armed rig at the moment checkpoint 3 runs: the architect asked for approval and PARKED.
# Here it decomposed first — child LEGSMOKE-2 exists in `triage` (the skill allows creating
# children before the approval request) — but released nothing and claimed no worker.
write_parked_state() {
  printf '%s' '{"issues":{"LEGSMOKE-1":{"status":"in_progress","children":["LEGSMOKE-2"]},"LEGSMOKE-2":{"parent":"LEGSMOKE-1","status":"triage","children":[]},"LEGSMOKE-99":{"status":"in_progress","children":[]}},"trees":{"LEGSMOKE-1":{"root":"LEGSMOKE-1","status":"active","locator":{"tmuxWindowId":"@2","tmuxPaneId":"%2"}}},"controllerLocator":{"tmuxWindowId":"@1","tmuxPaneId":"%1"},"roles":{"legion-exampleorg24-legsmoke-1-architect":{"issue":"LEGSMOKE-1","role":"architect","sessionId":"ses_arch"}},"admission":{"active":["LEGSMOKE-1"]},"gates":'"$1"'}' >"${smoke_dir}/daemon/state.json"
}
write_state '{}'
printf 'off\n' >"${smoke_dir}/design-gate"
# LEGSMOKE-1 is this rig's own root; LEGSMOKE-99 is a second parentless issue with no relation to
# it at all, standing in for a concurrent rig's own root sharing the same LEGSMOKE project. The
# recorded root-issue file below must make every checkpoint below target LEGSMOKE-1 regardless --
# a regression back to guessing "the first parentless issue" could as easily land on LEGSMOKE-99,
# which has no case in the fake curl script below and would fail loudly instead of silently
# checking the wrong exercise.
printf 'LEGSMOKE-1\n' >"${smoke_dir}/root-issue"
cat >"${fake_bin}/curl" <<'EOF'
#!/usr/bin/env bash
request="$*"
printf '%s\n' "$request" >>"$CURL_LOG"
case "$request" in
  *"/api/v1/issues/LEGSMOKE-1/artifacts"*)
    if [[ -n "${ARTIFACTS_FILE:-}" ]]; then
      cat "$ARTIFACTS_FILE"
    else
      printf '%s' '[{"id":"art-spec","name":"spec.md","primary":true,"versions":[{"number":1}],"approval":{"state":"draft","latest_version":1}}]'
    fi
    ;;
  *"/api/v1/issues?project=LEGSMOKE&parent=LEGSMOKE-1"*)
    if [[ -n "${CHILDREN_FILE:-}" ]]; then
      cat "$CHILDREN_FILE"
    else
      printf '%s' '[{"key":"LEGSMOKE-2","parent":"LEGSMOKE-1","status":"in_progress"}]'
    fi
    ;;
  *"/api/v1/issues/LEGSMOKE-1"*)
    printf '%s' '{"key":"LEGSMOKE-1","status":"in_progress"}'
    ;;
  *)
    printf 'unexpected curl request: %s\n' "$request" >&2
    exit 1
    ;;
esac
EOF
# Understands the `-L <socket>` global option the real daemon and checkpoints.sh use, logging
# `<socket> <args…>` per invocation so the harness can assert every call targeted the private
# server. `has-session` on the default server (no socket) reports no session. `display-message`
# answers the server's own pid (`FAKE_TMUX_SERVER_PID`, defaulting to `FAKE_TMUX_PID`) when asked
# without a `-t` target and a pane's pid (`FAKE_TMUX_PID`) otherwise, so checkpoint 13's server and
# pane checks can be pointed at different real processes; `show-environment -g` appends
# `FAKE_TMUX_GLOBAL_ENV` when set, standing in for a global table the daemon never scrubbed.
cat >"${fake_bin}/tmux" <<'EOF'
#!/usr/bin/env bash
socket=""
if [[ "${1:-}" == "-L" ]]; then socket="$2"; shift 2; fi
printf '%s\n' "$socket $*" >>"${TMUX_LOG:-/dev/null}"
case "${1:-}" in
  has-session) [[ -n "$socket" ]] ;;
  display-message) if [[ "$*" == *" -t "* ]]; then printf '%s\n' "$FAKE_TMUX_PID"; else printf '%s\n' "${FAKE_TMUX_SERVER_PID:-$FAKE_TMUX_PID}"; fi ;;
  show-environment) printf 'PATH=/usr/bin\n'; [[ -z "${FAKE_TMUX_GLOBAL_ENV:-}" ]] || printf '%s\n' "$FAKE_TMUX_GLOBAL_ENV" ;;
  list-windows|list-panes)
    if [[ "$*" == *"#{window_id}"* ]]; then printf '@1\n@2\n@3\n'; else printf 'controller\nlegsmoke-1\nlegsmoke-2\n'; fi ;;
  *) printf 'controller\nlegsmoke-1\nlegsmoke-2\n' ;;
esac
EOF
cat >"${fake_bin}/gh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${fake_bin}/curl" "${fake_bin}/gh" "${fake_bin}/tmux"

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 1 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 1 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
grep -Fq 'Authorization: Bearer test-dispatch-token' "$curl_log"
grep -Fq 'http://dispatch.test/api/v1/issues/LEGSMOKE-1' "$curl_log"

# Checkpoint 13 inspects `/proc/<pid>/environ` of every pid the fake tmux reports, so those pids
# must be real, long-lived processes the harness owns: one with a clean environment, one with a
# planted `DISPATCH_TOKEN` plus a planted App key and canary, one with only the canary, one with a
# planted `OMP_SESSION_ID`, and one with a planted `DISPATCH_TOKEN` at the head of a 70 KB
# environment (a `$PPID` trick would name a command-substitution subshell that has already exited
# by the time /proc is read). Each scrubs every variable the checkpoint inspects from whatever shell
# runs this harness (a Legion worker's own pane carries a boot token and, before LEGION-74, the
# launching shell's OMP_SESSION_ID; a box's panes may carry the keys).
scrubbed=(-u DISPATCH_TOKEN -u LEGION_BOOT_TOKEN -u LEGION_CONTROLLER_SECRET
  -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 -u FOO_SECRET -u OMP_SESSION_ID)
env "${scrubbed[@]}" sleep 300 &
clean_pid=$!
env "${scrubbed[@]}" DISPATCH_TOKEN="leaked-into-a-pane" GH_AGENT_APP_PRIVATE_KEY_B64="leaked-into-a-pane" FOO_SECRET="leaked-canary" sleep 300 &
planted_pid=$!
env "${scrubbed[@]}" FOO_SECRET="leaked-canary" sleep 300 &
canary_pid=$!
env "${scrubbed[@]}" OMP_SESSION_ID="01a083dd-4579-7000-8202-9898ac713e12" sleep 300 &
planted_session_pid=$!
env -i DISPATCH_TOKEN="leaked-into-a-large-pane" LARGE_PAD="$(head -c 70000 /dev/zero | tr '\0' x)" sleep 300 &
planted_large_pid=$!
trap 'kill "$clean_pid" "$planted_pid" "$canary_pid" "$planted_session_pid" "$planted_large_pid" 2>/dev/null; rm -rf "$temporary_dir"' EXIT

checkpoint_thirteen_against() {
  # $1: the pid the fake tmux reports for every pane; $2: SMOKE_CANARY_ENV (may be empty);
  # $3: the pid it reports as the private server's own (defaults to the clean process, so a case
  # exercises the pane rule unless it names a server); $4: an extra `show-environment -g` line
  # (may be empty), standing in for a global table the daemon never scrubbed.
  PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_REPO="example-org/legion-smoke" \
    SMOKE_PROJECT="example-org/24" DISPATCH_URL="http://dispatch.test" FAKE_TMUX_PID="$1" \
    SMOKE_CANARY_ENV="$2" FAKE_TMUX_SERVER_PID="${3:-$clean_pid}" FAKE_TMUX_GLOBAL_ENV="${4:-}" \
    env -u DISPATCH_TOKEN bash "$checkpoints_script" 13 >"$output_file" 2>&1
}

if ! checkpoint_thirteen_against "$clean_pid" ""; then
  cat "$output_file" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'CHECKPOINT 13 OK'*'GH_REVIEW_APP_PRIVATE_KEY_B64'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 13 passes when no recorded process carries a secret\n'

if checkpoint_thirteen_against "$planted_pid" ""; then
  printf 'expected checkpoint 13 to fail when a recorded process environment carries DISPATCH_TOKEN\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"pid ${planted_pid} environ carries DISPATCH_TOKEN="* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 13 fails naming the pid and variable when a recorded process environment carries it\n'

# The fixed names are checked before any canary: a planted App key is caught with no
# SMOKE_CANARY_ENV configured at all.
env "${scrubbed[@]}" GH_AGENT_APP_PRIVATE_KEY_B64="leaked-into-a-pane" sleep 300 &
app_key_pid=$!
trap 'kill "$clean_pid" "$planted_pid" "$canary_pid" "$app_key_pid" 2>/dev/null; rm -rf "$temporary_dir"' EXIT
if checkpoint_thirteen_against "$app_key_pid" ""; then
  printf 'expected checkpoint 13 to fail when a recorded process environment carries a GitHub App private key\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"pid ${app_key_pid} environ carries GH_AGENT_APP_PRIVATE_KEY_B64="* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 13 fails on a planted GitHub App private key with no canary configured\n'

if ! checkpoint_thirteen_against "$canary_pid" ""; then
  cat "$output_file" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'CHECKPOINT 13 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
if checkpoint_thirteen_against "$canary_pid" "FOO_SECRET"; then
  printf 'expected checkpoint 13 to fail when SMOKE_CANARY_ENV names a variable a recorded process carries\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"pid ${canary_pid} environ carries FOO_SECRET="* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 13 inspects an operator-planted canary only when SMOKE_CANARY_ENV names it\n'

# LEGION-43: the launching shell's OMP_SESSION_ID is checked beside the secrets, with its own
# message (a pane's OMP mints its own session id; no `_FILE` twin exists for it).
if checkpoint_thirteen_against "$planted_session_pid" ""; then
  printf 'expected checkpoint 13 to fail when a recorded process environment carries OMP_SESSION_ID\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"pid ${planted_session_pid} environ carries OMP_SESSION_ID="*"a pane's OMP mints its own session id"* && "$(<"$output_file")" != *'_FILE'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 13 fails naming the pid and OMP_SESSION_ID, without promising an _FILE twin, when a recorded process inherited a session id\n'

# The pane rule must bite whatever the environment's size. The planted secret heads a 70 KB
# environment — past the pipe buffer, where a read with the matching process downstream of a
# writer can miss under pipefail — and the checkpoint runs 20 times, since such a miss is a
# scheduling race that one run cannot rule out.
((  $(wc -c <"/proc/${planted_large_pid}/environ") > 65536 )) || {
  printf 'the large planted process environment must exceed the 64 KB pipe buffer\n' >&2
  exit 1
}
for attempt in $(seq 1 20); do
  if checkpoint_thirteen_against "$planted_large_pid" ""; then
    printf 'expected checkpoint 13 to fail when a recorded process with a 70 KB environment carries DISPATCH_TOKEN (attempt %s)\n' "$attempt" >&2
    exit 1
  fi
  [[ "$(<"$output_file")" == *"pid ${planted_large_pid} environ carries DISPATCH_TOKEN="* ]] || {
    cat "$output_file" >&2
    exit 1
  }
done
printf 'PASS: checkpoint 13 fails naming the pid and variable 20/20 times when the planted secret heads a 70 KB environment\n'

# LEGION-43 acceptance 6: a private server that outlived a pre-LEGION-74 daemon was forked with the
# launching shell's OMP_SESSION_ID, and /proc/<server pid>/environ records that for the server's
# whole life -- but the daemon has scrubbed the tmux tables new panes actually inherit, so a clean
# pane on that server passes, with the frozen record reported as a note.
if ! checkpoint_thirteen_against "$clean_pid" "" "$planted_session_pid"; then
  cat "$output_file" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"CHECKPOINT 13 NOTE: private tmux server pid ${planted_session_pid} was forked with OMP_SESSION_ID in its environment"* && "$(<"$output_file")" == *'CHECKPOINT 13 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 13 passes, noting the frozen record, when only the surviving server process was forked with OMP_SESSION_ID and its panes are clean\n'

# A bearer, boot secret, or App key in that same frozen record is the opposite case (LEGION-6): it
# is readable for as long as the server lives and no table scrub can remove it, so it fails even
# though every pane is clean, and the message names the only remedy (kill the private server once).
if checkpoint_thirteen_against "$clean_pid" "" "$planted_pid"; then
  printf 'expected checkpoint 13 to fail when the surviving server process was forked with DISPATCH_TOKEN\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"CHECKPOINT 13 FAILED: private tmux server pid ${planted_pid} was forked with DISPATCH_TOKEN="*"tmux -L legion-exampleorg24 kill-server"* && "$(<"$output_file")" != *'CHECKPOINT 13 NOTE'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 13 fails naming the secret and the kill-server remedy when the surviving server process was forked with DISPATCH_TOKEN\n'

# The global table is the operative fact for the server: one that still lists the id (a daemon
# that never scrubbed it) fails even though every pane is clean.
if checkpoint_thirteen_against "$clean_pid" "" "$clean_pid" "OMP_SESSION_ID=01a083dd-4579-7000-8202-9898ac713e12"; then
  printf 'expected checkpoint 13 to fail when the private server global environment carries OMP_SESSION_ID\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'private tmux server global environment carries OMP_SESSION_ID'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 13 fails naming the variable when the private server global environment still carries OMP_SESSION_ID\n'

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 2 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 2 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
grep -Fq 'http://dispatch.test/api/v1/issues/LEGSMOKE-1' "$curl_log"

run_checkpoint() {
  PATH="${fake_bin}:${PATH}" \
    SMOKE_DIR="$smoke_dir" \
    SMOKE_REPO="example-org/legion-smoke" \
    SMOKE_PROJECT="example-org/24" \
    DISPATCH_URL="http://dispatch.test" \
    DISPATCH_TOKEN="test-dispatch-token" \
    bash "$checkpoints_script" "$@" >"$output_file" 2>&1
}
expect_output() {
  [[ "$(<"$output_file")" == *"$1"* ]] || {
    printf 'expected output to contain: %s\n' "$1" >&2
    cat "$output_file" >&2
    exit 1
  }
}

# Default rig (`gates.design: off`): the architect registered no gate and requested no approval;
# checkpoints 3 and 4 pass on the spec, the child, and the release alone.
run_checkpoint 3
expect_output 'CHECKPOINT 3 OK: posted spec artifact, no design gate or approval request (gates.design: off), and a child issue observed'
grep -Fq 'http://dispatch.test/api/v1/issues/LEGSMOKE-1/artifacts' "$curl_log"
grep -Fq 'http://dispatch.test/api/v1/issues?project=LEGSMOKE&parent=LEGSMOKE-1' "$curl_log"
! grep -Fq '/asks?state=' "$curl_log" || {
  printf 'checkpoint 3 still reads asks; the design gate is a document approval\n' >&2
  exit 1
}
run_checkpoint 4
expect_output 'CHECKPOINT 4 OK: released child LEGSMOKE-2 has no tree or admission entry of its own; its architect runs as a worker pane of LEGSMOKE-1 and Dispatch reports it in_progress'
printf 'PASS: checkpoints 3 and 4 pass under gates.design: off with no gate and no approval request; the released child is owned by a sub-architect worker pane inside its parent tree\n'

# The pre-LEGION-57 shape: the child admitted as a queued root tree of its own.
state_file="${smoke_dir}/daemon/state.json"
jq -c '.trees["LEGSMOKE-2"] = {"root":"LEGSMOKE-2","status":"queued"} | .admission.queue = ["LEGSMOKE-2"]' \
  "$state_file" >"${state_file}.next" && mv "${state_file}.next" "$state_file"
run_checkpoint 4 && { printf 'checkpoint 4 passed with the child admitted as a tree of its own\n' >&2; exit 1; }
expect_output 'CHECKPOINT 4 FAILED: child LEGSMOKE-2 is admitted as a tree of its own (LEGION-57)'
printf 'PASS: checkpoint 4 fails with a clear message when a child is admitted as a tree of its own\n'
# Released but not yet spawned: no sub-architect claim for the child, and no phase worker on the
# root either. The failure names the child and the missing claim.
write_state '{}'
jq -c 'del(.roles["legion-exampleorg24-legsmoke-2-architect"])' \
  "$state_file" >"${state_file}.next" && mv "${state_file}.next" "$state_file"
run_checkpoint 4 && { printf 'checkpoint 4 passed with no sub-architect claim for the released child\n' >&2; exit 1; }
expect_output 'CHECKPOINT 4 FAILED: released child LEGSMOKE-2 (in_progress) holds no sub-architect role claim with a worker pane on LEGSMOKE-1 (LEGION-57'
printf 'PASS: checkpoint 4 fails with a clear message when no released child holds a sub-architect claim\n'
# The window after `release_wave` and before `spawn_worker`, with a planner running on the root: a
# phase worker on the root does not own the released child, so the single-issue fallback must not
# label this OK (tester observation on the rig).
todo_child="${temporary_dir}/children-todo.json"
printf '%s' '[{"key":"LEGSMOKE-2","parent":"LEGSMOKE-1","status":"todo"}]' >"$todo_child"
write_state '{}'
jq -c '
  del(.roles["legion-exampleorg24-legsmoke-2-architect"])
  | .issues["LEGSMOKE-2"].status = "todo"
  | .roles["legion-exampleorg24-legsmoke-1-planner"] = {"issue":"LEGSMOKE-1","role":"planner","sessionId":"ses_planner","locator":{"tmuxWindowId":"@2","tmuxPaneId":"%5"}}
' "$state_file" >"${state_file}.next" && mv "${state_file}.next" "$state_file"
CHILDREN_FILE="$todo_child" run_checkpoint 4 && { printf 'checkpoint 4 passed on the root planner while a released child had no sub-architect\n' >&2; exit 1; }
expect_output 'CHECKPOINT 4 FAILED: released child LEGSMOKE-2 (todo) holds no sub-architect role claim with a worker pane on LEGSMOKE-1 (LEGION-57'
# The same planner on the root with the child still unreleased (`triage`): a single-issue tree
# for checkpoint 4's purposes, so the fallback applies and says so.
triage_child="${temporary_dir}/children-triage-only.json"
printf '%s' '[{"key":"LEGSMOKE-2","parent":"LEGSMOKE-1","status":"triage"}]' >"$triage_child"
jq -c '.issues["LEGSMOKE-2"].status = "triage"' "$state_file" >"${state_file}.next" && mv "${state_file}.next" "$state_file"
CHILDREN_FILE="$triage_child" run_checkpoint 4
expect_output 'CHECKPOINT 4 OK: a planner phase worker claimed on the root (single-issue tree)'
printf 'PASS: checkpoint 4 refuses the single-issue fallback while a released child has no sub-architect, and applies it once no child is released\n'
write_state '{}'
# The sub-architect claim exists but Dispatch still reports the child `todo` (the `in_progress`
# PATCH in flight, or failed and parked for resync): the failure names that, not a missing claim.
jq -c '.issues["LEGSMOKE-2"].status = "todo"' "$state_file" >"${state_file}.next" && mv "${state_file}.next" "$state_file"
CHILDREN_FILE="$todo_child" run_checkpoint 4 && { printf 'checkpoint 4 passed with the child still todo on Dispatch\n' >&2; exit 1; }
expect_output 'CHECKPOINT 4 FAILED: released child LEGSMOKE-2 has a sub-architect worker pane on LEGSMOKE-1 but Dispatch still reports it todo'
printf 'PASS: checkpoint 4 names a spawned child Dispatch still reports todo, instead of calling its claim missing\n'
write_state '{}'

# Under `off`, a registered gate means the architect ignored its policy line: fail naming it.
write_state '{"LEGSMOKE-1":{"artifactId":"art-spec","latestVersion":1}}'
run_checkpoint 3 && { printf 'checkpoint 3 passed with a gate registered under gates.design: off\n' >&2; exit 1; }
expect_output 'CHECKPOINT 3 FAILED: LEGSMOKE-1 registered a design gate although the rig runs with gates.design: off'
printf 'PASS: checkpoint 3 fails under gates.design: off when a gate was registered anyway\n'

# `root-issues`: the human-approval exercise. Before the human acts the gate is registered at the
# spec's current version and the document awaits approval; afterwards the daemon records it.
printf 'root-issues\n' >"${smoke_dir}/design-gate"
awaiting_artifacts="${temporary_dir}/artifacts-awaiting.json"
printf '%s' '[{"id":"art-spec","name":"spec.md","primary":true,"versions":[{"number":1}],"approval":{"state":"awaiting","latest_version":1,"requested_by":{"kind":"session","id":"arch"},"ask_id":"ask-design"}}]' >"$awaiting_artifacts"
triage_children="${temporary_dir}/children-triage.json"
printf '%s' '[{"key":"LEGSMOKE-2","parent":"LEGSMOKE-1","status":"triage"}]' >"$triage_children"
# Before the human acts: gate registered at the spec's current version, document awaiting, and
# the architect parked — a child may exist in triage, nothing released, no worker claimed.
write_parked_state '{"LEGSMOKE-1":{"artifactId":"art-spec","latestVersion":1}}'
ARTIFACTS_FILE="$awaiting_artifacts" CHILDREN_FILE="$triage_children" run_checkpoint 3
expect_output 'CHECKPOINT 3 OK: posted spec artifact awaiting approval, registered design gate, and the architect parked on it (nothing released, no phase worker)'
# An architect that released a child while the document is still awaiting moved before approval.
ARTIFACTS_FILE="$awaiting_artifacts" run_checkpoint 3 && { printf 'checkpoint 3 passed with a child released while the spec awaits approval\n' >&2; exit 1; }
expect_output "CHECKPOINT 3 FAILED: LEGSMOKE-1's architect moved before approval: child issue LEGSMOKE-2 (in_progress) was released while the spec document is still awaiting approval"
printf 'PASS: checkpoint 3 under root-issues proves the architect parked, and fails naming a child released before approval\n'
write_parked_state '{"LEGSMOKE-1":{"artifactId":"art-spec","latestVersion":1}}'
# A Dispatch server without document approval (or a spec nobody requested approval of) reports no
# open request: the checkpoint must fail naming the missing request, never pass on a gate that can
# never be satisfied.
CHILDREN_FILE="$triage_children" run_checkpoint 3 && { printf 'checkpoint 3 passed without an open approval request on the spec document\n' >&2; exit 1; }
expect_output 'CHECKPOINT 3 FAILED: LEGSMOKE-1 has no open approval request on its registered spec document'
printf 'PASS: checkpoint 3 under root-issues requires an open approval request on the registered spec document\n'
# After the human approves, the architect releases the child (the default state): checkpoint 4
# needs the recorded approval first, then the release.
write_state '{"LEGSMOKE-1":{"artifactId":"art-spec","latestVersion":1}}'
run_checkpoint 4 && { printf 'checkpoint 4 passed before the daemon recorded the approval\n' >&2; exit 1; }
expect_output 'CHECKPOINT 4 FAILED: daemon has not recorded the spec approval for LEGSMOKE-1'
write_state '{"LEGSMOKE-1":{"artifactId":"art-spec","latestVersion":1,"approvedVersion":1}}'
run_checkpoint 4
expect_output 'CHECKPOINT 4 OK: spec approval recorded on the gate; released child LEGSMOKE-2 has no tree or admission entry of its own; its architect runs as a worker pane of LEGSMOKE-1 and Dispatch reports it in_progress'
printf 'PASS: checkpoint 4 under root-issues requires the recorded approval at the current spec version\n'
printf 'off\n' >"${smoke_dir}/design-gate"

# Single-issue tree (the legion-architect skill allows one): no child issue exists, but the
# architect spawned a phase worker on the root. Checkpoints 3 and 4 pass on that claim and say so.
no_children="${temporary_dir}/children-none.json"
printf '%s' '[]' >"$no_children"
write_single_issue_state '{}'
CHILDREN_FILE="$no_children" run_checkpoint 3
expect_output 'CHECKPOINT 3 OK: posted spec artifact, no design gate or approval request (gates.design: off), and a planner phase worker claimed on the root (single-issue tree) observed'
CHILDREN_FILE="$no_children" run_checkpoint 4
expect_output 'CHECKPOINT 4 OK: a planner phase worker claimed on the root (single-issue tree)'
printf 'root-issues\n' >"${smoke_dir}/design-gate"
# Armed, single-issue, before approval: the architect registered the gate and parked — no child,
# no worker. This is exactly what the real architect produced in every rig run.
write_single_issue_state '{"LEGSMOKE-1":{"artifactId":"art-spec","latestVersion":1}}' architect
ARTIFACTS_FILE="$awaiting_artifacts" CHILDREN_FILE="$no_children" run_checkpoint 3
expect_output 'CHECKPOINT 3 OK: posted spec artifact awaiting approval, registered design gate, and the architect parked on it (nothing released, no phase worker)'
# A worker claimed on the root while the document still awaits approval is an early move.
write_single_issue_state '{"LEGSMOKE-1":{"artifactId":"art-spec","latestVersion":1}}'
ARTIFACTS_FILE="$awaiting_artifacts" CHILDREN_FILE="$no_children" run_checkpoint 3 && { printf 'checkpoint 3 passed with a phase worker claimed while the spec awaits approval\n' >&2; exit 1; }
expect_output "CHECKPOINT 3 FAILED: LEGSMOKE-1's architect moved before approval: a planner on LEGSMOKE-1 phase worker is claimed while the spec document is still awaiting approval"
# After approval the architect spawns the worker: checkpoint 4 passes on the claim.
write_single_issue_state '{"LEGSMOKE-1":{"artifactId":"art-spec","latestVersion":1,"approvedVersion":1}}'
CHILDREN_FILE="$no_children" run_checkpoint 4
expect_output 'CHECKPOINT 4 OK: spec approval recorded on the gate; a planner phase worker claimed on the root (single-issue tree)'
printf 'PASS: checkpoints 3 and 4 accept a single-issue tree: parked before approval, a phase worker on the root after; a worker claimed while awaiting fails naming the early move\n'
# Neither a child nor a phase worker: the tree has not moved past the gate, and the failure says so.
printf 'off\n' >"${smoke_dir}/design-gate"
write_single_issue_state '{}' architect
CHILDREN_FILE="$no_children" run_checkpoint 3 && { printf 'checkpoint 3 passed with neither a child issue nor a phase worker\n' >&2; exit 1; }
expect_output 'CHECKPOINT 3 FAILED: LEGSMOKE-1 has neither a Dispatch child issue nor a phase-worker role claim on the root'
CHILDREN_FILE="$no_children" run_checkpoint 4 && { printf 'checkpoint 4 passed with neither a released child nor a phase worker\n' >&2; exit 1; }
expect_output 'CHECKPOINT 4 FAILED: neither a released child holds a sub-architect role claim with a worker pane nor a phase worker is claimed on LEGSMOKE-1'
printf 'PASS: checkpoints 3 and 4 fail naming the root when neither a child issue nor a phase worker exists\n'
write_state '{}'

# none mode: no Dispatch issue event reaches the rig NATS, so 1-4 and 12 are blocked with the
# Dispatch-ingress reason before any Dispatch request is made (12 before it even asks for
# SMOKE_QUEUED_ISSUE); 5-7 and 9-11 keep the GitHub-ingress reason below; 13 is not gated by mode.
printf 'none\n' >"${smoke_dir}/webhook-mode"
curl_calls_before="$(wc -l <"$curl_log")"
for blocked_checkpoint in 1 2 3 4 12; do
  if PATH="${fake_bin}:${PATH}" \
    SMOKE_DIR="$smoke_dir" \
    SMOKE_REPO="example-org/legion-smoke" \
    SMOKE_PROJECT="example-org/24" \
    DISPATCH_URL="http://dispatch.test" \
    DISPATCH_TOKEN="test-dispatch-token" \
    bash "$checkpoints_script" "$blocked_checkpoint" >"$output_file" 2>&1; then
    printf 'expected none mode to block checkpoint %s\n' "$blocked_checkpoint" >&2
    exit 1
  else
    status=$?
  fi
  [[ "$status" == 3 && "$(<"$output_file")" == *"CHECKPOINT ${blocked_checkpoint} SKIPPED-BLOCKED: SMOKE_WEBHOOK_MODE=none with SMOKE_DISPATCH_INGRESS=shared: "*'requires Dispatch issue-event ingress'*'use SMOKE_WEBHOOK_MODE=envoy, or SMOKE_DISPATCH_INGRESS=rig'* ]] || {
    printf 'expected exit 3 and the Dispatch-ingress reason for checkpoint %s; got %s:\n%s\n' "$blocked_checkpoint" "$status" "$(<"$output_file")" >&2
    exit 1
  }
done
[[ "$(wc -l <"$curl_log")" == "$curl_calls_before" ]] || {
  printf 'expected a mode-blocked checkpoint to make no Dispatch request\n' >&2
  exit 1
}
if ! PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" DISPATCH_URL="http://dispatch.test" FAKE_TMUX_PID="$clean_pid" \
  env -u DISPATCH_TOKEN bash "$checkpoints_script" 13 >"$output_file" 2>&1; then
  cat "$output_file" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'CHECKPOINT 13 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: none mode blocks checkpoints 1-4 and 12 with the Dispatch-ingress reason and leaves 13 ungated\n'

# The same none mode with the recorded Dispatch ingress `rig` (a scratch Dispatch publishes into
# the rig NATS itself): the webhook mode no longer decides checkpoints 1-4 and 12. Checkpoint 1
# passes the gate, says which record let it through, and reaches its own assertion — here it
# passes on the same fixtures the envoy-mode case above uses. An explicitly recorded `shared` is
# today's behaviour, blocked exactly as before; anything else on record is refused naming the two
# values. The webhook-only checkpoints (5-7, 9-11) stay blocked under none whatever the ingress.
printf 'rig\n' >"${smoke_dir}/dispatch-ingress"
if ! PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  FAKE_TMUX_PID="$clean_pid" \
  bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected rig ingress under none to let checkpoint 1 run its own assertion; got:\n%s\n' "$(<"$output_file")" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'CHECKPOINT 1: SMOKE_WEBHOOK_MODE=none does not block this checkpoint; the recorded SMOKE_DISPATCH_INGRESS=rig says a scratch Dispatch publishes issue events into the rig NATS directly'*'CHECKPOINT 1 OK'* && "$(<"$output_file")" != *'SKIPPED-BLOCKED'* ]] || {
  printf 'expected the rig-ingress notice followed by checkpoint 1 OK; got:\n%s\n' "$(<"$output_file")" >&2
  exit 1
}
if PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" DISPATCH_URL="http://dispatch.test" DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 5 >"$output_file" 2>&1; then
  printf 'expected none mode to keep blocking the GitHub-fed checkpoint 5 under rig ingress\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 3 && "$(<"$output_file")" == *'CHECKPOINT 5 SKIPPED-BLOCKED: '*'requires live GitHub webhook ingress'* ]] || {
  printf 'expected checkpoint 5 to stay blocked with the GitHub-ingress reason under rig ingress; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  exit 1
}
printf 'shared\n' >"${smoke_dir}/dispatch-ingress"
if PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" DISPATCH_URL="http://dispatch.test" DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected shared ingress under none to block checkpoint 1\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 3 && "$(<"$output_file")" == *'CHECKPOINT 1 SKIPPED-BLOCKED: SMOKE_WEBHOOK_MODE=none with SMOKE_DISPATCH_INGRESS=shared: '* ]] || {
  printf 'expected the shared-ingress block for checkpoint 1; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  exit 1
}
printf 'sideways\n' >"${smoke_dir}/dispatch-ingress"
if PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" DISPATCH_URL="http://dispatch.test" DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected an unknown recorded Dispatch ingress to be refused\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 1 && "$(<"$output_file")" == *'CHECKPOINT 1 FAILED: recorded SMOKE_DISPATCH_INGRESS must be shared or rig'* ]] || {
  printf 'expected the unknown-ingress refusal naming the two values; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  exit 1
}
rm -f "${smoke_dir}/dispatch-ingress"
printf 'PASS: a recorded rig Dispatch ingress lets none mode reach checkpoint 1 (naming the record), shared stays blocked, an unknown record is refused\n'

# Every GitHub-fed checkpoint under none: exit 3, the GitHub-ingress reason, and no Dispatch
# request. The mode gate runs before each checkpoint's own require_env, so none of the SMOKE_*
# inputs those checkpoints normally need is required here.
curl_calls_before="$(wc -l <"$curl_log")"
for blocked_checkpoint in 5 6 7 9 10 11; do
  if PATH="${fake_bin}:${PATH}" \
    SMOKE_DIR="$smoke_dir" \
    SMOKE_REPO="example-org/legion-smoke" \
    SMOKE_PROJECT="example-org/24" \
    bash "$checkpoints_script" "$blocked_checkpoint" >"$output_file" 2>&1; then
    printf 'expected none mode to block checkpoint %s\n' "$blocked_checkpoint" >&2
    exit 1
  else
    status=$?
  fi
  [[ "$status" == 3 && "$(<"$output_file")" == *"CHECKPOINT ${blocked_checkpoint} SKIPPED-BLOCKED: "*'requires live GitHub webhook ingress'* ]] || {
    printf 'expected exit 3 and the GitHub-ingress reason for checkpoint %s; got %s:\n%s\n' "$blocked_checkpoint" "$status" "$(<"$output_file")" >&2
    exit 1
  }
done
[[ "$(wc -l <"$curl_log")" == "$curl_calls_before" ]] || {
  printf 'expected a GitHub-blocked checkpoint to make no Dispatch request\n' >&2
  exit 1
}
# Checkpoint 8 is not gated by the mode: under none it reaches its own branch-protection gate
# and reports that reason, never either ingress reason.
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  env -u SMOKE_BRANCH_PROTECTION bash "$checkpoints_script" 8 >"$output_file" 2>&1; then
  printf 'expected checkpoint 8 to be blocked by its branch-protection gate\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 3 && "$(<"$output_file")" == *'CHECKPOINT 8 SKIPPED-BLOCKED: SMOKE_BRANCH_PROTECTION=1 requires branch protection/ruleset availability'* && "$(<"$output_file")" != *'ingress'* ]] || {
  printf 'expected checkpoint 8 under none to report only its branch-protection reason; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  exit 1
}

printf 'envoy\n' >"${smoke_dir}/webhook-mode"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  bash "$checkpoints_script" 5 >"$output_file" 2>&1; then
  printf 'expected checkpoint 5 to require a PR fixture\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 1 && "$(<"$output_file")" == *'set SMOKE_PR or open a legion/issue-* pull request'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: none mode blocks GitHub-fed checkpoints 5-7 and 9-11 with the GitHub-ingress reason and leaves checkpoint 8 to its branch-protection gate; envoy lets checkpoint 5 reach its own PR check\n'

# forward mode: `gh webhook forward` relays GitHub events only, so 1-4 and 12 are blocked with the
# same Dispatch-ingress reason (naming forward as the recorded mode), while 5 passes the mode gate
# and reaches its own PR-fixture check exactly as under envoy.
printf 'forward\n' >"${smoke_dir}/webhook-mode"
curl_calls_before="$(wc -l <"$curl_log")"
for blocked_checkpoint in 1 2 3 4 12; do
  if PATH="${fake_bin}:${PATH}" \
    SMOKE_DIR="$smoke_dir" \
    SMOKE_REPO="example-org/legion-smoke" \
    SMOKE_PROJECT="example-org/24" \
    DISPATCH_URL="http://dispatch.test" \
    DISPATCH_TOKEN="test-dispatch-token" \
    bash "$checkpoints_script" "$blocked_checkpoint" >"$output_file" 2>&1; then
    printf 'expected forward mode to block checkpoint %s\n' "$blocked_checkpoint" >&2
    exit 1
  else
    status=$?
  fi
  [[ "$status" == 3 && "$(<"$output_file")" == *"CHECKPOINT ${blocked_checkpoint} SKIPPED-BLOCKED: SMOKE_WEBHOOK_MODE=forward with SMOKE_DISPATCH_INGRESS=shared: "*'requires Dispatch issue-event ingress'*'use SMOKE_WEBHOOK_MODE=envoy, or SMOKE_DISPATCH_INGRESS=rig'* ]] || {
    printf 'expected exit 3 and the Dispatch-ingress reason naming forward for checkpoint %s; got %s:\n%s\n' "$blocked_checkpoint" "$status" "$(<"$output_file")" >&2
    exit 1
  }
done
[[ "$(wc -l <"$curl_log")" == "$curl_calls_before" ]] || {
  printf 'expected a forward-blocked checkpoint to make no Dispatch request\n' >&2
  exit 1
}
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  bash "$checkpoints_script" 5 >"$output_file" 2>&1; then
  printf 'expected checkpoint 5 under forward to require a PR fixture\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 1 && "$(<"$output_file")" == *'set SMOKE_PR or open a legion/issue-* pull request'* ]] || {
  printf 'expected forward mode to let checkpoint 5 past the mode gate to its PR-fixture check; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  exit 1
}
printf 'envoy\n' >"${smoke_dir}/webhook-mode"
printf 'PASS: forward mode blocks checkpoints 1-4 and 12 with the Dispatch-ingress reason and lets checkpoint 5 reach its own PR check\n'

# A SMOKE_DIR up.sh never populated, under the trapped temporary directory so every exit path
# below leaves nothing behind.
bare_smoke_dir="${temporary_dir}/bare-smoke"
mkdir -p "$bare_smoke_dir"
# No recorded mode and no SMOKE_WEBHOOK_MODE: the script stops naming the missing file and the
# remedy, before any gating decision or Dispatch request -- it never guesses a mode.
curl_calls_before="$(wc -l <"$curl_log")"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$bare_smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  env -u SMOKE_WEBHOOK_MODE bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected checkpoint 1 to stop when no webhook mode was recorded or exported\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 1 && "$(<"$output_file")" == *"CHECKPOINT 1 FAILED: no recorded webhook mode at ${bare_smoke_dir}/webhook-mode; run up.sh, or export SMOKE_WEBHOOK_MODE=envoy|forward|none"* && "$(<"$output_file")" != *'SKIPPED-BLOCKED'* ]] || {
  printf 'expected the no-recorded-mode message and exit 1; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  exit 1
}
[[ "$(wc -l <"$curl_log")" == "$curl_calls_before" ]] || {
  printf 'expected the no-recorded-mode stop to make no Dispatch request\n' >&2
  exit 1
}
# An explicit export stands in for the missing record: none gates checkpoint 1 exactly as a
# recorded none would.
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$bare_smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  SMOKE_WEBHOOK_MODE=none bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected an exported none mode to block checkpoint 1\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 3 && "$(<"$output_file")" == *'CHECKPOINT 1 SKIPPED-BLOCKED: SMOKE_WEBHOOK_MODE=none with SMOKE_DISPATCH_INGRESS=shared: '*'requires Dispatch issue-event ingress'* ]] || {
  printf 'expected the exported none mode to gate checkpoint 1; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  exit 1
}
printf 'PASS: stops naming the missing webhook-mode record and the remedy instead of guessing a mode; an exported SMOKE_WEBHOOK_MODE stands in for it\n'

# Both a record and an export: the file up.sh recorded decides, in both directions -- a recorded
# none blocks checkpoint 1 under an exported envoy, and a recorded envoy lets it past the mode
# gate to its own root-issue check under an exported none.
printf 'none\n' >"${bare_smoke_dir}/webhook-mode"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$bare_smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  SMOKE_WEBHOOK_MODE=envoy bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected a recorded none mode to block checkpoint 1 despite an exported envoy\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 3 && "$(<"$output_file")" == *'CHECKPOINT 1 SKIPPED-BLOCKED: SMOKE_WEBHOOK_MODE=none with SMOKE_DISPATCH_INGRESS=shared: '* ]] || {
  printf 'expected the recorded none mode to outrank the exported envoy; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  exit 1
}
printf 'envoy\n' >"${bare_smoke_dir}/webhook-mode"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$bare_smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  SMOKE_WEBHOOK_MODE=none bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected checkpoint 1 to reach its root-issue check under a recorded envoy mode and fail there\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 1 && "$(<"$output_file")" == *'SMOKE_ROOT_ISSUE is unset'* && "$(<"$output_file")" != *'SKIPPED-BLOCKED'* ]] || {
  printf 'expected the recorded envoy mode to outrank the exported none and reach the root-issue check; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  exit 1
}
printf 'PASS: the recorded webhook-mode file outranks an exported SMOKE_WEBHOOK_MODE in both directions\n'

printf 'envoy\n' >"${bare_smoke_dir}/webhook-mode"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$bare_smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected checkpoint 1 to fail without SMOKE_ROOT_ISSUE or a recorded root-issue file\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'SMOKE_ROOT_ISSUE is unset'* && "$(<"$output_file")" == *'root-issue'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: fails with a clear message when neither SMOKE_ROOT_ISSUE nor a recorded root-issue file is present\n'

# LEGION-40: checkpoints.sh resolves DISPATCH_URL/DISPATCH_TOKEN exactly as up.sh does (shared
# dispatch-config.sh). XDG_CONFIG_HOME points at harness-owned directories so both cases are
# independent of the box's own ~/.config/opencode/envoy.json (present here, absent in CI).
xdg_file_dir="${temporary_dir}/xdg-with-file"
xdg_empty_dir="${temporary_dir}/xdg-empty"
mkdir -p "${xdg_file_dir}/opencode" "$xdg_empty_dir"
printf '{"dispatch":{"enabled":true,"serverUrl":"http://dispatch.test","token":"file-token"}}\n' >"${xdg_file_dir}/opencode/envoy.json"
# Both from the file: the token reaches the Authorization header of a real Dispatch request.
if ! PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  XDG_CONFIG_HOME="$xdg_file_dir" \
  env -u DISPATCH_URL -u DISPATCH_TOKEN bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  cat "$output_file" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'CHECKPOINT 1 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
grep -Fq 'Authorization: Bearer file-token' "$curl_log" || {
  printf 'expected the envoy.json token to reach the Dispatch request\n' >&2
  exit 1
}
printf 'PASS: checkpoints read DISPATCH_URL and DISPATCH_TOKEN from envoy.json when the variables are unset\n'

# Neither source: the checkpoint stops naming the variable, the file, and the key -- not a `secrets`
# command (DISPATCH_TOKEN is not a secretsd key on this machine) -- and makes no Dispatch request.
curl_calls_before="$(wc -l <"$curl_log")"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  XDG_CONFIG_HOME="$xdg_empty_dir" \
  env -u DISPATCH_TOKEN bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected checkpoint 1 to fail without DISPATCH_TOKEN and without envoy.json\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"CHECKPOINT 1 FAILED: DISPATCH_TOKEN is unset and ${xdg_empty_dir}/opencode/envoy.json does not exist; export DISPATCH_TOKEN or set .dispatch.token in that file"* ]] || {
  cat "$output_file" >&2
  exit 1
}
[[ "$(wc -l <"$curl_log")" == "$curl_calls_before" ]] || {
  printf 'expected no Dispatch request when the token cannot be resolved\n' >&2
  exit 1
}
printf 'PASS: missing-token error names the variable, the envoy.json path, and the key\n'

checkpoint_nine_root_file="${temporary_dir}/checkpoint-nine-root.json"
checkpoint_nine_children_file="${temporary_dir}/checkpoint-nine-children.json"
export CHECKPOINT_NINE_ROOT_FILE="$checkpoint_nine_root_file"
export CHECKPOINT_NINE_CHILDREN_FILE="$checkpoint_nine_children_file"

# Overwrites the shared fake curl (every earlier checkpoint invocation above has already run and
# asserted) so checkpoint 9's root/children Dispatch-status fixtures are independently settable.
cat >"${fake_bin}/curl" <<'EOF'
#!/usr/bin/env bash
request="$*"
printf '%s\n' "$request" >>"$CURL_LOG"
case "$request" in
  *"/api/v1/issues?project=LEGSMOKE&parent=LEGSMOKE-1"*)
    printf '%s' "$(<"$CHECKPOINT_NINE_CHILDREN_FILE")"
    ;;
  *"/api/v1/issues/LEGSMOKE-1"*)
    printf '%s' "$(<"$CHECKPOINT_NINE_ROOT_FILE")"
    ;;
  *)
    printf 'unexpected curl request: %s\n' "$request" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${fake_bin}/curl"
printf 'envoy\n' >"${smoke_dir}/webhook-mode"

printf '{"key":"LEGSMOKE-1","status":"done"}' >"$checkpoint_nine_root_file"
printf '[{"key":"LEGSMOKE-2","parent":"LEGSMOKE-1","status":"done"}]' >"$checkpoint_nine_children_file"
PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 9 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 9 OK: root issue and all its children are done'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 9 passes when the root issue and every child are done\n'

printf '[{"key":"LEGSMOKE-2","parent":"LEGSMOKE-1","status":"done"},{"key":"LEGSMOKE-3","parent":"LEGSMOKE-1","status":"in_progress"}]' >"$checkpoint_nine_children_file"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 9 >"$output_file" 2>&1; then
  printf 'expected checkpoint 9 to fail when a child issue is not done\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'CHECKPOINT 9 FAILED: LEGSMOKE-1 has a child issue that is not done'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 9 fails with a clear message when a child issue is not done\n'

printf '{"key":"LEGSMOKE-1","status":"in_progress"}' >"$checkpoint_nine_root_file"
printf '[{"key":"LEGSMOKE-2","parent":"LEGSMOKE-1","status":"done"}]' >"$checkpoint_nine_children_file"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 9 >"$output_file" 2>&1; then
  printf 'expected checkpoint 9 to fail when the root issue is not done\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'CHECKPOINT 9 FAILED: Dispatch issue LEGSMOKE-1 is not done'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 9 fails with a clear message when the root issue is not done\n'

rm -f "${smoke_dir}/daemon/state.json"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected checkpoint 1 to fail when the daemon state file is missing\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"CHECKPOINT 1 FAILED: daemon state file is missing: ${smoke_dir}/daemon/state.json"* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 1 fails with a clear message naming the path when the daemon state file is missing\n'

# Every tmux call any checkpoint above made must have targeted the daemon's private socket,
# `legion-<slug>` (`project_slug` of `example-org/24` is `exampleorg24`) — the default server never
# hosts a Legion pane. The one call allowed off the private socket is checkpoint 13's own
# default-server `has-session` probe, which exists precisely to prove no such session is there.
[[ -s "$TMUX_LOG" ]] || {
  printf 'no tmux invocation was logged; the fake tmux is not being exercised\n' >&2
  exit 1
}
off_socket="$(grep -v '^legion-exampleorg24 ' "$TMUX_LOG" | grep -v '^ has-session -t legion-exampleorg24$' || true)"
[[ -z "$off_socket" ]] || {
  printf 'tmux invocations off the private legion-exampleorg24 socket:\n%s\n' "$off_socket" >&2
  exit 1
}
grep -q '^ has-session -t legion-exampleorg24$' "$TMUX_LOG" || {
  printf 'checkpoint 13 never probed the default server for a legion-exampleorg24 session\n' >&2
  exit 1
}
printf 'PASS: every tmux invocation across checkpoints 1-13 targets the private legion-<slug> socket (default-server probe excepted)\n'
