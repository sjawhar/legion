---
title: "TanStack runs no refetch interval in the Dispatch unit suite until a test says it is not a server"
category: dispatch-web
tags:
  - tanstack-query
  - refetch-interval
  - polling
  - bun-test
  - happy-dom
date: 2026-10-26
status: active
module: packages/dispatch/web/src
related_issues:
  - "INFRA-440"
  - "sjawhar/legion#2237"
---

# TanStack runs no refetch interval in the Dispatch unit suite until a test says it is not a server

- The Dispatch SPA's unit test preload imports `@tanstack/react-query` before it registers
  happy-dom. TanStack then decides it is running on a server and never schedules a
  `refetchInterval`. A unit test that asserts "polls every N seconds" or "polls at most once per
  N seconds" passes whether or not any interval exists.
- A test about polling sets `environmentManager.setIsServer(() => false)` (from
  `@tanstack/react-query`) before it renders, with fake timers, and puts back the earlier value in
  `finally`. `features/refs/AgentName.test.tsx`'s poller test is the example.
- TanStack runs one interval per observer, and resets an observer's interval whenever the query
  updates. N components that each pass `refetchInterval` and mount at different times can fetch up
  to N times per period. One app-shell poller, enabled by a mount count that the readers register
  with (read through `useSyncExternalStore`), keeps it to one request per period per tab. The
  readers set `refetchOnMount: false` and no interval of their own.

## Evidence

On sjawhar/legion#2237 the existing `CredentialRequestsSection` "one poll" test had never run
an interval. It passed because TanStack thought it was on a server, and it went on passing after
the component's own `refetchInterval` was removed. The new poller test mounts five agent names at
staggered times plus a `useAgents(…, true)` caller over 45 s. It runs intervals only with
`setIsServer(() => false)`, and it observed fetches at about 0, 15 and 41 s: TanStack's interval
restarts from the latest update to the query.
