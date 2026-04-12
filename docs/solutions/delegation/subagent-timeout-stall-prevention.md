---
title: Subagent Timeout and Stall Prevention Pattern
date: 2026-04-12
status: active
tags:
  - delegation
  - subagents
  - timeout
  - reliability
  - skill-authoring
  - background-task
---

# Subagent Timeout and Stall Prevention Pattern

## Problem

Workers stall indefinitely when background subagents time out, die, or never return.
Without explicit timeouts, one failing subagent cascades to a system-wide stall — the
worker waits forever, the controller sees no `worker-done`, and the issue silently freezes.

## Two-Layer Fix

The solution separates **mechanism** (infrastructure) from **policy** (workflow docs):

### Layer 1: Infrastructure — `timeoutMs` in `BackgroundTaskManager`

Add `timeoutMs` to `LaunchOptions` and `BackgroundTask`. In `launch()`, call
`scheduleTimeout()` after session creation:

```typescript
if (opts.timeoutMs !== undefined && opts.timeoutMs > 0) {
  this.scheduleTimeout(task.id, opts.timeoutMs);
}
```

`scheduleTimeout()` fires a timer that calls `finalize(task, "failed", { error: "Timed out after Xs" })`
and aborts the session. The timer is cleared on normal completion or cancellation.

The `background_task` tool exposes this as `timeout_seconds` (optional integer).

### Layer 2: Workflow Policy — Skill Docs

Every workflow that spawns a background subagent must:
1. Pass `timeout_seconds: 180` (or 300 for complex operations)
2. Include explicit skip instructions: "If the subagent fails or times out (>3 min): proceed with your own analysis only"

**Standard timeouts:**
- Most subagent operations: `timeout_seconds: 180` (3 minutes)
- Complex operations (wave convergence, cross-family review): `timeout_seconds: 300`

## Quality Gates Must Be Optional

Subagent steps are **quality improvements, not hard prerequisites**. Always provide a
continuation path when a quality gate fails:

```markdown
**If the subagent fails or times out (>3 min):** Proceed with your own analysis only.
Note "Outside perspective skipped (subagent timeout)" in the retro output.
Your implementation context is the primary value — the outside perspective is supplementary.
**Do NOT stall waiting for the subagent.**
```

This principle applies to: retro outside-perspective subagents, cross-family review,
Oracle consultations, and any other non-blocking quality step.

## Testing Timeout Behavior

When adding timeout functionality, test both the timeout behavior AND the cleanup:

```typescript
// Test: timeout fires and marks task failed
it("auto-cancels after timeoutMs", async () => {
  const task = await manager.launch({ ..., timeoutMs: 50 });
  await new Promise((resolve) => setTimeout(resolve, 100));
  expect(task.status).toBe("failed");
  expect(task.error).toContain("Timed out");
  expect(abortSpy).toHaveBeenCalledTimes(1);
});

// Test: timer cleared on normal completion (no double-fire)
it("does not fire timeout when task completes in time", async () => {
  const task = await manager.launch({ ..., timeoutMs: 200 });
  task.status = "running";
  await manager.finalize(task, "completed");
  await new Promise((resolve) => setTimeout(resolve, 300));
  expect(task.status as string).toBe("completed"); // not overwritten
});

// Test: timer cleared on cancel
it("clears timer on cancel", async () => {
  const task = await manager.launch({ ..., timeoutMs: 5000 });
  expect(timers.has(task.id)).toBe(true);
  await manager.cancel(task.id);
  expect(timers.has(task.id)).toBe(false);
});
```

## Documentation Sync

Code changes and workflow doc changes must ship in the same PR. If the infrastructure
adds `timeout_seconds` but the skill docs don't instruct workers to use it, the feature
is invisible. Update both in the same commit.

## Related

- `delegation/hardening-patterns.md` — idempotent finalization, task persistence
- `skill-patterns/parallel-subagent-background-execution.md` — background task patterns
- PR: https://github.com/sjawhar/legion/pull/460
