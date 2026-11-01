---
title: "Two paths that index one document agree only on what the renderer round-trips"
category: architecture-patterns
tags:
  - dispatch
  - pmdoc
  - block-index
  - round-trip
  - renderer
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch
---

# Two paths that index one document agree only on what the renderer round-trips

## The rule

- When one derived index is written from two sources - the in-memory tree a version write holds,
  and the stored markdown a reconciliation reads back - key it only on what the renderer writes so
  that its parser reads it back unchanged. Make the renderer guarantee that for every value a
  browser can set: write the short form only when the parser reads all of it as that form, and a
  quoted, escaped form otherwise.
- Every test of such an index asserts the same rows after the version write and after the
  reconciliation, with a negative control for each half of the fix.
- When you change how a value is spelled in stored text, export the writer and make every
  consumer that searches stored text for it call the writer. Grep production code as well as
  tests. A consumer in another language gets a port of the writer, and both copies are tested
  against one checked-in table.

## Evidence

- A browser could give a typed block an id such as `a b`. The renderer wrote `#a b`, which read
  back as the id `a` plus an undeclared attribute. So the version write (tree) indexed a pointer
  where the reconciliation (markdown) indexed a mention.
  - The fix: `pmdoc.TypedBlockIDAttribute` writes `#id` only when the editor's shortcut reads the
    whole id, and `id="…"` with character references otherwise (`pmdoc/directive.go`).
  - The test is `TestATypedBlockIDReadsBackFromTheMarkdownAsTheTreeHoldsIt` in
    `docs/block_pointer_links_test.go`; review round 7 of sjawhar/legion#2394.
- Three rounds (5-7) went to the repeated-id rule before it held on both paths.
  `TestARepeatedBlockIDWritesPointersOnlyFromItsNode` runs three cases and asserts both paths:
  paragraph then claim, claim then claim, and list item then claim.
- The spelling change broke `dispatch request-approval`. Its preflight in
  `packages/envoy-client/src/dispatch-execute.ts` searched for `ask{#<id>` and so refused every
  request over an ask whose id is now written `id="…"` (round 8).
  - The fix: `@legion/contracts`' `typedBlockIdAttribute`, a port of the Go writer.
  - Both writers are tested against `DISPATCH_TYPED_BLOCK_ID_SPELLINGS` and its Go copy,
    `pmdoc/testdata/typed-block-id-spellings.json`.
