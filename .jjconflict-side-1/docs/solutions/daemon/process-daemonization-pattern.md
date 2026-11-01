---
title: "Process Daemonization via Re-exec Pattern"
category: daemon
tags:
  - cli
  - daemonization
  - process-management
  - bun
date: 2026-04-16
status: active
module: daemon
related_issues:
  - "562"
symptoms:
  - "legion start blocks the terminal"
  - "how to run daemon in background"
  - "detached child process in Bun"
  - "file descriptor management for log redirection"
---

# Process Daemonization via Re-exec Pattern

## Context

`legion start` needed to run as a background daemon instead of blocking the terminal.
The implementation uses a re-exec pattern rather than a daemon library or fork.

## Pattern: Re-exec with --foreground Flag

Instead of forking or using a daemon library, the parent process re-execs itself with
`--foreground` as a detached child:

```typescript
const childArgs = buildDaemonArgs(team, opts);
const child = spawn(process.execPath, [process.argv[1], "start", ...childArgs], {
  detached: true,
  stdio: ["ignore", logFile, logFile],
  cwd: process.cwd(),
  env: process.env,
});
child.unref();
```

**Why re-exec instead of fork:** Config resolution (env vars, config files, CLI args) runs
identically in both parent and child. No need to serialize/deserialize resolved config across
a process boundary. The `buildDaemonArgs` function reconstructs the original CLI flags so the
child resolves the same config.

## Gotcha: File Descriptor Ordering

The log file FD must be opened before `spawn()` and closed **after** `child.unref()`:

```typescript
const logFile = fs.openSync(logPath, "a");       // 1. Open FD
const child = spawn(process.execPath, [...], {    // 2. Spawn with FD
  stdio: ["ignore", logFile, logFile],
});
child.unref();                                     // 3. Detach
fs.closeSync(logFile);                             // 4. Close parent's FD copy
```

Closing the FD before `unref()` would break the child's stdio. The child inherits the FD
at spawn time, so the parent can safely close its copy after detaching.

## Gotcha: Registry-Based Readiness Polling

`waitForDaemonStart` polls the legions registry file — not the HTTP health endpoint —
because the registry write happens **after** the HTTP server is fully initialized:

```typescript
async function waitForDaemonStart(legionId, legionsFile, expectedPid, timeoutMs = 10_000) {
  while (Date.now() - start < timeoutMs) {
    const entry = await findLegionByProjectId(legionsFile, legionId);
    if (entry && entry.pid === expectedPid) return true;
    await new Promise(resolve => setTimeout(resolve, 250));
  }
  return false;
}
```

Polling the health endpoint would race against server startup. The registry entry with the
matching PID is a reliable signal that the daemon is fully operational.

## Pattern: Graceful Stop with Escalation

`cmdStop` uses a three-tier shutdown strategy:

1. **HTTP shutdown** (graceful) — `POST /shutdown` to the daemon port
2. **SIGTERM** (fallback) — from PID in legions registry, with timeout
3. **SIGKILL** (last resort) — if SIGTERM doesn't exit within 5 seconds

Also handles stale registry entries where the PID is dead (cleans up without signaling).
