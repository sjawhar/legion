---
title: "A browser test of a value a server cache serves polls the read, or warms the cache through a probe, before the page acts"
category: testing
tags:
  - playwright
  - dispatch
  - cache
  - flaky-test
date: 2026-10-28
status: active
module: packages/dispatch/e2e
---

# A browser test of a value a server cache serves polls the read, or warms the cache through a probe, before the page acts

- **`resetDatabase` and the fake listener's `setLiveSessions` do not clear a cache the server holds
  in process.** When an assertion depends on a value such a cache serves (Dispatch's route roster,
  `routeRosterTTL`, 5 s), `expect.poll` the same API read until it shows the fixture, and only then
  load the page or make the change under test. The page refreshes once per SSE event and renders
  whatever the cache served at that moment.
- **When the issue under test must not carry the state yet**, because setting it is the event the
  test drives, warm the cache through a probe: give a second issue the same state, poll its read
  until it shows, then act on the real one.
- **After a forward merge of main, check main's diff for a new cache behind a read your tests
  assert.** That is when tests that passed for rounds start to race. Do not add a test-only TTL
  setting to dodge it.

## Evidence

`packages/dispatch/e2e/recipients-claim.e2e.ts` passed for several rounds. After the merge of
main 2df3353c, which brought #2267 (one route roster per process, `api/issue_route_status.go`),
two scenarios read `route_holder: null` for a session made live a moment earlier. The tester
measured the roster reading `no_holder` at once and `live` 5.5 s later. The scenarios now poll
`getAsk` before asserting (`expect.poll`, 15 s). The follower-holds-claim scenario, whose route
change is itself the event, warms the roster through a probe issue routed to `role:reviewer`
(commit d56b0642).
