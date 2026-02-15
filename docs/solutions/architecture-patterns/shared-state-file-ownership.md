---
title: Shared state file with multiple writers — callback consolidation pattern
created: 2026-02-15
category: architecture-patterns
tags:
  - state-file
  - race-condition
  - callback-pattern
  - persistence
  - daemon
  - lifecycle-separation
module: daemon
symptoms:
  - state file fields silently dropped on write
  - conflict detection / adoption code is dead (never finds existing state)
  - entity state vanishes after unrelated operations
  - read-after-write returns stale data because intervening write clobbered it
related_issues:
  - LEG-122
---

# Shared State File with Multiple Writers

## Problem

When two modules write the same state file but each only knows about its own fields, writes from one module silently drop fields managed by the other.

### How This Manifested (LEG-122)

The daemon has a single state file (`workers.json`) written by two modules:

- **`server.ts`** — manages workers (spawn, update, delete). Its `persistState()` writes `{ workers, crashHistory }` on every worker operation.
- **`index.ts`** — manages the controller lifecycle. Writes `{ ..., controller: { sessionId, port, pid } }` at startup and on controller death.

After separating the controller from the workers map, `server.ts` no longer included `controller` in its writes. Every worker operation silently overwrote the file without the controller field — destroying conflict detection and adoption data.

A second instance of the same bug: `startDaemon()` read the state file, adopted workers, then wrote state (dropping controller), then read state again expecting to find the controller. The intervening write had already clobbered it.

## Root Cause

**Ownership split without write consolidation.** When the controller was IN the workers map, `server.ts` naturally persisted it. Moving it to a separate field created a new writer (`index.ts`) without updating the existing writer (`server.ts`) to preserve the new field.

## Solution: Callback Consolidation

Give the primary writer a callback to collect state it doesn't own:

```typescript
// server.ts — ServerOptions
getControllerState?: () => ControllerState | undefined;

// persistState() includes it
const persistState = async () => {
  const state = { workers: {}, crashHistory: {} };
  // ... populate workers and crashHistory ...
  state.controller = opts.getControllerState?.();
  await writeStateFile(path, state);
};
```

The owner module (`index.ts`) provides the callback:

```typescript
let controllerState: ControllerState | undefined = preState.controller;

startServer({
  // ...
  getControllerState: () => controllerState,
});

// Update controllerState whenever controller lifecycle changes
controllerState = { sessionId, port, pid };  // on spawn
controllerState = undefined;                  // on death/shutdown
```

### Why Not Read-Modify-Write?

Reading the file before every write would technically preserve unknown fields, but:

1. **Race condition** — two near-simultaneous writes can clobber each other
2. **Performance** — extra disk read on every worker operation
3. **Implicit coupling** — you depend on file format you don't control

The callback makes the dependency explicit and in-memory.

## Prevention Pattern

When splitting a persisted entity out of an existing collection:

1. **Audit all writers** — grep for `writeStateFile` (or equivalent). Every writer must handle the new field.
2. **Read-before-first-write** — if startup logic reads state, writes state, then reads again, the intervening write can clobber data. Read once and carry the data through.
3. **Single write path preferred** — if possible, consolidate all writes through one function that assembles the complete state. If that's impractical, use a callback to let the primary writer collect foreign state.
4. **Test the round-trip** — write a test that: sets controller state → triggers a worker operation → reads state file → asserts controller is still present.

## Related

- The `DaemonDependencies` DI pattern made this testable — mock `writeStateFile` captures what was actually written, making it easy to assert field preservation.
