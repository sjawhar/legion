---
title: "Idempotent Creation Must Trust the Server's Actual ID"
category: daemon
tags:
  - session-management
  - idempotent-creation
  - error-handling
  - observability
  - 409-conflict
date: 2026-04-05
status: active
module: daemon
related_issues:
  - "205"
symptoms:
  - "zombie workers with 0 messages"
  - "prompts sent to wrong session"
  - "session ID mismatch on 409 DuplicateIDError"
  - "workers idle after daemon restart"
---

# Idempotent Creation Must Trust the Server's Actual ID

## The Bug

`createSession()` in `serve-manager.ts` had two code paths for handling the server's
response — the success path (200 OK) correctly returned the server-assigned `body.id`,
but the 409 DuplicateIDError path silently returned the client's computed/requested ID.

When the server assigns a different ID than requested (which happens in production),
the daemon stored the wrong session ID. Subsequent prompts were sent to a session that
didn't match, producing zombie workers — 0 messages, 688MB idle, no errors logged.

## Root Cause Pattern: Error Path as Second-Class Success Path

The 409 "already exists" response is not an error — it's an **alternative success path**.
The resource exists and the server is telling you its actual identity. But developers
naturally treat 409 handlers as "no-op shortcuts" and skip the same validation logic
applied to the 200 path.

```typescript
// 200 path — full validation, correct ID, warning on mismatch
const parsed = Schema.safeParse(body);
if (parsed.data.id !== sessionId) {
  console.warn(`mismatch: requested=${sessionId} actual=${parsed.data.id}`);
}
return parsed.data.id;  // ✅ server's actual ID

// 409 path — quick return, no validation, no warning
return sessionId;  // ❌ client's computed ID — WRONG
```

## The Invariant

**Any code path that resolves a resource identity — whether via creation (200) or
conflict recovery (409) — must return the server-assigned ID, not the client's
requested ID.**

The server is the source of truth. The client's computed ID is a *suggestion*, not
a guarantee.

## Warning Discipline

If the success path warns on ID mismatch, every alternative success path must too.
Silent "recovery" paths are debugging black holes — the bug manifested as idle workers
with zero diagnostic signal until someone manually inspected process memory usage.

**Heuristic for code review**: When reviewing 409/conflict handlers, ask: "If the server
returns a different ID than requested, will I know?" If the answer is no, add a warning.

## Architectural Insight

The server layer (`server.ts`) already correctly stored whatever `createSession()`
returned — the bug was entirely in the adapter layer (`serve-manager.ts`). This meant
the fix was surgical: 8 lines in one function, no cascading changes.

This validates the layered design: `server.ts` treats the adapter's return value as
authoritative, so fixing the adapter automatically fixed the entire prompt routing chain.

## Applicability

This pattern applies anywhere the codebase does idempotent creation with server-assigned
IDs. The issue flagged `index.ts` worker re-creation as a watch-item for the same class
of bug. Any future adapter methods that create resources (sessions, workspaces, etc.)
should follow this invariant from the start.
