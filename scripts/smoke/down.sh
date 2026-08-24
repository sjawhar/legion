#!/usr/bin/env bash
set -euo pipefail

readonly smoke_dir="${SMOKE_DIR:-/tmp/legion-smoke}"
readonly nats_name="legion-smoke-nats"

warn() {
  printf 'warning: %s\n' "$*" >&2
}

project_slug() {
  local project="${SMOKE_PROJECT:-}"
  project="${project,,}"
  project="${project//[^a-z0-9]/}"
  printf '%s\n' "$project"
}

process_start_time() {
  local pid="$1"
  [[ "$pid" =~ ^[0-9]+$ && -r "/proc/${pid}/stat" ]] || return 1
  awk '{print $22}' "/proc/${pid}/stat"
}

terminate_pid_file() {
  local name="$1"
  local pid_file="${smoke_dir}/${name}.pid"
  local start_file="${smoke_dir}/${name}.start"
  local pid
  local expected_start
  local actual_start
  local attempt

  [[ -r "$pid_file" && -r "$start_file" ]] || {
    rm -f "$pid_file" "$start_file"
    return 0
  }
  pid="$(<"$pid_file")"
  expected_start="$(<"$start_file")"
  actual_start="$(process_start_time "$pid" 2>/dev/null || true)"
  if [[ -z "$actual_start" || "$actual_start" != "$expected_start" ]]; then
    rm -f "$pid_file" "$start_file"
    return 0
  fi
  rm -f "$pid_file" "$start_file"

  kill "$pid"
  for ((attempt = 1; attempt <= 10; attempt += 1)); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 1
  done
  kill -KILL "$pid" 2>/dev/null || true
  printf 'STOPPED %s (pid %s)\n' "$name" "$pid"
}

close_protection_probe() {
  local pr_file="${smoke_dir}/protection-probe-pr"
  local pr

  [[ -n "${SMOKE_REPO:-}" && -r "$pr_file" ]] || return 0
  command -v gh >/dev/null 2>&1 || {
    warn "gh is unavailable; left the disposable branch-protection probe open"
    return 0
  }
  pr="$(<"$pr_file")"
  [[ "$pr" =~ ^[0-9]+$ ]] || return 0
  gh pr close "$pr" -R "$SMOKE_REPO" --delete-branch >/dev/null 2>&1 ||
    warn "could not close disposable branch-protection probe #${pr}"
}
remove_webhook_forwarder() {
  local name="$1"
  local hook_file="${smoke_dir}/${name}.hook"
  local hook_endpoint

  [[ -n "${SMOKE_REPO:-}" && -r "$hook_file" ]] || return 0
  hook_endpoint="$(<"$hook_file")"
  rm -f "$hook_file"
  [[ "$hook_endpoint" =~ ^(repos|orgs)/[^/]+(/[^/]+)?/hooks/[0-9]+$ ]] || return 0
  gh api -X DELETE "$hook_endpoint" >/dev/null 2>&1 ||
    warn "could not remove managed ${name} hook"
}


stop_tmux_session() {
  local slug
  [[ -n "${SMOKE_PROJECT:-}" ]] || return 0
  slug="$(project_slug)"
  [[ -n "$slug" ]] || return 0
  tmux has-session -t "legion-${slug}" 2>/dev/null || return 0
  tmux kill-session -t "legion-${slug}"
  printf 'STOPPED tmux session legion-%s\n' "$slug"
}

main() {
  terminate_pid_file webhook-forward
  terminate_pid_file board-webhook-forward
  remove_webhook_forwarder webhook-forward
  remove_webhook_forwarder board-webhook-forward
  terminate_pid_file daemon
  stop_tmux_session
  terminate_pid_file dispatch
  terminate_pid_file listener

  if docker container inspect "$nats_name" >/dev/null 2>&1; then
    docker rm -f "$nats_name" >/dev/null
    printf 'STOPPED NATS container %s\n' "$nats_name"
  fi

  close_protection_probe
  printf 'RIG DOWN\n'
}

main "$@"
