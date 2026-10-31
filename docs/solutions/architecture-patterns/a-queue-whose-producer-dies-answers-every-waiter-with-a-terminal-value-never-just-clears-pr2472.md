---
title: "A queue whose producer dies answers every waiter with a terminal value, never just clears"
category: architecture-patterns
tags:
  - web-worker
  - callbacks
  - failure-handling
  - contracts
date: 2026-10-29
status: active
module: packages/dispatch/web
related_issues:
  - "sjawhar/legion#2472"
---

# A queue whose producer dies answers every waiter with a terminal value, never just clears

- When the thing that answers requests (a worker, a socket, a renderer) fails, call every waiting
  callback with a terminal answer that says so, then clear the queue. A waiter dropped without an
  answer keeps showing what it showed before, forever, and any "everything settled" promise built
  on the queue never resolves.
- A request made while the producer is already down gets that same answer at once, inside the
  call, not silence.
- Put the terminal answer in the contract's type (`{ unavailable: true }` beside the success and
  refusal variants), so every consumer has to handle it, and write in the contract's doc that the
  callback runs at most once and never after its cancel.
- A consumer handles the terminal answer by returning to its no-producer state (here: clear the
  old drawing, show the source), not by keeping its last good result.

## Evidence

In sjawhar/legion#2472, `fail()` in `packages/dispatch/web/src/features/refs/mermaid-diagrams.ts`
first cleared the waiting draws without calling them. An editor block that had drawn diagram A and
was then edited kept A above the new source, with no status, after its worker failed on restart
(review round 1, finding 2). The fix calls each waiting draw with `{ unavailable: true }` and
answers a later request the same way; `MermaidDrawing` in
`packages/proof-editor/src/mermaid-svg.ts` gained the variant, and the editor view's `show()`
clears its preview for it. Negative control: `fail()` clearing without answering made the browser
row "a document diagram whose worker fails after its first drawing shows its source" fail at
`toHaveCount(0)` with 1 diagram still shown.
