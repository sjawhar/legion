---
title: "A review's remedy and the figures behind it are two claims; report the gap rather than closing it"
category: best-practices
tags:
  - code-review
  - review-rounds
  - evidence
date: 2026-11-04
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2705"
---

# A review's remedy and the figures behind it are two claims; report the gap rather than closing it

A review that names both a fix and the numbers that motivated it has made two claims, and they
can disagree. When they do, the remedy is the suspect, because the numbers came from whatever the
reviewer actually computed.

- Implement the prescribed remedy exactly as written, then compute the figures it produces.
- When your figures do not match the reviewer's, say so in the round, with both sets of numbers
  and the input each was taken at, and stop there. That disagreement is the finding.
- Never close the gap by adding a term that makes the numbers come out right, and never close it
  by argument that your figures are the correct ones. Either way a wrong model ships under an
  approval, with the reviewer's own numbers as its evidence.
- The same rule binds the reviewer: a round whose only answer is "the implementation follows my
  words" has not read the numbers again.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454), the lease model, round 2.

Round 1's review prescribed a model where each refused renewal either fails at once or is held to
the whole 30 s client timeout, and quoted the floors that model gives: 330 s, 360 s and 105 s at
the sweeps it named. Round 2 implemented exactly that and printed 322 s, 351 s and 97 s. The
implementer reported the disagreement rather than reconciling it.

Round 2's review answered: "One finding stands, and the fault is mine: my round-1 comment on the
lease model asked for two outcomes per renewal. The figures I quoted came from a model where a
renewal can fail at any instant inside that 30 s." The reported gap is what located the real
defect — the two-outcome model is not a bound at all
(`docs/solutions/architecture-patterns/a-worst-case-over-a-retry-loop-takes-each-failure-instant-from-a-continuum-and-walks-one-interval-pr2705.md`).

Had round 2 instead tuned the model until it printed 330/360/105, the published floors would have
been right by accident at three sweeps and wrong at the rest, under a reviewer's approval and a
tester's pass.
