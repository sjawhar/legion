---
title: "Widen the contender count before calling a race unreproducible; reproduce and prove under a recorded load recipe"
category: testing
tags:
  - race-conditions
  - reproduction
  - load-testing
  - flaky-tests
  - before-after-evidence
  - bun-test
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/__tests__/instance-lock.test.ts
applies_when:
  - A concurrency defect is described by a spec or reviewer and the existing race test passes N of N
  - A race test reproduced for one agent and not for the next on the same host
  - A PR needs before/after evidence for a race fix that a reviewer can re-run
  - A fix's regression test must be shown non-flaky before merge
related_issues:
  - "LEGION-35"
  - "sjawhar/legion#1004"
---

# Widen the Contender Count Before Calling a Race Unreproducible

## What happened

The pre-fix instance-lock race test raced **two** contenders against a stale pid file, 25 rounds
per run. Under the load recipe below, on one 32-core workstation already at load ≈110 from other
agents, the same day:

| who | contenders | result |
| :--- | :--- | :--- |
| planner | 2 | 49 pass, 1 fail of 50 |
| implementer | 2 | 49 pass, 1 fail of 50 |
| tester | 2 | 100 pass, 0 fail of 100 |
| tester | **3** | **32 pass, 18 fail of 50** |

Two contenders were a marginal reproduction — real (both single failures were the two-winner
outcome, `Expected length: 1 / Received length: 2` on the fulfilled promises) but not
reproducible on demand. Three contenders reproduced the defect in 36% of runs.

The third contender is not arbitrary. The pid-file protocol's window was: contender A reads a dead
pid and renames the file aside *after* contender B has already created a live lock; until A's
restore the name is absent, and an exclusive create by a **third** contender succeeds. The window
admitted exactly one more participant than the test supplied. The spec anticipated this and
prescribed the widening; the fixed test runs three contenders permanently.

**Rule:** before reporting a race as unreproducible, add the contender the suspected window
admits. Read the protocol, count the parties its bad interleaving needs, and race that many.
Ask the spec or architect to name the widening up front so the tester does not have to discover it.

## The load recipe (record it with the result)

A race that never fires at idle fires under scheduler contention. The recipe used here, as a
script taking `<workdir> <iterations> <busy-loops> <bun test args…>`:

- `<busy-loops>` (use `nproc`) copies of `nice -n 10 bash -c 'while :; do :; done'`
- one `nice -n 10` writer looping `dd if=/dev/zero of=<tmp>/io.bin bs=1M count=64 conv=fsync`
- a `trap … EXIT` that kills them all
- `bun test <args>` repeated `<iterations>` times, counting a run as passing only when its output
  has both ` 0 fail` and a `N pass` line (an early crash prints neither)
- for each failing run, print the `error:`, `Expected`/`Received`, `(fail)`, and `at …` lines
- one final `RESULT: <pass> pass, <fail> fail of <n> (busy=<b>, load=<cut -d' ' -f1-3 /proc/loadavg>, wall=<s>s)`

The `load=` figure is the host's *actual* condition, not the recipe's nominal one — this machine
ran other agents' work throughout, and the number is what makes two results comparable. Save the
full output to a file (`before.txt`, `after.txt`) and paste the `RESULT` line plus one failing
block into the PR body under the acceptance criterion it satisfies.

## Before/after discipline

- **Before** must be the exact pre-fix module *and* the pre-fix test. After the fix lands, extract
  both from the base revision (`jj file show -r <base> <path>`) into a scratch directory rather
  than editing the working copy back; the tester did this to re-check the planner's figures.
- **After** runs the *whole* new test file, not just the race test, under the same recipe: the
  fix's other tests are new and their flakiness is what you are looking for. LEGION-35's first
  after-run was 50/50 and still hid a 2% flake in a different test that only a later 150-run and
  two full package suites exposed (see
  `docs/solutions/testing/bun-spawn-vfork-window-wait-for-the-child-to-pass-exec.md`). Budget at
  least 100–150 file iterations and two full-suite runs for a concurrency change.
- **Red set first.** Run the rewritten test file against the unchanged module and record which
  tests fail (here 6 of 12) before replacing the module; the list is the proof that each new test
  defends something the old code got wrong.

## Related

- `docs/solutions/testing/race-regression-tests-that-fail-before-the-fix.md` — making a race
  test deterministic with owned gates when the interleaving can be controlled from a fake runner;
  this document is for the cases where it cannot (kernel scheduling, real processes).
- `docs/solutions/daemon/instance-lock-is-a-kernel-flock-not-a-pid-file.md` — the defect and the
  kernel-lock fix these runs bracket.
- `docs/solutions/testing/loop-a-flaky-suite-sequentially-parallel-lanes-of-it-starve-its-timer-tests.md`
  — why the load recipe above is external busy loops and not parallel lanes of the suite itself:
  lanes starve the suite's real-timer tests and hide the target race.
