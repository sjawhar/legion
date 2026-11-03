---
title: "A cost that must not grow with N is judged by median ratios on a fixed input, with the old loop beside it"
category: testing
tags:
  - benchmark
  - performance
  - acceptance
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2447"
---

# A cost that must not grow with N is judged by median ratios on a fixed input, with the old loop beside it

When an acceptance line says a cost "no longer grows with N":

- State the line as the ratio of medians from `-count=5`, at the high N against the low N:
  `B/op` and `allocs/op` at ≤1.2, `ns/op` at ≤1.3. In a worker pod, time ratios move by about 0.3
  between runs of one commit, and memory ratios by about 0.02. Quote the base's ratios beside the
  head's.
- Keep the input identical across N. For example, place more marks than the largest N once, and
  look up the first N of them. Placing N marks changes the document, because each mark splits text
  runs at its edges, and the benchmark then measures a different document at each N.
- Put the old per-item loop in the same benchmark as a sub-benchmark. Its growth shows that the
  benchmark can detect growth.
- Run the same benchmark file unchanged at base, in an exported tree of the base commit. Use only
  APIs and helpers that already exist at base.
- Each iteration that writes runs in its own transaction and rolls back, so no iteration leaves the
  next more state. Before timing, assert that one write leaves the fixture's rows and events as they
  were.

## Evidence

- In sjawhar/legion#2447 (plan AC3), `docs/anchored_write_bench_test.go:33` and
  `pmdoc/find_marks_bench_test.go:11` hold the benchmarks.
- The anchored-comment `ns/op` ratio at head read 0.97, 0.92, 0.90, 1.05, and 1.24 in the tester's
  run at the same head as the 1.05. `B/op` and `allocs/op` stayed at 1.01–1.03. At base the ratios
  were 3.79–5.37 (`ns/op`) and 4.74–7.01 (`B/op`), and the per-key `FindMark` loop grew 7.2–8.3×.
- The first `BenchmarkFindMarks` placed N marks for each N. FindMarks then read 1.49× at 150 KB,
  because the 80-mark document had more runs than the 10-mark one. Marking once with 100 marks and
  taking the first N gave 1.05–1.15×.
