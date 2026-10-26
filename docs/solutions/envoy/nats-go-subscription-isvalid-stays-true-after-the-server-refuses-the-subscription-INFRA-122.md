---
title: "nats.go Subscription.IsValid stays true after the server refuses the subscription"
category: envoy
tags:
  - nats
  - nats-go
  - permissions
  - health
date: 2026-10-26
status: active
module: packages/envoy
related_issues:
  - "INFRA-122"
  - "sjawhar/legion#2199"
---

# nats.go Subscription.IsValid stays true after the server refuses the subscription

Extends docs/solutions/envoy/nats-go-facts-a-test-or-a-shutdown-path-must-design-around-INFRA-182.md.

- In nats.go v1.50.0, `Subscription.IsValid()` checks only that the subscription has a connection
  and is not closed (nats.go:4928-4934). A server's `Permissions Violation` for the subscription
  is recorded in the unexported `permissionsErr` (nats.go:3819-3824), which `IsValid` never reads.
  The same happens after a reconnect to a server that no longer grants the subject, and after a
  config reload that takes the grant away.
- To report whether a core subscription is live, keep your own flag. The connection's
  `ErrorHandler` sets it on `nats.ErrPermissionViolation` for that subject and queue, and a new
  send of the subscription clears it. A `Flush` and `LastError` at start covers only the start.
- The server quotes the subject and queue in that error with Go's `%q`, so read them back with
  `strconv.Unquote` before you compare them.

## Evidence

INFRA-122's live lane reports `live_topics` on `/healthz`. Review round 1 restarted NATS without
the `notifications.>` grant under the built listener: `/healthz` still said `subscribed`, and no
live topic arrived. Fixed in 7139c129, and for a reload in d73383ee:
`bus.Client.CoreSubOK` and `onAsyncError` in `packages/envoy/internal/bus/nats.go`. Accepted
residual: a grant taken away and given back by reloads, with no reconnect, stays 503 until the
listener restarts, because nothing sends the subscription again.
