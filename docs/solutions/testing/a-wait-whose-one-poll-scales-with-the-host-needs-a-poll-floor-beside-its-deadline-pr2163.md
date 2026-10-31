---
title: "A wait whose one poll scales with the host needs a poll floor beside its deadline"
category: testing
tags:
  - timeouts
  - bounded-waits
  - flaky-tests
  - e2e
  - shell
  - mutation-testing
date: 2026-10-25
status: active
module: scripts/e2e
related_issues:
  - "INFRA-251"
  - "sjawhar/legion#2163"
---

# A wait whose one poll scales with the host needs a poll floor beside its deadline

A timeout bounded by wall time alone promises attempts it cannot deliver: when one poll costs more
than the whole budget, the wait is a single attempt wearing a deadline's clothes.

- **Ask what a poll costs and who owns that cost.** A poll that reads the machine — a `/proc` walk,
  the process table, a container list, a listing of a shared disk — costs more on a busy host, which
  is exactly the host where the thing waited for is also slower. A poll that reads one row, one file
  or one HTTP endpoint does not; that wait needs nothing from this rule.
- **Give such a wait two bounds and make it spend both.** Give up only when the deadline has passed
  **and** at least N polls have been made. The floor is the mechanism; the deadline is only the
  margin. Raising the deadline alone is a new wager on the next machine.
- **Pass both bounds as arguments with the production values as defaults**, so the real call site
  passes nothing and a test drives small ones. The default is then the value a real run uses and a
  test reads it back through the same function.
- **Say what is still running when the wait gives up.** A wait made patient must not be made vague:
  name the pids, rows or ids that remain, so a genuine hang is still a failure that reads as one.
- **Pin both halves of the conjunction, and prove each pin by mutation.** A case that lets the wait
  succeed late pins the floor and nothing else: a mutant that drops the wall-clock conjunct keeps it
  green. Add a case whose walk never succeeds and whose floor is met at the first poll, asserting
  more than one poll beside the failure. Then cut each conjunct in a scratch copy and check that
  exactly one case reddens.
- **Fix the caller's budget, not the shared helper**, when the helper serves waits whose polls are
  cheap. Widening a helper every proof shares to cure one caller's arithmetic spends every other
  proof's bound.
- **A test that drives a wall-clock bound needs its runner's own timeout raised.** Bun's default
  per-test timeout is 5 s and applies even while a synchronous `Bun.spawnSync` blocks.

Related: `docs/solutions/testing/a-test-only-knob-widens-the-value-production-runs-pin-it-at-the-construction-a-deployment-reaches-INFRA-56.md`
states the sibling rule for a bound a test fights — make it an input, never widen it — and this is
what to make the input when the poll itself is the expensive part.

## Evidence (INFRA-251, sjawhar/legion#2163)

The devbox proof `scripts/e2e/parked-decision-devbox.sh` ended
`parked decision devbox e2e: FAIL (check cleanup-is-complete)` on a developer machine after every
behavioural checkpoint had passed. The wait was
`until_true 30 "every process of the run to end" no_run_processes`, and `until_true`
(`scripts/e2e/lib/rig.sh:70-88`) loops only `while ((SECONDS - started < limit))`. Its poll was
`run_processes` (`rig.sh:183`), a `/proc` walk with a `readlink`, and often a `grep`, per process.
The machine was at load average ≈370 with ≈4,300 processes; one walk cost about 9 s and the first
one outran the whole 30 s budget, so the run failed with `timed out after 30s (1 polls)`. The
processes ended four minutes later. One checkpoint of a forty-minute run was lost to arithmetic,
not to the change under test.

The fix is `wait_for_run_processes [DEADLINE_SECONDS] [MIN_POLLS]`
(`scripts/e2e/parked-decision-devbox.sh`), defaults 600 and 10, called with no arguments, giving up
only on `((SECONDS - started >= limit && polls >= floor))` and naming the pids that remain.
`scripts/e2e/lib/rig.sh` was left alone: `until_true` serves every other proof, whose polls are a
`gh` read or a `jq` over a log.

Two mutations, each run in a scratch copy of the script and its test and each reddening exactly one
case, are what the pins are worth:

| Mutation | Result |
| --- | --- |
| the shape the fix replaced — a deadline with no floor | `FAIL … timed out after 1s (1 polls)`, the devbox failure reproduced |
| the floor drifted to `1` | `FAIL … (1 polls) … still running: 4242` — the floor, not the longer deadline, is what keeps the wait alive |
| the wall-clock conjunct dropped, `((polls >= floor))` | `3 pass, 1 fail`, `Expected: > 1 Received: 1` — found by the tester before the case existed, which is why the fourth case exists |

The fourth case carries an explicit `15_000` ms timeout. Measured with a scratch probe: a test whose
only statement is `Bun.spawnSync(["sleep", "6"])` reports
`(fail) … [5006.27ms]` and `this test timed out after 5000ms`.
