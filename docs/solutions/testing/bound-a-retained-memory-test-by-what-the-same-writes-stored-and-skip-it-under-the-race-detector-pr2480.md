---
title: "Bound a retained-memory test by what the same writes stored, and skip it under the race detector"
category: testing
tags:
  - memory
  - race-detector
  - test-design
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2480"
---

# Bound a retained-memory test by what the same writes stored, and skip it under the race detector

Extends docs/solutions/testing/judge-no-new-resident-memory-by-in-use-totals-and-a-state-census-not-by-heap-site-names-pr2427.md
and docs/solutions/testing/a-new-tests-race-cost-is-charged-to-a-package-already-near-its-lanes-limit-so-size-the-fixture-by-the-cases-pr2497.md.

- Bound the heap a structure retains after N writes by what those writes added to the store,
  plus a small fixed allowance: `Δretained ≤ Δstored + C`. Do not use an absolute byte budget.
  The relative bound holds as fixtures grow, and fails only when memory outgrows the stored data.
- Read the heap after two `runtime.GC()` calls (a `sync.Pool`'s contents survive the first), and
  take the least of three `HeapAlloc` readings. Before each reading, drain the work the writes
  queued, and assert that the measured structure is still resident at both ends, so the test
  cannot pass because the structure was evicted.
- Skip such a test under `-race`, through a build-tagged constant in a `_race`/`_norace` pair of
  `_test.go` files, with the reason in the skip message. The detector's shadow memory distorts the
  heap these tests measure. They check no concurrency, and under `-race` they cost minutes of the
  package's lane budget.

## Evidence

`packages/envoy/internal/dispatch/docs/upload_memory_test.go` (`roomAllowance`, `measureRoom`,
`skipUnderRace`) with `race_enabled_test.go` and `race_disabled_test.go`, in sjawhar/legion#2480.

- Without `-race`, 200 uploads retained 135 KB against a bound of 2.27 MB at the head, and
  97.9 MB against 18.9 MB on main.
- Under `-race`, the three tests took 69.7 s, 85.1 s and 65.5 s. CI run 40942207302 timed the docs
  package out at 1,801 s.
- With the skip, the package took 2,255 s under `-race` on a 2-vCPU pod, against 2,186 s on main,
  and `envoy-go-race` and `envoy-go-race-docs` passed at `b0a59b0b`.
