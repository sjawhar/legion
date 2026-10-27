#!/usr/bin/env bash
# packages/envoy/loadtest/common.test.sh
#
# Proves the run lock of common.sh (lock_root) without a stack. Each lock taker below sources
# common.sh and calls lock_root, as run-series.sh, run-step.sh and stack.sh do.
#
# INFRA-589 review of PR #2369: a series' run-step.sh re-opened descriptor 9, so it lost the lock
# that it inherited from run-series.sh and went on by the token alone. Killing run-series.sh then
# freed the root while its step still ran, and a new run-step.sh reset the stack under that step.
# And a refused caller could name a previous holder that had ended, whose complete record was still
# in the lock file while the new holder had locked but not yet written its own.
#
# Asserts that: (a) a holder's child that took the lock with the inherited token and descriptor
# keeps the root after the holder is killed: a third caller is refused, naming the holder's record
# and the live child, and is admitted once the child has exited; (b) a refusal names the live
# holder, not a dead previous holder whose complete record is still in the file; (c) a caller with
# the holder's token but without its descriptor 9 is refused.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly helper="${here}/common.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly locker="${temporary_dir}/locker.sh"
readonly slow_bin="${temporary_dir}/slow-bin"

pids=()
cleanup() {
  local pid
  for pid in "${pids[@]}"; do
    kill -TERM "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

# locker.sh HELPER ROOT WHO PIDFILE [CHILD_PIDFILE]: take ROOT's lock as WHO, sourcing HELPER as
# the scripts do, write this pid to PIDFILE once admitted, then sleep. With CHILD_PIDFILE it first
# starts a child that does the same with the inherited token and descriptor 9, as run-series.sh
# starts its run-step.sh.
cat >"$locker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=/dev/null
source "$1"
lock_root "$2" "$3"
if [ $# -ge 5 ]; then
  bash "${BASH_SOURCE[0]}" "$1" "$2" child "$5" &
fi
echo "$$" >"$4.tmp"
mv -f -- "$4.tmp" "$4"
exec sleep 300
EOF

# A `date` that takes 1 s: lock_root calls it after its flock and before it writes its record, so
# a holder with it on PATH holds the lock for 1 s without its own record in the file.
mkdir -p "$slow_bin"
cat >"${slow_bin}/date" <<'EOF'
#!/usr/bin/env bash
: >"${LOADTEST_TEST_DATE_MARK:?}"
sleep 1
exec "${LOADTEST_TEST_REAL_DATE:?}" "$@"
EOF
chmod +x "${slow_bin}/date"
real_date="$(command -v date)"
readonly real_date

# alive PID: PID runs and is not a zombie.
alive() {
  local stat
  { read -r stat <"/proc/$1/stat"; } 2>/dev/null || return 1
  [[ ${stat##*) } != Z* ]]
}

# wait_for FILE: wait up to 10 s for FILE to exist.
wait_for() {
  local i
  for ((i = 0; i < 100; i++)); do
    [[ -s "$1" ]] && return 0
    sleep 0.1
  done
  return 1
}

# wait_gone PID: wait up to 10 s for PID to end.
wait_gone() {
  local i
  for ((i = 0; i < 100; i++)); do
    alive "$1" || return 0
    sleep 0.1
  done
  return 1
}

# try_lock ROOT [NAME=VALUE...]: call lock_root on ROOT in a new process that holds no descriptor 9
# and no token unless one is given. Sets rc and out (its output, ADMITTED when it was admitted).
try_lock() {
  local root=$1
  shift
  rc=0
  out="$(env -u LOADTEST_LOCK_TOKEN "$@" bash -c 'source "$1"; lock_root "$2" caller; echo ADMITTED' _ \
    "$helper" "$root" 2>&1 9>&-)" || rc=$?
}

# names TEXT PID: TEXT names "pid PID".
names() { [[ $1 =~ pid\ $2([^0-9]|$) ]]; }

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  [[ -z "${out:-}" ]] || printf '  caller output:\n%s\n' "$out" >&2
  return 1
}

case_orphaned_child() {
  out=""
  local root="${temporary_dir}/root-a"
  bash "$locker" "$helper" "$root" holder "${temporary_dir}/holder-a" "${temporary_dir}/child-a" \
    >"${temporary_dir}/holder-a.out" 2>&1 &
  local holder=$!
  pids+=("$holder")
  wait_for "${temporary_dir}/child-a" || { out="$(cat "${temporary_dir}/holder-a.out")" fail "(a) the holder's child was not admitted"; return; }
  local child
  child="$(<"${temporary_dir}/child-a")"
  pids+=("$child")
  kill -TERM "$holder"
  wait "$holder" 2>/dev/null || true
  alive "$child" || { fail "(a) the child ended with its holder"; return; }

  try_lock "$root"
  if ((rc == 0)); then
    fail "(a) a caller was admitted while the child of the killed holder (pid $child) still runs: the child held no lock, so on a live stack a new run-step.sh would reset the stack under a running series step"
    return
  fi
  names "$out" "$holder" || { fail "(a) the refusal does not name the holder's record (pid $holder)"; return; }
  names "$out" "$child" || { fail "(a) the refusal does not name the live child that holds the lock (pid $child)"; return; }

  kill -TERM "$child"
  wait_gone "$child" || { fail "(a) the child (pid $child) did not end"; return; }
  try_lock "$root"
  if ((rc != 0)) || [[ $out != *ADMITTED* ]]; then
    fail "(a) a caller was refused after the child of the killed holder had exited"
    return
  fi
  out=""
  printf 'PASS: a holder'"'"'s child keeps the lock after the holder is killed, and the root is free once the child exits\n'
}

case_stale_record() {
  out=""
  local root="${temporary_dir}/root-b"
  bash "$locker" "$helper" "$root" previous "${temporary_dir}/previous-b" >/dev/null 2>&1 &
  local previous=$!
  pids+=("$previous")
  wait_for "${temporary_dir}/previous-b" || { fail "(b) the previous holder was not admitted"; return; }
  kill -TERM "$previous"
  wait "$previous" 2>/dev/null || true
  grep -q "^pid=${previous}\$" "${root}/.run.lock" || { fail "(b) the ended holder's record is not in the lock file"; return; }

  PATH="${slow_bin}:${PATH}" LOADTEST_TEST_DATE_MARK="${temporary_dir}/date-b" LOADTEST_TEST_REAL_DATE="$real_date" \
    bash "$locker" "$helper" "$root" holder "${temporary_dir}/holder-b" >"${temporary_dir}/holder-b.out" 2>&1 &
  local holder=$!
  pids+=("$holder")
  local i
  for ((i = 0; i < 100; i++)); do
    [[ -e "${temporary_dir}/date-b" ]] && break
    sleep 0.1
  done
  [[ -e "${temporary_dir}/date-b" ]] || { out="$(cat "${temporary_dir}/holder-b.out")" fail "(b) the new holder did not reach its record"; return; }

  try_lock "$root"
  if ((rc == 0)); then
    fail "(b) a caller was admitted while the new holder (pid $holder) held the lock"
    return
  fi
  if names "$out" "$previous"; then
    fail "(b) the refusal names the previous holder (pid $previous), which has ended, instead of the live holder (pid $holder)"
    return
  fi
  names "$out" "$holder" || { fail "(b) the refusal does not name the live holder (pid $holder)"; return; }
  kill -TERM "$holder"
  wait "$holder" 2>/dev/null || true
  out=""
  printf 'PASS: a refusal names the live holder, not the ended holder whose record was still in the file\n'
}

case_token_without_descriptor() {
  out=""
  local root="${temporary_dir}/root-c"
  bash "$locker" "$helper" "$root" holder "${temporary_dir}/holder-c" >/dev/null 2>&1 &
  local holder=$!
  pids+=("$holder")
  wait_for "${temporary_dir}/holder-c" || { fail "(c) the holder was not admitted"; return; }
  local token
  token="$(sed -n 's/^token=//p' "${root}/.run.lock")"
  [[ -n "$token" ]] || { fail "(c) the lock file holds no token"; return; }

  try_lock "$root" LOADTEST_LOCK_TOKEN="$token"
  if ((rc == 0)); then
    fail "(c) a caller with the holder's token but without its descriptor 9 was admitted while the holder (pid $holder) runs"
    return
  fi
  names "$out" "$holder" || { fail "(c) the refusal does not name the holder (pid $holder)"; return; }
  kill -TERM "$holder"
  wait "$holder" 2>/dev/null || true
  out=""
  printf 'PASS: a caller with the holder'"'"'s token but without its descriptor 9 is refused\n'
}

failed=0
case_orphaned_child || failed=$((failed + 1))
case_stale_record || failed=$((failed + 1))
case_token_without_descriptor || failed=$((failed + 1))
if ((failed > 0)); then
  printf 'FAIL: %d of 3 cases failed\n' "$failed" >&2
  exit 1
fi
