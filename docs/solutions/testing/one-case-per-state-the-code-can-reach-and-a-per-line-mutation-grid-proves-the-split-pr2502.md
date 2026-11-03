---
title: "One case per state the code can reach, and a per-line mutation grid proves the split"
category: testing
tags:
  - regression-tests
  - negative-control
  - refactoring
  - node-view
date: 2026-10-29
status: active
module: packages/proof-editor
related_issues:
  - "sjawhar/legion#2502"
---

# One case per state the code can reach, and a per-line mutation grid proves the split

Before you merge duplicated statements into one helper, find out whether any test observes those
statements at all; then lock them with cases the product can actually produce.

- **Count the states the code can reach, not the effects the helper has.** When the production
  code never leaves two of the helper's effects dirty at once, no single case can lock both: the
  second half's assertions hold for a reason that has nothing to do with the helper, and deleting
  that half of the helper leaves the case green. Write one case per reachable precondition.
- **Assert where the effect is on the page, not where the code performs it.** A method that ends
  by detaching the nodes it just cleared leaves the clearing unobservable at that point; the
  reader meets it when the layout puts those nodes back. Find the step that re-attaches them and
  read the window between it and the next answer.
- **Prove the split with a mutation grid, one line of the helper at a time**, and record which
  case goes red for each. A grid where every line fails a different case is the evidence that the
  cases are independent; a line no case notices is a line no test locks.
- **A helper's extraction is not the lock.** The refactor is behaviour-preserving by reading; the
  grid is what makes that a measurement.

## Evidence

sjawhar/legion#2502 merged two copies of four statements in
`packages/proof-editor/src/mermaid-code-block-view.ts` into one private `clearPreview()` (empty the
preview's shadow root, hide it, hide the status line, empty its text). The planner found that the
only existing case switching a block's language switched one that had never drawn, so all four
statements were observed by no test.

The first case written for them switched a block that had drawn to `js` and read the preview
there. Review refused it twice over. `showPlainLayout` ends `this.dom.replaceChildren(this.pre)`,
so the preview is off the page whether the clearing ran or not and the case read a detached node.
And its status half could not fail at all: `show()` empties the status whenever it fills the
preview, so a block that had drawn always has an empty status, with or without `clearPreview`.

The shipped shape is two cases in
`packages/proof-editor/tests/mermaid-code-block-view.test.ts`, each taking the window after the
block goes back to `mermaid` — where `showMermaidLayout` re-attaches the preview and the status —
and before its new answer arrives: one from a drawing, one from an `{ error }`. The grid:

| mutation of `clearPreview` | result |
| --- | --- |
| the whole call dropped from `showPlainLayout` | 7 pass, 2 fail (both new cases) |
| its two status lines dropped | 8 pass, 1 fail (the refusal case) |
| its two preview lines dropped | 7 pass, 2 fail (the drawing case, and the pre-existing `an edit answered unavailable` case) |
