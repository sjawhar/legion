---
title: "Worker Directory Resolution via x-opencode-directory Header"
date: 2026-02-15
category: daemon
tags: [opencode-sdk, jj-workspaces, session-management, worker-isolation]
related-issues: [LEG-125]
---

# Worker Directory Resolution

## Problem

Workers spawned with `opencode serve --port X` and `cwd: /path/to/workspace` created sessions in the wrong directory. OpenCode followed jj workspace `.git` symlinks to resolve the project root, landing on the main repo (`/home/sami/legion/ce`) instead of the workspace (`/home/sami/legion/leg-122`).

This broke worker isolation — multiple workers edited the same files in the main repo, causing conflicts.

## Root Cause

jj workspaces share a `.git` directory via symlinks:
```
/home/sami/legion/leg-122/.git → /home/sami/legion/default/.git/worktrees/leg-122
```

OpenCode's project resolution follows this chain to the main repo. Setting `cwd` on the subprocess is necessary but insufficient — OpenCode resolves the project root independently.

## Solution

Three-pronged fix:

1. **`x-opencode-directory` header on session creation** — `initializeSession()` sends this header on `POST /session`, telling OpenCode which directory to use. Only needed at creation time; prompts inherit the session's directory.

2. **`createWorkerClient(port, workspace)` helper** — Wraps `createOpencodeClient({baseUrl, directory})`. The SDK automatically sets `x-opencode-directory` on all requests. Used for status proxy and controller prompt.

3. **`workspace` field in `WorkerEntry`** — Stores the workspace path so downstream call sites can construct workspace-aware SDK clients without re-deriving the path.

## Key Patterns

### SDK vs Raw Fetch Split

The OpenCode SDK's `session.create()` doesn't accept a custom `id` parameter, but the daemon uses deterministic session IDs (`computeSessionId`). Solution: keep raw `fetch` for session creation (which needs `{id: sessionId}` in the body) but add the `x-opencode-directory` header manually. Use the SDK for everything else.

**When to use raw fetch:** When the SDK's typed API doesn't expose a field the server accepts.
**When to use SDK:** For all standard operations — the SDK handles headers automatically.

### Centralized Client Construction

Pattern: Create a helper that wraps `createOpencodeClient` with domain-specific parameters:
```typescript
export function createWorkerClient(port: number, workspace: string): OpencodeClient {
  return createOpencodeClient({ baseUrl: `http://127.0.0.1:${port}`, directory: workspace });
}
```
This ensures every call site gets the directory header without remembering to add it.

### WorkerEntry as Source of Truth

Adding `workspace` to `WorkerEntry` means any code with a worker reference can construct a properly-configured client. The workspace flows from `SpawnOptions` → `spawnServe` return → stored in `WorkerEntry` → available at status/prompt time.

## Gotchas

### Session directory is set at creation, not per-request

The `x-opencode-directory` header only matters on `POST /session`. Subsequent `prompt_async` and status calls operate within the session's already-established directory context. Don't add the header to prompt calls — it's redundant and misleading.

### SDK `session.create()` doesn't support custom IDs

The SDK's TypeScript types for `session.create()` accept `{directory?, parentID?, title?, permission?}` — no `id` field. The daemon's deterministic session IDs (`computeSessionId`) require passing `{id: sessionId}` in the body, which only works via raw fetch.

### jj working copy auto-commits are dangerous across sessions

When resuming work in a new session, the previous session's changes have already been auto-committed by jj. Running `jj edit @-` changes what `@` points to, and a subsequent `jj abandon @` will destroy the commit with all your changes. Always use `jj new` to create fresh commits; never use `jj edit` + `jj abandon` to restructure.

Recovery: `jj op log` shows all operations. `jj op restore <op-id>` can recover from any destructive operation.
