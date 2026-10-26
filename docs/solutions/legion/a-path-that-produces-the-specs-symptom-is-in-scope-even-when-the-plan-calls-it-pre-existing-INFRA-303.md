---
title: "A path that produces the spec's symptom is in scope, even when the plan calls it pre-existing"
category: legion
tags:
  - planning
  - scope
  - acceptance-criteria
  - tester
date: 2026-10-26
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-303"
  - "sjawhar/legion#2221"
---

# A path that produces the spec's symptom is in scope, even when the plan calls it pre-existing

- When the spec's acceptance is something a user sees ("opening the Conversation lands on the
  newest message"), every path that produces the opposite is in scope. That holds whether or not
  the bug predates the issue, or has a different cause than the one the spec traced.
- A planner who finds such a path fixes it in the plan. If a person must decide it, the planner
  asks the architect to decide it before planning ends. A decisions line calling it "unchanged and
  out of scope" is not enough. The tester tries each path a user can reach, so the plan sends the
  issue back a round later, after a full implement-test cycle.
- An implementer reading a plan with such an exclusion asks the architect before implementing,
  rather than shipping the round the tester will fail.

## Evidence

INFRA-303's plan read `IssuePage`'s tab-scroll restore and named the path. Leaving the Conversation
before its reads finish and coming back left the reader at the top: the parent's restore overrides
the first placement. The plan recorded it as "unchanged and out of scope (pre-existing, not caused by
the race)" (`plan.json` `decisions[2]`), after the gap analyst raised it. The tester wrote a red row
for exactly that path, the architect ruled it in scope, and round 2 fixed it with one callback
(sjawhar/legion#2221). Round 1's implementer followed the exclusion without asking. Asking then would
have saved one implement-and-test round.
