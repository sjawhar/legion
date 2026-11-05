---
title: "What a retry under the same key resends is data on the kept record, so keeping a restored record gives the record back"
category: dispatch-web
tags:
  - held-sends
  - session-storage
  - idempotency-key
  - reload
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features/conversation
---

# What a retry under the same key resends is data on the kept record, so keeping a restored record gives the record back

A write kept across a reload under an idempotency key must resend exactly what its first attempt sent, or the server answers `422 KEY_REUSED`. So every value the first attempt computed at the press is stored as a plain field of the kept record:

- Never hold such a value only in a closure (a restored `save` function). Storage keeps data, not closures, so the next `keep` loses it.
- Never re-derive it in `keep` from page state (the editor's spans, the blocks a route answered). A restored record has none of that state.
- The test is a round trip: keep, restore, keep again gives the same record. Test two reloads, and a reload that interrupts a Retry already in flight. One reload can pass while the second loses the value.

## Evidence

In sjawhar/legion#2463 a comment edit carries block ids. Commit `a21c72bb` restored them through a closure in `restoredEdit`, while `keep` still worked them out from `edit.blocks`, which a restored edit lacks. So the Retry after one reload resent the ids, and the Retry after a second reload sent the body alone and was refused `422 KEY_REUSED`. Commit `881b03e5` made the ids a field: `KeptEdit` (`held-sends.ts:57`), shape-checked by `keptCarried` (`:180-190`), copied by `restoredEdit` (`:254-258`) and by `keep` (`:274-279`). `held-sends.test.ts:311`, "a comment edit keeps its ids through two reloads and through a reload that interrupts its Retry", fails at `8c404ede`.

## A kept record outlives the build that wrote it

A tab can reload straight through a deploy, so storage holds records in the previous build's shape. Make the restore accept that shape as a union member and behave as the old build did with it. Then write a test that stores a record in the old shape and restores it. Here `KeptEdit` is `string | { carried?, id }` (`held-sends.ts:57`, `shape.either` at `:188-190`). The bare comment id the earlier build kept restores with no ids, and the server matches the blocks by their text, as that build's save did (`held-sends.test.ts:355`).
