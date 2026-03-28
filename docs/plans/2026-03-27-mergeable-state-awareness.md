# Mergeable State Awareness for PR Decision Logic

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the state machine distinguish "PR has conflicts but CI passed" from "CI checks failing" — and suggest auto-rebase instead of blocking.

**Architecture:** Add `mergeableStatus` as a first-class field alongside `ciStatus` and `prIsDraft`. Fetch it via the existing GraphQL batch query (zero additional API calls). Insert a mergeable check *before* the CI check in decision priority, so conflicts are resolved before blaming code. The controller handles the new `rebase_pr` action by calling GitHub's update-branch API.

**Tech Stack:** TypeScript / Bun, GitHub GraphQL API, `gh` CLI

---

## Context

PR#6570 sat unmergeable for 7+ hours because the state machine only tracks `prIsDraft` and `ciStatus`. When a PR has merge conflicts (`mergeable: CONFLICTING`) but the smoke test already passed, the state machine either:
- Returns `transition_to_retro` (if ciStatus is passing) — but the merger then discovers conflicts
- Returns `retry_ci_check` (if GitHub marks CI as stale/pending due to conflicts) — and loops forever

Neither path detects that a simple rebase would unblock the PR.

### Key Design Decisions

1. **Mergeable check comes before CI check** — Conflicts can cause CI failures. Rebase first, then re-evaluate CI.
2. **Single GraphQL call** — Add `mergeable` to the existing `getCiStatusBatch` query (no extra API round-trip).
3. **`mergeableStatus` parameter is optional with null default** — Preserves backward compatibility. All existing tests pass unchanged.
4. **Controller handles rebase, not a worker** — The controller already has autonomy to rebase branches. No new worker mode needed.

### Learnings Applied

- [Using PR draft status instead of labels | tags: github, graphql, pull-requests, state-machine] — Established the pattern of using native GitHub PR fields (like `isDraft`) instead of labels. We follow the same pattern with `mergeable`.
- [Controller session anti-patterns: polling waste | tags: controller, polling] — Polling loops are expensive. The new action should resolve the state (rebase), not just retry.
- [Worker smoke testing must use real builds | tags: smoke-testing, quality-gates] — The smoke test signal exists and is reliable. The issue is that the state machine ignores merge conflicts when smoke passes.

### Metis Pre-Analysis Findings

1. **"dirty" semantics** — GitHub GraphQL `mergeable` enum (`MERGEABLE | CONFLICTING | UNKNOWN`) is cleaner than REST `mergeable_state`. We use GraphQL exclusively.
2. **Aggregate CI vs specific smoke** — `statusCheckRollup.state` aggregates all checks. This is fine for our purposes — if all checks pass but PR is conflicting, we rebase.
3. **UNKNOWN state** — GitHub lazily computes mergeability. Treat `UNKNOWN` as "retry next iteration" (same as `prIsDraft === null`).

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `packages/daemon/src/state/types.ts` | Modify | Add `MergeableStatus` type, extend `FetchedIssueData`, `IssueState`, `ActionType` |
| `packages/daemon/src/state/fetch.ts` | Modify | Add `mergeable` to GraphQL query, update return types and mapping |
| `packages/daemon/src/state/decision.ts` | Modify | Add mergeableStatus to `suggestAction()` priority chain |
| `packages/daemon/src/state/__tests__/decision.test.ts` | Modify | Add tests for mergeable scenarios (existing tests unchanged) |
| `packages/daemon/src/state/__tests__/fetch.test.ts` | Modify | Add tests for mergeable field in GraphQL response parsing |
| `.opencode/skills/legion-controller/SKILL.md` | Modify | Document `rebase_pr` action and controller handling |

---

## Task 1: Add mergeable types and extend data structures

**Files:**
- Modify: `packages/daemon/src/state/types.ts`

- [ ] **Step 1: Add MergeableStatus type and constants**

After the `CiStatus` constant (around line 76), add:

```typescript
/**
 * PR merge conflict status.
 * - "mergeable": no conflicts, can be merged
 * - "conflicting": has merge conflicts, needs rebase
 * - "unknown": GitHub hasn't computed yet (lazy evaluation)
 * - null: no PR, couldn't check, or not applicable
 */
export type MergeableStatusLiteral = "mergeable" | "conflicting" | "unknown";

export const MergeableStatus = {
  MERGEABLE: "mergeable" as MergeableStatusLiteral,
  CONFLICTING: "conflicting" as MergeableStatusLiteral,
  UNKNOWN: "unknown" as MergeableStatusLiteral,
} as const;
```

- [ ] **Step 2: Add `rebase_pr` to ActionType union**

Add `"rebase_pr"` to the `ActionType` union type:

```typescript
export type ActionType =
  | "skip"
  // ... existing values ...
  | "retry_ci_check"
  | "rebase_pr";
```

- [ ] **Step 3: Add mergeableStatus to FetchedIssueData**

Add to the `FetchedIssueData` interface, after `ciStatus`:

```typescript
export interface FetchedIssueData {
  // ... existing fields ...
  ciStatus: CiStatusLiteral | null;
  mergeableStatus: MergeableStatusLiteral | null; // null if no PR or couldn't check
  // ... remaining fields ...
}
```

- [ ] **Step 4: Add mergeableStatus to IssueState and IssueStateDict**

Add `mergeableStatus: MergeableStatusLiteral | null` to both `IssueState` interface and `IssueStateDict` interface (after `ciStatus` in each).

Update `IssueState.toDict()` to include `mergeableStatus`:

```typescript
toDict(state: IssueState): IssueStateDict {
  const dict: IssueStateDict = {
    // ... existing fields ...
    ciStatus: state.ciStatus,
    mergeableStatus: state.mergeableStatus,
    // ... remaining fields ...
  };
  return dict;
}
```

- [ ] **Step 5: Verify types compile**

Run: `bunx tsc --noEmit`
Expected: Type errors in `fetch.ts` and `decision.ts` (they don't populate the new field yet). No errors in `types.ts`.

- [ ] **Step 6: Describe and advance**

```bash
jj describe -m "feat(state): add MergeableStatus type and rebase_pr action to type system"
jj new
```

**Depends on:** Nothing — Independent

---

## Task 2: Extend GraphQL fetching for mergeable status

**Files:**
- Modify: `packages/daemon/src/state/fetch.ts`

- [ ] **Step 1: Add mapMergeableState function**

After `mapCiRollupState()`, add:

```typescript
/**
 * Map GitHub GraphQL MergeableState enum to MergeableStatusLiteral.
 *
 * GitHub GraphQL PullRequest.mergeable values:
 * - MERGEABLE -> "mergeable"
 * - CONFLICTING -> "conflicting"
 * - UNKNOWN -> "unknown" (GitHub hasn't computed yet)
 * - null or unrecognized -> null
 */
export function mapMergeableState(state: string | null | undefined): MergeableStatusLiteral | null {
  if (state === null || state === undefined) {
    return null;
  }
  switch (state) {
    case "MERGEABLE":
      return MergeableStatus.MERGEABLE;
    case "CONFLICTING":
      return MergeableStatus.CONFLICTING;
    case "UNKNOWN":
      return MergeableStatus.UNKNOWN;
    default:
      return null;
  }
}
```

Add the imports for `MergeableStatus` and `MergeableStatusLiteral` from `./types`.

- [ ] **Step 2: Add `mergeable` to the CI status GraphQL query**

In `getCiStatusBatch()`, update the PR query fragment to also fetch `mergeable`:

Change:
```typescript
`${prAlias}: pullRequest(number: ${prNumber}) { commits(last: 1) { nodes { commit { statusCheckRollup { state } } } } }`
```

To:
```typescript
`${prAlias}: pullRequest(number: ${prNumber}) { mergeable commits(last: 1) { nodes { commit { statusCheckRollup { state } } } } }`
```

- [ ] **Step 3: Update getCiStatusBatch return type**

Change the return type from `Record<string, CiStatusLiteral | null>` to:

```typescript
interface CiAndMergeStatus {
  ciStatus: CiStatusLiteral | null;
  mergeableStatus: MergeableStatusLiteral | null;
}

export async function getCiStatusBatch(
  prRefs: Record<string, GitHubPRRefType>,
  runner: CommandRunner = defaultRunner
): Promise<Record<string, CiAndMergeStatus>> {
```

- [ ] **Step 4: Update response parsing in getCiStatusBatch**

Update all `result[issueId] = ...` assignments to use the new shape:

Where CI status was parsed:
```typescript
result[issueId] = {
  ciStatus: mapCiRollupState(rollupState),
  mergeableStatus: mapMergeableState(
    typeof rawPr === "object" && rawPr !== null && "mergeable" in rawPr
      ? (rawPr as { mergeable?: string | null }).mergeable ?? null
      : null
  ),
};
```

Where null was returned:
```typescript
result[issueId] = { ciStatus: null, mergeableStatus: null };
```

- [ ] **Step 5: Update enrichParsedIssues to use new return type**

In `enrichParsedIssues()`:

Change variable:
```typescript
let ciAndMergeMap: Record<string, CiAndMergeStatus> = {};
```

Update fetch:
```typescript
ciAndMergeMap = await getCiStatusBatch(ciRefsForStatus, runner);
```

Update error fallback:
```typescript
for (const issueId of Object.keys(ciRefsForStatus)) {
  ciAndMergeMap[issueId] = { ciStatus: null, mergeableStatus: null };
}
```

Update FetchedIssueData construction:
```typescript
ciStatus: ciAndMergeMap[issue.issueId]?.ciStatus ?? null,
mergeableStatus: ciAndMergeMap[issue.issueId]?.mergeableStatus ?? null,
```

- [ ] **Step 6: Verify types compile**

Run: `bunx tsc --noEmit`
Expected: Errors in `decision.ts` (doesn't pass mergeableStatus yet) and possibly tests. No errors in `fetch.ts`.

- [ ] **Step 7: Describe and advance**

```bash
jj describe -m "feat(state): fetch mergeable status alongside CI status in single GraphQL query"
jj new
```

**Depends on:** Task 1

---

## Task 3: Update decision logic for mergeable awareness

**Files:**
- Modify: `packages/daemon/src/state/decision.ts`

- [ ] **Step 1: Add mergeableStatus parameter to suggestAction**

Update the function signature (add as last parameter with null default):

```typescript
export function suggestAction(
  status: IssueStatusLiteral | string,
  hasWorkerDone: boolean,
  hasLiveWorker: boolean,
  prIsDraft: boolean | null,
  hasPr: boolean,
  hasTestPassed: boolean,
  ciStatus: CiStatusLiteral | null = null,
  mergeableStatus: MergeableStatusLiteral | null = null
): ActionType {
```

Add imports for `MergeableStatus` and `MergeableStatusLiteral` from `./types`.

- [ ] **Step 2: Insert mergeable check in NEEDS_REVIEW worker-done path**

In the `case IssueStatus.NEEDS_REVIEW:` block, after the `prIsDraft` checks and BEFORE the `ciStatus` checks, insert:

```typescript
    // Check merge conflicts before CI status.
    // Conflicts can cause CI failures, so resolve them first.
    if (mergeableStatus === MergeableStatus.UNKNOWN) {
      return "retry_pr_check";
    }
    if (mergeableStatus === MergeableStatus.CONFLICTING) {
      return "rebase_pr";
    }
```

The full NEEDS_REVIEW worker-done path becomes:
1. `!hasPr` -> investigate_no_pr
2. `prIsDraft === null` -> retry_pr_check
3. `prIsDraft` -> resume_implementer_for_changes
4. `mergeableStatus === UNKNOWN` -> retry_pr_check (NEW)
5. `mergeableStatus === CONFLICTING` -> rebase_pr (NEW)
6. `ciStatus === FAILING` -> resume_implementer_for_ci_failure
7. `ciStatus === PENDING` -> retry_ci_check
8. -> transition_to_retro

- [ ] **Step 3: Insert mergeable check in NEEDS_REVIEW no-worker-done path**

In the non-worker-done section, add before the CI checks:

```typescript
  if (hasPr && mergeableStatus === MergeableStatus.CONFLICTING) {
    return "rebase_pr";
  }
```

- [ ] **Step 4: Add rebase_pr to ACTION_TO_MODE mapping**

```typescript
rebase_pr: WorkerMode.REVIEW,
```

- [ ] **Step 5: Update buildIssueState to pass mergeableStatus**

Update the `suggestAction` call:
```typescript
action = suggestAction(
  data.status,
  data.labels.includes("worker-done"),
  data.hasLiveWorker,
  data.prIsDraft,
  data.hasPr,
  data.hasTestPassed ?? false,
  data.ciStatus,
  data.mergeableStatus
);
```

Include `mergeableStatus` in the returned IssueState.

- [ ] **Step 6: Verify types compile**

Run: `bunx tsc --noEmit`
Expected: Clean (zero errors).

- [ ] **Step 7: Describe and advance**

```bash
jj describe -m "feat(state): add mergeable-aware decision logic with rebase_pr action"
jj new
```

**Depends on:** Task 1, Task 2

---

## Task 4: Add decision tests for mergeable scenarios

**Files:**
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts`

- [ ] **Step 1: Add tests for NEEDS_REVIEW + worker-done + mergeable scenarios**

Add to the existing `describe("suggestAction", ...)` block. Import `MergeableStatus` and `CiStatus` from `../types`.

```typescript
it("needs_review_worker_done_conflicting_pr_rebases", () => {
  const action = suggestAction(
    IssueStatus.NEEDS_REVIEW, true, false, false, true, false,
    CiStatus.PASSING, MergeableStatus.CONFLICTING
  );
  expect(action).toBe("rebase_pr");
});

it("needs_review_worker_done_conflicting_pr_with_failing_ci_still_rebases", () => {
  const action = suggestAction(
    IssueStatus.NEEDS_REVIEW, true, false, false, true, false,
    CiStatus.FAILING, MergeableStatus.CONFLICTING
  );
  expect(action).toBe("rebase_pr");
});

it("needs_review_worker_done_conflicting_pr_with_pending_ci_still_rebases", () => {
  const action = suggestAction(
    IssueStatus.NEEDS_REVIEW, true, false, false, true, false,
    CiStatus.PENDING, MergeableStatus.CONFLICTING
  );
  expect(action).toBe("rebase_pr");
});

it("needs_review_worker_done_unknown_mergeable_retries", () => {
  const action = suggestAction(
    IssueStatus.NEEDS_REVIEW, true, false, false, true, false,
    CiStatus.PASSING, MergeableStatus.UNKNOWN
  );
  expect(action).toBe("retry_pr_check");
});

it("needs_review_worker_done_mergeable_pr_with_passing_ci_transitions", () => {
  const action = suggestAction(
    IssueStatus.NEEDS_REVIEW, true, false, false, true, false,
    CiStatus.PASSING, MergeableStatus.MERGEABLE
  );
  expect(action).toBe("transition_to_retro");
});

it("needs_review_worker_done_null_mergeable_falls_through_to_ci", () => {
  const action = suggestAction(
    IssueStatus.NEEDS_REVIEW, true, false, false, true, false,
    CiStatus.FAILING, null
  );
  expect(action).toBe("resume_implementer_for_ci_failure");
});
```

- [ ] **Step 2: Add tests for NEEDS_REVIEW no-worker-done + mergeable**

```typescript
it("needs_review_no_worker_done_conflicting_pr_rebases", () => {
  const action = suggestAction(
    IssueStatus.NEEDS_REVIEW, false, false, false, true, false,
    CiStatus.PASSING, MergeableStatus.CONFLICTING
  );
  expect(action).toBe("rebase_pr");
});

it("needs_review_no_worker_done_mergeable_pr_dispatches_reviewer", () => {
  const action = suggestAction(
    IssueStatus.NEEDS_REVIEW, false, false, false, true, false,
    CiStatus.PASSING, MergeableStatus.MERGEABLE
  );
  expect(action).toBe("dispatch_reviewer");
});
```

- [ ] **Step 3: Add ACTION_TO_MODE and buildIssueState tests**

```typescript
it("rebase_pr maps to review mode", () => {
  expect(ACTION_TO_MODE["rebase_pr"]).toBe("review");
});
```

Add a `buildIssueState` test with `mergeableStatus: MergeableStatus.CONFLICTING` verifying `suggestedAction: "rebase_pr"`.

- [ ] **Step 4: Run tests**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: All tests pass.

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "test(state): add decision tests for mergeable-aware rebase_pr action"
jj new
```

**Depends on:** Task 3

---

## Task 5: Add fetch tests for mergeable field

**Files:**
- Modify: `packages/daemon/src/state/__tests__/fetch.test.ts`

- [ ] **Step 1: Read existing fetch test patterns**

Read `packages/daemon/src/state/__tests__/fetch.test.ts` to understand mock runner patterns.

- [ ] **Step 2: Add mapMergeableState unit tests**

```typescript
describe("mapMergeableState", () => {
  it("maps MERGEABLE to mergeable", () => {
    expect(mapMergeableState("MERGEABLE")).toBe("mergeable");
  });
  it("maps CONFLICTING to conflicting", () => {
    expect(mapMergeableState("CONFLICTING")).toBe("conflicting");
  });
  it("maps UNKNOWN to unknown", () => {
    expect(mapMergeableState("UNKNOWN")).toBe("unknown");
  });
  it("maps null to null", () => {
    expect(mapMergeableState(null)).toBeNull();
  });
  it("maps undefined to null", () => {
    expect(mapMergeableState(undefined)).toBeNull();
  });
  it("maps unrecognized value to null", () => {
    expect(mapMergeableState("INVALID")).toBeNull();
  });
});
```

- [ ] **Step 3: Add getCiStatusBatch tests with mergeable field**

Test that the GraphQL response parsing includes `mergeableStatus` alongside `ciStatus`. Use the mock runner pattern from existing tests. Test both present and absent `mergeable` field.

- [ ] **Step 4: Update existing getCiStatusBatch tests**

Change assertions from `result["id"]` to `result["id"].ciStatus` to match new return shape.

- [ ] **Step 5: Run tests**

Run: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
Expected: All tests pass.

- [ ] **Step 6: Describe and advance**

```bash
jj describe -m "test(state): add fetch tests for mergeable GraphQL field and mapping"
jj new
```

**Depends on:** Task 2

---

## Task 6: Update controller skill documentation

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`

- [ ] **Step 1: Add rebase_pr to the routing table**

In "Routing by Action Intent" table, add:
```
| `rebase_` | Auto-rebase PR branch | Use GitHub update-branch API, re-check next iteration |
```

- [ ] **Step 2: Add rebase_pr handling documentation**

After the `retry_pr_check` documentation, add a paragraph explaining the `rebase_pr` action: the PR has conflicts but CI passed, controller should call GitHub's update-branch API (`gh api repos/$OWNER/$REPO/pulls/$PR_NUMBER/update-branch -X PUT`), and fall back to `resume_implementer_for_changes` if the API fails.

- [ ] **Step 3: Update the signals table**

Add: `| rebase_pr | mergeableStatus: "conflicting" | PR has conflicts; auto-rebase via GitHub API |`

- [ ] **Step 4: Document mergeableStatus in collected state output**

Note that `/state/collect` now returns `mergeableStatus` alongside `ciStatus`.

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "docs(controller): document rebase_pr action and mergeable state handling"
jj new
```

**Depends on:** Nothing — Independent

---

## Task 7: Full verification

- [ ] **Step 1: Type check** — `bunx tsc --noEmit` — expect zero errors
- [ ] **Step 2: Lint** — `bunx biome check packages/daemon/src/` — expect clean
- [ ] **Step 3: Full test suite** — `bun test` — expect all pass
- [ ] **Step 4: Final commit**

```bash
jj squash
jj describe -m "feat(state): distinguish merge conflicts from CI failures with rebase_pr action

Add mergeableStatus as a first-class field in the state machine. When a PR
has merge conflicts but CI has passed (or conflicts may be causing CI failures),
the state machine now suggests rebase_pr instead of blocking. The controller
handles this by calling GitHub's update-branch API.

Closes #136"
```

**Depends on:** Tasks 1-6

---

## Testing Plan

### Setup
```bash
bun install
```

### Health Check
```bash
bunx tsc --noEmit
bun test
```

### Verification Steps

1. **Type safety** — `bunx tsc --noEmit` — expect zero errors
2. **Conflicting PR with passing CI returns rebase_pr** — `bun test packages/daemon/src/state/__tests__/decision.test.ts --filter "conflicting"` — PASS
3. **Unknown mergeable retries** — `bun test packages/daemon/src/state/__tests__/decision.test.ts --filter "unknown_mergeable"` — PASS
4. **Null mergeableStatus backward compat** — `bun test packages/daemon/src/state/__tests__/decision.test.ts --filter "null_mergeable"` — PASS
5. **GraphQL response parsing** — `bun test packages/daemon/src/state/__tests__/fetch.test.ts --filter "mergeableStatus"` — PASS
6. **All existing tests unchanged** — `bun test packages/daemon/src/state/` — all pass
7. **Lint** — `bunx biome check packages/daemon/src/state/` — clean

### Tools Needed
- Bun test runner
- TypeScript compiler
- Biome linter
