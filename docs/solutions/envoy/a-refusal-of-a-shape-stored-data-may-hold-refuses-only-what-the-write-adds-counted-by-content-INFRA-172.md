---
title: "A refusal of a shape stored data may already hold refuses only what the write adds, counted by content"
category: envoy
tags:
  - dispatch
  - refusal
  - doc-edit
  - write-validation
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "INFRA-172"
  - "sjawhar/legion#2272"
---

# A refusal of a shape stored data may already hold refuses only what the write adds, counted by content

- When a write path refuses a shape that a document can already hold (left by a browser edit,
  an older build or a migration), refuse only the instances the write adds. Never exempt a whole
  block, table or document because it already holds one: that lets a write put a new instance
  right beside the old.
- List the offending instances in the changed region before and after the write, compare them as a
  multiset keyed by their content, and refuse the first instance after the write that the count
  before does not cover. Key by content, not position, because a write can move a cell or a block.
- Test the three cases: a write beside an existing instance that adds none (accepted), a rewrite
  that removes the existing instance (accepted), and a write that adds one beside it (refused).

## Evidence

- `packages/envoy/internal/dispatch/docs/edits.go:1247` (`refuseCodePipeInTableCell`, commit
  96976206): the first version went through `replacementBroke`, which gives no verdict when the
  check already fails on a changed block before the write. A browser edit had left code `a\|b` in
  one cell, so a replace that put new such code in another cell of the same table was stored with
  its `|` silently taken out of the code. The fix counts `pmdoc.TableCodePipes` of the changed
  blocks by code text before and after.
- Pinned by `edits_test.go` ("a code pipe replaced in beside code a browser edit left", the delete
  equivalent, "the rewrite of code a browser edit left") and `marks_test.go` (the accepted
  suggestion equivalent).
