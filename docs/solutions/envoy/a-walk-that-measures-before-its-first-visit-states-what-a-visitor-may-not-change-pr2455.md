---
title: "A walk that measures before its first visit states what a visitor may not change"
category: envoy
tags:
  - pmdoc
  - performance
  - visitor
  - contract
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2455"
---

# A walk that measures before its first visit states what a visitor may not change

When you move work a callback walk did at each visit to before its first visit (sizes, positions,
counts):

- Write in the walk's doc comment what a visitor must not change from then on: every field the
  precomputed values or the descent read, on every node, the visited one included. In pmdoc that
  is a node's `Type`, `Text` and `Children`.
- Read every existing visitor against that rule before you merge.
- Give the walk's measure and any standalone function of the same value one shared rule
  (`nodeSize(node)` is `measure(node, nil)`), so the two cannot drift.
- Index the precomputed values in visit order, reserving a node's slot before its children are
  measured, and size the slice from a count that follows the same descent.

## Evidence

- On sjawhar/legion#2455 the first contract named only `Text` and `Children` of unvisited nodes.
  Review round 1 (thread PRRT_kwDORFy7ds6xKhsh, fixed in 32065ac7) showed that `walkNode` reads the
  visited node's `Type` and `Children` after the visit against sizes measured before it, so a
  change there gives wrong ranges or an index panic. Only `StripServerOwnedAttrs`
  (`pmdoc/retype.go`) writes during a walk, and it writes only `Attrs`.
- The tester's mutation that made `nodeSize` and `walk` stop sharing the rule failed 3 tests.
