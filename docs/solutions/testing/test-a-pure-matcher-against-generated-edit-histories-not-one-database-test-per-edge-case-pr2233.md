---
title: "Test a pure matcher against generated edit histories, not one database test per edge case"
category: testing
tags:
  - property-testing
  - matching
  - test-design
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/docs
---

# Test a pure matcher against generated edit histories, not one database test per edge case

- When the logic that decides is a pure function, test it as one before any database test. Build
  inputs whose true identity you know: a version, then generated edits (move, change, delete,
  retype the same text, paste a copy that keeps its source's id, type something new), with and
  without stored rows.
- Assert the invariants, not one expected output: no item takes another's id, each id names at most
  one item, and every indexed text is text the version holds. Where identity cannot be known (no
  rows), assert only that the text is true.
- Use `testing.F` or a table generator, and keep the database tests for the paths around the
  function.

## Evidence

In sjawhar/legion#2233, `closedVersionBlocks` (`packages/envoy/internal/dispatch/docs/closed_version_blocks.go`)
is a pure function, yet its edge cases arrived one review or testing round at a time: the tester's
red tests 196f5a85, 2f3a9b7f, 44cf9cac and c12da9a2, then the reviewer's pasted-id thread. Each round
re-ran the full docs package against Postgres (763–832 s). The hand-written crossing test
`TestAClosedDocumentsCrossedEditsKeepTheirRowsHistory` (e02af334) still missed unequal copy counts
and pasted ids. The first pure test of the function, `closed_version_blocks_test.go`, came in the
last fixing round (fd3bfa69).
