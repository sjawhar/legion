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
  - "INFRA-261"
---

# A live-session list is a cache view: decide one session's absence by its own record

Extends docs/solutions/envoy/a-cache-miss-must-not-release-what-another-listener-wrote.md.

That rule is about a cache miss inside one listener. This is its cross-service half: the listener
**exports** that cache, so a consumer of `GET /v1/sessions` inherits the same defect one process
further out, where it has no way to see it.

## Rules

- **A row missing from `GET /v1/sessions` is not evidence that the session ended.** That route
  answers from `sessions.List()`, the watcher-fed cache alone, and that cache trails the bucket.
  Since INFRA-261 the listener no longer empties the cache to read the bucket again (recreating the
  session bucket, or rebuilding the hub machine, starts such a read): every row stays until the
  new read completes. And it refuses the list, rather than answer from a copy it cannot stand
  behind, before the copy's first complete read and once the copy has not followed the bucket for
  longer than its stale bound. That closes the long window in which a working session was simply
  not in a `200`; it does not make a `200` authoritative. A session another listener registered a
  moment ago is in the bucket before it is in this copy, and a copy inside its bound may trail.
  Decide a session's absence only from an authority that answered about **that session**:
  `GET /v1/registry/<session_id>`, which reads through to the bucket.
- **A refusal is no list at all.** The listener's `503` with `Retry-After: 5` and code
  `refilling` (from the route, or from the closed `/v1` start gate) means "could not read", never
  an empty list. Dispatch passes it on as `503 ENVOY_REFILLING`, and as route status `unknown` with
  `route_status_reason: "refilling"`, never `no_holder`; every web view says the list is refilling
  where it would have said an agent is absent. A script that reads the list does the same: a
  non-200 is a list it could not read.
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
