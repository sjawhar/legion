---
title: "Race regression tests: gate the racing side, hard-assert the precondition, prove the pre-fix failure"
category: testing
tags:
  - regression-tests
  - race-conditions
  - bun-test
  - false-green
  - deterministic-reproduction
  - fail-pre-fix
date: 2026-09-11
status: active
module: daemon
problem_type: testing
component: processes.test.ts
severity: high
applies_when:
  - A reviewer reproduced a concurrency defect and the fix needs a test that fails at the parent commit
  - A test waits for an asynchronous precondition with a bounded poll
  - A defect depends on one exact error string or stderr shape among several similar ones
  - A code path only reaches the bug when a later side effect throws
related_issues:
  - "LEGION-6"
  - "sjawhar/legion#923"
---

# Race Regression Tests: Gate the Racing Side, Hard-Assert the Precondition, Prove the Pre-Fix Failure

## Context

`sjawhar/legion#923` needed regression tests for three defects the reviewer had reproduced by
hand: a kill-pane stderr shape the daemon treated as a real failure (C1), a prune racing an
in-flight launch across two root generations (C2), and a hold leaked when a catch block's side
effect threw (R1). Each fix's test had to fail at the parent commit and pass at the head — and
round 2 found that one of them could pass by accident. These are the techniques that made them
deterministic, all in `packages/daemon/src/daemon/__tests__/processes.test.ts`.

## Techniques

**Gate the racing side with promises you own, in the fake runner.** The C2 test controls the
ordering with three `Promise.withResolvers()` gates inside the fake `run`: generation 1's
`new-window` blocks on `releaseFirstLaunch`; generation 2's `split-window` resolves
`secondLaunchSplitting` and blocks on `releaseSecondLaunch`. The test releases generation 1,
awaits its whole `spawnRoot` (retire + persist + prune), *then* releases generation 2 — so the
prune provably runs while generation 2's pane launch is in flight with no locator in state.
Timing never enters; only the gates do.

**Wait for the observable precondition — then assert it, separately from the outcome.**
`flushEventLoopUntil(cond)` returns silently after its tick budget. The C2 test waited for
`secrets/<architect>` to read `boot-gen-2` and then checked the outcome; had the poll exhausted,
generation 1 would have settled first and the *pre-fix* code passes the same outcome assertions
(round-2 R2). The fix is one line right after the wait:
`expect(readFileSync(architectFile, "utf8")).toBe("boot-gen-2")`. Any bounded wait needs a hard
assertion that the awaited thing actually happened; "did the event occur" and "is the result
right" are two assertions.

**Reproduce the exact shape the bug depends on.** The daemon already treated
`no server running` as pane-gone; the C1 defect was the *other* server-absent shape, `error
connecting to <socket> (No such file or directory)`, which a first boot after the upgrade
runbook (or a reboot clearing `TMUX_TMPDIR`) actually produces — verified with
`tmux -L <absent> kill-pane -t %1` on the box. A fixture using the already-handled string
would have passed pre-fix and proven nothing. Seed the claim so the test takes the path it
describes: `sessionId` + `readyConfirmedAt` → `markWorkerDead → retireWorkerLocator →
stopProcess`; without them the same entry point (`reconnectWorkers`) goes through the
unconfirmed-boot retirement instead (a round-2 nit).

**For a throw-path bug, pick the fixture that makes the side effect throw, then observe the
next unrelated write.** R1: seed `launchFailures: 2` so the third failure crosses
`MAX_LAUNCH_FAILURES` and calls `publishController`; make `natsPublish` throw on the controller
topic; then run `ensureController()` — its persist must reap the architect file. A leaked hold
keeps the file exempt and the `readdir` assertion fails. The assertion is on the *next*
persist's effect, not on the internal counter.

**Prove the pre-fix failure mechanically, and record which assertion failed.** Temporarily
revert the production change (a scripted text swap, run the test, restore) rather than trusting
that the test "would have" failed. R1 pre-fix: `readdir` returned the architect file alongside
the controller's. C2 pre-fix: `ENOENT` from generation 1's prune `rm` racing generation 2's
`writeSecretFile` chmod — a louder failure than the one the reviewer described, which is fine;
what matters is that it fails for the defect's reason. Name that reason in the PR thread reply.

## Anti-flake lessons from the same rounds

- Real fs I/O inside `persist()` (a per-save `readdir`) made fixtures that fake `sleep` (deadline
  timers fire instantly) race the launch chain; the production design changed to avoid the
  listing, not the tests.
- A test that awaited `Bun.sleep(0)` after emitting `idle` broke once the promotion drain's
  persist gained an `await`. Await the persist that records the transition (a `saveState`
  override resolving a promise when the queue is empty and the prompt was sent), never a guessed
  number of ticks.
- The plan's `sleep: async () => {}` override fires the controller registration deadline
  immediately and retires the controller under test; drop the override when the default fake
  RPC client already closes gracefully.

## Related

- `docs/solutions/daemon/refcounted-hold-guards-a-derived-prune.md` — the defects these tests
  guard.
- `docs/solutions/testing/integration-seam-needs-producer-exact-value.md` — same principle
  ("the exact value, not a lookalike") at an integration seam.
