---
title: "Worker Directory Fix: Explicit Directory Passing to OpenCode"
date: 2026-02-15
category: daemon
tags: [opencode-sdk, jj-workspaces, worker-spawning, directory-resolution]
related-issues: [LEG-125]
---

# Worker Directory Fix: Explicit Directory Passing to OpenCode

## Problem

Workers were spawning in the wrong project directory. When OpenCode resolved the project root through jj workspace `.git` symlinks, it would follow the symlink to the main repository instead of staying in the workspace directory.

**Symptom:** Workers would operate on the main repo instead of their isolated jj workspace, causing:
- Changes appearing in the wrong workspace
- Confusion about which branch/workspace was active
- Potential conflicts between workers

**Root Cause:** The daemon spawned `opencode serve` processes with the correct working directory, but when creating OpenCode SDK sessions via `POST /session`, no explicit directory was passed. OpenCode's default project root resolution followed `.git` symlinks in jj workspaces, which point back to the main repo's `.jj/repo/store`.

## Solution

Pass the workspace directory explicitly to OpenCode via the `x-opencode-directory` header on session creation.

### Key Changes

1. **`x-opencode-directory` header** on `POST /session` — tells OpenCode which directory to use, bypassing its default resolution logic
2. **`createWorkerClient(port, workspace)` helper** — centralized SDK client creation that wraps `createOpencodeClient({baseUrl, directory})`
3. **`workspace` field in `WorkerEntry`** — stored in worker state so downstream call sites (status proxy, controller prompt) know the workspace
4. **SDK client for controller prompt** — replaced raw fetch `prompt_async` with `client.session.promptAsync()`
5. **Status proxy uses workspace** — `createWorkerClient(entry.port, entry.workspace)` instead of bare `createOpencodeClient`

### Implementation Pattern

```typescript
// Before: OpenCode resolves directory itself (follows symlinks)
const client = createOpencodeClient({ baseUrl: `http://127.0.0.1:${port}` });

// After: Explicit directory passed via SDK
const client = createOpencodeClient({ 
  baseUrl: `http://127.0.0.1:${port}`,
  directory: workspace 
});
```

The SDK automatically adds the `x-opencode-directory` header when `directory` is provided.

## Key Patterns

### 1. Centralized Client Creation

When spawning external processes that need directory context, create a helper that encapsulates both the connection details AND the directory:

```typescript
function createWorkerClient(port: number, workspace: string) {
  return createOpencodeClient({
    baseUrl: `http://127.0.0.1:${port}`,
    directory: workspace,
  });
}
```

**Why:** Prevents call sites from forgetting to pass the directory. Every worker client automatically gets the right context.

### 2. Store Context in State

When spawning workers, store the workspace path in the worker entry:

```typescript
interface WorkerEntry {
  port: number;
  workspace: string;  // ← Add this
  // ... other fields
}
```

**Why:** Downstream operations (status checks, prompts, cleanup) need to know which workspace the worker is operating in. Storing it once at spawn time prevents re-deriving it or passing it through multiple layers.

### 3. Explicit Directory Headers

When working with tools that resolve project roots automatically, prefer explicit directory passing over relying on working directory + symlink resolution:

```typescript
// Fragile: relies on cwd + symlink resolution
spawn("opencode", ["serve"], { cwd: workspace });

// Robust: explicit directory via API
client.session.create({ directory: workspace });
```

**Why:** Symlink resolution is environment-dependent and can follow unexpected paths. Explicit directory passing is deterministic.

### 4. Replace Raw Fetch with SDK Clients

When interacting with OpenCode's HTTP API, use the SDK instead of raw fetch:

```typescript
// Before: raw fetch
const response = await fetch(`http://127.0.0.1:${port}/session/${sessionId}/prompt_async`, {
  method: "POST",
  body: JSON.stringify({ prompt }),
});

// After: SDK client
const client = createWorkerClient(port, workspace);
await client.session.promptAsync(sessionId, prompt);
```

**Why:** SDK handles headers (including `x-opencode-directory`), error handling, and type safety. Raw fetch is error-prone.

## Gotchas

### 1. Jj Workspace `.git` Symlinks

Jj workspaces use `.git` symlinks that point to `.jj/repo/store` in the main repo. Tools that resolve project roots by walking up to `.git` will follow the symlink and end up in the main repo.

**Solution:** Always pass explicit directory context when spawning processes in jj workspaces. Don't rely on tools' default project root resolution.

### 2. OpenCode's Default Resolution

OpenCode's default project root resolution is designed for typical git repos. In jj workspaces, this resolution follows symlinks and breaks isolation.

**Solution:** Use the `x-opencode-directory` header (or SDK's `directory` option) to override default resolution.

### 3. Worker State Must Include Workspace

If you spawn workers with workspace context but don't store the workspace path in the worker entry, downstream operations won't know which workspace to use.

**Solution:** Add `workspace: string` to `WorkerEntry` at spawn time. Every operation that touches a worker should use `entry.workspace`.

### 4. Status Proxy Needs Workspace Too

The status proxy (`/workers/:issueId/status`) forwards requests to worker processes. If it doesn't pass the workspace directory, the worker's responses may reflect the wrong directory.

**Solution:** Status proxy must use `createWorkerClient(entry.port, entry.workspace)` to ensure the forwarded request includes directory context.

### 5. Controller Prompts Need Workspace

When the controller sends prompts to workers, it must use the SDK client with the workspace directory. Raw fetch won't include the `x-opencode-directory` header.

**Solution:** Replace raw fetch with `createWorkerClient(port, workspace).session.promptAsync()`.

## Related Patterns

- **Process Spawning with Context** — when spawning external processes, store all context needed for future interactions (port, PID, workspace, session ID)
- **SDK Over Raw HTTP** — prefer SDK clients over raw fetch for type safety and automatic header management
- **Explicit Over Implicit** — when tools have "smart" defaults that break in edge cases, prefer explicit configuration

## Testing

After implementing this fix, verify:

1. **Worker spawns in correct directory** — `jj status` in worker session shows the workspace, not main repo
2. **Changes appear in correct workspace** — commits made by worker appear in the workspace's log, not main
3. **Status proxy works** — `GET /workers/:issueId/status` returns correct directory info
4. **Controller prompts work** — controller can send prompts to workers and get responses

## References

- PR: https://github.com/sjawhar/legion/pull/42
- Issue: LEG-125
- OpenCode SDK: `@opencode-ai/sdk`
- Jj workspaces: https://martinvonz.github.io/jj/latest/working-copy/
