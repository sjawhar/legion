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
printf '%s' '{"issues":{"LEGSMOKE-1":{"status":"in_progress","children":["LEGSMOKE-2"]},"LEGSMOKE-2":{"parent":"LEGSMOKE-1","status":"todo","children":[]},"LEGSMOKE-99":{"status":"in_progress","children":[]}},"trees":{"LEGSMOKE-1":{"root":"LEGSMOKE-1","status":"active","locator":{"tmuxWindowId":"@2","tmuxPaneId":"%2"}},"LEGSMOKE-2":{"root":"LEGSMOKE-2","status":"active","locator":{"tmuxWindowId":"@3","tmuxPaneId":"%3"}}},"controllerLocator":{"tmuxWindowId":"@1","tmuxPaneId":"%1"},"roles":{"legion-exampleorg24-legsmoke-2-tester":{"issue":"LEGSMOKE-2","role":"tester","locator":{"tmuxWindowId":"@3","tmuxPaneId":"%4"}}},"admission":{"active":["LEGSMOKE-1","LEGSMOKE-2"]},"gates":{"LEGSMOKE-1":{"designAskId":"ask-design","designApproved":"gate-off"}}}' >"${smoke_dir}/daemon/state.json"
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
  *"/api/v1/issues/LEGSMOKE-1/asks?state=all"*)
    printf '%s' '[{"id":"ask-design","state":"resolved","options":[{"label":"Approve"}]}]'
    ;;
  *"/api/v1/issues/LEGSMOKE-1/artifacts"*)
    printf '%s' '[{"name":"spec.md","primary":true,"versions":[{"number":1}]}]'
    ;;
  *"/api/v1/issues?project=LEGSMOKE&parent=LEGSMOKE-1"*)
    printf '%s' '[{"key":"LEGSMOKE-2","parent":"LEGSMOKE-1","status":"todo"}]'
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
# server. `has-session` on the default server (no socket) reports no session.
cat >"${fake_bin}/tmux" <<'EOF'
#!/usr/bin/env bash
socket=""
if [[ "${1:-}" == "-L" ]]; then socket="$2"; shift 2; fi
printf '%s\n' "$socket $*" >>"${TMUX_LOG:-/dev/null}"
case "${1:-}" in
  has-session) [[ -n "$socket" ]] ;;
  display-message) printf '%s\n' "$FAKE_TMUX_PID" ;;
  show-environment) printf 'PATH=/usr/bin\n' ;;
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
# must be real, long-lived processes the harness owns: one with a clean environment and one with
# a planted `DISPATCH_TOKEN` (a `$PPID` trick would name a command-substitution subshell that has
# already exited by the time /proc is read). Both scrub every variable the checkpoint inspects
# from whatever shell runs this harness (a Legion worker's own pane carries a boot token).
env -u DISPATCH_TOKEN -u LEGION_BOOT_TOKEN -u LEGION_CONTROLLER_SECRET sleep 300 &
clean_pid=$!
env -u LEGION_BOOT_TOKEN -u LEGION_CONTROLLER_SECRET DISPATCH_TOKEN="leaked-into-a-pane" sleep 300 &
planted_pid=$!
trap 'kill "$clean_pid" "$planted_pid" 2>/dev/null; rm -rf "$temporary_dir"' EXIT

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
printf 'PASS: checkpoint 13 passes when no recorded process carries a secret\n'

if PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" DISPATCH_URL="http://dispatch.test" FAKE_TMUX_PID="$planted_pid" \
  env -u DISPATCH_TOKEN bash "$checkpoints_script" 13 >"$output_file" 2>&1; then
  printf 'expected checkpoint 13 to fail when a recorded process environment carries DISPATCH_TOKEN\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"pid ${planted_pid} environ carries DISPATCH_TOKEN="* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: checkpoint 13 fails naming the pid and variable when a recorded process environment carries it\n'

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

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 3 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 3 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
grep -Fq 'http://dispatch.test/api/v1/issues/LEGSMOKE-1/asks?state=all' "$curl_log"
grep -Fq 'http://dispatch.test/api/v1/issues/LEGSMOKE-1/artifacts' "$curl_log"
grep -Fq 'http://dispatch.test/api/v1/issues?project=LEGSMOKE&parent=LEGSMOKE-1' "$curl_log"

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 4 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 4 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}

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
  [[ "$status" == 3 && "$(<"$output_file")" == *"CHECKPOINT ${blocked_checkpoint} SKIPPED-BLOCKED: SMOKE_WEBHOOK_MODE=none: "*'requires Dispatch issue-event ingress'*'use SMOKE_WEBHOOK_MODE=envoy'* ]] || {
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
  [[ "$status" == 3 && "$(<"$output_file")" == *"CHECKPOINT ${blocked_checkpoint} SKIPPED-BLOCKED: SMOKE_WEBHOOK_MODE=forward: "*'requires Dispatch issue-event ingress'*'use SMOKE_WEBHOOK_MODE=envoy'* ]] || {
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

bare_temporary_dir="$(mktemp -d)"
bare_smoke_dir="${bare_temporary_dir}/smoke"
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
  rm -rf "$bare_temporary_dir"
  exit 1
else
  status=$?
fi
[[ "$status" == 1 && "$(<"$output_file")" == *"CHECKPOINT 1 FAILED: no recorded webhook mode at ${bare_smoke_dir}/webhook-mode; run up.sh, or export SMOKE_WEBHOOK_MODE=envoy|forward|none"* && "$(<"$output_file")" != *'SKIPPED-BLOCKED'* ]] || {
  printf 'expected the no-recorded-mode message and exit 1; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  rm -rf "$bare_temporary_dir"
  exit 1
}
[[ "$(wc -l <"$curl_log")" == "$curl_calls_before" ]] || {
  printf 'expected the no-recorded-mode stop to make no Dispatch request\n' >&2
  rm -rf "$bare_temporary_dir"
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
  rm -rf "$bare_temporary_dir"
  exit 1
else
  status=$?
fi
[[ "$status" == 3 && "$(<"$output_file")" == *'CHECKPOINT 1 SKIPPED-BLOCKED: SMOKE_WEBHOOK_MODE=none: '*'requires Dispatch issue-event ingress'* ]] || {
  printf 'expected the exported none mode to gate checkpoint 1; got %s:\n%s\n' "$status" "$(<"$output_file")" >&2
  rm -rf "$bare_temporary_dir"
  exit 1
}
printf 'PASS: stops naming the missing webhook-mode record and the remedy instead of guessing a mode; an exported SMOKE_WEBHOOK_MODE stands in for it\n'

printf 'envoy\n' >"${bare_smoke_dir}/webhook-mode"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$bare_smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected checkpoint 1 to fail without SMOKE_ROOT_ISSUE or a recorded root-issue file\n' >&2
  rm -rf "$bare_temporary_dir"
  exit 1
fi
[[ "$(<"$output_file")" == *'SMOKE_ROOT_ISSUE is unset'* && "$(<"$output_file")" == *'root-issue'* ]] || {
  cat "$output_file" >&2
  rm -rf "$bare_temporary_dir"
  exit 1
}
rm -rf "$bare_temporary_dir"
printf 'PASS: fails with a clear message when neither SMOKE_ROOT_ISSUE nor a recorded root-issue file is present\n'

if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  env -u DISPATCH_TOKEN bash "$checkpoints_script" 1 >"$output_file" 2>&1; then
  printf 'expected checkpoint 1 to fail without DISPATCH_TOKEN\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *'secrets DISPATCH_TOKEN -- bash scripts/smoke/checkpoints.sh'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: missing-token error names the exact secrets-wrapped invocation\n'

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
