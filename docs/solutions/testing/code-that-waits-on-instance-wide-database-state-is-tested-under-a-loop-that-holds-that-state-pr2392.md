---
title: "Code that waits on instance-wide database state is tested under a loop that holds that state"
category: testing
tags:
  - postgres
  - flaky-tests
  - go-test
  - streaming
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2392"
---

# Code that waits on instance-wide database state is tested under a loop that holds that state

- When code waits on state that spans the whole Postgres instance (snapshots, `pg_snapshot_xmin`, any open transaction id), the test packages that share one test cluster delay each other. Run the whole affected package under a loop that holds that state in another database (for example, a transaction holding its id for 2 s of every 2.5 s), and fix every test that then fails.
- A stream or connection test that fails to open must cancel its request and close the body. Otherwise the server's `Close` waits on the open handler, and the package times out with no named failing test.
- Wait for the event a test needs (a recompute, a cursor at an id) rather than for a quiet period. An event the horizon delays can arrive after any fixed quiet time.

## Evidence

In sjawhar/legion#2392, the reviewer reproduced a CI timeout of `envoy-go` with one held transaction in database `postgres`. The loop of `holdClusterTransactionID` (`packages/envoy/internal/dispatch/api/event_horizon_helpers_test.go:72`) then exposed further timing assumptions, which were fixed in `79cf9b708611`, among them `TestAResetEventLogIsCaughtAndEveryViewRecomputed`. `openSSEResponse` (`8c2e6451627e`) cancelled a stream test that failed to open; the SSE route and that helper later went with main's move of the feed to `/ws/live`. The delivery hub tests were changed in `576d7a2b` to settle on recomputes rather than on a quiet hub.
