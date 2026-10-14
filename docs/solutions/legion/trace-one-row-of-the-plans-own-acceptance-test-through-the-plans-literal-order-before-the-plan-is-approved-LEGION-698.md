---
title: "Trace one row of the plan's own acceptance test through the plan's literal control flow before the plan is approved, and name the tie-break where two independent rules meet at one fact"
category: legion
tags:
  - planning
  - plan-review
  - acceptance-tests
  - two-rules-one-fact
  - specDepartures
date: 2026-10-14
status: active
module: skills/legion-architect
related_issues:
  - "LEGION-698"
  - "sjawhar/legion#1950"
---

# Trace one row of the plan's own acceptance test through the plan's literal control flow before the plan is approved, and name the tie-break where two independent rules meet at one fact

- When a plan orders two rules as a fall-through ("case A ends the round; case B sends the work
  back; then …"), the planner and the plan reviewer hand-trace at least one row of the plan's own
  named acceptance test through that exact order, fact by fact, before approving the plan. A row
  whose first fact already takes the second branch (the round is not yet complete at the approval,
  so the red branch fires and the completion is refused) is unreachable under the plan, and the
  implementer meets the contradiction only once the test is written.
- When the spec says two rules are independent and names no order, the plan's tie-break for the
  one fact where both hold is a decision to record (a `D<n>` with its reason), and the
  implementation that departs from it is a `deviations` entry the reviewer reads beside the plan,
  never a silent choice. Apply the tie-break where the two rules actually meet (here the hold's
  retry and the late push), not in a function one of the rules has left.
- An implementer who finds a plan row unreachable does not weaken the row: it makes the rule the
  spec states literally true (the round read no CI at all), writes the row the spec implies, and
  tells the architect which plan decision it reversed and why, with the reachable state where the
  two orders differ.

## Evidence

LEGION-698's plan D1 kept `reviewRound`'s order as `roundApproved` then `roundSentBack` and named
`TestAnApprovalEndsTheRoundWhateverCISays` with red rows "each with the approval and the completion
in both orders, each ending in `phase.Retro`". Under that order the approval fact, with a red
standing, finds the round incomplete and falls through to `RedSendsBack`, which sends the work to
implementing and has the completion refused `HANDOFF_NOT_CURRENT_PHASE` — the row's failure on
`main`, and the plan's own order would have failed it too. The plan passed its review in one round.
The implementation took the CI read out of the round (`standingRed`, applied by the facts that make
a red stand) and recorded the one reachable asymmetry — a hold's retry or a late push that finds
both an approval and a standing red sends the work to implementing, where D1 said retro — as a
deviation the architect confirmed (`.legion/LEGION-698/implement.json` on the branch, PR #1950).
