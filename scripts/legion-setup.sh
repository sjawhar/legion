#!/bin/sh
# Installs this repository's packages into a Legion issue workspace before the role's agent
# starts, so that the checks the role is told to run can run at all. Without `node_modules` a
# check command answers with the wrong tool and passes: `bunx biome check .` resolves a squatted
# npm package that checks nothing and exits 0, and a bare `tsc` is the worker image's own
# compiler, not the one this repository pins.
#
# Legion runs it as the deployment's setup command — one line a person adds outside this
# repository, run in every role container of an issue pod and in the daemon-launched controller's,
# before each start of that role's Oh My Pi:
#
#   runtime:
#     kubernetes:
#       setup: [/bin/sh, -c, 'if [ -r ./scripts/legion-setup.sh ]; then sh ./scripts/legion-setup.sh; fi']
#
# The `-r` test belongs to that line, not to this file: the same argv runs where this script does
# not exist — the controller's `/legion`, the project's other repository, a branch older than this
# change — and the shell's own "not found" would be a non-zero exit.
#
# It runs as the role's user (uid 1000), in the container's working directory (the issue's
# workspace), with the agent's whole environment, stdin on `/dev/null` and its stdout redirected
# onto stderr, so everything it prints is the role container's log.
#
# Every path here ends `exit 0`, the failures included. A setup command that exits non-zero stops
# Oh My Pi from starting, charges the launch a failure, and after three of them ends the claim in
# `worker-died` with the issue held: a failed install must reach the agent as a report it can read,
# never as a dead role. That report is `$LEGION_STATE_DIR/legion-setup-report.json`, whose
# `detail` is the install log's last 1000 bytes with control characters and non-ASCII bytes
# stripped and quotes and backslashes escaped, so the file is valid JSON whatever bun printed. Its
# `issue`, `role` and `generation` are there for its reader to match against its own environment:
# `LEGION_STATE_DIR` outlives a generation in a pod, and is shared by every pane of the host under
# the tmux runtime.
#
# The run shares the role's 360 s registration deadline, so it is bounded twice over:
# `LEGION_SETUP_LOCK_WAIT` (90 s) waiting for another role's install of the same workspace, then
# `LEGION_SETUP_INSTALL_TIMEOUT` (180 s) for the install itself — 270 s in the worst case, against
# a measured 54 s for the first install of 1,986 packages and about 0.3 s when they are already
# there and `bun.lock` has not moved. Whichever bound fires is a report, not a non-zero exit.
#
# It writes only under the issue volume — the workspace root the `/workspaces/` component names
# (`/legion`), or `$LEGION_WORKSPACE/.legion-setup`, which `.gitignore` holds, when that cannot be
# derived or written under — and the report under `$LEGION_STATE_DIR`. Never `/tmp` and never
# `$HOME`: those are the container's own ephemeral storage, whose breach evicts the whole pod and
# every role's work in it. bun's download cache is the one the deployment already names in
# `BUN_INSTALL_CACHE_DIR` — an issue pod tells every role container the issue's own, on its volume
# — and one beside this script's state when nothing names it.
# The marker that makes a second run cost nothing is keyed by
# `cksum < bun.lock`, not by the issue, because a forward merge moves `bun.lock` mid-issue and a
# `node_modules` from the lock before it fails the type check for the wrong reason. The install is
# `--frozen-lockfile`, which is what keeps `bun.lock` unchanged under an implementer who commits
# from this workspace.
#
# Run by hand from a workspace: LEGION_WORKSPACE=<that workspace> sh scripts/legion-setup.sh
# With LEGION_WORKSPACE unset it does nothing at all: a human's clone keeps bun's own cache and
# the `bun install` it always ran, and this script's Linux-only flock and timeout never reach a
# developer's machine. Its tests are scripts/legion-setup.test.sh, which CI runs in the test job
# of pr-and-main.yaml against a stub bun.
set -u

workspace=${LEGION_WORKSPACE-}
if [ -z "$workspace" ]; then
  exit 0
fi

# No checkout here, nothing to install: the controller's container has no workspace, and the
# project's other repository carries no lock file. Both are expected, so both are silent.
if [ ! -r bun.lock ]; then
  exit 0
fi

lock_wait=${LEGION_SETUP_LOCK_WAIT:-90}
install_timeout=${LEGION_SETUP_INSTALL_TIMEOUT:-180}
detail_bytes=1000

# The issue volume's root, which every workspace on it shares, so one role's install serves the
# next. When the workspace is not under a `/workspaces/` component the expansion returns it
# unchanged — or empty, when the path is nothing but that component — and the fallback is a
# directory inside the workspace itself, which .gitignore holds. The same fallback answers a
# derived root this user cannot write under.
#
# usable_root <dir>: sets root and state from <dir>, true when it can be written under.
usable_root() {
  root=$1
  state=$root/bun/state
  mkdir -p "$state" 2> /dev/null && [ -w "$state" ]
}
derived=${workspace%/workspaces/*}
if [ "$derived" = "$workspace" ] || [ -z "$derived" ]; then
  derived=$workspace/.legion-setup
fi
if ! usable_root "$derived" && ! usable_root "$workspace/.legion-setup"; then
  echo "legion setup: no writable install root under $root; leaving the install to the agent" >&2
  exit 0
fi

# The download cache is the deployment's when it names one — every role container of an issue pod
# is told the issue's own `BUN_INSTALL_CACHE_DIR` on its volume, and that one directory serving
# every role is the point of it — and otherwise one of this script's own beside the state, so a
# run anywhere else still keeps bun's cache off the container's own disk.
cache=${BUN_INSTALL_CACHE_DIR:-}
if [ -z "$cache" ]; then
  cache=$root/bun/cache
  mkdir -p "$cache" 2> /dev/null || true
fi
marker=$state/installed
log=$state/install.log

report=
if [ -n "${LEGION_STATE_DIR-}" ] && [ -d "${LEGION_STATE_DIR}" ] && [ -w "${LEGION_STATE_DIR}" ]; then
  report=$LEGION_STATE_DIR/legion-setup-report.json
fi

# escape: stdin to one JSON string's worth of characters on stdout. Control characters and bytes
# above ASCII are deleted rather than escaped — a tail cut mid-sequence would otherwise be invalid
# UTF-8, and a report that does not parse tells its reader nothing — and tabs, newlines and
# carriage returns become spaces, so the whole value is one line.
escape() {
  LC_ALL=C tr -d '\000-\010\013\014\016-\037\177-\377' \
    | LC_ALL=C tr '\011\012\015' '   ' \
    | LC_ALL=C sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# write_report <reason> <exit status> <detail>: the one file the agent's first turn reads. Written
# through a temporary name in the same directory, so a reader never sees half of it, and only when
# LEGION_STATE_DIR names a directory this user can write to.
write_report() {
  if [ -z "$report" ]; then
    return 0
  fi
  report_status=$2
  case "$report_status" in
    '' | *[!0-9]*) report_status=1 ;;
  esac
  if printf '{"schemaVersion":1,"kind":"install-failed","issue":"%s","role":"%s","generation":"%s","at":"%s","reason":"%s","exitStatus":%s,"detail":"%s"}\n' \
    "$(printf '%s' "${LEGION_ISSUE-}" | escape)" \
    "$(printf '%s' "${LEGION_ROLE-}" | escape)" \
    "$(printf '%s' "${LEGION_GENERATION-}" | escape)" \
    "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    "$1" \
    "$report_status" \
    "$(printf '%s' "$3" | escape)" \
    > "$report.tmp" 2> /dev/null; then
    mv -f "$report.tmp" "$report" 2> /dev/null
  fi
  rm -f "$report.tmp" 2> /dev/null
  return 0
}

clear_report() {
  if [ -n "$report" ]; then
    rm -f "$report" 2> /dev/null
  fi
  return 0
}

digest=$(cksum < bun.lock)

# The packages this lock names are installed, and installed here: the marker alone would survive a
# node_modules an agent deleted, or a volume whose workspace was recreated under it.
installed() {
  [ -f "$marker" ] || return 1
  [ -e node_modules/.bin/biome ] || return 1
  [ "$(cat "$marker" 2> /dev/null)" = "$digest" ]
}

# `exit` in a POSIX shell function ends the shell, so both call sites end the run the same way.
exit_if_installed() {
  installed || return 0
  clear_report
  echo "legion setup: this bun.lock's packages are already installed; nothing to do" >&2
  exit 0
}

exit_if_installed

# One install per workspace at a time: an issue pod runs a container per role, each with this
# workspace as its working directory, and two bun installs writing one node_modules leave neither.
# The lock file is created before the redirection, because a redirection that fails on `exec` ends
# a non-interactive shell with a status of its own.
if ! : > "$state/install.lock" 2> /dev/null; then
  echo "legion setup: cannot write $state/install.lock; leaving the install to the agent" >&2
  exit 0
fi
exec 9> "$state/install.lock"
flock -w "$lock_wait" 9
status=$?
if [ "$status" -ne 0 ]; then
  echo "legion setup: another install held $state/install.lock for more than ${lock_wait}s" >&2
  write_report lock-timeout "$status" \
    "another install held $state/install.lock for more than ${lock_wait}s"
  exit 0
fi

# The holder may have been installing exactly this lock's packages and just finished.
exit_if_installed

echo "legion setup: installing this bun.lock's packages into $workspace" >&2
BUN_INSTALL_CACHE_DIR="$cache" timeout -k 5 "$install_timeout" bun install --frozen-lockfile \
  > "$log" 2>&1
status=$?
# The log's last lines on stderr too, so the role container's log holds what bun said: stdout
# first, or the 2> would be the fd it was pointed at.
tail -n 20 "$log" >&2 2> /dev/null

if [ "$status" -eq 0 ]; then
  printf '%s\n' "$digest" > "$marker" 2> /dev/null
  clear_report
  echo "legion setup: installed this bun.lock's packages into $workspace" >&2
  exit 0
fi

# 124 is the timeout above; 137 is a SIGKILL, which on a 1 Gi role is the kernel reclaiming the
# install's memory — a working outcome, since the next role to start installs again.
case "$status" in
  124) reason=install-timeout ;;
  137) reason=install-killed ;;
  *) reason=install-failed ;;
esac
rm -f "$marker" 2> /dev/null
echo "legion setup: bun install exited $status ($reason); starting the agent without it" >&2
write_report "$reason" "$status" "$(tail -c "$detail_bytes" "$log" 2> /dev/null)"
exit 0
