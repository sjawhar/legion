---
title: "Attribute an out-of-band failure by its own key and time, not by the connection's last error"
category: envoy
tags:
  - nats
  - bus
  - error-classification
  - metrics
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2508"
---

# Attribute an out-of-band failure by its own key and time, not by the connection's last error

A transport that reports a failure on the connection rather than in the call's own answer gives you
one slot for many operations. nats.go keeps **one last error per connection**
(`(*nats.Conn).LastError`), and a NATS server answers a refused publish with
`-ERR 'Permissions Violation for Publish to "<subject>"'` and no acknowledgement, so the publish's
own answer is a timeout. Reading that slot to name one operation is right only while exactly one
operation is in flight.

- Record the evidence where the transport reports it — the asynchronous error callback — keyed by
  **what it names** (here the subject), not in a single slot.
- Keep the records of one key as a **queue in arrival order**, because the server reports one
  violation per refused message: one entry per key answers one of the messages a batch sent to it
  and leaves every other to its timeout.
- Let each operation take the record that answers **it**: the oldest entry recorded at or after its
  own send time, removed as it is read. The send time is what stops a stale record naming a later
  operation whose failure has another cause.
- Bound the record three ways, or it is a leak on a connection the server refuses: drop what a
  retention has spent, cap how many entries one key keeps (measure the cap against the largest
  batch one caller can have in flight), and clear everything when the connection is replaced or
  reconnects — the new session's grant is its own.
- **Count at every exit, not only where the outcome is classified.** A failure answered before the
  operation is recorded never reaches the place that classifies answers, so a counter written there
  reads zero for exactly the outage it exists for.

## Evidence

`internal/bus/denials.go` holds the record (`recordPublishDenial`, `deniedPublish`,
`publishDenialRetention` of one minute, `publishDenialDepth` of 256 measured against the outbox's
`batchSize` of 100), fed from `Client.onAsyncError` in `internal/bus/nats.go` and cleared by
`install` and `onReconnect`. Both JetStream paths read it with their own subject and send time:
`window.acknowledged` (`internal/bus/async.go`) and `publishJetStream` (`internal/bus/nats.go`).

Each bound came from a round that failed without it, on a real nats-server with a real per-client
grant:

- the single last error named only the destination whose violation landed last, so a notifying
  event's other destination (its session route) answered `ErrUnavailable`; the Dispatch outbox read
  that as an outage, released the event at INFO with no attempt counted, counted `reason="failed"`,
  and abandoned the rest of that scan's backlog;
- one entry per subject then answered only one of two comments a person wrote a moment apart on one
  issue, which share an owner topic and one publish window — the commonest traffic there is — and
  left the other to the same pathology.

`TestAWindowAnswersEachDeniedJetStreamSendItsOwnDenial` and
`TestAWindowAnswersEachDeniedSendOfOneSubjectItsOwnDenial`
(`internal/bus/publish_denied_window_test.go`) pin both, each red before its fix with
`bus: NATS is unreachable: nats: timeout waiting for ack`.

The counting rule is the same incident: `attempt.answer` classifies each destination's outcome, but
`publishDestination` (`internal/dispatch/outbox/publisher.go`) returns an `ErrUnavailable` that is
already in the channel **before** the destination enters `a.sent`, so the unreachable-NATS case
counted nothing while the same error answered a moment later counted `failed`.
