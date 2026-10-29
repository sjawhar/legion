---
title: "A ProseMirror node view sets layout only in update, and an async answer fills only its own region"
category: dispatch-web
tags:
  - prosemirror
  - node-view
  - caret
  - async-rendering
date: 2026-10-29
status: active
module: packages/proof-editor
related_issues:
  - "sjawhar/legion#2472"
---

# A ProseMirror node view sets layout only in update, and an async answer fills only its own region

Extends docs/solutions/dispatch-web/an-interactive-element-inside-a-prosemirror-node-view-leaves-ignoremutation-alone-knows-stopevent-gates-only-the-editors-own-handlers-is-no-drag-source-and-seats-the-caret-before-firefox-focuses-the-host-LEGION-695.md.

- In a node view that waits on asynchronous work (a worker, a fetch), give the block its final
  layout inside `update()` (and the constructor), where ProseMirror expects DOM changes: the
  `contentDOM` and its wrapper go where they will stay before any answer arrives.
- When the answer lands, touch only the elements that exist to show it (a preview's shadow root,
  a status line), never move `contentDOM` or its ancestors. An answer arrives while the reader
  types, and moving the content then moves it under the caret.
- Debounce the request on edit inside the view (`update` restarts a timer), cancel the earlier
  request before asking again, and cancel both in `destroy()`.
- Lock it with a unit case that answers late under a `MutationObserver` on the view's frame and
  asserts that neither the `pre` nor `contentDOM` was added or removed.

## Evidence

sjawhar/legion#2472's `packages/proof-editor/src/mermaid-code-block-view.ts` replaced upstream's
synchronous view, which moved the `pre` into a disclosure inside `update`. The first draft of the
async view also moved the `pre` when a drawing arrived; the case in
`packages/proof-editor/tests/mermaid-code-block-view.test.ts` with the `MutationObserver` caught it
(the happy-dom run hung with the move restored). The final view sets the layout in
`showMermaidLayout` from `update` and has `show()` change only the preview and the status.
