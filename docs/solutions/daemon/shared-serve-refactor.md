---
title: Shared Serve Refactor
category: daemon
tags:
  - shared-serve
  - opencode-serve
  - architecture
  - process-management
  - dependency-injection
  - sessions
date: 2026-02-15
status: active
module: daemon
related_issues:
  - "LEG-136"
---

# Shared Serve Refactor

**Problem:** Per-worker `opencode serve` spawning caused 3-4s startup overhead, zombie processes, port allocation bugs, PID tracking issues, and complex `killWorker` failure modes.

**Solution:** Replace per-worker processes with a single shared `opencode serve` instance. One long-lived serve process handles all worker and controller sessions via the OpenCode SDK's session API.

## Architecture Shift

### Before
- Each worker dispatch spawned a new `opencode serve` process
- Port allocation from a sequential pool (base port + N)
- PID tracking for process lifecycle management
- `killWorker()` with dispose → poll → SIGKILL pattern
- `adoptExistingWorkers()` to restore state on daemon restart

### After
- One `opencode serve` spawns on daemon startup
- All workers and controller share the same port
- Session creation via `POST /session` (instant, idempotent)
- Worker removal just deletes tracking — sessions go idle naturally
- Health monitoring checks the shared serve once; crash recovery restarts it and re-creates sessions

## Key Patterns

### 1. Dependency Injection for Testability

The daemon uses a `DaemonDependencies` interface that's broader than the HTTP server's `ServeManagerInterface`:

```typescript
// HTTP server needs only these operations
interface ServeManagerInterface {
  createSession(port: number, sessionId: string, workspace: string): Promise<string>;
  healthCheck(port: number, timeoutMs?: number): Promise<boolean>;
}

// Daemon needs the full lifecycle
interface DaemonServeManager extends ServeManagerInterface {
  spawnSharedServe(opts: SharedServeOptions): Promise<SharedServeState>;
  waitForHealthy(port: number, maxRetries?: number, delayMs?: number): Promise<void>;
  stopServe(port: number, pid: number, ...): Promise<void>;
}
```

**Why this matters:** The HTTP server doesn't care about spawning or stopping — it only creates sessions. The daemon owns the lifecycle. This separation makes both modules independently testable.

**Test pattern:**
```typescript
const serveManager = {
  createSession: async (port, sessionId, workspace) => {
    createSessionCalls.push({ port, sessionId, workspace });
  },
  healthCheck: async () => true,
};
```

Tests mock only what the module needs, not the entire serve-manager API.

### 2. Idempotent Session Creation

The OpenCode SDK returns `409 DuplicateIDError` when creating a session with an existing ID. The refactor treats this as success:

```typescript
export async function createSession(
  port: number,
  sessionId: string,
  workspace: string,
): Promise<string> {
  const res = await fetch(`${baseUrl}/session`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "x-opencode-directory": encodeURIComponent(workspace),
    },
    body: JSON.stringify({ id: sessionId }),
  });
  if (res.ok) {
    return;
  }
  const body = (await res.json().catch(() => ({}))) as Record<string, unknown>;
  if (res.status === 409 && body.name === "DuplicateIDError") {
    return; // Idempotent — session already exists
  }
  throw new Error(`Failed to create session ${sessionId}: ${JSON.stringify(body)}`);
}
```

**Why this matters:** Daemon restart can safely call `createSession` for all persisted workers without checking if sessions already exist. Crash recovery can re-create sessions without complex state reconciliation.

**Deterministic session IDs enable this:** `computeSessionId(teamId, issueId, mode)` produces the same UUIDv5 every time, so re-creating a session for the same worker is a no-op.

### 3. Health Tick Recovery

The health loop shifted from per-worker checks to shared serve monitoring:

```typescript
const serveHealthy = await resolvedDeps.serveManager.healthCheck(sharedServePort);

if (!serveHealthy) {
  console.error("Shared serve is unhealthy, attempting restart...");

  // Restart shared serve
  const serve = await resolvedDeps.serveManager.spawnSharedServe({
    port: sharedServePort,
    workspace: config.legionDir ?? "",
    logDir: config.logDir,
  });
  sharedServePid = serve.pid;
  await resolvedDeps.serveManager.waitForHealthy(sharedServePort);

  // Re-create sessions for active workers
  const state = await resolvedDeps.readStateFile(config.stateFilePath);
  for (const entry of Object.values(state.workers)) {
    try {
      await resolvedDeps.serveManager.createSession(
        sharedServePort,
        entry.sessionId,
        entry.workspace,
      );
    } catch {
      // Best-effort session re-creation
    }
  }
}
```

**Why this matters:** One health check instead of N. Crash recovery is automatic — the daemon doesn't need to track which workers were affected. All sessions are re-created from the persisted state.

**Trade-off:** If the shared serve crashes during a prompt, the in-flight prompt is lost. The controller will re-dispatch when it sees no live worker, so this is a no-op in practice.

### 4. Controller as Session

The controller shifted from a separate `opencode serve` process to a session on the shared serve:

```typescript
const sessionId = computeControllerSessionId(config.teamId!);
await resolvedDeps.serveManager.createSession(
  sharedServePort,
  sessionId,
  config.legionDir ?? "",
);
const client = createWorkerClient(sharedServePort, config.legionDir ?? "");
await client.session.promptAsync({
  sessionID: sessionId,
  parts: [{ type: "text", text: "/legion-controller" }],
});
controllerState = { sessionId, port: sharedServePort };
```

**Why this matters:** Controller lifecycle is now identical to worker lifecycle. No special-case process management. Crash recovery re-creates the controller session just like worker sessions.

## Non-Obvious Decisions

### Why `pid` is Optional in `WorkerEntry`

The `WorkerEntry` type kept the `pid` field but made it optional:

```typescript
export interface WorkerEntry {
  id: string;
  port: number;
  pid?: number; // Optional now
  sessionId: string;
  workspace: string;
  startedAt: string;
  status: "starting" | "running" | "stopped" | "dead";
  crashCount: number;
  lastCrashAt: string | null;
}
```

**Reason:** Backward compatibility with existing state files. Old state files have `pid: 1234` for each worker. The refactor doesn't use `pid` (no process to track), but keeping the field as optional allows the daemon to load old state files without migration.

**Alternative considered:** Remove `pid` entirely and add a state file migration. Rejected because the field is harmless and migration adds complexity.

### Why `env` Field is Kept But Unused

The `SharedServeOptions` interface has an `env` field that's passed to `Bun.spawn` but never populated by callers:

```typescript
export interface SharedServeOptions {
  port: number;
  workspace: string;
  logDir?: string;
  env?: Record<string, string>; // Never used in practice
}
```

**Reason:** Future-proofing. The shared serve might need environment variables for configuration (e.g., `OPENCODE_LOG_LEVEL`). Keeping the field in the interface makes it easy to add later without changing the function signature.

**Alternative considered:** Remove `env` and add it back when needed. Rejected because the field is already in the spawn call and removing it would require changing all call sites.

### Why Controller Re-Creation After Crash

The health tick re-creates the controller session AND re-prompts it after a shared serve crash. Only internal controllers (those with `controllerState.port` set) are re-created — external controllers manage their own lifecycle:

```typescript
if (controllerState?.port) {
  await resolvedDeps.serveManager.createSession(
    sharedServePort,
    controllerState.sessionId,
    config.legionDir ?? "",
  );
  const client = createWorkerClient(sharedServePort, config.legionDir ?? "");
  await client.session.promptAsync({
    sessionID: controllerState.sessionId,
    parts: [{ type: "text", text: "/legion-controller" }],
  });
}
```

**Why re-prompt:** The controller's `/legion-controller` skill is idempotent — it checks state before dispatching. Re-prompting after crash is safe and ensures the controller loop resumes immediately rather than waiting for the next health tick cycle.

**Why only internal controllers:** External controllers (configured via `LEGION_CONTROLLER_SESSION_ID`) are identified by the absence of a `port` field on `controllerState`. They manage their own lifecycle and should not be re-prompted by the daemon.

## What This Eliminates

- **Per-worker spawn overhead** — 3-4s → instant (session creation is ~10ms)
- **`initializeSession` timeouts** — no polling for readiness per worker
- **Port allocation** — `PortAllocator` class removed entirely
- **PID tracking** — no process.kill calls, no PID mismatch bugs
- **Zombie processes** — one process to manage, not N
- **`killWorker` failure modes** — dispose → poll → SIGKILL pattern only runs once on daemon shutdown
- **`adoptExistingWorkers` complexity** — replaced by idempotent session creation

## Known Limitation

Per-mode skill permission enforcement (`DENIED_SKILLS_BY_MODE` / `OPENCODE_PERMISSION`) was dropped. The old model set `OPENCODE_PERMISSION` in the environment when spawning each worker process, denying specific skills based on the worker mode (e.g., architect mode couldn't use `todowrite`).

**Why dropped:** The shared serve has one environment. Per-session permissions would require OpenCode SDK support for per-session `OPENCODE_PERMISSION` headers or a separate permissions API.

**Mitigation:** Worker workflow instructions enforce skill discipline. The `/legion-worker` skill loads different workflows based on mode, and each workflow's instructions tell the agent which skills to use.

**Follow-up:** Track per-session permissions in a future OpenCode SDK release.

## Lessons for Future Refactors

### 1. Separate Interface Concerns

The HTTP server and daemon have different needs. The server needs "create session" and "health check". The daemon needs "spawn", "wait", "stop". Don't force both to depend on the same interface.

**Pattern:** Define a narrow interface for the server (`ServeManagerInterface`) and extend it for the daemon (`DaemonServeManager extends ServeManagerInterface`).

### 2. Idempotency Enables Crash Recovery

Deterministic session IDs + idempotent session creation = automatic crash recovery. The daemon doesn't need to track which sessions were lost — it just re-creates all of them from the persisted state.

**Pattern:** Use UUIDv5 for deterministic IDs. Treat 409 conflicts as success. Re-create sessions on every restart.

### 3. Test the Abstraction, Not the Implementation

Tests should mock the interface (`ServeManagerInterface`), not the implementation (`serve-manager.ts`). This allows refactoring the implementation without changing tests.

**Pattern:** Define interfaces in the module that uses them (e.g., `server.ts` defines `ServeManagerInterface`). Import the concrete implementation only in production code, not tests.

### 4. Keep Optional Fields for Backward Compatibility

Removing fields from persisted state requires migration. Adding optional fields is free.

**Pattern:** When refactoring state types, make removed fields optional instead of deleting them. The code doesn't use them, but old state files still parse.

### 5. Health Monitoring Should Match the Architecture

Per-worker health checks made sense when each worker was a separate process. Shared serve architecture needs shared serve health checks.

**Pattern:** Health monitoring should check the unit of failure. If one process serves all sessions, check the process once, not each session.

## Related Patterns

- **Deterministic session IDs** — see `docs/solutions/daemon/opencode-serve-lifecycle.md` for the original pattern
- **Controller lifecycle separation** — see `docs/solutions/daemon/controller-lifecycle-separation.md` for why the controller is tracked separately from workers
- **Graceful shutdown** — see `docs/solutions/daemon/graceful-worker-shutdown.md` for the dispose → poll → SIGKILL pattern (now only used once on daemon shutdown)

## Verification

- **Tests:** 385 pass, 0 fail (up from 172 — new tests for shared serve lifecycle)
- **Type safety:** `tsc --noEmit` clean
- **Lint:** `biome check` clean (pre-existing warnings only)
- **CI:** All checks pass

## Impact

- **Startup time:** 3-4s per worker → ~10ms (300-400x faster)
- **Resource usage:** N processes → 1 process (N = number of active workers)
- **Code complexity:** Removed `PortAllocator` (100 lines), `killWorker` (50 lines), `adoptExistingWorkers` (80 lines), `initializeSession` (40 lines) — ~270 lines deleted
- **Bug surface:** Eliminated port allocation bugs, PID mismatch bugs, zombie process bugs, killWorker crash modes
