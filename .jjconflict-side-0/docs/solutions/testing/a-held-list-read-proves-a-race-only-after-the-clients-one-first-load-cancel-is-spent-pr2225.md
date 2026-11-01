---
title: "A held list read proves a race only after the client's one first-load cancel is spent"
category: testing
tags:
  - playwright
  - dispatch
  - race
  - tanstack-query
date: 2026-10-28
status: active
module: packages/dispatch/e2e
---

# A held list read proves a race only after the client's one first-load cancel is spent

Extends docs/solutions/testing/a-browser-race-with-no-in-page-signal-is-proven-by-a-held-read-a-margin-a-red-run-justifies-a-sampled-settle-and-one-shot-assertions-INFRA-303.md.

- **Before you trust a held-read race test, turn the guard off and watch it fail. Then list every
  client mechanism that would rescue the stale body without the guard.** In the Dispatch web app,
  the event flush cancels a query's first load once (`cancelFirstLoads` in
  `web/src/api/query-refresh.ts`). Hold only the first list request and the event's flush cancels
  it and fetches again. The fresh body is then correct, and the guard is never consulted.
- **Spend that cancel first.** Let an unrelated write's flush cancel the first request. Hold the
  second one, with its body built by `route.fetch()` before the event. Wait for a request the event's
  own flush makes (another query it refreshes), not a timer. Release the stale body after that.

## Evidence

The scenario `a claim that lands while the Inbox is loading reaches the card that mounts from the
earlier list` in `packages/dispatch/e2e/recipients-claim.e2e.ts` (commit 7a7fd806). Its first
version held only Inbox request 1 and passed with `markPendingAskThreadsOnIssue` made a no-op. The
version that holds request 2, after a title change on another issue spent the cancel, fails under
that mutation (`Received "Reaches 1"`) and passes with the guard.
