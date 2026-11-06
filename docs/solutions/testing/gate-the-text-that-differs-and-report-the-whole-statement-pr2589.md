---
title: "Gate the text that differs, report the whole statement"
category: testing
tags:
  - benchmarks
  - performance
  - bounds
  - postgres
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/benchtime
related_issues:
  - "sjawhar/legion#2589"
---

# Gate the text that differs, report the whole statement

Extends docs/solutions/testing/three-reds-one-shape-a-flaky-upper-bound-is-repaired-by-the-statistic-or-the-observable-pr2641.md.
That one repairs a flaky upper bound by changing what is observed or which statistic of it is held;
this one is about what a committed performance pair should observe in the first place.

- Gate a committed performance pair on the text that differs between the two sides,
measured in the smallest statement that runs that text and nothing else, with both sides built by one function from their own release's pieces.

- Where that text is a small share of the statement it lives in, report the whole statement beside the gate with no bound,
under a name that says it is not the gate, because the end-to-end number is what a reader of the gate asks for next.

- Say in the test why the bound is where it is, with the readings that show the whole statement's spread swallowing the gain.

- Tie the gate to the text that differs, never to the part that improved:
a pair over a statement whose unchanged remainder dominates it passes or fails on which way the machine drifted.

## Evidence

In sjawhar/legion#2589 `relatedKeywordQuery` and `relatedMeaningQuery` are gated on the document arm they share,
measured as one `Pair` under a name that says whose arm it is
(`packages/envoy/internal/dispatch/api/statement_timing_test.go:206-213`),
and each whole statement is kept beside it as a `Report` with no bound
(`:214-219`).

The test states the reason:
the arm "is a fraction of a second of a statement of about ten, whose other four arms, fusion, page and snippet are the same text on both sides:
what the arm saves sits inside the run-to-run spread of that unchanged remainder, and readings of the whole statement have come out both ways round —
on this database the after text read 10.9 s against the before text's 10.7 s while its document arm read 160 ms against 287 ms"
(`packages/envoy/internal/dispatch/api/statement_timing_test.go:188-205`).
The pull request body gives five readings of the whole statement, −8.5%, +0.9%, −3.7%, −1.7% and +0.1%, and concludes that the pair over it "gated nothing".

Both sides of the gated arm are built by one function, `documentLegStatement`,
"the smallest statement that runs one keyword document arm and nothing else: the check's own q, the term weights the arm's rank reads, the arm as a CTE of its own, and a count over it …
so the two readings differ in the arm and in nothing else"
(`packages/envoy/internal/dispatch/api/statement_timing_test.go:226-255`).

The two contracts it rests on are the harness's own.
`Pair` "measures a statement's before text against its after text under protocol 1 and fails when the after text's median is not below the before text's"
(`packages/envoy/internal/dispatch/benchtime/benchtime.go:134-149`),
and `Report` "measures the same two sides as Pair and bounds neither, for a statement whose gain is real but smaller than the run-to-run spread of the parts it shares a statement with:
the gate belongs on the piece that differs, and the two end-to-end numbers are still what a reader of that gate asks for next"
(`packages/envoy/internal/dispatch/benchtime/benchtime.go:151-160`).
