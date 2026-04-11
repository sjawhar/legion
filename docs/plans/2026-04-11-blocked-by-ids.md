# Plan: Full Blocking Issue IDs in State Collector

**Issue:** sjawhar-legion-114 (Phase 2)
**Date:** 2026-04-11
**Status:** Ready for review

## Summary

Extend the count-based `isBlocked: boolean` (from PR #402) to surface the actual blocking issue IDs. Add `blockedByIds: string[]` throughout the type pipeline, computed from the full `blockedBy(first: 50)` GraphQL connection filtered to OPEN blockers only. Display canonical blocker IDs in poll output.

## Prior Art (PR #402)

The existing pipeline already has:
- `isBlocked: boolean` on ParsedIssue, FetchedIssueData, IssueState, IssueStateDict
- `issueDependenciesSummary { blockedBy }` in GraphQL Issue fragments (both ORG_QUERY and USER_QUERY)
- `nodeToProjectItem()` computes `isBlocked` from count > 0
- `buildIssueState()` gates `dispatch_*` → `skip` when `isBlocked`
- Poll formatter shows "blocked" reason
- `createParsedIssue()` accepts `isBlocked: boolean = false`

## Architect Routing Hints

- **Complexity:** small
- **Skip retro:** yes
- **Concerns:** `buildIssueId()` is one-way (not reversible for display); `blockedBy` returns open+closed (filter to OPEN); ORG_QUERY/USER_QUERY must stay in sync; `issueDependenciesSummary` kept (removal is follow-on); dedup+sort for stable output.

## Tasks

### Task 1: Add `blockedByIds: string[]` to types and thread through pipeline — Independent

**Files:** `packages/daemon/src/state/types.ts`, `packages/daemon/src/state/fetch.ts`, `packages/daemon/src/state/decision.ts`

**1a. types.ts — Replace `isBlocked` parameter with `blockedByIds` on `createParsedIssue()`:**

Change the `isBlocked: boolean = false` parameter to `blockedByIds: string[] = []`:
```typescript
export function createParsedIssue(
  issueId: string,
  status: IssueStatusLiteral | string,
  labels: string[],
  prRef: GitHubPRRef | null,
  source: IssueSource | null = null,
  blockedByIds: string[] = []  // CHANGED from isBlocked: boolean = false
): ParsedIssue {
  return {
    issueId,
    status,
    labels,
    prRef,
    source,
    blockedByIds,                        // NEW
    isBlocked: blockedByIds.length > 0,  // CHANGED — computed from array
    // ... existing getters unchanged
  };
}
```

**1b. types.ts — Add `blockedByIds` to `ParsedIssue` interface (before `isBlocked`):**
```typescript
export interface ParsedIssue {
  // ... existing fields
  blockedByIds: string[];  // NEW
  isBlocked: boolean;      // EXISTING — now computed from blockedByIds
  // ... existing getters
}
```

**1c. types.ts — Add `blockedByIds` to `FetchedIssueData` (before `isBlocked`):**
```typescript
export interface FetchedIssueData {
  // ... existing fields
  blockedByIds: string[];  // NEW
  isBlocked: boolean;      // EXISTING
}
```

**1d. types.ts — Add `blockedByIds` to `IssueState` and `IssueStateDict` (before `isBlocked`):**
```typescript
export interface IssueState {
  // ... existing fields
  blockedByIds: string[];  // NEW
  isBlocked: boolean;      // EXISTING
}

export interface IssueStateDict {
  // ... existing fields
  blockedByIds: string[];  // NEW
  isBlocked: boolean;      // EXISTING
}
```

**1e. types.ts — Update `IssueState.toDict()` to include `blockedByIds`:**
```typescript
toDict(state: IssueState): IssueStateDict {
  const dict: IssueStateDict = {
    // ... existing fields
    blockedByIds: state.blockedByIds,  // NEW — add before isBlocked
    isBlocked: state.isBlocked,
    // ...
  };
  return dict;
},
```

**1f. fetch.ts — Thread `blockedByIds` in `enrichParsedIssues()` (line ~589, add before `isBlocked`):**
```typescript
return {
  // ... existing fields
  blockedByIds: issue.blockedByIds,  // NEW
  isBlocked: issue.isBlocked,
  // ...
};
```

**1g. decision.ts — Thread `blockedByIds` in `buildIssueState()` return value (line ~322, add before `isBlocked`):**
```typescript
return {
  // ... existing fields
  blockedByIds: data.blockedByIds,  // NEW
  isBlocked: data.isBlocked ?? false,
  // ...
};
```

No logic changes in decision.ts — the `dispatch_*` gate (line ~291) still uses `data.isBlocked`.

**Verify:** `bunx tsc --noEmit` (will have errors in github.ts until Task 3 — that's expected)

### Task 2: Add `blockedBy(first: 50)` connection to GraphQL queries — Independent

**File:** `packages/daemon/src/state/github-fetch.ts`

**2a. Update `GitHubProjectItemNode` interface (around line 14) — add `blockedBy` to content union:**
```typescript
interface GitHubProjectItemNode {
  id: string;
  fieldValueByName: { name?: string } | null;
  labels: { nodes: Array<{ name: string }> };
  content:
    | {
        __typename: "Issue" | "PullRequest" | "DraftIssue";
        number?: number;
        title?: string;
        url?: string;
        repository?: { nameWithOwner: string };
        issueDependenciesSummary?: { blockedBy: number } | null;
        blockedBy?: {                              // NEW
          nodes: Array<{
            number: number;
            state: string;
            repository: { nameWithOwner: string };
          }>;
        } | null;
        linkedPullRequests?: { nodes: Array<{ url: string }> } | null;
      }
    | Record<string, never>;
}
```

**2b. Add `blockedBy(first: 50)` to Issue fragment in `ORG_QUERY` (after `issueDependenciesSummary` line ~101):**
```graphql
issueDependenciesSummary { blockedBy }
blockedBy(first: 50) {
  nodes {
    number
    state
    repository { nameWithOwner }
  }
}
```

**2c. Apply EXACT same change to `USER_QUERY`** (same position in the Issue fragment, line ~152).

**2d. Update `nodeToProjectItem()` to extract raw blocker references (replace `isBlocked` computation around line 224-225):**

Replace:
```typescript
isBlocked:
  typename === "Issue" ? (content.issueDependenciesSummary?.blockedBy ?? 0) > 0 : false,
```

With:
```typescript
isBlocked:
  typename === "Issue" ? (content.issueDependenciesSummary?.blockedBy ?? 0) > 0 : false,
blockerRefs:
  typename === "Issue" && content.blockedBy?.nodes
    ? content.blockedBy.nodes
        .filter((n) => n.state === "OPEN")
        .map((n) => ({
          number: n.number,
          repository: n.repository.nameWithOwner,
        }))
    : [],
```

**Verify:** `bunx tsc --noEmit`

### Task 3: Update GitHub backend parser to build `blockedByIds` — Depends on: Task 1, Task 2

**File:** `packages/daemon/src/state/backends/github.ts`

**3a. Update `GitHubProjectItem` interface — add `blockerRefs`:**
```typescript
interface GitHubProjectItem {
  // ... existing fields
  isBlocked?: boolean;
  blockerRefs?: Array<{ number: number; repository: string }>;  // NEW
  [key: string]: unknown;
}
```

**3b. In `parseIssues()`, build `blockedByIds` from `blockerRefs` (replace the `isBlocked` extraction around line 119):**

Replace:
```typescript
const isBlocked = typedItem.isBlocked === true;

parsed.push(createParsedIssue(issueId, status, labels, prRef, source, isBlocked));
```

With:
```typescript
// Build blockedByIds from raw blocker references
const blockedByIds: string[] = [];
if (Array.isArray(typedItem.blockerRefs)) {
  for (const ref of typedItem.blockerRefs) {
    if (typeof ref.number === "number" && typeof ref.repository === "string") {
      const blockerOwnerRepo = parseOwnerRepo(ref.repository);
      if (blockerOwnerRepo) {
        blockedByIds.push(
          buildIssueId(blockerOwnerRepo.owner, blockerOwnerRepo.repo, ref.number)
        );
      }
    }
  }
}
// Dedup and sort for stable output
const uniqueBlockedByIds = [...new Set(blockedByIds)].sort();

parsed.push(createParsedIssue(issueId, status, labels, prRef, source, uniqueBlockedByIds));
```

Note: `buildIssueId` and `parseOwnerRepo` are already available in this file (private functions).

**Verify:**
```bash
bunx tsc --noEmit
bun test packages/daemon/src/state/backends/__tests__/
```

### Task 4: Update poll formatter to display blocker IDs — Depends on: Task 1

**File:** `packages/daemon/src/cli/poll-formatter.ts`

**4a. Update `BlockedIssue` interface to include `blockedByIds`:**
```typescript
interface BlockedIssue {
  issueId: string;
  reason: string;
  blockedByIds: string[];  // NEW
  source: IssueSource | null;
}
```

**4b. Update `categorizeIssues()` — pass `blockedByIds` in the blocked issue check (line ~41-43):**

Replace:
```typescript
if (issue.isBlocked) {
  blocked.push({ issueId, reason: "blocked", source: issue.source });
  continue;
}
```

With:
```typescript
if (issue.isBlocked) {
  const blockerList = issue.blockedByIds?.join(", ") ?? "";
  const reason = blockerList ? `blocked by ${blockerList}` : "blocked";
  blocked.push({ issueId, reason, blockedByIds: issue.blockedByIds ?? [], source: issue.source });
  continue;
}
```

Also update the other `blocked.push()` calls to include `blockedByIds: []` for non-dependency-blocked items (user-input-needed, stale worker-active).

**Verify:**
```bash
bunx tsc --noEmit
bun test packages/daemon/src/cli/__tests__/poll-formatter.test.ts
```

### Task 5: Tests — Depends on: Tasks 1-4

**Files:** Test files in `__tests__/` for each affected module.

**5a. github-fetch.test.ts:**
- Add `blockedBy` connection to mock Issue nodes with mix of OPEN and CLOSED blockers
- Verify `blockerRefs` in output contains only OPEN blockers
- Verify items with no `blockedBy` connection get `blockerRefs: []`
- Verify PullRequest items get `blockerRefs: []`

**5b. backends/github.test.ts:**
- Item with `blockerRefs: [{ number: 110, repository: "sjawhar/legion" }, { number: 112, repository: "sjawhar/legion" }]` → `blockedByIds: ["sjawhar-legion-110", "sjawhar-legion-112"]`, `isBlocked: true`
- Item with `blockerRefs: []` → `blockedByIds: []`, `isBlocked: false`
- Item with `blockerRefs` missing → `blockedByIds: []`, `isBlocked: false`
- Verify dedup: duplicate blocker refs → single ID in output
- Verify sort: IDs appear in lexicographic order

**5c. decision.test.ts:**
- Verify `blockedByIds` threads through `buildIssueState()` into output
- Existing blocking tests continue to pass (dispatch_* gate unchanged)

**5d. poll-formatter.test.ts:**
- Blocked issue with `blockedByIds: ["sjawhar-legion-110", "sjawhar-legion-112"]` → output contains `blocked by sjawhar-legion-110, sjawhar-legion-112`
- Blocked issue with empty `blockedByIds` → output contains `blocked`
- Non-dependency blocked issues → reason unchanged ("user-input-needed", "worker-active (stale)")

**5e. End-to-end pipeline test:**
- Mock issue with 1 open blocker + 1 closed blocker → `blockedByIds` contains only open blocker ID, `isBlocked: true`, dispatch actions overridden to `skip`
- Mock issue with only closed blockers → `blockedByIds: []`, `isBlocked: false`, dispatch actions proceed normally

**Run full verification:**
```bash
bun test packages/daemon/src/state/
bun test packages/daemon/src/cli/
bunx tsc --noEmit
bunx biome check src/
bun test  # Full suite
```

### Dependency Graph

```
Task 1 (types + pipeline) ──┬──► Task 3 (backend parser) ──► Task 5 (tests)
                             ├──► Task 4 (poll formatter) ─┘
Task 2 (GraphQL + extract) ──┘
```

Tasks 1 and 2: Independent — can execute in parallel.
Task 3: Depends on Task 1 (new `createParsedIssue` signature) and Task 2 (new `blockerRefs` in intermediate format).
Task 4: Depends on Task 1 (new `blockedByIds` on `IssueStateDict`).
Task 5: Depends on all above.

## Testing Plan

### Setup
```bash
bun install
```

### Health Check
```bash
bunx tsc --noEmit  # Must exit 0
bunx biome check src/  # Must exit 0
```

### Verification Steps

1. **AC1: GraphQL queries fetch blocker references**
   - Action: Verify ORG_QUERY and USER_QUERY contain `blockedBy(first: 50) { nodes { number state repository { nameWithOwner } } }` in the Issue fragment
   - Expected: Both queries include the connection
   - Tool: Grep / test assertion

2. **AC2: Only open blockers included**
   - Action: `bun test --filter "open blockers"` or `bun test --filter "blockerRefs"`
   - Expected: Given issue with 2 open + 1 closed blocker → `blockerRefs` has length 2
   - Tool: Bun test runner

3. **AC3: Blocker references use buildIssueId**
   - Action: `bun test packages/daemon/src/state/backends/__tests__/`
   - Expected: Blocker with `repository: "sjawhar/legion"` and `number: 110` → `"sjawhar-legion-110"`, deduplicated and sorted
   - Tool: Bun test runner

4. **AC4-AC5: blockedByIds flows through pipeline, isBlocked computed**
   - Action: `bun test packages/daemon/src/state/`
   - Expected: `blockedByIds` present on ParsedIssue, FetchedIssueData, IssueState; `isBlocked === (blockedByIds.length > 0)`
   - Tool: Bun test runner

5. **AC6: Decision gate unchanged**
   - Action: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
   - Expected: Blocked issues still get dispatch_* overridden to skip. Non-dispatch actions unaffected.
   - Tool: Bun test runner

6. **AC7: Poll formatter displays blocker IDs verbatim**
   - Action: `bun test packages/daemon/src/cli/__tests__/poll-formatter.test.ts`
   - Expected: `#114  blocked by sjawhar-legion-110, sjawhar-legion-112` in BLOCKED section
   - Tool: Bun test runner

7. **AC8: Linear backend compatible**
   - Action: `bun test` (existing Linear tests)
   - Expected: All pass — `createParsedIssue()` default `blockedByIds: []` gives `isBlocked: false`
   - Tool: Bun test runner

8. **AC9: End-to-end pipeline contract**
   - Action: `bun test --filter "pipeline"` or `bun test --filter "end-to-end"`
   - Expected: Mock issue with 1 open + 1 closed blocker → correct `blockedByIds`, `isBlocked: true`, dispatch → skip
   - Tool: Bun test runner

9. **AC10: Full test suite**
   - Action: `bun test && bunx tsc --noEmit && bunx biome check src/`
   - Expected: All pass
   - Tool: Bun test runner + tsc + Biome

### Tools Needed
- Bun test runner
- TypeScript compiler (tsc)
- Biome linter
