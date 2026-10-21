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
# `kind` is `install-failed` and whose `detail` is the install log's last 1000 bytes with
# control characters and non-ASCII bytes stripped and quotes and backslashes escaped, so the
# file is valid JSON whatever bun printed. A setting of this container's environment the run
# could not use is told the same way — before that detail, or as a `setting-ignored` report of
# its own when the install succeeded and there is no failure to carry it (`use_seconds`). Its
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
# not a non-zero exit. A second bound sits inside that one: `LEGION_SETUP_LOCK_HOLD` (90 s) is the
# longest one attempt may hold the cache's lock, because the daemon's own plugin install waits on
# that same inode and fails the launch when its wait runs out — sized against that wait's
# derivation where it is set out below, and the first thing to lower in a deployment that lowers
# `worker_boot_timeout_seconds`.
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
# from this workspace, and `--backend=copyfile`, which keeps the cache every role shares from
# being hardlinked into that workspace's node_modules (the install below says why).
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

# Both of this run's bounds are read from the environment here, through the one reader either
# has. A value this shell cannot use must reach neither `timeout` nor its arithmetic: `$((08))`
# is dash's fatal `Illegal number: 08`, which would be the non-zero exit the header above
# promises this script cannot make, and `$((060))` is a silent 48 where the operator meant 60.
# So each knob is judged against the shape it accepts — a whole number of seconds, 1 or more,
# written without a leading zero — and a value of any other shape is replaced by the default and
# told to the agent, who is the only one who can pass it back to the person who typed it.
#
# setting_note: one sentence for each setting this run could not use, empty when there is
# nothing to tell. Every report written below carries it before its own detail, and a run that
# succeeds writes a `setting-ignored` report of its own to carry it (report_success).
setting_note=

# The largest number the shells this script runs under can take: dash answers `Illegal number`
# above it, in `$((…))` fatally and in `[ … -le … ]` with a status that would be read as "the
# clamp does not apply". Nothing below compares a value against it numerically; only its text
# and its length are read, which is the whole point.
SHELL_MAX=9223372036854775807

# use_seconds <name> <value> <default>: sets `seconds` to the whole number of seconds <value>
# names, or to <default> when it names anything else, appending the sentence for it to
# `setting_note`. It assigns rather than printing, because a note added inside a `$(…)` dies
# with the subshell that made it.
use_seconds() {
  seconds=$2
  case "$2" in
    # Unset or empty is how nearly every container runs this: the default is what the script
    # means by it, so it is no mistake and there is nothing to tell.
    '')
      seconds=$3
      return 0
      ;;
    # A bare `0`, rejected on its own account rather than for its shape: `timeout -k 5 0` is GNU
    # timeout's spelling of no timeout at all and `flock -w 0` is not a wait, so a 0 here would
    # be the unbounded hold this bound exists to prevent, not the shortest one. This line is
    # what stops that, however much it looks like an inconsistency with the arm below it.
    0) ;;
    # A leading zero: `08` and `060` are the octal parse above. The value is refused rather than
    # normalised, because normalising it with `$((…))` is that same parse.
    0*) ;;
    # Anything that is not digits throughout: `-9`, `abc`, `9x`, `1.5`, `90 `.
    *[!0-9]*) ;;
    # A digit string this shell cannot work with. `$((…))` and `[ … -le … ]` both answer dash's
    # `Illegal number` above its integer range: the first is a fatal exit — the one this file
    # promises it cannot make — and the second is worse than fatal, because its non-zero status
    # takes the `|| left=$hold` branch and `timeout -k 5 <huge>` then bounds nothing at all,
    # which is the unbounded hold on the shared cache lock this bound exists to prevent. So the
    # value is refused here, before any arithmetic or comparison touches it, by its text alone:
    # more digits than the largest number this shell takes, or the same count and a larger
    # string — equal-length digit strings compare as their numbers do. `9223372036854775807` is
    # the last one it can use and is taken; one more is refused.
    *)
      if [ "${#2}" -lt "${#SHELL_MAX}" ]; then
        return 0
      fi
      # `\>` is the shell's string comparison, which is what makes this safe: the value never
      # reaches arithmetic. Both shells this script runs under — the image's dash and the bash
      # its tests use — have it.
      if [ "${#2}" -eq "${#SHELL_MAX}" ] && ! [ "$2" \> "$SHELL_MAX" ]; then
        return 0
      fi
      ;;
  esac
  seconds=$3
  if [ -n "$setting_note" ]; then
    setting_note="$setting_note "
  fi
  setting_note="$setting_note$1=$2 is not a whole number of seconds (1 or more, with no"
  setting_note="$setting_note leading zero, and no larger than this shell can use), so this run"
  setting_note="$setting_note used the default of ${3}s instead."
  return 0
}

# One bound for the whole run: the wall clock the lock wait, the install and the single retry
# share, defaulting to the 270 s that leaves the role's 360 s registration deadline its margin.
# `deadline` is the instant that budget ends; every wait below is what is left of it then.
use_seconds LEGION_SETUP_BUDGET "${LEGION_SETUP_BUDGET-}" 270
budget=$seconds
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

# A second bound, on the only part of the run that makes another process wait: how long one
# attempt may hold `/usr/bin/flock` on the cache. That inode is not ours alone — the daemon's own
# plugin install takes it in this same container (plugins.go:225), waiting `installLockWait`,
# `RegistrationDeadline/3`, twice, and then exiting 1, which fails the launch before Oh My Pi
# starts; three such launches end the role in `worker-died`. A hold bounded only by the whole
# budget could therefore stop five other roles from starting, which is the one thing this install
# must never do, so the locked region yields instead: an install that cannot finish inside the
# hold releases the lock, writes no marker, reports what happened to its agent, and leaves the
# next role to install with whatever the cache now holds.
#
# The number, defended against the derivation rather than against today's value. The daemon's wait
# is `installLockWait` = `RegistrationDeadline(worker_boot_timeout_seconds,
# worker_boot_registration_deadline_intervals)/3`, floored at 1 s (plugins.go:139-141), so it is
# `boot × intervals / 3`: 120 s at the defaults (60 × 6 / 3), 60 s if a deployment halves the boot
# timeout, 1 s at the floor. A role container is told none of those three settings — its
# environment carries no boot timeout, no interval count and no deadline, which I checked
# (`roleEnvironment`, manifest.go) — so this script cannot derive the bound and must assume one.
# 90 s: under the daemon's 120 s wait at its defaults, so a plugin install that began waiting the
# instant this one took the lock never sees its first wait expire, and above the whole measured
# spread of a cold install of 1,986 packages on this volume — 33, 37, 45 and 59 seconds across
# four runs at this head, the variance being the registry's rather than the backend's. A bound
# inside that spread would be the worse failure: every role cut off at the hold, each leaving the
# next a warmer cache, until one happens to finish.
#
# What that fixed number costs where the assumption does not hold: at half the default boot
# timeout the daemon waits 60 s twice, and this hold can consume the first of the two and 30 s of
# the second, leaving the retry short rather than whole; below that, and at the floor where two
# 1 s waits already consume the deadline by design, a 90 s hold would retire a role that queued
# behind it. Such a deployment sets `LEGION_SETUP_LOCK_HOLD` below its own `installLockWait` —
# which is why the knob exists, why it is the first thing `docs/kubernetes.md` says beside this
# bound, and why the paragraph there that sizes `installLockWait` now names this script among the
# holders of that inode.
use_seconds LEGION_SETUP_LOCK_HOLD "${LEGION_SETUP_LOCK_HOLD-}" 90
lock_hold=$seconds

# The report's path comes before the install root below, and not after it as everything else
# about the report does, because the one failure this report mechanism exists for is a volume
# this role cannot write under — every check unavailable, for every role of the issue. That
# branch exits, so a report prepared after it would never be written at all. It depends on
# nothing the install root sets.
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

# write_report <kind> <reason> <exit status> <detail>: the one file the agent's first turn reads.
# Written through a temporary name in the same directory, so a reader never sees half of it, and
# only when LEGION_STATE_DIR names a directory this user can write to. The kind is the caller's
# because this file carries two of them: `install-failed`, the install whose absence the agent's
# checks will meet, and `setting-ignored`, a run that installed everything and could not use one
# setting of its container's environment. Whatever the kind, a setting this run refused comes
# first in the detail: that sentence has no report of its own to wait for, and the run that
# would have carried it may be this agent's only news of it.
write_report() {
  if [ -z "$report" ]; then
    return 0
  fi
  report_status=$3
  case "$report_status" in
    '' | *[!0-9]*) report_status=1 ;;
  esac
  report_detail=$4
  if [ -n "$setting_note" ]; then
    if [ -n "$report_detail" ]; then
      report_detail="$setting_note $report_detail"
    else
      report_detail=$setting_note
    fi
  fi
  if printf '{"schemaVersion":1,"kind":"%s","issue":"%s","role":"%s","generation":"%s","at":"%s","reason":"%s","exitStatus":%s,"detail":"%s"}\n' \
    "$1" \
    "$(printf '%s' "${LEGION_ISSUE-}" | escape)" \
    "$(printf '%s' "${LEGION_ROLE-}" | escape)" \
    "$(printf '%s' "${LEGION_GENERATION-}" | escape)" \
    "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    "$2" \
    "$report_status" \
    "$(printf '%s' "$report_detail" | escape)" \
    > "$report.tmp" 2> /dev/null; then
    mv -f "$report.tmp" "$report" 2> /dev/null
  fi
  rm -f "$report.tmp" 2> /dev/null
  return 0
}

# report_success: what a run that has this lock file's packages leaves behind. Normally nothing —
# a failure an earlier generation wrote must not reach this agent as its own news — but a run
# that could not use a setting of its environment has that one sentence to tell and no failing
# report to tell it in, and the removal would swallow it. Such a run writes `setting-ignored` in
# the removal's place: exit status 0, because nothing failed, and the sentence as its detail.
report_success() {
  if [ -z "$report" ]; then
    return 0
  fi
  if [ -n "$setting_note" ]; then
    write_report setting-ignored setting-ignored 0 ""
    return 0
  fi
  rm -f "$report" 2> /dev/null
  return 0
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
  # Nothing can be installed for any role of this issue, so the agent is told rather than left to
  # meet it as a check that will not run. It is the one failure that leaves no install log, so
  # the detail is what this run knows; the status is 0 because no install ran to have one.
  write_report install-failed no-install-root 0 \
    "no writable install root: neither $derived nor $workspace/.legion-setup can be written under"
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
  report_success
  echo "legion setup: this bun.lock's packages are already installed; nothing to do" >&2
  exit 0
}

exit_if_installed

# This run's own log, made after the check above because a run that installs nothing needs none,
# and removed when the run ends. Not one shared name: `$state` is on the issue volume, which
# every role container of the pod shares, so a waiter would truncate the holder's log the moment
# it began waiting, and the holder's own `tail` — and the report's `detail`, the only diagnostic
# the agent ever sees — would then quote the other role's install. mktemp names it rather than
# `$$`, because two role containers are two PID namespaces, where one pid is two runs.
log=$(mktemp "$state/install.XXXXXX" 2> /dev/null) || log=$state/install.$$
trap 'rm -f "$log" 2> /dev/null' EXIT

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
# What the locked shell exits when it found the packages already there and did nothing: a success
# this shell must not announce as an install it made.
ALREADY_INSTALLED=201

# `--backend=copyfile`, for the reason plugins.go:155-163 states about this same cache in this
# same container moments earlier: BUN_INSTALL_CACHE_DIR and the node_modules being written are
# both on the issue volume, one filesystem, where bun's default backend hardlinks the installed
# files out of the cache — an installed file and its cache entry become one inode, and a single
# in-place write under node_modules corrupts the cache every role of the issue shares for the
# life of the volume. The flag on the command is the only way to say it: bun ignores the setting
# in a bunfig.toml and has no environment variable for it (oven-sh/bun#25391, #32158). Sami's
# decision on dispatch://INFRA-31 to accept those links covers an install an agent types, which
# bun gives no way to change; this one is Legion's own, run before the agent, which is exactly
# the class the daemon's plugin install pays copyfile for.
#
# Everything else the install needs is done here too, by the shell flock runs under the lock, and
# nothing of it by the shell that waits for the lock:
#   * the install's own bound, because `timeout`'s duration is fixed when its argument is
#     expanded: an install handed what was left when the flock *started* would be given a budget
#     the wait has since spent, and the two would add up past the whole. $1 is the deadline, read
#     after the lock is taken;
#   * the same check the outer shell made before it queued, because two roles that start together
#     both pass that one: the first installs for the ~50 s it takes, and the second would
#     otherwise run a second `bun install` over the finished tree, in the workspace the first
#     role's agent is already working in — and a failure of that redundant install would delete a
#     marker that is true, cost every later role a full reinstall, and tell its agent the
#     packages could not be installed in a container where they are, which is the untrue report
#     this whole script exists to prevent. A waiter that arrives behind a finished install does
#     no work at all;
#   * the marker, so the run that wrote the tree is the run that vouches for it and no waiter
#     rewrites it;
#   * the log's truncation, because the log is this run's own file and this is the moment it
#     starts being written — after the check above, so that a waiter arriving behind a finished
#     install says so on the role container's own stderr rather than into a log it then deletes.
#   * the install's own bound, which is now two numbers rather than one: what is left of the whole
#     budget, and `lock_hold`, whichever is smaller. The first keeps this run inside the role's
#     start; the second keeps it out of everyone else's, and both are computed here, after the
#     lock is taken, so a wait and a hold can never add past the budget.
# shellcheck disable=SC2016 # the arithmetic belongs to the shell under the lock, not to this one
locked_install='deadline=$1 marker=$2 digest=$3 log=$4 already=$5 hold=$6
if [ -f "$marker" ] && [ -e node_modules/.bin/biome ] &&
  [ "$(cat "$marker" 2> /dev/null)" = "$digest" ]; then
  exit "$already"
fi
exec > "$log" 2>&1
left=$((deadline - $(date +%s)))
[ "$left" -ge 1 ] || left=1
[ "$left" -le "$hold" ] || left=$hold
timeout -k 5 "$left" bun install --backend=copyfile --frozen-lockfile || exit $?
printf "%s\n" "$digest" > "$marker" 2> /dev/null || true'

install_once() {
  [ -d "$cache" ] || rm -f -- "$cache"
  mkdir -p -- "$cache" 2> /dev/null
  BUN_INSTALL_CACHE_DIR="$cache" /usr/bin/flock -w "$(remaining)" -E "$LOCK_BUSY" -- "$cache" \
    sh -c "$locked_install" sh "$deadline" "$marker" "$digest" "$log" "$ALREADY_INSTALLED" \
      "$lock_hold"
}

echo "legion setup: installing this bun.lock's packages into $workspace" >&2
install_once
status=$?
# The first attempt's own status, kept because `status` becomes the retry's: it is the only
# thing that tells a run which never got the lock from one that held it, installed, and lost the
# lock on the way back (the failure branch below turns on it).
first_status=$status

# The retry is for the cold-entry race alone, and it waits on the same remaining budget as the
# attempt before it, so a second attempt cannot push the script past the registration deadline it
# shares with the role's launch. A budget with no more than a few seconds left buys nothing but
# that deadline's risk, so the retry is skipped. The match is the part of bun's message that does
# not depend on how it reached the file — plugins.go:169-171 measured
# `ENOENT: failed copying files from cache to destination` under copyfile, this install carries a
# backend flag of its own, and a wording pinned to one whole sentence would silently stop
# retrying the moment either changed. What identifies the race is the copy out of the cache.
if [ "$status" -ne 0 ] && [ "$status" -ne "$LOCK_BUSY" ] &&
  grep -q 'failed copying files from cache' "$log" 2> /dev/null; then
  if [ "$(remaining)" -gt 10 ]; then
    echo "legion setup: the cache was filled under this install; retrying once" >&2
    install_once
    status=$?
  fi
fi
# The log's last lines on stderr too, so the role container's log holds what bun said: stdout
# first, or the 2> would be the fd it was pointed at.
tail -n 20 "$log" >&2 2> /dev/null

# The marker is the locked shell's, written where the tree was written and under the lock that
# made it safe to write; this shell only reports what that one did, and says which of the two
# things happened rather than claiming the install either way.
if [ "$status" -eq "$ALREADY_INSTALLED" ]; then
  report_success
  echo "legion setup: another role installed this bun.lock while this run waited; nothing to do" >&2
  exit 0
fi
if [ "$status" -eq 0 ]; then
  report_success
  echo "legion setup: installed this bun.lock's packages into $workspace" >&2
  exit 0
fi

# 199 is flock's own, set by -E above: another install of this issue held the cache for every
# second this run had, and the agent must start anyway, since a failure must not hold the work
# up. 137 is a SIGKILL, which on a 1 Gi role is the kernel reclaiming the install's memory — a
# working outcome, since the next role to start installs again. 124 is `timeout`'s, and since
# the hold went in it is two different outcomes with one status: the whole budget ran out on a
# slow or offline container, or this attempt reached `lock_hold` and yielded the cache's lock
# with the budget still running. They are worth different things to the agent — only the second
# makes running this script again by hand likely to work, which is what its notice advises — so
# the deadline tells them apart: an attempt that ended before the budget's instant is the yield.
case "$status" in
  "$LOCK_BUSY") reason=lock-timeout ;;
  124)
    if [ "$(date +%s)" -lt "$deadline" ]; then
      reason=install-hold-timeout
    else
      reason=install-timeout
    fi
    ;;
  137) reason=install-killed ;;
  *) reason=install-failed ;;
esac
# Whether this run ever held the lock is the *first* attempt's status, not the reason it ends
# with: the retry of the cold-entry race queues for the lock again, and can lose it to another
# role of the issue, so a run that took the lock, installed, and failed the copy out of the
# cache also ends at $LOCK_BUSY. Only a first attempt that timed out waiting learned nothing;
# anything else means this run held the lock, wrote into node_modules, and has an install log of
# its own, which is what its agent must be told and what its marker must answer for.
if [ "$first_status" -eq "$LOCK_BUSY" ]; then
  # No marker is touched here: this run never held the lock, never looked at node_modules, and
  # learned nothing about what is installed. The holder writes the marker as its last act inside
  # the lock, so a `-w` that expires in that window would otherwise delete a marker that is true
  # and cost every later role of the issue a full reinstall.
  echo "legion setup: another install held $cache for this run's whole ${budget}s budget" >&2
  write_report install-failed lock-timeout "$status" \
    "another install held $cache for this run's whole ${budget}s budget"
  exit 0
fi
# Only a run that attempted an install invalidates the marker: what it found there is now
# whatever a cut-off `bun install` left.
rm -f "$marker" 2> /dev/null
echo "legion setup: bun install exited $status ($reason); starting the agent without it" >&2
write_report install-failed "$reason" "$status" "$(tail -c "$detail_bytes" "$log" 2> /dev/null)"
exit 0
