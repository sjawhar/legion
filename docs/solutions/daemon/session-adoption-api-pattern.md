---
title: "Session Enlistment API Pattern"
category: daemon
tags:
  - session-management
  - api-extension
  - guard-pattern
  - external-registry
  - cli-enlistment
date: 2026-04-11
status: active
module: daemon
related_issues:
  - "#98"
  - "#522"
symptoms:
  - "How to add optional fields to POST /workers"
  - "How to prevent duplicate session enlistment"
  - "How to scan OC registry for session info"
  - "session_already_enlisted 409 error"
---

# Session Enlistment API Pattern

## Problem

Standalone OpenCode sessions need to be enlisted as Legion workers without restarting them.
This requires extending `POST /workers` with an optional `sessionId` field while preserving
all existing behavior when the field is absent.

## Pattern: Optional Field Extension on Existing Endpoints

When adding an optional field to an existing HTTP endpoint:

```
extract field → validate format (422) → validate semantics (409) → override default computation
```

### 1. Extract Early, Validate Before Use

```typescript
const providedSessionId = payload.sessionId;

// Format validation — pure regex, no I/O
if (
  providedSessionId !== undefined &&
  (typeof providedSessionId !== "string" ||
    !SESSION_ID_PATTERN.test(providedSessionId))
) {
  return jsonResponse({ error: "invalid_session_id", ... }, 422);
}
```

Check `!== undefined` first so that omitting the field entirely is always valid.
Type-check (`typeof ... !== "string"`) before regex to reject `sessionId: 12345`.

### 2. Guard-Before-Insert for In-Memory Maps

When the `workers` Map has no transactional guarantees, place the duplicate check
immediately before `workers.set()` — not at the top of the handler.

```typescript
// Right before workers.set(entry.id, entry):
if (typeof providedSessionId === "string") {
  for (const [, existingEntry] of workers) {
    if (existingEntry.status !== "dead" && existingEntry.sessionId === providedSessionId) {
      return jsonResponse({ error: "session_already_enlisted", id: existingEntry.id }, 409);
    }
  }
}
workers.set(entry.id, entry);
```

**Why here and not earlier:** Between the check and the insert, code may call
`createSession()` which has side effects (creates SQLite rows). Placing the guard
right before `set()` minimizes the window where a concurrent request could slip through.
In single-threaded Bun this is mostly academic, but the pattern is correct for any runtime.

**Why only non-dead workers:** Dead workers are tombstones — their session IDs should be
recyclable for new enlistment.

### 3. Conditional Override for Computed Values

```typescript
const sessionId =
  typeof providedSessionId === "string"
    ? providedSessionId
    : computeSessionId(opts.legionId, issueId, mode, version);
```

This preserves the existing deterministic computation when `sessionId` is absent.
The `typeof` check doubles as a null guard — no need for a separate `if`.

## Pattern: CLI Enlistment via Existing Endpoint

Enlistment reuses `POST /workers` with `force: true` rather than a new endpoint:

```typescript
const body = {
  issueId: opts.issue,
  mode: opts.mode,
  workspace,
  sessionId: session,
  force: true,  // bypass phase prerequisite validation
  prompt: `/legion-worker ${opts.mode} mode for ${opts.issue}`,
};
```

**Why `force: true`:** Enlisted sessions weren't started by Legion, so phase gates
don't apply. The session may be mid-task — we just want to register it.

**Why include `prompt`:** Even though the session is already running, the prompt tells
it to load the worker skill and start operating as a managed worker.

## Pattern: Defensive External Registry Parsing

The OC registry (`/run/user/$UID/opencode-$UID/*.json`) is an external contract that
can change format without notice. Parse defensively:

```typescript
for (const file of files) {
  if (!file.endsWith(".json")) continue;
  try {
    const content = await readFile(path.join(dir, file), "utf-8");
    const entry = JSON.parse(content);
    const session = entry.session;
    if (session?.id === sessionId) {
      const pid = typeof entry.pid === "number" ? entry.pid : undefined;
      const entryDir = typeof entry.dir === "string" ? entry.dir : undefined;
      if (pid !== undefined && entryDir !== undefined) {
        return { pid, dir: entryDir };
      }
    }
  } catch { continue; }
}
```

**Rules:**
1. Directory missing → return null (not an error)
2. Non-`.json` files → skip
3. Malformed JSON → `continue` (silently skip)
4. Missing `pid` or `dir` → skip (type-check each field individually)
5. Use `registryDir` parameter override for testing (inject temp directory)

## Gotchas

- **`normalizedIssueId` is declared downstream** — when inserting validation blocks
  in `POST /workers`, check you're not accidentally referencing a variable that hasn't
  been declared yet. See `server-side-dispatch-validation.md`.
- **`createSession()` 409 is idempotent** — when an enlisted session already exists in
  SQLite, `createSession()` returns 409 `DuplicateIDError`, which `serve-manager.ts`
  treats as success. No special handling needed for this case.
- **SIGHUP PID is transient** — the PID from the OC registry is used once at enlist-time
  to send SIGHUP. Do NOT persist it in `WorkerEntry` or `workers.json`.
- **`SESSION_ID_PATTERN` lives in `types.ts`** — it's shared between daemon (server.ts)
  and CLI (cli/index.ts), so it belongs in the shared types module, not in either consumer.
