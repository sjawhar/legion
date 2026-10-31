---
title: "A write that always changes the document stays off the no-change guard, and a fast path shares only the steps that can fire for it"
category: envoy
tags:
  - document-writes
  - ygo
  - performance
  - refactoring
  - merge
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2448"
---

# A write that always changes the document stays off the no-change guard, and a fast path shares only the steps that can fire for it

Extends docs/solutions/envoy/a-crdt-emitting-an-update-does-not-mean-it-changed-anything-guard-the-shared-write-path-with-state-vector-and-delete-set-pr2233.md.

- When you split a cheap write path off a shared write function (`applyMargin` beside `applyLive`
  in `packages/envoy/internal/dispatch/docs`), share the steps every write needs through helpers
  both paths call: opening the operation, capturing its updates, recording it (the decode refusal,
  the append, the failed-room refusal, the kept rendering, the anchor refresh), and abandoning a
  fork the operation moved but never recorded. Refusals then run in one order for both.
- Keep a check out of the shared helpers when it provably cannot fire for one of the paths, and
  say why in a comment at the check. The no-change guard (`StateVector` plus
  `crdt.DeleteSetFromDoc` before the write, `forkHeld` after) walks the whole store twice. A
  marks-map `Set` always adds an item, so a margin write always moves the fork, and the guard only
  costs it CPU. It stays in `applyLive`.
- When a merge from main adds a check to the shared function, decide again which path it belongs
  to. Do not drop it into the shared helper because that is where the conflict was.

## Evidence

- sjawhar/legion#2448: merge 19ff4162 put main's no-change guard into the shared
  `liveOperation.change`, so every margin write paid two `DeleteSetFromDoc` walks. Review thread
  PRRT_kwDORFy7ds6xNCXg flagged it, and f5035f9e moved the guard back into `applyLive`, around its
  `op.change` call.
- With the drop removed from `applyLive`,
  `TestAJoinedWriteWhoseTransactionChangesNothingAppendsNoUpdateAndKeepsItsSettlementMark` fails
  (`updates:3`, want `updates:2`). The margin path needs no such test, because its update always
  advances the state vector.
