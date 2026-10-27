# shellcheck shell=bash
# lock.sh: one run at a time per LOADTEST_ROOT (README.md, "One run at a time"). run-step.sh,
# run-series.sh and stack.sh source it. Not executable.
#
# lock_root ROOT WHO takes an exclusive, non-blocking flock on ROOT/.run.lock on file descriptor 9,
# held until the process exits and the descriptor closes. The holder writes its pid, command, start
# time and a random token into the lock file, and exports the token as LOADTEST_LOCK_TOKEN. While
# the lock is held, a caller whose LOADTEST_LOCK_TOKEN equals the file's token is one of the
# holder's own children (run-series.sh's run-step.sh, run-step.sh's stack.sh): it goes on without
# locking. Any other caller is refused with exit 1, naming the root and the holder, before it
# touches the stack. stack.sh closes descriptor 9 in every stack process it starts, so no stack
# process keeps the lock.

lock_root() {
  local root=$1 who=$2
  local file=$root/.run.lock
  command -v flock >/dev/null || {
    printf '%s: flock (util-linux) is required\n' "$who" >&2
    exit 1
  }
  mkdir -p -- "$root"
  exec 9<>"$file"
  if flock -n 9; then
    local command
    command=$(tr '\0' ' ' <"/proc/$$/cmdline")
    LOADTEST_LOCK_TOKEN=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
    export LOADTEST_LOCK_TOKEN
    printf 'pid=%s\ncommand=%s\nstarted=%s\ntoken=%s\n' "$$" "${command% }" \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$LOADTEST_LOCK_TOKEN" >"$file"
    return 0
  fi
  exec 9>&-
  # The holder writes the file just after it locks: wait briefly for a complete record.
  local holder="" i
  for ((i = 0; i < 10; i++)); do
    holder=$(cat -- "$file" 2>/dev/null || true)
    [[ $holder == *$'\ntoken='* ]] && break
    sleep 0.1
  done
  local token
  token=$(sed -n 's/^token=//p' <<<"$holder")
  if [ -n "$token" ] && [ "${LOADTEST_LOCK_TOKEN:-}" = "$token" ]; then
    return 0
  fi
  printf '%s: LOADTEST_ROOT %s is in use by another run (pid %s, started %s): %s\n' "$who" "$root" \
    "$(sed -n 's/^pid=//p' <<<"$holder")" "$(sed -n 's/^started=//p' <<<"$holder")" \
    "$(sed -n 's/^command=//p' <<<"$holder")" >&2
  printf '%s: one run at a time per root (%s): wait for that run to end, or use another LOADTEST_ROOT\n' \
    "$who" "$file" >&2
  exit 1
}
