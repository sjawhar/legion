---
title: "Liveness Sweep: Multi-Serve Session Detection Patterns"
category: daemon
tags:
  - health-tick
  - liveness
  - multi-serve
  - session-management
  - union-aggregation
  - false-positive-prevention
date: 2026-04-12
status: active
module: daemon
related_issues:
  - "447"
symptoms:
  - "workers show status running but serve sessions are dead"
  - "daemon restart leaves workers silently stalling"
  - "health tick does not detect dead serve sessions"
---

# Liveness Sweep: Multi-Serve Session Detection Patterns

## Problem

After a daemon restart, workers show status "running" but their serve sessions are gone. The health tick checked shared serve health but never verified individual sessions existed. Workers sat "running" indefinitely with no output.

## Solution: Union-Based Session Liveness

The health tick now collects active sessions from all serves (shared + role) into a union `Set<string>`, then checks each running worker's session ID against the union. Workers absent from the union are marked dead via the existing `PATCH /workers/:id` cleanup path.

## Key Patterns

### 1. Dual Failure-Tracking Sets

Two distinct failure modes require separate tracking to avoid false positives:

```typescript
const restartedRoles = new Set<string>(); // populated during role health check
const failedRoles = new Set<string>();    // populated during liveness query
```

- **`restartedRoles`**: A restarted serve has fresh sessions — old worker session IDs won't match, producing false positives.
- **`failedRoles`**: A serve that threw on `listActiveSessions()` means incomplete data — workers on that serve must not be reaped.

Both sets are checked before marking any worker dead:

```typescript
if (roleAdapter !== resolvedDeps.adapter &&
    (failedRoles.has(role) || restartedRoles.has(role))) {
  continue; // skip — can't determine liveness for this worker
}
```

**Critical lesson**: The "obvious" guard (restart) is easy to implement; the "subtle" guard (query failure) is easy to miss. Both are required.

### 2. Primary Source Gate (`sharedServeQueried`)

The reap loop is gated on a boolean set only when the shared serve query succeeds:

```typescript
let sharedServeQueried = false;
if (serveHealthy && !restartReason) {
  try {
    const sessions = await adapter.listActiveSessions();
    for (const s of sessions) allActiveSessions.add(s);
    sharedServeQueried = true;
  } catch { /* non-fatal */ }
}
// ...role serve queries...
if (sharedServeQueried) {
  // reap loop runs only with a reliable baseline
}
```

This is better than checking `allActiveSessions.size > 0` (wrong if the serve genuinely has zero sessions). The pattern: use a boolean gate for "primary source succeeded," not the aggregated data's emptiness.

### 3. Feedback Events After Confirmed State Change

Only emit `daemon.worker_reaped` after the PATCH confirms success:

```typescript
const patchRes = await fetch(url, { method: "PATCH", ... });
if (patchRes.ok) {
  feedbackLogger?.log({ event: "daemon.worker_reaped", ... });
}
```

Any feedback event representing a state change should be emitted only after the operation confirms success. Otherwise, monitoring sees a "reaped" event but the worker is still running.

### 4. Self-HTTP PATCH for Cleanup Reuse

Rather than extracting the dead-marking logic (crash history, Envoy detach, state persistence), the sweep calls the daemon's own `PATCH /workers/:id` endpoint. This reuses all existing cleanup without code duplication. Tradeoff: requires injectable `fetch` in `DaemonDependencies` (already present).

## Testing Pattern

Health tick tests capture the `setTimeout` callback and invoke it manually:

```typescript
let timeoutCallback: TimeoutCallback | null = null;
const mockSetTimeout = Object.assign(
  ((callback, _delay, ..._args) => {
    timeoutCallback = callback;
    return {} as ReturnType<typeof setTimeout>;
  }) as unknown as typeof setTimeout,
  { __promisify__: setTimeout.__promisify__ }  // Required for TypeScript
);
```

The `__promisify__` assignment is non-obvious but required to satisfy `typeof setTimeout`.

## Known Limitation

No concurrency protection between reading the state file and PATCHing the worker. A newly replaced worker could theoretically be marked dead by a stale sweep. Low risk: the health tick runs every 60s, and the PATCH endpoint validates the worker still exists.

## Generalization

When aggregating data from multiple independent sources where partial failure matters:
1. Track failures per source, not globally
2. Gate the consumer on the primary source's success
3. Skip consumers that depend on failed sources
4. Emit side effects (events, notifications) only after confirmed state changes
