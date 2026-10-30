---
title: "An expected-gain ratio belongs to the surface it was measured on"
category: testing
tags:
  - load-test
  - pprof
  - merge-rule
  - performance
date: 2026-10-29
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2479"
---

# An expected-gain ratio belongs to the surface it was measured on

Extends docs/solutions/testing/a-gain-smaller-than-the-run-to-run-spread-is-judged-by-the-functions-profile-share-pr2429.md.

When the merge rule's share clause takes an expected gain `E = base share × r`, with `r` the
fraction of the function's time the change removes:

- **Measure `r` on the workload the gate runs**, or say why it transfers. A ratio timed on a corpus
  benchmark predicts more than a load-test profile share shows.
- **Name the two usual reasons before the head runs.** The profile share's measure leaves work out
  (the fold share subtracts GC), while a wall-clock benchmark includes it. And a synthetic seed
  lacks the input the change attacks (no inline marks, fewer texts), so the changed code weighs
  less in it.
- **Record where `r` came from**: which corpus, a quiet or a busy box, how many runs. Report
  whether the verdict changes with it. A first timing taken beside a running test suite gave half
  the ratio a quiet box gave.
- When the clauses disagree only through `r`, let the CPU clause decide, as the rule allows, and
  write every `r` with its bar in the pull request.

## Evidence

- sjawhar/legion#2479, body section A4. The fold share fell 1.65 points, against a bar of 1.73
  (`r` = 0.14, from the planner's 10 specs), 1.63 (`r` = 0.132, a busy box) and 3.03 (`r` = 0.246,
  on 1,204 LINUX specs on a quiet box). The load-test fold fell about 11 % against about 25 % on
  the corpus.
- The tester's independent runs on another base gave the same split: `r` = 0.14 passed and
  `r` = 0.246 failed. Clause 1 (every head run below every base run) decided both times, which
  the tree's architect accepted.
