---
title: "A live-session list is a cache view: decide one session's absence by its own record"
category: envoy
tags:
  - nats
  - kv
  - cache
  - dispatch
  - session-registry
  - cross-service
date: 2026-10-25
status: active
module: envoy
applies_when:
  - You are about to decide that a session has ended, from a listener answer
  - You are adding a Dispatch decision that excludes, takes over or releases on a session's absence
  - You are tempted to judge a whole selection against one read of GET /v1/sessions
related_issues:
  - "INFRA-249"
  - "sjawhar/legion#2191"
---

# A live-session list is a cache view: decide one session's absence by its own record

Extends docs/solutions/envoy/a-cache-miss-must-not-release-what-another-listener-wrote.md.

That rule is about a cache miss inside one listener. This is its cross-service half: the listener
**exports** that cache, so a consumer of `GET /v1/sessions` inherits the same defect one process
further out, where it has no way to see it.

## Rules

- **A row missing from `GET /v1/sessions` is not evidence that the session ended.** That route
  answers from `sessions.List()`, the watcher-fed cache alone. While the cache refills — which is
  what recreating the session bucket does, and what rebuilding the hub machine does to it — a
  working session is simply not in the list, and an absent row cannot tell "ended" from "not
  refilled yet". Decide a session's absence only from an authority that answered about **that
  session**: `GET /v1/registry/<session_id>`, which reads through to the bucket.
- **A list cannot be made authoritative key by key, so do not try.** Leave `GET /v1/sessions`,
  the agents page, the issue subscribers and `route_status` as the cache views they are; they
  display a fleet and act on nobody. Change the *caller that acts*, not the list.
- **Ask per id even when the caller holds many ids.** A broadcast names up to
  `contracts.MaxBroadcastRecipients` sessions and used one list read for all of them, which looks
  thriftier and is wrong: the thing each recipient needs is its own record. Bound the fan-out
  instead (`resolveBroadcastSessions`: eight reads in flight under one budget).
- **Split "gone" from "could not read" at the route, and keep the words.** Only the bucket's own
  key-not-found is `404 {"error":"no live session <id>"}`; an id NATS refuses as a KV key is
  `400`/`413`; a read that did not answer is `503` with `Retry-After: 1`. The 404 wording is
  load-bearing, not cosmetic: `internal/dispatch/api/comment_delivery.go` classifies a delivery
  failure on the `"no live session "` prefix, and the dashboard prints the text verbatim.
- **The listener is authoritative for absence, not for presence.** `roleHolderSession` answers
  `200` from the cache when the cache holds the entry and reads the bucket only on a miss, so a
  `200` can be stale by one watcher tick. That is the right trade — a stale "present" costs a
  refused send, a wrong "absent" costs a session its work — but do not write "authoritative"
  without it.

## Evidence

Three Dispatch decisions read a session's liveness out of the list, and each was wrong in its own
way while the cache refilled (`packages/envoy/internal/dispatch/api/`):

- `resolveDeliveryTarget` scanned the list for a mention's target and answered
  `no live session <id>` — the words a person reads as "that agent is gone" — for an agent that
  was working.
- `createBroadcast` judged its whole selection against one list read, excluding working agents
  with `no live session`, and committed that exclusion under the send's idempotency key, where
  `replayedExclusions` repeats it to every retry of that key.
- `fetchLiveSessions` asked the list whether an issue's claim holder was still working, and
  `claimBlockedBy` read "not in the list" as "free to take" — a working session's issue handed to
  another caller with no `{"force": true}`, which no later read undoes.

The listener's own `/v1/registry/<session_id>` had the same defect in a third shape: it read
`sessions.Get` and answered `404 {"error":"not found"}` for *every* failure, so an unreadable
bucket was indistinguishable from an ended session.

The fix is proven across both services at once in
`packages/envoy/cmd/listener/dispatch_resolution_test.go` (the real listener mux and the real
Dispatch mux over one NATS and one Postgres, Dispatch pointed at a listener whose cache never saw
the sessions), and on real binaries through the hub-rebuild sequence — delete `KV_envoy_sessions`,
restart one listener, re-register through it while the other's watcher waits out its tick — which
is the reproduction that speaks for production. The trailing-cache rig in
`cmd/listener/api_test.go` is the regression lock CI runs; it makes a cache trail deliberately
rather than by a bucket being recreated under it.
