---
title: "A hook pair that brackets a decision checks only the steps between its hooks"
category: testing
tags:
  - test-hooks
  - invariant
  - vacuous-fixture
  - negative-control
  - dispatch-settlement
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2467"
---

# A hook pair that brackets a decision checks only the steps between its hooks

- When a test checks an invariant with two hooks (one where a value is first read, one where
  production decides on it), list the code that runs between the two hook calls. The test checks
  that code and nothing else. A fixture or a comment that names a step outside the window is
  vacuous: remove it, or move a hook.
- Hand the closing hook the decision production computed (compute it once into a local and pass
  that local to both the hook and the branch), never a value the test recomputes. A recomputed
  decision can agree with the test and differ from production.
- Count each branch of the decision per key and assert the counts in every test that installs the
  hooks, so a test that never reaches the branch it checks fails instead of passing.
- Put each negative-control mutation inside the window. A mutation outside it proves nothing about
  the check.
- When the change under test runs after the closing hook, say in the pull request that the hook
  test does not cover it, and prove that change another way.

## Evidence

- `packages/envoy/internal/dispatch/docs/service.go`: `afterSettleTreeRead` fires at :1502 and
  `beforeSettleVersionRender` at :1676 with `reused`, the local the branch at :1678 also reads
  (:1674). The mark sweep this pull request made cheaper (`sweepUnrecordedMarks`, :1829) runs after
  the closing hook and after the commit.
- The first version of `TestSettlementReusesTheRenderOnlyOfAnUntouchedRead`
  (`settlement_read_invariant_test.go`) seeded a recorded comment mark and an unrecorded mark and
  said the read stayed unchanged through "its listing" of them. The review found the listing runs
  outside the window; `63ebec0987` removed the fixtures and the clause.
- `requireCounts` asserts one reuse and no render in the new test, and no reuse and one render in
  the two tests of `settlement_render_once_test.go` (:71 and the stamp test), so neither path is
  vacuous.
- The negative controls that hold: an unrecorded `!` appended to `read` right after
  `afterSettleTreeRead`, and `changedTree()` forced to `false`. Each fails on the helper's "changed
  after the read" assertion.
