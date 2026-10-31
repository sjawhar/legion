---
title: "Replace a deep copy with copy-on-write by keeping the old pipeline as a test oracle and finding every writer"
category: envoy
tags:
  - copy-on-write
  - pmdoc
  - performance
  - test-oracle
  - aliasing
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2427"
---

# Replace a deep copy with copy-on-write by keeping the old pipeline as a test oracle and finding every writer

- Before you remove a deep copy, list every function downstream of it that writes into what it is
  handed: a filter that reuses its argument's backing array (`out := marks[:0]`), an in-place sort,
  an attribute map assignment. Each one was safe only because it wrote into the copy. After the
  change, every such function either gets a slice or map of its own, or the copy-on-write pass
  clones that one node before the write.
- A helper that returns its input when nothing changes, and a new value otherwise, also returns a
  `kept` flag. A caller that must write clones only when `kept` is true.
- Keep the old pipeline's order of decisions. When the old code first merged and then rewrote
  values, the new pass decides every merge on the values as they were and rewrites after.
- Copy the deleted functions into a `_test.go` file as an oracle and compare the new pass with
  it by serialized tree and by the rendered output, on generated cases for every branch and on
  real-size documents. Add one negative control per decision order (rewrite before merge must
  fail a case).
- Lock the sharing with three tests: the input is unchanged after every public entry point that
  reaches the pass; a document that needs no change comes back as the same pointer with
  `testing.AllocsPerRun == 0`; an untouched sibling is shared by pointer beside a changed one.

## Evidence

- `asWritten` (`render.go:942`) replaced `StripAnchorMarks` + `writeLineBreaksIn` +
  `writeNulsAsReplacement`. `writtenMarks` hands `visibleMarks`'s result to `withoutMark`
  (`marks.go:43`), which filters in place, so `visibleMarks` (`marks.go:108`) returns nil or a slice
  of its own; `renderedMarks` (`render.go:965`) returns `(marks, kept)`.
- U+0000 is written as U+FFFD after the merge decisions (`childrenAsWritten`, `render.go:993`;
  `withNulsWritten`, `render.go:1123`): two runs whose link hrefs are `u\x00` and `u\uFFFD` stay
  two runs, as the old pipeline left them. With the replacement moved before the merge, the oracle
  test `TestAsWrittenIsTheOldPipelineOnEveryBranch/nul_and_replacement` fails.
- Oracle and locks: `render_written_test.go:302`, `:310`, `:324`, `:344`, `:393`.
- On every LINUX spec (1,204), render time fell to 0.514 of base and allocations to 0.265 of base,
  with output `cmp` identical (PR body, "Render cost" and "E2E (implementer)").
