---
title: "Data the publish reads by update index is appended in one record step and cleared in one place"
category: envoy
tags:
  - document-writes
  - live-write
  - merge
  - invariants
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2448"
---

# Data the publish reads by update index is appended in one record step and cleared in one place

- `liveWrite` keeps several slices that `publishLiveWrite` reads by the index of each update:
  `updates`, `versions`, `renderings` and `marginRooms`. Append every one of them in the one
  function that records an operation (`recordLiveOperation`), next to each other, and clear every
  one of them in `finishLiveWrite`.
- A write path that appends to only some of them makes the publish index past the end of the
  others after the commit, and that happens only on the publish of a committed transaction.
- When main adds another such slice, or another write path is added, the merge or the change
  appends to the new slice in that record step, so every path records it. Give the record step the
  value as a parameter (the zero value where a path has none) instead of appending to the slice in
  each caller.
- A test that a committed write keeps none of its per-transaction state checks every one of these
  slices.

## Evidence

- In sjawhar/legion#2448, main's #2453 added `renderings` while this pull request added
  `marginRooms` and a second write path (`applyMargin`). Merge 9346736e made `recordLiveOperation`
  take both values and append them beside `updates` and `versions` (`docs/mutation.go`).
  `finishLiveWrite` (`docs/livewrite.go`) clears all of them, and
  `TestRenderReuseACommittedWriteKeepsNoRendering` checks `marginRooms` with the others. Review
  thread PRRT_kwDORFy7ds6xQ9x4 named the index-past-the-end failure this prevents.
