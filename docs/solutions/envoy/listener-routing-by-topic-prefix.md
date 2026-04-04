---
title: "Listener Routing Must Use Topic Prefix, Not Source Field"
category: envoy
tags:
  - envoy
  - routing
  - go
  - nats
  - integration-testing
date: 2026-04-04
status: active
module: envoy
related_issues:
  - "sjawhar-legion-211"
symptoms:
  - "envoy_publish messages not reaching subscribers"
  - "broadcast messages routed to direct agent delivery"
  - "registry.Match() never called for agent-sourced broadcasts"
  - "session not found for published topic"
---

# Listener Routing Must Use Topic Prefix, Not Source Field

When routing messages in `cmd/listener/main.go`, the routing predicate must discriminate on
**topic structure**, not the envelope `Source` field. `Source` describes who sent the message;
topic prefix encodes the delivery semantics.

## The Bug

The `/v1/messages/publish` endpoint (used by `envoy_publish`) sets `Source: "agent"` on all
envelopes. The listener used `item.Source == "agent"` to branch into direct session delivery,
which extracts a session ID from the topic. Broadcast topics like `notifications.legion.sprint-team`
were treated as session IDs — which don't exist — so messages silently vanished.

## The Fix

```go
// WRONG: source describes origin, not delivery intent
if item.Source == "agent" {

// CORRECT: topic prefix encodes delivery semantics
if strings.HasPrefix(item.Topic, "notifications.agent.") {
```

Both `envoy_send` and `envoy_publish` set `Source: "agent"`, but they differ in topic shape:
- `envoy_send` → topic `notifications.agent.<session_id>` → direct delivery
- `envoy_publish` → topic `notifications.legion.*`, `notifications.github.*`, etc. → broadcast fanout

## The `notifications.agent.` Namespace Is a Protocol Contract

After this fix, `notifications.agent.*` is effectively a reserved namespace for direct session
delivery. Any message with this topic prefix bypasses `registry.Match()` and goes straight to
the named session. All other topics go through interest-based fanout.

This is undocumented as a formal contract — future Envoy work touching topic naming or routing
should treat it as one.

## Integration Test Consumer Must Stay In Sync

`internal/integration/delivery_test.go` has a `startConsumer()` function that mirrors the
listener's routing logic. After this fix, `startConsumer()` still uses the old
`item.Source == "agent"` condition — a divergence that doesn't cause test failures because
existing test data never exercises the exact bug scenario (source="agent" + non-agent topic).

**Checklist for any future listener routing change:**
- [ ] Update `cmd/listener/main.go` — production routing predicate
- [ ] Update `internal/integration/delivery_test.go:startConsumer()` — must match production
- [ ] Add a test exercising the exact scenario the change addresses

The ideal long-term fix: extract the routing predicate into a shared function so both the
listener and test consumer reference a single source of truth.
