---
title: "A refresh is owed from the moment it is asked for: judge it on the response that lands, keep it across an unmount, and never cancel"
category: dispatch-web
tags:
  - tanstack-query
  - scheduler
  - race
  - playwright
date: 2026-10-28
status: active
module: packages/dispatch/web/src/api
---

# A refresh is owed from the moment it is asked for: judge it on the response that lands, keep it across an unmount, and never cancel

Extends docs/solutions/testing/a-browser-race-with-no-in-page-signal-is-proven-by-a-held-read-a-margin-a-red-run-justifies-a-sampled-settle-and-one-shot-assertions-INFRA-303.md.

TanStack's state while a fetch is in flight does not tell you whether that fetch saw the event. An event-driven scheduler on top of it must track that itself.

- **Record when each refresh was asked for.** Only a fetch that *started* after that moment pays the debt, whoever started it (`requestedAt` against the query's last fetch start, `web/src/api/query-refresh.ts` `drive`).
- **Never cancel.** `invalidateQueries` defaults to `cancelRefetch: true`, which restarts a running request and leaves the first one running on the server. Fetch with `cancelRefetch: false`, and when a fetch is already out, fetch again after it settles. Do the same in a `QueryClient` subclass so mutations' invalidations follow the same rule (`DispatchQueryClient` in `api/query-client.ts`).
- **A refresh conditioned on data waits for the landed response.** A predicate such as "does this list hold issue K" must not run on the data a fetch in flight is about to replace. Defer it while *any* fetch is out (a first load or a refetch), run it on the response, and refetch if it holds and that fetch started before the request. If the refetch fails, run the predicate on the data still on screen.
- **Keep the debt across an unmount.** TanStack's success clears `isInvalidated` even for a body composed before the event. A query left while it owes a refresh must be marked stale again after the in-flight fetch lands, or a `staleTime: Infinity` read stays wrong until the next event that names it.
- **Test these races with a body composed before the event.** Hold the response with `route.fetch()` when the request arrives and `route.fulfill({ response })` on release (`e2e/refresh-freshness.e2e.ts`, the held-read helper). The extended document's `route.continue()` on release lets the server compose the body *after* the event, which hides this whole class of bug.

## Evidence

The base cancelled each first load once and resent it, so the sidebar's three first reads went out twice under a steady stream of events (acceptance test 2 at `16e4bb0f758e`: `inbox 2, pinned 2, projects 2`). The branch's first scheduler deferred predicates only for first loads, and dropped the owed refresh when the query was left. Tester round 2 found both:
- a closed issue's ask stayed in the Inbox when the close landed during the Inbox fetch;
- a reference written during its read's fetch kept the old title after the reader left and came back.

They are pinned by the two red unit tests in `api/query-refresh.test.ts` (commit `cbcf7310c171`) and fixed by `d65080e1`.
