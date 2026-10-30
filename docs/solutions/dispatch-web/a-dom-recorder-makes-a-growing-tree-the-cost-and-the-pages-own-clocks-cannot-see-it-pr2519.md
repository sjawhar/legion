---
title: "A DOM recorder makes a growing tree the cost, and the page's own clocks cannot see it"
category: dispatch-web
tags:
  - playwright
  - dom
  - main-thread
  - measurement
  - browser-tests
date: 2026-10-29
status: active
module: packages/dispatch/web
---

# A DOM recorder makes a growing tree the cost, and the page's own clocks cannot see it

Building a large subtree in slices so no one task holds the reader's thread is right, and growing
it **in the page** undoes it. Everything watching the document pays per change, not per paint: the
browser's own layout, any `MutationObserver`, and the DOM recorder a browser test runs under. A
Playwright trace is one of those recorders, and it is on by default on a failing run
(`trace: "retain-on-failure"`, `packages/dispatch/e2e/playwright.config.ts`).

- **Build off the page and attach once.** Append into a detached element and finish with one
  `replaceChildren`, whatever the slicing. The reader gains the same way they do from a single
  swap — they never see half a list, and the page lays the body out once — and the per-change
  watchers see one change instead of tens of thousands.
- **Slice the work, not the attachment.** Yield between pieces of *building*; the piece that puts
  the tree in the page is the last one. Everything that mounts into the painted body — portals,
  cards, diagrams, the "rendered" callback — waits for that piece, so nothing is mounted into a
  tree still being built.
- **When the page's own clocks cannot account for the time, suspect the observer, not the code.**
  A main-thread heartbeat (a `setTimeout(0)` loop recording its longest gap) showing multi-second
  gaps in which no page code ran means the time is being spent outside the page's JavaScript —
  where nothing the page can measure, a long-task observer included, has anything to attribute it
  to. Re-run with the recorder off before believing a number, and never take a timing from a run
  whose trace is being kept.
- **Assert what is in the page, not what is laid out.** A long body also carries
  `content-visibility: auto` on its items (`.dispatch-markdown-long li`, `styles.css`), so an
  off-screen item has no box. A row counts `li` elements; it never asks whether they are visible.

## Evidence

Dispatch's long Markdown bodies (sjawhar/legion#2403). The parse worker's answer is painted a
piece at a time by `paintBlockInSlices`
(`packages/dispatch/web/src/features/refs/MarkdownBody.tsx`), at most `SLICE_MS` (50 ms) of work a
task (`useRenderedMarkdown.tsx`). Measured on an 8,000-item list — 16,000 elements — with the
pieces appended into the element already in the page, under a recording browser, the body took
45–54 s to land, the thread running nothing at all for 7 s and then 35 s at a stretch. The same
slicing, building into a detached `div` and ending in one `replaceChildren`, took 3.1 s. Both
numbers are recorded beside the painter they belong to.

The main-thread heartbeat those gaps come from is `measureDraw` in
`packages/dispatch/e2e/heavy-bodies.e2e.ts`: a `setTimeout(0)` loop installed before the page's
own scripts, whose largest gap between ticks is the row's evidence and whose record restarts at
the first request for the data the body arrives in.
