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
  - StopFailed
  - stopRetainedRootProcess
  - deep-review
  - fault-injection
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
  - "A todo on a lingering tree whose retire could not confirm the root stopped opens a second root pane and a fresh architect while the old pane is still alive; the tree's locator now names the new pane and nothing names the old one"
  - "A test that launches a root over the tree() fixture's default locator fails with `Refusing to start … while resurrecting: recorded OMP session file is missing`"
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

## Round 2: the blocker two scratch daemons and 31 tests missed

After the reviewer approved the `.legion/` deletion head and retro landed, the merge queue's own
adversarial deep review (see
[`../legion/the-merge-queues-adversarial-deep-review-can-fail-a-ready-head-the-corrective-round-after-approval.md`](../legion/the-merge-queues-adversarial-deep-review-can-fail-a-ready-head-the-corrective-round-after-approval.md))
failed `ba7a1dc8` on a third reopen defect, on the failure branch of the retire this change added:

The root leg of `retireTreeProcessesLocked` kept the locator when its stop threw `StopFailed`
(right — the sweep retries it) but wrote `tree.resumeSessionFile` only **after**
`stopProcessSerialized` resolved. `spawnRoot` derived `resuming` from `tree.resumeSessionFile`
alone, never from the retained `locator.ompSessionFile`. So a `todo` inside the window on a tree
whose root would not die was admitted (`admit` does not care about a locator), launched **fresh**
(`spawnTree` got `resumeSessionFile: undefined`), and `tree.locator = {...locator}` overwrote the
only durable handle to a process the daemon had just proved it could not stop — two roots, one
record, the reopen not held to the recorded session.

Why every proof missed it: the round-1 scratch daemon, the tester's independent one, the real-tmux
E2E, and 31 unit titles all exercised the retire's *success* branch. The rig's `kill-pane` always
succeeds after the stop timeout, so a failed root leg cannot occur on it without a fault at the
tmux boundary, and no unit test combined "root leg `StopFailed`" with "then a `todo`". The tests
that did model `StopFailed` were the worker paths and the sweep's retry — the retire, not the
reopen after it.

Two rules, both general:

1. **A retire whose stop can fail records its durable handoff before the fallible step.** The
   transcript path goes into `resumeSessionFile` before `probeTree`/`stopProcessSerialized`, so a
   `StopFailed` leaves the record complete for the sweep's retry *and* for a reopen. This is the
   same ordering rule
   [`retire-then-drain-a-pane-you-decide-not-to-prompt.md`](./retire-then-drain-a-pane-you-decide-not-to-prompt.md)
   states for the worker boot confirmation ("confirmed before the stop that can throw"): whenever
   a primitive has a caller-visible record and an `await` that can throw, ask what the record says
   if the throw lands, and write the durable part first.
2. **Every later launch decision reads the retained handle, not only the field the success path
   writes.** A launch that lands on a tree still recording a locator is landing on a process
   nobody proved stopped. `spawnRoot` now runs a fence, `stopRetainedRootProcess`, before bumping
   the generation: record the transcript, probe, stop with the same probe-derived options the
   retire uses (under `stoppingForRelaunch`, so the old root's own `/process/exit` during that stop
   is not read as a death), clear the locator only on a confirmed stop, and let a `StopFailed`
   propagate into `spawnRoot`'s existing rollback (generation restored, tree `queued` with
   `retryResumeSessionFile`, `launchFailures` charged, `launch-failed` at the bound). It is a
   no-op for every ordinary launch — no locator, immediate `false` — and costs nothing there.
   The claim is deliberately left alone: it carries the session the resumed launch must expect
   (`expectedSessionId`), and the teardown that finally stops the process deletes both together.

The review offered two mechanisms — fence the launch, or derive `resuming` from
`tree.locator?.ompSessionFile ?? tree.resumeSessionFile`. The derivation alone would still have
opened a pane over a process nobody proved stopped; only the fence makes "a locator is present"
mean "stop it first". Rule 1 without rule 2 is a transcript kept for an agent that is still
running. Say which mechanism you chose and why in the reply to the review, in the PR body's E2E
paragraph, not only in the code comment.

A smaller finding in the same round with its own general shape: `retireUnconfirmedBoot` skipped
`closed` and closing trees but not `lingering`, so a boot watchdog or restart-time reconnect on a
finished tree still charged `launchFailures`, queued a retry, and could publish `worker-died` to a
finished tree's architect. The fix enumerates the statuses (`closed`, `lingering`, `closingTrees`)
rather than routing through `isTreeGone`, because that umbrella is also true for a *missing* tree
record, and this guard must still clear a dangling locator for an unresolvable tree. A skip-guard
that has to separate "resolved and inactive" from "unresolvable" names its statuses; a new tree
status therefore means grepping `=== "lingering"` / `=== "closed"` near every guard, not trusting
one helper.

### What the fence did to the test file

- `tree(state)` seeds a locator whose `ompSessionFile` is a path that never exists on disk. Four
  tests launched a root over it and passed only because a launch over a recorded locator used to
  mean nothing; with the fence it means "stop and resume", and `assertResumeSessionFile` refuses
  the phantom transcript (`Refusing to start … while resurrecting: recorded OMP session file is
  missing`). Each was a fixture, not an assertion: a first-launch test deletes the locator, a test
  about the sweep's workspace retry deletes only `ompSessionFile`. When a change makes "this
  record has a locator" load-bearing, audit every shared builder that seeds one incidentally.
- One of those four — `starts fresh when launching a root outside the resurrection path` — pinned
  the hazard itself (a fresh launch over a recorded locator). It became `stops the process a
  retained locator names and resumes its transcript when a launch lands on a tree that still
  records one`, asserting the `stop-frame` before `new-window`, with a genuine negative control
  beside it (`… records neither a locator nor a session file`). Delete a test that pins the bug,
  never re-pin it to the new text.
- The fixture for the four red-first tests (`failedRootRetireFixture`) needs a real `sleep` and
  one-second stop timeouts: a collapsed sleep starves macrotasks through the boot watchdog's
  re-arm loop, and the `StopFailed` the tests need comes from a runtime whose `kill-pane` answers
  exit 1 with a stderr that does not match `PANE_GONE_STDERR`.
- How the tester then drove the same blocker live, with a fault at the tmux boundary, is in
  [`../testing/an-isolated-scratch-daemon-from-inside-a-legion-pane-what-it-proves-and-the-two-things-it-cannot-reach.md`](../testing/an-isolated-scratch-daemon-from-inside-a-legion-pane-what-it-proves-and-the-two-things-it-cannot-reach.md)
  ("The third thing — reachable after all").

## Related

- [`a-linger-that-stops-nothing-document-the-transition-not-the-expiry-and-start-the-shared-retire-after-the-durable-persist.md`](./a-linger-that-stops-nothing-document-the-transition-not-the-expiry-and-start-the-shared-retire-after-the-durable-persist.md)
  — the change these defects were found in.
- [`a-resume-the-same-agent-guarantee-is-kept-by-session-identity-and-the-resume-reference-must-survive-a-refused-generation.md`](./a-resume-the-same-agent-guarantee-is-kept-by-session-identity-and-the-resume-reference-must-survive-a-refused-generation.md)
  — the other place `resumeSessionFile`'s lifetime was extended, and the same kind of gate audit.
- [`dispatch-status-writes-one-writer-per-transition-and-the-pending-write-fence.md`](./dispatch-status-writes-one-writer-per-transition-and-the-pending-write-fence.md)
  — why the root `in_progress` write has exactly one site, which is what made `!resume` load-bearing.
