# shellcheck shell=bash
# common.sh: what run-step.sh, run-series.sh and stack.sh share (README.md). They source it. Not
# executable.
#
# resolve_paths sets SRC to LOADTEST_SRC (default: the checkout holding this file) and ROOT to
# LOADTEST_ROOT (default ${TMPDIR:-/tmp}/dispatch-loadtest, created), both as physical paths, and
# exports them as LOADTEST_SRC and LOADTEST_ROOT.
#
# lock_root ROOT WHO: one run at a time per root (README.md, "One run at a time"). It takes an
# exclusive, non-blocking flock on ROOT/.run.lock on file descriptor 9. The lock lasts until every
# process that holds that descriptor has exited: the holder and the children that inherit it. Once
# locked, the holder empties the lock file, then writes its pid, command, start time and a random
# token into it with one printf, and exports the token as LOADTEST_LOCK_TOKEN.
#
# A holder's own child (run-series.sh's run-step.sh, a run's stack.sh) goes on without locking when
# its LOADTEST_LOCK_TOKEN equals the file's token and its descriptor 9 is the inherited locked one:
# it keeps that descriptor and sets LOCK_INHERITED=true. So a series step, and the loadgen run that
# it starts, keep the root when run-series.sh is killed. Any other caller is refused with exit 1
# before it touches the stack, naming the root and the live holder: the record's pid, once the file
# holds a complete record whose pid runs, or else the processes that still hold descriptor 9 on the
# file (the children of a holder that has exited). stack.sh closes descriptor 9 in every stack
# process that it starts, so no stack process keeps the lock.
#
# prepare_stack WHO runs stack.sh setup (an incremental build of SRC), start and seed, each of which
# does nothing when its work is already done, and the migration check: a build whose highest
# migration is older than the seed's is refused with exit 1. Then it sets BUILD to
# `stack.sh build-info` ("<commit> <dirty>"). run-step.sh calls it before a step that is not part of
# a series; run-series.sh calls it once, before its first step.

resolve_paths() {
  SRC=${LOADTEST_SRC:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd -P)}
  ROOT=${LOADTEST_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest}
  mkdir -p -- "$ROOT"
  ROOT=$(cd -- "$ROOT" && pwd -P)
  export LOADTEST_SRC=$SRC LOADTEST_ROOT=$ROOT
}

# lock_field NAME RECORD: the value of NAME= in a lock record.
lock_field() { sed -n "s/^$1=//p" <<<"$2"; }

# lock_pid_alive PID: PID is a running process, not a zombie.
lock_pid_alive() {
  local stat
  [[ $1 =~ ^[0-9]+$ ]] || return 1
  { read -r stat <"/proc/$1/stat"; } 2>/dev/null || return 1
  [[ ${stat##*) } != Z* ]]
}

# lock_holders FILE: "pid N (command)" of each process whose descriptor 9 is open on FILE, joined
# with ", ".
lock_holders() {
  local file=$1 dir cmd out=""
  for dir in /proc/[0-9]*; do
    [ "$dir/fd/9" -ef "$file" ] 2>/dev/null || continue
    cmd=$(tr '\0' ' ' <"$dir/cmdline" 2>/dev/null) || continue
    out+="${out:+, }pid ${dir#/proc/} (${cmd% })"
  done
  [ -z "$out" ] || printf '%s\n' "$out"
}

lock_root() {
  local root=$1 who=$2
  local file=$root/.run.lock
  command -v flock >/dev/null || {
    printf '%s: flock (util-linux) is required\n' "$who" >&2
    exit 1
  }
  mkdir -p -- "$root"
  LOCK_INHERITED=false
  # A holder's own child keeps the descriptor 9 that it inherited, which holds the lock: opening the
  # file again on descriptor 9 would replace it, and the child would hold nothing. flock on the
  # holder's own open file description succeeds; on any other description, while the holder runs, it
  # fails.
  if [ -n "${LOADTEST_LOCK_TOKEN:-}" ] && [ /dev/fd/9 -ef "$file" ] &&
    [ "$(lock_field token "$(cat -- "$file" 2>/dev/null)")" = "$LOADTEST_LOCK_TOKEN" ] && flock -n 9; then
    LOCK_INHERITED=true
    return 0
  fi
  exec 9<>"$file"
  if flock -n 9; then
    # Empty the file before anything else, so that a refused caller never reads the previous
    # holder's record, still complete in the file, as this one's.
    : >"$file"
    local command token
    command=$(tr '\0' ' ' <"/proc/$$/cmdline")
    token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
    printf 'pid=%s\ncommand=%s\nstarted=%s\ntoken=%s\n' "$$" "${command% }" \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$token" >"$file"
    export LOADTEST_LOCK_TOKEN=$token
    return 0
  fi
  exec 9>&-
  # The holder writes its record just after it locks. Wait up to 3 s for a complete record whose pid
  # runs; a complete record whose pid has exited is a holder whose children still hold the lock, or
  # one that has just ended: name the processes that hold descriptor 9 on the file instead.
  local record="" pid="" holders="" i
  for ((i = 0; i < 30; i++)); do
    record=$(cat -- "$file" 2>/dev/null || true)
    pid=$(lock_field pid "$record")
    if [[ $(lock_field token "$record") =~ ^[0-9a-f]{32}$ ]]; then
      lock_pid_alive "$pid" && break
      holders=$(lock_holders "$file")
      [ -n "$holders" ] && break
    fi
    sleep 0.1
  done
  local started command
  started=$(lock_field started "$record")
  command=$(lock_field command "$record")
  if lock_pid_alive "$pid"; then
    printf '%s: LOADTEST_ROOT %s is in use by another run (pid %s, started %s): %s\n' "$who" "$root" \
      "$pid" "$started" "$command" >&2
  else
    [ -n "$holders" ] || holders=$(lock_holders "$file")
    local taker="left no record in the lock file"
    [ -z "$pid" ] || taker="(pid $pid, started $started: $command) has exited"
    if [ -n "$holders" ]; then
      printf '%s: LOADTEST_ROOT %s is in use by another run: %s holds its lock; the run that took it %s\n' \
        "$who" "$root" "$holders" "$taker" >&2
    else
      printf '%s: LOADTEST_ROOT %s was locked by another run, and no process holds it now; the run that took it %s. Try again\n' \
        "$who" "$root" "$taker" >&2
    fi
  fi
  printf '%s: one run at a time per root (%s): wait for that run to end, or use another LOADTEST_ROOT\n' \
    "$who" "$file" >&2
  exit 1
}

prepare_stack() {
  local who=$1 stack
  stack=$(dirname -- "${BASH_SOURCE[0]}")/stack.sh
  "$stack" setup
  "$stack" start
  "$stack" seed
  # A build cannot start on a seed that a newer build migrated: compare the highest migration file
  # of this checkout with the seed's level.
  local migrations=$SRC/packages/envoy/internal/dispatch/store/migrations build_file build_level seed_level
  build_file=$(cd -- "$migrations" && printf '%s\n' [0-9]*.up.sql | sort -n | tail -n1)
  build_level=$((10#${build_file%%_*}))
  seed_level=$(jq -er .migration "$ROOT/seed.meta.json")
  if ((build_level < seed_level)); then
    printf '%s: this build'"'"'s highest migration is %s (%s), older than the seed'"'"'s %s (seed made by %s); reseed with this build (stack.sh seed --force) or run a newer one\n' \
      "$who" "$build_level" "$migrations/$build_file" "$seed_level" "$(jq -r .build_commit "$ROOT/seed.meta.json")" >&2
    exit 1
  fi
  if ((build_level > seed_level)); then
    printf '%s: note: the seed is at migration %s and this build at %s. Dispatch migrates each step'"'"'s copy forward at start, and any backfill those migrations start runs inside the step; reseed with this build (stack.sh seed --force) to measure it alone\n' \
      "$who" "$seed_level" "$build_level" >&2
  fi
  BUILD=$("$stack" build-info)
}
