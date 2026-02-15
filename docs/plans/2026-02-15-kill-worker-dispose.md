# LEG-130: Replace SIGTERM with Graceful Dispose in killWorker

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix zombie opencode serve processes by replacing SIGTERM (which is ignored) with `POST /global/dispose`, and add port-free verification as defense in depth.

**Architecture:** Two independent changes: (1) `killWorker()` sends `POST /global/dispose` to the worker's HTTP endpoint, polls for process exit, falls back to SIGKILL; (2) New `isPortFree()` utility function injected via `ServerOptions` and used in `server.ts` POST `/workers` handler to verify allocated port is actually free before spawning (single check, fail-fast — no retry loop).

**Tech Stack:** TypeScript, Bun, node:net for TCP bind check

---

### Task 1: Update killWorker tests and implementation — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/__tests__/serve-manager.test.ts`
- Modify: `packages/daemon/src/daemon/serve-manager.ts`

**Step 1: Replace the old killWorker test and add new tests**

In `packages/daemon/src/daemon/__tests__/serve-manager.test.ts`, replace the existing `it("kills a worker by pid", ...)` test (lines 157-168) with four new tests. The four tests cover:
1. Graceful dispose + process exits → success
2. Dispose succeeds but process lingers → SIGKILL fallback
3. Dispose fails (connection refused) → still SIGKILL
4. Process already dead → returns immediately

Replace the single existing test with:

```typescript
it("disposes worker via HTTP and confirms process exit", async () => {
  let disposeUrl = "";
  let disposeMethod = "";
  globalThis.fetch = (async (input: Request | string, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.url;
    const method = typeof input === "string" ? (init?.method ?? "GET") : input.method;
    if (url.includes("/global/dispose")) {
      disposeUrl = url;
      disposeMethod = method;
      return new Response(null, { status: 200 });
    }
    return new Response("not found", { status: 404 });
  }) as unknown as typeof fetch;

  process.kill = ((pid: number, signal?: string | number) => {
    if (signal === 0) {
      throw Object.assign(new Error("ESRCH"), { code: "ESRCH" });
    }
    return true;
  }) as typeof process.kill;

  await killWorker(baseEntry);
  expect(disposeUrl).toBe(`http://127.0.0.1:${baseEntry.port}/global/dispose`);
  expect(disposeMethod).toBe("POST");
});

it("falls back to SIGKILL when process does not exit after dispose", async () => {
  globalThis.fetch = (async () =>
    new Response(null, { status: 200 })) as unknown as typeof fetch;

  let sigkillPid = 0;
  process.kill = ((pid: number, signal?: string | number) => {
    if (signal === "SIGKILL") {
      sigkillPid = pid;
      return true;
    }
    if (signal === 0) {
      return true; // Still alive
    }
    return true;
  }) as typeof process.kill;

  await killWorker(baseEntry, 400, 100);
  expect(sigkillPid).toBe(baseEntry.pid);
});

it("handles dispose failure and still attempts SIGKILL", async () => {
  globalThis.fetch = (async () => {
    throw new Error("connection refused");
  }) as unknown as typeof fetch;

  let sigkillPid = 0;
  process.kill = ((pid: number, signal?: string | number) => {
    if (signal === "SIGKILL") {
      sigkillPid = pid;
      return true;
    }
    if (signal === 0) {
      return true; // Still alive
    }
    return true;
  }) as typeof process.kill;

  await killWorker(baseEntry, 400, 100);
  expect(sigkillPid).toBe(baseEntry.pid);
});

it("returns immediately when process is already dead", async () => {
  globalThis.fetch = (async () =>
    new Response(null, { status: 200 })) as unknown as typeof fetch;

  process.kill = ((_pid: number, _signal?: string | number) => {
    throw Object.assign(new Error("ESRCH"), { code: "ESRCH" });
  }) as typeof process.kill;

  // Should not throw, should return quickly
  await killWorker(baseEntry);
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts`
Expected: FAIL — `killWorker` doesn't accept timeout params and doesn't call fetch.

**Step 3: Implement new killWorker**

In `packages/daemon/src/daemon/serve-manager.ts`, replace the `killWorker` function (lines 118-128) with:

```typescript
export async function killWorker(
  entry: WorkerEntry,
  waitTimeoutMs = 5000,
  pollIntervalMs = 200,
  disposeTimeoutMs = 3000,
): Promise<void> {
  // Step 1: Try graceful dispose via HTTP
  try {
    await fetch(`http://127.0.0.1:${entry.port}/global/dispose`, {
      method: "POST",
      signal: AbortSignal.timeout(disposeTimeoutMs),
    });
  } catch {
    // Dispose failed (process already dead, network error, timeout)
    // Fall through to wait/SIGKILL
  }

  // Step 2: Wait for process to exit
  const deadline = Date.now() + waitTimeoutMs;
  while (Date.now() < deadline) {
    try {
      process.kill(entry.pid, 0);
    } catch {
      return; // Process exited
    }
    await new Promise((resolve) => setTimeout(resolve, pollIntervalMs));
  }

  // Step 3: SIGKILL as fallback
  try {
    process.kill(entry.pid, "SIGKILL");
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ESRCH") {
      return;
    }
    throw error;
  }
}
```

Note: Uses default parameters (`waitTimeoutMs = 5000, pollIntervalMs = 200, disposeTimeoutMs = 3000`) following the existing convention from `initializeSession(port, sessionId, workspace, maxRetries = 30, delayMs = 500)` on line 91-96. No new interface/type export needed.

**Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts`
Expected: ALL PASS

**Step 5: Commit**

```bash
jj describe -m "fix: replace SIGTERM with POST /global/dispose + SIGKILL fallback in killWorker

killWorker now:
1. Sends POST /global/dispose to gracefully shut down the opencode serve process
2. Polls process.kill(pid, 0) to confirm process exit (5s timeout, 200ms interval)
3. Falls back to SIGKILL if process doesn't exit

This fixes zombie opencode serve processes that ignore SIGTERM, which caused
cascading failures: port conflicts, stale sessions, incorrect worker tracking.

Resolves LEG-130 (partial — killWorker fix)"
jj new
```

### Task 2: Add port verification to spawn flow — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/ports.ts`
- Modify: `packages/daemon/src/daemon/__tests__/ports.test.ts`
- Modify: `packages/daemon/src/daemon/server.ts`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`
- Modify: `packages/daemon/src/daemon/index.ts`

**Step 1: Write failing test for `isPortFree` utility**

In `packages/daemon/src/daemon/__tests__/ports.test.ts`, update the import and add a new describe block. Change the existing import:

```typescript
import { describe, expect, it } from "bun:test";
import { PortAllocator } from "../ports";
```

to:

```typescript
import { describe, expect, it } from "bun:test";
import { createServer } from "node:net";
import { PortAllocator, isPortFree } from "../ports";
```

Then add after the existing `PortAllocator` describe block:

```typescript
describe("isPortFree", () => {
  it("returns true for an unoccupied port", async () => {
    const result = await isPortFree(19876);
    expect(result).toBe(true);
  });

  it("returns false for an occupied port", async () => {
    const blocker = createServer();
    await new Promise<void>((resolve) => {
      blocker.listen(19877, "127.0.0.1", resolve);
    });
    try {
      const result = await isPortFree(19877);
      expect(result).toBe(false);
    } finally {
      blocker.close();
    }
  });
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/ports.test.ts`
Expected: FAIL — `isPortFree` is not exported from ports.ts

**Step 3: Implement `isPortFree` in ports.ts**

In `packages/daemon/src/daemon/ports.ts`, add the import at the top and the function after the class:

```typescript
import { createServer } from "node:net";

// ... existing PortAllocator class unchanged ...

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

**Step 4: Run port tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/ports.test.ts`
Expected: ALL PASS

**Step 5: Add `isPortFree` to ServerOptions interface**

In `packages/daemon/src/daemon/server.ts`, add an optional `isPortFree` field to the `ServerOptions` interface (around line 26-38). Add after the `getControllerState` field:

```typescript
isPortFree?: (port: number) => Promise<boolean>;
```

So the full interface becomes:

```typescript
export interface ServerOptions {
  port?: number;
  hostname?: string;
  teamId: string;
  legionDir: string;
  shortId: string;
  serveManager: ServeManagerInterface;
  portAllocator: PortAllocatorInterface;
  stateFilePath: string;
  logDir?: string;
  shutdownFn?: () => void | Promise<void>;
  getControllerState?: () => ControllerState | undefined;
  isPortFree?: (port: number) => Promise<boolean>;
}
```

**Step 6: Write failing server tests for port verification**

In `packages/daemon/src/daemon/__tests__/server.test.ts`, update `startTestServer` to accept and pass through `isPortFree`.

Add `isPortFree?` to the options type (around line 55-59):

```typescript
async function startTestServer(options?: {
  state?: PersistedWorkerState;
  serveManagerOverrides?: Partial<ServeManagerInterface>;
  portAllocatorOverride?: TestPortAllocator;
  isPortFree?: (port: number) => Promise<boolean>;
}) {
```

Pass it through to `startServer` (around line 93-102), adding `isPortFree: options?.isPortFree,` to the options object:

```typescript
const { server, stop } = startServer({
  port: 0,
  hostname: "127.0.0.1",
  teamId,
  legionDir: tempDir,
  shortId: "test",
  serveManager,
  portAllocator,
  stateFilePath,
  isPortFree: options?.isPortFree,
});
```

Then add two tests inside the existing describe block:

```typescript
it("returns error when allocated port is occupied", async () => {
  await startTestServer({
    isPortFree: async () => false,
  });

  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-99",
      mode: "implement",
      workspace: "/tmp/test",
    }),
  });

  expect(response.status).toBe(500);
  const data = (await response.json()) as Record<string, unknown>;
  expect(data.error).toBe("allocated_port_occupied");
});

it("spawns worker normally when isPortFree confirms port is free", async () => {
  await startTestServer({
    isPortFree: async () => true,
  });

  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-101",
      mode: "implement",
      workspace: "/tmp/test",
    }),
  });

  expect(response.status).toBe(200);
  const data = (await response.json()) as Record<string, unknown>;
  expect(data.port).toBe(15500);
});
```

**Step 7: Run tests to verify the new tests fail**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: The "occupied port" test FAILS (server doesn't check yet). The "free port" test may PASS.

**Step 8: Add port verification to server.ts POST /workers handler**

In `packages/daemon/src/daemon/server.ts`, in the POST /workers handler, add a port-free check after `const port = opts.portAllocator.allocate();` (line 232). Add these lines immediately after that line:

```typescript
if (opts.isPortFree) {
  const free = await opts.isPortFree(port);
  if (!free) {
    opts.portAllocator.release(port);
    return serverError("allocated_port_occupied");
  }
}
```

This is a single check with fail-fast behavior — no retry loop. If the port is occupied, it's a signal something is wrong with cleanup, and the controller surfaces it. The check is optional (only runs when `isPortFree` is injected).

**Step 9: Wire up `isPortFree` in daemon startup**

In `packages/daemon/src/daemon/index.ts`, update the import from `"./ports"` (line 4):

Change:
```typescript
import { PortAllocator } from "./ports";
```
to:
```typescript
import { PortAllocator, isPortFree } from "./ports";
```

Then add `isPortFree,` to the `startServer` call (around line 224-241), alongside the other options. Add it after `logDir: config.logDir,`:

```typescript
isPortFree,
```

**Step 10: Run all tests, type check, and lint**

Run: `bun test`
Expected: ALL PASS (172+ tests)

Run: `bunx tsc --noEmit`
Expected: No errors

Run: `bunx biome check packages/daemon/src/`
Expected: No errors

**Step 11: Commit and squash**

```bash
jj describe -m "fix: add port-free verification before spawning workers

Adds isPortFree() TCP bind check in ports.ts, injected via ServerOptions.
Single check with fail-fast error if allocated port is occupied — surfaces
cleanup bugs rather than silently retrying.

Defense in depth for LEG-130"
```

Then squash both commits into one:

```bash
jj squash --from @- --into @
jj describe -m "fix: replace SIGTERM with POST /global/dispose + port verification

killWorker now:
1. Sends POST /global/dispose to gracefully shut down opencode serve
2. Polls process.kill(pid, 0) to confirm exit (5s timeout)
3. Falls back to SIGKILL if process doesn't exit

Port allocator defense in depth:
- isPortFree() TCP bind check before spawning workers
- Fail-fast error if allocated port is occupied by zombie

Resolves LEG-130"
```
