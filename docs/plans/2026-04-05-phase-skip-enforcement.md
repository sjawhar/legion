# Phase Skip Enforcement — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent the controller from bypassing lifecycle phases (e.g., skipping retro before merge) by adding server-side dispatch validation to `POST /workers`.

**Architecture:** Add a `canDispatchMode()` pure function in `decision.ts` that validates whether a requested worker mode is consistent with the state machine's `suggestedAction` for a given issue. The daemon server caches `IssueState` per issue from `/state/collect` and `/state/fetch-and-collect`, and calls `canDispatchMode()` in `POST /workers` to reject invalid dispatches with 422. A `--force` flag on the CLI bypasses the gate for human escape hatches. The controller SKILL.md gets a brief note about daemon-enforced lifecycle ordering.

**Tech Stack:** TypeScript (Bun runtime), Bun test runner, Biome linter

---

## File Structure

| File | Responsibility |
|------|---------------|
| `packages/daemon/src/state/decision.ts` | Add `GATED_MODES`, `DISPATCH_ACTIONS`, `canDispatchMode()` pure function |
| `packages/daemon/src/state/__tests__/decision.test.ts` | Unit tests for `canDispatchMode()` |
| `packages/daemon/src/daemon/server.ts` | Add `issueStateCache` Map, populate in collect handlers, validate in `POST /workers`, pass `force` flag through |
| `packages/daemon/src/daemon/__tests__/server.test.ts` | Integration tests for dispatch validation + cache behavior |
| `packages/daemon/src/cli/index.ts` | Add `--force` flag to dispatch command, pass through to `POST /workers` body |
| `.opencode/skills/legion-controller/SKILL.md` | Add note about daemon-enforced lifecycle ordering |

---

### Task 1: Add `canDispatchMode()` pure function to decision.ts — Independent

**Files:**
- Modify: `packages/daemon/src/state/decision.ts`
- Test: `packages/daemon/src/state/__tests__/decision.test.ts`

#### Design

`canDispatchMode(cachedState, requestedMode)` validates whether a dispatch is allowed:

1. **`GATED_MODES`**: A `Set<WorkerModeLiteral>` containing modes that require validation. Initially only `"merge"`.
2. **`DISPATCH_ACTIONS`**: A `Set<string>` containing the `ActionType` values that represent actual dispatch actions (those starting with `dispatch_`). Used to distinguish "the state machine wants to dispatch a worker of mode X" from transition/resume/skip actions.
3. **Logic**:
   - If `requestedMode` is NOT in `GATED_MODES` → return `{ valid: true }`
   - If `cachedState` is `undefined` (cache miss) → return `{ valid: true }` (graceful degradation)
   - Compute the mode that the `suggestedAction` maps to via `ACTION_TO_MODE`
   - Check if `suggestedAction` is a dispatch action in `DISPATCH_ACTIONS` AND the mapped mode equals `requestedMode`
   - If yes → `{ valid: true }`
   - If no → `{ valid: false, reason: "...", suggestedAction, expectedAction }`

The function returns `{ valid: true }` or `{ valid: false, suggestedAction: ActionType, reason: string }`.

- [ ] **Step 1: Write failing tests for `canDispatchMode()`**

Add to `packages/daemon/src/state/__tests__/decision.test.ts`:

```typescript
describe("canDispatchMode", () => {
  // Helper to create minimal IssueState for testing
  function makeIssueState(overrides: Partial<IssueState>): IssueState {
    return {
      status: IssueStatus.RETRO,
      labels: ["worker-done"],
      hasPr: true,
      prIsDraft: false,
      ciStatus: null,
      mergeableStatus: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      suggestedAction: "dispatch_merger",
      sessionId: "ses_test123",
      hasUserFeedback: false,
      isBlocked: false,
      source: null,
      ...overrides,
    };
  }

  // --- Gated mode: merge ---

  it("allows merge when suggestedAction is dispatch_merger", () => {
    const state = makeIssueState({ suggestedAction: "dispatch_merger" });
    const result = canDispatchMode(state, "merge");
    expect(result.valid).toBe(true);
  });

  it("rejects merge when suggestedAction is transition_to_retro", () => {
    const state = makeIssueState({
      status: IssueStatus.NEEDS_REVIEW,
      suggestedAction: "transition_to_retro",
    });
    const result = canDispatchMode(state, "merge");
    expect(result.valid).toBe(false);
    if (!result.valid) {
      expect(result.suggestedAction).toBe("transition_to_retro");
      expect(result.reason).toContain("merge");
    }
  });

  it("rejects merge when suggestedAction is dispatch_implementer_for_retro", () => {
    const state = makeIssueState({
      status: IssueStatus.RETRO,
      suggestedAction: "dispatch_implementer_for_retro",
      labels: [],
    });
    const result = canDispatchMode(state, "merge");
    expect(result.valid).toBe(false);
  });

  it("rejects merge when suggestedAction is skip", () => {
    const state = makeIssueState({
      suggestedAction: "skip",
      hasLiveWorker: true,
    });
    const result = canDispatchMode(state, "merge");
    expect(result.valid).toBe(false);
  });

  // --- Cache miss (undefined state) ---

  it("allows merge when cachedState is undefined (cache miss)", () => {
    const result = canDispatchMode(undefined, "merge");
    expect(result.valid).toBe(true);
  });

  // --- Non-gated modes (backward compatible) ---

  it("allows architect regardless of cached state", () => {
    const state = makeIssueState({ suggestedAction: "skip" });
    const result = canDispatchMode(state, "architect");
    expect(result.valid).toBe(true);
  });

  it("allows plan regardless of cached state", () => {
    const state = makeIssueState({ suggestedAction: "dispatch_merger" });
    const result = canDispatchMode(state, "plan");
    expect(result.valid).toBe(true);
  });

  it("allows implement regardless of cached state", () => {
    const state = makeIssueState({ suggestedAction: "skip" });
    const result = canDispatchMode(state, "implement");
    expect(result.valid).toBe(true);
  });

  it("allows test regardless of cached state", () => {
    const state = makeIssueState({ suggestedAction: "skip" });
    const result = canDispatchMode(state, "test");
    expect(result.valid).toBe(true);
  });

  it("allows review regardless of cached state", () => {
    const state = makeIssueState({ suggestedAction: "skip" });
    const result = canDispatchMode(state, "review");
    expect(result.valid).toBe(true);
  });

  it("allows non-gated mode when cachedState is undefined", () => {
    const result = canDispatchMode(undefined, "implement");
    expect(result.valid).toBe(true);
  });

  // --- Error message content ---

  it("includes attempted mode and suggestedAction in rejection reason", () => {
    const state = makeIssueState({
      status: IssueStatus.NEEDS_REVIEW,
      suggestedAction: "transition_to_retro",
    });
    const result = canDispatchMode(state, "merge");
    expect(result.valid).toBe(false);
    if (!result.valid) {
      expect(result.reason).toContain("merge");
      expect(result.reason).toContain("transition_to_retro");
    }
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts --filter "canDispatchMode"`
Expected: FAIL — `canDispatchMode` is not exported from decision.ts

- [ ] **Step 3: Implement `canDispatchMode()` in decision.ts**

Add after the existing `ACTION_TO_MODE` map (after line 180):

```typescript
/**
 * Modes that require server-side validation before dispatch.
 * Non-gated modes are always allowed (backward compatible).
 */
export const GATED_MODES: ReadonlySet<WorkerModeLiteral> = new Set<WorkerModeLiteral>([
  WorkerMode.MERGE,
]);

/**
 * Actions that represent actual worker dispatch (not transitions/resumes/skips).
 */
const DISPATCH_ACTIONS: ReadonlySet<string> = new Set([
  "dispatch_architect",
  "dispatch_planner",
  "dispatch_implementer",
  "dispatch_implementer_for_retro",
  "dispatch_tester",
  "dispatch_reviewer",
  "dispatch_merger",
]);

export type DispatchValidation =
  | { valid: true }
  | { valid: false; suggestedAction: ActionType; reason: string };

/**
 * Validate whether a worker mode can be dispatched given cached issue state.
 *
 * For gated modes (initially: merge), the cached suggestedAction must be a
 * dispatch action that maps to the requested mode. This prevents the controller
 * from bypassing lifecycle phases (e.g., skipping retro before merge).
 *
 * For non-gated modes, always returns valid (backward compatible).
 * For cache misses (undefined state), always returns valid (graceful degradation).
 */
export function canDispatchMode(
  cachedState: IssueState | undefined,
  requestedMode: WorkerModeLiteral,
): DispatchValidation {
  if (!GATED_MODES.has(requestedMode)) {
    return { valid: true };
  }
  if (cachedState === undefined) {
    return { valid: true };
  }

  const { suggestedAction } = cachedState;
  const suggestedMode = ACTION_TO_MODE[suggestedAction];

  if (DISPATCH_ACTIONS.has(suggestedAction) && suggestedMode === requestedMode) {
    return { valid: true };
  }

  return {
    valid: false,
    suggestedAction,
    reason:
      `Cannot dispatch "${requestedMode}" worker: ` +
      `current suggestedAction is "${suggestedAction}" ` +
      `(maps to "${suggestedMode}" mode). ` +
      `The issue must reach the correct lifecycle state before "${requestedMode}" can be dispatched.`,
  };
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts --filter "canDispatchMode"`
Expected: All tests pass

- [ ] **Step 5: Run full decision.test.ts to check for regressions**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: All existing tests still pass, new tests pass

- [ ] **Step 6: Describe and advance**

```bash
jj describe -m "feat: add canDispatchMode() pure function for lifecycle enforcement (#203)"
jj new
```

---

### Task 2: Add state cache + dispatch validation to server.ts — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts`
- Test: `packages/daemon/src/daemon/__tests__/server.test.ts`

#### Design

1. Add `issueStateCache: Map<string, IssueState>` alongside the existing `workers` map inside `startServer()`.
2. After `buildCollectedState()` in both `/state/collect` and `/state/fetch-and-collect`, iterate over `state.issues` and populate the cache.
3. In `POST /workers`, after existing validation but before creating the session:
   - Look up `issueStateCache.get(normalizedIssueId)`
   - If `force` flag is set in payload, skip validation (log a warning)
   - Call `canDispatchMode(cachedState, mode)`
   - If invalid → return 422 with structured error

- [ ] **Step 1: Write failing tests for state cache population**

Add to `packages/daemon/src/daemon/__tests__/server.test.ts`:

```typescript
describe("dispatch validation", () => {
  it("rejects gated mode dispatch when cache says wrong phase", async () => {
    await startTestServer();

    // Mock fetch for the /state/collect call to populate cache
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      // Let daemon-internal fetches (workers endpoint) pass through
      if (url.includes(baseUrl)) {
        return originalFetch(input, init);
      }
      // Mock the enrichParsedIssues calls
      return new Response(JSON.stringify([]), { status: 200 });
    }) as typeof fetch;

    // Populate cache with a Needs Review issue (suggestedAction: transition_to_retro)
    await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({
        backend: "github",
        issues: {
          items: [
            {
              content: {
                number: 42,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/42",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        },
    });

    // Try to dispatch merge — should be rejected
    const res = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "test-repo-42",
        mode: "merge",
        workspace: "/tmp/work",
      }),
    });

    expect(res.status).toBe(422);
    const body = (await res.json()) as { error: string; suggestedAction: string };
    expect(body.error).toBe("phase_prerequisite_unmet");
    expect(body.suggestedAction).toBeTruthy();
  });

  it("allows gated mode dispatch when cache says correct phase", async () => {
    await startTestServer();

    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes(baseUrl)) {
        return originalFetch(input, init);
      }
      return new Response(JSON.stringify([]), { status: 200 });
    }) as typeof fetch;

    // Populate cache with a Retro + worker-done issue (suggestedAction: dispatch_merger)
    await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({
        backend: "github",
        issues: {
          items: [
            {
              content: {
                number: 43,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/43",
                type: "Issue",
              },
              status: "Retro",
              labels: ["worker-done"],
            },
          ],
        },
    });

    // Dispatch merge — should succeed
    const res = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "test-repo-43",
        mode: "merge",
        workspace: "/tmp/work",
      }),
    });

    expect(res.status).toBe(200);
  });

  it("allows gated mode dispatch on cache miss (no prior collect)", async () => {
    await startTestServer();

    const res = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "unknown-issue-99",
        mode: "merge",
        workspace: "/tmp/work",
      }),
    });

    expect(res.status).toBe(200);
  });

  it("allows non-gated modes regardless of cached state", async () => {
    await startTestServer();

    // Don't populate cache — dispatch non-gated mode
    const res = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "test-issue-1",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });

    expect(res.status).toBe(200);
  });

  it("bypasses validation when force flag is set", async () => {
    await startTestServer();

    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes(baseUrl)) {
        return originalFetch(input, init);
      }
      return new Response(JSON.stringify([]), { status: 200 });
    }) as typeof fetch;

    // Populate cache with wrong phase for merge
    await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({
        backend: "github",
        issues: {
          items: [
            {
              content: {
                number: 44,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/44",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        },
    });

    // Force dispatch merge — should succeed
    const res = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "test-repo-44",
        mode: "merge",
        workspace: "/tmp/work",
        force: true,
      }),
    });

    expect(res.status).toBe(200);
  });

  it("422 error includes attempted mode and suggestedAction", async () => {
    await startTestServer();

    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes(baseUrl)) {
        return originalFetch(input, init);
      }
      return new Response(JSON.stringify([]), { status: 200 });
    }) as typeof fetch;

    // Populate cache
    await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({
        backend: "github",
        issues: {
          items: [
            {
              content: {
                number: 45,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/45",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        },
    });

    const res = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "test-repo-45",
        mode: "merge",
        workspace: "/tmp/work",
      }),
    });

    expect(res.status).toBe(422);
    const body = (await res.json()) as {
      error: string;
      attemptedMode: string;
      suggestedAction: string;
      reason: string;
    };
    expect(body.error).toBe("phase_prerequisite_unmet");
    expect(body.attemptedMode).toBe("merge");
    expect(body.suggestedAction).toBeTruthy();
    expect(body.reason).toBeTruthy();
  });

  // --- Cache overwrite (stale cache replaced by fresh collect) ---

  it("updates cache on subsequent collect calls (latest wins)", async () => {
    await startTestServer();

    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes(baseUrl)) {
        return originalFetch(input, init);
      }
      return new Response(JSON.stringify([]), { status: 200 });
    }) as typeof fetch;

    // First collect: Needs Review + worker-done → transition_to_retro
    await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({
        backend: "github",
        issues: {
          items: [
            {
              content: {
                number: 50,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/50",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        },
      }),
    });

    // merge should fail with stale cache
    const res1 = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "test-repo-50",
        mode: "merge",
        workspace: "/tmp/work",
      }),
    });
    expect(res1.status).toBe(422);

    // Second collect: Retro + worker-done → dispatch_merger
    await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({
        backend: "github",
        issues: {
          items: [
            {
              content: {
                number: 50,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/50",
                type: "Issue",
              },
              status: "Retro",
              labels: ["worker-done"],
            },
          ],
        },
    });

    // merge should now succeed with updated cache
    const res2 = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "test-repo-50",
        mode: "merge",
        workspace: "/tmp/work",
      }),
    });
    expect(res2.status).toBe(200);
  });

  // --- Issue ID normalization ---

  it("cache lookup uses normalized issue ID (case insensitive)", async () => {
    await startTestServer();

    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes(baseUrl)) {
        return originalFetch(input, init);
      }
      return new Response(JSON.stringify([]), { status: 200 });
    }) as typeof fetch;

    // Populate cache — issue ID from GitHub will be lowercase
    await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({
        backend: "github",
        issues: {
          items: [
            {
              content: {
                number: 51,
                repository: "Test/Repo",
                url: "https://github.com/Test/Repo/issues/51",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        },
    });

    // Dispatch with mixed-case issueId — should still hit cache and reject
    const res = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "Test-Repo-51",
        mode: "merge",
        workspace: "/tmp/work",
      }),
    });

    expect(res.status).toBe(422);
  });

  // --- Version does not bypass validation ---

  it("version parameter does not bypass dispatch validation", async () => {
    await startTestServer();

    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes(baseUrl)) {
        return originalFetch(input, init);
      }
      return new Response(JSON.stringify([]), { status: 200 });
    }) as typeof fetch;

    // Populate cache with wrong phase
    await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({
        backend: "github",
        issues: {
          items: [
            {
              content: {
                number: 52,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/52",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        },
    });

    // Dispatch with version — should still be rejected
    const res = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "test-repo-52",
        mode: "merge",
        workspace: "/tmp/work",
        version: 5,
      }),
    });

    expect(res.status).toBe(422);
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "dispatch validation"`
Expected: FAIL — no 422 response, no issueStateCache

- [ ] **Step 3: Implement state cache + validation in server.ts**

**3a. Add import** at the top of server.ts — add `canDispatchMode` and `IssueState` (the type) to the imports from the state module:

Update the import from `"../state/decision"`:
```typescript
import { buildCollectedState, canDispatchMode } from "../state/decision";
```

Update the import from `"../state/types"`:
```typescript
import {
  CollectedState,
  computeSessionId,
  type IssueState,
  WorkerMode,
  type WorkerModeLiteral,
} from "../state/types";
```

**3b. Add cache** inside `startServer()`, after the existing `crashHistory` map (around line 166):

```typescript
const issueStateCache = new Map<string, IssueState>();
```

**3c. Populate cache** in `/state/collect` handler — after `const state = buildCollectedState(...)` and before the feedbackLogger block:

```typescript
// Populate dispatch validation cache
for (const [issueId, issueState] of Object.entries(state.issues)) {
  issueStateCache.set(issueId.toLowerCase(), issueState);
}
```

**3d. Populate cache** in `/state/fetch-and-collect` handler — same pattern, after `buildCollectedState`:

```typescript
// Populate dispatch validation cache
for (const [issueId, issueState] of Object.entries(state.issues)) {
  issueStateCache.set(issueId.toLowerCase(), issueState);
}
```

**3e. Validate dispatch** in `POST /workers` handler — after the `validModes` check (around line 320) and before the `resolvedWorkspace` logic:

```typescript
// Phase prerequisite validation for gated modes
const forceDispatch = payload.force === true;
if (!forceDispatch) {
  const cachedState = issueStateCache.get(normalizedIssueId);
  const validation = canDispatchMode(cachedState, mode as WorkerModeLiteral);
  if (!validation.valid) {
    return jsonResponse(
      {
        error: "phase_prerequisite_unmet",
        attemptedMode: mode,
        suggestedAction: validation.suggestedAction,
        reason: validation.reason,
      },
      422
    );
  }
} else {
  console.warn(
    `[dispatch] force=true for ${normalizedIssueId} mode=${mode} — skipping phase validation`
  );
}
```

Note: `normalizedIssueId` is computed earlier in the handler at `const normalizedIssueId = issueId.toLowerCase()`. The validation must be placed after that line but before the workspace resolution block.

- [ ] **Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "dispatch validation"`
Expected: All new tests pass

- [ ] **Step 5: Run full server test suite for regressions**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: All existing tests still pass (non-gated modes unaffected, no cache populated = cache miss = allowed)

- [ ] **Step 6: Describe and advance**

```bash
jj describe -m "feat: add state cache + dispatch validation to POST /workers (#203)"
jj new
```

---

### Task 3: Add `--force` flag to CLI dispatch command — Depends on: Task 2

**Files:**
- Modify: `packages/daemon/src/cli/index.ts`

#### Design

Add a `--force` boolean flag to the `dispatch` command. When set, it's passed as `force: true` in the POST /workers body, bypassing phase validation. Also handle the new 422 response status.

- [ ] **Step 1: Add `--force` flag to dispatchCommand args**

In `packages/daemon/src/cli/index.ts`, in the `dispatchCommand` args object (around line 808), add:

```typescript
force: {
  type: "boolean",
  alias: "f",
  description: "Bypass phase prerequisite validation (emergency use only)",
},
```

- [ ] **Step 2: Pass force flag through cmdDispatch**

In the `DispatchOptions` interface (search for it in the file), add:

```typescript
force?: boolean;
```

In `dispatchCommand`'s `run()` function, add to `dispatchOpts`:

```typescript
if (args.force) {
  dispatchOpts.force = true;
}
```

In `cmdDispatch()`, add to the `body` object:

```typescript
if (opts.force) {
  body.force = true;
}
```

- [ ] **Step 3: Handle 422 response in cmdDispatch**

After the existing 429 handler (around line 419), add:

```typescript
if (response.status === 422) {
  const detail = responseBody as {
    error: string;
    attemptedMode: string;
    suggestedAction: string;
    reason: string;
  };
  throw new CliError(
    `Phase prerequisite not met for "${detail.attemptedMode}":\n` +
      `  ${detail.reason}\n\n` +
      `To force dispatch: legion dispatch ${issue} ${mode} --force`,
  );
}
```

- [ ] **Step 4: Run lint to verify no issues**

Run: `bunx biome check packages/daemon/src/cli/index.ts`
Expected: No errors

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "feat: add --force flag to legion dispatch for phase validation bypass (#203)"
jj new
```

---

### Task 4: Add controller skill reinforcement note — Independent

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`

- [ ] **Step 1: Add note to Pipeline Integrity section**

In `.opencode/skills/legion-controller/SKILL.md`, after the "Pipeline Integrity" section's existing content (after line 272, before "### Role Boundary"), add:

```markdown
**Daemon enforcement:** The daemon validates lifecycle ordering on `POST /workers`. Dispatching a gated mode (e.g., merge) when the issue hasn't reached the correct state returns 422. The controller should follow `suggestedAction` one step at a time — never construct multi-step dispatch pipelines. If the daemon rejects a dispatch, the issue needs to progress through intermediate phases first.
```

- [ ] **Step 2: Verify the change reads correctly in context**

Read: `.opencode/skills/legion-controller/SKILL.md` around the Pipeline Integrity section.
Expected: The new paragraph fits naturally after existing content.

- [ ] **Step 3: Describe and advance**

```bash
jj describe -m "docs: add daemon enforcement note to controller skill (#203)"
jj new
```

---

### Task 5: Run full test suite and lint — Depends on: Task 1, Task 2, Task 3, Task 4

- [ ] **Step 1: Run type checker**

Run: `bunx tsc --noEmit`
Expected: No type errors

- [ ] **Step 2: Run Biome lint**

Run: `bunx biome check packages/daemon/src/`
Expected: No lint errors

- [ ] **Step 3: Run full test suite**

Run: `bun test`
Expected: All tests pass (existing + new)

- [ ] **Step 4: Squash all commits and describe**

```bash
jj squash
jj describe -m "feat: enforce lifecycle phase ordering via POST /workers validation (#203)

Add canDispatchMode() pure function that validates dispatch requests against
cached issue state. The daemon caches IssueState per issue from /state/collect
and rejects gated mode dispatches (merge) that skip required lifecycle phases
(e.g., retro). Includes --force CLI escape hatch and controller skill docs."
```

---

## Testing Plan

### Setup
- `bun install` (dependencies should already be installed in workspace)

### Health Check
- `bunx tsc --noEmit` returns 0 (type checking)
- `bunx biome check packages/daemon/src/` returns 0 (lint)

### Verification Steps

1. **canDispatchMode rejects merge when not in correct phase**
   - Action: `bun test packages/daemon/src/state/__tests__/decision.test.ts --filter "canDispatchMode"`
   - Expected: All canDispatchMode tests pass
   - Tool: Bun test runner

2. **POST /workers returns 422 for gated mode in wrong phase**
   - Action: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "dispatch validation"`
   - Expected: All dispatch validation tests pass (including cache overwrite, normalization, version bypass)
   - Tool: Bun test runner

3. **No regressions in existing tests**
   - Action: `bun test`
   - Expected: All tests pass (existing + new)
   - Tool: Bun test runner

4. **Type checking and lint**
   - Action: `bunx tsc --noEmit && bunx biome check packages/daemon/src/`
   - Expected: Exit 0
   - Tool: tsc + Biome

5. **CLI handles 422 gracefully**
   - Action: Verify `--force` flag exists in dispatch command args, 422 handler in cmdDispatch
   - Expected: Code review confirmation
   - Tool: Manual code review

### Tools Needed
- Bun test runner for unit + integration tests
- Biome for linting
- tsc for type checking
