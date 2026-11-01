---
title: "NATS KV dual-bucket lifecycle: permanent subscriptions vs ephemeral session liveness"
category: architecture-patterns
tags:
  - nats
  - kv
  - ttl
  - envoy
  - session-registry
  - delivery
date: 2026-04-04
status: active
module: envoy
related_issues:
  - "197"
symptoms:
  - "session entry expired but subscription still exists"
  - "delivery uses wrong port after restart"
  - "KV cache masks TTL expiry"
---

# NATS KV Dual-Bucket Lifecycle: Permanent Subscriptions vs Ephemeral Session Liveness

## Context

Envoy manages two kinds of session metadata with fundamentally different lifecycles:

- **Subscriptions** (`envoy_interests`): What topics a session wants to hear about. Permanent — survives process restarts. An agent's subscription shouldn't vanish when its editor crashes.
- **Session liveness** (`envoy_sessions`): Where to deliver messages (port, host). Ephemeral — should expire when the process dies. A crashed process's port entry must auto-expire.

## Pattern: Separate Buckets for Separate Lifecycles

Use **two KV buckets** with different TTL policies rather than a single bucket with mixed semantics.

| Bucket | TTL | Cache | Purpose |
|--------|-----|-------|---------|
| `envoy_interests` | None (permanent) | Yes (local, watch-synced) | Subscription topics |
| `envoy_sessions` | 5 minutes | **No** (direct KV reads) | Port/host liveness |

### Why No Cache on the Session Registry

The interest `Registry` has a local cache because `Match()` needs to scan all entries — iterating KV on every message is too expensive. The session `SessionRegistry` has **no cache** because:

1. TTL expiry is the correctness mechanism — a cache would mask expired entries
2. Only point lookups are needed (`Get` by session ID), not scans
3. A 30-second cache would mean delivering to dead sessions for 30 seconds after crash

This is a deliberate design constraint. Do NOT add a cache to `SessionRegistry` to "optimize" without understanding this trade-off.

### Heartbeat-as-Re-Subscribe

The plugin doesn't send a dedicated heartbeat ping. Instead, it re-calls `POST /v1/interests/subscribe` every 2 minutes (with `port` in the payload) to refresh the 5-min TTL on `envoy_sessions`.

Benefits:
- No separate heartbeat endpoint to maintain
- Re-subscribing is idempotent (upsert semantics)
- First subscribe after restart re-establishes everything atomically

**Coupling warning**: The heartbeat interval (2 min, in TypeScript plugin) and TTL (5 min, in Go listener) are coupled but live in different files. Changing one without the other silently breaks liveness.

### KV-First Delivery with Silent Fallback

`Deliverer.Deliver()` tries KV first, falls back silently to the filesystem:

```go
// KV-first: try SessionRegistry for port
if d.Sessions != nil {
    if entry, err := d.Sessions.Get(interest.SessionID); err == nil && entry.Port > 0 {
        return d.prompt(entry.Port, interest.SessionID, text)
    }
}
// Fallback: file registry (silent — missing KV entry is not an error)
entry, _ := d.Find(interest.SessionID)
```

The fallback is intentionally silent — a missing KV entry just means the session uses the old filesystem path. Don't add logging here unless you want noise for every old-format session.

## Anti-Patterns

- **Merging buckets**: Don't combine interests and sessions into one bucket "for simplicity." The TTL difference is load-bearing.
- **Caching session entries**: Don't cache `SessionRegistry.Get()` results. The whole point of TTL is that stale entries disappear.
- **Fixed heartbeat = fixed TTL**: If you change one, change the other. Heartbeat must be well under TTL (currently 2min << 5min).

## When to Apply

Any time you have data with fundamentally different lifecycles sharing a backing store. The key signal is: "one thing should persist across restarts; the other should expire."
