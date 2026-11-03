---
title: "A settlement race test holds the peer at afterCreditUpdate and checks the version by its block rows"
category: testing
tags:
  - dispatch
  - settlement
  - race-test
  - test-hooks
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
---

# A settlement race test holds the peer at afterCreditUpdate and checks the version by its block rows

- When a peer's edit must land inside a settlement's window and the settlement must still write
  a version, hold the peer's observer at `service.afterCreditUpdate` (`holdPeerEdit(t, service,
  id, &service.afterCreditUpdate, edit)`), not at `beforeObserveUpdate`. Release it only after
  the settlement returns.
- The reason: a settlement's repair or stamp that changes nothing still commits a transaction.
  The observer of that empty update drains the room's update log, classifies the held peer edit,
  and arms a settlement. The generation moves, and the superseded settlement writes no version.
  `afterCreditUpdate` holds the peer after the drain and before `scheduleSettle`.
- If the version still does not appear, report it. Never weaken the assertion to "no version".
- Check a version against the quiet room by its markdown and also by its indexed block rows:
  `(block_id, type, sha256(markdown[From:To]))` for the first offset of each id, compared with
  `renderTreeWithOffsets` of the live tree. After a stamp the markdown can match while the
  offsets and ids differ. Reuse `roomRendering` and `requireIndexedBlocks`
  (`settlement_render_once_test.go:153,165`).
- Give each half of a compound condition a test of its own, and remove each half in turn as a
  negative control. Each test must fail on its assertion, not on a timeout or setup.

## Evidence

- With `beforeObserveUpdate`, both new tests wrote no version (`latest version = 2, want 3`).
  With `afterCreditUpdate` they pass `-count=5 -race` (`settlement_render_once_test.go:44-51,110-115`;
  the hooks are documented at `service.go:161-174`).
- Under the fix ranking's `len(slots) > 0`, the stamp test fails on the block rows: the pasted
  callout is still indexed under the id of the callout it copies. The markdown assertion would
  fail too, since a typed block always renders its id (`pmdoc/schema.go:286-290`) and the reused
  rendering shows the copy as `#note` where the room shows its stamped id. The block-row check
  fails first only because it runs first (`settlement_render_once_test.go:147-148`). Removing
  `changedTree()` fails the repair test, and removing `versioned != read` fails the stamp test.
