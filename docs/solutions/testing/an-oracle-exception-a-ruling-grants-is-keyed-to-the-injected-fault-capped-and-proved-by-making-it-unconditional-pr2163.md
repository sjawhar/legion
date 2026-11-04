---
title: "An oracle exception a ruling grants is keyed to the injected fault, capped, and proved by making it unconditional"
category: testing
tags:
  - property-test
  - oracle
  - fault-injection
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2163"
---

# An oracle exception a ruling grants is keyed to the injected fault, capped, and proved by making it unconditional

- When a ruling accepts a behaviour a property test's oracle flags, do not loosen the invariant.
  Have the fault injector record which event it failed (an outbox row, a request id), and excuse a repeat only when the earlier event it repeats is one of those.

- Cap the exception: one repeat per recorded fault, and none on top of another exception.

- Cite the ruling beside the check.

- Prove the exception is not a hole: make it unconditional in a scratch copy and show a case that must fail now passes;
  then show the cases outside it (a third repeat, a repeat with no fault) still fail.

## Evidence

The kept-decision model (`packages/daemon/internal/daemon/kept_decision_property_test.go`) flagged seed 41:
a catch-up told a decision, its own telling write failed by injection, and a later catch-up told it again, which the architect ruled acceptable (https://github.com/sjawhar/legion/pull/2163#issuecomment-6542200410).
The injector `keptForgetStore` records the failed telling by the outbox row its publish went out under (`failedTellings`, `:347`, read by `tellingFailed`, `:364`), and `keptRun.tell` (`:3162`) excuses a second telling only when that earlier telling's row is recorded, at most once and never beside a retold task.
A throwaway driver of `tell` showed a repeat with no failed write, a third telling and a retold prompt after a failed catch-up each still rejected;
the tester made the exception unconditional and saw a double telling with no failed write pass.
