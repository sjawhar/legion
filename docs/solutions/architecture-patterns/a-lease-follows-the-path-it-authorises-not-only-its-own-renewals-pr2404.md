---
title: "A lease follows the path it authorises, not only its own renewals"
category: architecture-patterns
tags:
  - lease
  - leader-election
  - take-over
  - liveness
date: 2026-10-31
status: active
module: packages/envoy/cmd/listener
related_issues:
  - "sjawhar/legion#2404"
---

# A lease follows the path it authorises, not only its own renewals

## The rule

1. When a lease renews over one path (its own connection, a store) and its holder does the work
   over another (a consumer subscription, a volume), the two can fail apart. Read the work path's
   health inside the lease's own loop, once a round. When it has been down for a bound, release
   the lease, however well the renewals land. Otherwise the lease elects the process with the
   healthiest lease connection, not the one that can serve.
2. Use the same signal to refuse a take. A process whose work path is down takes no record,
   absent, released or expired, so the lease cannot bounce back to it.
3. Choose the bound between two measured numbers. It must be longer than a reconnect that
   comes back on its own, so a short drop keeps the lease. It must be short enough that
   detection, the bound, the standby's poll and its hold still fit the take-over bound.
4. Judge "am I serving" against the authority that owns the work path, never by comparing two of
   your own copies. Two copies that one failing path updates together agree on a stale value and
   read healthy while nothing is delivered.
5. A release for this reason carries no clean-hand-back mark and owes the full hold, as a lapse
   does, since what the dead path held comes back only at its ack wait.

## Evidence

- The tester measured 602 s before a standby delivered when only the holder's receive
  connection was cut: the lease renewed on its own connection until self-health killed the pod.
  Commit 580089b5 added `leaseReceiveLost` (3 s) and `releaseReceiveLost` and gated `poll` on
  the same signal (`packages/envoy/cmd/listener/lease.go`). Its row
  `TestTwoPodsReceiveConnectionLostHandsOverWithin15s` failed first. Afterwards the standby took
  the lease 6.16 s after the cut and delivered 289 messages in order, the first at 12.1 s.
- The 3 s bound sits above a measured reconnect that came back 1.008 s after detection
  (`TestTwoPodsReceiveConnectionBackInsideTheBoundKeepsTheLease`). It sits inside the 15 s
  take-over bound: about 3 s detection, up to 1 s until the lease's step, 3 s, 0.5 s poll, 7 s
  hold.
- Rule 4: before commit 581c5f4b, `receiveUp` compared the receive loop's copy of the connection
  with the term's subscription connection, and both named the connection the client had replaced
  when a reconnect hook ahead of the receiver's kept failing. They now must both equal
  `bus.Client.ConnectedConn()`. `TestAReplacedConnectionBehindAFailingHookReadsDownAndReleasesTheLease`
  (`cmd/listener/receive_signal_test.go`) failed first.
- The opposite direction, not counting the renewal service's own outage against the lease, is
  docs/solutions/envoy/a-lease-renewed-through-a-service-does-not-run-out-while-no-instance-of-it-serves-INFRA-487.md.
