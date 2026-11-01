---
title: "When a sibling merge takes the measured gain, judge each part on evidence of its own"
category: testing
tags:
  - merge-rule
  - measurement
  - load-test
  - benchmark
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2497"
---

# When a sibling merge takes the measured gain, judge each part on evidence of its own

Extends docs/solutions/testing/a-gain-smaller-than-the-run-to-run-spread-is-judged-by-the-functions-profile-share-pr2429.md.

- Before the head runs, fetch main and read its diff of the functions your expected gain names.
  If a sibling change removed that work, the gain against the main that will merge is gone: stop
  the runs on the old base, merge main forward, and tell the architect before more runs.
- Then keep each part of the change only on evidence of its own:
  - A correctness part merges with a test that fails on main and passes at the head.
  - A part that only reorders a refusal merges only if a rolled-back refused request leaves
    something observable on main (rows, events, broadcasts, the room) that it does not leave at
    the head. Also check that the API can reach the refusal at all. If neither holds, drop it.
  - A performance part on a path the replay never sends is measured by a benchmark of that
    operation on real inputs (benchstat, n of 8 or more, base and head alternating). Its
    production share is written as not proven.
- Write up the CPU part on the parent issue with the base numbers and the expected gain that you
  stated before any head run.

## Evidence

sjawhar/legion#2497 stated an expected gain of 0.85% of samples (MarkQuote's third tree read,
`MarkQuote.func1 -> treeOf`) from a base replay run. Before the head runs,
sjawhar/legion#2448 merged and removed that read on main. The panic release kept a test that
fails with main's condition. Reading the anchor before the append was dropped: 464 calls with the
API's `MarkSpec` never reached the refusal, and a rolled-back refusal left identical rows, events,
broadcasts and room state on main and the head. The rendering reuse was kept on a benchmark of a
suggestion comment on 4 real specs (`SuggestionKind` −83.33%, p = 0.000), a request the replay
does not send.
