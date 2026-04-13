---
title: "Polling Pattern for Testing Fire-and-Forget Async Work"
category: testing
tags:
  - async-testing
  - fire-and-forget
  - polling
  - background-tasks
  - opencode-plugin
  - flaky-tests
date: 2026-04-13
status: active
related_issues:
  - "275"
symptoms:
  - "integration test passes locally but flakes in CI"
  - "setTimeout in test makes it timing-dependent"
  - "fire-and-forget async work not settled when test asserts"
  - "BackgroundTaskManager task not complete when checking results"
---

# Polling Pattern for Testing Fire-and-Forget Async Work

## Context

When `BackgroundTaskManager.launch()` fires a task via a detached promise (fire-and-forget),
tests cannot `await` the result directly. The initial approach of `setTimeout(100)` was
timing-dependent and flaky — fast in local dev, potentially slow in CI.

This came up in the model fallback chain integration tests (issue #275) but applies to any
fire-and-forget async pattern in the codebase.

## The Pattern

Replace fixed-duration sleeps with a polling helper that checks observable state:

```typescript
async function waitForTaskSettled(
  task: { status: string },
  maxWaitMs = 2000,
  intervalMs = 10,
): Promise<void> {
  const deadline = Date.now() + maxWaitMs;
  while (
    (task.status === "running" || task.status === "pending") &&
    Date.now() < deadline
  ) {
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
}
```

### Why This Works

- **Deterministic**: completes as soon as state changes, not after an arbitrary delay
- **Fast locally**: 10ms polling means typical latency is 10-20ms, not 100ms
- **Safe in CI**: 2s max wait accommodates slow environments without flaking
- **Self-documenting**: the poll condition makes the dependency explicit

### When to Use

- Any test that needs to observe side effects of `BackgroundTaskManager.launch()`
- Tests for `startPrompt` outcomes (model switching, failure recording, finalization)
- Any fire-and-forget async pattern where the test needs to wait for completion

### When NOT to Use

- If you can `await` the async work directly — do that instead
- If the observable state doesn't exist — you may need to add it first

## Key Insight

The root problem isn't timing — it's that fire-and-forget patterns intentionally sever the
promise chain. The solution is to poll on *observable state* (task status, result values, etc.)
rather than guessing how long the severed chain takes to resolve.

If you find yourself writing `setTimeout(N)` in a test to "wait for something to finish,"
that's the signal to use this pattern instead.
