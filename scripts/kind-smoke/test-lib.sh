#!/usr/bin/env bash
# Sourced by up.test.sh, down.test.sh, and checkpoints.test.sh. Under `set -e` bash exempts two
# shapes from errexit (bash manual, "set -e"; shellcheck SC2251): a command inverted with `!`, and
# every operand of an `&&`/`||` list except the last. A bare `! grep -Fq canary "$log"` therefore
# never fails a harness, and in `[ -f a ] && [ -f b ]` only the second test can. So:
#   - every negative assertion is `refute CMD…` — CMD succeeding fails the harness, naming the line;
#   - every positive assertion is one bare simple command on its own line, and the ERR trap names
#     the file, line, and command that failed (bash exits on it; `-E` carries the trap into
#     functions);
#   - a command whose non-zero exit is the point (`run_up … || status=$?`) sits in an `||` list.
set -Eeuo pipefail
trap 'printf "FAIL %s:%s: %s\n" "${BASH_SOURCE[0]}" "$LINENO" "$BASH_COMMAND" >&2' ERR

refute() { # refute CMD… — CMD must fail; its success is a harness failure naming the caller's line
  if "$@"; then
    printf 'FAIL %s:%s: expected failure, but succeeded: %s\n' "${BASH_SOURCE[1]}" "${BASH_LINENO[0]}" "$*" >&2
    exit 1
  fi
}
