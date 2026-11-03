---
title: "A batch version beside a kept single-key function makes that function its one-key call"
category: envoy
tags:
  - pmdoc
  - refactor
  - oracle-test
  - duplication
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2447"
---

# A batch version beside a kept single-key function makes that function its one-key call

Extends docs/solutions/envoy/keep-a-frozen-copy-of-the-old-walk-in-the-tests-and-hold-a-rewrite-to-it-on-real-and-synthetic-trees-pr2455.md.

When you add a many-key version of a function and other callers keep using the single-key one:

- Rewrite the single-key function as the batch called with one key. Move its old body into the
  `_test.go` file as a frozen oracle, and point the differential, fuzz and benchmark baselines at
  the frozen copy. A copy kept in production drifts. Then two callers read different answers for
  the same input.
- If the new single pass computes positions or sizes in its own traversal, move each rule into a
  helper that every traversal calls. Then search the package for the rule's literal pieces (the
  type checks, the `+1`/`+2` offsets). A review names the copies it saw, and there can be more.
- When the batch shares data from its input, let it return that data read-only and say so in the
  type's doc comment. Copy only in the wrapper whose contract promised a copy. A copy per key per
  write is an allocation that the batch's main caller never reads.
- A frozen oracle must not call a helper the rewrite extracted. If it does, a wrong change to the
  helper moves the oracle and the code together, and the test still passes. Freeze the whole old
  traversal.

## Evidence

- Review round 1 of sjawhar/legion#2447 (at f67fcb42) had two important findings: `FindMarks`
  repeated `FindMark`'s quote rule and `MarkAttrs`' first-mark rule, and `yFragmentLocations`
  repeated walk's position rules. One minor finding was a per-anchor `cloneAttrs` that a refresh
  never reads. All were fixed in a9727ac8.
- `pmdoc/find.go:740` (`FindMark`) and `:989` (`MarkAttrs`, which still clones) are one-key
  `FindMarks` calls. The old bodies are `frozenFindMark` and `frozenMarkAttrs`
  (`find_marks_test.go:27`, `:52`).
- `pmdoc/walk.go:117` (`isOpaqueNodeType`) and `:133` (`boundaryTokens`) serve `walkNode`,
  `measure`, `yFragmentLocations` and `splice.go:241` (`childBoundary`). `childBoundary` was a third
  copy, which the review had not named.
- The first frozen `yTextRanges` oracle called the production `walk`. With `boundaryTokens`
  returning 0 for `blockquote`, the `yTextRanges` oracle tests still passed until the oracle walked
  with a frozen walk. At the merge with main, #2455's `referenceWalk` is that frozen walk, and the
  same broken helper fails `TestWalkMatchesTheReferenceWalk` and
  `TestYTextRangesMatchesFrozenOnDocuments`.
