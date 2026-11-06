---
title: "An engine pays for an attach after your task, where no clock on the page can see it"
category: dispatch-web
tags:
  - dom
  - main-thread
  - measurement
  - browser-tests
  - webkit
date: 2026-11-01
status: active
module: packages/dispatch/web
related_issues:
  - "sjawhar/legion#2519"
---

# An engine pays for an attach after your task, where no clock on the page can see it

Building a large subtree in slices so no one task holds the reader's thread is only half of it.
Putting the finished tree in is a few pointer writes, which every clock on the page records as
nothing; the style and layout of what just arrived is the engine's own work, in the rendering
update after the task, or in whatever task next reads the page's geometry. Slice the build and
the attach alike.

- **End a task on a batch of nodes, not on a time budget.** A painter that yields "this reached
  the page" lets its driver stop there whatever is left of its milliseconds, because the clock it
  budgets by cannot see what the batch will cost. 500 nodes a task is what this body measured at.
- **Make each batch's layout the batch's own.** Read the element's geometry once a batch, while
  that batch is all of the page that is dirty. Left alone, the layout of every batch falls into
  whichever task first reads geometry, and arrives as one long task again.
- **Build off the page, attach in the page.** A body's finishing passes run on the built tree
  before any of it is in the document, so they cost one off-page walk rather than thousands of
  live mutations, and the live element takes finished nodes only.
- **Restore what you removed if the paint stops.** The element's previous children wait off the
  page while the batches land and go back whole if the paint is cut short, so a reader is left
  the body they were reading rather than a prefix of the one that was arriving.
- **A preview is one run of text.** A clamped preview of a body of thousands of plain blocks puts
  each run of them in as one text node, not one node a block: the two or three lines a reader sees
  read the same, and 16,000 nodes for them is ~2 s of WebKit's thread.

## Evidence

Dispatch's long Markdown bodies (sjawhar/legion#2519). The parse worker's answer is painted by
`paintBlockInSlices` (`packages/dispatch/web/src/features/refs/MarkdownBody.tsx:230`): built off
the page at most `SLICE_MS` (50 ms) of work a task
(`packages/dispatch/web/src/features/refs/useRenderedMarkdown.tsx:132`), then put into the
element at most `ATTACH_BATCH` (500) nodes a task (`MarkdownBody.tsx:168`), the task ending at
once on a piece that reached the page (`useRenderedMarkdown.tsx:163-175`).

The batch's layout is forced in `afterPageBatch`, by one `element.getBoundingClientRect()`
(`useRenderedMarkdown.tsx:581`), whose comment records why: "in WebKit a list of 8,000 items laid
out in one go is ~700 ms of that task … Laid out a batch at a time it is a few milliseconds each"
(`:573-580`). The finishing pass is `finishBlock` — the task checkboxes and the unwrapping of
tight list items (`MarkdownBody.tsx:83-91`) — run on the built tree at `MarkdownBody.tsx:239`,
before the first batch is attached. What the element held waits in a fragment and is put back in
the `finally` where the attach did not finish (`MarkdownBody.tsx:246-261`).

`ATTACH_BATCH`'s docstring carries the measurement that chose it (`MarkdownBody.tsx:157-167`),
taken on a two-core pod against the dashboard's own stylesheet, an 8,000-item list of 16,000
nodes, as the time the thread ran no task:

| | WebKit | Chromium | Firefox |
| --- | --- | --- | --- |
| one `replaceChildren` | 918-971 ms | 211 ms | 173 ms |
| 500 nodes a task | 72 ms | 124 ms | 39 ms |

The same docstring states why the attach is split at all: WebKit "spends ~700 ms on a pass over
16,000 freshly attached nodes whether their boxes are laid out or skipped". The preview figure is
`MarkdownPreview.tsx:44-47`: 16,000 nodes for the three lines a reader sees "is ~2 s of WebKit's
thread to style and lay out".

On the surfaces themselves, the pull request reports the longest main-thread gap at its worst on
a two-core pod as Chromium 212 ms, WebKit 185 ms and Firefox 347 ms, against the 1,000 ms bound
the rows hold (`packages/dispatch/e2e/heavy-bodies.e2e.ts:50`). That spec's `measureDraw`
(`heavy-bodies.e2e.ts:115`) installs a `setTimeout(0)` heartbeat before the page's own scripts and
restarts its record at the first request for the body's data; the recipe is the earlier change's,
`docs/solutions/testing/prove-a-tab-freeze-with-a-heartbeat-reset-at-the-first-data-request-and-a-rebuilt-old-module-as-the-control-pr2403.md`,
and the spec applying it to these bodies is this pull request's. The trace recorder is on for a
failing run (`trace: "retain-on-failure"`, `packages/dispatch/e2e/playwright.config.ts:135`), so
it is part of what every row measures.
