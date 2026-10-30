---
title: "A browser row that freezes the page clock advances it after every response, because TanStack delivers on a page timer"
category: testing
tags:
  - playwright
  - page-clock
  - tanstack-query
  - debounce
  - negative-control
date: 2026-10-29
status: active
module: packages/dispatch/e2e
---

# A browser row that freezes the page clock advances it after every response, because TanStack delivers on a page timer

A Playwright row that measures a debounce holds the page's clock still
(`page.clock.install()`, then `pauseAt`, as `e2e/hover-preview.e2e.ts` does: `install()` alone
leaves the clock running at real speed). Everything the page's own timers drive is then the row's
to fire, and two of those are not obvious.

- **Advance the clock by 1 ms after a response reaches the page, before asserting on what the
  query rendered.** TanStack Query delivers through `notifyManager`, which batches on a
  zero-delay timer; with the clock held, a landed response sits in the store and the component
  never re-renders. Choose that tick deliberately: it must be far inside the next debounce window,
  so the *next* request cannot fire inside the assertion.
- **Take the settle margin from Node, never `page.waitForTimeout`.** That helper waits on the
  page's clock and never returns while it is held. `import { setTimeout as sleep } from
  "node:timers/promises"` is the margin an absence assertion needs.
- **Release a held response while the old request is still the one in flight.** A key change that
  forwards react-query's `signal` aborts the superseded fetch, so a response released after the
  new request starts proves nothing: the gate under test is never consulted. Release after the
  first debounce has fired and the input has changed, and before the second debounce window
  elapses, and say in the row's comment why that ordering is load-bearing.
- **Match a routed path that carries a query string with a RegExp, not a glob**: in a glob `?` is
  a single-character wildcard, so a sibling path matches too.

Prove each of these with its own mutation, and read which assertion goes red. A row whose first
draft passed against the broken code was measuring the transient state before the answer landed,
not the behaviour.

## Evidence

sjawhar/legion#2511. `packages/dispatch/e2e/create-issue-related.e2e.ts`'s row "an answer that
arrives after the title changed is not shown for the new title" holds the first title's check in a
`page.route` handler, releases it once the box already holds a second title, then
`await answered; await page.clock.runFor(1); await sleep(250);` before asserting.

- The first draft asserted straight after the release. It passed whether or not the display gate
  existed, because the assertion ran before the held answer had returned.
- The second draft added a Node-side margin but no tick. With the clock held, the answer never
  reached React at all: the section read `Checking for related items…` in both the fixed and the
  mutated build, and the row's later assertion failed in both.
- With the tick, the control is honest: the gate removed (`waiting: false`) makes that row red 3
  of 3 at `expect(related.getByText("Checking for related items…")).toBeVisible()`, and the
  debounce set to 0 makes a different row red 3 of 3 at `expect(checks).toBe(0)`, each leaving the
  others green.

The plan predicted the abort trap before any row was written: "with the signal forwarded
react-query aborts a superseded fetch, so a late release after the debounce fires never exercises
the display gate."

`pauseClock` now exists identically in `e2e/hover-preview.e2e.ts` and
`e2e/create-issue-related.e2e.ts`. The next spec that needs it should lift it into a shared `e2e/`
module, as `e2e/unclipped.ts` and `e2e/touch.ts` already are.
