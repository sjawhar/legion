# Workspace Path Validation and Persistence Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add lightweight workspace path validation/normalization, persist optional workspace metadata, and align `x-opencode-directory` with the resolved path.

**Architecture:** Validate workspace at the daemon `/workers` boundary using `path.resolve` + basic safety checks, pass the resolved value to both `spawnServe` and `initializeSession`, and persist optional `workspaceRaw`/`workspaceResolved` for debugging without relying on them at runtime.

**Tech Stack:** TypeScript, Bun, OpenCode SDK (`@opencode-ai/sdk/v2`).

---

### Task 1: Add workspace validation + normalization in `/workers`

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts`
- Test: `packages/daemon/src/daemon/__tests__/server.test.ts`

**Step 1: Write the failing test**

Add a new test in `server.test.ts` (near existing `/workers` tests):

```typescript
it("rejects invalid workspace paths", async () => {
  await startTestServer();
  const invalid = [
    "relative/path",
    "/tmp/bad\r\nvalue",
    "/tmp/" + "a".repeat(5000),
    "/tmp/../etc",
  ];

  for (const workspace of invalid) {
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-42",
        mode: "implement",
        workspace,
      }),
    });
    expect(response.status).toBe(400);
    const body = (await response.json()) as { error: string };
    expect(body.error).toBe("invalid_workspace");
  }
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: FAIL — server currently accepts these workspaces.

**Step 3: Write minimal implementation**

In `server.ts`, add a helper to validate and normalize the workspace and return the resolved path:

```typescript
function resolveWorkspace(workspace: string): { ok: true; value: string } | { ok: false } {
  if (!path.isAbsolute(workspace)) {
    return { ok: false };
  }
  if (/\r|\n/.test(workspace)) {
    return { ok: false };
  }
  if (workspace.length > 4096) {
    return { ok: false };
  }
  const segments = workspace.split(path.sep);
  if (segments.includes("..")) {
    return { ok: false };
  }
  return { ok: true, value: path.resolve(workspace) };
}
```

Use it in the `POST /workers` handler:

```typescript
const resolvedWorkspace = resolveWorkspace(workspace);
if (!resolvedWorkspace.ok) {
  return badRequest("invalid_workspace");
}
```

Pass `resolvedWorkspace.value` to `spawnServe` and `initializeSession` later in the plan.

**Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: PASS.

**Step 5: Commit**

```bash
jj status
jj commit -m "feat: validate and normalize workspace paths"
```

---

### Task 2: Persist workspace metadata and use resolved path for spawn

**Files:**
- Modify: `packages/daemon/src/daemon/serve-manager.ts`
- Modify: `packages/daemon/src/daemon/server.ts`
- Test: `packages/daemon/src/daemon/__tests__/server.test.ts`

**Step 1: Write the failing test**

Update `server.test.ts` "creates workers" test to expect resolved metadata:

```typescript
expect(spawnCalls[0]).toMatchObject({
  workspace: "/tmp/work",
});

const listResponse = await requestJson("/workers");
const listBody = (await listResponse.json()) as WorkerEntry[];
expect(listBody[0].workspaceResolved).toBe("/tmp/work");
expect(listBody[0].workspaceRaw).toBe("/tmp/work");
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: FAIL — fields not present.

**Step 3: Write minimal implementation**

In `serve-manager.ts`, extend `WorkerEntry` with optional fields:

```typescript
  workspaceRaw?: string;
  workspaceResolved?: string;
```

In `server.ts` (POST /workers), store these values on the `WorkerEntry` returned from `spawnServe`:

```typescript
entry = {
  ...entry,
  workspaceRaw: workspace,
  workspaceResolved: resolvedWorkspace.value,
};
```

Ensure `spawnServe` receives `workspace: resolvedWorkspace.value`.

**Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: PASS.

**Step 5: Commit**

```bash
jj status
jj commit -m "feat: persist workspace metadata in worker state"
```

---

### Task 3: Align `x-opencode-directory` with resolved workspace

**Files:**
- Modify: `packages/daemon/src/daemon/serve-manager.ts`
- Modify: `packages/daemon/src/daemon/server.ts`
- Test: `packages/daemon/src/daemon/__tests__/serve-manager.test.ts`

**Step 1: Write the failing test**

Add a test in `serve-manager.test.ts` asserting the header matches the supplied workspace:

```typescript
it("sends x-opencode-directory header on session creation", async () => {
  let capturedHeaders: Record<string, string> = {};
  globalThis.fetch = async (_url, init) => {
    capturedHeaders = init?.headers as Record<string, string>;
    return new Response(null, { status: 200 });
  };

  await initializeSession(13381, "ses_test", "/workspace/leg-122", 1, 0);
  expect(capturedHeaders["x-opencode-directory"]).toBe("/workspace/leg-122");
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts`
Expected: FAIL — header not sent or signature missing.

**Step 3: Write minimal implementation**

Update `initializeSession` signature to accept `workspace: string` and add header:

```typescript
export async function initializeSession(
  port: number,
  sessionId: string,
  workspace: string,
  maxRetries = 30,
  delayMs = 500
): Promise<void> {
  // ...
  headers: {
    "content-type": "application/json",
    "x-opencode-directory": workspace,
  },
}
```

Update interface in `server.ts` and call site to pass `resolvedWorkspace.value`.

**Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts`
Expected: PASS.

**Step 5: Commit**

```bash
jj status
jj commit -m "fix: send resolved workspace on session creation"
```

---

## Verification

Run the following to ensure correctness:

```bash
bun test packages/daemon/src/daemon/__tests__/server.test.ts
bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts
bunx tsc --noEmit
```

Expected: All tests pass and typecheck is clean.
