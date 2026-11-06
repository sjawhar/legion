#!/usr/bin/env bash
# What a stage proof run under LEGION_E2E_MODEL_ROUTE=box-login (lib/install-box-login-route.sh)
# needs beyond the route itself: every pane of such a run shares the operator's own Oh My Pi login
# database through the profile's agent.db link. Sourced, never run.
#
# box_login_lock_file is one fixed path per operator uid, /tmp/legion-e2e-box-login.<uid>.lock,
# whatever the caller's XDG_RUNTIME_DIR or TMPDIR, so every box-login run of that operator meets
# the same lock. A test may set it after sourcing; nothing reads it from the environment.
box_login_lock_file=/tmp/legion-e2e-box-login.$(id -u).lock
#
# take_box_login_lock — takes a non-blocking flock on $box_login_lock_file. The stage proofs call it
# first, before they make a work directory or start anything, so a refused run leaves nothing. The
# lock is held on a file descriptor of the calling shell for the rest of its life, and by every
# process it starts that keeps that descriptor, so a pane or daemon a run leaves behind still holds
# it. A run whose lock is taken fails at once, naming the holder the lock file records (its pid and
# command, or that it has exited and a process it started still holds the lock).
#
# Why one at a time: two runs at once would double the panes refreshing one login and race each
# other's teardown against it; one at a time keeps a run's load on the operator's login bounded and
# its evidence its own. The gateway and key-file routes share nothing of the operator's and take no
# lock.
take_box_login_lock() {
  local file=$box_login_lock_file holder pid cmd
  command -v flock >/dev/null || { echo "take_box_login_lock: flock is required" >&2; return 1; }
  exec {box_login_lock_fd}>>"$file" || { echo "take_box_login_lock: cannot open $file" >&2; return 1; }
  if ! flock -n "$box_login_lock_fd"; then
    holder=$(<"$file") || holder=
    pid=${holder%% *}
    if [[ $pid =~ ^[0-9]+$ ]] && cmd=$(ps -o args= -p "$pid" 2>/dev/null) && [ -n "$cmd" ]; then
      echo "take_box_login_lock: another box-login proof holds $file: pid $pid ($cmd); one box-login proof runs at a time" >&2
    elif [[ $pid =~ ^[0-9]+$ ]]; then
      echo "take_box_login_lock: another box-login proof holds $file: pid $pid (${holder#* }) has exited, and a process it started still holds the lock; one box-login proof runs at a time" >&2
    else
      echo "take_box_login_lock: another box-login proof holds $file (it records no holder yet); one box-login proof runs at a time" >&2
    fi
    exec {box_login_lock_fd}>&-
    return 1
  fi
  # Same inode, so the lock the descriptor holds stays; the content names this holder.
  printf '%s %s\n' "$$" "$(ps -o args= -p "$$" 2>/dev/null || echo "$0")" >"$file"
}

# box_login_stop_grace is how long stop_box_login_processes waits, in seconds.
box_login_stop_grace=10
# stop_box_login_processes PID… — SIGTERM to every PID, a wait of at most box_login_stop_grace
# seconds for all of them to exit, then SIGKILL to those still alive. A pane's Oh My Pi in the
# middle of a token refresh gets the time to write the rotated token back to the operator's
# database before it goes; a plain SIGKILL could leave the database with a refresh token that is
# already spent. Never fails.
stop_box_login_processes() {
  local pid i alive
  [ $# -gt 0 ] || return 0
  kill -TERM "$@" 2>/dev/null || true
  for ((i = 0; i < box_login_stop_grace * 5; i++)); do
    alive=()
    for pid in "$@"; do kill -0 "$pid" 2>/dev/null && alive+=("$pid"); done
    [ "${#alive[@]}" -gt 0 ] || return 0
    sleep 0.2
  done
  kill -KILL "${alive[@]}" 2>/dev/null || true
  return 0
}

# unlink_box_login_db AGENT_DIR — removes AGENT_DIR/agent.db when it is a symlink (the link
# lib/install-box-login-route.sh made to the operator's database), the link itself and never what
# it names, so a kept work directory holds no live path to the operator's login. Call it once every
# process of the run is gone: a process that opened the database again after the link went would
# make a fresh one of its own in the profile. Never fails.
unlink_box_login_db() {
  local link=${1:-}/agent.db
  [ -n "${1:-}" ] && [ -L "$link" ] || return 0
  rm -f -- "$link" || true
  return 0
}
