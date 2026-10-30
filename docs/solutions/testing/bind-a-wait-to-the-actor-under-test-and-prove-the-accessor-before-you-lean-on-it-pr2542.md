---
title: "A wait over shared state can be satisfied by another actor's work: bind it to the client under test, and prove the accessor first"
category: testing
tags:
  - race-conditions
  - test-helpers
  - nats
  - jetstream
date: 2026-10-30
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2542"
---

"The stream now looks reconciled" is a claim about the stream. A second start against a stream a
first start already reconciled reads it as settled at once, so a wait on that reading returns before
this client's own work has run, and the test's next assertion races.

## Rules

- **Wait on a counter of this client's own completed work.** `bus.Client.HookedConnectionEvents()`
  (`internal/bus/nats.go:529`) counts the connection events whose reconcile hooks have run to
  completion for that client; the dispatch start test waits on
  `client.Connected() && client.HookedConnectionEvents() >= 1`
  (`cmd/dispatch/stream_replicas_test.go:43-52`).
- **Count only runs that succeeded**, or a refused reconcile satisfies the wait while the stream is
  untouched (`internal/bus/recovery.go:198-215` sets the count after the hooks succeed).
- **The hook must be registered inside the constructor.** A hook a caller adds after the constructor
  returns can miss the first connection event, because a hook run copies the list at its start
  (`internal/bus/nats.go:414`, with the trap stated at `:523-528`). An accessor whose count can skip
  the first event is not a wait condition.
- **Prove the accessor before any test leans on it**: zero before the first connection, at least one
  after the first reconcile, in a test of its own
  (`internal/bus/connect_background_test.go:53-79`).

## Coverage

New. The nearest pages,
`docs/solutions/testing/ask-the-thing-itself-at-the-moment-the-claim-is-about-INFRA-250.md` and the
negative-control pages beside it, say to read the system's state rather than the handle you hold;
neither states the actor-binding rule, nor the step of proving a new accessor's meaning before
using it as a gate.
