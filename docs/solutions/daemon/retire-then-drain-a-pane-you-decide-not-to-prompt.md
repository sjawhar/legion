---
title: "A freshly booted pane you decide not to prompt must be retired, then the worker queue drained, with the boot confirmed before the stop that can throw"
category: daemon
tags:
  - worker-lifecycle
  - workerCap
  - worker-admission
  - retire
  - StopFailed
  - critical-section
  - processes.ts
date: 2026-09-13
status: active
module: daemon
related_issues:
  - "LEGION-37"
  - "sjawhar/legion#991"
symptoms:
  - "`spawn_worker` answers `queued` at the cap while a pane that was never prompted sits at `runState: unknown`"
  - "a queued spawn is promoted only on the next linger sweep (tens of seconds) although a slot freed earlier"
  - "after a `StopFailed`, every later `spawn_worker` for the role queues its task behind a boot that never reaches ready again"
---

# A freshly booted pane you decide not to prompt must be retired, then the worker queue drained, with the boot confirmed before the stop that can throw

## Context

#991's ready-time bystander rule (`workerReady`, `processes.ts`): a `catchup` queued for a role
that stopped being the active phase while its relaunched pane booted is dropped instead of
prompted. The first version dropped the prompt and confirmed the boot. Three review/test rounds
turned that one line into the sequence below; each step looked unnecessary from the previous
one's vantage and each has a regression test that fails without it. The sequence applies to any
future path that boots a pane and then decides it has nothing to say to it.

## Step 1 — a never-prompted pane is not free: retire it

A fresh `omp --mode rpc` that is never prompted emits no `agent_end`, and `clientFor` never asks
`get_state`, so its cached client sits at `runState: "unknown"` — counted against `workerCap` by
`runningWorkerCount`, and never reached by `armIdleRetire`, which fires only from an idle
transition. The reviewer reproduced it in the unit harness: `runState=unknown`, `getStateCalls=0`,
no idle-retire armed, `spawnWorker(reviewer)` → `queued` at cap 1. The pane would have held the
slot until some unrelated probe happened to seed its state.

So the drop retires the pane on the spot, exactly as `retireIdleWorker` retires a finished worker:
`retireWorkerLocator` (graceful `shutdown` frame, kill-pane fallback), locator cleared,
`ompSessionFile` carried into `resumeSessionFile` so the architect's next `spawn_worker` resumes
the same agent with `--resume`, `readyConfirmedAt` set and `launchFailures` reset because the boot
itself did succeed, one `persist`. Not a bare "clear the locator": the process must actually stop.

## Step 2 — a retirement outside the client-close path must drain the queue itself

Every other retirement gets its queue drain for free: the retired client's socket closes →
`onWorkerClientClosed` → `markWorkerDead` → `promoteWorkerQueue()`. This pane was never connected
through `clientFor`, so no cached client exists to close, and the slot it freed in state was
handed to the queue only by the next 60 s linger sweep's `reconcileWorkerAdmission`. The tester's
rig measured 5.3 s and 33.3 s from retire to promotion — both on the sweep tick, with the other
slot-holder still busy.

`workerReady` now sets a flag inside the critical section and calls
`this.workerAdmission.promoteWorkerQueue()` **after** `mutateClaim` resolves, exactly as
`markWorkerDead` does (outside the lock, fire-and-forget, only on the retire path). Rig after: the
queued tester's pane started 1.0 s after the retire persisted. Unit lock: the ready test queues a
spawn behind the cap *before* ready arrives and awaits the promotion's own `worker-started`
publish with no sweep call; it times out with the drain line removed.

Ask of any new path that frees a `workerCap` slot: "whose socket close drains the queue for this?"
If the answer is "nobody's," call the drain.

## Step 3 — confirm the boot in memory *before* the stop that can throw

`retireWorkerLocator` may throw `StopFailed` (a real `kill-pane` failure on a pane that may still
be alive — not the routine "already gone" race), and by then it has already cancelled the token's
boot watchdog. With the confirmation written *after* the stop, that failure left a claim that
looked like a boot still in flight with no watchdog left to judge it, and every later
`spawn_worker` for the role queued its task on the booting branch for a `/worker/ready` that
would never come again.

The order is now: `delete claim.pendingAssignment; claim.readyConfirmedAt = now(); delete
claim.launchFailures;` → `await retireWorkerLocator(…)` → locator clear/carry → `persist`. A
`StopFailed` then leaves exactly `retireIdleWorker`'s own StopFailed shape — locator intact, boot
confirmed, catch-up gone — which the next `spawn_worker` recovers through the live-claim probe
path. Nothing is persisted until after the stop, so a daemon restart re-derives "unconfirmed" from
disk and `reconnectWorkers` re-arms the watchdog; that case recovers too. The redundant
post-retire `cancelBootWatchdog(token, generation)` went with it (`retireWorkerLocator` cancels
every generation).

General form: in a critical section whose cleanup can throw, write the state that makes the record
recoverable *before* the risky `await`, not in the success path after it. A reviewer's flag: any
`await someStop()` followed by state writes that run only on success — ask what the record looks
like if the stop throws, and who recovers it.

## How each step was found

Unit suites passed at every round. Step 1 came from the reviewer reading the code and reproducing
in the unit harness; step 2 from the tester timing the rig against the sweep tick; step 3 from the
reviewer reading the error path. None of the three is visible to a test that asserts only the
happy path of the drop.

## Tests

`processes.test.ts`: `retires a relaunched worker at worker/ready when its only queued prompt is a
bystander's catch-up, freeing its cap slot and promoting a spawn already queued behind it, while an
assignment in the same position is still delivered` (fails at the `shutdown`-frame assertion with
the retire replaced by a bare clear; times out with the drain removed) and `confirms the boot before
the ready-time bystander retire, so a StopFailed leaves a confirmed live claim the next
spawn_worker probes instead of queueing behind a boot forever` (shim unreachable at stop,
`kill-pane` exit 1 with a non-gone stderr; fails at `readyConfirmedAt` with the round-3 order).

## Related

- `one-writer-for-the-active-phase-and-bystander-catchups.md` — why the catch-up is dropped.
- `idle-expiry-timers-rearm-on-declines-that-change-while-idle.md` — the retire path this one
  mirrors, and why `armIdleRetire` cannot see a never-prompted pane.
- `../testing/mutation-proof-probe-tests.md` — the tester's break-then-fix checks that pinned
  steps 1–3 as separate guards.
