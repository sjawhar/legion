---
title: "Graceful degradation when NATS KV is unavailable"
category: architecture-patterns
tags:
  - nats
  - kv
  - envoy
  - graceful-degradation
  - nil-receiver
  - go-idioms
  - operational
date: 2026-04-04
status: active
module: envoy
related_issues:
  - "206"
symptoms:
  - "no suitable peers for placement"
  - "listener crashes on startup"
  - "KV bucket creation fails with single NATS peer"
  - "log.Fatal on optional feature initialization"
---

# Graceful Degradation When NATS KV Is Unavailable

## Context

PR#201 added a NATS KV session registry (`envoy_sessions`) for fast port lookups.
The code used `Replicas:3` and `log.Fatal(err)` on bucket creation failure. Production
had only 1 reachable NATS peer, so bucket creation failed with "no suitable peers for
placement" and the listener crashed on startup.

The dual-path delivery design (KV-first, file-fallback) already existed in `Deliverer.Deliver()`,
but the startup code treated KV as a hard dependency.

## Pattern: Nil-Receiver Safety for Optional Components

When a Go component is optional (may not be available at runtime), make its methods
safe to call on a nil receiver. This avoids scattering nil checks across every call site.

```go
var ErrNoKV = fmt.Errorf("session registry: KV unavailable")

func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
    if r == nil {
        return SessionEntry{}, ErrNoKV
    }
    // ... normal implementation
}
```

The caller passes `nil` when the component isn't available. Methods return a sentinel
error instead of panicking. The sentinel (`ErrNoKV`) distinguishes "feature unavailable"
from "key not found", enabling callers to handle each case differently.

### Startup: Warning, Not Fatal

```go
var sessions *session.SessionRegistry
if reg, err := session.OpenSessionRegistry(client.Conn); err != nil {
    log.Printf("WARN: KV registry unavailable, using file-only delivery: %v", err)
} else {
    sessions = reg
}
```

Reserve `log.Fatal` for truly required dependencies (NATS connection, config). Optional
enhancements degrade with a warning log so operators know the system is running in
reduced mode.

## Operational Gotcha: Replicas Must Match Peer Count

NATS JetStream KV requires `Replicas <= available_peers`. The original code defaulted
to `Replicas:3`, which fails silently on single-peer deployments. The fix changed the
default to `Replicas:1`.

**Rule of thumb**: Default replica count to 1 (works everywhere), allow override via
configuration for HA deployments. The `WithSessionReplicas(n)` functional option exists
for this purpose.

## Testing Degradation

Test nil paths explicitly — don't assume fallback works:

```go
func TestDeliver_NilSessionsFallsBackToFile(t *testing.T) {
    deliverer := Deliverer{
        RegistryDir: dir,
        Sessions:    nil, // KV unavailable
    }
    err := deliverer.Deliver(item, interest)
    // Assert: delivery succeeds via file fallback
}

func TestSessionRegistry_NilGetReturnsErrNoKV(t *testing.T) {
    var reg *SessionRegistry
    _, err := reg.Get("ses_test")
    // Assert: err == ErrNoKV (not a panic)
}
```

## Key Insight

Graceful degradation is cheaper than mandating high availability. Instead of requiring
3-peer NATS clusters in every environment, accept single-peer deployments with
file-based fallback. The nil-receiver pattern makes this safe and testable without
wrapper types or pervasive nil-checking.

## When to Apply

Any time you add an optional runtime dependency (cache, KV store, external service)
to an existing system that already has a working fallback path. The signals:

- The feature is an optimization, not a correctness requirement
- A fallback path already exists (or is cheap to add)
- Deployment topology varies (dev, staging, production)
- The dependency has its own failure modes (network, config, capacity)
