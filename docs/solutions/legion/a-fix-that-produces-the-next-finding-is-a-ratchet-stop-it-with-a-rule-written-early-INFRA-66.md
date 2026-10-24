---
title: "A fix that produces the next finding is a ratchet: stop it with a rule written before it is needed"
category: legion
tags:
  - review-rounds
  - stopping-rule
  - scope
  - process
date: 2026-10-23
status: active
module: skills/legion-architect
related_issues:
  - "INFRA-66"
  - "sjawhar/legion#2087"
---

# A fix that produces the next finding is a ratchet: stop it with a rule written before it is needed

- When a mechanism's fix produces the next round's finding **in the same mechanism**, you are in a
  ratchet, not a review. Each finding is legitimate on its own terms, which is why counting
  findings cannot detect one.
- Write the stopping rule **before** it is needed and record it where the next round will meet
  it: the next structural move on this mechanism is a removal, not an addition; the remainder is
  a disclosed limit with a follow-up issue named.
- Judge a sequence of remedies by whether they converge on **less** machinery. A round that adds
  a map, then one that adds a bound, then one that deletes state by moving an invariant into the
  component that owns it, is converging. Three rounds that each add state are not.
- A remedy is a claim, and the rule holds for remedies the reviewer or the architect proposed as
  much as for the implementer's. Two of this change's remedies came from the reviewer and were
  measured insufficient afterwards.
- Carry what was *not* taken into the merge packet: the better shape deferred, with its reason,
  and every residual. The pull request body is where a person meets them; the merger quotes them
  into READY.

## Evidence

One change — making a single-process server correct at N processes — took eight review rounds.
Four times the fix produced the next round's finding, each time in the stale-room evictor and its
listener:

- A throttle that **dropped** what it refused became a deferred set (a map added).
- The deferred set's window was spent by attempts that reached no verdict, so verdict-less paths
  re-armed (a rule added).
- The re-arm held the listener's wake permanently short and retired its idle ping, so the re-arm
  gained a ceiling (a bound added).
- The ceiling could not bound a **minimum over rooms**, so the invariant moved into the component
  that owns the connection and the ceiling became a rate bound (state deleted, dependence
  removed).

The sequence stayed honest because it converged. The architect's stopping rule was written at the
fourth of those rounds, before it was needed, and is what made the fifth round's answer
"correct the sentences that teach the retired model" rather than a fifth mechanism.

Two counts worth keeping as the honest shape of such a change: every one of the eight rounds
removed a false statement from the tree, and twice the false statement was a claim that a
correction had been completed.
