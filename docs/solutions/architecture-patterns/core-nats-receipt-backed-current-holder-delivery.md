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
---

# Receipt-Backed Core NATS Delivery to Current Holders

## Context

A role, lease, or session registry can identify a current holder and show that its heartbeat is fresh. That is useful routing state, but it does not prove that the holder still has a live core-NATS subscription or that its receiver pump can accept an envelope. Core NATS `Publish` reports success after accepting the publish locally even when zero subscribers match the subject.

The Envoy role lane needs a different contract: a role message is live-only and must reach the current holder or become an explicit exception. Review hardening and a real listener-and-NATS proof exposed the gap: after a holder process was stopped, the listener recorded successful forwarding although the message had no recipient.

## Guidance

Make the listener the sole core-NATS subscriber for a role lane. At delivery time, it reads the current role holder from the authoritative registry, verifies the holder's session registration is fresh, then sends the original envelope to that holder's direct agent subject with NATS request-reply.

The agent-subject receiver sends its reply only after it has accepted the envelope for steering injection. The listener treats that response as the delivery receipt. A two-second request timeout, a missing receiver, or a receiver that fails before replying is `delivery_failed`; publish the original envelope on the exception lane. An absent role claim is instead `no_holder`.

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
    publishDeliveryException(client, item, "delivery_failed")
    return
}
```

The agent pump replies only after accepting the forwarded envelope for injection:

```ts
await deliver(message.subject, codec.decode(message.data), message.reply ?? "");

// inside deliver(), after pi.sendMessage accepts the envelope
if (reply !== "" && subject === agentSubject(sessionID)) {
  (await ensureConnection()).publish(reply);
}
```

This makes a fresh-but-deaf holder and a stopped holder observable as `delivery_failed`, while a live holder returns one receipt. The receipt comes from the direct agent receiver, not from JetStream persistence of the forwarded copy.

## Related

- [NATS KV dual-bucket lifecycle](nats-kv-dual-bucket-lifecycle.md) distinguishes durable subscription interests from ephemeral session liveness; neither bucket proves a receiver is currently listening.
- [Envoy auto-subscription patterns](../daemon/envoy-auto-subscription-patterns.md) describes non-blocking subscription registration. That best-effort registration path is separate from receipt-backed directed control delivery.
