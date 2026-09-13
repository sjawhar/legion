---
title: "The active phase has one writer — the delivery of an architect assignment — and a catch-up for a finished worker is a bystander's, dropped at every point it could still be delivered"
category: daemon
tags:
  - phases
  - pendingAssignment
  - catch-up
  - worker-lifecycle
  - single-writer
  - discriminated-union
  - processes.ts
  - legion-state.ts
date: 2026-09-13
status: active
module: daemon
related_issues:
  - "LEGION-37"
  - "sjawhar/legion#991"
symptoms:
  - "`legion handoff complete` → 409 `Phase for <KEY> is no longer owned by this worker` for the worker the architect just assigned"
  - "a phase-complete or review wake reaches a finished role instead of the active one"
  - "state.json shows `phases[<KEY>]` pointing at a role whose phase ended hours ago, with a fresh `sessionId`"
---

# The active phase has one writer — the delivery of an architect assignment — and a catch-up for a finished worker is a bystander's, dropped at every point it could still be delivered

## Context

`state.phases[issue]` is what `/phase/complete` checks (`phase.phase === grant.role &&
phase.sessionId === grant.sessionId`) and what `routeActive` routes wakes to. Before #991 it had
three writers: `/worker/started` (`handleWorkerStarted`, for *whatever* role registered),
`promptExistingWorker` (for whatever it prompted, catch-up included), and the `pendingAssignment`
delivery at `/worker/ready`. The daemon's own recovery — a role-lane exception or a durable 404 on
a *finished* worker's topic — relaunched that worker with `--resume` and a `catchup-worker` prompt;
its `/worker/started` then re-pointed `phases[issue]` at a role whose phase had ended, and the
genuinely active worker's completion 409'd.

It is not a rare race. It bit LEGION-37's own tree three times while the fix was being written
(architect's log): 01:00 UTC a catch-up overwrote the queued architect assignment; 03:14 and 05:41
UTC a relaunch/catch-up took the phase from the assigned tester, 409ing its completion twice. Each
time a human had to read `state.json` and re-issue the phase by hand.

## Rule 1 — one writer, gated on *what kind of prompt* is being delivered

A queued prompt is now a discriminated object, not a string:

```ts
interface PendingAssignment { kind: "assignment" | "catchup"; task: string }
```

`promptExistingWorker` (`processes.ts`) is the only statement that writes `phases[issue]`, and only
when `pending.kind === "assignment"` — an architect's `spawn_worker` task, whether prompted straight
into a live worker or delivered from the claim at `/worker/ready`. A `catchup` (the daemon's own
recovery prompt, produced only by `resumeWorker`) never writes it. `/worker/started` never writes
it (a bystander registration gets one `… registered session … while <KEY>'s active phase is …`
line); a reconnect never writes it; `/phase/complete` only deletes, restores, or marks the record
`completed`.

Why a discriminated object rather than a sibling `kind` field, a second slot, or a runtime check
at the write site: every reader (`spawnWorker`, `resumeWorker`, `promoteQueuedWorker`,
`workerReady`, the catch-up payload, the state schema) is forced by the type to say which kind it
handles, and the spec's "a catch-up is DROPPED when an assignment is pending" needs no precedence
rule between two slots. Legacy bare strings are classified by payload at load (`migrateV25State`:
JSON whose `type` is `catchup-worker` is a `catchup`; anything else — free text, other JSON, text
that is not JSON — is an `assignment`).

## Rule 2 — one predicate for "is this role a bystander?"

```ts
isBystanderRole(state, issue, role)  = role !== "architect" && !isActivePhase(state, issue, role)
isBystanderCatchup(state, issue, role, pending) = pending?.kind === "catchup" && isBystanderRole(…)
```

Both live in `legion-state.ts` and are the one definition every bystander judgement uses:
`resumeWorker`'s pre-check, `deliverToWorker`'s lock-side guard, `promoteQueuedWorker`'s drop,
`workerReady`'s drop, and `handleWorkerStarted`'s log. Review round 2's should-fix was that the
same judgement was re-derived inline at three of those sites (`resumeWorker`, the registration
log, and `isBystanderCatchup` itself); a fourth site would have carried a fourth copy. Grep for
the predicate name is now the audit shortcut.

The architect exemption is deliberate, not an oversight: an architect (root or sub-architect) is
never `phases[issue].phase`, so the not-active-phase test would call it a bystander on every wake —
and a sub-architect parks for the life of its subtree with no other recovery path than this
catch-up. An architect's catch-up is therefore always delivered.

## Rule 3 — drop the bystander's catch-up at every point it could *still* be delivered

Deciding not to *send* a catch-up is not enough: a catch-up already queued keeps travelling after
the phase moves on. The phase can move while the catch-up is being computed, while it waits behind
the running-worker cap, and while the relaunched pane boots. #991 applies the same predicate at
each of those points; each was a separate review or test round because each looked handled from
the previous one's vantage:

| Point | Where | What happens to a bystander's `catchup` | An `assignment` in the same position |
| --- | --- | --- | --- |
| Before computing it | `resumeWorker` pre-check, before the GitHub catch-up fetch | not sent; one `[legion] no catch-up for …` line; no pane | n/a (only `spawnWorker` produces one) |
| Inside the role lock | `deliverToWorker`, after the fetch | dropped if an `assignment` landed meanwhile (`… the architect's assignment is queued and reaches the worker first`) — the 01:00 UTC bite | delivered |
| Behind the cap | `promoteQueuedWorker` (`worker-admission.ts`), both the idle-live prompt branch and the cold relaunch branch | queue entry consumed, pending cleared, nothing prompted or launched | prompted / launched |
| Behind a boot | `workerReady` | dropped and the relaunched pane **retired, then the queue drained** — see `retire-then-drain-a-pane-you-decide-not-to-prompt.md` | delivered; writes the phase |

The phase itself changes on a relaunch only through the architect's next `spawn_worker`; a
`spawn_worker` for another role *supersedes* the active phase by design, and the superseded
worker's completion is refused (`skills/legion-architect/SKILL.md`).

## Adding a producer or a delivery point

The invariant is enforced by the type and by review, not by a runtime assertion — a future
relaunch path that constructs `{ kind: "assignment" }` for what is really a recovery prompt
bypasses the whole fix. Before adding a path that prompts, queues, or relaunches a worker:

- Which kind does it produce? Only an architect's `spawn_worker` (`spawnWorker` →
  `deliverToWorker`) may produce `assignment`. Anything the daemon decides on its own is a
  `catchup`.
- Does it write `phases[issue]`? It must not; route the prompt through `promptExistingWorker` and
  let the kind decide.
- Can the queued prompt still be delivered after the phase moves? Then apply
  `isBystanderCatchup` at the delivery point, and decide what to do with the pane or slot the
  drop leaves behind (a never-prompted pane is not free — see the retire doc).
- Add the test in the same shape as the existing ones: the queued `catchup` is dropped, the
  queued `assignment` in the exact same position is delivered, and the phase is untouched by the
  drop.

## Tests that lock it

`api.test.ts`: `registers a worker session … without touching the issue's active phase`; `keeps
the implementer as the active phase when a finished reviewer's relaunch registers, and keeps
routing to the implementer`; `accepts the implementer's completion after a finished reviewer
relaunched`. `processes.test.ts`: `skips the catch-up for a finished phase worker …`; `drops a
catch-up inside the role lock when the architect's assignment lands while the catch-up is being
computed`; the two `drops a queued catch-up at promotion …` tests; `delivers a sub-architect's
queued catch-up at worker/ready whatever the child's active phase`. Each fails with exactly its
own guard removed (the tester re-ran the nine mutations after every round, including the rebase).

## Related

- `retire-then-drain-a-pane-you-decide-not-to-prompt.md` — what the ready-time drop must do with
  the pane it declines to prompt.
- `external-red-and-phase-ownership.md` §2 — the pre-#991 symptom as LEGION-23 met it.
- `idle-expiry-timers-rearm-on-declines-that-change-while-idle.md` — the other consumer of "the
  phase moved without this worker's involvement".
- `../testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md` — `GET /state` redacts
  `phases`; a proof about the active phase reads `state.json` on disk.
