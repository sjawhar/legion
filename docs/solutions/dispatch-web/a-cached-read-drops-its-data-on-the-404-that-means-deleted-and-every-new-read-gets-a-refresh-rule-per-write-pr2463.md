---
title: "A cached read drops its data on the 404 that means deleted, and every new read gets a refresh rule per write that moves it"
category: dispatch-web
tags:
  - tanstack-query
  - event-refresh
  - freshness
  - hover-card
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features/refs
---

# A cached read drops its data on the 404 that means deleted, and every new read gets a refresh rule per write that moves it

Extends docs/solutions/dispatch-web/narrowing-a-broad-cache-invalidation-starts-from-every-read-and-the-writes-that-move-what-it-shows-pr2324.md.

That document pairs each read with the writes that move it. Two cases it does not name:

- **A delete is a write too, and its refetch fails.** TanStack Query keeps a query's last `data` through a failed refetch, so a read refreshed after its object was deleted keeps showing the deleted text. Where a 404 `NOT_FOUND` means the object is gone, the consumer treats that error as no data, so the page reads as a freshly loaded one does. Keep the last data only for transient failures (5xx, a dropped connection).
- **A new read kind needs its own rule.** A read keyed by something the event payload never names (here a block of a comment, stored as `comment_block`, which `references_changed` does not carry) gets a rule on the item's own event, or it stays stale until a reload.
- Test the edit and the delete separately, each with the page open, the card showing and no reload.

## Evidence

In sjawhar/legion#2463 a hover card on a link to a comment's block kept its pre-edit text after the author edited it; `api/event-refresh.ts` now refreshes the block reads of comment `<id>` on `comment.edited`, and `block-references.e2e.ts` ("shows the block's new text once its author edits it") fails with that rule removed. The tester then deleted the cited paragraph: the refetch answered 404 and the card kept "Beta soon deleted.". `useReferenceData` (`features/refs/reference-target.ts`) now returns no block on 404 `NOT_FOUND`; a pinned read of a removed block answers 200 and keeps its pinned text. A document block on main had the same fault, covered by the spec-block row beside the comment one.
