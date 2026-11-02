---
title: "Two base runs decide whether the box can resolve the effect, before the full set is spent"
category: testing
tags:
  - performance
  - measurement
  - cpu
  - replay
date: 2026-10-31
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2522"
---

# Two base runs decide whether the box can resolve the effect, before the full set is spent

Extends docs/solutions/testing/compare-two-base-runs-before-an-end-state-diff-and-use-a-serial-replay-when-they-differ-pr2447.md,
which compares two base runs for end-state determinism. The same two runs answer a second
question first, and this one decides whether the rest of the measurement is worth running.

- **Run base twice, then compare their spread with the effect you expect.** When the spread is a
  large fraction of that effect, this box cannot resolve it: stop and change the method, rather
  than spending the full alternating set to produce a number no one can act on.
- **Take runs as adjacent pairs, base then head, back to back on one machine**, and record what
  machine each pair ran on. A set interleaved across hours or pods measures the machine as much as
  the change.
- **State the decision rule before the runs**, and report the runs that fail it. "The median head
  run uses no more than the highest base run" is a rule; picking the comparison after seeing the
  numbers is not.
- **A spread you cannot explain is a finding of its own.** Say which machine, how loaded, and what
  else was running, so the next reader knows whether to trust the set or re-take it.

## Evidence

- In sjawhar/legion#2522 the first six alternating replay runs, taken over about four hours, gave
  base 422.01 / 415.71 / 402.50 CPU-seconds — a spread of 19.51, 4.6 % of the highest base — and
  head 404.69 / 385.15 / 393.06, a claimed gain of 28.95 (6.9 %). The error bar was two thirds of
  the effect, and one head run (404.69) sat above one base run (402.50), which the body had to
  concede.
- Three adjacent base-then-head pairs on one pod, taken later, gave a base spread of 1.76 (0.4 %),
  every head run below every base run, and a gain of 18.34 (4.5 %) — about 60 % of the figure the
  scattered set claimed.
- The acceptance quoted the paired runs alone. The four hours of the first set decided nothing,
  and two base runs compared at the start would have shown why.
