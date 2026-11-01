---
title: "A linger that stops nothing: document what the transition does (not what expiry does), and start one shared retire in the background after the durable persist"
category: daemon
tags:
  - linger
  - closeTree
  - retireTreeProcesses
  - closingTrees
  - durable-lane
  - resident-processes
  - status-blind-lookup
  - boot-repair
date: 2026-09-18
status: active
module: packages/daemon/src/daemon/processes.ts
related_issues:
  - "LEGION-105"
  - "sjawhar/legion#1187"
symptoms:
  - "legion state --json shows dozens of `lingering` trees that still carry a locator, and role claims on them"
  - "A closed issue's architect is woken, re-registered, or relaunched hours after the issue finished"
  - "ps shows a resident `omp --mode rpc` per finished issue until the linger window ends"
---

# A linger that stops nothing

## What was wrong

`beginLinger` released the admission slot, wrote `lingering` and `lingerUntil`, cleared the
phases, persisted — and stopped nothing. Every process of a finished tree kept running until
`expireLinger` → `closeTree` ran at the end of the linger window (72 hours in production). On the
dogfood box that was 34 resident roots and 207 stale role claims.

The invariant in `packages/daemon/src/daemon/AGENTS.md` read "a lingering or closed root releases
its admission slot, gracefully stops every process recorded for the tree …" — every word true of
the *close*, none of it true of the *linger*. The sentence described what expiry did and was
read as what linger did. When you document a lifecycle transition, name the method that performs
each clause; a clause with no method behind it is a wish.

Three paths were status-blind and kept the resident roots woken:

- `handleException` resolved the role through `rootForIssue`, which walks parents to the nearest
  *tree record* whatever its status — so a `no_holder` on a finished tree's architect re-sent to it
  or resurrected it.
- `/process/ready` re-confirmed a lingering tree's root when the extension's heartbeat regained the
  role, publishing an overseer catch-up to it.
- `resurrectDeadTree` refused only `queued`; a lingering or closed tree reached the spawn.

## The fix shape

One primitive, `retireTreeProcessesLocked(treeKey)`: everything `closeTreeLocked` did to the
processes (root leg with `probeTree` + `stopProcessSerialized`, the worker fixed-point loop over
`inFlightLaunches`, the locator-less claim sweep, `clearTreePhases`, `pruneQueueForTree`, the
`spawnCapabilities` deletion, one persist) and nothing it did to the record (no status, no
`lingerUntil`, no workspace removal, no `done` write). It moves the root's `ompSessionFile` into
`TreeState.resumeSessionFile` so a `todo` inside the window resumes the same architect;
`closeTreeLocked` deletes that field again. Three callers: `closeTreeLocked` inline; the public
`retireTreeProcesses` (linger, a root's self-exit, the sweep); `retireLingeringTrees` (sweep tick
and boot pass, every lingering tree that `treeRecordsProcesses` says still records a locator or a
claim, all in parallel, never rejecting).

`closingTrees` became `Map<IssueKey, { kind: "retire" | "close"; settled: Promise<void> }>`.
Every `.has` reader keeps one meaning — a teardown of this tree's processes is in flight — and
`closeTree` loops: join whatever is in flight; a `close` → return; a `retire` → proceed to close,
but only if the tree is still `lingering` afterwards (a `todo` can land during the join). The
`settled` promise never rejects; the starter alone sees `StopFailed`.

`beginLinger` persists first, then `void this.retireTreeProcesses(treeKey).catch(log)`. Never
awaited inside the durable lane: a stop can take `tree_stop_timeout_seconds` and can fail, and a
throw in `onLinger` is `events.ts`'s `fatal` (the daemon exits). The retire is idempotent and
restart-safe by *rediscovery*, not by persisted teardown state: `closingTrees` is memory, and a
crash between the persist and the stop leaves a lingering tree with live processes that the next
sweep tick or the boot pass (`retireLingeringTrees` after `reconnectWorkers`, before
`enableLaunches()` — `runningWorkerCount()` counts every claim with a locator, so promotion must
not decide against finished trees' stale claims) finds again through `treeRecordsProcesses`.

Then the guards, each one line and a return: `resurrectDeadTree` refuses every status but
`active`/`dead`; `handleException` returns for a role on a lingering or closed tree before any
probe, ledger claim, or re-send; `/process/started` and `/process/ready` 409 a non-active tree
before the boot token is consumed; `isTreeGone` treats `lingering` like `closed`;
`emitOverseerCatchup` publishes only for `active`.

## Rules a future worker needs

1. **`closingTrees` is a convention, not a type.** `markProcessDead`, `recordRootExit`,
   `isTreeGone`, `awaitClosingTrees`, `retireUnconfirmedBoot`, `spawnWorker`, `workerReady`, and
   `readyClaim` all branch on it, and nothing fails to compile when a new path that stops,
   resurrects, or writes a claim for a tree forgets to. Any new mutator of a tree's processes
   consults `closingTrees` first — before the probe, before the write.
2. **`treeRecordsProcesses` is status-blind on purpose.** It answers "does this tree still record a
   locator or a claim"; `retireLingeringTrees` filters to `lingering` before calling it. Reused
   from another site without that filter it would retire an active tree's processes.
3. **The log lines are the operator's evidence.** One header per pass that found anything
   (`retiring the processes of <n> lingering tree(s) …`), one per tree that retired something or
   failed (`retired the processes of lingering tree <tree>: root <stopped|failed|none>, <n>
   worker(s) stopped, <n> claim(s) removed`), nothing for a tree whose retire joined a close.
   The summary line is emitted by the *entry point* `retireTreeProcesses`, not the primitive, so a
   close stays as quiet as before (an existing test pins that a close of an already-reaped pane
   logs nothing).
4. **A root that will not stop keeps its claim beside its locator.** The primitive's locator-less
   claim sweep skips the architect token when the root leg failed (`rootOutcome === "failed"`):
   locator and claim leave together on the retire that finally stops it, and until then the
   reopen's resumed launch still knows the session to expect. The first draft deleted the claim
   and left the sentence "keeps its locator and claim" false for the root; the reviewer read the
   paragraph against the code sentence by sentence.
5. **`closeTreeLocked` re-marks `lingering` with `lingerUntil = now` only on a `StopFailed`.** Any
   other rejection from the primitive (its own persist failing) propagates untouched, exactly like
   a rejected save anywhere else — the crash-safety test (`marks a tree lingering as its first
   durable act …`) counts saves and catches a catch-all.

## Related

- [`a-close-fence-awaited-downstream-must-clear-before-the-close-hands-on-its-slot.md`](./a-close-fence-awaited-downstream-must-clear-before-the-close-hands-on-its-slot.md)
  — the `closingTrees` fence and why the promotion sweep runs only after the map forgets the tree;
  its LEGION-143 note (a reopened root killed by the close) is what the joined-close re-check now
  prevents on the retire path.
- [`a-dead-worker-is-relaunched-from-one-death-path-decided-under-the-retirement-lock-with-no-flag-for-daemon-stops.md`](./a-dead-worker-is-relaunched-from-one-death-path-decided-under-the-retirement-lock-with-no-flag-for-daemon-stops.md)
  — `decideWorkerRelaunch`'s explicit `lingering` clause became redundant once `isTreeGone` covers it.
- [`a-kept-field-changes-every-gate-keyed-on-it-test-the-reopen-a-linger-change-exists-for.md`](./a-kept-field-changes-every-gate-keyed-on-it-test-the-reopen-a-linger-change-exists-for.md)
  — the two reopen-path defects the review found in this change.
