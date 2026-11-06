---
title: "A forward-merge census of a rule follows the record it guards, not the function you edited"
category: architecture-patterns
tags:
  - forward-merge
  - census
  - credit
  - dispatch
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2485"
---

# A forward-merge census of a rule follows the record it guards, not the function you edited

Extends docs/solutions/architecture-patterns/a-rule-main-adds-beside-a-resolver-your-branch-shares-stays-in-the-one-write-path-that-asks-it-and-shares-only-its-predicate-pr2395.md and docs/solutions/daemon/a-forward-merge-brings-mains-new-outbox-writer-in-unscoped-the-commit-scope-census-runs-again-and-the-racy-wakes-error-line-is-the-tests-witness-LEGION-694.md.

- When your branch adds a rule to a decision (who is credited, who is authorized, what is recorded), the census you run at every forward merge is every path that writes that record, found by searching for the record's field or the write itself, not for the function your branch edited.
- Main can add a second writer under a new name. It merges with no conflict marker, and a search for your function's name never finds it.
- Put the rule in one named predicate and call it from every writer, so a writer that skips it is visible in review.
- Add a test that drives the new writer, and check that it fails with the predicate removed from that writer alone.

## Evidence

- This branch made a commit with no origin credit no one, in `creditContentChange` (`docs/websocket.go`, ac4cff72); round 2 closed with "the one nil-origin rule is `creditContentChange`'s".
- Main's 05d65789 added `unchangedCredit`, which writes the same credit record (`class.roomCredit`, `websocket.go:752`) for an open write's shadow to store (`storedClasses`, `shadow.go`). Merge 2164df49 resolved its six conflicts without touching it, and the merge notes said the rule was unchanged.
- Round 5's review reproduced a nil-origin commit stored with the connected browser as its author. 8a4db0df added `originCreditsNoOne` (`websocket.go:833`), called at `:855` and `:878`, and `testANilOriginCommitAnOpenWritesShadowMeasures` (`nil_origin_credit_test.go:506`) fails at `:562` with `unchangedCredit` back on main's check.
