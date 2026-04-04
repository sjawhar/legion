---
title: "Testing NATS KV with real containers: TTL polling, dual mock servers, and decomposed helpers"
category: testing
tags:
  - nats
  - kv
  - testcontainers
  - integration-testing
  - envoy
  - ttl
date: 2026-04-04
status: active
module: envoy
related_issues:
  - "197"
symptoms:
  - "TTL expiry test is flaky"
  - "can't test KV-first vs file-fallback precedence"
  - "test helper always sets up both registries"
---

# Testing NATS KV with Real Containers

## Context

Envoy's session registry uses NATS KV with TTL-based expiry. Testing this requires real NATS (not mocks) because TTL expiry behavior, bucket creation, and watch semantics are NATS-specific. These patterns emerged from the `envoy_sessions` KV migration (#197).

## Pattern 1: Polling Loop for TTL Expiry (Not Fixed Sleep)

NATS TTL expiry is not instantaneous — it runs on the server's GC cycle. Fixed `time.Sleep(TTL + epsilon)` is flaky because epsilon is environment-dependent.

**Use deadline + poll instead:**

```go
deadline := time.Now().Add(5 * time.Second)
for time.Now().Before(deadline) {
    time.Sleep(500 * time.Millisecond)
    if _, err := reg.Get("ses_expiry"); err != nil {
        return // expired — test passes
    }
}
t.Fatal("session entry did not expire within 5s (TTL was 2s)")
```

This exits as soon as expiry is observed — faster on fast machines, more reliable on slow ones.

## Pattern 2: Functional Options for Test-Configurable TTL

Production defaults (5-min TTL, 3 replicas) are too slow for tests. Use functional options to shrink them:

```go
// Production: 5-min TTL, 3 replicas (defaults in function body)
sessions, _ := session.OpenSessionRegistry(conn)

// Tests: 2-second TTL, 1 replica
sessions, _ := session.OpenSessionRegistry(conn,
    session.WithSessionReplicas(1),
    session.WithSessionTTL(2*time.Second))
```

**Extend shared test helpers with variadic options** for backward compatibility:

```go
func setupTestEnv(t *testing.T, options ...testEnvOption) *testEnv
// Existing calls: setupTestEnv(t) — still work (default 10s TTL)
// New TTL tests: setupTestEnv(t, withSessionTTL(2*time.Second))
```

Adding parameters instead of options breaks all existing call sites.

## Pattern 3: Dual Mock Servers for Routing Precedence

To verify KV-first delivery takes precedence over file fallback, use **two separate `httptest.Server` instances** with independent counters:

```go
// KV port (should be used)
var kvDeliveries atomic.Int32
kvMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    kvDeliveries.Add(1)
    w.WriteHeader(http.StatusNoContent)
}))
kvPort := mockPort(kvMock.URL)

// File port (should NOT be used)
var fileDeliveries atomic.Int32
fileMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    fileDeliveries.Add(1)
    w.WriteHeader(http.StatusNoContent)
}))
filePort := mockPort(fileMock.URL)

// Register KV with kvPort, file with filePort
// Deliver → assert kvDeliveries == 1, fileDeliveries == 0
```

This proves which path was taken without mocking internals. Reusable any time you need to verify routing precedence between multiple sources.

## Pattern 4: Decomposed Setup Helpers

When adding a new data source to a system with existing tests, split monolithic setup helpers into atomic primitives:

```go
registerInterest(sessionID, topics)     // KV interests only
registerFileSession(sessionID, port)    // filesystem only
registerKVSession(sessionID, port)      // KV sessions only
registerSession(sessionID, port, topics) // file + interest (backward compat)
```

New tests compose from primitives to control exactly which sources are populated. The composite helper preserves backward compat for existing tests.

## Pattern 5: Cross-File Test Helper Sharing

Test files in the same Go package share unexported helpers. `session_test.go` reuses `setupNATS(t)` defined in `registry_test.go` because both are `package session`.

**Implicit coupling warning**: If `registry_test.go` is moved or the package is split, `session_test.go` breaks silently.
