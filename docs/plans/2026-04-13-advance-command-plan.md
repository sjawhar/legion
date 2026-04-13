# `legion advance` + Auto-Progression Implementation Plan (#494)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a high-level `legion advance` CLI command that moves an issue to its next lifecycle stage, and daemon auto-progression that dispatches the next worker when the current one finishes.

**Architecture:** Extend the `IssueTracker` interface with `transitionIssue()` and `removeLabel()` mutation methods, implemented by both GitHub and Linear backends. Add `POST /state/advance` endpoint to `server.ts` that reads `suggestedAction` from the state cache and executes it (dispatch, transition, or skip). Add auto-progression via Envoy event + poll fallback, opt-in via `LEGION_AUTO_ADVANCE=true`. Wire a `legion advance` CLI command that calls the endpoint.

**Tech Stack:** TypeScript on Bun, citty CLI framework, `gh` CLI for GitHub mutations, Envoy for event routing, bun:test for testing.

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `packages/daemon/src/state/backends/issue-tracker.ts` | Modify | Add `transitionIssue()` and `removeLabel()` to interface |
| `packages/daemon/src/state/backends/github.ts` | Modify | Implement GitHub mutations via `gh` CLI |
| `packages/daemon/src/state/backends/linear.ts` | Modify | Add stub `transitionIssue()` and `removeLabel()` (Linear mutations use MCP, not daemon CLI) |
| `packages/daemon/src/state/backends/index.ts` | Modify | No factory changes needed — methods are optional on interface |
| `packages/daemon/src/daemon/server.ts` | Modify | Add `POST /state/advance` endpoint, `autoAdvanceReadyIssues()`, advance handler logic, add `issueBackend` to `ServerOptions` |
| `packages/daemon/src/daemon/config.ts` | Modify | Add `autoAdvance` flag |
| `packages/daemon/src/daemon/index.ts` | Modify | Wire Envoy subscription for worker-done events, call auto-advance |
| `packages/daemon/src/cli/index.ts` | Modify | Add `cmdAdvance()` + `advanceCommand`, register in `mainCommand` |
| `.opencode/skills/legion-controller/SKILL.md` | Modify | Update to use `legion advance` where appropriate |
| `packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts` | Create | Tests for `transitionIssue()` and `removeLabel()` |
| `packages/daemon/src/daemon/__tests__/advance.test.ts` | Create | Tests for `POST /state/advance` endpoint |
| `packages/daemon/src/daemon/__tests__/auto-advance.test.ts` | Create | Tests for auto-progression logic |

---

## Task 1: Extend IssueTracker Interface with Mutation Methods — Independent

**Files:**
- Modify: `packages/daemon/src/state/backends/issue-tracker.ts`
- Modify: `packages/daemon/src/state/types.ts` (if needed for imports)

- [ ] **Step 1: Add `transitionIssue()` and `removeLabel()` to the IssueTracker interface**

In `packages/daemon/src/state/backends/issue-tracker.ts`, the current interface has only `parseIssues`. Add two optional mutation methods (optional because not all consumers need mutations — the interface is also used for pure parsing):

```typescript
import type { IssueStatusLiteral, ParsedIssue } from "../types";

export type BackendName = "linear" | "github";

export interface IssueTracker {
  /** Parse raw issue data from the tracker into normalized form. */
  parseIssues(raw: unknown): ParsedIssue[];

  /**
   * Transition an issue to a new status in the tracker.
   * Implementations use tracker-native APIs (GitHub Projects V2 GraphQL, Linear API).
   * @throws if the transition fails.
   */
  transitionIssue?(issueId: string, newStatus: IssueStatusLiteral): Promise<void>;

  /**
   * Remove a label from an issue.
   * @throws if the removal fails.
   */
  removeLabel?(issueId: string, label: string): Promise<void>;
}
```

- [ ] **Step 2: Verify types compile**

Run: `bunx tsc --noEmit`
Expected: No new errors.

- [ ] **Step 3: Commit**

```bash
jj describe -m "feat(state): extend IssueTracker interface with transitionIssue and removeLabel (#494)"
jj new
```

---

## Task 2: Implement GitHub Backend Mutations — Depends on: Task 1

**Files:**
- Create: `packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`
- Modify: `packages/daemon/src/state/backends/github.ts`

- [ ] **Step 1: Write failing tests for GitHubTracker.removeLabel()**

Create `packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`:

```typescript
import { describe, expect, it, afterEach } from "bun:test";
import { GitHubTracker } from "../backends/github";

// Save originals for restore
const originalSpawn = Bun.spawn;

function mockSpawn(
  capturedCalls: { cmd: string[] }[],
  responses: { exitCode: number; stdout: string; stderr: string }[] = [{ exitCode: 0, stdout: "", stderr: "" }],
) {
  let callIndex = 0;
  Bun.spawn = ((cmd: string[]) => {
    capturedCalls.push({ cmd: [...cmd] });
    const response = responses[Math.min(callIndex++, responses.length - 1)];
    return {
      stdout: new Blob([response.stdout]).stream(),
      stderr: new Blob([response.stderr]).stream(),
      exited: Promise.resolve(response.exitCode),
    };
  }) as typeof Bun.spawn;
}

afterEach(() => {
  Bun.spawn = originalSpawn;
});

describe("GitHubTracker.removeLabel", () => {
  it("calls gh issue edit with --remove-label for github-format issueId", async () => {
    const calls: { cmd: string[] }[] = [];
    mockSpawn(calls);

    const tracker = new GitHubTracker();
    await tracker.removeLabel("acme-backend-42", "worker-done");

    expect(calls.length).toBe(1);
    expect(calls[0].cmd).toEqual([
      "gh", "issue", "edit", "42",
      "--remove-label", "worker-done",
      "-R", "acme/backend",
    ]);
  });

  it("throws on non-zero exit code", async () => {
    const calls: { cmd: string[] }[] = [];
    mockSpawn(calls, [{ exitCode: 1, stdout: "", stderr: "not found" }]);

    const tracker = new GitHubTracker();
    await expect(tracker.removeLabel("acme-backend-42", "worker-done")).rejects.toThrow();
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`
Expected: FAIL — `removeLabel` is not a function.

- [ ] **Step 3: Implement GitHubTracker.removeLabel()**

In `packages/daemon/src/state/backends/github.ts`, add to the `GitHubTracker` class:

```typescript
import type { IssueStatusLiteral } from "../types";

// Add this helper at module level (near existing parseOwnerRepo):
function parseIssueIdParts(issueId: string): { owner: string; repo: string; number: string } {
  // issueId format: "owner-repo-number" or "owner-repo-number-slug"
  // The owner and repo may contain hyphens, but the number is always a digit sequence.
  // Strategy: find the last numeric segment that's preceded by a hyphen.
  const parts = issueId.split("-");
  // Find the first all-numeric segment (the issue number)
  let numberIdx = -1;
  for (let i = parts.length - 1; i >= 0; i--) {
    if (/^\d+$/.test(parts[i])) {
      numberIdx = i;
      break;
    }
  }
  if (numberIdx < 2) {
    throw new Error(`Cannot parse issueId "${issueId}" — expected format: owner-repo-number`);
  }
  // Everything before numberIdx minus one is owner, the part at numberIdx-1 is part of repo
  // Actually, the convention is: first segment is owner, middle segments are repo, last numeric is number
  // Match the existing buildIssueId pattern: buildIssueId(owner, repo, number) -> `${owner}-${repo}-${number}`
  const owner = parts[0];
  const repo = parts.slice(1, numberIdx).join("-");
  const number = parts[numberIdx];
  return { owner, repo, number };
}

// In the GitHubTracker class:
async removeLabel(issueId: string, label: string): Promise<void> {
  const { owner, repo, number } = parseIssueIdParts(issueId);
  const proc = Bun.spawn(
    ["gh", "issue", "edit", number, "--remove-label", label, "-R", `${owner}/${repo}`],
    { stdout: "pipe", stderr: "pipe" },
  );
  const exitCode = await proc.exited;
  if (exitCode !== 0) {
    const stderr = await new Response(proc.stderr).text();
    throw new Error(`Failed to remove label "${label}" from ${issueId}: ${stderr}`);
  }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`
Expected: PASS

- [ ] **Step 5: Write failing tests for GitHubTracker.transitionIssue()**

Add to the same test file:

```typescript
describe("GitHubTracker.transitionIssue", () => {
  it("calls gh project item-edit via graphql to set status", async () => {
    const calls: { cmd: string[] }[] = [];
    // transitionIssue uses gh api graphql, which spawns a process
    mockSpawn(calls, [
      // First call: query project item ID
      { exitCode: 0, stdout: JSON.stringify({ data: { repository: { issue: { projectItems: { nodes: [{ id: "PVTI_item1", project: { id: "PVT_proj1", field: { id: "PVTSSF_field1", options: [{ id: "opt_todo", name: "Todo" }, { id: "opt_in_progress", name: "In Progress" }] } } }] } } } } }), stderr: "" },
      // Second call: mutation to update status
      { exitCode: 0, stdout: JSON.stringify({ data: { updateProjectV2ItemFieldValue: { projectV2Item: { id: "PVTI_item1" } } } }), stderr: "" },
    ]);

    const tracker = new GitHubTracker();
    await tracker.transitionIssue("acme-backend-42", "In Progress");

    expect(calls.length).toBe(2);
    // First call should be the query
    expect(calls[0].cmd[0]).toBe("gh");
    expect(calls[0].cmd[1]).toBe("api");
    expect(calls[0].cmd[2]).toBe("graphql");
    // Second call should be the mutation
    expect(calls[1].cmd[0]).toBe("gh");
    expect(calls[1].cmd[1]).toBe("api");
    expect(calls[1].cmd[2]).toBe("graphql");
  });

  it("throws when status option not found", async () => {
    const calls: { cmd: string[] }[] = [];
    mockSpawn(calls, [
      { exitCode: 0, stdout: JSON.stringify({ data: { repository: { issue: { projectItems: { nodes: [{ id: "PVTI_item1", project: { id: "PVT_proj1", field: { id: "PVTSSF_field1", options: [{ id: "opt_todo", name: "Todo" }] } } }] } } } } }), stderr: "" },
    ]);

    const tracker = new GitHubTracker();
    await expect(tracker.transitionIssue("acme-backend-42", "Retro")).rejects.toThrow("status option");
  });

  it("throws when issue has no project items", async () => {
    const calls: { cmd: string[] }[] = [];
    mockSpawn(calls, [
      { exitCode: 0, stdout: JSON.stringify({ data: { repository: { issue: { projectItems: { nodes: [] } } } } }), stderr: "" },
    ]);

    const tracker = new GitHubTracker();
    await expect(tracker.transitionIssue("acme-backend-42", "Todo")).rejects.toThrow("no project");
  });
});
```

- [ ] **Step 6: Run test to verify it fails**

Run: `bun test packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`
Expected: FAIL — `transitionIssue` is not a function.

- [ ] **Step 7: Implement GitHubTracker.transitionIssue()**

In `packages/daemon/src/state/backends/github.ts`, add to the `GitHubTracker` class:

```typescript
async transitionIssue(issueId: string, newStatus: IssueStatusLiteral): Promise<void> {
  const { owner, repo, number } = parseIssueIdParts(issueId);

  // Step 1: Query the project item ID, project ID, Status field ID, and option IDs
  const queryResult = await this.runGraphQL(`query {
    repository(owner: "${owner}", name: "${repo}") {
      issue(number: ${number}) {
        projectItems(first: 10) {
          nodes {
            id
            project {
              id
              field(name: "Status") {
                ... on ProjectV2SingleSelectField {
                  id
                  options { id name }
                }
              }
            }
          }
        }
      }
    }
  }`);

  const projectItems = queryResult?.data?.repository?.issue?.projectItems?.nodes;
  if (!projectItems || projectItems.length === 0) {
    throw new Error(`transitionIssue: ${issueId} has no project items`);
  }

  // Use the first project item that has a Status field
  const item = projectItems.find(
    (n: Record<string, unknown>) => (n.project as Record<string, unknown>)?.field != null,
  );
  if (!item) {
    throw new Error(`transitionIssue: ${issueId} has no project with Status field`);
  }

  const projectId = item.project.id;
  const fieldId = item.project.field.id;
  const options = item.project.field.options as { id: string; name: string }[];
  const targetOption = options.find((o) => o.name === newStatus);
  if (!targetOption) {
    const available = options.map((o) => o.name).join(", ");
    throw new Error(
      `transitionIssue: status option "${newStatus}" not found for ${issueId}. Available: ${available}`,
    );
  }

  // Step 2: Mutation to update the status
  await this.runGraphQL(`mutation {
    updateProjectV2ItemFieldValue(input: {
      projectId: "${projectId}"
      itemId: "${item.id}"
      fieldId: "${fieldId}"
      value: { singleSelectOptionId: "${targetOption.id}" }
    }) {
      projectV2Item { id }
    }
  }`);
}

private async runGraphQL(query: string): Promise<Record<string, unknown>> {
  const proc = Bun.spawn(
    ["gh", "api", "graphql", "-f", `query=${query}`],
    { stdout: "pipe", stderr: "pipe" },
  );
  const exitCode = await proc.exited;
  const stdout = await new Response(proc.stdout).text();
  if (exitCode !== 0) {
    const stderr = await new Response(proc.stderr).text();
    throw new Error(`GraphQL query failed: ${stderr}`);
  }
  return JSON.parse(stdout);
}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `bun test packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`
Expected: PASS

- [ ] **Step 9: Run full type check**

Run: `bunx tsc --noEmit`
Expected: No new errors.

- [ ] **Step 10: Commit**

```bash
jj describe -m "feat(github): implement transitionIssue and removeLabel for GitHub backend (#494)"
jj new
```

---

## Task 3: Implement Linear Backend Mutations — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/state/backends/linear.ts`
- Add to: `packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`

- [ ] **Step 1: Write failing tests for LinearTracker mutations**

Add to `packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`:

```typescript
import { LinearTracker } from "../backends/linear";

describe("LinearTracker.transitionIssue", () => {
  it("is defined and throws not-implemented (Linear mutations use MCP, not CLI)", async () => {
    const tracker = new LinearTracker();
    // Linear status transitions are handled by the controller via Linear MCP,
    // not by the daemon. This is a stub that throws to make it explicit.
    await expect(tracker.transitionIssue("ENG-42", "In Progress")).rejects.toThrow(
      "not implemented",
    );
  });
});

describe("LinearTracker.removeLabel", () => {
  it("is defined and throws not-implemented", async () => {
    const tracker = new LinearTracker();
    await expect(tracker.removeLabel("ENG-42", "worker-done")).rejects.toThrow("not implemented");
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`
Expected: FAIL — `transitionIssue` is not a function.

- [ ] **Step 3: Implement LinearTracker stubs**

In `packages/daemon/src/state/backends/linear.ts`, add to the `LinearTracker` class:

```typescript
import type { IssueStatusLiteral } from "../types";

async transitionIssue(_issueId: string, _newStatus: IssueStatusLiteral): Promise<void> {
  throw new Error(
    "LinearTracker.transitionIssue not implemented — Linear status transitions are handled by the controller via Linear MCP, not by the daemon CLI.",
  );
}

async removeLabel(_issueId: string, _label: string): Promise<void> {
  throw new Error(
    "LinearTracker.removeLabel not implemented — Linear label mutations are handled by the controller via Linear MCP, not by the daemon CLI.",
  );
}
```

**Design note:** The Linear backend cannot use CLI tools for mutations — it requires the Linear MCP tool which is only available in agent sessions. The `advance` command will initially only support GitHub backend. The Linear stubs throw explicitly so callers know this is intentional, not missing.

- [ ] **Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
jj describe -m "feat(linear): add stub transitionIssue and removeLabel for Linear backend (#494)"
jj new
```

---

## Task 4: Add `autoAdvance` Config Flag — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/config.ts`
- Modify: `packages/daemon/src/daemon/__tests__/config.test.ts`

- [ ] **Step 1: Write failing test for autoAdvance config**

Add to the existing `config.test.ts` file — find the section testing env var resolution and add:

```typescript
it("reads LEGION_AUTO_ADVANCE from env", () => {
  const { config } = resolveDaemonConfig({
    env: {
      LEGION_ID: "test/1",
      LEGION_AUTO_ADVANCE: "true",
    },
  });
  expect(config.autoAdvance).toBe(true);
});

it("defaults autoAdvance to false", () => {
  const { config } = resolveDaemonConfig({
    env: { LEGION_ID: "test/1" },
  });
  expect(config.autoAdvance).toBe(false);
});

it("reads autoAdvance from yaml config", () => {
  const yaml = `
team: test/1
autoAdvance: true
`;
  const config = loadConfigFromFile(yaml, "/tmp");
  expect(config.autoAdvance).toBe(true);
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/config.test.ts`
Expected: FAIL — `autoAdvance` is undefined.

- [ ] **Step 3: Add autoAdvance to DaemonConfig interface**

In `packages/daemon/src/daemon/config.ts`:

1. Add to the `DaemonConfig` interface:
```typescript
autoAdvance: boolean;
```

2. Add to the env var resolution in `resolveDaemonConfig()`:
```typescript
autoAdvance: env.LEGION_AUTO_ADVANCE === "true",
```

3. Add to the YAML config schema in `loadConfigFromFile()`:
```typescript
// In the known keys and parsing section:
autoAdvance: parsed.autoAdvance === true,
```

4. Add to the defaults:
```typescript
autoAdvance: false,
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/config.test.ts`
Expected: PASS

- [ ] **Step 5: Run full type check**

Run: `bunx tsc --noEmit`
Expected: No new errors (may need to add `autoAdvance` to places that construct DaemonConfig).

- [ ] **Step 6: Commit**

```bash
jj describe -m "feat(config): add autoAdvance flag to DaemonConfig (#494)"
jj new
```

---

## Task 5: Add `POST /state/advance` Endpoint — Depends on: Task 1, Task 2, Task 3, Task 4

**Files:**
- Create: `packages/daemon/src/daemon/__tests__/advance.test.ts`
- Modify: `packages/daemon/src/daemon/server.ts`

**Important context:**
- `ServerOptions` does NOT currently have `issueBackend`. Add `issueBackend?: BackendName` to `ServerOptions` so the advance handler knows which backend to use.
- All state (issueStateCache, workers, etc.) is closure-scoped inside `startServer()`. New handlers in the same scope access them directly.
- `IssueState` has a `.source` field with `{ owner, repo, number }` — use this for repo resolution, NOT issueId parsing (which is ambiguous for repos with hyphens).
- The POST /workers handler is an inline block inside the `Bun.serve({ fetch() {} })` closure. To reuse dispatch logic from the advance handler, extract core logic into a shared function.

- [ ] **Step 1: Add `issueBackend` to ServerOptions**

In `packages/daemon/src/daemon/server.ts`, add to the `ServerOptions` interface:

```typescript
issueBackend?: BackendName;
```

Import `BackendName` from `../state/backends/issue-tracker`.

Update `startServer` callers in `index.ts` to pass `issueBackend: config.issueBackend`.

- [ ] **Step 2: Write failing tests for POST /state/advance**

Create `packages/daemon/src/daemon/__tests__/advance.test.ts`. Follow the exact test infrastructure patterns from `server.test.ts` — copy `startTestServer()`, `requestJson()`, `makeAdapter()`, and the `afterEach` cleanup. For seeding state, use `POST /state/collect` with crafted issue data that produces the desired `suggestedAction`:

```typescript
import { describe, expect, it, afterEach } from "bun:test";
// Copy test infrastructure from server.test.ts (startTestServer, requestJson, makeAdapter, etc.)

const originalFetch = globalThis.fetch;
const originalSpawn = Bun.spawn;

afterEach(() => {
  globalThis.fetch = originalFetch;
  Bun.spawn = originalSpawn;
  // Stop server, clean temp dirs
});

describe("POST /state/advance", () => {
  it("returns 412 when issue not in state cache", async () => {
    await startTestServer({ issueBackend: "github" });
    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "unknown-issue-99" }),
    });
    expect(response.status).toBe(412);
    const body = (await response.json()) as { error: string };
    expect(body.error).toBe("issue_not_in_cache");
  });

  it("returns skipped when suggestedAction is skip", async () => {
    await startTestServer({ issueBackend: "github" });
    // Seed cache via POST /state/collect with issue data that produces "skip"
    // (e.g., Backlog status, no worker-done, no live worker)
    await seedIssueCache("acme-backend-42", "skip");

    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as { executed: string; action: string };
    expect(body.executed).toBe("skipped");
    expect(body.action).toBe("skip");
  });

  it("dispatches worker for dispatch_planner action", async () => {
    await startTestServer({ issueBackend: "github" });
    // Seed cache with Todo issue, no worker-done, no live worker -> dispatch_planner
    await seedIssueCache("acme-backend-42", "dispatch_planner");

    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as { executed: string; action: string; workerId?: string };
    expect(body.executed).toBe("dispatched");
    expect(body.action).toBe("dispatch_planner");
    expect(body.workerId).toBeDefined();
  });

  it("returns 409 when worker already running for dispatch action", async () => {
    await startTestServer({ issueBackend: "github" });
    // Seed a running worker first, then seed cache with dispatch action + hasLiveWorker=true
    await seedIssueCache("acme-backend-42", "dispatch_planner", { hasLiveWorker: true });

    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    expect(response.status).toBe(409);
  });

  it("executes transition_to_testing by calling tracker methods", async () => {
    await startTestServer({ issueBackend: "github" });
    await seedIssueCache("acme-backend-42", "transition_to_testing");

    // Mock Bun.spawn for gh CLI calls (transitionIssue + removeLabel)
    const spawnCalls: { cmd: string[] }[] = [];
    mockSpawnForGh(spawnCalls);

    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as { executed: string; newStatus: string };
    expect(body.executed).toBe("transitioned");
    expect(body.newStatus).toBe("Testing");
  });
});
```

**Note:** The `seedIssueCache` helper should call `POST /state/collect` with crafted GitHub project item data that the GitHubTracker will parse into the desired state. The exact seed data depends on how `buildIssueState()` computes actions — refer to `decision.test.ts` for the input combinations that produce each `suggestedAction`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/advance.test.ts`
Expected: FAIL — 404 (route not found).

- [ ] **Step 3: Implement the advance handler in server.ts**

In `packages/daemon/src/daemon/server.ts`, add the route handler. Find the route registration section (the `if/else` chain in the `fetch()` handler) and add a new branch:

```typescript
// Add near the other /state/* routes (after POST /state/fetch-and-collect)
} else if (method === "POST" && segments[0] === "state" && segments[1] === "advance") {
  return handleAdvance(request);
}
```

Add the transition mapping constant (inside `startServer()` near the top, alongside other constants):

```typescript
const TRANSITION_ACTION_TO_STATUS: Partial<Record<ActionType, IssueStatusLiteral>> = {
  transition_to_todo: "Todo",
  transition_to_in_progress: "In Progress",
  transition_to_testing: "Testing",
  transition_to_needs_review: "Needs Review",
  transition_to_retro: "Retro",
  transition_to_done: "Done",
};
```

Add prompt construction helpers (inside `startServer()` closure):

```typescript
function buildDefaultPrompt(issueId: string, mode: WorkerModeLiteral, repo: string | undefined): string {
  const backend = opts.issueBackend ?? "github";
  const backendSuffix = repo ? ` (${backend} backend, repo: ${repo})` : "";
  return `Invoke the /legion-worker skill for ${mode} mode for ${issueId}${backendSuffix}. Before starting, check for project-specific skills that may be relevant to this work.`;
}

function buildResumePrompt(action: ActionType, issueId: string, mode: WorkerModeLiteral, repo: string | undefined): string {
  const backend = opts.issueBackend ?? "github";
  const backendSuffix = repo ? ` (${backend} backend, repo: ${repo})` : "";
  switch (action) {
    case "resume_implementer_for_changes":
      return `Invoke the /legion-worker skill for implement mode. The reviewer has requested changes on your PR — check the review comments and address them${backendSuffix}.`;
    case "resume_implementer_for_retro":
      return `/legion-retro`;
    case "resume_implementer_for_ci_failure":
      return `Invoke the /legion-worker skill for implement mode. CI is failing on your PR — check the failures and fix${backendSuffix}.`;
    case "resume_implementer_for_test_failure":
      return `Invoke the /legion-worker skill for implement mode. The tester found issues — check the test feedback and fix${backendSuffix}.`;
    default:
      return buildDefaultPrompt(issueId, mode, repo);
  }
}
```

Then add the handler function (inside `startServer()`, before the `Bun.serve()` block or inside the `fetch()` handler as a local function):

```typescript
async function handleAdvance(request: Request): Promise<Response> {
  const payload = (await request.json()) as { issueId?: string; stage?: string };

  if (!payload.issueId) {
    return Response.json({ error: "missing_issue_id" }, { status: 400 });
  }

  const issueId = payload.issueId.toLowerCase();
  const cachedState = issueStateCache.get(issueId);

  if (!cachedState) {
    return Response.json(
      { error: "issue_not_in_cache", message: "Run 'legion poll <team>' first" },
      { status: 412 },
    );
  }

  // Resolve repo from cached source (canonical — no ambiguous issueId parsing)
  const repo = cachedState.source
    ? `${cachedState.source.owner}/${cachedState.source.repo}`
    : undefined;

  // If --stage was provided, delegate to dispatch with force
  if (payload.stage) {
    const mode = payload.stage as WorkerModeLiteral;
    return handleWorkerCreate({ issueId, mode, force: true, repo });
  }

  const action = cachedState.suggestedAction;

  // Skip/retry/investigate — return without executing
  if (action === "skip" || action === "retry_pr_check" || action === "retry_ci_check") {
    return Response.json({ action, executed: "skipped", reason: `Issue is not ready to advance: ${action}` });
  }
  if (action === "investigate_no_pr") {
    return Response.json({ action, executed: "error", reason: "Issue has worker-done but no PR — needs investigation" });
  }
  if (action === "add_needs_approval") {
    return Response.json({ action, executed: "skipped", reason: "Issue needs approval before advancing" });
  }

  // Check for live worker before dispatch
  if (cachedState.hasLiveWorker && (action.startsWith("dispatch_") || action.startsWith("resume_"))) {
    return Response.json(
      { error: "worker_already_running", action, workerId: `${issueId}-${ACTION_TO_MODE[action]}` },
      { status: 409 },
    );
  }

  // Dispatch actions — dispatch a new worker
  if (action.startsWith("dispatch_") || action === "relay_user_feedback" || action === "remove_worker_active_and_redispatch") {
    const mode = ACTION_TO_MODE[action];
    const prompt = buildDefaultPrompt(issueId, mode, repo);
    const dispatchResponse = await handleWorkerCreate({ issueId, mode, repo, prompt });
    const dispatchResult = (await dispatchResponse.json()) as Record<string, unknown>;

    if (!dispatchResponse.ok) {
      return Response.json(
        { action, executed: "error", reason: dispatchResult.error ?? "dispatch failed", ...dispatchResult },
        { status: dispatchResponse.status },
      );
    }
    return Response.json({
      action, executed: "dispatched",
      workerId: dispatchResult.id, sessionId: dispatchResult.sessionId, port: dispatchResult.port,
    });
  }

  // Transition actions — update issue status in tracker
  if (action.startsWith("transition_to_")) {
    const targetStatus = TRANSITION_ACTION_TO_STATUS[action];
    if (!targetStatus) {
      return Response.json({ action, executed: "error", reason: `Unknown transition action: ${action}` }, { status: 500 });
    }

    try {
      const tracker = getBackend(opts.issueBackend ?? "github");
      if (tracker.transitionIssue) {
        await tracker.transitionIssue(issueId, targetStatus);
      }
      if (tracker.removeLabel) {
        await tracker.removeLabel(issueId, "worker-done");
      }
    } catch (err) {
      return Response.json(
        { action, executed: "error", reason: `Transition failed: ${err instanceof Error ? err.message : String(err)}` },
        { status: 500 },
      );
    }
    return Response.json({ action, executed: "transitioned", newStatus: targetStatus });
  }

  // Resume actions — resume an existing worker session
  if (action.startsWith("resume_")) {
    const mode = ACTION_TO_MODE[action];
    const prompt = buildResumePrompt(action, issueId, mode, repo);
    const dispatchResponse = await handleWorkerCreate({ issueId, mode, repo, prompt });
    const dispatchResult = (await dispatchResponse.json()) as Record<string, unknown>;

    if (!dispatchResponse.ok) {
      return Response.json(
        { action, executed: "error", reason: dispatchResult.error ?? "resume failed", ...dispatchResult },
        { status: dispatchResponse.status },
      );
    }
    return Response.json({
      action, executed: "dispatched",
      workerId: dispatchResult.id, sessionId: dispatchResult.sessionId, port: dispatchResult.port,
    });
  }

  return Response.json({ action, executed: "skipped", reason: `Unhandled action: ${action}` });
}
```

**Important refactoring note:** The existing `POST /workers` handler is a giant inline block (~330 lines) inside the `Bun.serve({ fetch() {} })` closure. To reuse its dispatch logic from `handleAdvance`, extract the core dispatch logic into a shared `handleWorkerCreate(body)` function that accepts the parsed body and returns a `Response`. This is a **refactor-first step** — extract the existing handler body into a function that both routes call.

- [ ] **Step 4: Refactor POST /workers handler to extract shared dispatch logic**

Extract the body of the `POST /workers` route into a `handleWorkerCreate(payload)` function that accepts the parsed body and returns a `Response`. Both the existing `POST /workers` route and the new `handleAdvance` call this function.

The route handler becomes:
```typescript
} else if (method === "POST" && segments[0] === "workers" && segments.length === 1) {
  const payload = await request.json();
  return handleWorkerCreate(payload);
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/advance.test.ts`
Expected: PASS

- [ ] **Step 6: Run full test suite**

Run: `bun test`
Expected: All existing tests still pass.

- [ ] **Step 7: Commit**

```bash
jj describe -m "feat(daemon): add POST /state/advance endpoint with dispatch and transition support (#494)"
jj new
```

---

## Task 6: Add `legion advance` CLI Command — Depends on: Task 5

**Files:**
- Modify: `packages/daemon/src/cli/index.ts`

- [ ] **Step 1: Implement cmdAdvance and advanceCommand**

In `packages/daemon/src/cli/index.ts`, add:

```typescript
async function cmdAdvance(
  issue: string,
  advanceOpts: {
    stage?: string;
    repo?: string;
    dryRun?: boolean;
    daemonPort?: number;
  },
): Promise<void> {
  const daemonPort = advanceOpts.daemonPort ?? (await getDaemonPort());
  const baseUrl = `http://127.0.0.1:${daemonPort}`;

  // Health check first (follows cmdDispatch pattern)
  try {
    const healthResp = await fetch(`${baseUrl}/health`);
    if (!healthResp.ok) {
      throw new CliError("Daemon is not healthy. Is it running?");
    }
  } catch (error) {
    if (error instanceof CliError) throw error;
    throw new CliError(`Could not connect to daemon. Is it running?\nTried: ${baseUrl}/health`);
  }

  // If --stage provided, delegate to dispatch with --force
  if (advanceOpts.stage) {
    return cmdDispatch(issue, advanceOpts.stage, {
      force: true,
      repo: advanceOpts.repo,
      daemonPort,
    });
  }

  const body: Record<string, unknown> = { issueId: issue };
  if (advanceOpts.repo) body.repo = advanceOpts.repo;

  if (advanceOpts.dryRun) {
    // Fetch state to show what would happen
    const stateResp = await fetch(`${baseUrl}/state/materialized`);
    const state = await stateResp.json();
    const issueState = state.issues?.[issue] ?? state.issues?.[issue.toLowerCase()];
    if (!issueState) {
      console.error(`Issue ${issue} not found in state cache.`);
      process.exit(1);
    }
    console.log(`Dry run: would execute action "${issueState.suggestedAction}" for ${issue}`);
    return;
  }

  const response = await fetch(`${baseUrl}/state/advance`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });

  const result = await response.json();

  if (!response.ok) {
    if (response.status === 412) {
      console.error(`Issue ${issue} not in state cache. Run: legion poll <team>`);
    } else if (response.status === 409) {
      console.error(`Worker already running for ${issue}: ${result.workerId}`);
    } else {
      console.error(`Advance failed: ${JSON.stringify(result)}`);
    }
    process.exit(1);
  }

  switch (result.executed) {
    case "dispatched":
      console.log(`Dispatched ${result.action} → worker ${result.workerId} (session ${result.sessionId})`);
      break;
    case "transitioned":
      console.log(`Transitioned ${issue} → ${result.newStatus}`);
      break;
    case "skipped":
      console.log(`Skipped: ${result.reason}`);
      break;
    case "error":
      console.error(`Error: ${result.reason}`);
      process.exit(1);
      break;
  }
}

const advanceCommand = defineCommand({
  meta: {
    name: "advance",
    description: "Advance an issue to its next lifecycle stage",
  },
  args: {
    issue: {
      type: "positional",
      description: "Issue identifier (e.g., sjawhar-legion-494)",
      required: true,
    },
    stage: {
      type: "string",
      description: "Force advance to specific stage (architect|plan|implement|test|review|merge)",
    },
    repo: {
      type: "string",
      alias: "r",
      description: "Repository (owner/repo)",
    },
    "dry-run": {
      type: "boolean",
      description: "Print action without executing",
      default: false,
    },
    "daemon-port": {
      type: "string",
      description: "Override daemon port",
    },
  },
  async run({ args }) {
    await cmdAdvance(args.issue, {
      stage: args.stage,
      repo: args.repo,
      dryRun: args["dry-run"],
      daemonPort: args["daemon-port"] ? Number(args["daemon-port"]) : undefined,
    });
  },
});
```

- [ ] **Step 2: Register in mainCommand**

In the `mainCommand` definition, add `advance: advanceCommand` to the `subCommands` object:

```typescript
subCommands: {
  start: startCommand,
  stop: stopCommand,
  status: statusCommand,
  attach: attachCommand,
  adopt: adoptCommand,
  advance: advanceCommand,  // NEW
  dispatch: dispatchCommand,
  prompt: promptCommand,
  // ... rest
},
```

- [ ] **Step 3: Verify CLI registration works**

Run: `bun run packages/daemon/src/cli/index.ts advance --help`
Expected: Shows help text with issue positional arg and options.

- [ ] **Step 4: Run type check**

Run: `bunx tsc --noEmit`
Expected: No errors.

- [ ] **Step 5: Commit**

```bash
jj describe -m "feat(cli): add 'legion advance' command for high-level lifecycle transitions (#494)"
jj new
```

---

## Task 7: Wire Auto-Progression in Daemon — Depends on: Task 4, Task 5

**Files:**
- Create: `packages/daemon/src/daemon/__tests__/auto-advance.test.ts`
- Modify: `packages/daemon/src/daemon/server.ts`
- Modify: `packages/daemon/src/daemon/index.ts`

- [ ] **Step 1: Write failing tests for autoAdvanceReadyIssues()**

Create `packages/daemon/src/daemon/__tests__/auto-advance.test.ts`:

```typescript
import { describe, expect, it, afterEach } from "bun:test";
// Import/setup test server following server.test.ts patterns

describe("autoAdvanceReadyIssues", () => {
  it("dispatches workers for issues with dispatch_* suggestedAction when autoAdvance is enabled", async () => {
    await startTestServer({ autoAdvance: true });

    // Seed two issues — one dispatch-ready, one skip
    await seedIssueState("acme-backend-42", {
      suggestedAction: "dispatch_planner",
      status: "Todo",
      hasLiveWorker: false,
      source: { owner: "acme", repo: "backend", number: 42 },
    });
    await seedIssueState("acme-backend-43", {
      suggestedAction: "skip",
      status: "Backlog",
      hasLiveWorker: false,
    });

    // Trigger auto-advance (simulating post-collection processing)
    const response = await requestJson("/state/auto-advance", { method: "POST" });
    expect(response.status).toBe(200);
    const body = await response.json();
    expect(body.advanced).toHaveLength(1);
    expect(body.advanced[0].issueId).toBe("acme-backend-42");
  });

  it("skips issues with live workers", async () => {
    await startTestServer({ autoAdvance: true });
    await seedIssueState("acme-backend-42", {
      suggestedAction: "dispatch_planner",
      status: "Todo",
      hasLiveWorker: true,
    });

    const response = await requestJson("/state/auto-advance", { method: "POST" });
    expect(response.status).toBe(200);
    const body = await response.json();
    expect(body.advanced).toHaveLength(0);
  });

  it("does nothing when autoAdvance is disabled", async () => {
    await startTestServer({ autoAdvance: false });
    await seedIssueState("acme-backend-42", {
      suggestedAction: "dispatch_planner",
      status: "Todo",
      hasLiveWorker: false,
    });

    const response = await requestJson("/state/auto-advance", { method: "POST" });
    expect(response.status).toBe(200);
    const body = await response.json();
    expect(body.advanced).toHaveLength(0);
    expect(body.reason).toBe("auto_advance_disabled");
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/auto-advance.test.ts`
Expected: FAIL — route not found.

- [ ] **Step 3: Implement autoAdvanceReadyIssues() in server.ts**

Add the auto-advance logic and an internal endpoint for testing:

```typescript
// In server.ts, add constant for auto-advanceable actions:
const AUTO_ADVANCE_ACTIONS: ReadonlySet<ActionType> = new Set([
  "dispatch_architect",
  "dispatch_planner",
  "dispatch_implementer",
  "dispatch_implementer_for_retro",
  "dispatch_tester",
  "dispatch_reviewer",
  "dispatch_merger",
  "transition_to_todo",
  "transition_to_in_progress",
  "transition_to_testing",
  "transition_to_needs_review",
  "transition_to_retro",
]);

async function autoAdvanceReadyIssues(): Promise<Array<{ issueId: string; action: ActionType; result: string }>> {
  if (!opts.autoAdvance) {
    return [];
  }

  const advanced: Array<{ issueId: string; action: ActionType; result: string }> = [];

  for (const [issueId, issueState] of issueStateCache) {
    if (!AUTO_ADVANCE_ACTIONS.has(issueState.suggestedAction)) continue;
    if (issueState.hasLiveWorker) continue;

    try {
      // Call the advance handler internally
      const advanceRequest = new Request("http://localhost/state/advance", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ issueId }),
      });
      const response = await handleAdvance(new URL(advanceRequest.url), advanceRequest);
      const result = await response.json();
      advanced.push({
        issueId,
        action: issueState.suggestedAction,
        result: result.executed ?? "unknown",
      });
    } catch (err) {
      console.error(`[auto-advance] ${issueId}: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  return advanced;
}

// Add route for testing (and for health tick to call internally):
} else if (method === "POST" && segments[0] === "state" && segments[1] === "auto-advance") {
  if (!opts.autoAdvance) {
    return Response.json({ advanced: [], reason: "auto_advance_disabled" });
  }
  const results = await autoAdvanceReadyIssues();
  return Response.json({ advanced: results });
}
```

- [ ] **Step 4: Wire auto-advance into the health tick**

In `server.ts`, modify the `runPostCollectionProcessing` function to call auto-advance after state update:

```typescript
// At the end of runPostCollectionProcessing, after all existing logic:
if (opts.autoAdvance) {
  // Fire-and-forget auto-advance (log errors, don't throw)
  autoAdvanceReadyIssues().catch((err) => {
    console.error(`[auto-advance] Error: ${err instanceof Error ? err.message : String(err)}`);
  });
}
```

- [ ] **Step 5: Wire Envoy subscriber for worker-done events in index.ts**

In `packages/daemon/src/daemon/index.ts`, add Envoy subscription for worker-done events when auto-advance is enabled:

```typescript
// After the controller Envoy setup (around line 562), add:
if (config.autoAdvance && config.envoyUrl) {
  // Subscribe daemon to worker-done events for fast-path auto-progression
  try {
    const daemonSessionId = `daemon-${config.legionId}`;
    await fetch(`${config.envoyUrl}/v1/sessions/${encodeURIComponent(daemonSessionId)}/subscribe`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ topics: ["notifications.role.legion-controller"] }),
    });
    console.log("[auto-advance] Subscribed to worker-done events via Envoy");

    // Poll for messages (simple approach: check periodically)
    // Actually, use the existing Envoy webhook pattern if available,
    // or add a polling loop in the health tick.
    // For now, rely on the health tick fallback (60s) since
    // the Envoy integration for the daemon is not yet established.
  } catch (err) {
    console.error(`[auto-advance] Failed to subscribe to Envoy: ${err instanceof Error ? err.message : String(err)}`);
    // Non-fatal — fall back to health tick polling
  }
}
```

**Note:** The Envoy fast path is best-effort. The primary mechanism is the health tick fallback which already runs `fetchAndProcessState()` → `runPostCollectionProcessing()` → `autoAdvanceReadyIssues()` every 60s. The Envoy subscription is the fast path for sub-second response when a worker signals completion.

- [ ] **Step 6: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/auto-advance.test.ts`
Expected: PASS

- [ ] **Step 7: Run full test suite**

Run: `bun test`
Expected: All tests pass.

- [ ] **Step 8: Commit**

```bash
jj describe -m "feat(daemon): add auto-progression with Envoy fast path and poll fallback (#494)"
jj new
```

---

## Task 8: Update Controller Skill — Depends on: Task 5, Task 6

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`

- [ ] **Step 1: Read the current controller skill**

Read `.opencode/skills/legion-controller/SKILL.md` to understand the full dispatch flow that needs updating.

- [ ] **Step 2: Update the controller skill to use `legion advance`**

Add a new section documenting `legion advance` as the preferred dispatch mechanism, while keeping manual dispatch as fallback:

Add to the controller skill near the dispatch section:

```markdown
### Using `legion advance` (Preferred)

When auto-progression is disabled, use `legion advance` instead of manual dispatch sequences:

```bash
# Advance an issue to its next stage (reads suggestedAction from daemon cache)
legion advance "$ISSUE_IDENTIFIER"

# Force a specific stage
legion advance "$ISSUE_IDENTIFIER" --stage implement

# See what would happen without doing it
legion advance "$ISSUE_IDENTIFIER" --dry-run
```

`advance` handles:
- Reading `suggestedAction` from the daemon's state cache
- Dispatching workers with correct mode and prompt
- Executing `transition_to_*` actions (status change + label cleanup)
- Resuming workers for `resume_*` actions
- Returning actionable messages for skip/investigate actions

This replaces manual dispatch sequences in the loop body. The controller's role shifts to:
1. Calling `POST /state/fetch-and-collect` to refresh state
2. Iterating over actionable issues
3. Calling `legion advance` for each
4. Handling exceptions (blocked issues, quality concerns)

### Auto-Progression

When `LEGION_AUTO_ADVANCE=true` is set:
- The daemon auto-dispatches the next worker when the current one finishes
- The controller only needs to handle exceptions, triage, and priority changes
- Check `LEGION_AUTO_ADVANCE` env var before manual dispatch — skip if auto-advance is handling it

```bash
if [ "$LEGION_AUTO_ADVANCE" = "true" ]; then
  # Only handle exceptions — auto-advance handles normal flow
else
  # Use legion advance for each actionable issue
fi
```
```

- [ ] **Step 3: Commit**

```bash
jj describe -m "docs(controller): update skill to document legion advance and auto-progression (#494)"
jj new
```

---

## Task 9: Wire issueBackend Through to Server — Depends on: Task 5

**Files:**
- Modify: `packages/daemon/src/daemon/index.ts` (pass `issueBackend` to `startServer`)

- [ ] **Step 1: Pass issueBackend when constructing ServerOptions in startDaemon**

In `packages/daemon/src/daemon/index.ts`, find where `startServer()` is called and add `issueBackend: config.issueBackend` to the options object.

- [ ] **Step 2: Verify type check passes**

Run: `bunx tsc --noEmit`
Expected: No errors.

- [ ] **Step 3: Commit**

```bash
jj describe -m "feat(daemon): wire issueBackend through ServerOptions to advance handler (#494)"
jj new
```

---

## Task 10: Final Integration Test & Lint — Depends on: All previous tasks

**Files:** None new

- [ ] **Step 1: Run full test suite**

Run: `bun test`
Expected: All tests pass (existing + new).

- [ ] **Step 2: Run linter**

Run: `bunx biome check src/ --write`
Expected: No errors.

- [ ] **Step 3: Run type check**

Run: `bunx tsc --noEmit`
Expected: No errors.

- [ ] **Step 4: Final commit**

```bash
jj describe -m "chore: final lint and type fixes for advance command (#494)"
jj new
```

---

## Testing Plan

### Setup
- `bun install` (if needed)
- Tests run via `bun test` (no server/infra needed — all tests use in-process Bun server on port 0)

### Health Check
- `bun test --timeout 30000` should complete within the timeout
- `bunx tsc --noEmit` returns 0

### Verification Steps

1. **IssueTracker interface extension**
   - Action: Run `bun test packages/daemon/src/state/__tests__/issue-tracker-mutations.test.ts`
   - Expected: All tests pass — `removeLabel`, `transitionIssue` for GitHub, stubs for Linear
   - Tool: CLI

2. **POST /state/advance endpoint**
   - Action: Run `bun test packages/daemon/src/daemon/__tests__/advance.test.ts`
   - Expected: All tests pass — 412 for missing issue, 409 for live worker, 200 for dispatch/transition/skip
   - Tool: CLI

3. **Auto-progression**
   - Action: Run `bun test packages/daemon/src/daemon/__tests__/auto-advance.test.ts`
   - Expected: All tests pass — auto-advances dispatch-ready issues, skips live workers, respects config flag
   - Tool: CLI

4. **Config flag**
   - Action: Run `bun test packages/daemon/src/daemon/__tests__/config.test.ts`
   - Expected: All tests pass — autoAdvance reads from env and yaml
   - Tool: CLI

5. **CLI command registration**
   - Action: Run `bun run packages/daemon/src/cli/index.ts advance --help`
   - Expected: Shows help text with issue arg and options
   - Tool: CLI

6. **Full regression**
   - Action: Run `bun test`
   - Expected: All tests pass (existing + new)
   - Tool: CLI

7. **Type safety**
   - Action: Run `bunx tsc --noEmit`
   - Expected: No errors
   - Tool: CLI

8. **Lint**
   - Action: Run `bunx biome check src/`
   - Expected: No errors
   - Tool: CLI

### Skills to Invoke
- No project-specific testing skills identified beyond standard `bun test`.

### Tools Needed
- Bun runtime (for tests and type checking)
- Biome (for linting)
