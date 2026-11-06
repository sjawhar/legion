---
title: "A long-lived state gets the exits you can enumerate, and a window is removed rather than guarded"
category: daemon
tags:
  - state-machine
  - reconciler
  - races
  - update-round
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "INFRA-484"
  - "sjawhar/legion#2538"
---

# A long-lived state gets the exits you can enumerate, and a window is removed rather than guarded

- When a state can last hours and external events arrive while it holds, **write its exits down
  and keep them minimal**. Each extra exit is a branch every later change must reason about, and
  the ones that accumulate are the ones nobody chose deliberately.
- Remove a race window by making the transition happen **at the read that discovers the cause**,
  not by adding a guard over the window. A guard leaves the window and adds a condition; moving
  the decision deletes both.
- An end **held across a turn** is an exit you will not be able to reason about. It outlives the
  thing that scheduled it, and whatever it is keyed on can be true again for a different reason
  by the time it runs.
- When a facet moves from one mechanism to another and the new one is chosen by a predicate,
  enumerate **every kind × every facet** and name the owner of each cell. A facet removed from the
  old mechanism and not covered by the new leaves an object nothing ever replaces, and no existing
  test fails for it.
- When round after round of review finds defects in the same branch of the code, the branch is
  the finding. Deleting it is cheaper than patching each path through it.

## Evidence (INFRA-484, sjawhar/legion#2538)

The invariant as shipped: **a round, once opened, ends only by replacing its pod, or because that
pod is gone.** The drift clearing ends none — every worker in the pod has already been told to
push and to start no new work, so the only honest end left is the restart each of them was asked
for.

That invariant was not the first design. Rounds 3 to 6 each found a defect in the same place: how
a round ended *without* replacing its pod. That branch carried a carry-on task, an end held for a
turn, and a release of answered workers; each round patched one path through it and left the next.
The architect's decision at the round cap was to delete the branch rather than patch it again,
which removed four defects at once and changed nothing the issue delivers.

Two of that round's fixes are the window rule in action. A pod gone while its round was open had
been decided at the sweep that read the claims recording the pod that replaced it — leaving a
window in which each relaunched role's turns and answers were counted against the old pod's round.
The fix ends the round **at the first sweep that reads the pod gone**, so the window is removed
rather than guarded. And `Machine.roundOver`, its run at a turn's end, and `PodUpdateEnded.Replaced`
were deleted, so no end is held across a turn: a held end survived a round reopened over the same
pod, whose id is the same, and told the worker that the new round was over.

A later round named the remaining forbidden exit explicitly in the code, where the sweep finds no
live claim recording the round's pod: it asks the runtime about the pod itself and ends only if it
is gone, because ending there with the pod still running would be the third way out of a round
that must not exist.

The kind × facet table is the same change's other half: three facets — the secrets broker's
enrollment, a role's overlays, the session store — moved out of the launch's replacement and into
the round, and no round is ever opened over the project controller's pod. The controller would
have run for ever in a pod built before the deployment changed. A tester's red test found it, and
the fix is a fall-back for every kind no round covers.
