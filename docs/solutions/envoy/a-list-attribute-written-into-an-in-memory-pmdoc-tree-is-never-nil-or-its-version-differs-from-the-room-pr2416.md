---
title: "A list attribute written into an in-memory pmdoc tree is never nil, or its version differs from the room"
category: envoy
tags:
  - dispatch
  - pmdoc
  - crdt
  - attributes
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
---

# A list attribute written into an in-memory pmdoc tree is never nil, or its version differs from the room

- Write a list-valued node attribute into an in-memory `pmdoc` tree as a non-nil slice
  (`append([]string{}, s...)`), never `append([]string(nil), s...)`, which is nil when `s` is
  empty.
- A nil list renders as `null` from the in-memory tree. The same block after a round trip through
  the room's CRDT renders as `[]`. Any version rendered from the in-memory tree (a settlement that
  wrote nothing into the room) then differs from the room. The next settlement writes another
  version that changes only that attribute.
- The equality check will not show this: `askServerAttributeEqual` treats nil, `[]string{}` and
  `[]any{}` as equal, so no repair or test of attribute agreement sees the difference. Only a
  comparison of rendered markdown does.

## Evidence

- Before PR #2416, `setAskServerAttributes` wrote `selected` as `append([]string(nil), …)`. It now
  writes `append([]string{}, …)` (`ask_blocks.go:805-808`). The equality check is at
  `ask_blocks.go:834`.
- An answer with an empty selection, already written into the room by a peer, was versioned
  `selected="null"` while the room rendered `selected="[]"`. With the old line,
  `TestSettlementVersionsARepairTheRoomAlreadyHolds` fails exactly on that (PR #2416 body,
  negative control).
