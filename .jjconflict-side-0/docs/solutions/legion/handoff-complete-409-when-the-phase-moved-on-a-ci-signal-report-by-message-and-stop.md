---
title: "`legion handoff complete` answers 409 when the phase moved before you reported: the two 409 texts, why the phase follows the last delivered assignment, and the message fallback"
category: legion
tags:
  - handoff-complete
  - phase-complete
  - 409
  - active-phase
  - spawn_worker
  - ci-green
  - envoy_publish
  - implementer
date: 2026-09-14
status: active
module: packages/daemon/src/daemon/api/routes/workers.ts, packages/daemon/src/daemon/processes.ts
problem_type: process
severity: low
related_issues:
  - "LEGION-93"
  - "sjawhar/legion#1082"
  - "LEGION-37"
applies_when:
  - `legion handoff complete` answers 409 and you must decide whether to retry, wait, or report another way
  - You are an architect deciding whether to spawn the next role on a CI signal before the current role has reported
  - A `spawn_worker` re-task reaches a worker that is still mid-turn
---

# `legion handoff complete` 409 when the phase moved first

## What happened (LEGION-93, corrective round)

The implementer pushed the round-2 fix, CI went green within minutes, and the architect — woken by
the `ci-green` notice — spawned the reviewer. The implementer's `legion handoff complete`, sent a
few minutes later, answered `409 {"error":"Phase for LEGION-93 is no longer owned by this worker"}`.
Nothing was lost: per the deployment rule, the completion facts went to the architect topic with
`envoy_publish` and the implementer stopped; the reviewer approved the same head an hour later.

## The two 409s say different things

`POST /legion/v1/phase/complete` (`api/routes/workers.ts`) checks two records in order:

- `Grant does not match the worker currently holding this role` — the **claim** for your
  (issue, role) belongs to another session: you are a stale generation. Stop; the daemon resumed a
  newer you.
- `Phase for <issue> is no longer owned by this worker` — your claim is fine, but
  `state.phases[issue]` names another role or another session: the **phase** moved on. Your work
  stands; only the completion record has nothing to clear.

## Why the phase moves without you

The active phase is written in exactly one place: the delivery of an architect assignment
(`promptExistingWorker` → `commitPromptDelivery` writes `phases[issue] = {phase: role, sessionId}`;
`phase/complete` only deletes, restores, or marks it completed). So the phase belongs to the
**most recently delivered assignment**, never to the worker that believes it is finishing. Two
ways it moves before your report:

- The architect acts on a signal that reaches it before your completion — here `ci-green` — and
  spawns the next role; that role's assignment overwrites the phase.
- A `spawn_worker` re-task for **you** arrives while you are mid-turn: `resumeOrQueueExisting`
  sees `runState !== "idle"`, queues it (`worker-queued`), and delivers it at your idle transition;
  that delivery re-takes the phase for the new assignment, so a completion report for the old one
  that lands after it 409s.

Both are legitimate. An architect spawning the next role on `ci-green` is faster than waiting for
the report; the cost is that the previous role's `handoff complete` becomes a message.

## The handling

Implementer: on either 409, do not retry and do not wait for the phase to come back. Publish the
whole completion report — head SHA, gates, threads, anything the summary would have carried — to
the architect's role topic (`notifications.role.<your tree's architect token>`, stated at the end of
your system prompt), then stop. Make the report the last act of the turn and keep the turn short
after the push, so the window in which a signal outruns you stays small. LEGION-37's bug (a
relaunched worker re-registering as the active phase) shares the 409 text; the fallback covers both.

Architect: when you spawn the next role before the current one has reported, expect that role's
completion as a message rather than a `phase-complete` payload, and read the head SHA from it
before dispatching further.

## Related

- `../daemon/one-writer-for-the-active-phase-and-bystander-catchups.md` — `commitPromptDelivery`
  as the one writer of `phases[issue]`, and what a catch-up may not do to it.
- `tree-recovers-from-lost-wakes-by-reading-state-and-handoffs.md` — the architect's recovery
  when a completion never arrives as a payload.
