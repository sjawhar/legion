---
title: "A per-call bound that only names a hang is derived from the run's own deadline"
category: testing
tags:
  - go
  - timeout
  - t-deadline
  - race-detector
  - loaded-runner
  - flaky-tests
date: 2026-11-03
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2699"
---

# A per-call bound that only names a hang is derived from the run's own deadline

A fixed wall-clock bound over a measurement whose wall time scales with contention is not a bound:
it is a second failure mode. Under `-race`, on a shared runner, a healthy call takes whatever the
machine gives it.

- Where a per-call timeout exists only so that a hang fails by name rather than as the package's
  whole-run timeout, derive it from the run's own deadline: `t.Deadline()` minus a reporting
  margin, capped for a run with no deadline. Do not pick a constant.
- Fail by name when the deadline leaves less than the margin. That is a misconfigured run, not a
  hang, and it should say so.
- Do not raise the constant instead. A raised constant moves the cliff and keeps the shape that
  made the test fail on a loaded machine, and it costs the next reader the same measurement.
- Record the measurement that forced it in the comment beside the bound: the healthy call's
  observed time, on which machine, against the bound it replaced. The next person to touch the
  number needs it, and a number with no measurement invites another guess.
- Keep each copy of this shape local, with its own constants and failure text, when the call sites
  are in different packages. A cross-package helper here buys one definition and costs every
  reader a jump.

## Evidence

sjawhar/legion#2699 (INFRA-436) replaced a fixed 60 s per-call bound in
`packages/envoy/internal/dispatch/pmdoc/linear_test.go` with `perCallDeadline(t)`:
`t.Deadline()` less `perCallReportMargin`, capped at `perCallHangCap`. The comment carries the
measurement — a healthy 1 MiB call took 50.26 s, 84 % of the bound it replaced, in a two-CPU gVisor
pod — inside a file whose two linearity tests took 1849.7 s loaded and 1410 s unloaded under
`-race`. `packages/envoy/internal/broker/enroll/serving_test.go` took the same shape independently
for its own wait.

The rejected alternative is on record: sjawhar/legion#2495 raised the same constant to two minutes
and dropped the mebibyte case, which moves the cliff and loses the shape the mutation control has
to fail on.
