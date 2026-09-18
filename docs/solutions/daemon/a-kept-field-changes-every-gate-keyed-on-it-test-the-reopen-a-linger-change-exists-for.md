---
title: "A change that keeps a field changes every gate keyed on it: test the reopen the linger change exists for, not only the linger it fixes"
category: daemon
tags:
  - resumeSessionFile
  - reopen
  - re-admission
  - in_progress
  - markProcessDead
  - closingTrees
  - admittedIssues
  - review-round
  - fingerprint
date: 2026-09-18
status: active
module: packages/daemon/src/daemon/processes.ts
related_issues:
  - "LEGION-105"
  - "sjawhar/legion#1187"
symptoms:
  - "A finished issue moved back to todo stays todo on the board through implementation; the implementer's completion cannot move it to testing"
  - "legion state --json shows a re-admitted tree `dead` with an empty admission.active seconds after the reopen (or `dead` inside admission.queue at cap)"
  - "The unchanged-diff fingerprint differs after a conflict-forced rebase that also carried fixes"
---

# A change that keeps a field changes every gate keyed on it

## The two defects the review found

LEGION-105 made the linger retire keep the root's transcript path as `TreeState.resumeSessionFile`
so a human's `todo` inside the window resumes the same architect. The unit suite, the real-tmux
E2E, and two scratch daemons all proved the linger half and none of them drove the reopen. The
reviewer did, with a throwaway test and by reading the launch path, and found two defects on the
path the change exists to support:

1. **The exit report during the retire's shutdown window.** `beginLinger` now stops the root, and
   the root's own `/process/exit` arrives seconds later (up to `tree_stop_timeout_seconds` when it
   is mid-turn). A `todo` in that window: `admit` takes a slot and writes `active`, `spawnRoot`
   waits on the retire (`awaitClosingTrees`) before bumping the generation. The old root's exit
   report therefore carries the current generation with the tree `active` and the issue `todo`;
   the route's "finished" branch is false and it reaches `markProcessDead` →
   `recordRootExit(tree, "dead")`, which marks the tree `dead` and releases the slot the
   re-admission just took. At cap: `dead` inside `admission.queue`, which the promotion sweep skips
   — the reopen stalls until resync. Fix: `markProcessDead` returns after one log line while
   `closingTrees` names the tree, mirroring its `stoppingForRelaunch` guard — the teardown's root
   leg owns the locator, `beginLinger` already released the old slot. (A root that genuinely dies
   mid-retire is not lost: its socket closing confirms the retire's own stop.)
2. **The kept transcript turned the reopen into a `--resume` launch.** `spawnRoot` derives
   `resuming = resume || tree.resumeSessionFile !== undefined`, and `spawnTree` wrote
   `in_progress` only `if (!resume)` — the daemon's *only* root `in_progress` write. Before the
   change a lingering tree kept no session file, so a `todo` re-admission launched fresh and wrote
   the status. After it the resumed architect ran while Dispatch kept the issue at `todo`;
   `phaseCompleteStatus` moves implementer → `testing` only from `in_progress`, so the issue read
   `todo` until the tester's unconditional `needs_review`.

## The lesson

`!resume` had been standing in for "this launch is an admission" because the two never disagreed:
a launch that resumed was always a resurrection. Keeping `resumeSessionFile` through linger created
the combination *admitted AND resuming*, and every gate keyed on the resume flag silently changed
meaning. When a change makes a record keep a field it used to clear, list every reader of that
field and every boolean derived from it, and ask of each: does this flag now conflate two things
that used to always agree? Then write the new combination as an explicit test scenario — here
"linger → retire → `todo` → resumed launch with `--resume` **and** `in_progress` written" — rather
than trusting the existing resume tests (resurrection, writes nothing) and admission tests (fresh,
writes `in_progress`), neither of which covers it.

The fix threads a second, independent boolean: `ProcessManager.admittedIssues`, in memory, marked
by `admit()` on every path that launches or queues (never for a tree already in its slot), read by
`spawnRoot`, passed to `spawnTree` beside `resume`; the write is `admitted || !resume`. Consumed by
the launch that lands or ends `launch-failed`, kept across a below-threshold retry, dropped with a
dequeued entry, and — because memory does not survive a restart — re-marked by `reconcileAdmission`
for every queued issue whose Dispatch status is `todo` (a queued issue at `todo` is one awaiting
admission by the reducer's own definition; a queued resurrection's issue reads its lifecycle
status and stays unmarked). No schema change.

And the second half of the lesson: **test the path the change exists to support.** The spec's
requirement was "keep the transcript so a reopen resumes the same architect"; the tests proved the
transcript was kept and never proved the reopen. A feature that exists so that X works needs X
driven end to end — direct and at cap — as its first test, before the mechanism tests.

## Two hardening threads in the same functions

- The retire deletes worker claims (freeing `worker_cap` slots) but only `closeTreeLocked` called
  `promoteWorkerQueue()`; the socket-close path cannot fill in because `stopProcess` evicts the
  cached client before closing it. One line after `await retiring`, once `closingTrees` has
  forgotten the tree.
- A `closeTree` that joined an in-flight retire re-entered `closeTreeLocked` without re-checking
  the tree: a `todo` landing during the join was re-lingered, retired again, written `done` over
  the human's `todo`, and closed. It now closes only a tree still `lingering` after a join; a
  direct call on a not-yet-lingering tree keeps its contract (mark lingering first, then close).

## Test-harness facts the corrective round cost

- A held shutdown frame must be released *per leg*: the root leg is awaited before the worker leg,
  so a fixture holding every `client.close()` behind one gate deadlocks the primitive; release the
  first, wait for the second frame, release it. And hold only the lingering root's own frame (the
  first) — a relaunched root a wrong path asks to exit must answer at once, so the wrong path fails
  on an assertion instead of hanging on the stop timeout.
- The promoted worker launch does real workspace I/O (`jj git clone` into the scratch state dir)
  before its `new-window`; await the fixture's `runs("new-window").completed.next()`, not a tick
  budget — see
  [`../testing/tick-budgets-over-real-io-flake-await-the-event-the-production-path-emits.md`](../testing/tick-budgets-over-real-io-flake-await-the-event-the-production-path-emits.md).

## A corrective push that rides a conflict-forced rebase records three fingerprints

The rebase was Sami's one allowed kind (GitHub `CONFLICTING`, #1186 had touched the same
`AGENTS.md` row). Because the same push carried the fixes, the skill's `fingerprint <a> → <b>;
changed` comment cannot by itself tell the reviewer what changed the hash. Record three:

1. before the rebase, at the old tip;
2. after the rebase alone, at the rebased tip *before any fix commit* — and interdiff it against
   (1) so the only differing lines are the conflict's (here one `AGENTS.md` row whose base text
   `main` had edited, resolved by applying `main`'s edit inside the branch's row; every code file's
   added and removed lines byte-identical);
3. at the pushed head, naming the files the fixes touch.

The reviewer then compares (2) to its own last verified head and reviews (3) − (2) as the round.
The general rule that a conflict-forced rebase may change the fingerprint is in
[`../legion/sibling-pr-rewrites-your-function-spell-both-shapes-in-the-plan-and-expect-the-fingerprint-to-change.md`](../legion/sibling-pr-rewrites-your-function-spell-both-shapes-in-the-plan-and-expect-the-fingerprint-to-change.md);
this is the addition for a push that also carries a review's fixes.

## Related

- [`a-linger-that-stops-nothing-document-the-transition-not-the-expiry-and-start-the-shared-retire-after-the-durable-persist.md`](./a-linger-that-stops-nothing-document-the-transition-not-the-expiry-and-start-the-shared-retire-after-the-durable-persist.md)
  — the change these defects were found in.
- [`a-resume-the-same-agent-guarantee-is-kept-by-session-identity-and-the-resume-reference-must-survive-a-refused-generation.md`](./a-resume-the-same-agent-guarantee-is-kept-by-session-identity-and-the-resume-reference-must-survive-a-refused-generation.md)
  — the other place `resumeSessionFile`'s lifetime was extended, and the same kind of gate audit.
- [`dispatch-status-writes-one-writer-per-transition-and-the-pending-write-fence.md`](./dispatch-status-writes-one-writer-per-transition-and-the-pending-write-fence.md)
  — why the root `in_progress` write has exactly one site, which is what made `!resume` load-bearing.
