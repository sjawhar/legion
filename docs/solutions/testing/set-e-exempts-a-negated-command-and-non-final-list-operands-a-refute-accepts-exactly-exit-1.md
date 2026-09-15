---
title: "set -e exempts a `!`-inverted command and every non-final `&&`/`||` operand: a bash harness's negatives need a `refute` that accepts exactly exit 1"
category: testing
tags:
  - bash-harness
  - set-e
  - errexit
  - shellcheck
  - secret-leak
  - smoke-rig
  - kind-smoke
date: 2026-09-15
status: active
module: scripts/kind-smoke (test-lib.sh, up.test.sh, down.test.sh, checkpoints.test.sh)
applies_when:
  - Writing a bash test harness under `set -e` whose assertions include "X must not happen"
  - A harness passes and you have not planted the thing an assertion exists to catch
  - `shellcheck` reports SC2251 anywhere in a test script
related_issues:
  - "LEGION-26"
  - "sjawhar/legion#1114"
---

# set -e exempts a `!`-inverted command and every non-final `&&`/`||` operand

The kind smoke's three harnesses (`up.test.sh`, `down.test.sh`, `checkpoints.test.sh`) were green
for three rounds while most of their negative assertions could not fail. Each round the tester
found one more shape bash exempts from `errexit`; the third one had disarmed every secret-leak
lock in the suite. The fix is one shared file, `scripts/kind-smoke/test-lib.sh`, and one rule per
assertion shape.

## The three shapes, in the order they were found

1. **A bare `! cmd` never fails a `set -e` script.** The bash manual's `set -e` text exempts a
   pipeline inverted with `!`; shellcheck flags it as SC2251. `! grep -Fq canary "$log"` on its own
   line is a no-op: `bash -euo pipefail -c 'echo canary >f; ! grep -Fq canary f; echo REACHED'`
   prints `REACHED` and exits 0. The tester counted 24 such lines across the three harnesses and
   proved it by planting a `docker run --token anthropic-canary-value` line in the fake call log:
   `up.test.sh: OK`.
2. **Only the last operand of an `&&`/`||` list can fail the script.** The survivor after the
   first fix was `[ -f "$pid" ] && [ -f "$start" ]` in a loop: with `.pid` missing and `.start`
   present the script continues (the first operand is exempt); only a missing `.start` stops it. So
   a `start_process` regression that wrote no pid file passed.
3. **A `refute` that accepts any non-zero exit is disarmed by the command erroring.** The first
   `refute CMD` treated every non-zero status as the expected negative. `grep -Fq <secret> <file>`
   exits 2 when `<file>` does not exist, so a renamed or unwritten runner-output file made every
   secret-leak refute pass silently — the tester planted `rm -f "$tmp/last.txt"` right before one
   and got `grep: …/last.txt: No such file or directory` on stderr followed by `up.test.sh: OK`,
   exit 0.

## The rules `test-lib.sh` fixes

```bash
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
```

- Every negative assertion is `refute CMD…`, and the negative is **exactly exit 1** — the "no"
  every refuted shape gives: grep found no match, `kill -0` on a dead pid or pgid, a false `[ ]`.
  Exit 0 fails the harness; any other status fails it too, because that is an error, not a
  negative. `${BASH_SOURCE[1]}:${BASH_LINENO[0]}` names the caller's line, not `refute`'s.
- Every positive assertion is **one bare simple command on its own line**. No `A && B` chains;
  split them. The ERR trap names the file, line, and command that failed; `-E` carries it into
  functions.
- A command whose non-zero exit is the point sits in an `||` list on purpose:
  `run_up … || status=$?`, then assert on `$status`.
- The check that the rules hold: `shellcheck` SC2251 count is 0 across the harnesses, and every
  lock has been shown to bite at least once — plant the canary (a secret value in the argv log, a
  `rm -f` of the file a refute reads) and read the `FAIL <file>:<line>: …` line before removing
  the plant.

Two smaller facts met on the way:

- `grep -Fq '' <file>` exits 0, so `refute grep -Fq "$(cat <secret file>)" …` with an empty or
  missing secret file reports `expected failure, but succeeded` — loud, misleading text, never a
  silent pass. Strip blank lines from a `grep -f` pattern file and assert a minimum pattern count.
- `refute grep … <(tail -n +N "$FAKE_LOG")` cannot tell a missing `$FAKE_LOG` from no new lines:
  the process substitution swallows `tail`'s exit and grep exits 1. Use that shape only where the
  file is one the harness itself created.

## Related

- `docs/solutions/testing/bash-harness-cases-that-pass-for-the-wrong-reason.md`: the two other
  errexit holes (`set -e` is off inside an `if` condition; `local x="$(cmd)"` masks the
  substitution's failure) and the mutate-and-watch method.
- `docs/solutions/testing/harness-that-sources-a-copy-pins-its-root-fails-closed-on-success-and-proves-absence-with-a-call-log.md`:
  proving "never calls X" with a call log.
