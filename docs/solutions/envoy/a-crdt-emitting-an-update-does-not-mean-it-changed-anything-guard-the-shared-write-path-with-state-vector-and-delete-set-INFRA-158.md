---
title: "A CRDT emitting an update does not mean it changed anything: guard the shared write path with the state vector and the delete set"
category: envoy
tags:
  - ygo
  - yjs
  - document-writes
  - settlement
  - idempotency
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "INFRA-158"
  - "sjawhar/legion#2233"
---

# A CRDT emitting an update does not mean it changed anything: guard the shared write path with the state vector and the delete set

- ygo (and Yjs) hand a document's update observers an update for every transaction, including one
  that changes nothing. Never take "an update was emitted" as "the document changed".
- Decide whether a write changed anything by comparing the document's state vector and its delete
  set before and after. A deletion creates no struct and advances no clock, so the state vector alone
  calls a pure delete a no-op.
- Put that check in the one path every writer goes through, not in the writer that showed the bug.
  A write that changes nothing then appends no update, refreshes no anchors, and leaves the
  document's owed settlement marked as it was.
- For any repeated pass (an interval sweep, a repair), prove that a second pass over unchanged data
  writes nothing.

## Evidence

- In sjawhar/legion#2233, a repair pass that changed nothing appended a 2-byte update and marked the
  document as owing a settlement. Every later interval pass would then have swept every closed issue's
  documents again.
- The first fix guarded only the ask-state writer. Review round 2 moved the check into the shared live
  write: `packages/envoy/internal/dispatch/docs/mutation.go` (`applyLive`: "the updates the fork emits
  cannot say it, for they include one for a transaction that changes nothing") and `livewrite.go`
  ("A deletion advances no clock, so the delete set is half the answer").
- `TestARepairThatChangesNothingAppendsNoUpdateAndMarksNoDocument` in
  `packages/envoy/internal/dispatch/api/issue_ask_repair_test.go`.
