---
title: "Stop an internal/bus subscription through the client, never the handle it returned"
category: envoy
tags:
  - nats
  - internal-bus
  - reconnect
  - shutdown
date: 2026-10-29
status: active
module: packages/envoy/internal/bus
---

# Stop an internal/bus subscription through the client, never the handle it returned

`bus.Client` keeps a registration for its one JetStream subscription and binds it again after a
reconnect, so the `*nats.Subscription` a `Subscribe` returned stops being the live one.

- Stop a subscription with `bus.Client.DrainSubscription(sub)`. It drains the handle the client
  holds now (the rebound one after a connection replacement), gives the registration up so no
  later reconnect binds it again, and returns the handle so the caller can wait for the drain.
  Never call `sub.Drain()` or `sub.Unsubscribe()` on the returned handle to stop for good.
- After a `Subscribe` that failed, call `bus.Client.ReleaseFailed(subject)`. The client keeps a
  failed registration so a restore can bind it, and a restore would bind a callback you have
  already abandoned.
- Test both on a replaced connection, not a reconnect in place: `nats-server` restarted on the same
  store only reconnects in place (`MaxReconnect -1`), so close the client's connection
  (`Conn.Close`, or a proxy that sends `-ERR`) and wait for `resubscribing` in the log.

## Evidence

Pull request sjawhar/legion#2391. At commit 4f9bd6e8 the intake's stop drained the handle
`Subscribe` returned. After a connection replacement it logged
`drain the intake's subscription error="nats: connection closed"`, and the restored subscription
stayed bound to the stopping process until its client closed (review thread on
`delivery/intake.go`, fixed in 823f782f, simplified in a56be739). The rules are in
`internal/bus/nats.go` (`DrainSubscription`, `ReleaseFailed`), their callers in
`internal/dispatch/delivery/intake_delivery.go`, and the tests in `internal/bus/release_test.go`
and `TestAStopAfterTheConnectionWasReplacedDrainsTheRestoredSubscription`.
