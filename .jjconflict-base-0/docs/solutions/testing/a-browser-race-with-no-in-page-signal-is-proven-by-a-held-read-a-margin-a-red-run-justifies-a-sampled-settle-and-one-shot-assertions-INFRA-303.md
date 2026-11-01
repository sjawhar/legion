---
title: "A browser race with no in-page signal is proven by a held read, a margin a red run justifies, a sampled settle and one-shot assertions"
category: testing
tags:
  - playwright
  - race-condition
  - page-route
  - negative-control
  - mutation-testing
date: 2026-10-26
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-303"
  - "sjawhar/legion#2221"
---

# A browser race with no in-page signal is proven by a held read, a margin a red run justifies, a sampled settle and one-shot assertions

- **Hold the late request with a URL predicate, never a glob.** Match the pathname and the query
  parameter that tells it apart (`url.searchParams.get("thread") === "1"`). In a glob `?` is a
  wildcard, and a sibling read on the same path matches too. After the release margin, assert
  exactly one request was held: that catches a second read during the hold. Release with
  `route.continue()` so the real server answers.
- **When nothing on the page says the partial state was seen, use a fixed in-page margin and prove
  it red.** Wait for `requestfinished` on the other reads. Then, inside the page, wait two
  macrotasks, two animation frames and a few hundred milliseconds. The margin's proof is an N-of-N
  `--repeat-each` red against the unfixed code, every failure at the target assertion. A timeout
  or a failure elsewhere is not that red. A margin too short gives a false green, never a false
  red, so name it under the PR's "Not proven / risk".
- **Settle by sampling, then assert once.** Wait until the element's box is unchanged across about
  30 samples 50 ms apart, past any read dwell. Never scroll, focus or click before asserting:
  Playwright's actionability auto-scroll changes the scroll state under test. Then assert once, in
  view and clear of the fixed composer. A polling helper (`expectUncovered`, `toBeInViewport`
  with its default timeout) can pass on a transient frame and stand in for the end state.
- **End the row with a sensitivity guard.** Put the page in the state the fault leaves (here
  `scrollTo(0, 0)`). Assert the target is then not both in view and uncovered, with a message saying
  the fixture can no longer catch the fault. A layout change that makes the row pass vacuously then
  fails it.
- **Run each unforced repeat on a new page.** Assert `history.length === 1` and `scrollY === 0`
  before navigating. A `reload()` or a same-URL `goto` gets the browser's scroll restoration.
- **A comment that names the mechanism a row triggers is a claim; check its listener's target.**
  `document.dispatchEvent(new Event("visibilitychange"))` does not bubble, so it reaches only
  listeners on `document`. TanStack Query's `focusManager` listens on `window` and never sees it;
  Dispatch's event stream (`web/src/api/sse.ts`) listens on `document`, reconnects and refreshes
  every query. Name the path that actually runs. A row that depends on it fails as a bare timeout
  when that path changes.
- **One mutation per condition and per guard claim.** For a fix that ORs conditions
  (`loading || failed`), revert each term on its own against the row named for it. Reverting the
  whole fix credits the red to whichever term fires first. For a comment citing another row as the
  guard of a behaviour, break that behaviour and watch the cited row go red.

## Evidence

`packages/dispatch/e2e/phone-landing.e2e.ts` (sjawhar/legion#2221) holds the Conversation's asks
read while messages and comments answer. At 390x664 it failed 20 of 20 times at the unfixed test
commit 6afa7e09, at the composer assertion (newest card y494-634, composer y423-600, `scrollY` 0),
and passed 20 of 20 with the fix. At the file's 390x844 viewport the composer sits below that card,
so the same row could not fail. Pick the viewport the fault was measured at.

The first draft of the failed-read row credited its refetch to TanStack's focus refetch; review
corrected it to the event stream's `forceReconnect` → `refreshEverything`. The leave-while-loading
row's comment first cited two rows that never check the restore; review corrected it to
`doc.e2e.ts`'s tab round-trip. The retro proved that row is the guard: firing `onFirstPlacement` on
every return made it fail 2 of 2. The tester's per-term mutation (`loading ? undefined :
shown.at(0)`, dropping only `failed`) made the failed-read row fail 6 of 6 while the forced-order row
and the unit suite stayed green. Without that mutation, nothing showed the `failed` term mattered.
