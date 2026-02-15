# LEG-129: Address Deferred PR Review Comments — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Address deferred review items from PRs #41-43: add workspace path validation and document SDK client caching decision.

**Architecture:** Two small edits in the daemon's HTTP server, one new test. No new abstractions or refactors.

**Tech Stack:** TypeScript, Bun, `node:path`

---

### Task 1: Add workspace validation + document caching decision — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts` (add `isAbsolute` import, add validation in POST /workers handler, add comment near status proxy)
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts` (add rejection test)

**Step 1: Add test for relative workspace path rejection**

In `packages/daemon/src/daemon/__tests__/server.test.ts`, add a new test after the existing "rejects invalid worker creation payloads" test:

```typescript
it("rejects relative workspace paths", async () => {
  await startTestServer();
  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-50",
      mode: "implement",
      workspace: "relative/path",
    }),
  });
  expect(response.status).toBe(400);
  const body = (await response.json()) as { error: string };
  expect(body.error).toBe("workspace must be an absolute path");
});
```

**Step 2: Add `isAbsolute` import and validation**

In `packages/daemon/src/daemon/server.ts`:

1. Add import at top of file:
```typescript
import { isAbsolute } from "node:path";
```

2. In the POST /workers handler, after the `typeof` checks that return `badRequest("missing_fields")` and before the `validModes` check, add:
```typescript
if (!isAbsolute(workspace)) {
  return badRequest("workspace must be an absolute path");
}
```

**Step 3: Add caching decision comment**

In `packages/daemon/src/daemon/server.ts`, in the `/workers/:id/status` handler, directly before the `createWorkerClient(entry.port, entry.workspace)` call, add:

```typescript
// Client is lightweight (fetch wrapper); no caching needed at current
// polling frequency. Revisit if status endpoint becomes a hot path.
```

**Step 4: Run tests and verify**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: ALL PASS including the new "rejects relative workspace paths" test

**Step 5: Commit**

```
jj commit -m "fix(daemon): validate workspace is absolute path, document caching decision

Addresses deferred review comments from PR #42:
- P2: Reject relative/empty workspace paths in POST /workers
- P3: Document why SDK client caching is premature for status proxy"
```

---

### Task 2: Final verification — Depends on: Task 1

**Step 1: Run full quality checks**

Run: `bunx biome check packages/daemon/src/ && bunx tsc --noEmit && bun test`
Expected: All pass (lint clean, no type errors, 173 tests pass)

---

## Notes

- **initializeSession SDK migration** (original item 2): Already completed in a prior PR. `serve-manager.ts` uses `createWorkerClient(port, workspace).session.create({ id: sessionId })`. No work needed.
- **"Why raw fetch" comment** (original item 2b): Moot since raw fetch was already removed.
- **healthCheck raw fetch**: Stays as-is — SDK doesn't expose `/global/health`.
- **`isAbsolute("")`** returns `false`, so empty workspace strings are also rejected.
