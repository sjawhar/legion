---
title: Receipt-backed core NATS delivery to current holders
category: architecture-patterns
tags:
  - nats
  - core-nats
  - request-reply
  - delivery-receipts
  - role-routing
  - session-liveness
  - error-sentinels
  - receipt-timeout
date: 2026-08-24
status: active
module: envoy
problem_type: architecture_pattern
component: tooling
severity: high
applies_when:
  - A message must reach the process that currently holds a role, lease, or session-owned route
  - A registry or KV lookup selects the recipient before core NATS delivery
  - An undeliverable control message must produce an observable failure
  - Application logic keys a decision on a vendor library's error sentinel
  - A client-writable field is widened to carry a value the delivery path also mints internally
related_issues:
  - "LEGION-108"
  - "sjawhar/legion#1079"
  - "LEGION-101"
---

# Receipt-Backed Core NATS Delivery to Current Holders

## Context

A role, lease, or session registry can identify a current holder and show that its heartbeat is fresh. That is useful routing state, but it does not prove that the holder still has a live core-NATS subscription or that its receiver pump can accept an envelope. Core NATS `Publish` reports success after accepting the publish locally even when zero subscribers match the subject.

The Envoy role lane needs a different contract: a role message is live-only and must reach the current holder or become an explicit exception. Review hardening and a real listener-and-NATS proof exposed the gap: after a holder process was stopped, the listener recorded successful forwarding although the message had no recipient.

## Guidance

Make the listener the sole core-NATS subscriber for a role lane. At delivery time, it reads the current role holder from the authoritative registry, verifies the holder's session registration is fresh, then sends the original envelope to that holder's direct agent subject with NATS request-reply.

The agent-subject receiver sends its reply only after it has accepted the envelope for steering injection. The listener treats that response as the delivery receipt. A two-second request timeout after the forward was published and flushed to a registered, live-looking holder is `receipt_timeout` (LEGION-108): no receipt arrived, usually because the holder is busy — but a holder whose registration is still fresh while its process is already gone times out the same way and never had the envelope, so the listener cannot tell slow from dead; the Legion daemon probes the process to decide (the parent contract, LEGION-101) and treats a live process as having the message. The signal is receipt-only: `bus.Client.RequestCoreTo` returns its `ErrReceiptTimeout` sentinel from the receipt wait alone, and a flush that fails or times out (the nats client's `Flush` returns the same `nats.ErrTimeout` a receipt wait would, so it must never be the key) is an ordinary error — the forward is still buffered in a reconnecting or stalled connection and is not known to have left the process. A holder absent from the session registry, a stale registration, or a request that could not be published or flushed is `delivery_failed`; an absent role claim is `no_holder`. Every reason publishes the original envelope on the exception lane.

Session registry and KV checks remain valuable gates, but they must not be promoted into proof of active delivery. They establish that a route was recently registered; the receipt establishes that a live receiver accepted this specific message.

## Why This Matters

A direct core-NATS publish has no subscriber-count error path. Treating `Publish` success as delivery turns any “deliver to the current holder of X” route into a silent black hole when the holder crashes, its subscription disappears, or its pump has gone deaf.

Request-reply narrows the guarantee precisely: it proves the selected receiver accepted the envelope before the timeout. It does not claim end-user completion or durable processing. That boundary is intentional and keeps live role delivery distinct from durable JetStream notification paths.

## When to Apply

- A current holder is selected from a role, lease, leader-election, or session registry and must receive a live control message.
- A missing recipient must be observable to operators or an exception consumer rather than silently discarded.
- A recent KV entry or heartbeat would otherwise be used as evidence that the target is reachable.

Do not use this receipt path merely to make durable notification delivery synchronous. Use the durable delivery mechanism's own acknowledgement and retry contract for messages that must survive an unavailable recipient.

## Examples

### Incorrect: publish success is treated as delivery

```go
sessionID, err := registry.RoleHolder(role)
if err != nil {
    publishDeliveryException(client, item, "delivery_failed")
    return
}
if sessionID == "" {
    publishDeliveryException(client, item, "no_holder")
    return
}
if err := client.PublishCoreTo(contracts.AgentSubject(sessionID), item); err != nil {
    publishDeliveryException(client, item, "delivery_failed")
}
// A nil error here does not prove an agent subscribed to the subject.
```

### Correct: require the selected receiver to reply

```go
sessionID, err := registry.RoleHolder(role)
if err != nil {
    publishDeliveryException(client, item, "delivery_failed")
    return
}
if sessionID == "" {
    publishDeliveryException(client, item, "no_holder")
    return
}

if err := client.RequestCoreTo(
    contracts.AgentSubject(sessionID),
    item,
    2*time.Second,
); err != nil {
    if errors.Is(err, bus.ErrReceiptTimeout) {
        // Published and flushed, no receipt: a slow holder, or one that died
        // inside the registry's stale window. The daemon probes it; the
        // listener does not. Never key on nats.ErrTimeout here: the client's
        // Flush returns that same sentinel while the forward is still buffered.
        publishDeliveryException(client, item, "receipt_timeout")
        return
    }
    // The forward itself failed: the holder never received anything.
    publishDeliveryException(client, item, "delivery_failed")
    return
}
```

The agent pump replies as soon as the forwarded envelope is decoded, before injecting it or calling any service; the receipt says "a live receiver has this frame", not "the host has finished with it" (LEGION-101):

```ts
await deliver(message.subject, codec.decode(message.data), message.reply ?? "");

// inside deliver(), immediately after renderInbound() returns and before the
// inbox update, any Dispatch call, or pi.sendMessage
if (reply !== "" && subject === agentSubject(sessionID)) {
  (await ensureConnection()).publish(reply);
}
```

This makes a fresh-but-deaf holder observable as `receipt_timeout` and a holder whose registration has gone stale as `delivery_failed`, while a live holder returns one receipt. The receipt comes from the direct agent receiver, not from JetStream persistence of the forwarded copy.

## Pitfalls learned on LEGION-108 (`sjawhar/legion#1079`)

The first cut of the three-reason model keyed `receipt_timeout` on `errors.Is(err, nats.ErrTimeout)` straight from `RequestCoreTo`. CI was green, the Go suites on the NATS testcontainer were green, and the tester's fault injections were green. Review round 1 found it by reading the vendor source: it would have lost messages in production.

### One vendor sentinel, two exits: mint your own at the exit that means what you need

nats.go v1.50.0's `Conn.Flush()` is `FlushTimeout(10 * time.Second)` and returns the very same `nats.ErrTimeout` value the receipt wait's `NextMsg` returns. `RequestCoreTo` flushes between `PublishRequest` and the receipt wait, so on a reconnecting or stalled connection — `ensureConnWithContext` hands back any connection that is not `CLOSED`, `SubscribeSync` and `PublishRequest` buffer without error — the *flush* timed out with the forward still in the client's buffer, and the caller's `errors.Is(err, nats.ErrTimeout)` read that as "forwarded, no receipt". Under the LEGION-101 contract the daemon treats `receipt_timeout` from a live process as delivered and does nothing, so that message was gone; before the change it had been `delivery_failed` and recovered. Three rules fall out:

- Do not key an application decision on a vendor sentinel that more than one call site in your own function can produce. Mint a package sentinel (`bus.ErrReceiptTimeout`) and return it from exactly the exits that carry the meaning — here the post-flush deadline check and a `NextMsg` timeout — mapping the vendor value to yours at those exits only.
- Wrap every other failure with `%w` under a distinct message (`bus: flush forward: %w`), so `errors.Is(err, nats.ErrTimeout)` stays true for diagnostics while the consumer keys on the package sentinel alone. The consumer's table test then has a row for the raw vendor value and a row for the wrapped one, both mapping to `delivery_failed`.
- Bound the flush by the same deadline as the receipt wait (`FlushTimeout(time.Until(deadline))` behind a guard for a window that has already elapsed), or the call outlives its window by the client's default 10 s while the exception consumer believes the window is two seconds.

The regression lock for the flush branch needed a real stalled transport — the fault injection that swapped the flush error for the sentinel stayed green in both committed suites. How it was driven, and the seams that make the consumer side table-testable, are in [Locking an error-to-reason mapping with an injectable forward seam and a paused NATS testcontainer](../testing/injectable-forward-seam-and-paused-nats-testcontainer-lock-an-error-to-reason-mapping.md).

### The listener cannot tell deaf from slow, by construction

`RequestCoreTo` publishes with `PublishRequest` to a manually subscribed inbox; it never uses `Conn.Request`, so there is no `ErrNoResponders` path. A holder whose session registration is still inside `ClaimStaleAfter` but whose process is gone therefore looks exactly like a busy holder: the publish succeeds, the flush succeeds, no receipt arrives, `receipt_timeout`. That is the intended division of labour — the registry check bounds how long a dead holder can look live, and the daemon owns the process probe that tells slow from dead — so a `receipt_timeout` consumer must not read it as "the holder has the envelope". Write the docs that way too: an earlier wording of this page said exactly that and had to be corrected in review.

### The forward mark is an internal namespace; the API must refuse it

The forwarded copy carries `envoy.role.forward.` prefixed to its dedupe key so the role arbiter, which is also a subscriber on the agent stream, drops it on sight and never re-enters itself. While every API-minted key began `publish.` or `agent.`, that guard was unreachable from outside. The moment `POST /v1/messages/publish` accepted `dedupe_key` verbatim, a caller could send `dedupe_key: "envoy.role.forward.x"` to a role topic and get a 200 with `holder` set for a message the arbiter's first line discarded — no forward, no exception, no log, no metric. The rule: any value the delivery path drops on sight is a 400 at the API, checked before anything is published (`publishHandler` in `cmd/listener/api.go`; `TestPublishHandler_RejectsReservedDedupeKeyPrefix` proves it on a plain topic and on an unheld role topic, where the 400 must win over the 404). More generally, widening a client-writable field to a verbatim value re-exposes every internal prefix that field participates in; enumerate them when you widen.

### Deploy coupling: the consumer may not know the new reason yet

At the time this landed, the Legion daemon's `exceptionInfo` (`packages/daemon/src/daemon/events.ts`) accepted only `no_holder` and `delivery_failed` and returned `undefined` for anything else, so until LEGION-107 deploys a `receipt_timeout` exception is dropped by the daemon — including the dead-inside-the-stale-window holder that used to arrive as `delivery_failed` and be recovered. The parent spec accepts either deploy order (the resync probe is the backstop), but a worker judging whether this fix is effective in production must check that the consumer has landed, not just the producer.

## Related

- [NATS KV dual-bucket lifecycle](nats-kv-dual-bucket-lifecycle.md) distinguishes durable subscription interests from ephemeral session liveness; neither bucket proves a receiver is currently listening.
- [Envoy auto-subscription patterns](../daemon/envoy-auto-subscription-patterns.md) describes non-blocking subscription registration. That best-effort registration path is separate from receipt-backed directed control delivery.
