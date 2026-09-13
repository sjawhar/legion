#!/usr/bin/env bash
set -euo pipefail

readonly smoke_dir="${SMOKE_DIR:-/tmp/legion-smoke}"
# up.sh writes this for every rig it starts (write_daemon_config); its `project:` line is the one
# source of the rig's identity for teardown. A directory without it never started a rig, and
# down.sh stops no tmux server and removes no container for it -- the environment's SMOKE_PROJECT
# can name another tester's live rig, and once did.
readonly rig_config="${smoke_dir}/legion.yaml"
# up.sh records the rig's derived NATS container name (legion-smoke-nats-<slug>) here; rigs
# started before that record existed used one fixed name for every rig.
readonly nats_record="${smoke_dir}/nats-container"
readonly legacy_nats_name="legion-smoke-nats"

warn() {
  printf 'warning: %s\n' "$*" >&2
}

project_slug() {
  local project="$1"
  project="${project,,}"
  project="${project//[^a-z0-9]/}"
  printf '%s\n' "$project"
}

# Prints the project named by the directory's legion.yaml; fails when the file has no such line.
rig_project() {
  local line
  while IFS= read -r line; do
    if [[ "$line" =~ ^project:[[:space:]]*([^[:space:]]+)[[:space:]]*$ ]]; then
      printf '%s\n' "${BASH_REMATCH[1]}"
      return 0
    fi
  done <"$rig_config"
  return 1
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
terminate_process_group_file() {
  local name="$1"
  local pid_file="${smoke_dir}/${name}.pid"
  local start_file="${smoke_dir}/${name}.start"
  local pgid
  local expected_start
  local actual_start
  local attempt

  [[ -r "$pid_file" && -r "$start_file" ]] || {
    rm -f "$pid_file" "$start_file"
    return 0
  }
  pgid="$(<"$pid_file")"
  expected_start="$(<"$start_file")"
  actual_start="$(process_start_time "$pgid" 2>/dev/null || true)"
  if [[ -z "$actual_start" || "$actual_start" != "$expected_start" ]]; then
    rm -f "$pid_file" "$start_file"
    return 0
  fi
  rm -f "$pid_file" "$start_file"

  kill -- "-${pgid}"
  for ((attempt = 1; attempt <= 10; attempt += 1)); do
    kill -0 -- "-${pgid}" 2>/dev/null || return 0
    sleep 1
  done
  kill -KILL -- "-${pgid}" 2>/dev/null || true
  printf 'STOPPED %s (pgid %s)\n' "$name" "$pgid"
}


remove_webhook_forwarder() {
  local name="$1"
  local hook_file="${smoke_dir}/${name}.hook"
  local hook_endpoint
  local delete_response


  [[ -n "${SMOKE_REPO:-}" && -r "$hook_file" ]] || return 0
  hook_endpoint="$(<"$hook_file")"
  rm -f "$hook_file"
  [[ "$hook_endpoint" =~ ^(repos|orgs)/[^/]+(/[^/]+)?/hooks/[0-9]+$ ]] || return 0
  delete_response="$(gh api -X DELETE "$hook_endpoint" 2>&1)" ||
    [[ "$delete_response" == *'"status":"404"'* ]] ||
    warn "could not remove managed ${name} hook"
}


stop_tmux_session() {
  local project
  local slug
  local session
  local owner
  project="$(rig_project)" || {
    warn "${rig_config} has no project line; stopping no tmux server"
    return 0
  }
  if [[ -n "${SMOKE_PROJECT:-}" && "$SMOKE_PROJECT" != "$project" ]]; then
    warn "SMOKE_PROJECT=${SMOKE_PROJECT} disagrees with ${rig_config} (project: ${project}); tearing down ${project}"
  fi
  slug="$(project_slug "$project")"
  [[ -n "$slug" ]] || return 0
  session="legion-${slug}"
  # Every Legion pane lives on the daemon's private tmux server, whose socket name equals the
  # session name; the default server never hosts one.
  tmux -L "$session" has-session -t "$session" 2>/dev/null || return 0
  owner="$(tmux -L "$session" show-option -qv -t "$session" @legion_owner 2>/dev/null || true)"
  if [[ "$owner" != "$session" ]]; then
    warn "refusing to kill unowned tmux session ${session}"
    return 0
  fi
  tmux -L "$session" kill-session -t "$session"
  printf 'STOPPED tmux session %s on private socket %s\n' "$session" "$session"
}

# Removes exactly the container up.sh recorded for this rig. The record is left in place so a
# repeat teardown against the same scratch directory still names this rig's container instead of
# falling through to the fixed name, which may belong to an older rig still running. Called only
# for a directory whose legion.yaml proves a rig started here, so the fixed-name fallback can only
# ever reach a pre-record rig's own container.
remove_nats_container() {
  local nats_name

  if [[ -r "$nats_record" ]]; then
    nats_name="$(<"$nats_record")"
    if [[ ! "$nats_name" =~ ^legion-smoke-nats-[a-z0-9]+$ ]]; then
      warn "refusing to remove NATS container: ${nats_record} does not name a rig container (${nats_name})"
      return 0
    fi
  else
    nats_name="$legacy_nats_name"
    printf 'no %s record; falling back to the old fixed name %s\n' "$nats_record" "$nats_name"
  fi
  docker container inspect "$nats_name" >/dev/null 2>&1 || return 0
  docker rm -f "$nats_name" >/dev/null
  printf 'STOPPED NATS container %s\n' "$nats_name"
}

main() {
  local started_a_rig=0
  if [[ -r "$rig_config" ]]; then
    started_a_rig=1
  fi

  terminate_pid_file envoy-bridge
  terminate_process_group_file webhook-forward
  terminate_process_group_file board-webhook-forward
  remove_webhook_forwarder webhook-forward
  remove_webhook_forwarder board-webhook-forward
  terminate_pid_file daemon
  if ((started_a_rig)); then
    stop_tmux_session
  else
    printf '%s has no legion.yaml: this directory never started a rig; stopping no tmux server and removing no container\n' "$smoke_dir"
  fi
  terminate_pid_file dispatch
  terminate_pid_file listener
  if ((started_a_rig)); then
    remove_nats_container
  fi
  printf 'RIG DOWN\n'
}

main "$@"
