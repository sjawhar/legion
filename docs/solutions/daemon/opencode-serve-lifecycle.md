---
title: "OpenCode Serve Process Lifecycle"
date: 2026-02-15
category: daemon
tags: [opencode, serve, process-management, SIGTERM, dispose]
related-issues: [LEG-130, LEG-131]
---

# OpenCode Serve Process Lifecycle

## Key Facts

1. **`opencode serve` never exits on its own.** It runs `await new Promise(() => {})` — a promise that never resolves. After a session prompt completes, the process stays alive and healthy.

2. **SIGTERM is ignored.** The never-resolving promise prevents clean shutdown via signals. `kill -15` has no effect. SIGKILL works but is violent.

3. **`POST /global/dispose` is the graceful shutdown mechanism.** This calls `Instance.disposeAll()` which cleans up all OpenCode instances. After dispose, the process should exit (nothing left to serve).

4. **The correct kill sequence is:** dispose → poll for exit → SIGKILL fallback.

5. **Session data persists in SQLite** (`~/.local/share/opencode/opencode.db`). Sessions survive process restarts. A new `opencode serve` process can serve sessions from a killed process.

6. **The SQLite database can grow very large** (6.7GB observed). Session creation takes 3-4 seconds against it. Plan for this in timeouts.

## Session ID Format

OpenCode requires session IDs matching: `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`

UUIDs (even with `ses_` prefix) are rejected. The daemon's `computeSessionId()` must convert UUIDv5 output to this format:
- First 6 bytes → 12 lowercase hex chars
- Remaining 10 bytes → 14 Base62 chars (big-endian encoding)

## Workspace Directory

`opencode serve` resolves its project root by following `.git` pointers. In jj workspaces, this resolves back to the main repo, not the workspace directory. The `x-opencode-directory` header on `POST /session` overrides this:

```typescript
const client = createWorkerClient(port, workspace);
await client.session.create({ id: sessionId });
```

`createWorkerClient` sets the `x-opencode-directory` header automatically via the SDK's `directory` config. Without it, all workers edit files in the main workspace regardless of where they were spawned.

## Port Allocation

Zombie processes from killed daemon runs hold ports. Port allocation is two-step: `PortAllocator.allocate()` picks the next port from an in-memory set, then `server.ts` optionally calls `isPortFree()` (which uses `net.createServer().listen()`) to verify the port is actually available. If occupied, the port is released and the request fails. Currently fails fast rather than skipping to the next port (LEG-131).
