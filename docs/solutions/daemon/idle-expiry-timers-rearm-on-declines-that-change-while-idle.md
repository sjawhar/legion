---
title: "Idle-expiry timers: judge every condition at expiry, re-arm on the declines that can change while the subject stays idle, and guard against a cancel that resolves"
category: daemon
tags:
  - timers
  - idle-retire
  - cancellable-sleep
  - critical-section
  - processes.ts
  - worker-lifecycle
date: 2026-09-12
status: active
module: daemon
related_issues:
  - "LEGION-30"
  - "sjawhar/legion#973"
---

# Idle-expiry timers: judge every condition at expiry, re-arm on the declines that can change while the subject stays idle, and guard against a cancel that resolves

## Context

LEGION-30 added `armIdleRetire`/`retireIdleWorker` to `ProcessManager`: a per-role-token clock
armed on the worker client's idle transition (`WorkerRpcClient.onIdle`), whose expiry re-reads live
state inside the token's `mutateClaim` critical section and retires the worker only if every
condition still holds (client still cached and idle; claim ready-confirmed with a locator; role not
`architect`; tree neither closing nor closed; no `pendingAssignment`; `phases[issue]` absent,
`completed`, or naming another role). The first shipped version passed eleven unit tests and leaked
on a real rig within minutes. Three rules fell out, each with a regression test that fails without
it.

## Rule 1 — a decline for a reason that changes *without the trigger* must re-arm

`onIdle` fires once per transition into idle. A worker that finishes its turn while it is still
`phases[issue].phase` declines at expiry — correct — and then never transitions to idle again. When
`/worker/started` or `promptExistingWorker` later rewrites `phases[issue]` for another role, nothing
fires: the clock's map entry was deleted at expiry, and the only arm site was the trigger that will
never recur. The worker stays resident for the life of its tree. The tester's rig showed a reviewer
idle 8.7 minutes, not the active phase for 5.7 of them, no clock pending.

The fix is in the expiry itself: when the predicate declines on a condition that can change while
the subject stays idle (here: the role is the active phase, or a `pendingAssignment` is queued),
re-arm the same clock from inside the same critical section and return. Every other decline leaves
it unarmed on purpose — running (its own next idle report arms), architect (never retired; a re-arm
would spin forever), closing/closed tree (`closeTree` owns the stop), missing claim/locator/
confirmation or replaced client (nothing left to judge). Order the tree-gone and architect checks
*before* the re-arm decision so neither can re-arm.

Ask of any expiry-judged timer: "for each decline, what event arms the next clock?" If the answer
is "the same trigger that already fired and will not fire again," the expiry must re-arm.

Regression lock: `processes.test.ts` "re-arms the clock when expiry declines because the role is
the active phase, and retires within one further window once phases[issue] moves to another role"
— fire → untouched + exactly one new wait pending; mutate `phases[root]`; fire → retired. Failed
at the pre-fix commit on `Expected length: 1 / Received length: 0`.

## Rule 2 — if `cancel()` resolves the sleep, the expiry needs an entry-identity check

`createCancellableSleep.cancel()` (and `boundedWait` over it) *resolves* the pending sleep rather
than dropping it. A re-arm therefore fires the superseded clock's `.then` immediately. Without a
guard, a worker re-prompted inside the window would be retired the instant it went idle again —
the spec's own rejected design ("kill immediately on phase completion") arriving by accident.

`armIdleRetire` cancels → creates → `set`s the new `{ cancel }` object synchronously, and the expiry
callback's first line is `if (this.idleRetireWaits.get(token) !== wait) return;`. `dispose()`'s
`clear()` makes every in-flight fire see `undefined` and return the same way. Object identity, not
a generation counter, because the map holds exactly one entry per token and the only question is
"am I still the current arm?"

Under an injected `deps.sleep` (the test seam) `cancel` is a no-op, so the superseded wait stays
firable — which is how a test delivers the stale expiry on purpose. Regression locks:
"ignores a superseded clock's expiry: after idle -> running -> idle, firing the older wait does
nothing and firing the newer one retires" and "ignores an armed clock's expiry after dispose()".
Delete the identity check and the first one retires on the stale fire.

## Rule 3 — the re-arm belongs inside the critical section that owns the prompt

Every `client.prompt()` site runs inside the token's `mutateClaim` section, and `prompt()` flips
`runState` to `"running"` synchronously before sending. Re-arming from inside the same callback —
with no `await` between the top-of-callback identity/idle/disposed checks and the arm — means the
re-arm can never interleave with a prompt and the checks it relies on still hold. A re-arm from a
`phases[issue]` write site (the rejected alternative: three sites — worker registration, prompting,
phase completion) would have coupled every phase write to the timer; one self-re-arming expiry
keeps the timer in one place.

## Why the unit tests missed Rule 1 and the rig caught it

The eleven original tests each seeded one state and fired once. None mutated `phases[issue]`
*between* an expiry and a second fire, because nothing in the unit picture suggested the phase
could move without the worker's involvement. On the rig the phase moved the natural way: the
architect resumed a retired planner, and that planner's `/worker/started` rewrote `phases[issue]`.
A state-mutation-between-fires test is cheap once you know to write it; knowing to write it came
from running the real mechanism. See
`docs/solutions/testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md`.

## Checklist for the next expiry-judged timer in `processes.ts`

- Arm through `boundedWait(ms, this.deps.sleep)` (the surface the boot watchdog and registration
  deadlines use), never a bare `setTimeout`.
- Store `{ cancel }` in a per-key map; first line of the expiry: identity check against the map.
- Enumerate every decline; for each, name the event that arms the next clock. Re-arm inside the
  expiry for any decline whose event is "none."
- Put the never-re-arm declines (architect, tree gone) ahead of the re-arm branch.
- Cancel the map in `dispose()`; set `disposed` before cancelling.
- Tests: fire-once positive; fire-once negatives per guard; stale-fire no-op; dispose-then-fire
  no-op; and one **state-mutation-between-fires** test per re-arming decline.
