---
title: "A boot or resync sweep for a condition a reducer already handles live: one exhaustive predicate, the reducer's own effect executor, and the bare fixtures the new invariant surfaces"
category: daemon
tags:
  - reducers
  - effects
  - resync
  - reconcileAdmission
  - exhaustive-switch
  - test-fixtures
  - admission-queue
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-79"
  - "sjawhar/legion#1037"
  - "LEGION-56"
symptoms:
  - "A finished issue sits in admission.queue with a queued tree; the controller's reorder pass writes backlog → todo → done on it every ~22 minutes"
  - "After adding a status-derived sweep, five unrelated tests fail because their fixtures queued a key with no state.issues node"
---

# A sweep for a condition a reducer already handles live

LEGION-56 was closed in Dispatch but stayed in the daemon's admission queue: the reducer's
`backlog`/`icebox` branch only lingered an *active* tree and returned `[]` for a queued one, and
`issue.closed` did the same. LEGION-79 (#1037) added the live rule (a `dequeue` effect) **and**
two sweeps for entries already stale when the daemon starts or resyncs. Three decisions made
the three sites unable to disagree, and one consequence for the test suite followed from them.

## 1. One exhaustive predicate on the domain type, written before the second call site

The rule "this status has left the waiting line" is needed by the reducer, the boot sweep, and
the resync sweep. It lives once, in `legion-state.ts`, as a `switch` over the closed
`IssueStatus` union with a `never` default:

```ts
export function isStaleQueuedStatus(status: IssueStatus): boolean {
  switch (status) {
    case "triage": case "icebox": case "backlog": case "done": return true;
    case "todo": case "in_progress": case "testing": case "needs_review": case "retro": return false;
    default: { const unhandled: never = status; throw new Error(`unknown issue status: ${JSON.stringify(unhandled)}`); }
  }
}
```

A new `IssueStatus` member is a compile error at the one definition, not a silent default at
three sites. The reviewer's check was mechanical: the literal `"icebox"` appears in no other
non-test file of the diff. The spec's Rejected table records why the simpler rule *anything but
`todo`* was wrong — a daemon-owned status (`in_progress`, …) on a queued entry is the legitimate
shape boot's demote loop produces (admission wrote `in_progress`, the pane was never recorded),
and the simpler predicate would have dropped those trees.

The sweeps share a small companion, `staleQueueEntryReason(state, issue)`, which adds the one
case a reducer never meets — a queue entry with no `state.issues` node at all (`unknown issue`)
— and returns the tail of the log line, so the two sweeps cannot word it differently either.

## 2. The sweeps run the reducer's executor, never a parallel mutation

The reducer stays pure and emits `{ kind: "dequeue", issue }`; the durable lane awaits
`onDequeue` (→ `ProcessManager.dequeue`) **before** `saveState`, so the queue change and the
status change persist in one transaction and a persist failure is fatal like every other
effect's. The sweeps do not re-derive the mutation:

- `reconcileAdmission` (boot) calls the same private `removeQueued` that `dequeue` calls,
  before the demote/promote loop, and logs `[legion] dropped <KEY> from the admission queue at
  boot: <reason>` — so a stale head of the queue is never promoted and the valid entry behind
  it is.
- `resync.ts`'s `sweepStaleQueue` runs after `healStatusDrift` (so the status judged is the one
  Dispatch reports) and dispatches `[{kind: "dequeue"}, {kind: "log", …}]` through
  `deps.applyEffects` — the pump's own switch, the same `onDequeue`.

`dequeue` itself deletes a tree record only when it is `queued`; active, lingering, dead,
launch-failed and closed records belong to the linger, close and launch-failure paths, and a
redelivered event finds nothing to do (silent no-op, no save). A future change to what
"leaving the line" means is one predicate edit and one executor edit, applied everywhere.

**Pattern.** When a periodic or boot reconciliation must enforce a condition a reducer handles
live: extract the predicate first (exhaustive if the domain is a closed union), give the
reducer an effect, and have the sweep dispatch that effect (or call the effect's executor)
rather than mutate state itself. `api/state.ts` and `catchup.ts` then need no change — the
controller's view is the state, clean once the state is.

## 3. A state-derived invariant surfaces every fixture that built the adjacent slice bare

`staleQueueEntryReason` reads `state.issues[key]` for every queue entry. Five existing tests
pushed keys onto `admission.queue` with **no** `state.issues` node — a shape production never
produces (`admit()` runs only from a reducer that already required the node) but the minimal
shape a fixture author reaches for. The new boot sweep dropped them as `unknown issue` and
their original, unrelated assertions (launch-failed entries stay queued; promotion order;
bounded launch attempts) went red.

The fix is to seed the minimal node and keep the assertions exactly as written:

```ts
for (const issue of [root, child, grandchild]) {
  state.issues[issue] = { key: issue, title: issue, status: "todo", children: [] };
}
```

Two things to do next time:

- **Grep the whole `__tests__` tree, not the file the task names.** The plan swept
  `processes.test.ts` (`admission.queue.push` without a matching `state.issues[...]`) and found
  four; the full suite in task 7 found a fifth in `index.test.ts`
  (`promotes queued persisted issues before boot completes …`). Run the grep across
  `packages/daemon/src/**/__tests__/` when the invariant is state-derived, and fold the seeds
  into the commit that introduces the invariant (`jj squash --into <that commit> <test path>`).
- **Re-seeding is in-scope cleanup, not scope creep.** The fixtures were latent debt (a state
  shape the daemon never writes); the new invariant made it visible. Seed the node the production
  path would have written and leave the assertion alone — a changed assertion would mean the
  invariant changed behaviour the test was protecting.

## Related

- `packages/daemon/src/daemon/AGENTS.md` — the operational-invariant bullet "A waiting issue
  leaves the line with its status" (the contract these three sites implement).
- [two-documented-models-one-predicate-and-what-a-removed-tree-leaves-behind](two-documented-models-one-predicate-and-what-a-removed-tree-leaves-behind.md)
  — LEGION-57's `liveAncestorTree`, the same one-predicate discipline for tree ownership.
- [fixtures-derive-what-production-derives](../testing/fixtures-derive-what-production-derives.md)
  — the general rule that a fixture should hold what the production path would have written.
