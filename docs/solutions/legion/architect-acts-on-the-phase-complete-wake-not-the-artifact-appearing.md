---
title: "The architect acts on the phase-complete wake, not on the artifact appearing: a next-role spawn that outruns `handoff complete` turns the completion into a 409"
category: legion
tags:
  - legion
  - architect
  - phase-complete
  - spawn_worker
  - handoff
  - "409"
  - race
date: 2026-09-14
status: active
module: skills/legion-architect
related_issues:
  - "LEGION-109"
  - "sjawhar/legion#1083"
---

# The architect acts on the phase-complete wake, not on the artifact appearing: a next-role spawn that outruns `handoff complete` turns the completion into a 409

## What happened

On LEGION-109 the planner committed `.legion/plan.json`, pushed it, and then ran
`legion handoff complete`. The architect, woken by the artifact landing on the branch, had already
called `spawn_worker` for the implementer. The planner's completion answered
`409 Phase for LEGION-109 is no longer owned by this worker`.

## Why the daemon says 409

`state.phases[issue]` is the issue's active phase — `{phase, sessionId}`. It is written in exactly
one place: the delivery of an architect **assignment** (`promptExistingWorker` with
`kind: "assignment"` in `processes.ts`, whether prompted straight into a live worker or queued as
`pendingAssignment` and delivered at `/worker/ready`). `POST /legion/v1/phase/complete`
(`api/routes/workers.ts`) verifies that record still names the caller's role and session before it
captures, clears, and publishes `{type: "phase-complete"}` to the architect; when the record has
moved on it answers 409 and publishes nothing. So the sequence

1. planner pushes `.legion/plan.json`,
2. architect `spawn_worker(implementer, …)` — the assignment lands, `phases[KEY]` now names the implementer,
3. planner `legion handoff complete`

makes step 3 a 409 by construction. It is not LEGION-37's respawn case and not a multi-round tree
(`worker-pane-shell-gotchas` §11 covers those); the worker did nothing wrong and nothing was
restarted. The architect simply treated "the artifact exists" as "the phase is complete".

## The rule

**The artifact appearing is not completion. The `phase-complete` wake is.** An architect that
sees a handoff land — a push or `ci-green` envelope, a `jj log` that shows `plan: record handoff`,
a Dispatch status echo — waits for the `{type: "phase-complete", issue, role, summary}` payload on
its role topic before spawning the next role. The worker's two-sentence summary is part of the
contract the next assignment is built from, and the completion write is what clears the phase
record so the next assignment does not collide with it.

If the wake does not arrive within a bound, do not guess: ask the worker on its own role topic
(`notifications.role.legion-<project>-<KEY>-<role>`) whether it has completed. A worker that has
already run `handoff complete` and got 202 (`no architect was live`) has its completion recorded
as `phases[issue].completed`, and `overseerCatchup` replays it on the architect's next resume; a
worker that has not run it yet will.

## What the worker does when it happens anyway

A 409 here carries no record on the daemon (contrast the 202 above). The worker publishes its
completion summary to the architect's topic with `envoy_publish` — head SHA, what changed, the
test counts, the run ids, and the 409 itself — and stops; a repeat of `handoff complete` only 409s
again. The architect acts on that message directly. This is §11's recovery, reached by a
different route.

## Related

- [worker-pane-shell-gotchas](worker-pane-shell-gotchas.md) §11 — the same 409 after a respawn and
  in a multi-round tree, and the message-the-architect recovery.
- [phase-complete-stranded-on-no-holder](phase-complete-stranded-on-no-holder.md) — the 202 side:
  a completion with no live architect is recorded and replayed, never dropped.
- [one-writer-for-the-active-phase-and-bystander-catchups](../daemon/one-writer-for-the-active-phase-and-bystander-catchups.md)
  — why exactly one path writes `phases[issue]`, which is what makes this race deterministic.
