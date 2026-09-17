#!/usr/bin/env bash
# Sourced by up.test.sh, down.test.sh, and checkpoints.test.sh. Under `set -e` bash exempts two
# shapes from errexit (bash manual, "set -e"; shellcheck SC2251): a command inverted with `!`, and
# every operand of an `&&`/`||` list except the last. A bare `! grep -Fq canary "$log"` therefore
# never fails a harness, and in `[ -f a ] && [ -f b ]` only the second test can. So:
#   - every negative assertion is `refute CMD…` — CMD's exit status must be exactly 1, the "no"
#     every refuted shape gives (grep found no match, `kill -0` on a dead pid or pgid, a false
#     `[ ]`); CMD succeeding fails the harness, and so does any other non-zero status, because that
#     is an error, not a negative: grep exits 2 for a missing or unreadable file, so a renamed
#     runner output file would otherwise disarm every secret-leak refute with a green harness;
#   - every positive assertion is one bare simple command on its own line, and the ERR trap names
#     the file, line, and command that failed (bash exits on it; `-E` carries the trap into
#     functions);
#   - a command whose non-zero exit is the point (`run_up … || status=$?`) sits in an `||` list.
set -Eeuo pipefail
trap 'printf "FAIL %s:%s: %s\n" "${BASH_SOURCE[0]}" "$LINENO" "$BASH_COMMAND" >&2' ERR

refute() { # refute CMD… — CMD must exit 1; 0 is a failed negative, anything else is CMD erroring
  local status=0
  "$@" || status=$?
  case "$status" in
    1) return 0 ;;
    0) printf 'FAIL %s:%s: expected failure, but succeeded: %s\n' "${BASH_SOURCE[1]}" "${BASH_LINENO[0]}" "$*" >&2 ;;
    *) printf 'FAIL %s:%s: refuted command errored (exit %s): %s\n' "${BASH_SOURCE[1]}" "${BASH_LINENO[0]}" "$status" "$*" >&2 ;;
  esac
  exit 1
}

assert_eq() { # assert_eq ACTUAL EXPECTED
  [ "$1" = "$2" ] || {
    printf 'FAIL %s:%s: expected %q, got %q\n' "${BASH_SOURCE[1]}" "${BASH_LINENO[0]}" "$2" "$1" >&2
    exit 1
  }
}
assert_ge() { # assert_ge ACTUAL MINIMUM
  [ "$1" -ge "$2" ] || {
    printf 'FAIL %s:%s: expected %s >= %s\n' "${BASH_SOURCE[1]}" "${BASH_LINENO[0]}" "$1" "$2" >&2
    exit 1
  }
}
assert_file() { [ -f "$1" ] || { printf 'FAIL %s:%s: expected file %s\n' "${BASH_SOURCE[1]}" "${BASH_LINENO[0]}" "$1" >&2; exit 1; }; }
assert_no_file() { [ ! -e "$1" ] || { printf 'FAIL %s:%s: expected no file %s\n' "${BASH_SOURCE[1]}" "${BASH_LINENO[0]}" "$1" >&2; exit 1; }; }
assert_grep() { grep -Eq -- "$1" "${2:--}"; }
# shellcheck disable=SC2154  # Each harness selects its own global $state directory.
assert_record() { # assert_record NAME VALUE — $state is the test's selected rig state directory
  assert_file "$state/records/$1"
  assert_eq "$(<"$state/records/$1")" "$2"
}
assert_pid_live() { # assert_pid_live NAME — the pid and start-time record still name a live process
  local pid
  assert_file "$state/pids/$1.pid"
  assert_file "$state/pids/$1.start"
  pid="$(<"$state/pids/$1.pid")"
  kill -0 "$pid"
  assert_eq "$(awk '{print $22}' "/proc/$pid/stat")" "$(<"$state/pids/$1.start")"
}
pid_live() { kill -0 "$1" 2>/dev/null; }
