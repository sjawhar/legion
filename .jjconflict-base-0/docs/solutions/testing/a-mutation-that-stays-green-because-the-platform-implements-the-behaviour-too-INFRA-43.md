---
title: "A mutation that stays green because the platform implements the behaviour too"
category: testing
tags:
  - mutation-testing
  - playwright
  - browser-engines
  - scroll-anchoring
  - acceptance-criteria
date: 2026-10-20
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-43"
  - "sjawhar/legion#2084"
---

# A mutation that stays green because the platform implements the behaviour too

Extends docs/solutions/testing/mutation-proof-probe-tests.md.

That document's causes are all in the test. This is a different one: the row is correctly written
and correctly asserted, and still green when you delete the production line it exists to lock,
because the platform under it is a **second implementation** of the behaviour.

- Delete the line, run the row, and read what you get. A row that stays green there is not a lock,
  however faithfully it describes the feature.
- When the mutation stays green because the environment did the work, do not tighten the
  assertion — it is already asserting the right thing. Take the environment's implementation away
  (a CSS property, a flag, a polyfill boundary), or run the row in the engine that lacks it.
- Write a reviewer's acceptance as the mutation's outcome, not as a file state: "the row fails
  with `<line>` removed" is checkable; "a row exists for X" is not.
- A unit harness that draws its own geometry can never be this lock: it has no platform
  behaviour to compete with, which is exactly why its rows kept passing while the browser row did
  not bite.

## Evidence

INFRA-43's browser row asserts that a reader's turn stays where it was after the transcript is
rebuilt. With the hook's compensation line deleted the row still passed, because Chromium's own
scroll anchoring held the turn; the two unit rows failed (`Expected: 50 Received: 150`), which is
what first showed the compensation was pinned nowhere a reader could see.

The reviewer wrote the acceptance in the checkable form — "the rebuild rows fail when
`settleRebuild`'s compensation is removed" — and named the two remedies: remove the engine
dependence, or add the rows to the `webkit-iphone` grep in `packages/dispatch/e2e/playwright.config.ts`
(whose own comment already says engine-dependent rows run in WebKit). The change took the first:
`[overflow-anchor:none]` on the scroller leaves the hook's compensation the only one, and the same
mutation now fails `Expected: "m12" Received: "m20"` and passes restored.

Both remedies have a cost worth weighing: the second buys a lane that cannot be run locally in an
issue pod (WebKit's system libraries need root), so its failures surface only as CI red nobody can
reproduce.
