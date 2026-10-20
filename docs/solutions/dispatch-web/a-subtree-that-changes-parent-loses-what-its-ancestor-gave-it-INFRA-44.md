---
title: "A subtree that changes parent loses what its ancestor gave it, and a green browser suite cannot see that it is gone"
category: dispatch-web
tags:
  - component-extraction
  - scroll-anchoring
  - optimistic-writes
  - error-paths
  - review
date: 2026-10-20
status: active
module: packages/dispatch/web/src/features
related_issues:
  - "INFRA-44"
  - "sjawhar/legion#2083"
---

# A subtree that changes parent loses what its ancestor gave it, and a green browser suite cannot see that it is gone

- **Before you move rows or a panel under a new parent, list what the old ancestors gave them for
  free and re-provide each one explicitly.** A scroll anchor, an error banner, a write guard, a
  context, a focus trap: none of these is written in the moved code, so none of them can be
  *missing* from it. The check is mechanical — diff the ancestor chain you left against the one
  you arrive in — and takes minutes.
- **An end-to-end suite is structurally blind to this class.** The behaviour was never asserted in
  the moved component, and the row that looks like it covers the area usually exercises the one
  path that cannot fail: paging in older rows appends *beneath* a reader and so can never catch a
  lost scroll anchor.
- **Where a count and a list claim to describe the same set, derive both from the same array.** A
  label computed over the unfiltered collection beside a control that acts on the filtered one is
  correct in every single-variable test and wrong only on the cross-product nobody seeds.
- **A write with optimistic UI needs its refusal driven, not just its success.** Without a fault
  injected at the route, a silently reverted optimistic write and a successful one render
  identically. Keep one reusable "refuse this route" helper rather than building it ad hoc and
  throwing it away.

## Evidence

INFRA-44 moved the issue's event rows out of the Conversation tab into a new Activity tab. CI was
green — the whole Playwright suite in four browser projects, the Go suite against a real
pgvector Postgres — and review then found three defects of exactly this shape at that head:

- `features/activity/ActivityTab.tsx` rendered the rows in a plain `<section>`. They had had
  viewport stability only because they used to sit inside the Conversation's
  `<ViewportAnchor as="section" item="data-turn">`. The tab reads `["events", key]`, the query
  the stream prepends each published event to, and renders newest first, so every arrival pushed
  a scrolled reader down one row. The suite's one Activity row covered `Load older`.
- Event pinning had had the Conversation's failed-op state, its `Couldn't save pin — retry`
  banner and its write-guard in scope. After the move `onError` was a bare
  `invalidateQueries(["user-state"])`: a refused pin reverted with no message, which reads to a
  user as a broken button. The fix was one hook, `features/issue/usePinWrites.tsx`, that both tabs
  consume — which also removed the third copy of that sequence.
- `Jump to latest · N new` counted the unfiltered conversation list while the pill was gated on,
  labelled from and scrolled into the filtered one. With the filter on Open and the unread
  conversations all closed it promised a jump to cards that were not on the page.

All three are review-shaped rather than test-shaped. The transferable part is the ancestor
checklist, not more rows in the suite.
