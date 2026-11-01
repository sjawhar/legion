---
title: "Server-side dispatch validation via pure functions and in-memory cache"
category: daemon
tags:
  - dispatch-validation
  - pure-functions
  - caching
  - gated-modes
  - escape-hatch
date: 2026-04-05
status: active
module: daemon
related_issues:
  - "sjawhar-legion-203"
symptoms:
  - "phase_prerequisite_unmet"
  - "controller skips retro before merge"
  - "422 from POST /workers"
---

# Server-side dispatch validation via pure functions and in-memory cache

## Context

The controller agent can construct multi-step dispatch pipelines that skip
lifecycle phases (e.g., review → merge, bypassing retro). Prose-based enforcement
in the controller skill is insufficient — agents rationalize around it.

Issue #203 added server-side enforcement: `POST /workers` validates gated mode
dispatches against cached issue state from `/state/collect`.

## Pattern 1: Pure validation function in decision.ts

Extract validation logic into a pure function that takes data and returns a
discriminated union — no side effects, no I/O:

```typescript
export type DispatchValidation =
  | { valid: true }
  | { valid: false; suggestedAction: ActionType; reason: string };

export function canDispatchMode(
  cachedState: IssueState | undefined,
  requestedMode: WorkerModeLiteral
): DispatchValidation
```

**Why this works:**

- Independently testable without spinning up a server
- The server handler just calls it and maps the result to HTTP status codes
- TypeScript narrowing on the discriminated union forces callers to handle both cases
- Graceful degradation is baked in: `undefined` → `{ valid: true }` (cache miss = allow)

**Reuse when:** Adding any new server-side guard with non-trivial logic. Extract to
`decision.ts`, test there, wire in `server.ts`.

## Pattern 2: Gated Set for extensibility

Use `ReadonlySet<T>` constants to define which modes require validation:

```typescript
export const GATED_MODES: ReadonlySet<WorkerModeLiteral> = new Set([WorkerMode.MERGE]);
```

Adding a new gated mode is a one-line change. The validation logic in
`canDispatchMode` doesn't need to change — it checks `GATED_MODES.has(mode)` and
uses the existing `ACTION_TO_MODE` mapping to determine validity.

**Reuse when:** Any feature where "which items need special handling" will grow.
Start with a Set, not a chain of `if` statements.

## Pattern 3: Cache at existing collection points

Populate a `Map<string, IssueState>` as a side effect of the existing
`/state/collect` and `/state/fetch-and-collect` endpoints:

```typescript
for (const [issueId, issueState] of Object.entries(state.issues)) {
  issueStateCache.set(issueId.toLowerCase(), issueState);
}
```

**Why this works:**

- No new I/O in the dispatch hot path
- Cache is always fresh because the controller calls collect before dispatching
- Cache miss = graceful degradation (allow dispatch), not hard failure
- Key normalization (`toLowerCase()`) applied at both write and read time

**Limitation:** The cache is in-memory only. Daemon restart empties it, and the
first dispatch after restart bypasses validation. This is acceptable because the
controller calls `/state/collect` every ~60s, so the window is brief.

## Pattern 4: Force flag escape hatch

Every validation gate should ship with a bypass mechanism:

```typescript
const forceDispatch = payload.force === true;
if (!forceDispatch) { /* validate */ }
else { console.warn(`[dispatch] force=true ...`); }
```

The CLI error message tells humans exactly how to bypass:
`To force dispatch: legion dispatch <issue> <mode> --force`

**Key details:**

- `force === true` (strict equality) prevents accidental bypass from truthy values
- Warning log creates an audit trail
- The `--force` flag is for humans only — the controller skill must never use it

## Gotcha: normalizedIssueId in POST /workers

The `POST /workers` handler is ~200 lines. When adding validation early in the
handler, check whether variables like `normalizedIssueId` are already declared
downstream. Moving the declaration earlier (and removing the duplicate) avoids
runtime `has already been declared` errors that TypeScript doesn't catch at
compile time.
