---
title: "A hold stops the pass and a decided skip moves on, and the last hold released is the one that admits"
category: daemon
tags:
  - admission
  - hold
  - waiting-line
  - regression-test
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2727"
---

# A hold stops the pass and a decided skip moves on, and the last hold released is the one that admits

Extends docs/solutions/daemon/a-hold-is-computed-with-the-check-the-action-that-releases-it-uses-INFRA-252.md.

That document says how a hold is computed and that every way it can end needs a test. This adds
what the pass does with the candidates *behind* the held one, and how a test of two overlapping
holds is written.

- **Classify every `continue` in a loop that hands out a scarce resource in rank order.** A *hold*
  means "this pass does not yet know enough to decide this candidate", and it must `break`: the
  resource is not taken back, so skipping the candidate turns a few seconds of ignorance into a
  permanent inversion of the order. A *decided skip* means "this candidate cannot run now", and it
  may `continue`, because the next pass decides it again. Both read the same in the code and both
  are described by the same sentence, "it keeps its place in the waiting line" — which is true of
  the place and says nothing about the slot.
- **Do not trust a loop's own claim that each pass repairs the state.** The claim holds for the
  resources a pass gives back; it does not hold for one already handed out.
- **A flag that bypasses the holds rarely bypasses all of them.** Read which hold the release path
  under test actually drops before you write the expectation. Where two holds hold one candidate,
  the pass that admits it is the one after the *last* release, not the first — a test written as
  "after the first release the held candidate is admitted" is red for its own reason, and the code
  is not what is wrong.
- **Assert the intermediate state, not only the end state.** Between the two releases, assert that
  the resource is still free and the line is unchanged. An end-state assertion passes with either
  hold doing the work.
- **Name the collection the loop iterates, and prove the seed reaches it.** A seed can be
  reachable in production and still invisible to the one list the loop walks, and such a test
  reads the same before and after the fix. Assert the subject is in that list as a precondition,
  before the act.
- **Turning a `continue` into a `break` deletes every remaining side effect of the loop body for
  every candidate behind the one that stopped it.** List them, decide each absence is correct, and
  pin none of them with a test — a test on a suppressed log line forbids the fix.

## Evidence

On sjawhar/legion#2727 the Legion daemon's admission pass skipped a root the boot held for the
Dispatch consumer (`continue`) and went on down the waiting line, so a lower-ranked root took the
one free slot and kept it until its tree ended. Every INFRA boot hit it: `record.Waiting` puts
answered roots first and the boot holds them, so the head of the line was the held root by
construction.

- The loop now breaks for either hold at `packages/daemon/internal/admit/admit.go:1150`. Three
  `continue`s remain in the same body and each is a decided skip: an answered root that may not
  resume (`:1164`, which `releaseUnslotted` takes out of the workflow on this same pass), a root
  Dispatch shows blocked (`:1174`, deliberate — `AGENTS.md` states that admission "admits the next
  unblocked root in rank order, then admits it at its own rank position once its last blocker
  closes"), and a tree whose cleanup or other authority holds it (`:1188`, `:1194`).
- `promote`'s doc comment at `:1061-1062` says each pass "repairs that state rather than keeping
  it". It is true of `releaseReturnedSlots` and `releaseUnslotted`, both named in the same
  sentence, and false of a slot already taken: nothing in the loop reclaims one before its tree
  ends. That sentence is why the skip looked harmless.
- `release` calls `a.promoteHolds(ctx, tx, !caughtUp)` (`admit.go:1508`) and `promoteHolds` sets
  `heldKeys = a.catchingUpFor` when `respectHolds` is false (`:1094-1096`), so the release bypasses
  the Dispatch-consumer hold and never the catch-up hold. A root in both holds is admitted by the
  catch-up's release. The plan recorded a spec departure for it, and the "both holds" case of
  `TestAHeldRootKeepsItsPlaceAndNothingBehindItTakesTheSlot`
  (`packages/daemon/internal/admit/hold_test.go`) asserts the intermediate state — the slot still
  free and the line unchanged after the consumer release — before the catch-up release takes it.
- The same test's seed comment records the invisible-fixture trap: `Reconcile` defers an unrecorded
  key too (`admit.go:482-489`), but `record.Store.LiveIssues` never carries one, so it never
  reaches `record.Waiting` and a line seeded that way locks nothing. Both roots are seeded with
  `putIssue`, and `assertWaiting` reads the line before the act.
- What the `break` suppresses behind the held root, for the hold's duration: the four log lines at
  `:1164`, `:1175`, `:1188` and `:1194`, and the `OpenTreeLifecycle` call at `:1186`. Nothing reads
  them — `legion state`'s blocked list is computed independently by `blockers.Blocked` — and
  `heldKeys`, `releaseUnslotted` and `adoptQueuedRoots` run before the loop while
  `StartUnblockedChildren` runs after it, unconditionally, so a blocker-held child of any live tree
  still gets its look.
- One of the three table rows, the catch-up hold alone, already passed before the fix: that hold
  already broke the loop, and the consumer hold's `continue` was checked first and masked it. It is
  a pin, not a lock, and the handoff says so.
