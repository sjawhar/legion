---
title: "A durable consumer's start is kept in the daemon's store, and a GitHub fact is deduplicated by its delivery"
category: daemon
tags:
  - jetstream
  - durable-consumer
  - dedupe
  - processed-events
date: 2026-10-29
status: active
module: packages/daemon
---

# A durable consumer's start is kept in the daemon's store, and a GitHub fact is deduplicated by its delivery

Extends docs/solutions/envoy/a-jetstream-answer-is-not-the-outcome-a-sequence-zero-duplicate-stored-nothing-and-a-timed-out-update-may-have-applied-pr2362.md. Its evidence that the daemon drops a repeat by event id only no longer holds: since this pull request a GitHub delivery's fact is keyed by its delivery.

- Record a consumer's position with the work it applies, in the same transaction
  (`consumer_positions`: the stream time of the last applied message, moving forward only). When
  the durable consumer is gone, recreate it `DeliverByStartTime` at that time: never "new" (it
  loses the gap) or "all" (it replays the stream). The one boundary message is dropped by the fact
  dedupe. Store the time, not the sequence: a recreated stream restarts its sequences.
- A message acknowledged without a transaction does not move the position, so an old stored time
  on a quiet consumer is no proof of loss: warn that events may have been missed, not that they
  were lost.
- Key a GitHub webhook fact by the envelope's `dedupe_key` (`github.<X-GitHub-Delivery>`). Not by
  `event_id`, which the listener mints per request, so a redelivery would be a new fact; and not by
  `dedupe_key` plus topic, because the listener copies one comment that mentions the trigger onto
  several topics under one key, and each copy is the same event.
- A dedupe key must outlive the ledger that holds it. `processed_events` is never pruned, and a
  checks settlement's `dedupe_key` can repeat once Envoy's CI record expires (7 days), so a
  settlement keeps its `event_id`; a reused key against a ledger that never forgets drops a real
  event.

## Evidence

- sjawhar/legion#2478: `packages/daemon/internal/intake/position.go`, `openConsumer` and
  `createConsumer` in `consume.go`, migration `0063_consumer_positions.up.sql`; proven by a real
  `legion start` whose deleted GitHub consumer came back `by_start_time` and applied the three
  events published meanwhile, once each. `envoyEnvelope.factID` in
  `packages/daemon/internal/intake/decode.go`; `dedupe_test.go` applies a review that mentions the
  trigger, published on three topics and again with new event ids, once.
