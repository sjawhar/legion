# LEG-125: Fix workers spawning in wrong project directory

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ensure Legion workers operate in their assigned jj workspace directory by explicitly passing the workspace to OpenCode via the SDK's `directory` option, and validating workspace paths at the boundary.

**Architecture:** Add a `createWorkerClient(port, workspace)` helper that wraps `createOpencodeClient({ baseUrl, directory })`. All communication with worker serve processes goes through this helper, which automatically sends `x-opencode-directory` on every request. Replace raw `fetch()` calls for session creation and prompt delivery with typed SDK methods. Add workspace path validation at the daemon boundary and track workspace per worker.

**Tech Stack:** TypeScript, Bun, OpenCode SDK (`@opencode-ai/sdk/v2`)

**Key design decisions:**
- SDK client with `directory` option handles header automatically — no manual header plumbing
- Eliminates mixed fetch/SDK pattern (daemon already imports SDK for status checks)
- Workspace path validation prevents bad paths from silently producing wrong directory
- `WorkerEntry.workspace` enables on-demand SDK client creation from persisted state

---

### Task 1: Add `workspace` to `WorkerEntry` and validate workspace paths — Independent

Track workspace per worker for state persistence, debugging, and SDK client creation.

**Files:**
- Modify: `packages/daemon/src/daemon/serve-manager.ts` (WorkerEntry interface + spawnServe return)
- Modify: `packages/daemon/src/daemon/server.ts` (POST /workers validation)
- Test: `packages/daemon/src/daemon/__tests__/serve-manager.test.ts`

**Step 1: Write the failing test**

In `packages/daemon/src/daemon/__tests__/serve-manager.test.ts`, update the `baseEntry` fixture to include `workspace`:

```typescript
const baseEntry = {
  id: "eng-42-implement",
  port: 15001,
  pid: 2222,
  sessionId: "ses_test",
  workspace: "/tmp/test-workspace",
  startedAt: "2026-01-01T00:00:00.000Z",
  status: "running" as const,
  crashCount: 0,
  lastCrashAt: null,
};
```

And in the test "spawns a serve process and returns worker entry", add after existing assertions:

```typescript
expect(entry.workspace).toBe("/tmp");
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts`
Expected: FAIL — `workspace` property doesn't exist on WorkerEntry return value

**Step 3: Add workspace to WorkerEntry and spawnServe return**

In `packages/daemon/src/daemon/serve-manager.ts`, add `workspace: string;` to the `WorkerEntry` interface (after `sessionId`), and add `workspace: opts.workspace,` to the return object in `spawnServe()` (after `sessionId: opts.sessionId,`).

**Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts`
Expected: PASS

**Step 5: Add workspace validation in server.ts**

In `packages/daemon/src/daemon/server.ts`, the `POST /workers` handler currently checks `typeof workspace === "string"`. Strengthen to require an absolute path. Find the workspace validation (~line 165-175) and add after the string type check:

```typescript
if (!path.isAbsolute(workspace)) {
  return badRequest("workspace must be an absolute path");
}
```

Add `import path from "node:path";` at the top if not already present.

**Step 6: Run type check**

Run: `bunx tsc --noEmit`
Expected: Type errors in files that construct `WorkerEntry` without `workspace` — fixed in subsequent tasks.

---

### Task 2: Add `createWorkerClient` helper and refactor `initializeSession()` — Independent

Add a helper that creates SDK clients for worker communication. Use it in `initializeSession`.

**Files:**
- Modify: `packages/daemon/src/daemon/serve-manager.ts` (add helper + refactor initializeSession)
- Modify: `packages/daemon/src/daemon/server.ts` (ServeManagerInterface)

**Step 1: Add `createWorkerClient` helper**

In `packages/daemon/src/daemon/serve-manager.ts`, add the import and helper at the top (after existing imports):

```typescript
import { createOpencodeClient } from "@opencode-ai/sdk/v2";

export function createWorkerClient(port: number, workspace: string) {
  return createOpencodeClient({
    baseUrl: `http://127.0.0.1:${port}`,
    directory: workspace,
  });
}
```

**Step 2: Refactor `initializeSession` to use the helper**

Change the function signature and replace the raw fetch with the SDK client:

```typescript
export async function initializeSession(
  port: number,
  sessionId: string,
  workspace: string,
  maxRetries = 30,
  delayMs = 500
): Promise<void> {
  const client = createWorkerClient(port, workspace);

  for (let i = 0; i < maxRetries; i++) {
    const healthy = await healthCheck(port);
    if (healthy) {
      const result = await client.session.create({
        body: { id: sessionId },
      });
      if (result.response?.status === 409) {
        return;
      }
      if (result.error) {
        throw new Error(
          `Failed to create session ${sessionId}: ${JSON.stringify(result.error)}`
        );
      }
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, delayMs));
  }
  throw new Error(
    `OpenCode serve on port ${port} did not become healthy after ${maxRetries} retries`
  );
}
```

**Step 3: Update `ServeManagerInterface`**

In `packages/daemon/src/daemon/server.ts`, change:
```typescript
  initializeSession(port: number, sessionId: string): Promise<void>;
```
To:
```typescript
  initializeSession(port: number, sessionId: string, workspace: string): Promise<void>;
```

---

### Task 3: Update call sites — `initializeSession` + prompt delivery — Depends on: Task 1, Task 2

Update all callers of `initializeSession` to pass workspace. Also refactor prompt delivery in `cmdDispatch` and `cmdPrompt` to use the SDK.

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts` (worker spawn + status check)
- Modify: `packages/daemon/src/daemon/index.ts` (controller spawn + prompt)
- Modify: `packages/daemon/src/cli/index.ts` (dispatch + prompt commands)

**Step 1: Update worker spawn in server.ts (~line 258-259)**

Change:
```typescript
await opts.serveManager.initializeSession(port, sessionId);
```
To:
```typescript
await opts.serveManager.initializeSession(port, sessionId, workspace);
```

**Step 2: Update status check SDK client in server.ts (~line 371)**

Import the helper and use it:

```typescript
import { createWorkerClient } from "./serve-manager";
```

Change:
```typescript
const client = createOpencodeClient({ baseUrl: `http://127.0.0.1:${entry.port}` });
```
To:
```typescript
const client = createWorkerClient(entry.port, entry.workspace);
```

**Step 3: Update controller spawn + prompt in daemon/index.ts (~line 288-308)**

Change initializeSession call:
```typescript
await resolvedDeps.serveManager.initializeSession(port, sessionId);
```
To:
```typescript
const controllerWorkspace = path.resolve(config.legionDir ?? process.cwd());
await resolvedDeps.serveManager.initializeSession(port, sessionId, controllerWorkspace);
```

Add `import path from "node:path";` if not present.

Replace the controller's raw fetch prompt_async with SDK via the helper:
```typescript
const controllerClient = createWorkerClient(port, controllerWorkspace);
await controllerClient.session.promptAsync({
  path: { id: sessionId },
  body: {
    parts: [{ type: "text", text: "/legion-controller" }],
  },
});
```

Add `import { createWorkerClient } from "./serve-manager";` if not present.

**Step 4: Add workspace to WorkerStatusInfo in cli/index.ts**

```typescript
interface WorkerStatusInfo extends WorkerInfo {
  sessionId: string;
  status: string;
  workspace: string;
}
```

**Step 5: Refactor cmdDispatch prompt delivery (~line 333)**

Replace raw fetch with SDK via the helper:
```typescript
import { createWorkerClient } from "../daemon/serve-manager";
```

```typescript
const workerClient = createWorkerClient(workerPort, workspacePath);
await workerClient.session.promptAsync({
  path: { id: sessionId },
  body: {
    parts: [{ type: "text", text: initialPrompt }],
  },
});
```

**Step 6: Refactor cmdPrompt prompt delivery (~line 405)**

Replace raw fetch with SDK via the helper (import already added in Step 5):
```typescript
const workerClient = createWorkerClient(worker.port, worker.workspace);
const promptResult = await workerClient.session.promptAsync({
  path: { id: worker.sessionId },
  body: {
    parts: [{ type: "text", text: prompt }],
  },
});
if (promptResult.error) {
  throw new CliError(`Worker rejected prompt: ${worker.id}`);
}
```

**Step 7: Run type check**

Run: `bunx tsc --noEmit`
Expected: Remaining errors only in test files.

---

### Task 4: Update tests — Depends on: Task 1, Task 2, Task 3

**Files:**
- Modify: `packages/daemon/src/daemon/__tests__/serve-manager.test.ts`
- Modify: `packages/daemon/src/cli/__tests__/index.test.ts`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`
- Modify: `packages/daemon/src/daemon/__tests__/index.test.ts`

**Step 1: Add test for SDK-based initializeSession**

In `packages/daemon/src/daemon/__tests__/serve-manager.test.ts`, add a test that verifies initializeSession uses the SDK with the workspace:

```typescript
it("creates session via SDK with workspace directory", async () => {
  let callCount = 0;
  let capturedHeaders: Record<string, string> = {};
  globalThis.fetch = (async (input: Request | string) => {
    callCount++;
    const url = typeof input === "string" ? input : input.url;
    if (url.includes("/global/health")) {
      return { ok: true, json: async () => ({ healthy: true }) } as any;
    }
    // Capture headers from SDK request
    if (typeof input !== "string" && input.headers) {
      const h = input.headers as Headers;
      capturedHeaders["x-opencode-directory"] = h.get("x-opencode-directory") ?? "";
    }
    return {
      ok: true,
      status: 200,
      json: async () => ({ id: "ses_test" }),
      clone: () => ({
        ok: true,
        status: 200,
        json: async () => ({ id: "ses_test" }),
      }),
    } as any;
  }) as unknown as typeof fetch;

  await initializeSession(15000, "ses_test", "/workspace/leg-122", 1, 0);

  expect(capturedHeaders["x-opencode-directory"]).toBe("/workspace/leg-122");
});
```

Note: The SDK may use `Request` objects instead of string URLs. The implementer should adjust the mock based on what the SDK actually sends — run the test and inspect the `input` parameter type.

**Step 2: Fix all WorkerEntry constructions in tests**

Use `bunx tsc --noEmit` to find all remaining type errors. Add `workspace: "/tmp/test-workspace"` (or appropriate path) to every `WorkerEntry` object literal in test files.

Key files:
- `packages/daemon/src/daemon/__tests__/serve-manager.test.ts` (baseEntry — done in Task 1)
- `packages/daemon/src/cli/__tests__/index.test.ts` (mock workers + mock `GET /workers` responses)
- `packages/daemon/src/daemon/__tests__/server.test.ts` (mock workers)
- `packages/daemon/src/daemon/__tests__/index.test.ts` (mock workers)

**Step 3: Update mock `initializeSession` in test files**

Update mocks of `ServeManagerInterface` to accept the new `workspace` parameter in `initializeSession`.

**Step 4: Update CLI test mocks for SDK prompt delivery**

In `packages/daemon/src/cli/__tests__/index.test.ts`, tests that mock `prompt_async` fetch calls need updating to handle SDK-style requests (the SDK sends `Request` objects, not raw URL strings). The implementer should check how existing tests mock fetch and adapt accordingly.

**Step 5: Run all tests + checks**

Run: `bun test && bunx tsc --noEmit && bunx biome check packages/daemon/src/`
Expected: All pass, no errors.

---

### Task 5: Verify and commit — Depends on: Task 4

**Step 1: Run full test suite**

Run: `bun test`
Expected: All 172+ tests pass.

**Step 2: Commit**

Commit message: `fix: use OpenCode SDK with directory option to ensure workers operate in correct workspace`

Files to include: all modified source and test files in `packages/daemon/src/`

---

## What This Changes

| Change | Why |
|--------|-----|
| `WorkerEntry.workspace` field | State tracking + SDK client creation from persisted state |
| `initializeSession()` uses SDK | Typed, automatic directory header, consistent with status checks |
| CLI prompt delivery uses SDK | Eliminates raw fetch, type-safe, directory handled automatically |
| Controller prompt uses SDK | Same consistency benefit |
| Status check SDK gets `directory` | Already used SDK; now passes directory too |
| Workspace must be absolute path | Prevents bad path computation from silently producing wrong directory |
| `config.legionDir` resolved via `path.resolve()` | Avoids empty string footgun |

## What This Does NOT Change

- **No OpenCode changes** — uses existing SDK + `x-opencode-directory` header infrastructure
- **No jj workspace changes** — workspaces stay as-is
- **No new dependencies** — SDK already imported in daemon
