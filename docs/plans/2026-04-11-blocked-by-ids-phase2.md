# Plan: Full Blocking Issue IDs in State Collector (Phase 2)

**Issue:** sjawhar-legion-114
**Date:** 2026-04-11
**Status:** Ready for review
**Prior work:** PR #402 added count-based `isBlocked: boolean` via `issueDependenciesSummary.blockedBy`. This plan extends to full `blockedByIds: string[]`.

## Summary

Replace the count-based `isBlocked` boolean with a full `blockedByIds: string[]` array containing the actual Legion issue IDs of open blockers. Compute `isBlocked` from `blockedByIds.length > 0`. Display blocker IDs in poll output.

## Tasks

### Task 1: Add `blockedByIds` to type definitions — Independent

**File:** `packages/daemon/src/state/types.ts`

**1a. Update `ParsedIssue` interface** — add `blockedByIds: string[]` (after existing `isBlocked: boolean` on line 297):
```typescript
  blockedByIds: string[];
  isBlocked: boolean;
```
`isBlocked` stays but becomes computed from `blockedByIds` in `createParsedIssue`.

**1b. Update `createParsedIssue()`** — replace the `isBlocked: boolean = false` parameter with `blockedByIds: string[] = []`:

Current signature (line 316-323):
```typescript
export function createParsedIssue(
  issueId: string,
  status: IssueStatusLiteral | string,
  labels: string[],
  prRef: GitHubPRRef | null,
  source: IssueSource | null = null,
  isBlocked: boolean = false
): ParsedIssue {
```

New signature:
```typescript
export function createParsedIssue(
  issueId: string,
  status: IssueStatusLiteral | string,
  labels: string[],
  prRef: GitHubPRRef | null,
  source: IssueSource | null = null,
  blockedByIds: string[] = []
): ParsedIssue {
```

In the return object, replace `isBlocked,` with:
```typescript
    blockedByIds,
    isBlocked: blockedByIds.length > 0,
```

**1c. Update `FetchedIssueData`** — add `blockedByIds: string[]` (before existing `isBlocked: boolean`):
```typescript
  blockedByIds: string[];
  isBlocked: boolean;
```

**1d. Update `IssueState`** — add `blockedByIds: string[]` (before existing `isBlocked: boolean`):
```typescript
  blockedByIds: string[];
  isBlocked: boolean;
```

**1e. Update `IssueStateDict`** — add `blockedByIds: string[]` (before existing `isBlocked: boolean`):
```typescript
  blockedByIds: string[];
  isBlocked: boolean;
```

**1f. Update `IssueState.toDict()`** — add `blockedByIds` to the dict (before `isBlocked`):
```typescript
    blockedByIds: state.blockedByIds,
    isBlocked: state.isBlocked,
```

**Verify:** `bunx tsc --noEmit` — expect type errors in downstream files (backends/github.ts, fetch.ts, decision.ts) since they still pass `isBlocked` directly. These are fixed in Tasks 3 and 4.

### Task 2: Add `blockedBy` connection to GraphQL queries — Independent

**File:** `packages/daemon/src/state/github-fetch.ts`

**2a. Update `GitHubProjectItemNode` interface** (line ~14-36) — add `blockedBy` to the Issue content branch, after `issueDependenciesSummary`:
```typescript
issueDependenciesSummary?: { blockedBy: number } | null;
blockedBy?: {
  nodes: Array<{
    number: number;
    state: string;
    repository?: { nameWithOwner: string } | null;
  }>;
} | null;
```

**2b. Update `ORG_QUERY`** — add `blockedBy(first: 50)` to the Issue fragment (after `issueDependenciesSummary { blockedBy }` on line 101):
```graphql
... on Issue {
  number
  title
  url
  repository { nameWithOwner }
  issueDependenciesSummary { blockedBy }
  blockedBy(first: 50) {
    nodes {
      number
      state
      repository { nameWithOwner }
    }
  }
  linkedPullRequests: closedByPullRequestsReferences(first: 10, includeClosedPrs: true) {
    nodes { url }
  }
}
```

**2c. Apply the exact same change to `USER_QUERY`** — same Issue fragment, same `blockedBy(first: 50)` block.

**2d. Update `nodeToProjectItem()`** — extract blocker references as raw data. Replace the current `isBlocked` computation (lines 224-225) with:
```typescript
    // Extract blocker references (raw — IDs built by backend parser)
    const blockerRefs: Array<{ number: number; state: string; repository: string | null }> = [];
    if (typename === "Issue" && content.blockedBy?.nodes) {
      for (const blocker of content.blockedBy.nodes) {
        if (typeof blocker.number === "number" && typeof blocker.state === "string") {
          blockerRefs.push({
            number: blocker.number,
            state: blocker.state,
            repository: blocker.repository?.nameWithOwner ?? null,
          });
        }
      }
    }
```

Update the return object — replace `isBlocked: typename === "Issue" ? ... : false,` with:
```typescript
    isBlocked:
      typename === "Issue" ? (content.issueDependenciesSummary?.blockedBy ?? 0) > 0 : false,
    blockerRefs: blockerRefs.length > 0 ? blockerRefs : undefined,
```

Keep `isBlocked` from `issueDependenciesSummary` as-is (architect says keep it for this PR). The `blockerRefs` is the new raw data for the backend parser to process.

**Verify:** `bunx tsc --noEmit` (expect errors in other files, resolved by Tasks 3-4).

### Task 3: Update GitHub backend parser to build `blockedByIds` — Depends on: Task 1, Task 2

**File:** `packages/daemon/src/state/backends/github.ts`

**3a. Update `GitHubProjectItem` interface** — add `blockerRefs` alongside existing `isBlocked`:
```typescript
  isBlocked?: boolean;
  blockerRefs?: Array<{
    number: number;
    state: string;
    repository: string | null;
  }>;
```

**3b. Update `parseIssues()`** — after extracting `isBlocked` (line 119), build `blockedByIds` from `blockerRefs`:

Replace current line 119-121:
```typescript
      const isBlocked = typedItem.isBlocked === true;

      parsed.push(createParsedIssue(issueId, status, labels, prRef, source, isBlocked));
```

With:
```typescript
      // Build blockedByIds from blocker references: filter OPEN, build IDs, dedup+sort
      const blockedByIds: string[] = [];
      if (Array.isArray(typedItem.blockerRefs)) {
        const seen = new Set<string>();
        for (const ref of typedItem.blockerRefs) {
          if (
            ref.state === "OPEN" &&
            typeof ref.number === "number" &&
            typeof ref.repository === "string"
          ) {
            const parsed = parseOwnerRepo(ref.repository);
            if (parsed) {
              const blockerId = buildIssueId(parsed.owner, parsed.repo, ref.number);
              if (!seen.has(blockerId)) {
                seen.add(blockerId);
                blockedByIds.push(blockerId);
              }
            }
          }
        }
        blockedByIds.sort();
      }

      parsed.push(createParsedIssue(issueId, status, labels, prRef, source, blockedByIds));
```

Note: `parseOwnerRepo` and `buildIssueId` are already defined in this file. The `createParsedIssue` call now passes `blockedByIds` instead of `isBlocked` (the function computes `isBlocked` from the array).

**Verify:** `bunx tsc --noEmit`

### Task 4: Thread `blockedByIds` through pipeline and update display — Depends on: Task 1, Task 3

**Files:** `packages/daemon/src/state/fetch.ts`, `packages/daemon/src/state/decision.ts`, `packages/daemon/src/cli/poll-formatter.ts`

**4a. fetch.ts — Update `enrichParsedIssues()` return** (line 589) — add `blockedByIds` before `isBlocked`:
```typescript
      blockedByIds: issue.blockedByIds,
      isBlocked: issue.isBlocked,
```

**4b. decision.ts — Update `buildIssueState()` return** (line ~309-324) — add `blockedByIds` before `isBlocked`:
```typescript
    blockedByIds: data.blockedByIds,
    isBlocked: data.isBlocked ?? false,
```

The blocking gate (line 291-293) stays unchanged — it still checks `data.isBlocked`.

**4c. poll-formatter.ts — Update `BlockedIssue` interface** (line 10-14):
```typescript
interface BlockedIssue {
  issueId: string;
  reason: string;
  blockedByIds?: string[];
  source: IssueSource | null;
}
```

**4d. poll-formatter.ts — Update `categorizeIssues()` blocked check** (line 41-44) — pass blocker IDs:
```typescript
    if (issue.isBlocked) {
      const ids = issue.blockedByIds ?? [];
      const reason = ids.length > 0
        ? `blocked by ${ids.join(", ")}`
        : "blocked";
      blocked.push({ issueId, reason, blockedByIds: ids, source: issue.source });
      continue;
    }
```

This produces output like: `#114  blocked by sjawhar-legion-110, sjawhar-legion-112` instead of `#114  blocked`.

**Verify:** `bunx tsc --noEmit` — should be clean now.

### Task 5: Tests and verification — Depends on: Tasks 1-4

**Files:**
- `packages/daemon/src/state/__tests__/github-fetch.test.ts`
- `packages/daemon/src/state/backends/__tests__/github.test.ts`
- `packages/daemon/src/state/__tests__/decision.test.ts`
- `packages/daemon/src/cli/__tests__/poll-formatter.test.ts`

**5a. github-fetch.test.ts** — Update mock GraphQL Issue nodes to include `blockedBy` connection:
- Issue with 2 open + 1 closed blocker → `blockerRefs` has 3 entries, `isBlocked: true`
- Issue with 0 blockers → `blockerRefs` absent/empty, `isBlocked: false`
- Issue with only closed blockers → `blockerRefs` has entries but all closed, `isBlocked: false` (from summary)
- PullRequest → no `blockerRefs`, `isBlocked: false`
- Issue with `blockedBy: null` → graceful handling

**5b. backends/github.test.ts** — Test `GitHubTracker.parseIssues()` with `blockerRefs`:
- Item with 2 open blockers from same repo → `blockedByIds` has 2 sorted IDs, `isBlocked: true`
- Item with 1 open + 1 closed blocker → `blockedByIds` has 1 ID (open only), `isBlocked: true`
- Item with only closed blockers → `blockedByIds: []`, `isBlocked: false`
- Item with duplicate blocker refs → `blockedByIds` is deduplicated
- Item without `blockerRefs` → `blockedByIds: []`, `isBlocked: false` (backward compat)
- Cross-repo blocker → `blockedByIds` contains correct cross-repo Legion ID

**5c. decision.test.ts** — Verify `buildIssueState()` with `blockedByIds`:
- Issue with `blockedByIds: ["sjawhar-legion-110"]`, `isBlocked: true` → dispatch_* overridden to skip
- Issue with `blockedByIds: []`, `isBlocked: false` → dispatch actions proceed normally
- `blockedByIds` appears in returned `IssueState`

**5d. poll-formatter.test.ts** — Verify blocked display:
- Issue with `isBlocked: true`, `blockedByIds: ["sjawhar-legion-110", "sjawhar-legion-112"]` → output shows `blocked by sjawhar-legion-110, sjawhar-legion-112`
- Issue with `isBlocked: true`, `blockedByIds: []` → output shows `blocked` (fallback)

**5e. End-to-end pipeline test** (AC9): Given a mock issue with one open blocker and one closed blocker, verify the full pipeline: `blockedByIds` contains only the open blocker's ID, `isBlocked: true`, dispatch actions overridden.

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
Task 1 (types) ───┬──► Task 3 (backend parser) ──► Task 4 (pipeline + display)
                   │                                        │
Task 2 (GraphQL) ──┘                                        ▼
                                                    Task 5 (tests)
```

Tasks 1 and 2 are independent — can execute in parallel.
Task 3 depends on Task 1 and Task 2.
Task 4 depends on Task 1 and Task 3.
Task 5 depends on all previous tasks.

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

1. **Type safety** — `bunx tsc --noEmit` → exit 0
2. **State module tests** — `bun test packages/daemon/src/state/` → all pass
3. **CLI tests** — `bun test packages/daemon/src/cli/` → all pass
4. **AC2 verified**: blocked issue with 2 open + 1 closed blocker → `blockedByIds` has length 2
5. **AC3 verified**: blocker IDs use `buildIssueId` format (e.g., `sjawhar-legion-110`)
6. **AC5 verified**: `isBlocked === (blockedByIds.length > 0)` at every layer
7. **AC7 verified**: poll output shows `blocked by sjawhar-legion-110, sjawhar-legion-112`
8. **AC8 verified**: Linear backend existing tests pass (default `blockedByIds: []`)
9. **AC9 verified**: end-to-end pipeline test with mixed open/closed blockers
10. **Full suite** — `bun test` → all ~730 tests pass

### Tools Needed
- Bun test runner
- TypeScript compiler (tsc)
- Biome linter

## Architect Concerns Addressed

| Concern | How Addressed |
|---------|---------------|
| `buildIssueId()` is one-way | Poll formatter displays canonical Legion IDs verbatim (AC7) |
| `blockedBy` returns open AND closed | Filter to `state == "OPEN"` in `parseIssues()` (AC2) |
| ORG_QUERY and USER_QUERY must stay in sync | Task 2 explicitly modifies both queries identically |
| `issueDependenciesSummary` kept for this PR | Not removed — kept alongside `blockedBy` connection (DD9) |
| `blockedByIds` must be deduplicated and sorted | Set-based dedup + `sort()` in `parseIssues()` (AC3) |

## Relevant Learnings

1. `docs/solutions/integration-issues/github-graphql-pr-draft-status.md` — Same query→parse→enrich→decide pipeline pattern
2. `docs/solutions/github-api/graphql-org-user-fallback.md` — ORG_QUERY and USER_QUERY must stay in sync
3. `docs/solutions/daemon/controller-observability.md` — Surface raw signals through state machine layers
