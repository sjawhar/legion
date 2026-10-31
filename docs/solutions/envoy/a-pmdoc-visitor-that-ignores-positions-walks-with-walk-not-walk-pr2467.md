---
title: "A pmdoc visitor that ignores positions walks with Walk, not walk"
category: envoy
tags:
  - pmdoc
  - tree-walk
  - cpu
  - dispatch
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2467"
---

# A pmdoc visitor that ignores positions walks with Walk, not walk

- `pmdoc.walk` computes each node's ProseMirror range: `walkNode` calls `nodeSize` for every node
  and again for every child, and `nodeSize` measures the whole subtree, counting UTF-16 units of
  every text node. A visitor that ignores `path`, `pos` and `end` pays all of it for nothing. Give
  it the positionless `pmdoc.Walk`.
- `walk` does not descend into the children of a text node or a leaf node (`hr`, `hardbreak`,
  `image`, `html`, `footnote_reference`); `Walk` does. `Validate` refuses children on exactly those
  types, so the two visit the same nodes only for a tree `Validate` accepts. Before you switch,
  confirm every tree the caller gets came through `pmdoc.Read`, `ReadInTransaction` or a validated
  parse, and state that precondition in the function's comment.
- Prove the list is unchanged on real documents, base against head, and profile the caller: the
  cost was in `nodeSize` and `len16`, not in the visitor.

## Evidence

- `pmdoc/walk.go:37-77` (`walkNode`, `nodeSize`); the skip is at :42. `Validate`'s rule is
  `pmdoc/node.go:181-184`.
- `pmdoc/find.go:794-797`: `ListMarks`, called once per settlement by `sweepUnrecordedMarks`, moved
  from `walk` to `Walk` in `d0a8089e`. In the replay's profiles its 3.10 s (2.96 s in `nodeSize`,
  2.79 s in `len16`) became 0.04 s. A benchmark on 24 real specs fell from up to 67.4 ms and
  327,273 allocations per call to under 0.64 ms and 16 allocations.
- Settlement dumps of those 24 specs at base and head were identical. Review thread
  PRRT_kwDORFy7ds6xQnjk replaced the change-history sentence with the precondition.
- Other visitors that still call `walk` and ignore positions (`blocks.go` `TableDescendantIDs`,
  `find.go` `nearestBlocks` and `MarkAttrs`, `retype.go` `StripServerOwnedAttrs`) are not on the
  settlement path and were not profiled here.
