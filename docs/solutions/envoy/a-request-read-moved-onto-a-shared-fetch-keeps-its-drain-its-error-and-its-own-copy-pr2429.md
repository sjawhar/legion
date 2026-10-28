---
title: "A request read moved onto a shared fetch keeps its drain, its error and its own copy"
category: envoy
tags:
  - cache
  - shutdown
  - drain
  - dispatch-server
  - data-race
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/api
---

# A request read moved onto a shared fetch keeps its drain, its error and its own copy

When a handler that called a dependency on its own request starts reading a process-wide cached
answer (one fetch shared by every reader, such as `rosterCache` in `api/issue_route_status.go`),
check three things the old per-request call gave it for free.

## Rules

- **The drain.** A shared fetch runs on the process's lifetime (`s.lifetime()`), and Dispatch
  cancels that lifetime at SIGTERM (`cmd/dispatch/main.go`, `signal.NotifyContext`) before
  `http.Server.Shutdown` serves the requests still in flight (`cmd/dispatch/shutdown.go`). Any
  fetch started or awaited after SIGTERM fails at once, and a cache that keeps failures for a
  silence window keeps that one too. A handler that can answer "unknown" is fine. A handler that
  must answer from the dependency falls back to its own request context once
  `s.lifetime().Err() != nil`, as `listAgents` does (`api/agents.go`). Test it by calling
  `register`, then setting an already-cancelled `deps.Lifetime` and a healthy fake dependency:
  the read must still succeed, cold cache and cached failure both.
- **The error.** A cache built for a caller that treated failure as "unknown" usually drops the
  error. A caller that must report the failure needs it stored: the dependency's own error, and,
  for a reader whose request ended while it waited, an error wrapping the context's (so the
  caller never meets a zero value with a nil error). `routeRoster.err` holds both, and both wrap
  `envoy.ErrUnavailable`.
- **The copy.** Every reader shares the cached value. A reader that sorts or edits it copies first
  (`slices.Clone` before `sort.SliceStable` in `listAgents`), and never writes through the copy
  into nested slices, which `slices.Clone` still shares. Lock it with a test that runs several
  readers concurrently under `-race` and then reads the cached value back to check it is still the
  dependency's own answer, in the dependency's order.

## Evidence

- Before the drain fallback (`76dbf910`), a `GET /api/v1/agents` in flight at SIGTERM, against a
  healthy listener, answered `503 ENVOY_UNAVAILABLE … terminated signal received`; after it
  (`e66f2b32`) it answered 200 with the sessions. The regression test is
  `TestAnAgentsListDuringAShutdownDrainListsAHealthyListenersSessions` (`api/agents_test.go`).
  The deep review found it; no existing test covered a request served during the drain.
- Sorting the shared slice in place made
  `TestConcurrentAgentsListsAndIssueReadsLeaveTheSharedListAsTheListenerSentIt` report
  `WARNING: DATA RACE` and a reordered cached list.
