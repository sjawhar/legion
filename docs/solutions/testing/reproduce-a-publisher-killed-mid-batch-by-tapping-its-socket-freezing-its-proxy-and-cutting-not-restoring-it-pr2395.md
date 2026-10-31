---
title: "Reproduce a publisher killed mid-batch by tapping its socket, freezing its proxy, and cutting, not restoring, it"
category: testing
tags:
  - multiprocess
  - nats
  - testnats
  - crash-recovery
  - ordering
date: 2026-10-28
status: active
module: packages/envoy/cmd/dispatch
related_issues:
  - "sjawhar/legion#2395"
---

# Reproduce a publisher killed mid-batch by tapping its socket, freezing its proxy, and cutting, not restoring, it

To prove what happens when a process dies with a send in flight:

1. Put a tap (a TCP relay that keeps every byte the process writes) between the process and its
   `testnats.Proxy`. The tap is the mid-batch signal: it sees the envelope leave the process even
   when nothing in the process logs it and no row records it.
2. `Freeze()` the proxy, write the first event, and wait until the tap shows its envelope leave.
   Write the second event, then SIGKILL the process.
3. `Cut()` the proxy before `Restore()`. A restore alone forwards the bytes the dead process left
   in the frozen proxy, so the first event lands first and hides the bug. A cut drops them, as a
   dead host's unsent data never reaches the server.
4. Restart the process and read the stream: assert the order by stream sequence (first copy per
   event id) and a time bound from when the restarted process is healthy.

The same signal works on the code before the fix, since it reads only the wire.

## Evidence

- `packages/envoy/cmd/dispatch/multiprocess_test.go:1593`
  (`TestAPublisherKilledMidBatchAndRestartedPublishesTheIssuesCommentsInOrderWithin10Seconds`),
  `:1641` (the cut) and `:1744` (`startNATSTap`).
- On main's code the row failed as a live incident had: "the stream holds comment 2 at sequence 3
  and comment 1 at 4", and comment 1 was published 59.86 s after the restart (the old one-minute
  row claim). With the single-publisher hold, both were published within 0.1 s, in order.
