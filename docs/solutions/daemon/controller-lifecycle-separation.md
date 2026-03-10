---
title: Separating Manager Lifecycle from Managed Entities
date: 2026-02-15
category: daemon
tags:
  - architecture
  - lifecycle
  - state-management
  - testing
related_issues:
  - "LEG-122"
status: active
---

# Separating Manager Lifecycle from Managed Entities

## Problem

The daemon initially treated the controller as just another worker — registering it via self-calling HTTP as `controller-controller`, health-checking it alongside workers, and including it in `/workers` listings. This created architectural confusion: the controller is not a worker; it manages workers. The abstraction leak manifested in:

1. **Type pollution**: `WorkerMode` union included `"controller"`, forcing conditional logic throughout the codebase
2. **API confusion**: `POST /workers` accepted `mode: "controller"`, but controllers aren't workers
3. **Lifecycle coupling**: Controller startup/shutdown was entangled with worker management
4. **State file ambiguity**: Controllers lived in the workers map, requiring special-case filtering

## Solution

Pull the controller out into a dedicated, separately-tracked entity with its own lifecycle management:

### 1. Type System Separation

```typescript
// Before: controller polluted the worker type
type WorkerMode = "architect" | "plan" | "implement" | "review" | "merge" | "controller";

// After: controller has its own type
type WorkerMode = "architect" | "plan" | "implement" | "review" | "merge";
interface ControllerState {
  sessionId: string;
  port?: number;
  pid?: number;
}
```

**Pattern**: When an entity has fundamentally different lifecycle semantics, give it its own type. Don't shoehorn it into an existing union.

### 2. State File Structure

```typescript
// Before: controller was a worker entry
{
  workers: {
    "controller-controller": { id, port, pid, ... },
    "eng-21-implement": { ... }
  }
}

// After: controller is a top-level field
{
  controller?: { sessionId, port?, pid? },
  workers: {
    "eng-21-implement": { ... }
  }
}
```

**Pattern**: Managers and managed entities should live in separate state namespaces. This makes queries cleaner (no filtering needed) and prevents accidental operations on the manager.

### 3. Lifecycle Independence

```typescript
// Before: controller spawned via self-calling HTTP
const res = await fetch(`${baseUrl}/workers`, {
  method: "POST",
  body: JSON.stringify({ issueId: "controller", mode: "controller", ... })
});

// After: controller spawned directly
const port = portAllocator.allocate();
const sessionId = computeControllerSessionId(teamId);
controllerProcess = await serveManager.spawnServe({
  issueId: "controller",
  mode: "controller",
  port,
  sessionId,
  ...
});
```

**Pattern**: Managers should not use the same creation path as managed entities. Direct invocation makes the special status explicit and avoids polluting the worker API.

### 4. External vs Internal Mode

The refactor also added support for external controllers (user-managed sessions):

```typescript
if (config.controllerSessionId) {
  // External mode: store session ID, no process spawn, no health checks
  controllerState = { sessionId: config.controllerSessionId };
} else {
  // Internal mode: spawn process, health-check, kill on shutdown
  controllerProcess = await spawnServe({ ... });
}
```

**Pattern**: When a managed entity can be either internal (daemon-spawned) or external (user-provided), use a config flag to switch modes. External mode should skip lifecycle operations (spawn/kill/health-check) but still track the entity's existence.

## The State File Race Condition (Caught in Review)

### The Bug

Both controller state writes used a stale snapshot captured at startup:

```typescript
// Startup: capture snapshot
const adopted = await adoptExistingWorkers(...);

// HTTP server starts here — workers can be created via POST /workers

// Later: write controller state using stale snapshot
await writeStateFile(stateFilePath, {
  ...mapToState(adopted.workers, adopted.crashHistory), // ❌ Stale!
  controller: controllerState,
});
```

**Race window**: If a worker is created via `POST /workers` between server start and the controller state write, it gets clobbered. The server's next `persistState()` call would restore it (transient loss), but if the daemon crashes in this window, the worker is permanently lost.

### The Fix

Read current state before patching:

```typescript
const currentState = await readStateFile(stateFilePath);
await writeStateFile(stateFilePath, {
  ...currentState,
  controller: controllerState,
});
```

### The Pattern That Would Have Prevented It

**Never hold long-lived snapshots of mutable state.** When you need to patch state:

1. **Read-modify-write atomically** — read current state, apply patch, write back
2. **Or use a lock** — if multiple writers exist, coordinate via mutex/semaphore
3. **Or use a database** — let the DB handle concurrency

In this case, the daemon is single-threaded (no concurrent writes within the process), but the HTTP server introduces asynchrony. The fix is simple: always read before writing.

**Code smell**: If you see `const snapshot = await readState(); /* ... many lines later ... */ await writeState(snapshot)`, that's a race condition waiting to happen.

## Testing Patterns Used

### 1. Dependency Injection for Testability

The daemon accepts `DaemonDependencies` overrides:

```typescript
interface DaemonDependencies {
  adoptExistingWorkers: typeof adoptExistingWorkers;
  writeStateFile: typeof writeStateFile;
  readStateFile: typeof readStateFile;
  serveManager: {
    spawnServe: typeof spawnServe;
    initializeSession: typeof initializeSession;
    killWorker: typeof killWorker;
    healthCheck: typeof healthCheck;
  };
  portAllocator: PortAllocator;
  fetch: typeof fetch;
  setTimeout: typeof setTimeout;
  clearTimeout: typeof clearTimeout;
}
```

**Pattern**: Extract all I/O and time-dependent operations into an interface. Tests inject mocks; production uses real implementations. This enables:

- **Fast tests** — no actual process spawning, no network calls, no sleeps
- **Deterministic tests** — control time via mock `setTimeout`
- **Failure injection** — simulate spawn failures, health check failures, etc.

### 2. State File Testing

Tests for state file operations cover:

- **Backward compatibility** — legacy `controller-controller` entries are stripped on read
- **Optional fields** — missing `controller` field doesn't break parsing
- **Idempotency** — reading and writing the same state produces identical output

**Pattern**: When adding new fields to persisted state, always test:
1. Old state files (missing new fields) still parse
2. New state files (with new fields) round-trip correctly
3. Legacy data is migrated or stripped as appropriate

### 3. Config Validation Tests

```typescript
it("throws when controllerSessionId has invalid format", () => {
  expect(() => {
    loadConfig({ LEGION_CONTROLLER_SESSION_ID: "bad_value" });
  }).toThrow("LEGION_CONTROLLER_SESSION_ID must start with 'ses_'");
});
```

**Pattern**: Hard-fail on invalid config at startup, not at runtime. If a config value has format requirements, validate eagerly and throw with a clear error message.

## Review Findings Summary

The PR review caught **9 issues**, all P2 (important but not blocking):

1. **Port leaks** (3 instances) — allocated ports not released on failure paths
2. **State file race** (1 instance) — stale snapshot overwrite (discussed above)
3. **Silent failures** (2 instances) — errors caught but not logged or handled
4. **Legacy migration incomplete** (1 instance) — old controller entry stripped but not migrated
5. **Idempotency bug** (1 instance) — `POST /session` with existing ID treated as failure
6. **Scope creep** (1 instance) — skill permission enforcement bundled into lifecycle refactor

### Port Leak Pattern

**The bug**: Allocate port → operation fails → port never released

```typescript
const port = portAllocator.allocate();
const entry = await spawnServe({ port, ... }); // ❌ If this throws, port leaks
```

**The fix**: Wrap in try/catch, release on failure

```typescript
const port = portAllocator.allocate();
try {
  const entry = await spawnServe({ port, ... });
  // ... success path
} catch (error) {
  portAllocator.release(port);
  throw error;
}
```

**The pattern**: Whenever you allocate a resource (port, file handle, lock), immediately wrap the next operation in try/catch and release on failure. Or use RAII-style wrappers (e.g., `using` in TypeScript 5.2+).

### Silent Failure Pattern

**The bug**: Catch error, log nothing, continue as if success

```typescript
try {
  await initializeSession(port, sessionId);
} catch {
  // ❌ Silent failure — caller assumes success
}
```

**The fix**: At minimum, log the error

```typescript
try {
  await initializeSession(port, sessionId);
} catch (error) {
  console.error(`Failed to initialize session: ${error}`);
  // Decide: re-throw, return error, or continue with degraded state
}
```

**The pattern**: Empty catch blocks are almost always wrong. If you truly want to ignore an error, add a comment explaining why. Otherwise, log it.

## Architectural Lessons

### 1. Manager/Managed Separation Checklist

When you have a "manager" entity that coordinates other entities:

- [ ] **Separate types** — manager should not be in the managed entity's union
- [ ] **Separate state** — manager lives in its own state namespace, not in the managed collection
- [ ] **Separate API** — manager creation/deletion should not use the same endpoints as managed entities
- [ ] **Separate lifecycle** — manager spawn/shutdown logic should be explicit, not hidden in generic worker logic
- [ ] **Separate health checks** — manager health is checked independently (or not at all for external managers)

### 2. External vs Internal Entity Pattern

When an entity can be either daemon-managed or user-managed:

- **Config flag** — `ENTITY_SESSION_ID` env var (optional, validated format)
- **Mode detection** — if flag set → external mode; else → internal mode
- **External mode** — store session ID, skip spawn/kill/health-check
- **Internal mode** — spawn process, health-check, kill on shutdown
- **Conflict resolution** — on restart, if existing session ID differs from config:
  - If old process alive → error (conflict)
  - If old process dead → accept new session ID

### 3. State Patching Anti-Pattern

**Anti-pattern**: Capture state snapshot early, use it for writes later

```typescript
const snapshot = await readState();
// ... many operations, async boundaries ...
await writeState({ ...snapshot, newField: value }); // ❌ Race condition
```

**Pattern**: Read-modify-write atomically

```typescript
const current = await readState();
await writeState({ ...current, newField: value }); // ✅ No race
```

**When to use locks**: If multiple processes/threads write to the same state file, use file locking (e.g., `flock`) or a database.

## Future Improvements

1. **Port allocator RAII** — Wrap port allocation in a resource manager that auto-releases on scope exit
2. **State file transactions** — Add `patchStateFile(path, patcher)` that handles read-modify-write atomically
3. **Controller auto-restart** — Currently, dead internal controller → daemon continues headless. Could add auto-restart with backoff.
4. **External controller health** — For external mode, could optionally accept a health-check URL
5. **Structured logging** — Replace `console.log` with structured logger (JSON output, log levels, correlation IDs)

## Related Patterns

- **Separation of Concerns** — managers and managed entities have different responsibilities
- **Dependency Injection** — testability via interface-based mocking
- **Fail-Fast** — validate config at startup, not at runtime
- **Resource Acquisition Is Initialization (RAII)** — tie resource lifetime to scope
- **Read-Modify-Write** — atomic state updates to prevent races
- **Backward Compatibility** — graceful handling of legacy state formats
