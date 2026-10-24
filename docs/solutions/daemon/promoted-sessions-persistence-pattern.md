---
title: "Promoted Sessions: Persistence, Startup Reclaim, and Feedback Event Registration"
category: daemon
tags:
  - promoted-sessions
  - envoy
  - feedback-events
  - startup
  - persistence
  - zod-schema
  - discriminated-union
  - session-management
  - defense-in-depth
date: 2026-04-16
status: active
module: daemon
related_issues:
  - "594"
symptoms:
  - "tsc --noEmit fails with 'Type is not assignable' on feedback event literal"
  - "promoted session Envoy roles lost after daemon restart"
  - "corrupt promoted.json silently returns empty with no debug trail"
---

# Promoted Sessions: Persistence, Startup Reclaim, and Feedback Event Registration

## Problem

Adding persistent promoted sessions (CLI promote/demote with `promoted.json` storage) surfaced
three patterns worth documenting:

1. **Feedback event registration is a 3-place change** — logging a new event type via
   `feedbackLogger?.log()` requires registering it in `feedback.ts` in three places, and
   missing any one causes a typecheck failure that only CI catches.

2. **Startup role reclaim is required for Envoy role persistence** — Envoy roles are volatile
   (session-scoped). Persisting promoted sessions in `promoted.json` is necessary but not
   sufficient; the daemon must re-claim roles on startup.

3. **Corrupt file handling should preserve evidence** — silently returning empty on corrupt
   JSON loses debugging information. The `moveCorruptFile()` pattern preserves the corrupt
   file for post-mortem analysis.

## Solution Patterns

### 1. Adding a New Feedback Event Type

When adding a new event to the feedback logger, you must update three things in
`packages/daemon/src/daemon/feedback.ts`:

```typescript
// 1. Zod schema with z.literal for the event name
export const MyNewEventSchema = FeedbackEventBase.extend({
  event: z.literal("daemon.my_new_event"),
  // ... event-specific fields
});

// 2. Type alias
type MyNewEvent = z.infer<typeof MyNewEventSchema>;

// 3. Add to BOTH the Zod discriminated union AND the TypeScript type union
export const FeedbackEventSchema = z.discriminatedUnion("event", [
  // ... existing schemas ...
  MyNewEventSchema,  // <-- add here
]);

export type FeedbackEvent =
  | // ... existing types ...
  | MyNewEvent;  // <-- and here
```

**Why this is easy to miss:** The logger accepts events at runtime regardless of schema
registration (Zod validation happens on read, not write). Tests pass because they exercise
the logger, not the schema. Only `tsc --noEmit` catches the type mismatch, and only if you
run it locally — otherwise it shows up as a CI-only failure.

**Prevention:** Always run `bunx tsc --noEmit` after adding new feedback events.

### 2. Startup Role Reclaim for Promoted Sessions

Envoy roles are volatile — they disappear when the session dies or the Envoy server restarts.
Persisting promoted sessions in `promoted.json` handles daemon restarts, but the Envoy role
registrations must be actively re-claimed.

The startup reclaim runs after serve is up and worker sessions are recreated (so
`adapter.sessionExists()` works):

```
Read promoted.json
  → For each entry:
    → Check liveness via adapter.sessionExists()
    → If alive: POST /v1/roles/set to Envoy (re-claim role)
    → If dead: auto-demote (remove from promoted.json)
```

This mirrors the health tick auto-demote logic but in reverse — the health tick removes dead
sessions, while startup reclaim restores live ones.

**Key placement:** After worker session recreation, before repo fetch warmup. The serve must
be running for `sessionExists()` to work, and `config.envoyUrl` must be set.

### 3. Defense-in-Depth Validation

When adding API routes that accept user input, validate at the API layer even if the CLI
already validates. The CLI's `SESSION_ID_PATTERN` check prevents malformed IDs from the
command line, but the daemon API can be called directly by the controller or other tools.

Pattern: Import and apply the same validation constant (`SESSION_ID_PATTERN`) in both the
CLI and the API route handler.

### 4. moveCorruptFile Pattern

When reading persisted JSON files, don't silently return empty on corruption. Rename the
corrupt file with a timestamp suffix so it's preserved for debugging:

```typescript
async function moveCorruptFile(filePath: string): Promise<void> {
  const timestamp = new Date().toISOString().replace(/[:.]/g, "-");
  const corruptPath = `${filePath}.corrupt.${timestamp}`;
  try {
    await rename(filePath, corruptPath);
  } catch (err) {
    console.warn(`Failed to rename corrupt file ${filePath}:`, err);
  }
}
```

This pattern exists in `state-file.ts` and `persistence.ts`. When adding new persisted JSON
files, follow the same pattern rather than silently returning defaults.

## Testing Considerations

- **Session ID format in tests:** When adding `SESSION_ID_PATTERN` validation to API routes,
  ensure test fixtures use valid session IDs. The pattern is
  `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$` (30 chars total). Invalid test IDs will cause
  previously-passing tests to fail with 400 responses.

- **Feedback event tests:** The existing feedback tests validate schema parsing (read path),
  not event logging (write path). A new event type that's logged but not registered in the
  schema will pass all tests but fail typecheck.

## Key Files

| File | Role |
|------|------|
| `packages/daemon/src/daemon/feedback.ts` | Feedback event schemas and types |
| `packages/daemon/src/daemon/promoted-sessions.ts` | Promoted session CRUD with atomic writes |
| `packages/daemon/src/daemon/index.ts` | Startup reclaim + health tick auto-demote |
| `packages/daemon/src/daemon/server.ts` | API routes with session ID validation |
| `packages/daemon/src/daemon/state-file.ts` | Reference for `moveCorruptFile` pattern |
