# Fix: Daemon Worker State Persistence

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix the daemon's shutdown handler to preserve worker state in `workers.json`, enabling worker registry restoration after daemon restarts.

**Architecture:** The daemon already loads workers from `workers.json` on startup (`server.ts:loadState`) and re-creates sessions on the serve (`index.ts:109-118`). The only bug is the shutdown handler explicitly clearing workers from the state file (`index.ts:139`). Fix the shutdown handler and align test expectations with the corrected behavior.

**Tech Stack:** TypeScript, Bun runtime, Bun test runner

---

## Root Cause

In `packages/daemon/src/daemon/index.ts`, the `shutdown()` function (lines 138-143) writes `workers: {}` to the state file:

```typescript
await resolvedDeps.writeStateFile(config.stateFilePath, {
  workers: {},                    // ← BUG: clears all worker entries
  crashHistory: state.crashHistory,
  controller: undefined,
});
```

This means any daemon restart (graceful or otherwise) loses the worker registry. The rest of the pipeline already works correctly:
- `server.ts:loadState()` populates the in-memory `workers` Map from the state file
- `index.ts:109-118` re-creates sessions on the serve for all persisted workers
- Session IDs are deterministic (UUIDv5), so `createSession` is idempotent

## Behavioral Contract

- **Graceful shutdown (`legion stop`, SIGTERM, SIGINT):** Preserve workers and crash history in state file. Clear only controller state (daemon-owned). The serve is killed, but sessions can be restored on next startup via deterministic session IDs.
- **Crash (SIGKILL, OOM):** Signal handlers don't run. State file retains last persisted state (workers intact). On restart, serve may be alive (sessions exist) or dead (sessions re-created).
- **Startup restore:** Read persisted workers, re-create sessions idempotently. Workers appear in `GET /workers` immediately. `legion prompt` works.

## Metis Pre-Analysis

- Primary contradiction: startup tries to restore persisted workers, but shutdown explicitly wipes them
- When daemon adopts an already-running serve, `adapter.stop()` can be a no-op for the serve while the daemon still clears `workers.json` — sessions survive but registry is lost
- Existing tests codify the wipe behavior (`index.test.ts:550`) — must update
- Keep fix tight: startup/shutdown/persistence + targeted tests. No supervisor redesign.

---

### Task 1: Fix shutdown handler to preserve workers — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/index.ts:140`

**Step 1: Change the shutdown handler**

In the `shutdown()` function, change line 140 from `workers: {}` to `workers: state.workers`:

```typescript
// Before (line 138-143):
const state = await resolvedDeps.readStateFile(config.stateFilePath);
await resolvedDeps.writeStateFile(config.stateFilePath, {
  workers: {},
  crashHistory: state.crashHistory,
  controller: undefined,
});

// After:
const state = await resolvedDeps.readStateFile(config.stateFilePath);
await resolvedDeps.writeStateFile(config.stateFilePath, {
  workers: state.workers,
  crashHistory: state.crashHistory,
  controller: undefined,
});
```

**Step 2: Verify type checking**

Run: `bunx tsc --noEmit`
Expected: PASS

**Step 3: Describe and advance**

```bash
jj describe -m "fix: preserve worker state in state file on daemon shutdown"
jj new
```

---

### Task 2: Update shutdown test expectations — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/daemon/__tests__/index.test.ts`

**Step 1: Update the "registers signal handlers and shuts down cleanly" test**

Find the assertion at approximately line 550:

```typescript
// Before:
expect((finalState as PersistedWorkerState).workers).toEqual({});

// After:
expect((finalState as PersistedWorkerState).workers).toEqual({
  [baseEntry.id]: baseEntry,
  [secondEntry.id]: secondEntry,
});
```

This test creates a daemon with two workers (`baseEntry` and `secondEntry`), triggers SIGTERM, and verifies the written state. The fix changes the expectation from "workers cleared" to "workers preserved".

**Step 2: Run the updated test**

Run: `bun test packages/daemon/src/daemon/__tests__/index.test.ts`
Expected: ALL PASS

**Step 3: Describe and advance**

```bash
jj describe -m "test: update shutdown test to expect preserved workers"
jj new
```

---

### Task 3: Add restart persistence test — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/daemon/__tests__/index.test.ts`

**Step 1: Add a test for worker preservation through stop()**

Add this test inside the `describe("daemon entry", ...)` block, after the existing shutdown test:

```typescript
it("preserves workers in state file when stopped via handle.stop()", async () => {
  let savedState: PersistedWorkerState | null = null;

  const handle = await startDaemon(
    {
      stateFilePath: "/tmp/daemon-workers.json",
      teamId: TEAM_ID,
      controllerSessionId: "ses_test",
    },
    {
      readStateFile: async () => ({
        workers: {
          [baseEntry.id]: baseEntry,
          [secondEntry.id]: secondEntry,
        },
        crashHistory: {
          [secondEntry.id]: { crashCount: 1, lastCrashAt: "2026-02-02T02:00:00.000Z" },
        },
      }),
      writeStateFile: async (_path, state) => {
        savedState = state;
      },
      adapter: makeAdapter(),
      startServer: () => ({
        server: { port: 15555 } as ReturnType<typeof Bun.serve>,
        stop: () => {},
      }),
      setTimeout: silentSetTimeout,
      clearTimeout: noopClearTimeout,
      fetch: originalFetch,
    }
  );

  await handle.stop();

  expect(savedState).not.toBeNull();
  const state = savedState as PersistedWorkerState;
  // Workers preserved
  expect(state.workers[baseEntry.id]).toBeDefined();
  expect(state.workers[secondEntry.id]).toBeDefined();
  // Crash history preserved
  expect(state.crashHistory[secondEntry.id]).toEqual({
    crashCount: 1,
    lastCrashAt: "2026-02-02T02:00:00.000Z",
  });
  // Controller cleared (daemon-owned)
  expect(state.controller).toBeUndefined();
});
```

**Step 2: Run the test**

Run: `bun test packages/daemon/src/daemon/__tests__/index.test.ts`
Expected: ALL PASS

**Step 3: Describe and advance**

```bash
jj describe -m "test: add restart persistence test for worker state"
jj new
```

---

### Task 4: Run full quality checks — Depends on: Task 2, Task 3

**Step 1: Run linting**

Run: `bunx biome check src/`
Expected: PASS

**Step 2: Run type checking**

Run: `bunx tsc --noEmit`
Expected: PASS

**Step 3: Run all tests**

Run: `bun test`
Expected: ALL ~640 tests pass

---

## Testing Plan

### Setup
- `bun install` (if not already done)

### Health Check
- `bunx tsc --noEmit` — no type errors
- `bunx biome check src/` — no lint errors

### Verification Steps
1. **Worker state preserved on shutdown**
   - Action: `bun test packages/daemon/src/daemon/__tests__/index.test.ts -t "registers signal handlers and shuts down cleanly"`
   - Expected: PASS — workers preserved in state file (not cleared)
   - Tool: Bun test runner

2. **Restart persistence via handle.stop()**
   - Action: `bun test packages/daemon/src/daemon/__tests__/index.test.ts -t "preserves workers in state file when stopped"`
   - Expected: PASS — workers and crash history preserved, controller cleared
   - Tool: Bun test runner

3. **Existing restore behavior unchanged**
   - Action: `bun test packages/daemon/src/daemon/__tests__/index.test.ts -t "re-creates sessions for persisted workers on startup"`
   - Expected: PASS — sessions still re-created from persisted state
   - Tool: Bun test runner

4. **Server loads persisted workers**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts -t "loads persisted workers from state file"`
   - Expected: PASS — workers Map populated from state file
   - Tool: Bun test runner

5. **Full regression**
   - Action: `bun test`
   - Expected: ALL tests pass
   - Tool: Bun test runner

### Tools Needed
- Bun test runner for unit tests
- TypeScript compiler (`tsc`) for type checking
- Biome for lint/format checking
