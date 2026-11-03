---
title: "A browser test waiting for requests to settle drops those of a document already replaced"
category: testing
tags:
  - playwright
  - navigation
  - requests
date: 2026-10-28
status: active
module: packages/dispatch/e2e
---

# A browser test waiting for requests to settle drops those of a document already replaced

- A page being left can start a request (a lazy import, a preload) between a test's wait and its
  next `page.goto`. Chromium then cancels it, and Playwright fires neither `requestfinished` nor
  `requestfailed` for it, so a set of pending requests that only those events empty never empties
  and every later wait times out.
- Key each pending request to the document that made it: count main-frame commits in
  `framenavigated` (only the first after each `goto`, since the app's own history navigations
  commit the main frame too), drop requests of an older document at the commit, and ignore a
  `requestfailed` of an older document, which is the navigation's cancel, not a refusal.
- Keep reading responses that do arrive for a replaced document: they still count as answers.

## Evidence

Pull request #2323, `packages/dispatch/e2e/two-builds.multiprocess.e2e.ts:516-556`. Before the
change the walk with its page from one process and its assets from the other failed 4 of 4 runs
from `/issues/CORE-1/spec` on with `Timeout 15000ms exceeded while waiting on the predicate`; the
trace showed the departing overview requesting `lib-*.js` and its stylesheet after the new page's
HTML arrived, both cancelled with no event.
