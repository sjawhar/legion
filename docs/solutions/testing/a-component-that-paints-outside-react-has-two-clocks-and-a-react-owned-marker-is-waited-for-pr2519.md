---
title: "A component that paints outside React has two clocks, and a React-owned marker is waited for"
category: testing
tags:
  - react
  - dom
  - flake
  - negative-control
  - dispatch-web
date: 2026-11-01
status: active
module: packages/dispatch/web
related_issues:
  - "sjawhar/legion#2519"
---

# A component that paints outside React has two clocks, and a React-owned marker is waited for

Where a component writes DOM itself and also publishes state through an attribute React renders,
the two land in different turns: the painter's nodes are there as it writes them, the attribute
only after the commit React queues when the painter finishes.

- A test that waits on painter-written DOM — its items, its paragraphs, its text — is already
  past that wait before the commit lands. Every read of a React-owned marker after such a wait is
  waited for, never read at once.
- Put the rule on the helper that reads the marker, not in each row, so the next row written
  against that helper inherits it.
- Keep a synchronous read only where a reason holds it: a preceding wait that already reads React
  state, a marker unchanged since a commit the test already flushed, or a row that expects the
  value from before the commit. Write the reason down per row.
- The natural red is load-dependent, so reproduce it by forcing the lag instead of chasing it:
  in a scratch copy, defer the state setters of that one commit by a task, run the unfixed file
  and the fixed file under the same seam, and check that the fixed file is green at a longer lag
  too. The reds you get name the class; count how many of them the one observed failure had
  shown.

## Evidence

`packages/dispatch/web/src/features/refs/MarkdownBody.test.tsx` (sjawhar/legion#2519). The rule
is the docblock on `pendingMark` (`:1097-1102`): "Both marks are React state: the commit that
clears `data-markdown-pending`, and the one that sets `data-markdown-fallback`, are queued when
the painter's last piece is in, after the painter has written the body itself straight into the
element. A row that waits on what the painter wrote … is therefore through that wait before the
commit lands, so a read of a mark the commit changes is waited for rather than read at once."
The painter's side is `useRenderedMarkdown.tsx:589-593`: `done()` runs `finish()` (`:509`),
whose `setIsPending(owed())` (`:511`) is the commit that clears the mark. The rows that wait read
`await waitFor(() => expect(pendingMark(view.container)).toBeNull())` (for example `:1221`,
`:1249`).

What it answers, from the pull request body: CI job 124798054818 turned the `dispatch` lane red
at `MarkdownBody.test.tsx:1241` with the pending marker still `"true"`. Eighteen rows now wait
for the mark; eleven stay synchronous with their reason given — 1607's preceding wait already
reads React state, 1305/1341/1348/1473/1556/1696 read a mark that has been `"true"` since a
commit `render`/`act` already flushed, and 613/656/669/710 expect the value the mark holds before
the commit.

The forced-lag control, also from the pull request body: a scratch copy of `packages/dispatch`
with `finish()`'s `setIsPending` and both `setIsFallback` flips deferred together by
`PROBE_LAG_MS`.

| | one task of lag | 120 ms |
| --- | --- | --- |
| reviewed test file (`c95e16651`) | 35 pass, 16 fail | 34 pass, 17 fail |
| this head's test file | 51 pass, 0 fail | 51 pass, 0 fail |

Every red read `Received: "true"`. The sixteen reds at one task are every one of the twelve
flagged rows by name plus four the same rule catches, so the fix covers the class rather than the
one line CI happened to catch. At the head the file was run twenty times: 51 pass, 0 fail in
every run.
