---
title: "Session bootstrap delay: prompt delivery requires readiness wait after creation"
category: daemon
tags:
  - dispatch
  - session-lifecycle
  - prompt-delivery
  - retry-pattern
  - integration-testing
  - caller-server-contract
date: 2026-04-05
status: active
module: daemon
related_issues:
  - "237"
symptoms:
  - "Worker dispatched but shows idle with 0 messages"
  - "Initial prompt silently dropped on fresh daemon"
  - "prompt_async fails after createSession"
  - "Manual re-prompting works but automatic dispatch does not"
---

# Session Bootstrap Delay: Prompt Delivery Requires Readiness Wait After Creation

## Problem

On a fresh daemon (clean workers.json, no pre-existing sessions), every dispatched worker
failed to receive its initial prompt. The session was created, the worker appeared in state,
but `sendPrompt` silently failed because the serve process hadn't finished bootstrapping the
Instance context for the new session's directory.

Manual re-prompting always worked because enough time had elapsed by the time a human noticed.

## Root Cause

**Session creation and session readiness are decoupled.** `createSession` returns successfully
as soon as the session ID is registered, but the serve process needs additional time to
bootstrap the Instance context (working directory, tool configuration, etc.) before the session
can accept prompts. Sending a prompt immediately after creation hits the uninitialized Instance.

## Fix: Delay + Retry in POST /workers Handler

Added an optional `prompt` field to the `POST /workers` payload. When provided, the handler:

1. Creates the session (existing behavior)
2. Waits `SESSION_READY_DELAY_MS` (2 seconds) for bootstrap
3. Sends the prompt with 3 retries and exponential backoff (100ms, 200ms, 400ms)
4. Returns `promptDelivered: boolean` in the response

Worker creation always succeeds — a failed prompt returns `promptDelivered: false` rather
than failing the dispatch. The operator can resend manually via `legion prompt`.

## Key Learnings

### 1. Server-Side Fix Alone Was Unreachable

The initial implementation only added the delay+retry mechanism to the server's `POST /workers`
handler. But the CLI's `cmdDispatch()` was still sending prompts via a separate `POST
/workers/:id/prompt` call with no delay. The server-side fix was correct but unreachable —
no caller was using the new `prompt` field.

**The tester caught this integration gap.** All server tests passed because they tested the
handler in isolation with the `prompt` field. But the end-to-end path (CLI → daemon → serve)
was still broken.

**Rule: When moving responsibility from caller to server, update all callers in the same
change.** The caller's code doesn't just shrink — it changes shape (from 3 fetches to 2,
from catching network errors to reading a boolean field).

### 2. The Delay Is a Heuristic, Not a Readiness Signal

The 2-second `SESSION_READY_DELAY_MS` works empirically but isn't derived from an actual
readiness check. A more robust future approach would poll for session readiness (e.g., check
the session's health endpoint) rather than sleep a fixed duration.

The retry loop provides defense-in-depth for cases where 2 seconds isn't enough. The total
retry window adds 700ms on top of the initial delay.

### 3. Mock Adapters Can't Catch Bootstrap Race Conditions

The test suite mocks `sendPrompt` — which validates retry logic but can't reproduce the
actual bootstrap timing issue. The original caller-side test mocked the `/prompt` endpoint
returning 200, masking the real failure mode entirely.

To catch this class of bug earlier, consider:
- **Integration test with real serve process** that creates a session and immediately sends
  a prompt
- **Configurable delay in test adapter** that simulates slow bootstrap
- **End-to-end dispatch test** that exercises CLI → daemon → serve → session

### 4. Backward-Compatible API Extension Pattern

The `prompt` field is optional in the request, and `promptDelivered` is only present in the
response when `prompt` was provided. Existing callers that don't send `prompt` get the same
response shape as before. This is a clean pattern for extending endpoints without breaking
existing consumers.

## Constants

```typescript
export const SESSION_READY_DELAY_MS = 2000;  // Configurable for testing
const PROMPT_RETRY_ATTEMPTS = 3;
const PROMPT_RETRY_BASE_MS = 100;            // Exponential: 100, 200, 400ms
```

## Future Improvement

Replace the fixed delay with a readiness poll against the session's status endpoint. The
retry loop infrastructure is already in place — change the trigger from "sleep then retry
on error" to "poll until ready, then send once."
