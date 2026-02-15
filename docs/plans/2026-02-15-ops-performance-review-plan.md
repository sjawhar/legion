# Ops/Performance Review Mitigations Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ensure absolute paths are never emitted in headers by default, and provide a low-overhead workspace ID alternative when a header is required.

**Architecture:** Add a small helper for canonicalizing/validating workspace paths and deriving a fixed-length ID (node:crypto). Use it at daemon ingress for `POST /workers`; keep headers path-free and optionally expose `workspaceId` in response bodies or env for correlation.

**Tech Stack:** TypeScript, Bun, node:crypto, Bun test

---

### Task 1: Add workspace path validation + workspace ID helper

**Files:**
- Create: `packages/daemon/src/daemon/workspace-id.ts`
- Create: `packages/daemon/src/daemon/__tests__/workspace-id.test.ts`

**Step 1: Write the failing test**

```ts
import { describe, expect, it } from "bun:test";
import { resolveWorkspacePath, computeWorkspaceId } from "../workspace-id";

describe("workspace-id", () => {
  it("rejects control characters", () => {
    expect(() => resolveWorkspacePath("/tmp/legion\nwork")).toThrow();
  });

  it("rejects excessively long paths", () => {
    const longPath = "/tmp/" + "a".repeat(5000);
    expect(() => resolveWorkspacePath(longPath)).toThrow();
  });

  it("returns a stable fixed-length id", () => {
    const resolved = resolveWorkspacePath("/tmp/legion");
    const id = computeWorkspaceId(resolved);
    expect(id).toMatch(/^[a-f0-9]{16}$/);
  });
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/daemon/__tests__/workspace-id.test.ts`
Expected: FAIL with module not found or undefined exports.

**Step 3: Write minimal implementation**

```ts
import path from "node:path";
import { createHash } from "node:crypto";

const MAX_WORKSPACE_LENGTH = 4096;
const CONTROL_CHAR_PATTERN = /[\u0000-\u001F\u007F]/;

export function resolveWorkspacePath(raw: string): string {
  if (CONTROL_CHAR_PATTERN.test(raw)) {
    throw new Error("invalid_workspace: control_chars");
  }
  if (raw.length > MAX_WORKSPACE_LENGTH) {
    throw new Error("invalid_workspace: too_long");
  }
  const resolved = path.resolve(raw);
  if (!path.isAbsolute(resolved)) {
    throw new Error("invalid_workspace: not_absolute");
  }
  return resolved;
}

export function computeWorkspaceId(resolved: string): string {
  return createHash("sha256").update(resolved).digest("hex").slice(0, 16);
}
```

**Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/daemon/__tests__/workspace-id.test.ts`
Expected: PASS

**Step 5: Commit**

```bash
git add packages/daemon/src/daemon/workspace-id.ts packages/daemon/src/daemon/__tests__/workspace-id.test.ts
git commit -m "feat: add workspace id helper"
```

### Task 2: Apply validation at daemon ingress

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`

**Step 1: Write the failing test**

Add to server tests a case that POST `/workers` rejects control characters and returns `400`.

```ts
const response = await fetch(`${baseUrl}/workers`, {
  method: "POST",
  headers: { "content-type": "application/json" },
  body: JSON.stringify({ issueId: "ENG-1", mode: "implement", workspace: "/tmp/legion\nwork" }),
});
expect(response.status).toBe(400);
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: FAIL with status 200 or 409.

**Step 3: Write minimal implementation**

- Import `resolveWorkspacePath`.
- Normalize `workspace` right after type checks.
- Use the resolved value for `spawnServe(cwd=...)` and (if needed) derive a `workspaceId` for response bodies, not headers.

**Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: PASS

**Step 5: Commit**

```bash
git add packages/daemon/src/daemon/server.ts packages/daemon/src/daemon/__tests__/server.test.ts
git commit -m "feat: validate workspace paths"
```
