---
title: "An engine pays for an attach after your task, where no clock on the page can see it"
category: dispatch-web
tags:
  - playwright
  - dom
  - main-thread
  - measurement
  - browser-tests
  - webkit
date: 2026-10-30
status: active
module: packages/dispatch/web
---

# An engine pays for an attach after your task, where no clock on the page can see it

Building a large subtree in slices so no one task holds the reader's thread is only half of it.
Putting the finished tree in is a few pointer writes, which every clock on the page records as
nothing; the style and layout of what just arrived is the engine's own work, in the rendering
update after the task or in whatever task next reads the page's geometry. Slice the build and the
attach alike.

- **End a task on a batch of nodes, not on a time budget.** A painter that yields "this reached
  the page" lets its driver stop there whatever is left of its milliseconds, since the clock it
  budgets by cannot see what the batch will cost. 500 nodes a task is what this body measured at.
- **Make each batch's layout the batch's own.** Read the element's geometry once a batch, while
  that batch is all of the page that is dirty. Left alone, the layout of every batch falls into
  whichever task first reads geometry - the reader's-place measurement at the end of the paint -
  and arrives as one long task again.
- **Build off the page, attach in the page.** The two passes over a whole body (task checkboxes,
  unwrapping tight items) cost one off-page walk that way, and the live element takes finished
  nodes only.
- **`content-visibility: auto` is not free, and in WebKit it is not a saving.** On an 8,000-item
  list it took one forced layout from 856 ms to 720 ms in WebKit (231 ms to 86 ms in Chromium),
  and with the attach batched it *cost* more than it saved in both. Worse, a per-element read of
  layout over such a body - what a DOM recorder does when it snapshots a page - forces each
  skipped item relevant one at a time: with the rule, the longest main-thread gap of a drawing
  body was 4,766 ms; with the rule deleted and nothing else changed, 141 ms.
- **When the page's own clocks cannot account for the time, split the recorder before you blame
  the code.** `trace: { mode: "on", snapshots: false, screenshots: true }` against
  `{ snapshots: true, screenshots: false }` says which half costs: 478 ms for the screencast,
  4,766 ms for the snapshots, same build, same row. A main-thread heartbeat (a `setTimeout(0)`
  loop recording its longest gap) showing multi-second gaps in which no page code ran means the
  time is outside the page's JavaScript, where nothing the page can measure has anything to
  attribute it to.
- **A preview is one run of text.** A clamped preview of a body of thousands of plain blocks puts
  each run of them in as one text node, not one node a block: the two or three lines a reader sees
  read the same, and 16,000 nodes for them is ~2 s of WebKit's thread.

## Evidence

Dispatch's long Markdown bodies (sjawhar/legion#2403). The parse worker's answer is painted by
`paintBlockInSlices` (`packages/dispatch/web/src/features/refs/MarkdownBody.tsx`): built off the
page at most `SLICE_MS` (50 ms) of work a task, then put into the element at most `ATTACH_BATCH`
(500) nodes a task, each batch laid out by `useRenderedMarkdown.tsx`'s driver before the next is
added to it.

Measured on this pod (two cores), an 8,000-item list - 16,000 nodes - against the dashboard's own
stylesheet, as the longest gap between the ticks of a `setTimeout(0)` heartbeat:

| | WebKit | Chromium | Firefox |
| --- | --- | --- | --- |
| one `replaceChildren` | 918-971 ms | 211 ms | 173 ms |
| 500 nodes a task | 72 ms | 124 ms | 39 ms |

In the dashboard itself (`packages/dispatch/e2e/heavy-bodies.e2e.ts`, whose `measureDraw`
installs that heartbeat before the page's own scripts and restarts its record at the first
request for the data the body arrives in), the three 8,000-item rows went from 2,357-5,301 ms in
WebKit to 107-185 ms, and Chromium's from 284-343 ms to 191-212 ms. The rows that fail a second of held
thread are the measurement; the trace recorder is on for a failing run
(`trace: "retain-on-failure"`, `packages/dispatch/e2e/playwright.config.ts`), so it is part of
what every row measures.
