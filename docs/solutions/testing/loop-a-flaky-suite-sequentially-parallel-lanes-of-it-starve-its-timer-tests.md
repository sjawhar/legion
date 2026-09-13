---
title: "Loop a flaky suite sequentially under external load; parallel lanes of the same suite starve its real-timer tests and hide the target"
category: testing
tags:
  - flaky-tests
  - race-conditions
  - reproduction
  - load-testing
  - bun-test
  - real-timers
  - processes.test.ts
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/__tests__/processes.test.ts
applies_when:
  - A CI-only flake must be reproduced before a fix is called proven, and the suite contains tests that wait on real timers or bounded event-loop flushes
  - A parallel-lane loop reports failures, but not in the test the flake was filed against
  - Choosing how to generate load for a before/after race loop
related_issues:
  - "LEGION-55"
  - "sjawhar/legion#1014"
  - "sjawhar/legion#980"
---

# Loop a Flaky Suite Sequentially Under External Load

LEGION-55's Acceptance 1 asked for 50 consecutive runs of
`bun test packages/daemon/src/daemon/__tests__/processes.test.ts` at the fixed head and, as the
negative control, the same loop at the pre-fix parent (`4d516a1f`, the base of
`sjawhar/legion#980`) to show the loop detects the race it was filed about. The tester ran two
shapes on a 32-core host already at load average 110–125 from other agents. Only one of them
detected the target.

| shape | pre-fix parent | fixed head | what the failures were |
| :--- | :--- | :--- | :--- |
| sequential, 50 runs | **49 pass, 1 fail** — run 47: `Incomplete Jujutsu clone at …/repos/github.com/sjawhar/legion: missing …/.jj` | 50 pass, 0 fail | the target race, once |
| 8 parallel lanes × 10 runs (80) | 58 pass, 22 fail — **0** mention the target | 72 pass, 8 fail — **0** mention the target | registration-deadline, boot-watchdog, and resurrection tests waiting on `setTimeout(…, 1)`/`setTimeout(…, 20)` or a bounded `flushEventLoopUntil`, starved by eight concurrent `bun` processes |

The parallel shape looked like a stronger detector — 80 runs, more load — and produced *more*
failures, but every one was noise from the daemon suite's own real-timer tests, and the target
race did not appear once at either revision. The sequential loop, running one `bun` at a time on
a host that was already loaded, caught the race on run 47 at the pre-fix parent and was clean at
the fixed head. That pair is the evidence the acceptance criterion wanted; the parallel pair
proves nothing about the race and would have sent a reader chasing four unrelated tests.

## Why lanes of the same suite are the wrong load generator

The daemon suite has tests whose correctness depends on a timer firing within a bounded window
relative to another operation — a `flushEventLoopUntil(cond)` that gives up after a tick budget,
a deadline armed at `setTimeout(…, 20)` that must land before a fake pane's next event. Running
eight copies of that suite at once multiplies the number of such windows competing for the same
cores; when a window is missed the test fails on *its* assertion, not on the provisioning race.
Those failures are real starvation, but of the wrong thing: they measure how robust the suite's
timer tests are to CPU contention, which is not the question. The target race here was a
filesystem ordering inside one process (two `existsSync` checks against one `mkdir -p`); lane
count does nothing specific to widen it, while it does everything to trip the timer tests.

External load — busy loops and an I/O writer that are not the suite — contends for the scheduler
without adding suite-internal timer windows. The recipe, with the `load=` field that makes two
runs comparable, is in
[widen-the-contender-count-before-calling-a-race-unreproducible](widen-the-contender-count-before-calling-a-race-unreproducible.md).
On this host the ambient load from other agents was already that recipe's effect; the tester
recorded `uptime` before, every ten runs, and after, and the numbers are in the PR body.

## Rules

- **One suite process at a time.** For a flake filed against a suite with real-timer or
  bounded-flush tests, the detector is a sequential loop of the suite's own CI command. Generate
  load *outside* it, never by running the suite in parallel with itself.
- **Read the failures before counting them.** A loop's fail count is meaningless until each
  failing run's `error:`/`(fail)` lines are checked against the target. Report "N failures, 0 of
  them the target" as exactly that — it is a different finding from "N reproductions". The
  tester's PR-body table did this per lane; keep doing it.
- **Detector first, then proof.** Run the loop at the pre-fix revision until it shows the
  target at least once, and record the run number and load; only then does the same loop's clean
  result at the fixed head mean anything. A loop that never showed the race is not evidence the
  race is gone — see [race-regression-tests-that-fail-before-the-fix](race-regression-tests-that-fail-before-the-fix.md).
- **Name the sibling that failed.** The race here surfaced in `admits only up to the configured
  global cap and selects the next queued tree on release`, not in either of the two tests the
  issue named — the issue's own Summary had already recorded the same wandering on `main`. Any
  test in the suite that provisions two trees against one clone directory is a candidate; the
  error string (`Incomplete Jujutsu clone`) is the stable identifier, not the test title.

## Related

- [widen-the-contender-count-before-calling-a-race-unreproducible](widen-the-contender-count-before-calling-a-race-unreproducible.md)
  — the external-load recipe, and widening the *contender* count (not the lane count) when the
  race's window admits more parties than the test supplies.
- [wait-for-a-subprocess-file-by-polling-not-by-watching](wait-for-a-subprocess-file-by-polling-not-by-watching.md)
  — the same host at load ≈245 killed tmux itself inside a 300-run loop; another instance of a
  loop's failures being about the load, not the race.
- [two-contenders-through-a-rename-collision-gate-both-in-wake-on-the-rename-pin-the-winner](two-contenders-through-a-rename-collision-gate-both-in-wake-on-the-rename-pin-the-winner.md)
  — the deterministic unit test that pins the fix this loop proved.
