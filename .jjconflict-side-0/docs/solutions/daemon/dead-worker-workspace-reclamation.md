---
title: "Two-Pass Dead Worker Workspace Reclamation"
category: daemon
tags:
  - health-tick
  - dead-worker
  - cleanup
  - workspace-management
  - jj-workspaces
  - two-pass-pattern
  - failure-isolation
  - testing
date: 2026-04-13
status: active
module: daemon
related_issues:
  - "488"
symptoms:
  - "jj workspace add fails because workspace already exists"
  - "new workers dispatched for an issue get 0 messages and appear busy"
  - "dead workers leave orphaned jj workspaces after daemon restart"
  - "workspace cleanup blocks future dispatch to the same issue"
---

# Two-Pass Dead Worker Workspace Reclamation

## Problem

When workers crash or are aborted during daemon restarts, they leave orphaned jj workspaces and git worktrees on disk. The liveness sweep (see `session-liveness-detection-pattern.md`) correctly marks these workers as `dead`, but their workspaces persist. When the controller later dispatches a new worker for the same issue, `jj workspace add` fails because the workspace name is already taken, producing a worker that appears busy (status `starting`) but never processes any messages.

## Solution: Health-Tick Cleanup After Liveness Sweep

Add a `cleanupDeadWorkers()` function that runs on every health tick, immediately after the liveness sweep. This placement ensures workers reaped in the current tick are eligible for cleanup in the same tick, rather than waiting for the next one.

The function is exposed on the `startServer` return object (alongside `fetchAndProcessState`), not as an HTTP endpoint. Dead worker cleanup is an internal daemon concern — exposing it externally would add an untested surface for an operation that should be autonomous.

### Guard Conditions

Cleanup runs only when `serveHealthy && !restartReason`, matching the same compound guard used by the liveness sweep. The call is wrapped in try/catch with a `(non-fatal)` warning — workspace cleanup failure must never take down the health tick or trigger serve restarts.

```typescript
if (serveHealthy && !restartReason) {
  try {
    await cleanupDeadWorkers();
  } catch (error) {
    console.warn(`[health-tick] dead worker cleanup failed: ... (non-fatal)`);
  }
}
```

## The Two-Pass Pattern

Multiple dead workers can exist for the same issue (e.g., an implement worker and a test worker both die). The workspace is shared per issue, so cleanup must happen only once per issue, but state removal happens per worker. This creates a natural two-pass structure:

### Pass 1: Workspace Cleanup (Deduplicated by Issue ID)

```
for each dead worker:
  extract issueId from workerId
  if already cleaned or already failed for this issueId: skip
  run cleanupWorkspace(paths, legionId, issueId, repoRef, repoManagerDeps)
    → jj workspace forget + rm -rf
  track success in cleanedWorkspaceIssueIds
  on failure: track in failedWorkspaceIssueIds
```

### Pass 2: State Removal (Per Worker, Gated on Pass 1)

```
for each dead worker:
  if issueId is in failedWorkspaceIssueIds: SKIP (preserve for retry)
  delete serve session
  detach from Envoy
  unsubscribe all Envoy topics
  remove from workers map + crashHistory map
  increment cleanedWorkers count

if cleanedWorkers > 0: persistState()
```

**Critical invariant**: If workspace cleanup fails for an issue, ALL workers for that issue are preserved. This prevents a scenario where the daemon forgets about a worker but leaves its worktree on disk — which would be worse than an extra dead entry that retries on the next tick.

## Why Two Passes Instead of One

A single-pass approach would attempt workspace cleanup and state removal in the same loop. The problem: if you have workers A (implement) and B (test) for the same issue, and A's workspace cleanup succeeds, you'd remove A's state, then try to clean B's workspace (which was already removed by A). The second `jj workspace forget` might fail with a "workspace not found" error, which would then block B's state removal.

The two-pass pattern avoids this by separating the concern cleanly:
- Pass 1 answers: "Is the workspace for this issue cleaned up?"
- Pass 2 answers: "For each worker whose issue workspace is clean, remove the worker state."

This is the same pattern used by `cleanupDoneIssueWorkers()` for cleaning up workers whose issues have transitioned to DONE.

## Duplication Note

`cleanupDeadWorkers` and `cleanupDoneIssueWorkers` share ~80% of their structure. The trigger differs (status === "dead" vs issue status === DONE) and the done-issue variant also cleans up `issueStateCache` and `trackedIssueIds`. A future refactor could extract a shared `cleanupWorkersMatching(predicate)` helper with an optional post-cleanup callback for the variant-specific cleanup.

## Testing Pattern: Direct Function Handle

The dead worker tests need access to the `cleanupDeadWorkers` function handle, which the shared `startTestServer` helper doesn't expose. The tests use `startServer` directly with `originalFetch` and local URLs, accepting the setup duplication in exchange for precise control.

The `StartServerDependency` type uses `Partial<Pick<ServerHandle, "cleanupDeadWorkers">>` to make the function optional for callers that don't need it — matching the existing pattern for `fetchAndProcessState`.

## Lifecycle Summary

This completes the dead worker lifecycle in the daemon:

1. **Detection**: Liveness sweep marks workers as `dead` (issue 447, `session-liveness-detection-pattern.md`)
2. **Workspace reclamation**: `cleanupDeadWorkers()` removes jj workspace + directory (this PR, issue 488)
3. **State removal**: Same function removes worker entries, crash history, sessions, Envoy subscriptions
4. **Re-dispatch**: Controller can now dispatch a new worker for the same issue without workspace conflicts

Previously, step 2-3 required manual intervention or a daemon restart.
