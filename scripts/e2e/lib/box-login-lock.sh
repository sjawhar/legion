#!/usr/bin/env bash
# One box-login proof at a time on a box. Sourced, never run.
#
# take_box_login_lock [DIR] — takes a non-blocking flock on DIR/legion-e2e-box-login.lock, DIR
# being the operator's XDG_RUNTIME_DIR when it names a directory, else /tmp (pass DIR to choose
# it; the stage proofs pass none and call it before they move any XDG directory). The lock is
# held on a file descriptor of the calling shell for the rest of its life, and by every process it
# starts that keeps that descriptor, so a pane or daemon a run leaves behind still holds it. A run
# whose lock is taken fails at once, naming the holder the lock file records (its pid and command,
# or that it has exited and a process it started still holds the lock), and starts nothing.
#
# Why: under LEGION_E2E_MODEL_ROUTE=box-login (lib/install-box-login-route.sh) every pane of a run
# shares the operator's own Oh My Pi login database. Two runs at once would double the panes
# refreshing one login and race each other's teardown against it; one at a time keeps a run's
# load on the operator's login bounded and its evidence its own. The gateway and key-file routes
# share nothing of the operator's and take no lock.
take_box_login_lock() {
  local dir=${1:-} file holder pid cmd
  if [ -z "$dir" ]; then
    if [ -n "${XDG_RUNTIME_DIR:-}" ] && [ -d "$XDG_RUNTIME_DIR" ]; then dir=$XDG_RUNTIME_DIR; else dir=/tmp; fi
  fi
  file=$dir/legion-e2e-box-login.lock
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
