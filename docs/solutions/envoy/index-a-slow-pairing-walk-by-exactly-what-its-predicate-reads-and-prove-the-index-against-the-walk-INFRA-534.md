---
title: "Index a slow pairing walk by exactly what its predicate reads, and prove the index against the walk"
category: envoy
tags:
  - pmdoc
  - performance
  - linearity
  - rule-of-three
  - equivalence-check
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "INFRA-534"
  - "sjawhar/legion#2360"
---

# Index a slow pairing walk by exactly what its predicate reads, and prove the index against the walk

When a search back over earlier candidates is quadratic (a closer walking back past every opener
it may not use), and you replace it with an index:

- Name the minimal key the accept/reject test reads. Bucket candidates by it, so the test rejects
  each bucket all or none. Then the nearest acceptable candidate is the latest bucket tail the test
  accepts.
- Keep every side effect of the old walk on the candidate you choose. The old walk's processor call
  also normalised state that the measure reads next.
- Before you delete the old walk, run both on the same inputs behind a temporary switch, and count
  disagreements on a large random corpus. Remove the switch and the copy of the old walk in the
  same change.
- Add the exact shape that was quadratic to the linearity test. Read CPU-clock timing tests from
  CI runners: in a gVisor pod the small size can measure 0 s and fail with "+Inf times".

## Evidence

- At main, pmdoc's `ParseRendering` of `**a ` repeated, then `a*a` repeated, took 100 s at 437
  KiB. Each `*` of `a*a` can both open and close, so the rule of three kept it from every `**`, and
  its walk passed all of them.
- The rule of three reads only the opener's remaining length mod 3 and whether it can close.
  `nearestOpener` (`delimiters.go`) checks at most six bucket tails per closer. `ruleLength` names
  the length the rule reads: the remaining length for `*`/`_`, the written length for `~`.
  `advance` still calls `CanOpenCloser` on the chosen opener, because `emphasisDelimiters` sets
  `OriginalLength` there before `CalcComsumption` reads it.
- Before the walk was removed in 5d3a48a0, the old walk and the buckets were compared on 200,000
  random inputs: 2.7 million closer lookups, 0 mismatches.
- The shape now takes about 0.5 s at 1 MiB. `linear_test.go` holds `{"**a ", "a*a"}` and
  `{"a**a*a ", "a*a**a"}`. The comparison harness was not committed, so a later change to
  `nearestOpener` should rebuild it.
