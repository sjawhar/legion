---
title: "Daemon-owned Dispatch status writes: enumerate the existing writers before adding one (a released child is its own tree), and read a pending write only through its statusAtRecord fence"
category: daemon
tags:
  - dispatch
  - issue-status
  - pendingStatusWrites
  - statusAtRecord
  - spawn-worker
  - phase-complete
  - tdd
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-59"
  - "sjawhar/legion#998"
  - "LEGION-82"
symptoms:
  - "an issue's Dispatch history shows testing right after retro"
  - "a child issue reads todo → needs_review → retro, never in_progress"
  - "the daemon PATCHes a lifecycle status over a move a human made from the dashboard"
---

# Daemon-owned Dispatch status writes: one writer per transition, and the pending-write fence

## Context

LEGION-59 (PR #998) stopped the daemon from PATCHing `testing` on every implementer completion
(the `.legion/` deletion push and retro are implementer completions too, so `retro → testing`
appeared after every clean review). The fix took four rounds, and three of them were about the
same two questions: *who already writes this status*, and *what "the status" means when the
daemon's own last write has not landed*.

## 1. Enumerate the writers before adding one

The daemon's lifecycle writes all go through `writeStatus` (`dispatch-client.ts`). At this head:

| transition | writer | where |
| --- | --- | --- |
| first admission → `in_progress` | `spawnTree` with `resume: false`, for `tree.root` — a resurrection (`resume: true`, from `resurrectDeadTree`) relaunches the same session and writes nothing (LEGION-82) | `processes.ts` |
| release → `todo` | `/waves/release` (architect capability) | `api/routes/issues.ts` |
| implementer completion → `testing` (only from `in_progress`) | `phaseCompleteStatus` | `api/routes/workers.ts` |
| tester completion → `needs_review`; reviewer completion → `retro` or `in_progress` (changes requested) | `phaseCompleteStatus` | `api/routes/workers.ts` |
| corrective implementer spawn under `changes_requested` → `in_progress` | `spawnStatus` on `/worker/spawn` | `api/routes/workers.ts` |
| merge / close → `done` | `closeTree` | `processes.ts` |
| controller `todo`/`backlog`/`icebox` | `/issues/status` | `api/routes/issues.ts` |

The round-2 ruling added a second spawn-time write, "`todo` → `in_progress` for the first worker
on a released child", reasoning from the table's *gaps*: only the root gets `in_progress` from
`spawnTree`, children are released to `todo`, so a child's implementer would complete from `todo`
and the new guard would write nothing. Unit tests modelled a child at `todo` under its parent's
tree and passed. The tester's throwaway daemon then showed the premise false: `reduceIssueUpdated`
emits `admit` for **any** issue reaching `todo`, so a released child is admitted as **its own
tree** — its own root pane, its own admission slot, its own `spawnTree` writing `in_progress`
(`LEGSMOKE-98`: `PATCH todo` at 06:25:25 by `/waves/release`, `PATCH in_progress` at 06:25:27 by
`spawnTree`, no spawn involved) — and the parent's `spawn_worker` for it is refused because
`rootForIssue(child)` is now the child. The live deployment's `GET /legion/v1/state` confirmed it:
every released child there is a tree. The only active issue ever at `todo` is one a human moved
back, which the daemon must not override. Round 3 dropped the branch.

Two habits that would have saved the round:

- **Before adding a status writer, trace the event that creates the state you think is
  unwritten.** Here: "what happens when a child reaches `todo`?" — grep `reducers.ts` for the
  status and follow the effect (`admit` → `ensureTree` → `startRoot` → `spawnTree`), not the API
  routes alone.
- **A fixture that hand-builds a state production never produces proves nothing about
  production.** The child-at-`todo`-under-the-parent fixture was internally consistent and
  unreachable. Ask what sequence of real events yields the fixture; if none does, the test is
  testing the fixture. The rig found this in one run; see
  `../testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md` (§ Proving a negative).

One residual is deliberate, not missed: the **tester's** completion still writes `needs_review`
unconditionally, so a post-approval, unchanged-fingerprint rebase re-check reads
`retro → needs_review → retro` (the reviewer's confirmation approval writes `retro` back). The spec
rejected gating it: after that re-check the head does go to the reviewer for a confirmation, so
`needs_review` is accurate there and nothing loops. A future worker who sees that wobble and
reaches for the implementer's guard should read the spec's Rejected list first.

## 2. Read a pending write only through its `statusAtRecord` fence

`state.issues[key].status` is the last status Dispatch *echoed* through the durable lane; a
daemon-owned PATCH that failed is recorded in `state.pendingStatusWrites[key]` as
`{status, statusAtRecord}` for resync to retry. The round-2 guard read the pending write first
(`pending?.status ?? echoed`) so a Dispatch outage between admission's `in_progress` and the
implementer's completion would not swallow the `testing` write. Reviewer round 2 (thread
3999033803) showed the regression that introduces: the pending write has no expiry, and only
resync's `retryPendingWrite` applies the supersession rule — so a pending `in_progress` left by a
failed PATCH outranked a human's later echoed `backlog` for up to `resyncIntervalMs` (600 s), and
the implementer's completion would PATCH `testing` over the human's move. The round-1 code, which
read only the echoed status, had never done that.

The fence is resync's own, applied at the read:

```ts
function knownIssueStatus(state: LegionState, issue: IssueKey): IssueStatus | undefined {
  const pending = state.pendingStatusWrites[issue];
  const echoed = state.issues[issue]?.status;
  return pending && echoed === pending.statusAtRecord ? pending.status : echoed;
}
```

A pending write counts only while the echoed status is still the one it was recorded against;
the moment Dispatch echoes anything else, the echo wins — exactly what `retryPendingWrite`
concludes when it later drops the entry. **Wherever a pending write is read, it is read through
this fence**; a bare `pending?.status ?? echoed` is a bug even when the write it shadows is the
daemon's own. Both the completion guard and the corrective-spawn guard now read through it, and
the AGENTS.md rows define it once.

The tests that pin it are a pair: pending `{in_progress, statusAtRecord: todo}` with echoed `todo`
→ the completion still writes `testing`; the same pending entry with echoed `icebox` → nothing.
The second failed red on the unfenced code with the exact write the reviewer predicted.

## 3. TDD notes from a four-round daemon change

- **Removing a branch is TDD too.** Round 3 did not delete the `todo` test and the branch; it
  first rewrote the test to assert the new contract ("a spawn for an issue a human moved back to
  `todo` writes nothing"), watched it fail against the still-present branch, then removed the
  branch. The old test's name now documents why the case exists.
- **Every guard gets its negative twin in the same commit**: `in_progress` → `testing` beside
  `retro` → nothing; pending-and-fenced → `testing` beside pending-and-superseded → nothing;
  corrective spawn under `changes_requested` beside `approved` and no-PR. The reviewer's
  "set the fixture's status explicitly in the positive test" nit is the same instinct: a test that
  relies on the `beforeEach` default can be retargeted silently.
- **Use the fixture builders.** Four 14-field `PrState` literals became
  `checkPr(root, { number: 9, reviewDecision: … })` (`__tests__/ci-fixtures.ts`); the literals
  had been copied from an older test in the same file. Grep `__tests__/*fixtures*.ts` before
  writing a literal of a state type.

## Related

- `../testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md` — the rig that found § 1,
  and the logging proxy that made "no PATCH left the daemon" a request-level fact.
- `../../packages/daemon/src/daemon/AGENTS.md` — the `/worker/spawn` and `/phase/complete` rows.
