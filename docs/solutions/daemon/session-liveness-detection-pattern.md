---
title: "Session Liveness Detection in Health Tick"
category: daemon
tags:
  - health-tick
  - liveness
  - session-management
  - worker-lifecycle
  - runtime-adapter
  - dependency-injection
  - dead-worker
  - reliability
  - testing
date: 2026-04-12
status: active
module: daemon
related_issues:
  - "447"
symptoms:
  - "workers show status 'running' but sessions are dead after daemon restart"
  - "workers sit idle indefinitely with no output after serve restart"
  - "session recreation failures are silently swallowed"
---

# Session Liveness Detection in Health Tick

## Problem

After a daemon restart, workers show status `running` but their serve sessions are actually dead. The daemon recreates worker entries from `workers.json` but session recreation is best-effort — failures are silently swallowed. Workers sit `running` indefinitely with no output, and the controller never learns they died.

The root cause: `scheduleHealthTick()` checked shared serve health (`/global/health`) but never verified that individual worker sessions existed on the serve.

## Solution Pattern

Add a **session liveness sweep** to the health tick loop. The sweep:

1. Fetches the full active session set in one bulk call (`listActiveSessions()`)
2. Compares all `running` workers against the set
3. PATCHes any worker whose session is absent to `status: dead`

```typescript
// In scheduleHealthTick(), after serve health check and restart logic:

// Session liveness sweep — only when serve is healthy AND not just restarted
let activeSessions: Set<string> | null = null;
try {
  activeSessions = await resolvedDeps.adapter.listActiveSessions();
} catch (err) {
  console.warn(`[liveness] Failed to fetch active sessions (non-fatal, skipping sweep): ${err}`);
}

if (activeSessions !== null) {
  const livenessState = await resolvedDeps.readStateFile(config.stateFilePath);
  for (const worker of Object.values(livenessState.workers)) {
    if (worker.status !== "running") continue;
    if (activeSessions.has(worker.sessionId)) continue;

    // Session missing — mark dead
    await resolvedDeps.fetch(`http://127.0.0.1:${config.daemonPort}/workers/${worker.id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ status: "dead", crashCount: worker.crashCount + 1, lastCrashAt: new Date().toISOString() }),
    });
  }
}
```

**Critical guard**: The sweep must only run when `serveHealthy && !restartReason`. Newly-recreated sessions must not be immediately reaped. The `restartReason` variable is already set earlier in the tick when a restart occurs — no new state needed.

## RuntimeAdapter Extension Pattern

Add `listActiveSessions(): Promise<Set<string>>` to the `RuntimeAdapter` interface. Returns a `Set` (not array) for O(1) per-worker membership tests.

```typescript
// In runtime/types.ts
interface RuntimeAdapter {
  // ... existing methods ...
  listActiveSessions(): Promise<Set<string>>;
}

// In runtime/opencode.ts
async listActiveSessions(): Promise<Set<string>> {
  const client = createWorkerClient(this.port, "");
  const result = await client.session.status();
  if (result.error || !result.data) {
    throw new Error(`Failed to list sessions: ${JSON.stringify(result.error ?? "no data")}`);
  }
  return new Set(Object.keys(result.data as Record<string, unknown>));
}
```

**Throw on error** — don't return an empty set. The caller uses `null` as the sentinel for "skip sweep" (see below). An empty set would incorrectly reap all workers.

## Null Sentinel for Non-Fatal Skip

Use `null` as the sentinel for "sweep skipped due to error", not an empty set or a boolean flag:

```typescript
let activeSessions: Set<string> | null = null;
try {
  activeSessions = await adapter.listActiveSessions();
} catch (err) {
  console.warn(`[liveness] Non-fatal: ${err}`);
  // activeSessions stays null — sweep skipped
}

if (activeSessions !== null) {
  // Only runs when we have a valid session list
}
```

This is cleaner than try/catch inside the loop and avoids the "empty set = reap everyone" footgun.

## Mock Update Burden

Adding a method to `RuntimeAdapter` requires updating **every mock** in every test file. For this PR, 6 files needed `listActiveSessions: async () => new Set<string>()` added. This is a recurring cost of the DI pattern.

**Mitigation**: When adding a new `RuntimeAdapter` method, search for all mock sites:
```bash
grep -rn "listActiveSessions\|createSession\|deleteSession\|getSessionStatus" packages/daemon/src --include="*.ts" | grep "async () =>"
```
Update all of them before running tests.

## Feedback Event Schema

Emit `daemon.worker_reaped` when a worker is reaped by the liveness sweep:

```typescript
feedbackLogger?.log({
  event: "daemon.worker_reaped",
  workerId: worker.id,
  sessionId: worker.sessionId,
  mode: workerMode,          // extracted from worker.id suffix
  serveType: "shared",       // anticipates future role-serve support
  reason: "session_missing",
});
```

The `serveType` field is forward-looking for multi-serve support (AC7) — when role serves are added, the sweep will need to check each worker against its own serve's session list, and `serveType` will distinguish them in logs.

## Test Matrix for Health-Tick Side Effects

For any feature that runs conditionally inside the health tick, test all four guard combinations:

| serveHealthy | restartReason | Expected |
|---|---|---|
| false | any | Skip (serve unhealthy) |
| true | set | Skip (just restarted) |
| true | null | Run sweep |
| true | null, throws | Skip (non-fatal error) |

Plus the positive case: worker present in session list → not reaped.

```typescript
// Pattern: capture calls to verify sweep was skipped
const listActiveSessionsCalls: number[] = [];
const adapter = {
  listActiveSessions: async () => {
    listActiveSessionsCalls.push(1);
    return new Set<string>();
  },
};
// ... trigger tick ...
expect(listActiveSessionsCalls).toHaveLength(0); // sweep skipped
```

## Generalization

This pattern applies to any daemon monitoring feature that needs to detect stale/dead state:

1. **Bulk fetch once** — one API call per tick, not per item
2. **Null sentinel for skip** — distinguish "no data" from "empty data"
3. **Compound guard** — `healthy && !justRestarted` before any sweep
4. **Non-fatal errors** — log and skip, never crash the tick
5. **PATCH via daemon HTTP** — reuse existing dead-marking path (cleanup, Envoy detach, state persistence)
6. **Forward-looking event fields** — include `serveType` even when only one serve exists
