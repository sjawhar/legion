# Shared OpenCode Serve Refactor — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace per-worker `opencode serve` process spawning with a single shared serve instance, eliminating 3-4s startup overhead, zombie processes, port allocation bugs, and killWorker crash modes.

**Architecture:** One `opencode serve` process spawns on daemon startup and serves all worker + controller sessions. Worker dispatch creates a session on the shared serve via `POST /session` (instant). Worker removal just deletes tracking — sessions go idle naturally. Health monitoring checks the shared serve once; crash recovery restarts it and re-creates sessions using deterministic IDs.

**Tech Stack:** TypeScript on Bun, `@opencode-ai/sdk/v2`, citty CLI, `Bun.serve` HTTP

**Assumptions (from Metis pre-analysis — all resolved by issue description):**
1. `DELETE /workers/:id` removes tracking only — session goes idle naturally (issue: "Remove killWorker entirely")
2. Daemon restart adopts existing shared serve if healthy, spawns new one if not (mirrors existing adoptExistingWorkers pattern)
3. Shared serve crash during prompt: daemon restarts serve and re-creates sessions; lost prompt is a no-op (controller will re-dispatch when it sees no live worker)

**Known limitation:** Per-mode skill permission enforcement (`DENIED_SKILLS_BY_MODE` / `OPENCODE_PERMISSION`) is dropped. Worker workflow instructions enforce skill discipline. Follow-up issue for per-session permissions.

---

## Task 1: Refactor serve-manager.ts — Shared serve functions replace per-worker spawn — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/serve-manager.ts`
- Modify: `packages/daemon/src/daemon/__tests__/serve-manager.test.ts`

### Step 1: Rewrite serve-manager.ts

Replace the entire module. The new module exports:

```typescript
import { mkdirSync, openSync } from "node:fs";
import { join } from "node:path";
import { createOpencodeClient, type OpencodeClient } from "@opencode-ai/sdk/v2";

// ── Types ──────────────────────────────────────────────────────────────────────

export interface SharedServeState {
  port: number;
  pid: number;
  status: "starting" | "running" | "dead";
}

export interface SharedServeOptions {
  port: number;
  workspace: string;
  logDir?: string;
  env?: Record<string, string>;
}

export interface WorkerEntry {
  id: string;
  port: number;
  pid?: number;
  sessionId: string;
  workspace: string;
  startedAt: string;
  status: "starting" | "running" | "stopped" | "dead";
  crashCount: number;
  lastCrashAt: string | null;
}

// ── SDK Client ─────────────────────────────────────────────────────────────────

export function createWorkerClient(port: number, workspace: string): OpencodeClient {
  return createOpencodeClient({
    baseUrl: `http://127.0.0.1:${port}`,
    directory: workspace,
  });
}

// ── Shared Serve Lifecycle ─────────────────────────────────────────────────────

export async function spawnSharedServe(opts: SharedServeOptions): Promise<SharedServeState> {
  let stderr: "ignore" | number = "ignore";
  if (opts.logDir) {
    mkdirSync(opts.logDir, { recursive: true });
    const logFile = join(opts.logDir, "shared-serve.stderr.log");
    stderr = openSync(logFile, "a");
  }

  const { OPENCODE_PERMISSION: _, ...baseEnv } = process.env;
  const subprocess = Bun.spawn(["opencode", "serve", "--port", String(opts.port)], {
    cwd: opts.workspace,
    env: {
      ...baseEnv,
      ...opts.env,
      SUPERPOWERS_SKIP_BOOTSTRAP: "1",
    },
    stdio: ["ignore", "ignore", stderr],
  });

  const pid = subprocess.pid;
  if (pid === undefined) {
    throw new Error("Failed to spawn shared opencode serve process");
  }

  return { port: opts.port, pid, status: "starting" };
}

export async function waitForHealthy(
  port: number,
  maxRetries = 30,
  delayMs = 500,
): Promise<void> {
  for (let i = 0; i < maxRetries; i++) {
    const healthy = await healthCheck(port);
    if (healthy) {
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, delayMs));
  }
  throw new Error(
    `Shared serve on port ${port} did not become healthy after ${maxRetries} retries`,
  );
}

export async function createSession(
  port: number,
  sessionId: string,
  workspace: string,
): Promise<void> {
  const baseUrl = `http://127.0.0.1:${port}`;
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
  if (res.status === 409 || body.name === "DuplicateIDError") {
    return;
  }
  throw new Error(`Failed to create session ${sessionId}: ${JSON.stringify(body)}`);
}

export async function stopServe(
  port: number,
  pid: number,
  waitTimeoutMs = 5000,
  pollIntervalMs = 200,
  disposeTimeoutMs = 3000,
): Promise<void> {
  try {
    await fetch(`http://127.0.0.1:${port}/global/dispose`, {
      method: "POST",
      signal: AbortSignal.timeout(disposeTimeoutMs),
    });
  } catch {
    // Dispose is best-effort; proceed to poll + SIGKILL
  }

  const deadline = Date.now() + waitTimeoutMs;
  while (Date.now() < deadline) {
    try {
      process.kill(pid, 0);
    } catch {
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, pollIntervalMs));
  }

  try {
    process.kill(pid, "SIGKILL");
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ESRCH") {
      return;
    }
    throw error;
  }
}

export async function healthCheck(port: number, timeoutMs = 5000): Promise<boolean> {
  try {
    const response = await fetch(`http://127.0.0.1:${port}/global/health`, {
      signal: AbortSignal.timeout(timeoutMs),
    });
    if (!response.ok) {
      return false;
    }
    const data = (await response.json()) as { healthy?: boolean };
    return data.healthy === true;
  } catch {
    return false;
  }
}
```

**What changed:**
- **Removed:** `SpawnOptions` (replaced by `SharedServeOptions` — no issueId, mode, sessionId), `DENIED_SKILLS_BY_MODE`, `spawnServe()` (per-worker), `initializeSession()` (poll+create combined), `killWorker()` (process kill), `adoptExistingWorkers()` (state file adoption)
- **Added:** `SharedServeState`, `SharedServeOptions`, `spawnSharedServe()`, `waitForHealthy()`, `createSession()`, `stopServe()`
- **Kept:** `WorkerEntry` (pid now optional), `createWorkerClient()`, `healthCheck()`
- **`createSession` vs old `initializeSession`:** `createSession` assumes serve is already healthy (no polling). `waitForHealthy` handles the polling separately.
- **`stopServe` vs old `killWorker`:** Same dispose→poll→SIGKILL pattern, but takes port+pid instead of WorkerEntry.

### Step 2: Rewrite serve-manager.test.ts

Replace the test file with tests for the new functions:

```typescript
import { afterEach, describe, expect, it } from "bun:test";
import {
  createSession,
  createWorkerClient,
  healthCheck,
  spawnSharedServe,
  stopServe,
  waitForHealthy,
} from "../serve-manager";

describe("serve-manager", () => {
  const originalSpawn = Bun.spawn;
  const originalFetch = globalThis.fetch;
  const originalKill = process.kill;

  afterEach(() => {
    Bun.spawn = originalSpawn;
    globalThis.fetch = originalFetch;
    process.kill = originalKill;
  });

  describe("spawnSharedServe", () => {
    it("spawns opencode serve on the given port", async () => {
      const spawnArgs = { cmd: [] as string[], options: {} as any };
      Bun.spawn = ((cmd: string[], options: any) => {
        spawnArgs.cmd = cmd;
        spawnArgs.options = options;
        return { pid: 4242 } as any;
      }) as typeof Bun.spawn;

      const result = await spawnSharedServe({
        port: 13381,
        workspace: "/tmp/legion",
      });

      expect(result.port).toBe(13381);
      expect(result.pid).toBe(4242);
      expect(result.status).toBe("starting");
      expect(spawnArgs.cmd).toEqual(["opencode", "serve", "--port", "13381"]);
      expect(spawnArgs.options.cwd).toBe("/tmp/legion");
      expect(spawnArgs.options.env.SUPERPOWERS_SKIP_BOOTSTRAP).toBe("1");
    });

    it("strips OPENCODE_PERMISSION from environment", async () => {
      const spawnArgs = { options: {} as any };
      Bun.spawn = ((_: string[], options: any) => {
        spawnArgs.options = options;
        return { pid: 4243 } as any;
      }) as typeof Bun.spawn;

      const origPermission = process.env.OPENCODE_PERMISSION;
      process.env.OPENCODE_PERMISSION = '{"skill":{}}';
      try {
        await spawnSharedServe({ port: 13381, workspace: "/tmp" });
        expect(spawnArgs.options.env.OPENCODE_PERMISSION).toBeUndefined();
      } finally {
        if (origPermission !== undefined) {
          process.env.OPENCODE_PERMISSION = origPermission;
        } else {
          delete process.env.OPENCODE_PERMISSION;
        }
      }
    });
  });

  describe("waitForHealthy", () => {
    it("resolves when health check passes", async () => {
      let calls = 0;
      globalThis.fetch = (async () => {
        calls += 1;
        return {
          ok: true,
          json: async () => ({ healthy: true }),
        } as any;
      }) as unknown as typeof fetch;

      await waitForHealthy(13381, 5, 10);
      expect(calls).toBe(1);
    });

    it("retries until healthy", async () => {
      let calls = 0;
      globalThis.fetch = (async () => {
        calls += 1;
        if (calls < 3) {
          throw new Error("not ready");
        }
        return {
          ok: true,
          json: async () => ({ healthy: true }),
        } as any;
      }) as unknown as typeof fetch;

      await waitForHealthy(13381, 5, 10);
      expect(calls).toBe(3);
    });

    it("throws after max retries", async () => {
      globalThis.fetch = (async () => {
        throw new Error("not ready");
      }) as unknown as typeof fetch;

      await expect(waitForHealthy(13381, 3, 10)).rejects.toThrow(
        "did not become healthy after 3 retries",
      );
    });
  });

  describe("createSession", () => {
    it("creates session with correct headers", async () => {
      const captured: { url: string; headers: Record<string, string>; body: any } = {
        url: "",
        headers: {},
        body: null,
      };
      globalThis.fetch = (async (input: string, init?: RequestInit) => {
        captured.url = input;
        if (init?.headers && typeof init.headers === "object") {
          for (const [k, v] of Object.entries(init.headers)) {
            captured.headers[k.toLowerCase()] = String(v);
          }
        }
        captured.body = JSON.parse(init?.body as string);
        return new Response(JSON.stringify({ id: "ses_test" }), { status: 200 });
      }) as unknown as typeof fetch;

      await createSession(13381, "ses_test123", "/home/user/workspace");

      expect(captured.url).toBe("http://127.0.0.1:13381/session");
      expect(captured.headers["x-opencode-directory"]).toBe(
        encodeURIComponent("/home/user/workspace"),
      );
      expect(captured.body.id).toBe("ses_test123");
    });

    it("treats 409 DuplicateIDError as success", async () => {
      globalThis.fetch = (async () => {
        return new Response(
          JSON.stringify({ name: "DuplicateIDError" }),
          { status: 409 },
        );
      }) as unknown as typeof fetch;

      // Should not throw
      await createSession(13381, "ses_existing", "/tmp");
    });

    it("throws on other errors", async () => {
      globalThis.fetch = (async () => {
        return new Response(
          JSON.stringify({ error: "internal" }),
          { status: 500 },
        );
      }) as unknown as typeof fetch;

      await expect(createSession(13381, "ses_fail", "/tmp")).rejects.toThrow(
        "Failed to create session",
      );
    });
  });

  describe("stopServe", () => {
    it("disposes and returns when process exits", async () => {
      const calls = { disposeUrl: "", signals: [] as (number | undefined | NodeJS.Signals)[] };
      globalThis.fetch = (async (input: string) => {
        calls.disposeUrl = input;
        return new Response(null, { status: 200 });
      }) as unknown as typeof fetch;

      process.kill = ((_: number, signal?: NodeJS.Signals) => {
        calls.signals.push(signal);
        const err = new Error("ESRCH") as NodeJS.ErrnoException;
        err.code = "ESRCH";
        throw err;
      }) as typeof process.kill;

      await stopServe(13381, 4242, 50, 10, 100);

      expect(calls.disposeUrl).toBe("http://127.0.0.1:13381/global/dispose");
      expect(calls.signals).toEqual([0]);
    });

    it("sends SIGKILL when process lingers after dispose", async () => {
      const calls = { sigkill: false, signalChecks: 0 };
      globalThis.fetch = (async () =>
        new Response(null, { status: 200 })) as unknown as typeof fetch;

      process.kill = ((_: number, signal?: NodeJS.Signals) => {
        if (signal === "SIGKILL") {
          calls.sigkill = true;
          return true;
        }
        calls.signalChecks += 1;
        return true;
      }) as typeof process.kill;

      await stopServe(13381, 4242, 50, 10, 100);

      expect(calls.signalChecks).toBeGreaterThan(0);
      expect(calls.sigkill).toBe(true);
    });
  });

  describe("healthCheck", () => {
    it("returns true when healthy", async () => {
      globalThis.fetch = (async (url: string) => {
        expect(url).toBe("http://127.0.0.1:15000/global/health");
        return { ok: true, json: async () => ({ healthy: true }) } as any;
      }) as unknown as typeof fetch;

      expect(await healthCheck(15000, 500)).toBe(true);
    });

    it("returns false on error", async () => {
      globalThis.fetch = (async () => {
        throw new Error("boom");
      }) as unknown as typeof fetch;

      expect(await healthCheck(15001, 500)).toBe(false);
    });
  });

  describe("createWorkerClient", () => {
    it("creates SDK client with correct config", () => {
      const client = createWorkerClient(13381, "/home/user/workspace");
      expect(client).toBeDefined();
      expect(client.session).toBeDefined();
    });
  });
});
```

### Step 3: Run tests to verify

Run: `bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts`
Expected: All tests PASS

### Step 4: Commit

```
feat(daemon): replace per-worker serve functions with shared serve lifecycle

spawnSharedServe spawns one process, waitForHealthy polls readiness,
createSession creates a session on the shared serve, stopServe handles
graceful shutdown. Removes killWorker, initializeSession, and per-mode
skill permission enforcement (DENIED_SKILLS_BY_MODE).
```

---

## Task 2: Refactor server.ts — Update interface and handlers for shared serve — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`
- Modify: `packages/daemon/src/daemon/__tests__/session-id-contract.test.ts`
- Modify: `packages/daemon/src/__tests__/integration.test.ts`

### Step 1: Update ServeManagerInterface and ServerOptions

In `server.ts`, replace the interface and options:

```typescript
// OLD
export interface ServeManagerInterface {
  spawnServe(opts: SpawnOptions): Promise<WorkerEntry>;
  initializeSession(port: number, sessionId: string, workspace: string): Promise<void>;
  killWorker(entry: WorkerEntry): Promise<void>;
  healthCheck(port: number, timeoutMs?: number): Promise<boolean>;
}

export interface PortAllocatorInterface {
  allocate(): number;
  release(port: number): void;
  isAllocated?(port: number): boolean;
}

// NEW
export interface ServeManagerInterface {
  createSession(port: number, sessionId: string, workspace: string): Promise<void>;
  healthCheck(port: number, timeoutMs?: number): Promise<boolean>;
}
```

Remove `PortAllocatorInterface` entirely.

Update `ServerOptions`:
```typescript
// OLD fields to REMOVE: portAllocator, isPortFree
// NEW field to ADD: sharedServePort
export interface ServerOptions {
  port?: number;
  hostname?: string;
  teamId: string;
  legionDir: string;
  shortId: string;
  serveManager: ServeManagerInterface;
  sharedServePort: number;
  stateFilePath: string;
  logDir?: string;
  shutdownFn?: () => void | Promise<void>;
  getControllerState?: () => ControllerState | undefined;
}
```

Remove the import of `SpawnOptions` from serve-manager (no longer exists).

### Step 2: Rewrite POST /workers handler

Replace the spawn+initialize logic with session creation:

```typescript
if (method === "POST") {
  await stateLoaded;
  // ... validation unchanged (issueId, mode, workspace, env) ...
  // ... duplicate check unchanged ...
  // ... crash limit check unchanged ...

  // REMOVED: port allocation, isPortFree check, spawnServe, initializeSession
  // NEW: create session on shared serve
  const sessionId = computeSessionId(opts.teamId, issueId, mode as WorkerModeLiteral);

  try {
    await opts.serveManager.createSession(
      opts.sharedServePort,
      sessionId,
      workspace,
    );
  } catch (error) {
    return serverError(`Failed to create session: ${(error as Error).message}`);
  }

  let entry: WorkerEntry = {
    id: workerId,
    port: opts.sharedServePort,
    sessionId,
    workspace,
    startedAt: new Date().toISOString(),
    status: "running",
    crashCount: crashHistoryEntry?.crashCount ?? 0,
    lastCrashAt: crashHistoryEntry?.lastCrashAt ?? null,
  };

  workers.set(entry.id, entry);
  await persistState();

  return jsonResponse({
    id: entry.id,
    port: opts.sharedServePort,
    sessionId: entry.sessionId,
  });
}
```

### Step 3: Rewrite DELETE /workers/:id handler

Remove killWorker and port release — just remove tracking:

```typescript
if (method === "DELETE") {
  // Session goes idle naturally — no process to kill, no port to release
  crashHistory.set(id, {
    crashCount: entry.crashCount,
    lastCrashAt: entry.lastCrashAt,
  });
  workers.delete(id);
  await persistState();
  return jsonResponse({ status: "stopped" });
}
```

### Step 4: Update loadState

Replace per-worker health check adoption with state-only loading. Workers are re-validated by the daemon's shared-serve health, not individually:

```typescript
const loadState = async (): Promise<void> => {
  const state = await readStateFile(opts.stateFilePath);
  for (const [id, history] of Object.entries(state.crashHistory)) {
    crashHistory.set(id.toLowerCase(), history);
  }
  for (const [id, entry] of Object.entries(state.workers)) {
    const normalizedId = id.toLowerCase();
    workers.set(normalizedId, { ...entry, id: normalizedId });
  }
};
```

No per-worker healthCheck in loadState — the daemon handles shared serve health.

### Step 5: Rewrite server.test.ts

Key changes to the test file:
- Remove `TestPortAllocator` class
- Remove `spawnCalls` and `killCalls` tracking
- Add `createSessionCalls` tracking
- `startTestServer` takes `sharedServePort` instead of `portAllocator`
- `serveManager` only has `createSession` and `healthCheck`
- All assertions updated: no port allocation, no spawn calls, no kill calls
- Worker entries have `port: sharedServePort` and no `pid`
- Port-related tests (occupied port, port freeness) are removed

The test structure stays the same (health, list, create, duplicate, dead respawn, crash limit, delete, status proxy, shutdown) but the assertions change to match the session-based model.

**Critical test changes:**
- `"creates workers"` — assert `createSessionCalls.length === 1`, response has shared serve port, no spawn calls
- `"rejects duplicate"` — unchanged logic (still 409 on duplicate running workers)
- `"deletes workers"` — assert no kill calls, worker removed from list
- `"returns status from worker"` — still works via SDK client on shared port
- Port-related tests — **remove entirely** (no port allocation for workers)
- `"waits for state load"` — simplify (no health check gating, just state file read)

### Step 6: Refactor session-id-contract.test.ts

This test imports `SpawnOptions`, `PortAllocatorInterface`, and `ServeManagerInterface` with the old shape — all being removed. Update to the shared serve model:

- **Remove:** `TestPortAllocator` class, `SpawnOptions` import, `PortAllocatorInterface` import
- **Change `serveManager` mock** to only have `createSession` and `healthCheck`:

```typescript
const createSessionCalls: Array<{ port: number; sessionId: string; workspace: string }> = [];
const serveManager: ServeManagerInterface = {
  createSession: async (port, sessionId, workspace) => {
    createSessionCalls.push({ port, sessionId, workspace });
  },
  healthCheck: async () => true,
};
```

- **Change `startServer` call** — replace `portAllocator` with `sharedServePort`:

```typescript
const sharedServePort = 16500;
const { server, stop } = startServer({
  port: 0,
  hostname: "127.0.0.1",
  teamId,
  legionDir: tempDir,
  shortId: "test",
  serveManager,
  sharedServePort,
  stateFilePath,
});
```

- **Keep the core assertion** — `body.sessionId === computeSessionId(teamId, "ENG-42", "implement")` still validates the deterministic session ID contract.
- **Add assertion** — `expect(createSessionCalls[0].port).toBe(sharedServePort)` to verify session is created on the shared serve.

### Step 7: Refactor integration.test.ts

This test imports `SpawnOptions`, `WorkerEntry`, and `PortAllocator` — all changing or removed. Update the daemon HTTP lifecycle tests:

- **Remove:** `PortAllocator` import, `SpawnOptions` type usage, `spawnCalls`/`killCalls` tracking
- **Add:** `createSessionCalls` tracking

```typescript
interface TestServerContext {
  baseUrl: string;
  createSessionCalls: Array<{ port: number; sessionId: string; workspace: string }>;
}

async function withTestServer(run: (ctx: TestServerContext) => Promise<void>): Promise<void> {
  const tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-integration-"));
  const stateFilePath = path.join(tempDir, "workers.json");
  const createSessionCalls: Array<{ port: number; sessionId: string; workspace: string }> = [];
  const sharedServePort = randomPort();

  const serveManager = {
    createSession: async (port: number, sessionId: string, workspace: string): Promise<void> => {
      createSessionCalls.push({ port, sessionId, workspace });
    },
    healthCheck: async (): Promise<boolean> => true,
  };

  const { server, stop } = startServer({
    port: randomPort(),
    hostname: "127.0.0.1",
    teamId: TEAM_ID,
    legionDir: tempDir,
    shortId: "test",
    serveManager,
    sharedServePort,
    stateFilePath,
  });

  try {
    await run({ baseUrl: `http://127.0.0.1:${server.port}`, createSessionCalls });
  } finally {
    stop();
    await rm(tempDir, { recursive: true, force: true });
  }
}
```

- **Update CRUD test assertions:**
  - Replace `expect(spawnCalls.length).toBe(1)` → `expect(ctx.createSessionCalls.length).toBe(1)`
  - Replace `expect(spawnCalls[0].port).toBe(created.port)` → `expect(ctx.createSessionCalls[0].port).toBe(created.port)`
  - Remove `expect(allocator.isAllocated(...))` assertions (no port allocator)
  - Remove `expect(killCalls).toHaveLength(1)` (no kill on delete)
  - The state pipeline test (`buildCollectedState`) is **unchanged** — it doesn't touch daemon HTTP.

### Step 8: Run all affected tests

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts packages/daemon/src/daemon/__tests__/session-id-contract.test.ts packages/daemon/src/__tests__/integration.test.ts`
Expected: All tests PASS

### Step 9: Commit

```
feat(daemon): refactor server handlers for shared serve model

POST /workers creates session on shared serve instead of spawning
a process. DELETE /workers removes tracking only (no killWorker).
Removes PortAllocatorInterface and per-worker port allocation.
Updates session-id-contract and integration tests for new model.
```

---

## Task 3: Refactor index.ts — Shared serve startup, controller migration, health loop — Depends on: Task 1, Task 2

**Files:**
- Modify: `packages/daemon/src/daemon/index.ts`
- Modify: `packages/daemon/src/daemon/__tests__/index.test.ts`

### Step 1: Update DaemonDependencies and resolveDependencies

```typescript
// NEW DaemonDependencies — broader than ServeManagerInterface
interface DaemonServeManager extends ServeManagerInterface {
  spawnSharedServe(opts: SharedServeOptions): Promise<SharedServeState>;
  waitForHealthy(port: number, maxRetries?: number, delayMs?: number): Promise<void>;
  stopServe(
    port: number,
    pid: number,
    waitTimeoutMs?: number,
    pollIntervalMs?: number,
    disposeTimeoutMs?: number,
  ): Promise<void>;
}

interface DaemonDependencies {
  serveManager: DaemonServeManager;
  startServer: typeof startServer;
  readStateFile: typeof readStateFile;
  writeStateFile: typeof writeStateFile;
  fetch: typeof fetch;
  setTimeout: typeof setTimeout;
  clearTimeout: typeof clearTimeout;
}
```

Remove: `portAllocator`, `isPortFree`, `adoptExistingWorkers`, `setInterval`, `clearInterval`.

Update `resolveDependencies`:
```typescript
function resolveDependencies(
  config: DaemonConfig,
  overrides?: Partial<DaemonDependencies>,
): DaemonDependencies {
  return {
    serveManager: overrides?.serveManager ?? {
      spawnSharedServe,
      waitForHealthy,
      createSession,
      healthCheck,
      stopServe,
    },
    startServer: overrides?.startServer ?? startServer,
    readStateFile: overrides?.readStateFile ?? readStateFile,
    writeStateFile: overrides?.writeStateFile ?? writeStateFile,
    fetch: overrides?.fetch ?? globalThis.fetch,
    setTimeout: overrides?.setTimeout ?? setTimeout,
    clearTimeout: overrides?.clearTimeout ?? clearTimeout,
  };
}
```

### Step 2: Rewrite startDaemon — shared serve startup

Replace the adoption + port allocation logic:

```typescript
export async function startDaemon(
  overrides: Partial<DaemonConfig> = {},
  deps?: Partial<DaemonDependencies>,
): Promise<DaemonHandle> {
  const config = { ...loadConfig(), ...overrides };
  if (!config.teamId) {
    throw new Error("Missing teamId for daemon");
  }
  mkdirSync(config.logDir, { recursive: true });
  const resolvedDeps = resolveDependencies(config, deps);

  // ── Shared serve startup ────────────────────────────────────────────────
  const sharedServePort = config.baseWorkerPort;
  let sharedServePid = 0;

  const existingHealthy = await resolvedDeps.serveManager.healthCheck(sharedServePort);
  if (existingHealthy) {
    console.log(`Adopted existing shared serve on port ${sharedServePort}`);
  } else {
    const serve = await resolvedDeps.serveManager.spawnSharedServe({
      port: sharedServePort,
      workspace: config.legionDir ?? "",
      logDir: config.logDir,
    });
    sharedServePid = serve.pid;
    await resolvedDeps.serveManager.waitForHealthy(sharedServePort);
    console.log(`Shared serve started on port ${sharedServePort} pid=${sharedServePid}`);
  }

  // ── Re-create sessions for persisted workers ────────────────────────────
  const preState = await resolvedDeps.readStateFile(config.stateFilePath);
  let controllerState: ControllerState | undefined = preState.controller;

  for (const entry of Object.values(preState.workers)) {
    try {
      await resolvedDeps.serveManager.createSession(
        sharedServePort,
        entry.sessionId,
        entry.workspace,
      );
    } catch (error) {
      console.error(`Failed to re-create session for ${entry.id}: ${error}`);
    }
  }

  // ... (continue with HTTP server, controller, health tick, shutdown)
```

### Step 3: Rewrite controller startup

Replace per-process controller spawn with session creation on shared serve:

```typescript
  // ── Controller ──────────────────────────────────────────────────────────
  if (config.controllerSessionId) {
    // External controller mode
    if (controllerState && controllerState.sessionId !== config.controllerSessionId) {
      if (controllerState.port) {
        const oldAlive = await resolvedDeps.serveManager.healthCheck(controllerState.port);
        if (oldAlive) {
          throw new Error(
            `Another controller is running (session=${controllerState.sessionId})`,
          );
        }
      }
    }
    console.log(`External controller: session=${config.controllerSessionId}`);
    controllerState = { sessionId: config.controllerSessionId };
  } else {
    const sessionId = computeControllerSessionId(config.teamId!);
    try {
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
      console.log(`Controller started: session=${sessionId} port=${sharedServePort}`);
    } catch (error) {
      console.error(`Failed to start controller: ${error}`);
    }
  }
```

### Step 4: Rewrite health tick

Replace per-worker health checks with shared serve check:

```typescript
  const scheduleHealthTick = () => {
    healthTickTimeout = resolvedDeps.setTimeout(async () => {
      try {
        const serveHealthy = await resolvedDeps.serveManager.healthCheck(sharedServePort);

        if (!serveHealthy) {
          console.error("Shared serve is unhealthy, attempting restart...");

          // Restart shared serve
          try {
            const serve = await resolvedDeps.serveManager.spawnSharedServe({
              port: sharedServePort,
              workspace: config.legionDir ?? "",
              logDir: config.logDir,
            });
            sharedServePid = serve.pid;
            await resolvedDeps.serveManager.waitForHealthy(sharedServePort);
            console.log(`Shared serve restarted on port ${sharedServePort}`);

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
          } catch (error) {
            console.error(`Failed to restart shared serve: ${error}`);
          }
        }
      } finally {
        if (!shuttingDown) {
          scheduleHealthTick();
        }
      }
    }, config.checkIntervalMs);
  };
```

### Step 5: Rewrite shutdown

Replace per-worker kill with single shared serve stop:

```typescript
  const shutdown = async (exitAfter = false) => {
    if (shuttingDown) return;
    shuttingDown = true;

    if (healthTickTimeout) {
      resolvedDeps.clearTimeout(healthTickTimeout);
      healthTickTimeout = null;
    }

    // Stop the shared serve (handles all sessions)
    if (sharedServePid > 0) {
      await resolvedDeps.serveManager.stopServe(sharedServePort, sharedServePid);
    }

    controllerState = undefined;
    await resolvedDeps.writeStateFile(config.stateFilePath, {
      workers: {},
      crashHistory: (await resolvedDeps.readStateFile(config.stateFilePath)).crashHistory,
      controller: undefined,
    });
    stopServer();
    if (exitAfter) {
      process.exit(0);
    }
  };
```

Remove: `seedAllocator`, `fetchWorkers`, old `healthTick` function, `mapToState`, all PortAllocator references.

### Step 6: Rewrite index.test.ts

Key test changes:
- Remove `PortAllocator` import and usage
- `serveManager` mock provides: `spawnSharedServe`, `waitForHealthy`, `createSession`, `healthCheck`, `stopServe`
- Remove `adoptExistingWorkers` mock
- Remove `setInterval`/`clearInterval` from deps
- `"adopts existing workers"` → becomes `"re-creates sessions for persisted workers"` — verifies createSession called for each persisted worker
- `"health loop"` → verifies shared serve health check, restart on failure
- `"signal handlers"` → verifies stopServe called instead of per-worker killWorker

### Step 7: Run tests

Run: `bun test packages/daemon/src/daemon/__tests__/index.test.ts`
Expected: All tests PASS

### Step 8: Commit

```
feat(daemon): shared serve startup, controller as session, simplified health loop

Daemon spawns one opencode serve on startup. Controller creates a session
on the shared serve instead of spawning a separate process. Health tick
checks shared serve once and restarts on failure. Shutdown disposes the
shared serve process.
```

---

## Task 4: Cleanup ports.ts, unused code, state-file, and documentation — Depends on: Task 1, Task 2, Task 3

**Files:**
- Modify: `packages/daemon/src/daemon/ports.ts`
- Modify: `packages/daemon/src/daemon/__tests__/ports.test.ts`
- Modify: `packages/daemon/src/daemon/state-file.ts`
- Modify: `packages/daemon/src/daemon/AGENTS.md`

### Step 1: Simplify ports.ts

Remove `PortAllocator` class. Keep only `isPortFree` (may be useful for verifying shared serve port):

```typescript
import { createServer } from "node:net";

export function isPortFree(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const server = createServer();
    server.once("error", () => resolve(false));
    server.listen(port, "127.0.0.1", () => {
      server.close(() => resolve(true));
    });
  });
}
```

### Step 2: Update ports.test.ts

Remove all `PortAllocator` tests. Keep `isPortFree` tests.

### Step 3: Update state-file.ts

The `WorkerEntry` import path is unchanged (still from serve-manager.ts). The `pid` field is now optional in the type, but the state file normalization doesn't access `pid` directly, so no changes needed to the normalization logic.

Verify backward compatibility: old state files with `pid: 1234` still parse correctly (optional field can be present).

### Step 4: Update AGENTS.md for daemon module

Update `packages/daemon/src/daemon/AGENTS.md` to reflect the new architecture:

- **serve-manager.ts**: `spawnSharedServe()`, `waitForHealthy()`, `createSession()`, `stopServe()`, `healthCheck()`. No per-worker process management.
- **server.ts**: `POST /workers` creates session on shared serve. `DELETE /workers` removes tracking only. No port allocation.
- **index.ts**: Spawns shared serve on startup. Controller as session. Health tick checks shared serve. No `PortAllocator`, no `adoptExistingWorkers`.
- **ports.ts**: Only `isPortFree()` utility. `PortAllocator` removed.
- **config.ts**: `baseWorkerPort` now used as the shared serve port (single port, not a range).

### Step 5: Run all daemon tests

Run: `bun test packages/daemon/src/daemon/__tests__/`
Expected: All tests PASS

### Step 6: Commit

```
refactor(daemon): remove PortAllocator, update docs for shared serve model
```

---

## Task 5: Final verification — Depends on: Task 1, Task 2, Task 3, Task 4

**Files:** None (verification only)

### Step 1: Run full test suite

Run: `bun test`
Expected: All 172+ tests PASS

### Step 2: Run lint

Run: `bunx biome check packages/daemon/src/`
Expected: No errors

### Step 3: Run type check

Run: `bunx tsc --noEmit`
Expected: No errors

### Step 4: Verify no unused imports or dead code

Search for any remaining references to removed functions:
- `spawnServe` (the old per-worker version — `spawnSharedServe` is the replacement)
- `killWorker`
- `initializeSession`
- `adoptExistingWorkers`
- `DENIED_SKILLS_BY_MODE`
- `PortAllocator`
- `PortAllocatorInterface`

All should be zero references (except in git history).

### Step 5: Commit (if any remaining cleanup found)

```
chore: remove remaining dead code references
```
