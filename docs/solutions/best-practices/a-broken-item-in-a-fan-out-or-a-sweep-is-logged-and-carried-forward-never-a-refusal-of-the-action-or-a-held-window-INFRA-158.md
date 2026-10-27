---
title: "A broken item in a fan-out or a sweep is logged and carried forward, never a refusal of the action or a held window"
category: best-practices
tags:
  - fault-isolation
  - sweeps
  - repair-pass
  - side-effects
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "INFRA-158"
  - "sjawhar/legion#2233"
---

# A broken item in a fan-out or a sweep is logged and carried forward, never a refusal of the action or a held window

- When an action has a side effect on many items (each document of an issue, each asker), an item
  that can never be processed is logged and skipped. The action goes on. One broken item never makes
  the whole action fail.
- When a periodic sweep keeps a window (a "since" mark), it moves the window every pass that listed
  its candidates. The items that failed are carried into the next pass and retried by name. One item
  that fails every pass never holds the window back, or every pass would sweep everything again.
- For each such side effect or sweep, write a test with one permanently broken item. It proves that
  the action still succeeds, or that the window still moves, and that the item is retried.

## Evidence

Review round 1 of sjawhar/legion#2233 found both cases. The plan had found neither. Both were fixed in
a60624f0:

- One document whose stored history could not load made the whole close answer
  `409 DOCUMENT_UNLOADABLE`. Now it is logged and skipped:
  `TestClosingAnIssueWithAnUnloadableDocumentWithdrawsItsAsksAndReopeningRestoresThem` in
  `packages/envoy/internal/dispatch/api/issue_close_unloadable_test.go`.
- One issue that failed every repair pass kept the window from moving, so every pass swept every
  closed issue. Now the window moves and the failed issues are carried in `unrepaired`
  (`api/issue_ask_repair.go`):
  `TestARepairPassThatFailsOneIssueStillMovesTheWindowAndRetriesIt` in
  `api/issue_ask_repair_test.go`.
