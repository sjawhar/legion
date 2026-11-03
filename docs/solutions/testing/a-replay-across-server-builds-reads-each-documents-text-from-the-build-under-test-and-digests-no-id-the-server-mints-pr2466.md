---
title: "A replay across server builds reads each document's text from the build under test, and digests no id the server mints"
category: testing
tags:
  - load-test
  - replay
  - before-and-after
  - determinism
  - digest
date: 2026-10-30
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2466"
---

# A replay across server builds reads each document's text from the build under test, and digests no id the server mints

- **Before the window, read the current text of every document the replay will edit or upload from
  the build under test**, and plan against that text. A build renders a stored document its own way,
  so text recorded from one build, or from production, can be refused by another as a find that
  does not match or as markdown it no longer accepts. Replan only the documents whose text differs,
  and record which (here `base_docs {read, unread, differed, replanned, documents}`), so a reader
  sees whether the two sides still did the same work.
- **Build an edit from what the server matches**: Dispatch matches an edit's `find` against the
  rendered text, inside one block. A find copied from markdown that crosses a line, carries escapes
  or starts with a list marker is refused. Check every planned edit offline against those rules
  before a long run.
- **Digest the end state by position, not by id.** A "same work" digest that includes ids the
  server creates (a block id minted for every block an edit writes) differs between two runs that
  did identical work. Name each anchored block by its index in the document (`<i>`, `<i>.<r>.<c>`
  for a table cell, `-1` for one no longer present), and keep the id beside it for debugging.
- **Prove the plan unchanged by a refactor with a byte-identical plan**, from a binary built at the
  old commit and one at the new, over the same inputs.

## Evidence

`packages/envoy/loadtest/replay_run.go:272` (`readBaseTexts`) reads the build's text before the
window; `replay_record.go:455` (`endStateDigest`) and `:477` (`endStateBlockIndexes`) digest by
position; `replay_edit.go` builds only plain one-line runs, and `TestReplayEditsBundle` checks the
window's 1,003 edits and 1,490 anchor quotes offline.

Three defects, each found by a live run:
- On 35efd326, 6 of a window's first 9 edits were refused: 400 `TARGET_SPANS_BLOCKS` and
  `INVALID_OP`, 404 `TARGET_NOT_FOUND`. The finds had been built from markdown.
- On a newer build (8a911ecc), 2 of 12 uploads were refused 400 `INVALID_MARKDOWN`. That build
  writes a pipe inside a table cell's code span differently from the build that produced the
  bundle's text, and it accepts its own rendering. Reading the build's text fixed it.
- Three baseline runs sent identical requests and stored identical versions, sizes and anchor
  quotes, but gave three different end-state digests, because Dispatch had minted new block ids.
  After the change to positions, runs of one window give one digest.
