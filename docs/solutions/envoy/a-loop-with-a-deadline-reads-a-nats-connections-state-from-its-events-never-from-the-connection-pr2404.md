---
title: "A loop with a deadline reads a NATS connection's state from its events, never from the connection"
category: envoy
tags:
  - nats
  - nats-go
  - reconnect
  - lease
  - liveness
date: 2026-10-31
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2404"
---

# A loop with a deadline reads a NATS connection's state from its events, never from the connection

Extends docs/solutions/envoy/nats-go-facts-a-test-or-a-shutdown-path-must-design-around-INFRA-182.md.

## The rule

1. Never call `nats.Conn.Status()`, `IsConnected()` or any other read of a `*nats.Conn` from a loop
   whose lateness has a consequence (a lease renewal, a heartbeat, a readiness answer with a
   deadline). nats.go holds the connection's lock across every reconnect dial and the connect
   init after it. On a route that drops packets, each of those waits for the connect timeout, so
   the read blocks for seconds at a time and the loop stops with it.
2. Do not hold any of your own locks across a nats call either, when the loop with a deadline can
   take that lock. Holding your lock while `Subscribe` waits for nats.go's lock is the same stall
   one step removed.
3. Keep the state the loop needs in atomics, and let the connection's own events write them: the
   connect, reconnect, disconnect and close callbacks. Key every write on the connection it is
   about, so a late callback of a replaced connection cannot clear or set its replacement.
4. Make those events the only source. Do not mix a read of the connection's state into the
   bookkeeping (for example "store it, then clear it if it is not connected"): a callback that
   lands between your read and your write is overwritten, and nothing writes the state again
   until the connection's next event.
5. Prove the loop with a test that holds nats.go's lock, or dials an address that accepts TCP and
   never answers, and requires the read to answer within about 100 ms.

## Evidence

- The tester's row `TestTwoPodsReceiveConnectionLostToADroppingRouteReleasesTheLeaseWithin15s`
  (`packages/envoy/cmd/listener/multiprocess_takeover_test.go`) failed 3 of 3. A goroutine dump
  showed `lease.step -> receiveDown -> durableReceiver.receiveUp -> bus.Client.Connected ->
  nats.Conn.Status` waiting on the lock that `doReconnect -> processConnectInit` held, so the
  lease's renewals stalled with its release.
- Commit 6828bbb3 moved `bus.Client.Connected` and `receiveUp` onto atomics (`connectedConn`,
  `installedConn`, the receiver's `clientConn` and `subscribed`). `TestConnectedAnswersWhileAReconnectDialHangs`
  (`internal/bus/connected_test.go`) and `TestTheReceiveSignalAnswersWhileTheReceiveConnectionsLocksAreHeld`
  (`cmd/listener/receive_signal_test.go`) each took 4.9 s with the old reads and now answer within
  100 ms.
- Rule 4 comes from this pull request's retro. `settleConnected` stored the connection, then read
  `nc.IsConnected()` and cleared the flag when it read false. A disconnect and a reconnect landing
  between the read and the clear left the flag reading down on a live connection. The pod then
  released its lease and took it no more, while `/healthz` answered 200 `degraded`.
