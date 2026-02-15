---
title: Graceful Worker Shutdown via HTTP Dispose
date: 2026-02-15
category: daemon
tags: [process-management, cleanup, testing, defense-in-depth]
related-issues: [LEG-130]
related-prs: [46]
---

# Graceful Worker Shutdown via HTTP Dispose

## Problem

`opencode serve` processes ignore SIGTERM, causing zombie processes that hold ports and create cascading failures:

1. **Port conflicts** — zombie holds allocated port, new worker spawn fails
2. **Stale sessions** — zombie continues processing requests on old session
3. **Incorrect tracking** — daemon thinks worker is dead, but process still runs
4. **Resource leaks** — accumulated zombies consume memory and file descriptors

The root cause: `killWorker()` sent SIGTERM, which `opencode serve` doesn't handle.

## Solution

Replace SIGTERM with a three-phase graceful shutdown:

### Phase 1: HTTP Dispose (Graceful)

```typescript
await fetch(`http://127.0.0.1:${entry.port}/global/dispose`, {
  method: "POST",
  signal: AbortSignal.timeout(disposeTimeoutMs),
});
```

**Pattern**: Use the application's own shutdown endpoint instead of OS signals. This allows the process to:
- Close HTTP server gracefully
- Flush pending writes
- Clean up resources
- Exit cleanly

**Timeout**: 3s default. If dispose hangs, proceed to phase 2.

**Error handling**: Dispose failure (connection refused, timeout, 500 error) is non-fatal — proceed to phase 2. The process might already be dead, or the endpoint might be broken.

### Phase 2: Poll for Exit (Verification)

```typescript
const deadline = Date.now() + waitTimeoutMs;
while (Date.now() < deadline) {
  try {
    process.kill(entry.pid, 0); // Signal 0 = existence check
  } catch {
    return; // Process exited
  }
  await new Promise((resolve) => setTimeout(resolve, pollIntervalMs));
}
```

**Pattern**: Use `process.kill(pid, 0)` to check if process exists without sending a signal. Throws ESRCH if process is dead.

**Timing**: 5s timeout, 200ms poll interval (25 checks). Balances responsiveness vs CPU usage.

**Why poll instead of waitpid**: Node.js doesn't expose `waitpid()` for arbitrary PIDs. Polling is the portable solution.

### Phase 3: SIGKILL (Forceful)

```typescript
try {
  process.kill(entry.pid, "SIGKILL");
} catch (error) {
  const err = error as NodeJS.ErrnoException;
  if (err.code === "ESRCH") {
    return; // Already dead
  }
  throw error; // Unexpected error (EPERM, etc.)
}
```

**Pattern**: SIGKILL is unblockable — guaranteed to kill the process. Use as last resort.

**ESRCH handling**: If process died between poll loop and SIGKILL, treat as success.

**Other errors**: Re-throw (e.g., EPERM = permission denied, indicates system-level issue).

## Defense in Depth: Port Verification

Even with graceful shutdown, zombies can still occur (kernel bugs, OOM killer, power loss). Add a pre-spawn port check:

### isPortFree() — TCP Bind Check

```typescript
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

**Pattern**: Attempt to bind the port. If bind succeeds, port is free. If bind fails (EADDRINUSE), port is occupied.

**Why not lsof/netstat**: Parsing command output is fragile. TCP bind is the kernel's source of truth.

**Cleanup**: Close the test server immediately after bind succeeds.

### Integration: Fail-Fast on Occupied Port

```typescript
const port = opts.portAllocator.allocate();
if (opts.isPortFree) {
  const free = await opts.isPortFree(port);
  if (!free) {
    opts.portAllocator.release(port);
    return serverError("allocated_port_occupied");
  }
}
```

**Pattern**: Single check with fail-fast error. No retry loop.

**Why fail-fast**: Port occupation after allocation indicates a cleanup bug. Surfacing the error to the controller (via 500 response) allows it to:
- Log the issue
- Retry with a different port
- Alert the user
- Trigger cleanup logic

**Why no retry**: Retrying masks the underlying bug. Better to fail loudly and fix the root cause.

**Dependency injection**: `isPortFree` is optional in `ServerOptions`. Tests can inject a mock; production uses the real implementation.

## Testing Patterns

### 1. Mock fetch for Dispose Tests

```typescript
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
```

**Pattern**: Capture dispose URL and method for assertions. Return 200 to simulate success.

**Type assertion**: `as unknown as typeof fetch` bypasses TypeScript's strict function signature checks. Necessary for test mocks.

### 2. Mock process.kill for Exit Polling Tests

```typescript
process.kill = ((pid: number, signal?: NodeJS.Signals) => {
  if (signal === "SIGKILL") {
    sigkillPid = pid;
    return true;
  }
  if (signal === 0) {
    return true; // Still alive
  }
  return true;
}) as typeof process.kill;
```

**Pattern**: Track SIGKILL calls separately from existence checks (signal 0). Return `true` for "still alive" to force timeout.

**Configurable timeouts**: Tests pass short timeouts (50ms wait, 10ms poll) to avoid slow tests.

### 3. Test Coverage Matrix

Four killWorker tests cover all code paths:

| Test | Dispose | Poll Result | SIGKILL | Outcome |
|------|---------|-------------|---------|---------|
| Graceful | Success | Exits immediately | Not sent | Success |
| Lingering | Success | Times out | Sent | Success |
| Dispose fails | Error | Times out | Sent | Success |
| Already dead | Error | ESRCH on first poll | Not sent | Success |

**Pattern**: Test matrix ensures all branches are covered. Each test verifies:
- Dispose was attempted (or not)
- Poll loop behavior (exit vs timeout)
- SIGKILL sent (or not)

### 4. Port Verification Tests

Two `isPortFree` tests:

```typescript
it("returns true when port is not in use", async () => {
  const server = createServer();
  await new Promise<void>((resolve) => {
    server.listen(0, "127.0.0.1", () => resolve());
  });
  const port = (server.address() as AddressInfo).port;
  await new Promise<void>((resolve) => server.close(() => resolve()));

  const isFree = await isPortFree(port);
  expect(isFree).toBe(true);
});
```

**Pattern**: Use port 0 to get an OS-assigned free port, then close the server. The port is now free (briefly — race condition possible, but unlikely in tests).

```typescript
it("returns false when port is in use", async () => {
  const server = createServer();
  await new Promise<void>((resolve) => {
    server.listen(0, "127.0.0.1", () => resolve());
  });
  const port = (server.address() as AddressInfo).port;

  const isFree = await isPortFree(port);
  expect(isFree).toBe(false);
  await new Promise<void>((resolve) => server.close(() => resolve()));
});
```

**Pattern**: Keep the server open during the check. The port is occupied, so `isPortFree` should return false.

Two server integration tests:

| Test | isPortFree Mock | Expected Outcome |
|------|-----------------|------------------|
| Occupied port | Returns false | 500 error, port released, no spawn |
| Free port | Returns true | 200 success, worker spawned |

**Pattern**: Inject mock `isPortFree` via `ServerOptions`. Verify error handling and port cleanup.

## Architectural Lessons

### 1. Graceful Shutdown Checklist

When implementing process cleanup:

- [ ] **Use application shutdown endpoint** — not OS signals (signals can be ignored)
- [ ] **Poll for exit** — verify the process actually died
- [ ] **Timeout and fallback** — don't wait forever; use SIGKILL as last resort
- [ ] **Handle already-dead** — ESRCH is success, not failure
- [ ] **Configurable timeouts** — allow tests to use short timeouts

### 2. Defense in Depth for Resource Allocation

When allocating scarce resources (ports, file handles, locks):

- [ ] **Verify availability** — check that the resource is actually free before use
- [ ] **Fail fast** — surface allocation failures immediately, don't retry silently
- [ ] **Release on failure** — if spawn fails, release the allocated resource
- [ ] **Dependency injection** — make verification logic injectable for testing

### 3. Testing Process Management Code

- [ ] **Mock I/O** — `fetch`, `process.kill`, `setTimeout` should be injectable
- [ ] **Short timeouts** — tests should complete in milliseconds, not seconds
- [ ] **Test matrix** — cover all branches (success, timeout, already-dead, error)
- [ ] **Verify side effects** — assert that dispose was called, SIGKILL was sent, etc.

### 4. Error Handling Patterns

**Dispose failure is non-fatal**:
```typescript
try {
  await fetch(...);
} catch {
  // Fall through to poll + SIGKILL
}
```

**Pattern**: If graceful shutdown fails, proceed to forceful shutdown. Don't abort the entire cleanup.

**ESRCH is success**:
```typescript
try {
  process.kill(pid, "SIGKILL");
} catch (error) {
  if (error.code === "ESRCH") {
    return; // Already dead = success
  }
  throw error; // Other errors are real failures
}
```

**Pattern**: Distinguish between "resource already cleaned up" (success) and "permission denied" (failure).

## Implementation Details

### Default Timeouts

```typescript
export async function killWorker(
  entry: WorkerEntry,
  waitTimeoutMs = 5000,
  pollIntervalMs = 200,
  disposeTimeoutMs = 3000,
): Promise<void>
```

**Pattern**: Use default parameters for production values, allow override for testing.

**Rationale**:
- 3s dispose timeout: Enough for graceful shutdown, not so long that it delays forceful shutdown
- 5s wait timeout: Generous buffer for process exit
- 200ms poll interval: Balances responsiveness (detects exit within 200ms) vs CPU usage (5 checks/sec)

### Dependency Injection

```typescript
interface ServerOptions {
  // ... existing fields ...
  isPortFree?: (port: number) => Promise<boolean>;
}
```

**Pattern**: Optional dependency. If not provided, skip the check (backward compatible).

**Wiring**:
```typescript
// index.ts
import { isPortFree } from "./ports";
const { server, stop } = startServer({
  // ... other options ...
  isPortFree,
});
```

**Testing**:
```typescript
await startTestServer({
  isPortFree: async () => false, // Inject mock
});
```

## Future Improvements

1. **Exponential backoff for polling** — Start with 50ms, double each iteration up to 500ms. Reduces CPU usage for slow shutdowns.
2. **Structured logging** — Log dispose attempts, poll iterations, SIGKILL fallback. Helps debug zombie issues.
3. **Metrics** — Track dispose success rate, average shutdown time, SIGKILL fallback rate. Identify patterns.
4. **Port reaper** — Periodic background task that scans for occupied ports in the allocator's range and kills zombies.
5. **Process group kill** — Use `process.kill(-pid, signal)` to kill the entire process group (handles child processes).

## Operational Pitfall: Branch Ancestry in Multi-Workspace Development

When creating PRs from jj workspaces, **verify the branch ancestry is clean** before pushing. If a workspace was created from a commit that descends from unmerged work, the PR will include those unrelated changes. This happened on this PR — initial ancestry included 3 unrelated parent commits, inflating the PR from 8 to 21 changed files.

Check with:
```bash
jj log -r 'ancestors(BOOKMARK, 5)'
```

Ensure the chain goes directly to main. If polluted, detach with:
```bash
jj rebase -s <first-relevant-commit> -d main
```

## Related Patterns

- **Graceful Degradation** — dispose fails → poll; poll times out → SIGKILL
- **Defense in Depth** — multiple layers of protection (dispose, poll, SIGKILL, port check)
- **Fail Fast** — port occupied → immediate error, no retry
- **Dependency Injection** — testability via optional `isPortFree` parameter
- **Test Matrix** — exhaustive branch coverage via systematic test cases
