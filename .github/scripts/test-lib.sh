#!/usr/bin/env bash
# Sourced by the `.test.sh` harnesses beside it (release-push, check-bun-version): the counters,
# the per-case reporter, and the two predicates they both spell the same way. Each harness keeps
# its own fixtures — what they share is only how a case is reported and counted.
#
# A harness sources this, runs its cases with `check <description> <true|false>`, and ends with
# `summary <what was tested>`, which prints the tally and exits non-zero if anything failed.
#
# Not executable: `source "$(dirname "${BASH_SOURCE[0]}")/test-lib.sh"`.

PASS=0
FAIL=0

check() { # check DESCRIPTION true|false
  local desc="$1" result="$2"
  if [ "$result" = "true" ]; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc"
    FAIL=$((FAIL + 1))
  fi
}

# is <actual> <expected>: true/false for check.
is() { [ "$1" = "$2" ] && echo true || echo false; }

# contains <text> <regex>: true/false for check; greps the whole text.
contains() { printf '%s\n' "$1" | grep -q -- "$2" && echo true || echo false; }

summary() { # summary WHAT-WAS-TESTED
  echo
  echo "---"
  echo "Results: $PASS passed, $FAIL failed"
  if [ "$FAIL" -gt 0 ]; then
    exit 1
  fi
  echo "PASS: $1"
}
