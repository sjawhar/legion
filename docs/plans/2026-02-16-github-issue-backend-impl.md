# Pluggable Issue Tracker Backend — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Introduce an `IssueTracker` interface so Legion can use either Linear or GitHub Issues as its issue tracking backend.

**Architecture:** Backend-specific parsing moves behind an `IssueTracker` interface. A new `POST /state/collect` daemon endpoint replaces the `cli.ts` stdin pipe. Session ID computation generalizes to accept any string team ID. Skills swap between `linear/SKILL.md` and `github/SKILL.md` based on `LEGION_ISSUE_BACKEND`.

**Tech Stack:** TypeScript on Bun, Bun test runner, Biome for lint/format, `gh` CLI for GitHub operations, `uuid` for session IDs.

**Design doc:** `docs/plans/2026-02-16-github-issue-backend-design.md`

---

## Phase 1: TypeScript Abstraction (no behavior change)

### Task 1: Generalize Session ID Computation

Replace UUID-only `teamId` requirement with any-string support. This unblocks everything else.

**Files:**
- Modify: `packages/daemon/src/state/types.ts:409-434`
- Modify: `packages/daemon/src/state/__tests__/types.test.ts:19-82`

**Step 1: Write the failing tests**

Add tests for non-UUID team IDs to `packages/daemon/src/state/__tests__/types.test.ts`. Add a new `describe` block after the existing `computeControllerSessionId` tests:

```typescript
describe("computeSessionId with non-UUID team ID", () => {
  it("accepts a GitHub project ID string", () => {
    const result = computeSessionId("sjawhar/5", "gh-42", "implement");
    expect(result).toMatch(/^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
  });

  it("same non-UUID inputs produce same output", () => {
    const result1 = computeSessionId("sjawhar/5", "gh-42", "implement");
    const result2 = computeSessionId("sjawhar/5", "gh-42", "implement");
    expect(result1).toBe(result2);
  });

  it("different non-UUID team IDs produce different output", () => {
    const result1 = computeSessionId("sjawhar/5", "gh-42", "implement");
    const result2 = computeSessionId("sjawhar/6", "gh-42", "implement");
    expect(result1).not.toBe(result2);
  });

  it("non-UUID team ID produces different output from UUID team ID", () => {
    const uuidResult = computeSessionId("7b4f0862-b775-4cb0-9a67-85400c6f44a8", "ENG-21", "implement");
    const stringResult = computeSessionId("sjawhar/5", "ENG-21", "implement");
    expect(uuidResult).not.toBe(stringResult);
  });
});

describe("computeControllerSessionId with non-UUID team ID", () => {
  it("accepts a GitHub project ID string", () => {
    const result = computeControllerSessionId("sjawhar/5");
    expect(result).toMatch(/^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
  });

  it("same non-UUID input produces same output", () => {
    const result1 = computeControllerSessionId("sjawhar/5");
    const result2 = computeControllerSessionId("sjawhar/5");
    expect(result1).toBe(result2);
  });
});
```

Also update the existing "throws error for invalid team id" tests — they should be removed since any string is now valid. Replace them:

```typescript
// In computeSessionId describe block, replace:
//   it("throws error for invalid team id", ...
// With:
  it("accepts non-UUID team ID without throwing", () => {
    expect(() => {
      computeSessionId("not-a-valid-uuid", "ENG-21", "implement");
    }).not.toThrow();
  });

// In computeControllerSessionId describe block, replace:
//   it("throws error for invalid team id", ...
// With:
  it("accepts non-UUID team ID without throwing", () => {
    expect(() => {
      computeControllerSessionId("not-a-valid-uuid");
    }).not.toThrow();
  });
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/state/__tests__/types.test.ts`
Expected: New tests FAIL (computeSessionId throws for non-UUID input).

**Step 3: Implement the change**

In `packages/daemon/src/state/types.ts`, add a fixed namespace UUID constant and a `teamIdToNamespace()` helper. Replace the UUID validation in both `computeSessionId` and `computeControllerSessionId`:

```typescript
// Add after the BASE62_CHARS constant (line 370):

/**
 * Fixed namespace UUID for deriving team ID namespaces.
 * Generated once, never changes. Used to convert arbitrary
 * team ID strings into deterministic UUID namespaces.
 */
const LEGION_NAMESPACE = "a1b2c3d4-e5f6-7890-abcd-ef1234567890";

/**
 * Convert any team ID string into a UUID namespace.
 * All team IDs are hashed through the fixed LEGION_NAMESPACE
 * to produce a deterministic UUID, ensuring consistent
 * session IDs regardless of team ID format.
 */
function teamIdToNamespace(teamId: string): string {
  return uuidv5(teamId, LEGION_NAMESPACE);
}
```

Then update `computeSessionId` (line 409):

```typescript
export function computeSessionId(teamId: string, issueId: string, mode: WorkerModeLiteral): string {
  const namespace = teamIdToNamespace(teamId);
  const uuid = uuidv5(`${issueId.toLowerCase()}:${mode}`, namespace);
  return uuidToSessionId(uuid);
}
```

And `computeControllerSessionId` (line 427):

```typescript
export function computeControllerSessionId(teamId: string): string {
  const namespace = teamIdToNamespace(teamId);
  const uuid = uuidv5("controller", namespace);
  return uuidToSessionId(uuid);
}
```

Remove the `validate as validateUuid` import from `uuid` (no longer needed in types.ts — check if used elsewhere first).

**Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/state/__tests__/types.test.ts`
Expected: ALL PASS.

**Step 5: Run full test suite to check for regressions**

Run: `bun test packages/daemon/`
Expected: ALL 245 tests PASS.

**IMPORTANT:** Existing tests that used UUID team IDs will now produce DIFFERENT session IDs because all strings (including UUIDs) are hashed through the namespace. This is expected — the design doc says "breaking change, no backward compatibility." But check that `server.test.ts` still passes since it uses `computeSessionId` internally.

**Step 6: Commit**

```
feat: generalize session ID computation to accept any string team ID
```

---

### Task 2: Create IssueTracker Interface and Backend Factory

**Files:**
- Create: `packages/daemon/src/state/backends/issue-tracker.ts`
- Create: `packages/daemon/src/state/backends/index.ts`
- Create: `packages/daemon/src/state/backends/__tests__/index.test.ts`

**Step 1: Write the failing test**

Create `packages/daemon/src/state/backends/__tests__/index.test.ts`:

```typescript
import { describe, expect, it } from "bun:test";
import { getBackend } from "../index";

describe("getBackend", () => {
  it("returns a LinearTracker for 'linear'", () => {
    const backend = getBackend("linear");
    expect(backend).toBeDefined();
    expect(typeof backend.parseIssues).toBe("function");
    expect(typeof backend.resolveTeamId).toBe("function");
  });

  it("returns a GitHubTracker for 'github'", () => {
    const backend = getBackend("github");
    expect(backend).toBeDefined();
    expect(typeof backend.parseIssues).toBe("function");
    expect(typeof backend.resolveTeamId).toBe("function");
  });

  it("throws for unknown backend", () => {
    expect(() => getBackend("jira" as any)).toThrow("Unknown backend");
  });
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/state/backends/__tests__/index.test.ts`
Expected: FAIL (module not found).

**Step 3: Write the interface and factory**

Create `packages/daemon/src/state/backends/issue-tracker.ts`:

```typescript
import type { ParsedIssue } from "../types";

/**
 * Pluggable issue tracker backend interface.
 *
 * Implementations parse raw issue data from their respective APIs
 * into the normalized ParsedIssue format used by the state machine.
 */
export interface IssueTracker {
  /** Parse raw issue data from the tracker into normalized form. */
  parseIssues(raw: unknown): ParsedIssue[];

  /** Resolve a team/project reference to a stable internal ID. */
  resolveTeamId(ref: string): Promise<string>;
}

export type BackendName = "linear" | "github";
```

Create `packages/daemon/src/state/backends/index.ts`:

```typescript
import type { BackendName, IssueTracker } from "./issue-tracker";
import { GitHubTracker } from "./github";
import { LinearTracker } from "./linear";

export function getBackend(name: BackendName): IssueTracker {
  switch (name) {
    case "linear":
      return new LinearTracker();
    case "github":
      return new GitHubTracker();
    default:
      throw new Error(`Unknown backend: ${name}`);
  }
}

export type { BackendName, IssueTracker } from "./issue-tracker";
```

Also create stub implementations so the factory works. Create `packages/daemon/src/state/backends/linear.ts`:

```typescript
import type { ParsedIssue } from "../types";
import type { IssueTracker } from "./issue-tracker";

export class LinearTracker implements IssueTracker {
  parseIssues(_raw: unknown): ParsedIssue[] {
    throw new Error("Not yet implemented — will be moved from fetch.ts in Task 3");
  }

  async resolveTeamId(_ref: string): Promise<string> {
    throw new Error("Not yet implemented — will be moved from team-resolver.ts in Task 3");
  }
}
```

Create `packages/daemon/src/state/backends/github.ts`:

```typescript
import type { ParsedIssue } from "../types";
import type { IssueTracker } from "./issue-tracker";

export class GitHubTracker implements IssueTracker {
  parseIssues(_raw: unknown): ParsedIssue[] {
    throw new Error("Not yet implemented — see Task 4");
  }

  async resolveTeamId(_ref: string): Promise<string> {
    throw new Error("Not yet implemented — see Task 5");
  }
}
```

**Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/state/backends/__tests__/index.test.ts`
Expected: ALL PASS.

**Step 5: Run lint + type check**

Run: `bunx biome check packages/daemon/src/state/backends/ && bunx tsc --noEmit`
Expected: No errors.

**Step 6: Commit**

```
feat: add IssueTracker interface and backend factory
```

---

### Task 3: Move Linear Parsing Into LinearTracker

Relocate `parseLinearIssues()` from `fetch.ts` into `backends/linear.ts`. Keep `fetch.ts` importing from the backend so existing code paths still work.

**Files:**
- Modify: `packages/daemon/src/state/backends/linear.ts`
- Modify: `packages/daemon/src/state/fetch.ts`
- Modify: `packages/daemon/src/state/types.ts:110-144` (move Linear types)
- Create: `packages/daemon/src/state/backends/__tests__/linear.test.ts`
- Modify: `packages/daemon/src/state/__tests__/fetch.test.ts` (update imports)

**Step 1: Move Linear-specific types from `types.ts` to `backends/linear.ts`**

Move these interfaces from `types.ts` lines 110-144 to `backends/linear.ts`:
- `LinearStateDict`
- `LinearLabelNode`
- `LinearLabelsContainer`
- `LinearIssue`
- `LinearAttachment`
- `LinearIssueRaw`

Keep `GitHubLabel` and `GitHubPR` in `types.ts` (they're used by the GitHub PR draft fetching, which is backend-agnostic).

In `types.ts`, add a re-export for backward compatibility (so existing tests/imports don't break):

```typescript
// Re-export Linear types from backend for backward compatibility
export type {
  LinearAttachment,
  LinearIssue,
  LinearIssueRaw,
  LinearLabelNode,
  LinearLabelsContainer,
  LinearStateDict,
} from "./backends/linear";
```

**Step 2: Move `parseLinearIssues()` from `fetch.ts` to `backends/linear.ts`**

Copy the `parseLinearIssues()` function (fetch.ts lines 296-363) into `LinearTracker.parseIssues()`. The function becomes the class method, accepting `unknown` (cast internally to `LinearIssueRaw[]`).

In `fetch.ts`, replace the local `parseLinearIssues` with a re-export:

```typescript
import { LinearTracker } from "./backends/linear";

// Backward-compatible wrapper
export function parseLinearIssues(linearIssues: LinearIssueRaw[]): ParsedIssue[] {
  return new LinearTracker().parseIssues(linearIssues);
}
```

**Step 3: Create `backends/__tests__/linear.test.ts`**

Move the `parseLinearIssues` describe blocks from `fetch.test.ts` into this new file, importing from the new location. Keep `fetch.test.ts` tests for `getLiveWorkers`, `getPrDraftStatusBatch`, and `fetchAllIssueData`.

```typescript
import { describe, expect, it } from "bun:test";
import { LinearTracker } from "../linear";

const tracker = new LinearTracker();

describe("LinearTracker.parseIssues", () => {
  it("parses basic issue", () => {
    const issues = [
      {
        identifier: "ENG-21",
        state: { name: "In Progress" },
        labels: { nodes: [{ name: "worker-done" }] },
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("ENG-21");
    expect(result[0].status).toBe("In Progress");
    expect(result[0].hasWorkerDone).toBe(true);
  });

  // ... copy all parseLinearIssues tests from fetch.test.ts, adapted to use tracker.parseIssues()
});
```

**Step 4: Run all tests**

Run: `bun test packages/daemon/`
Expected: ALL PASS (245+ tests). The backward-compatible wrapper in `fetch.ts` means nothing downstream breaks.

**Step 5: Lint + type check**

Run: `bunx biome check packages/daemon/src/ && bunx tsc --noEmit`
Expected: No errors.

**Step 6: Commit**

```
refactor: move Linear parsing into LinearTracker backend
```

---

### Task 4: Implement GitHub Issue Parser

**Files:**
- Modify: `packages/daemon/src/state/backends/github.ts`
- Create: `packages/daemon/src/state/backends/__tests__/github.test.ts`

**Step 1: Write failing tests**

Create `packages/daemon/src/state/backends/__tests__/github.test.ts`:

```typescript
import { describe, expect, it } from "bun:test";
import { GitHubTracker } from "../github";

const tracker = new GitHubTracker();

describe("GitHubTracker.parseIssues", () => {
  it("parses a basic GitHub project item", () => {
    const items = [
      {
        id: "PVTI_abc",
        content: {
          number: 42,
          repository: "gh",
          url: "https://github.com/sjawhar/gh/issues/42",
          type: "Issue",
        },
        status: "In Progress",
        labels: ["worker-active"],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("GH-42");
    expect(result[0].status).toBe("In Progress");
    expect(result[0].labels).toEqual(["worker-active"]);
    expect(result[0].hasWorkerActive).toBe(true);
    expect(result[0].prRef).toBeNull();
  });

  it("normalizes status aliases", () => {
    const items = [
      {
        id: "PVTI_abc",
        content: { number: 1, repository: "gh", url: "https://github.com/o/gh/issues/1", type: "Issue" },
        status: "In Review",
        labels: [],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result[0].status).toBe("Needs Review");
  });

  it("skips non-issue items (DraftIssue, PullRequest)", () => {
    const items = [
      {
        id: "PVTI_draft",
        content: { title: "A draft", type: "DraftIssue" },
        status: "Todo",
        labels: [],
      },
      {
        id: "PVTI_issue",
        content: { number: 10, repository: "gh", url: "https://github.com/o/gh/issues/10", type: "Issue" },
        status: "Todo",
        labels: [],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("GH-10");
  });

  it("handles items with no status", () => {
    const items = [
      {
        id: "PVTI_abc",
        content: { number: 5, repository: "gh", url: "https://github.com/o/gh/issues/5", type: "Issue" },
        status: null,
        labels: [],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result[0].status).toBe("");
  });

  it("handles items with no labels", () => {
    const items = [
      {
        id: "PVTI_abc",
        content: { number: 5, repository: "gh", url: "https://github.com/o/gh/issues/5", type: "Issue" },
        status: "Backlog",
        labels: null,
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result[0].labels).toEqual([]);
  });

  it("handles multi-repo with different repo names", () => {
    const items = [
      {
        id: "PVTI_1",
        content: { number: 1, repository: "frontend", url: "https://github.com/org/frontend/issues/1", type: "Issue" },
        status: "Todo",
        labels: [],
      },
      {
        id: "PVTI_2",
        content: { number: 1, repository: "backend", url: "https://github.com/org/backend/issues/1", type: "Issue" },
        status: "Todo",
        labels: [],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result).toHaveLength(2);
    expect(result[0].issueId).toBe("FRONTEND-1");
    expect(result[1].issueId).toBe("BACKEND-1");
  });

  it("returns empty array for empty input", () => {
    expect(tracker.parseIssues([])).toEqual([]);
  });

  it("returns empty array for null/undefined input", () => {
    expect(tracker.parseIssues(null)).toEqual([]);
    expect(tracker.parseIssues(undefined)).toEqual([]);
  });
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/state/backends/__tests__/github.test.ts`
Expected: FAIL (parseIssues throws "Not yet implemented").

**Step 3: Implement GitHubTracker.parseIssues**

In `packages/daemon/src/state/backends/github.ts`:

```typescript
import { createParsedIssue, IssueStatus, type ParsedIssue } from "../types";
import type { IssueTracker } from "./issue-tracker";

/**
 * Raw project item from `gh project item-list --format json`.
 */
interface GitHubProjectItem {
  id?: string;
  content?: {
    number?: number;
    repository?: string;
    url?: string;
    type?: string;
  };
  status?: string | null;
  labels?: string[] | null;
}

export class GitHubTracker implements IssueTracker {
  parseIssues(raw: unknown): ParsedIssue[] {
    if (!Array.isArray(raw)) {
      return [];
    }

    const parsed: ParsedIssue[] = [];

    for (const item of raw as GitHubProjectItem[]) {
      const content = item.content;
      if (!content || content.type !== "Issue") {
        continue;
      }

      const number = content.number;
      const repo = content.repository;
      if (typeof number !== "number" || typeof repo !== "string") {
        continue;
      }

      const issueId = `${repo.toUpperCase()}-${number}`;
      const status = IssueStatus.normalize(item.status ?? null);

      let labels: string[] = [];
      if (Array.isArray(item.labels)) {
        labels = item.labels.filter((l): l is string => typeof l === "string" && l !== "");
      }

      // PR ref is null at parse time — discovered during enrichment
      parsed.push(createParsedIssue(issueId, status, labels, null));
    }

    return parsed;
  }

  async resolveTeamId(_ref: string): Promise<string> {
    throw new Error("Not yet implemented — see Task 5");
  }
}
```

**Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/state/backends/__tests__/github.test.ts`
Expected: ALL PASS.

**Step 5: Lint + type check**

Run: `bunx biome check packages/daemon/src/state/backends/ && bunx tsc --noEmit`
Expected: No errors.

**Step 6: Commit**

```
feat: implement GitHub project item parser
```

---

### Task 5: Add `POST /state/collect` Endpoint

**Files:**
- Modify: `packages/daemon/src/state/fetch.ts` (extract `enrichParsedIssues`)
- Modify: `packages/daemon/src/daemon/server.ts` (add endpoint)
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts` (add tests)

**Step 1: Write the failing tests**

Add to `packages/daemon/src/daemon/__tests__/server.test.ts`:

```typescript
describe("POST /state/collect", () => {
  it("returns collected state for linear backend", async () => {
    await startTestServer();
    const issues = [
      {
        identifier: "ENG-21",
        state: { name: "Todo" },
        labels: { nodes: [] },
      },
    ];
    const response = await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({ backend: "linear", issues }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as { issues: Record<string, unknown> };
    expect(body.issues).toBeDefined();
    expect(body.issues["ENG-21"]).toBeDefined();
  });

  it("returns collected state for github backend", async () => {
    await startTestServer();
    const issues = [
      {
        id: "PVTI_abc",
        content: { number: 42, repository: "gh", url: "https://github.com/o/gh/issues/42", type: "Issue" },
        status: "Todo",
        labels: [],
      },
    ];
    const response = await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({ backend: "github", issues }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as { issues: Record<string, unknown> };
    expect(body.issues).toBeDefined();
    expect(body.issues["GH-42"]).toBeDefined();
  });

  it("rejects invalid backend", async () => {
    await startTestServer();
    const response = await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({ backend: "jira", issues: [] }),
    });
    expect(response.status).toBe(400);
    const body = (await response.json()) as { error: string };
    expect(body.error).toBe("invalid_backend");
  });

  it("rejects invalid JSON body", async () => {
    await startTestServer();
    const response = await originalFetch(`${baseUrl}/state/collect`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: "not-json",
    });
    expect(response.status).toBe(400);
  });

  it("returns empty state for empty issues array", async () => {
    await startTestServer();
    const response = await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({ backend: "linear", issues: [] }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as { issues: Record<string, unknown> };
    expect(body.issues).toEqual({});
  });
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: New tests FAIL (404 on `/state/collect`).

**Step 3a: Refactor `fetchAllIssueData` to accept `ParsedIssue[]`**

`fetchAllIssueData` currently takes `LinearIssueRaw[]` and calls `parseLinearIssues()` internally (fetch.ts line 387). Refactor it into two functions:

In `packages/daemon/src/state/fetch.ts`, extract the enrichment logic:

```typescript
/**
 * Enrich parsed issues with live worker status and PR draft status.
 * This is the backend-agnostic enrichment step.
 */
export async function enrichParsedIssues(
  parsedIssues: ParsedIssue[],
  daemonUrl: string,
  runner: CommandRunner = defaultRunner
): Promise<FetchedIssueData[]> {
  // Phase 1: Identify PRs that need draft status lookup
  const prRefsForStatus: Record<string, GitHubPRRefType> = {};
  for (const p of parsedIssues) {
    if (p.needsPrStatus && p.prRef !== null) {
      prRefsForStatus[p.issueId] = p.prRef;
    }
  }

  // Phase 2: Fetch everything in parallel
  let liveWorkers: Record<string, { mode: string; status: string }> = {};
  let prDraftMap: Record<string, boolean | null> = {};

  const fetchWorkers = async () => {
    liveWorkers = await getLiveWorkers(daemonUrl);
  };

  const fetchPrDraftStatusSafe = async () => {
    if (Object.keys(prRefsForStatus).length === 0) return;
    try {
      prDraftMap = await getPrDraftStatusBatch(prRefsForStatus, runner);
    } catch {
      for (const issueId of Object.keys(prRefsForStatus)) {
        prDraftMap[issueId] = null;
      }
    }
  };

  await Promise.all([fetchWorkers(), fetchPrDraftStatusSafe()]);

  // Phase 3: Build results
  const results: FetchedIssueData[] = [];
  for (const issue of parsedIssues) {
    const workerInfo = liveWorkers[issue.issueId.toUpperCase()] ?? null;
    const hasLiveWorker = workerInfo !== null;
    const prIsDraft: boolean | null = prDraftMap[issue.issueId] ?? null;
    results.push({
      issueId: issue.issueId,
      status: issue.status,
      labels: issue.labels,
      hasPr: issue.hasPr,
      prIsDraft,
      hasLiveWorker,
      workerMode: workerInfo?.mode ?? null,
      workerStatus: workerInfo?.status ?? null,
      hasUserFeedback: issue.hasUserFeedback,
      hasUserInputNeeded: issue.hasUserInputNeeded,
      hasNeedsApproval: issue.hasNeedsApproval,
      hasHumanApproved: issue.hasHumanApproved,
    });
  }
  return results;
}
```

Then make `fetchAllIssueData` a thin wrapper (backward compat for cli.ts):

```typescript
export async function fetchAllIssueData(
  linearIssues: LinearIssueRaw[],
  daemonUrl: string,
  runner: CommandRunner = defaultRunner
): Promise<FetchedIssueData[]> {
  const parsedIssues = parseLinearIssues(linearIssues);
  return enrichParsedIssues(parsedIssues, daemonUrl, runner);
}
```

Run tests after this refactor to verify no regressions: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`

**Step 3b: Implement the endpoint**

In `packages/daemon/src/daemon/server.ts`, add the route. Add imports at the top:

```typescript
import { getBackend, type BackendName } from "../state/backends/index";
import { buildCollectedState } from "../state/decision";
import { enrichParsedIssues } from "../state/fetch";
import { CollectedState } from "../state/types";
```

Inside the `fetch()` handler, add before the final `notFound()` return (match the existing pattern of `if (method === ... && url.pathname === ...) {`):

```typescript
if (method === "POST" && url.pathname === "/state/collect") {
  let payload: Record<string, unknown>;
  try {
    payload = await parseJson(request);
  } catch {
    return badRequest("invalid_json");
  }

  const backend = payload.backend;
  if (backend !== "linear" && backend !== "github") {
    return badRequest("invalid_backend");
  }

  const issues = payload.issues;
  if (!Array.isArray(issues)) {
    return badRequest("invalid_issues");
  }

  try {
    const tracker = getBackend(backend as BackendName);
    const parsed = tracker.parseIssues(issues);
    const daemonUrl = `http://127.0.0.1:${server.port}`;
    const issuesData = await enrichParsedIssues(parsed, daemonUrl);
    const state = buildCollectedState(issuesData, opts.teamId);
    return jsonResponse(CollectedState.toDict(state));
  } catch (error) {
    const message = error instanceof Error ? error.message : "unknown_error";
    return serverError(message);
  }
}
```

Note: `server.port` is the actual port (Bun.serve resolves port 0 to a real port). The `daemonUrl` is the daemon calling itself — this works because the server is running when the endpoint is hit. `enrichParsedIssues` calls `getLiveWorkers(daemonUrl)` which hits `GET /workers` on the same server.

**Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: ALL PASS.

**Step 5: Run full test suite**

Run: `bun test packages/daemon/`
Expected: ALL PASS.

**Step 6: Commit**

```
feat: add POST /state/collect endpoint to daemon server
```

---

### Task 6: Add `legion collect-state` CLI Command

**Files:**
- Modify: `packages/daemon/src/cli/index.ts`

**Step 1: Implement the command**

Add to `packages/daemon/src/cli/index.ts`:

```typescript
async function cmdCollectState(backend: string): Promise<void> {
  if (backend !== "linear" && backend !== "github") {
    throw new CliError(`Invalid backend: ${backend}. Must be 'linear' or 'github'.`);
  }

  const stdinText = await new Response(Bun.stdin.stream()).text();
  let issues: unknown;
  try {
    issues = JSON.parse(stdinText);
  } catch {
    throw new CliError("Failed to parse stdin as JSON");
  }

  const daemonPort = getDaemonPort();
  const baseUrl = `http://127.0.0.1:${daemonPort}`;

  try {
    const response = await fetch(`${baseUrl}/state/collect`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ backend, issues }),
    });

    if (!response.ok) {
      const body = await response.text();
      throw new CliError(`Daemon returned ${response.status}: ${body}`);
    }

    const result = await response.text();
    process.stdout.write(`${result}\n`);
  } catch (error) {
    if (error instanceof CliError) throw error;
    throw new CliError(`Could not connect to daemon. Is it running?\nTried: ${baseUrl}/state/collect`);
  }
}

export const collectStateCommand = defineCommand({
  meta: { name: "collect-state", description: "Collect and analyze issue state via daemon" },
  args: {
    backend: {
      type: "positional",
      description: "Issue tracker backend (linear or github)",
      required: true,
    },
  },
  async run({ args }) {
    try {
      await cmdCollectState(args.backend);
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});
```

Add `"collect-state": collectStateCommand` to the `mainCommand.subCommands` object.

**Step 2: Run lint + type check**

Run: `bunx biome check packages/daemon/src/cli/ && bunx tsc --noEmit`
Expected: No errors.

**Step 3: Run full test suite**

Run: `bun test packages/daemon/`
Expected: ALL PASS.

**Step 4: Commit**

```
feat: add legion collect-state CLI command
```

---

### Task 7: Add `--backend` Flag to `legion start` and Config

**Files:**
- Modify: `packages/daemon/src/daemon/config.ts`
- Modify: `packages/daemon/src/cli/index.ts` (start command)
- Modify: `packages/daemon/src/daemon/__tests__/config.test.ts`

**Step 1: Add `issueBackend` to `DaemonConfig`**

In `config.ts`, add to `DaemonConfig` interface:

```typescript
issueBackend?: "linear" | "github";
```

In `loadConfig()`:

```typescript
const issueBackend = (env.LEGION_ISSUE_BACKEND || "linear") as "linear" | "github";
if (issueBackend !== "linear" && issueBackend !== "github") {
  throw new Error(`LEGION_ISSUE_BACKEND must be 'linear' or 'github' (got: ${issueBackend})`);
}
```

Add `issueBackend` to the return object.

**Step 2: Add `--backend` flag to `legion start`**

In the `startCommand` definition in `cli/index.ts`:

```typescript
backend: {
  type: "string",
  alias: "b",
  description: "Issue tracker backend (linear or github)",
},
```

In `cmdStart`, pass it through:

```typescript
if (opts.backend) {
  overrides.issueBackend = opts.backend as "linear" | "github";
}
```

**Step 3: Run lint + type check + tests**

Run: `bunx biome check packages/daemon/src/ && bunx tsc --noEmit && bun test packages/daemon/`
Expected: ALL PASS.

**Step 4: Commit**

```
feat: add --backend flag to legion start and LEGION_ISSUE_BACKEND config
```

---

## Phase 2: GitHub Skill + Controller Wiring

> **Note:** Tasks 8-10 modify skill files (markdown instructions for AI agents). These don't have automated tests — they are verified by running the full system. The content below provides structure and key patterns; the implementer should read the existing `linear/SKILL.md` and workflow files to match their style and completeness.

### Task 8: Create `github/SKILL.md`

**Files:**
- Create: `.opencode/skills/github/SKILL.md`

**Step 1: Write the skill file**

Create `.opencode/skills/github/SKILL.md` documenting `gh` CLI patterns for all issue operations. Model it after `linear/SKILL.md` but with `gh` commands. Key sections:

- Search issues: `gh project item-list $PROJECT_NUM --owner $OWNER --format json`
- Get issue: `gh issue view N --json title,body,labels,comments,state -R owner/repo`
- Update status: `gh api graphql` mutation for Projects V2 Status field
- Add label: `gh issue edit N --add-label "label" -R owner/repo`
- Remove label: `gh issue edit N --remove-label "label" -R owner/repo`
- Comment: `gh issue comment N --body "..." -R owner/repo`
- Create issue: `gh issue create --title "..." --body "..." -R owner/repo`

Include the GraphQL mutation template for Status field updates.

**Step 2: Commit**

```
feat: add github issue tracker skill
```

---

### Task 9: Update Controller Skill for Backend Switching

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`

**Step 1: Update the fetch step**

Add a conditional section at the beginning of "Step 1: Fetch Issues" that checks `LEGION_ISSUE_BACKEND`:

- If `linear`: use `linear_linear(action="search", ...)` (existing behavior)
- If `github`: use `gh project item-list ...`

Both paths pipe result to `POST /state/collect` via:
```bash
echo "$ISSUES_JSON" | curl -s -X POST http://127.0.0.1:$LEGION_DAEMON_PORT/state/collect \
  -H 'Content-Type: application/json' \
  --data @- <<< "{\"backend\": \"$LEGION_ISSUE_BACKEND\", \"issues\": $(cat)}"
```

Or more robustly using `jq`:
```bash
jq -n --arg backend "$LEGION_ISSUE_BACKEND" --argjson issues "$ISSUES_JSON" \
  '{"backend": $backend, "issues": $issues}' | \
  curl -s -X POST http://127.0.0.1:$LEGION_DAEMON_PORT/state/collect \
  -H 'Content-Type: application/json' --data @-
```

Update status transition commands to be backend-conditional.

**Step 2: Update environment variable references**

Replace `LINEAR_TEAM_ID` references with `LEGION_TEAM_ID`. Remove the `bun run packages/daemon/src/state/cli.ts` pipe pattern.

**Step 3: Commit**

```
feat: update controller skill for pluggable backend
```

---

### Task 10: Update Worker Skills for Backend Switching

**Files:**
- Modify: `.opencode/skills/legion-worker/SKILL.md`
- Modify: `.opencode/skills/legion-worker/workflows/architect.md`
- Modify: `.opencode/skills/legion-worker/workflows/plan.md`
- Modify: `.opencode/skills/legion-worker/workflows/implement.md`
- Modify: `.opencode/skills/legion-worker/workflows/review.md`
- Modify: `.opencode/skills/legion-worker/workflows/merge.md`
- Create: `.opencode/skills/legion-worker/references/github-labels.md`
- Modify: `.opencode/skills/legion-worker/references/linear-labels.md`
- Modify: `.opencode/skills/AGENTS.md`

**Step 1: Update worker SKILL.md**

Add `LEGION_ISSUE_BACKEND` to environment section. Replace `LINEAR_ISSUE_ID` references with "issue ID from prompt." Add conditional: if backend is `github`, load `github/SKILL.md` patterns; if `linear`, load `linear/SKILL.md`.

**Step 2: Update each workflow file**

For each workflow, replace hardcoded `linear_linear(...)` calls with backend-conditional patterns. Example for architect.md:

```
# If LEGION_ISSUE_BACKEND=github:
gh issue view $ISSUE_NUMBER --json title,body,labels,comments,state -R $REPO

# If LEGION_ISSUE_BACKEND=linear:
linear_linear(action="get", id=$ISSUE_ID)
```

For implement.md: Remove the auto-transition dependency. Add explicit status update before exit regardless of backend.

**Step 3: Create github-labels.md reference**

Document `gh issue edit --add-label` / `--remove-label` patterns. Note: no read-modify-write needed (unlike Linear).

**Step 4: Update AGENTS.md**

Update the skills structure table, environment variables table, and how-skills-invoke-TypeScript table to reflect the new backend architecture.

**Step 5: Commit**

```
feat: update worker skills for pluggable backend
```

---

## Phase 3: Cleanup

### Task 11: Consolidate Environment Variables

**Files:**
- Modify: `packages/daemon/src/daemon/config.ts`
- Modify: `packages/daemon/src/cli/index.ts`
- Modify: `.opencode/skills/legion-controller/SKILL.md`
- Modify: `.opencode/skills/legion-worker/SKILL.md`

**Step 1: In config.ts**

`loadConfig()` already reads `LEGION_TEAM_ID`. Verify no remaining references to `LINEAR_TEAM_ID` in TypeScript.

**Step 2: Remove `LINEAR_ISSUE_ID` from worker dispatch**

In `cli/index.ts` `cmdDispatch()`, remove any `LINEAR_ISSUE_ID` from the env vars passed to worker sessions.

**Step 3: Run full test suite**

Run: `bun test packages/daemon/ && bunx tsc --noEmit && bunx biome check packages/daemon/src/`
Expected: ALL PASS.

**Step 4: Commit**

```
chore: consolidate environment variables to LEGION_* prefix
```

---

### Task 12: Final Verification

**Step 1: Run full test suite**

Run: `bun test packages/daemon/`
Expected: ALL PASS (250+ tests with new ones).

**Step 2: Type check**

Run: `bunx tsc --noEmit`
Expected: No errors.

**Step 3: Lint**

Run: `bunx biome check packages/daemon/src/`
Expected: No errors.

**Step 4: Verify the design doc claims**

- `decision.ts` has zero changes: `git diff packages/daemon/src/state/decision.ts` should show nothing.
- All Linear tests still pass with relocated code.
- New GitHub parser tests pass.
- `/state/collect` endpoint works for both backends.
- `legion collect-state` command works.

**Step 5: Commit any remaining fixes, then final commit**

```
docs: update AGENTS.md and solution docs for pluggable backend
```
