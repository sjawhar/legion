---
title: "A test that needs an input past a production cap lowers the cap on its own instance"
category: testing
tags:
  - race
  - test-cost
  - fixtures
  - caps
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# A test that needs an input past a production cap lowers the cap on its own instance

Extends docs/solutions/testing/a-new-tests-race-cost-is-charged-to-a-package-already-near-its-lanes-limit-so-size-the-fixture-by-the-cases-pr2497.md.

- When a test must build an input past a production limit, and the limit sets the input's size, do not size the fixture against the constant. Make the limit a field of the instance under test, set to the constant by its only constructor, and let the test lower it on its own instance and restore it.
- Do not use a package-level variable that a test overwrites: every other test in the package would run under the lowered value.
- State the cap's rule once, at the constant, and name each field that holds it.
- Any code that judges a failed decode must use the cap that decode ran under, not the constant.

## Evidence

- Review round 6 of sjawhar/legion#2444: `internal/dispatch/docs` timed out under `-race` at 1801 s twice with the branch merged into main. Four tests of a log that does not fold each built about a million items to pass ygo's pending queue.
- `85b8b216`: `PgVersioned.maxPendingItems` (set in `NewPgVersioned`, `docs/persistence.go`) and `Service.maxPendingItems` (which sets `Server.MaxPendingItems`). `shrinkPendingItems` and `testPendingItems = 64` (`docs/history_fold_test.go`) let those tests use 33-item rows with the same assertions. The four tests fell from 88, 50, 31 and 28 s to 25, 0.7, 0.4 and 0.4 s.
- `itemCapError(update, maxItems, err)` (`docs/persistence.go`) takes the cap of the decode it judges (review round 7).
