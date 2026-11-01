---
title: "A close fence awaited downstream must clear before the close hands on its slot; a status a failure path rewrites is not proof that no live root is in the directory"
category: daemon
tags:
  - closingTrees
  - closeTree
  - promotion-sweep
  - admission-slot
  - reentrancy
  - deadlock
  - StopFailed
  - workspace-removal
  - processes.ts
  - fake-runtime-tests
date: 2026-09-14
status: active
module: daemon
related_issues:
  - "LEGION-104"
  - "sjawhar/legion#1087"
  - "LEGION-143"
---

# A close fence awaited downstream must clear before the close hands on its slot; a status a failure path rewrites is not proof that no live root is in the directory

LEGION-104 made `closeTreeLocked` remove a finished issue's jj workspace, and made `spawnRoot`
wait on any in-flight close of its tree or an ancestor's (`awaitClosingTrees`) so a re-admitted
root never provisions into a directory mid-removal. The wait was correct and the review found it
deadlocked the daemon; the status guard was correct and the review found it removed a live root's
workspace on the retry path. Three rules and one split-out issue came out of it. Every rule has a
fake-runtime regression in `packages/daemon/src/daemon/__tests__/processes.test.ts` that failed on
the shape it names.

## Rule 1 — never run the promotion cascade inside the frame `closingTrees` names

`closingTrees` (`processes.ts`) is a `Map<IssueKey, Promise<…>>`: `closeTree` sets the entry
before `closeTreeLocked` runs and deletes it in a `.finally`. It is a fence — `spawnWorker`,
`launchWorker`, and `isTreeGone` refuse work for a tree it names — and, since LEGION-104, also
something a launch **awaits**: `spawnRoot` → `awaitClosingTrees(issue)` awaits the map's promise
for the issue and each ancestor.

The close used to release its admission slot from inside its own frame:
`closeTreeLocked(T)` → `releaseSlot(T)` → `beginPromotionSweep` → `advancePromotionSweep` →
`await startRoot(next)` → `spawnRoot(next)` → `awaitClosingTrees(next)`. When `next` is a child
D of T holding its own `queued` tree record (moved to `todo` while T lingered — `liveAncestorTree`
sees no live ancestor and admits it as a root — whose first launch failed and left it queued with a
slot free) and T was re-admitted mid-close (so it holds a slot for the close to release), D's wait
finds T in the map and awaits `closeTreeLocked(T)`'s own promise. Neither settles: `closeTree(T)`
never resolves, `isTreeGone(T)` is true for the daemon's lifetime, `promotionSweep.inFlight` never
returns to 0, and `drainSpawns()`/`stop()` hang. The regression
(`settles a close whose freed slot promotes a queued child of the closing tree …`) hit bun's 5 s
per-test timeout on the previous code.

The fix is the boring one. `closeTreeLocked` splices the tree out of `admission.active` itself
and returns whether it did; it never calls `releaseSlot` or the sweep. `closeTree` awaits that
promise — whose `.finally` has already deleted the map entry by the time the outer `await`
continues — and only then, when a slot was released, awaits `beginPromotionSweep()`. Nothing the
fence protects needs it any longer at that point: every claim is deleted, the record reads
`closed`, and `isTreeGone` refuses launches on status alone. `releaseSlot`'s other callers
(`beginLinger`, `recordRootExit`, `spawnTree`'s human-parked rollback) are untouched: none of them
runs inside a frame the map names.

The general rule: a promise you put in a map for others to await must not, from inside its own
executing frame, trigger work that can reach one of those awaits. Hand the trigger to the caller
that owns the `.finally`, sequenced after it.

## Rule 2 — the wait walks the whole ancestor chain

`awaitClosingTrees` walks `issue` → `state.issues[issue].parent` → … with a `seen` set, checking
the map at every level. Two distinct races need it and are pinned separately: a root re-admitted
while its own tree closes (`provisions a root admitted while its previous tree is still closing
…`), and a child re-admitted as its own root while its **parent's** tree closes (the rule-1
regression: the child's own tree record makes `rootForIssue(child)` the child, so a wait keyed on
the issue alone would never see the parent's close). A one-level check passes the first test and
silently under-protects the second.

## Rule 3 — `lingering` is not proof that no live root is in the directory; key on the held slot

The removal step's first guard was `tree.status === "lingering"`: `admit` reuses the record
mid-close for a root a human moved back to `todo`, marks it `active`, and takes a slot, so a
non-lingering record means "someone is about to provision here — keep everything". The blind spot
is the failure path. A close whose stop fails (`StopFailed`: a kill that would not land, a pane
that could not be listed) rewrites `tree.status = "lingering"` with a fresh `lingerUntil` for the
sweep's retry — over the `active` the mid-close `admit` wrote — while the slot `admit` took stays
in `admission.active`. The sweep's retry close then reads `lingering`, removes the re-admitted
root's fresh workspace, and the live root works in a deleted directory.

`removeTreeWorkspaces` now keeps every workspace when either the record is not `lingering` **or**
`state.admission.active.includes(treeKey)`, each with its own reason in the one
`[legion] kept every workspace of tree <ROOT> at its close: …` line. The regression
(`keeps every workspace when the sweep retries a close whose first attempt failed after the root
was re-admitted …`) drives exactly that sequence — mid-close `admit`, one failing `kill-pane`,
`StopFailed`, `active` `[T]` with status `lingering`, then the retry — and saw the forget run on
the status-only guard.

The general rule, a sibling of the idle-retire rules in
`idle-expiry-timers-rearm-on-declines-that-change-while-idle.md`: a durable status field that a
failure path can rewrite is a hint, not a lock. When a decision is "is anyone live here", read the
resource the live party holds (the admission slot, a locator, a claim), not the status the last
writer left.

## LEGION-143 — what the close still does to the re-admitted root, and why it is not this fix

With rules 1–3 in place a root reopened mid-close is provisioned only after the close, and the
close removes nothing. What the close's tail does next is unchanged and pre-dates LEGION-104:
`tree.status = "closed"`, a `done` status write whenever the root's echoed status is not
`done`/`backlog`/`icebox` (so a human's fresh `todo` is overwritten), the slot released; and
`spawnTree` retires a just-opened root whose `tree.status` reads `closed`. So the reopened root is
provisioned and then killed. That is filed as LEGION-143 ("A root issue reopened while its previous
tree is still closing is killed by that close"). It was split out because fixing it changes the
close's semantics for every caller (the linger expiry, the root's own exit report) and LEGION-104's
scope was the workspace; the LEGION-104 tests deliberately pin neither the retire nor the `done`
write, so the LEGION-143 fix will not have to unpin them.

## How the regressions are built (the harness facts that cost time)

- **The gate is the stop's timeout sleep.** Inject `sleep` and resolve a `stopArmed` promise from
  it: when the root's shim never confirms shutdown (`stuckRootClient.shutdown = () => {}`), the
  close is parked inside `awaitShutdown` exactly when `stopArmed` resolves, and `stopGate` (what
  the fake `sleep` returns) is released when the test says. The re-admit (`processes.admit(root)`)
  lands between the two. No tick budget is needed for the positive waits.
- **Answer `split-window`, not only `new-window`.** The re-admitted root's spawn splits into the
  tree's recorded window; a `run` fake that answers only `new-window` fails the spawn, `spawnRoot`'s
  catch re-queues the tree, and the retry test silently exercises a spawn-failure path instead of
  the scenario.
- **Instrument "after the close" on state, not on `closeTree`'s promise.** `closeTree`'s promise
  now spans the promotion sweep that follows the close, so a flag set on it no longer marks the
  close's end. Record `state.trees[T]?.status` at each `jj` command instead: every provisioning
  command must see `closed`, the mark the close writes after its removal step.
- **Restore `console.error` spies in a `finally`.** A later test in the same file counts
  `console.error` calls (LEGION-107's `receipt_timeout` test); a spy left installed after an
  assertion throws makes it inherit these tests' lines.
- **Fail-first is the timeout.** The deadlock regression's RED is `this test timed out after
  5000ms` with every assertion unreached — run it against the previous `processes.ts` (swap the
  file in, run, swap back, `cmp`) and record that line; a green-on-first-run version of this test
  proves nothing.

## What is deliberate, not a gap

A failed removal is logged once and the tree still closes; nothing retries and the leftover is
today's state until that issue is next provisioned (`createWorkspace` repairs whichever half state
it finds). The spec rejected keeping the tree `lingering` for a retry — a persistent failure would
re-run the close every minute forever. The reviewer's thermo pass flagged the accumulation; it is
the chosen trade, recorded in `packages/daemon/src/daemon/AGENTS.md`'s "Workspace removal" bullet.
