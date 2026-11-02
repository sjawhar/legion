---
title: "A fresh JetStream consumer reports ack floor 0, so create it at a start sequence"
category: envoy
tags:
  - nats
  - jetstream
  - consumers
  - ack-floor
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-594"
---

# A fresh JetStream consumer reports ack floor 0, so create it at a start sequence

- nats-server v2.10.29 reports a consumer's `AckFloor.Stream` as 0 until an ack moves it. A new
  consumer's stored state sets only its delivered sequence. So a consumer that has delivered
  nothing, or whose first delivery is still unacked, reads as "acked up to nothing".
- A restart at the ack floor (read the floor, delete the consumer, create it again at floor + 1)
  then starts at stream sequence 1 and delivers the whole stream to a session that never asked
  for it.
- v2.10.29 has no consumer position reset: there is no `CONSUMER.RESET` API, and a consumer
  update refuses a changed deliver policy or start sequence. A restart is a delete and a create.
- Fix: create every such consumer at an explicit start sequence, never `DeliverNew`. A new
  consumer starts at the stream's last sequence + 1, which delivers what `DeliverNew` delivers and
  records `OptStartSeq`. Read the floor as the highest of `AckFloor.Stream`, `OptStartSeq - 1`,
  and, while `NumAckPending` is 0, `Delivered.Stream`.

## Evidence

`TestRestartAtFloorNeverDeliversWhatTheStreamHeldBeforeTheConsumer` in
`packages/envoy/internal/bus/session_consumers_test.go` runs against the in-process nats-server
v2.10.29 and logs what the server reports. On a stream whose last sequence is 3, a new consumer
created at sequence 4 reports an ack floor of 0, and it still reports 0 with its first delivery
unacked. `SessionConsumers.Ensure` and `ackFloor` in
`packages/envoy/internal/bus/session_consumers.go` hold the fix, and the test proves that a
restart at the floor delivers nothing the stream held before the consumer.
