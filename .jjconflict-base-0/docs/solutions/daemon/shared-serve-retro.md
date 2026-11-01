---
title: Shared Serve Refactor — Implementer Retro
category: daemon
tags:
  - shared-serve
  - opencode-serve
  - dependency-injection
  - testing
  - crash-recovery
  - sessions
date: 2026-02-15
status: active
module: daemon
related_issues:
  - "LEG-136"
---

# Shared Serve Refactor — Implementer Retro

**Issue:** LEG-136
**PR:** https://github.com/sjawhar/legion/pull/50

## What Was Hard

### Environment variable bleeding in tests

The daemon tests run inside a Legion worker context where `LEGION_CONTROLLER_SESSION_ID` is set. The internal controller test initially failed because `loadConfig()` picked up the real env var, forcing external controller mode. Fix: explicitly pass `controllerSessionId: undefined` in test overrides.

**Pattern:** When testing daemon startup paths, always explicitly set ALL config fields that come from environment variables. Don't rely on defaults — the test environment may have Legion env vars set.

### Health tick infinite recursion with noopSetTimeout

The original test used a `noopSetTimeout` that immediately invoked its callback. Combined with the recursive `scheduleHealthTick()` pattern (callback → finally → scheduleHealthTick → noopSetTimeout → callback → ...), this created an infinite microtask chain that hung the test runner.

**Fix:** Use `silentSetTimeout` that captures the callback without invoking it. Tests that need to exercise the health tick manually invoke the captured callback once.

**Pattern:** For recursive `setTimeout`-based loops in DI, test mocks should capture (not invoke) the callback. The test explicitly calls it when ready.

### Controller re-creation was missed

The cross-family review caught that after a shared serve crash+restart, worker sessions were re-created but the controller session was not. This was a real bug — the controller would be permanently dead after a serve restart.

**Pattern:** When replacing N independent processes with one shared process, the crash recovery path must re-create ALL session types, not just workers. Easy to miss because the controller is tracked separately from workers.

## Key Design Decisions

### pid is optional on WorkerEntry

Old model: every worker had its own process with a known PID. New model: workers are sessions on a shared process — no per-worker PID. Making `pid` optional (not removed) preserves backward compatibility with persisted state files that have `pid` values.

### env field kept but unused in POST /workers

The controller skill already sends `env` in dispatch requests. Removing validation would break the API contract. The env was used for per-process environment variables (`OPENCODE_PERMISSION`, `LINEAR_ISSUE_ID`). In the shared serve model, per-session env isn't possible — all sessions share one process environment. This is documented as a known limitation with a follow-up issue.

### controllerState.port distinguishes internal vs external controllers

External controllers have `controllerState = { sessionId }` (no port). Internal controllers have `controllerState = { sessionId, port }`. The health tick uses `controllerState?.port` to decide whether to re-create the controller — external controllers manage their own lifecycle.

### Adopted serve gets sharedServePid = 0

When the daemon starts and finds an existing healthy serve, it adopts it without knowing the PID. This means `stopServe` won't be called during shutdown (checked via `sharedServePid > 0`). Acceptable because: (1) we didn't spawn it so we shouldn't kill it, (2) if we did spawn it in a previous daemon lifecycle, the serve will eventually die when nothing connects to it.

## What Went Well

- **Plan quality was high** — the implementation plan had complete code for each module, which made execution straightforward. Sequential task ordering matched actual dependencies.
- **DI pattern enabled clean testing** — `DaemonDependencies` and `resolveDependencies` made it possible to test all startup/health/shutdown paths without real processes.
- **Idempotent session creation** — treating 409/DuplicateIDError as success in `createSession` is critical for crash recovery. Sessions can be re-created safely after serve restarts.
- **Cross-family review caught a real bug** — the controller re-creation omission would have been a production issue.

## What I'd Do Differently

- **Test the internal controller path from the start.** All three original index.ts tests used `controllerSessionId: "ses_test"` (external mode), which meant the internal controller startup + health recovery path was untested until the cross-family review flagged it.
- **Check for env var bleeding earlier.** The `LEGION_CONTROLLER_SESSION_ID` env var issue wasted a debugging cycle. A pattern of explicitly overriding all env-derived config in tests would prevent this class of issues.
