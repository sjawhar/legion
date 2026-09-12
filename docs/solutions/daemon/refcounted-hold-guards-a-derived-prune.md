---
title: "A derived prune needs a refcounted hold owned by the launch, released the moment its state lands"
category: daemon
tags:
  - concurrency
  - refcount
  - prune
  - secrets
  - process-manager
  - spawn-generations
  - code-review
date: 2026-09-11
status: active
module: daemon
problem_type: concurrency
component: processes.ts
severity: high
applies_when:
  - A cleanup pass derives "still needed" from durable state (locators, claims) that an in-flight operation has not written yet
  - Two concurrent operations can key off the same identifier (same role token, same tree, same file name)
  - A hold/lock is released inside a catch block that also performs side effects which can throw
related_issues:
  - "LEGION-6"
  - "sjawhar/legion#923"
---

# A Derived Prune Needs a Refcounted Hold Owned by the Launch, Released the Moment Its State Lands

## Context

`sjawhar/legion#923` moved every pane secret (Dispatch bearer, boot tokens, the controller
secret) out of tmux `-e` argv into `<state_dir>/secrets/<role token>` files, and made
`ProcessManager.persist()` prune any file no live locator references. The file is written
*before* the pane launches; the locator that makes it "live" is written *after* tmux returns.
Between the two, only an in-memory exemption keeps a concurrent `persist()` (any other role's
save) from reaping the file the pane is about to read. Three review rounds found three defects
in that exemption, each a general lesson.

## What went wrong, and the rule each one teaches

**Round 1 — a `Set` is not a hold.** The exemption was `launchingSecrets: Set<string>` inside a
`withPaneSecret(token, value, body)` wrapper: `add` before the write, `delete` in `finally`. Two
generations of one root can be launching at once (a park-then-re-admit starts generation 2
while generation 1 is still blocked inside tmux — `spawnTree`'s documented `treeReplaced`
path), and both share `legion-<project>-<root>-architect`. Generation 2's `add` was a no-op;
generation 1's `finally` deleted the exemption while generation 2's launch was still queued;
generation 1's own `persist()` then pruned the file (reviewer reproduced it; the regression
test later showed the prune's `rm` racing generation 2's `writeSecretFile` chmod → `ENOENT` →
launch failure). **Rule:** when "is this still needed" spans overlapping lifecycles of the same
identifier, count holders (`Map<string, number>`); membership is wrong.

**Round 1 — the hold belongs to whoever writes the state.** The wrapper released when the
*launch helper* returned, but the locator is stored by the *caller* (`spawnRoot`,
`launchWorker`, `spawnController`) some awaits later. The fix moved ownership up:
`holdProcessSecret(token)` is taken by the launch owner before anything is written and its
idempotent release runs only once the locator — or the failure rollback — is in state,
immediately before the `persist()` that follows, so that persist's prune sees the file referenced
or reaps it deliberately. `launchShimmedProcess` only writes (`writePaneSecret`).

**Round 2 — release first, side-effect second.** `spawnRoot`'s catch released the hold *after*
`publishController` (a synchronous NATS publish that throws on a closed connection) and two
promotion-sweep awaits that each `persist()`. A throw there exited the catch with the hold
still counted — for the daemon's lifetime, since `startRoot`'s `.catch` swallows it. The
sibling catch in `launchWorker` already released as its first statement; the reviewer treated
the asymmetry between two structurally similar blocks as the bug it was. **Rule:** release a
hold as the very first statement after the state mutation it protects, before anything in the
same block that can throw. When one code path already does it right, its sibling's divergence
is a defect, not a style choice.

## Two placement decisions that stood

- **The boot-token write stays before the per-issue serialize lane**, under the owner's hold,
  not inside the lane as the review first suggested. The only deterministic reproduction of the
  race observes generation 2's write landing before generation 1 settles; moving the write inside
  the lane queues it behind generation 1's blocked tmux call and makes the reproduction
  unobservable (the test deadlocks post-fix). The refcount alone closes the race; the boot token
  is generation-bound at `/process/started` (409 on a generation mismatch), so a stale pane that
  reads the newer token cannot assume its identity. Ruling accepted in round 2. **Rule:** when
  two placements are equally correct, keep the one the deterministic regression test can
  observe.
- **Steady-state prune walks only the names this process wrote or inherited** (`processSecretFiles`),
  never `readdir` per save — a per-save listing added fs I/O to every `persist()` and made
  fake-`sleep` test fixtures race the launch chain. The one listing prune at boot (`index.ts`)
  reaps crash leftovers **and seeds** `processSecretFiles` with its survivors (round-1 S1), so an
  inherited file is still reaped the moment its locator clears — "a pane file lives exactly as
  long as its locator" holds across restarts.

## Open edges (recorded, not fixed here)

- Reverse-order generations — generation 2 entering the lane before generation 1 has entered
  it at all — would let generation 1's later write clobber generation 2's token in either
  placement. Unreachable in the documented ordering; self-heals via the registration deadline
  (403 at `/process/started` → retire → `launchFailures++` → resurrect). A stale-at-lane-entry
  guard is the architect's call.
- Pre-existing on `main`: `index.ts` runs `reconnectWorkers` before `api` is assigned, so
  `markWorkerDead → revokeRoleClaim → api.revokeSessionCapability` throws for a confirmed-dead
  worker at boot. Needs its own issue.

## Related

- `docs/solutions/testing/race-regression-tests-that-fail-before-the-fix.md` — how the C2 and
  R1 reproductions were made deterministic.
- `packages/daemon/src/daemon/AGENTS.md` — the persisted contract (`holdProcessSecret`, boot prune
  seeding, `PANE_GONE_STDERR`).
