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
# The run shares the role's 360 s registration deadline, so it is bounded once, as a whole:
# `LEGION_SETUP_BUDGET` (270 s) is this run's wall clock, and every wait inside it — the flock on
# the cache another role's install of the same workspace holds, the install's own timeout, the one
# retry — is what is left of that budget when that wait starts. A first attempt therefore never
# gives up on the lock before the install it is waiting for would itself have timed out, and no
# two waits can add up past the whole: the worst case is the budget plus the 5 s `timeout` leaves
# between its SIGTERM and its SIGKILL, 275 s against that 360 s deadline, which leaves the launch
# the rest. Against it, a measured 54 s for the first install of 1,986 packages and about 0.3 s
# when they are already there and `bun.lock` has not moved. Whichever wait runs out is a report,
# not a non-zero exit.
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

# One bound for the whole run, from the environment: the wall clock the lock wait, the install and
# the single retry share, defaulting to the 270 s that leaves the role's 360 s registration
# deadline its margin. `deadline` is the instant that budget ends; every wait below is what is
# left of it then. A value that is not a whole number of seconds would make that arithmetic a
# fatal shell error, which is the one thing this script must never do, so it falls back to the
# default — as write_report's status does with a status that is not a number.
budget=${LEGION_SETUP_BUDGET:-270}
case "$budget" in
  '' | *[!0-9]*) budget=270 ;;
esac
deadline=$(($(date +%s) + budget))
detail_bytes=1000

# remaining: the whole seconds left of the budget, never fewer than 1. `timeout 0` is GNU
# timeout's spelling of "no timeout at all", which on an exhausted budget would be this script's
# longest wait rather than its shortest, and `flock -w 0` is not a wait either.
remaining() {
  left=$((deadline - $(date +%s)))
  if [ "$left" -lt 1 ]; then
    left=1
  fi
  printf '%s\n' "$left"
}

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
# The marker is the issue volume's, beside the cache every role of the issue shares, not the
# workspace's: that is what makes one role's install serve the next five. It is also why a failed
# run's `rm -f "$marker"` below reaches every workspace on the volume — a second workspace there
# whose install failed would cost the first's roles a reinstall. Nothing reaches that: production
# provisions exactly one clone and one jj workspace per issue volume (`workspace-init provision`,
# `workspace.Location` in `packages/daemon/internal/workspace/provision.go`, which names the
# repository and the issue and never a role), so one volume is one workspace. A tree that ever
# puts two on one volume is where this would have to be keyed by workspace as well as by lock.
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

# The install is serialised on the cache directory itself and retried once, following Legion's own
# plugin install (packages/daemon/internal/runtime/sandbox/plugins.go), which runs in this same
# container moments before this script and takes the same lock. Filling a cold entry of a bun
# install cache is not concurrency-safe: concurrent installs against one empty
# BUN_INSTALL_CACHE_DIR leave several exiting 1 with
# `ENOENT: failed copying files from cache to destination`, and the race is the cold entry's fill
# rather than the install out of it. The cache is what every role of the issue shares, so locking
# it also keeps two roles from writing one node_modules — a lock file of our own beside the marker
# would do the second and not the first. flock is advisory and per-inode, which is exactly the
# scope wanted: the containers of one pod share a kernel and that inode on the issue's volume, and
# two pods of two issues have two volumes anyway. The directory is remade inside each attempt,
# with a non-directory of that name removed first, because flock creates a missing path as a
# regular file and exits 0, which would leave every later run dying on `mkdir: File exists`.
# `/usr/bin/flock` by absolute path, as plugins.go names it, and the worker image's build is held
# to that exact path.
LOCK_BUSY=199

# The install's own bound is computed inside the lock, by the shell flock runs, rather than before
# it: `timeout`'s duration is fixed when its argument is expanded, so an install handed what was
# left when the flock *started* would be given a budget the wait has since spent, and the two
# would add up past the whole. $1 is the deadline, read after the lock is taken.
# shellcheck disable=SC2016 # the arithmetic belongs to the shell under the lock, not to this one
locked_install='left=$(($1 - $(date +%s)))
[ "$left" -ge 1 ] || left=1
exec timeout -k 5 "$left" bun install --frozen-lockfile'

install_once() {
  [ -d "$cache" ] || rm -f -- "$cache"
  mkdir -p -- "$cache" 2> /dev/null
  BUN_INSTALL_CACHE_DIR="$cache" /usr/bin/flock -w "$(remaining)" -E "$LOCK_BUSY" -- "$cache" \
    sh -c "$locked_install" sh "$deadline" > "$log" 2>&1
}

echo "legion setup: installing this bun.lock's packages into $workspace" >&2
install_once
status=$?

# The retry is for the cold-entry race alone, and it waits on the same remaining budget as the
# attempt before it, so a second attempt cannot push the script past the registration deadline it
# shares with the role's launch. A budget with no more than a few seconds left buys nothing but
# that deadline's risk, so the retry is skipped.
if [ "$status" -ne 0 ] && [ "$status" -ne "$LOCK_BUSY" ] &&
  grep -q 'ENOENT: failed copying files from cache to destination' "$log" 2> /dev/null; then
  if [ "$(remaining)" -gt 10 ]; then
    echo "legion setup: the cache was filled under this install; retrying once" >&2
    install_once
    status=$?
  fi
fi
# The log's last lines on stderr too, so the role container's log holds what bun said: stdout
# first, or the 2> would be the fd it was pointed at.
tail -n 20 "$log" >&2 2> /dev/null

if [ "$status" -eq 0 ]; then
  printf '%s\n' "$digest" > "$marker" 2> /dev/null
  clear_report
  echo "legion setup: installed this bun.lock's packages into $workspace" >&2
  exit 0
fi

# 199 is flock's own, set by -E above: another install of this issue held the cache for every
# second this run had, and the agent must start anyway, since a failure must not hold the work up.
# The wait was the whole budget, so that is what the report names. 124 is the install's own
# timeout, the budget cutting it off where it stood; 137 is a SIGKILL, which on a 1 Gi role is the
# kernel reclaiming the install's memory — a working outcome, since the next role to start
# installs again.
case "$status" in
  "$LOCK_BUSY") reason=lock-timeout ;;
  124) reason=install-timeout ;;
  137) reason=install-killed ;;
  *) reason=install-failed ;;
esac
rm -f "$marker" 2> /dev/null
if [ "$reason" = lock-timeout ]; then
  echo "legion setup: another install held $cache for this run's whole ${budget}s budget" >&2
  write_report lock-timeout "$status" \
    "another install held $cache for this run's whole ${budget}s budget"
  exit 0
fi
echo "legion setup: bun install exited $status ($reason); starting the agent without it" >&2
write_report "$reason" "$status" "$(tail -c "$detail_bytes" "$log" 2> /dev/null)"
exit 0
