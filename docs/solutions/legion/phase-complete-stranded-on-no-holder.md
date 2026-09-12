---
title: "A phase completion stranded on the no-holder path: what the worker, the architect, and a manual envoy_role_set each do and do not recover"
category: legion
tags:
  - legion
  - architect
  - worker
  - role-routing
  - phase-complete
  - catch-up
  - envoy
date: 2026-09-12
status: active
module: daemon
related_issues:
  - "LEGION-13"
  - "sjawhar/legion#978"
  - "LEGION-29"
  - "sjawhar/legion#970"
symptoms:
  - "legion handoff complete: [handoff] Warning: phase recorded; no architect was live to receive the summary"
  - "An architect re-claims its role with envoy_role_set and still never hears that a phase finished"
  - "legion state shows the issue at testing / needs_review but the architect has no phase-complete for it"
---

# A Phase Completion Stranded on the No-Holder Path

On LEGION-13 two phase completions (implement and test) were reported while the tree's architect held no
Envoy role — the listener had restarted and dropped every claim (LEGION-29). Both landed on the daemon's 202
path. The architect later re-claimed its role by hand with `envoy_role_set`; neither completion reached it that
way. This note records exactly what each mechanism recovers, so an architect in that state knows what to do.
The worker-facing half is [worker-pane-shell-gotchas § 7](worker-pane-shell-gotchas.md); the automatic
heartbeat recovery is [heartbeat-role-reassertion-and-regain-hooks](../envoy/heartbeat-role-reassertion-and-regain-hooks.md).

## What the daemon does with a completion nobody can receive

`POST /legion/v1/phase/complete` (`packages/daemon/src/daemon/api/routes/workers.ts`, `handlePhaseComplete`)
PATCHes the issue's Dispatch status, then publishes `{type:"phase-complete"}` to the architect's role topic
through the listener API. That publish is synchronous: an unheld role answers 404 at once. On that 404 the
daemon does **not** drop the completion and does **not** clear the phase — it rewrites
`state.phases[<KEY>]` with `completed: {summary, at}` and answers 202, which the CLI prints as
`[handoff] Warning: phase recorded; no architect was live to receive the summary`. Three consequences:

- Routing already treats the issue as having no active phase (`reducers.ts` `routeActive`: a completed
  phase routes to the architect), and the Dispatch status has already moved. The tree looks advanced.
- The parked record is invisible from outside the daemon: `GET /legion/v1/state` is an explicit allowlist
  (`api/state.ts` `buildLegionStateResponse`) and `phases` is not in it. `legion state` cannot show it.
- The only daemon path that replays it is `overseerCatchup` (`catchup.ts`, `phaseCompletions`), which runs
  from `/process/ready` — the root's ready call — never from a role claim.

## What a manual `envoy_role_set` recovers, and what it does not

A hand-run `envoy_role_set` is a hard claim (`envoy.ts` `claimEnvoyRole`). It restores routing for every
**future** publish. It replays nothing:

- The regain hook that re-runs `/process/ready` (`legion.ts` `bootstrapRoot` → `onEnvoyRoleRegained` →
  `rerunReadyAfterRegain("process/ready", …)`) fires only from the heartbeat's own `reassertRole`
  (`envoy.ts`), with reason `"reclaimed"` when the heartbeat's *soft* claim lands, or `"reregistered"` on
  the first healthy tick after a *failed registration*. After a manual hard claim the next tick's
  `GET /v1/roles/<role>` names this session, `afterOutage` is false, and `reassertRole` returns before the
  hook (`if (holder === id) { if (!afterOutage) return; … }`).
- The daemon's `reclaim-architect` control directive (`processes.ts` `handleException`) fires only for a
  role-lane *delivery exception* on the architect's role while the root pane is alive, and redelivers the
  daemon's own event. `handlePhaseComplete`'s 404 is the listener API refusing synchronously, not an
  exception-lane delivery, so it never reaches that path.

So with the pre-#970 plugin in the architect's pane — or after any manual claim — the parked completion stays
parked until the root is resurrected or `/process/ready` runs again for some other reason.

## The designed recovery: the worker reports again

`handlePhaseComplete` is idempotent by construction. Completing a phase never clears the worker's role claim
and never reassigns the phase; only a fresh `spawn_worker` assignment replaces the record. A **repeat**
`legion handoff complete` from the same worker, once the architect holds its role again, passes the same
ownership checks, publishes the `phase-complete`, clears the phase, and answers 200
(`api.test.ts`: "delivers an already-completed phase's report once the architect reappears, on a repeat
completion call"). A repeat while the role is still unheld just answers 202 again and rewrites the same
record — harmless.

Operationally, after an architect re-claims its role by hand:

1. Decide which phases may have completed during the gap: `legion state` → `issues[<KEY>].status` has moved
   (`testing`, `needs_review`, `retro`) while no `phase-complete` arrived; `roles[…-<role>].sessionId`
   names the worker.
2. `envoy_send(session_id=<that worker>, message="architect role re-held; re-run legion handoff complete")`.
   Direct session delivery does not depend on any role claim. The worker re-runs the one command, nothing
   else — no second handoff file, no new commit (its handoff is already committed).
3. Alternatively the worker sends the summary straight to the architect's session (what LEGION-13's workers
   did; gotchas § 7) — that informs the architect but leaves `phases[<KEY>].completed` parked until the
   next assignment or `/process/ready` overwrites it. Prefer the re-run: it clears the daemon's record too.

## Why the fix that landed did not cover this

`sjawhar/legion#970` makes the heartbeat re-assert a dropped claim and re-run `/process/ready` for the root
architect, which replays the parked completion. It reaches a pane only when the `legion` profile's plugin pin
moves (see the heartbeat note's § 8); a root architect launched before that runs the old plugin for its whole
life, and a human's `envoy_role_set` in the meantime is exactly the case that fires no hook. Until every
long-lived architect has been relaunched on a plugin with #970, the manual step above is the recovery.
