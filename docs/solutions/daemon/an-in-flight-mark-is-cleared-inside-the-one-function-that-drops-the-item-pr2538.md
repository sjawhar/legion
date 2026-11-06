---
title: "An in-flight mark is cleared inside the one function that drops the item, and a fold re-reads what it can now change"
category: daemon
tags:
  - delivery
  - restart-recovery
  - invariants
  - refactoring
date: 2026-11-02
status: active
module: packages/daemon/internal/supervise
related_issues:
  - "INFRA-484"
  - "sjawhar/legion#2538"
---

# An in-flight mark is cleared inside the one function that drops the item, and a fold re-reads what it can now change

- When a field means "something may already be out there" — a restored claim's unconfirmed
  delivery, a pending acknowledgement, a dedupe token — clear it **inside the function that
  removes the item**, and make that function the only removal path. Clearing it in each caller is
  a rule every future caller has to remember, and forgetting it is silent.
- Find the paths by grepping the **field's writers**, not the function's callers. A path that
  removes the item by another route is the one that will be missed.
- A field set only in a recovery path has coverage equal to the **cross product** of that recovery
  and every path that can remove the item. Write that matrix down when you add such a field: a
  removal path with no restart test has no coverage of the field at all.
- After folding a step into a shared helper, **enumerate the state the helper can now change and
  re-read every predicate the caller evaluated before it**. A guard that ran before the fold may
  now be stale by the time it matters.

## Evidence (INFRA-484, sjawhar/legion#2538)

`askFirst` is armed on a daemon restart and means "the agent may be in a turn, and I do not know
whether that turn is the pending delivery's". It is answered once. Left armed with **nothing
pending**, it makes the *next* task the running turn's — confirmed, never prompted — so that task
is retired without the worker ever receiving it.

One round cleared it for the two drops inside `sendPending` and wrote a comment claiming the rule
outright: *a dropped delivery leaves no mark armed for it*. Three other drops went through
`dropUpdateTask` and left it armed — `leaveRound`, `endPodUpdate`'s refused-relaunch branch, and
`openPodUpdate`'s round switch — and `sendPending` then returns early at `p == nil`, before the
block that would have answered it. The comment was false of the code for a whole round.

The second round folded the clearing into `dropUpdateTask` itself, so no caller can forget it.
That fold then produced the other half of this rule: the clearing can probe the agent and move the
claim to `StateWorking`, and `endPodUpdate` went straight on to `clearUpdate` and `replaceProcess`
— cutting the very turn the guard above it ("ends for nothing while its worker is in a turn")
exists to keep. The guard had been evaluated before the drop; after the fold it had to be read
again. Removing the re-read reddens the test with `state = launching, want working`.

Why the suite was green through both: the mark is set **only** on restart, so a drop path tested
without a restart exercises none of it. Four tests now cover the cross product, one per drop site.
