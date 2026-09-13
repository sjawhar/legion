#!/usr/bin/env bash
set -euo pipefail

readonly smoke_dir="${SMOKE_DIR:-/tmp/legion-smoke}"
# up.sh writes this for every rig it starts (write_daemon_config). Its `project:` line is the one
# source of the rig's identity for teardown, and its `nats_urls:` entry names the rig's NATS port.
# A directory without a parseable project never started a rig: down.sh stops no tmux server and
# removes no container for it, whatever SMOKE_PROJECT in the environment says.
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

# Prints the project named by the directory's legion.yaml; fails when the file is unreadable or
# has no such line.
rig_project() {
  local line
  [[ -r "$rig_config" ]] || return 1
  while IFS= read -r line; do
    if [[ "$line" =~ ^project:[[:space:]]*([^[:space:]]+)[[:space:]]*$ ]]; then
      printf '%s\n' "${BASH_REMATCH[1]}"
      return 0
    fi
  done <"$rig_config"
  return 1
}

# Prints the NATS port from the directory's legion.yaml: the first `- nats://127.0.0.1:<port>`
# list item under `nats_urls:` (write_daemon_config writes exactly that shape, one entry). Fails
# when the file has no such entry.
rig_nats_port() {
  local line
  local in_nats_urls=0
  while IFS= read -r line; do
    if [[ "$line" =~ ^nats_urls:[[:space:]]*$ ]]; then
      in_nats_urls=1
      continue
    fi
    if ((in_nats_urls)); then
      if [[ "$line" =~ ^[[:space:]]+-[[:space:]]*nats://127\.0\.0\.1:([0-9]+)[[:space:]]*$ ]]; then
        printf '%s\n' "${BASH_REMATCH[1]}"
        return 0
      fi
      return 1
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
  local project="$1"
  local slug
  local session
  local owner
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

# Removes this rig's NATS container. With a record (every rig up.sh has started since the record
# existed) that is exactly the container named in it; the record is left in place so a repeat
# teardown against the same scratch directory still names this rig's container. Without a record
# the rig predates it and used the fixed name shared by every such rig, so the fixed-name
# container is removed only when its published port is the NATS port this directory's legion.yaml
# names -- the same ownership test up.sh's ensure_nats uses to reuse a container, exact because one
# port is bound by one rig. A fixed-name container on another port is another rig's and is left.
remove_nats_container() {
  local nats_name
  local rig_port
  local published

  if [[ -e "$nats_record" ]]; then
    if [[ ! -r "$nats_record" ]] || ! nats_name="$(<"$nats_record")"; then
      warn "refusing to remove NATS container: ${nats_record} exists but cannot be read"
      return 0
    fi
    if [[ ! "$nats_name" =~ ^legion-smoke-nats-[a-z0-9]+$ ]]; then
      warn "refusing to remove NATS container: ${nats_record} does not name a rig container (${nats_name})"
      return 0
    fi
    docker container inspect "$nats_name" >/dev/null 2>&1 || return 0
  else
    nats_name="$legacy_nats_name"
    rig_port="$(rig_nats_port)" || {
      warn "no ${nats_record} record and ${rig_config} has no nats_urls entry naming this rig's NATS port; leaving ${nats_name}"
      return 0
    }
    docker container inspect "$nats_name" >/dev/null 2>&1 || return 0
    published="$(docker port "$nats_name" 4222/tcp 2>/dev/null || true)"
    if [[ "$published" != *":${rig_port}"* ]]; then
      printf 'no %s record and %s is published on %s, not this rig'"'"'s NATS port %s: it is another rig'"'"'s container; leaving it\n' \
        "$nats_record" "$nats_name" "${published:-no port}" "$rig_port"
      return 0
    fi
    printf 'no %s record; %s is published on this rig'"'"'s NATS port %s, removing it\n' "$nats_record" "$nats_name" "$rig_port"
  fi
  docker rm -f "$nats_name" >/dev/null
  printf 'STOPPED NATS container %s\n' "$nats_name"
}

main() {
  local project=""
  project="$(rig_project)" || project=""

  terminate_pid_file envoy-bridge
  terminate_process_group_file webhook-forward
  terminate_process_group_file board-webhook-forward
  remove_webhook_forwarder webhook-forward
  remove_webhook_forwarder board-webhook-forward
  terminate_pid_file daemon
  if [[ -n "$project" ]]; then
    stop_tmux_session "$project"
  else
    printf '%s has no legion.yaml naming a project: this directory never started a rig; stopping no tmux server and removing no container\n' "$smoke_dir"
  fi
  terminate_pid_file dispatch
  terminate_pid_file listener
  if [[ -n "$project" ]]; then
    remove_nats_container
  fi
  printf 'RIG DOWN\n'
}

main "$@"
