# Production Hardening Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Harden Legion for unattended autonomous operation by adding fail-closed agent restrictions, Zod-validated I/O with graceful recovery, bidirectional task persistence, and fixing documentation drift.

**Architecture:** Six tasks across two packages. Tasks 1-4 are in the plugin package, Tasks 5-6 are in the daemon package. Task 4 depends on Task 3 (Zod-validated task storage); all others are independent. The plugin package already has Zod patterns to follow (`config/index.ts`, `tools/task/types.ts`). The daemon package needs Zod added as a dependency.

**Tech Stack:** TypeScript, Bun test runner, Zod (via `@opencode-ai/plugin`'s `tool.schema` re-export in plugin; direct `zod` dep in daemon), atomic file I/O

---

## Task 1: Fail-closed agent restrictions

Unknown agents currently get empty restrictions (everything allowed). Flip to deny-by-default so typos and new agents are safe by default, and add an explicit delegator allowlist.

**Files:**
- Modify: `packages/opencode-plugin/src/delegation/agent-restrictions.ts`
- Modify: `packages/opencode-plugin/src/delegation/delegation-tool.ts:44-49`
- Modify: `packages/opencode-plugin/src/delegation/__tests__/agent-restrictions.test.ts`

### Step 1: Update tests to expect fail-closed behavior

In `packages/opencode-plugin/src/delegation/__tests__/agent-restrictions.test.ts`, change the two tests that assert unknown agents get `{}` / `false`:

```typescript
// REPLACE the "returns empty object for unknown agents" test:
it("returns default restrictions for unknown agents (fail-closed)", () => {
  const restrictions = getAgentToolRestrictions("unknown-agent");
  expect(restrictions).toEqual({
    write: false,
    edit: false,
    background_task: false,
    background_cancel: false,
  });
});

// REPLACE the "returns empty object for empty string" test:
it("returns default restrictions for empty string (fail-closed)", () => {
  const restrictions = getAgentToolRestrictions("");
  expect(restrictions).toEqual({
    write: false,
    edit: false,
    background_task: false,
    background_cancel: false,
  });
});

// REPLACE the isLeafAgent "returns false for unknown agents" test:
it("returns true for unknown agents (fail-closed)", () => {
  expect(isLeafAgent("unknown-agent")).toBe(true);
});

// REPLACE the isLeafAgent "returns false for empty string" test:
it("returns true for empty string (fail-closed)", () => {
  expect(isLeafAgent("")).toBe(true);
});
```

Add new tests for orchestrator and conductor:

```typescript
it("returns empty restrictions for orchestrator (delegator)", () => {
  const restrictions = getAgentToolRestrictions("orchestrator");
  expect(restrictions).toEqual({});
});

it("returns false for orchestrator in isLeafAgent", () => {
  expect(isLeafAgent("orchestrator")).toBe(false);
});

it("returns write/edit restrictions for conductor (can delegate, cannot edit)", () => {
  const restrictions = getAgentToolRestrictions("conductor");
  expect(restrictions).toEqual({
    write: false,
    edit: false,
  });
});

it("returns false for conductor in isLeafAgent (can delegate)", () => {
  expect(isLeafAgent("conductor")).toBe(false);
});
```

### Step 2: Run tests to verify they fail

Run: `bun test packages/opencode-plugin/src/delegation/__tests__/agent-restrictions.test.ts`
Expected: 8 failures (4 changed assertions for unknown agents + 4 new tests for orchestrator/conductor)

### Step 3: Implement fail-closed defaults

In `packages/opencode-plugin/src/delegation/agent-restrictions.ts`:

1. Add a `DEFAULT_RESTRICTIONS` constant:
```typescript
const DEFAULT_RESTRICTIONS: ToolRestrictions = {
  write: false,
  edit: false,
  background_task: false,
  background_cancel: false,
};
```

2. Add orchestrator and conductor to the restrictions map with explicit entries so they don't fall through to `DEFAULT_RESTRICTIONS`. Orchestrator is fully unrestricted. Conductor can delegate (`background_task`/`background_cancel` not restricted) but cannot write/edit (its permissions in `index.ts:124-134` also deny write/edit/bash as a second layer):
```typescript
orchestrator: {},
conductor: {
  write: false,
  edit: false,
},
```

Note: `conductor` is defined in `agents/index.ts:61` and gets separate permissions in `index.ts:124-134` (read-only, no bash). Adding it here prevents it from getting `DEFAULT_RESTRICTIONS` (which would block `background_task`/`background_cancel`, breaking delegation). The `DELEGATOR_ALLOWLIST` in `delegation-tool.ts` is a second check — but if `background_task: false` were in AGENT_RESTRICTIONS, the tool call would be blocked at the platform level before the allowlist check is reached.

3. Change the fallback in `getAgentToolRestrictions`:
```typescript
// BEFORE:
return (AGENT_RESTRICTIONS[normalized] ?? {}) as Record<string, boolean>;
// AFTER:
return (AGENT_RESTRICTIONS[normalized] ?? DEFAULT_RESTRICTIONS) as Record<string, boolean>;
```

4. Update the JSDoc comment:
```typescript
/**
 * Get tool restrictions for an agent.
 * Case-insensitive matching.
 * Unknown agents get default restrictions (fail-closed: no write, no edit, no delegation).
 */
```

### Step 4: Run tests to verify they pass

Run: `bun test packages/opencode-plugin/src/delegation/__tests__/agent-restrictions.test.ts`
Expected: All pass

### Step 5: Add delegator allowlist to delegation-tool.ts

In `packages/opencode-plugin/src/delegation/delegation-tool.ts`:

1. Remove the `isLeafAgent` import (line 5) — it's no longer needed:
```typescript
// REMOVE this line:
import { isLeafAgent } from "./agent-restrictions";
```

2. Add the allowlist at module level (after the imports):
```typescript
const DELEGATOR_ALLOWLIST = new Set(["orchestrator", "conductor", "hephaestus"]);
```

3. In the `background_task` execute function (line 46-49), replace the check:
```typescript
// BEFORE:
const callingAgent = context?.agent;
if (callingAgent && isLeafAgent(callingAgent)) {
  return `Error: Agent '${callingAgent}' cannot delegate tasks. Only orchestrator-type agents can use background_task.`;
}

// AFTER:
const callingAgent = context?.agent;
if (callingAgent && !DELEGATOR_ALLOWLIST.has(callingAgent.toLowerCase())) {
  return `Error: Agent '${callingAgent}' cannot delegate tasks. Only orchestrator-type agents can use background_task.`;
}
```

Note: `getAgentToolRestrictions` is still imported by `background-manager.ts` (line 6) for tool restrictions on subagent prompts — that import is unaffected.

### Step 6: Run full test suite for the delegation module

Run: `bun test packages/opencode-plugin/src/delegation/`
Expected: All pass

### Step 7: Run full plugin test suite

Run: `bun test packages/opencode-plugin/`
Expected: All pass

### Step 8: Commit

```
feat(plugin): fail-closed agent restrictions and delegator allowlist

Unknown agents now default to read-only + no delegation instead of
unrestricted. Orchestrator/conductor/hephaestus are explicitly allowed
to delegate. Prevents accidental full access from typos or new agents.
```

---

## Task 2: Zod-validated state file with fail-empty recovery

The daemon's `readStateFile()` throws on corrupted JSON, permanently bricking `/workers` routes. Add Zod validation and recover gracefully to empty state.

**Files:**
- Modify: `packages/daemon/package.json` (add zod dependency)
- Create: `packages/daemon/src/daemon/schemas.ts`
- Modify: `packages/daemon/src/daemon/state-file.ts`
- Modify: `packages/daemon/src/daemon/__tests__/state-file.test.ts`

### Step 1: Add Zod dependency to daemon package

In `packages/daemon/package.json`, add `"zod"` to dependencies. Use the same version as the plugin package (check `packages/opencode-plugin/package.json` or use `"^3"` — Zod comes transitively via `@opencode-ai/plugin` already, but the daemon should declare it explicitly).

Run: `bun install` from repo root

### Step 2: Write the Zod schemas

Create `packages/daemon/src/daemon/schemas.ts`:

```typescript
import { z } from "zod";

export const CrashHistoryEntrySchema = z.object({
  crashCount: z.number(),
  lastCrashAt: z.string().nullable(),
});

export const WorkerEntrySchema = z.object({
  id: z.string(),
  port: z.number(),
  sessionId: z.string(),
  workspace: z.string(),
  startedAt: z.string(),
  status: z.enum(["starting", "running", "stopped", "dead"]),
  crashCount: z.number(),
  lastCrashAt: z.string().nullable(),
}).passthrough();

export const ControllerStateSchema = z.object({
  sessionId: z.string(),
  port: z.number().optional(),
  pid: z.number().optional(),
});

export const PersistedWorkerStateSchema = z.object({
  workers: z.record(z.string(), WorkerEntrySchema),
  crashHistory: z.record(z.string(), CrashHistoryEntrySchema),
  controller: ControllerStateSchema.optional(),
}).passthrough();
```

Use `.passthrough()` on `WorkerEntrySchema` and the top-level schema so forward-compatible fields aren't stripped.

### Step 3: Write failing tests for corrupted state recovery

In `packages/daemon/src/daemon/__tests__/state-file.test.ts`, add:

```typescript
it("recovers gracefully from corrupted JSON", async () => {
  tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
  const filePath = path.join(tempDir, "workers.json");

  await writeFile(filePath, "NOT VALID JSON{{{");
  const state = await readStateFile(filePath);

  expect(state).toEqual({ workers: {}, crashHistory: {} });

  // Original file should be renamed aside for debugging
  const entries = await readdir(tempDir);
  const corruptFiles = entries.filter((e) => e.includes(".corrupt."));
  expect(corruptFiles.length).toBe(1);
});

it("recovers gracefully from schema-invalid JSON", async () => {
  tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
  const filePath = path.join(tempDir, "workers.json");

  // Valid JSON but wrong shape — workers should be a record, not a string
  await writeFile(filePath, JSON.stringify({ workers: "not-a-record" }));
  const state = await readStateFile(filePath);

  expect(state).toEqual({ workers: {}, crashHistory: {} });
});

it("recovers gracefully from empty file", async () => {
  tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
  const filePath = path.join(tempDir, "workers.json");

  await writeFile(filePath, "");
  const state = await readStateFile(filePath);

  expect(state).toEqual({ workers: {}, crashHistory: {}, controller: undefined });
});
```

### Step 4: Run tests to verify they fail

Run: `bun test packages/daemon/src/daemon/__tests__/state-file.test.ts`
Expected: "recovers gracefully from corrupted JSON" and "recovers gracefully from schema-invalid JSON" should fail (current code throws on corrupt JSON)

### Step 5: Implement Zod validation in state-file.ts

In `packages/daemon/src/daemon/state-file.ts`:

1. Add schema import (note: `rename` is already imported on line 1 from `node:fs/promises`):
```typescript
import { PersistedWorkerStateSchema } from "./schemas";
```

2. Replace the `readStateFile` function:

```typescript
const EMPTY_STATE: PersistedWorkerState = { workers: {}, crashHistory: {} };

export async function readStateFile(filePath: string): Promise<PersistedWorkerState> {
  const resolvedPath = resolveHome(filePath);
  let raw: string;

  try {
    raw = await readFile(resolvedPath, "utf-8");
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") {
      return { ...EMPTY_STATE, controller: undefined };
    }
    throw error;
  }

  if (!raw.trim()) {
    return { ...EMPTY_STATE, controller: undefined };
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    console.warn(`[state-file] Corrupt JSON in ${resolvedPath}, recovering to empty state`);
    await renameSafe(resolvedPath);
    return { ...EMPTY_STATE };
  }

  // Normalize legacy format before validation
  const normalized = normalizeState(parsed);

  const result = PersistedWorkerStateSchema.safeParse(normalized);
  if (!result.success) {
    const issues = result.error.issues
      .map((i) => `${i.path.join(".")}: ${i.message}`)
      .join(", ");
    console.warn(`[state-file] Invalid state file ${resolvedPath}: ${issues}, recovering to empty state`);
    await renameSafe(resolvedPath);
    return { ...EMPTY_STATE };
  }

  return result.data;
}

async function renameSafe(filePath: string): Promise<void> {
  try {
    await rename(filePath, `${filePath}.corrupt.${Date.now()}`);
  } catch {
    // Best-effort — if rename fails, we still return empty state
  }
}
```

Keep the `normalizeState` function but remove its fallback `return` at the bottom that casts raw to `WorkerEntry` — it should return `EMPTY_STATE` instead:

```typescript
// BEFORE (line 69-72):
  return {
    workers: (raw ?? {}) as Record<string, WorkerEntry>,
    crashHistory: {},
  };

// AFTER:
  return { workers: {}, crashHistory: {} };
```

### Step 6: Run tests to verify they pass

Run: `bun test packages/daemon/src/daemon/__tests__/state-file.test.ts`
Expected: All pass

### Step 7: Run full daemon test suite

Run: `bun test packages/daemon/`
Expected: All pass

### Step 8: Run typecheck

Run: `bunx tsc --noEmit` (from repo root)
Expected: No errors

### Step 9: Commit

```
feat(daemon): Zod-validated state file with fail-empty recovery

readStateFile() now validates with Zod and recovers gracefully from
corrupted or schema-invalid JSON by renaming aside and returning empty
state. Prevents permanently bricked /workers routes.
```

---

## Task 3: Zod-validated task storage (plugin)

The task storage in the plugin uses `JSON.parse` with an unchecked cast to `BackgroundTask`. Add a Zod schema and use `safeParse` for disk reads.

**Files:**
- Create: `packages/opencode-plugin/src/delegation/schemas.ts`
- Modify: `packages/opencode-plugin/src/delegation/task-storage.ts`
- Modify: `packages/opencode-plugin/src/delegation/__tests__/task-storage.test.ts`

### Step 1: Create BackgroundTask Zod schema

Create `packages/opencode-plugin/src/delegation/schemas.ts`:

```typescript
import { z } from "zod";

export const BackgroundTaskStatusSchema = z.enum([
  "pending",
  "running",
  "completed",
  "failed",
  "cancelled",
]);

export const BackgroundTaskSchema = z
  .object({
    id: z.string(),
    status: BackgroundTaskStatusSchema,
    agent: z.string(),
    model: z.string(),
    description: z.string(),
    sessionID: z.string().optional(),
    parentSessionID: z.string().optional(),
    result: z.string().optional(),
    error: z.string().optional(),
    createdAt: z.number(),
    completedAt: z.number().optional(),
    retryCount: z.number().optional(),
    concurrencyKey: z.string().optional(),
    lastMessageCount: z.number().optional(),
    lastActivityAt: z.number().optional(),
    staleAlertSent: z.boolean().optional(),
  })
  .passthrough();
```

### Step 2: Write failing test for schema-invalid task

In `packages/opencode-plugin/src/delegation/__tests__/task-storage.test.ts`, add:

```typescript
describe("schema validation", () => {
  it("listTasks skips tasks with invalid schema", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    fs.mkdirSync(tasksDir, { recursive: true });

    // Valid JSON, but missing required fields (no status, no agent)
    fs.writeFileSync(
      path.join(tasksDir, "bg_invalid.json"),
      JSON.stringify({ id: "bg_invalid", description: "missing fields" })
    );
    // Also write a valid task
    await writeTask(workspace, makeTask({ id: "bg_valid" }));

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    const tasks = await listTasks(workspace);
    expect(tasks).toHaveLength(1);
    expect(tasks[0].id).toBe("bg_valid");
    warnSpy.mockRestore();
  });

  it("readTask returns null for schema-invalid task", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    fs.mkdirSync(tasksDir, { recursive: true });

    fs.writeFileSync(
      path.join(tasksDir, "bg_badschema.json"),
      JSON.stringify({ id: "bg_badschema", wrong: "shape" })
    );

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    const result = await readTask(workspace, "bg_badschema");
    expect(result).toBeNull();
    warnSpy.mockRestore();
  });
});
```

### Step 3: Run tests to verify they fail

Run: `bun test packages/opencode-plugin/src/delegation/__tests__/task-storage.test.ts`
Expected: The schema-invalid tests fail (current code accepts any JSON)

### Step 4: Implement Zod validation in task-storage.ts

In `packages/opencode-plugin/src/delegation/task-storage.ts`:

1. Add import:
```typescript
import { BackgroundTaskSchema } from "./schemas";
```

2. In `readTask`, replace the bare `JSON.parse` cast:

```typescript
// BEFORE (line 64):
    return JSON.parse(data) as BackgroundTask;

// AFTER:
    const parsed = BackgroundTaskSchema.safeParse(JSON.parse(data));
    if (!parsed.success) {
      console.warn(`[task-storage] Invalid task schema ${taskId}: ${parsed.error.message}`);
      return null;
    }
    return parsed.data as BackgroundTask;
```

3. In `listTasks`, replace the bare `JSON.parse` cast (line 103-104):

```typescript
// BEFORE:
      const task = JSON.parse(data) as BackgroundTask;
      tasks.push(task);

// AFTER:
      const result = BackgroundTaskSchema.safeParse(JSON.parse(data));
      if (result.success) {
        tasks.push(result.data as BackgroundTask);
      } else {
        console.warn(`[task-storage] Skipping invalid task ${entry}: ${result.error.message}`);
      }
```

### Step 5: Run tests to verify they pass

Run: `bun test packages/opencode-plugin/src/delegation/__tests__/task-storage.test.ts`
Expected: All pass

### Step 6: Run full plugin test suite

Run: `bun test packages/opencode-plugin/`
Expected: All pass

### Step 7: Commit

```
feat(plugin): Zod-validated task storage reads

readTask() and listTasks() now validate with Zod schema before
returning. Invalid tasks are logged and skipped instead of silently
accepted with wrong shapes.
```

---

## Task 4: Bidirectional task persistence (rehydration)

Add a `rehydrate()` method to `BackgroundTaskManager` that loads persisted tasks from disk on plugin init, rebuilds in-memory indexes, and cleans up stale tasks using `taskRetentionMs`.

**Files:**
- Modify: `packages/opencode-plugin/src/delegation/background-manager.ts`
- Modify: `packages/opencode-plugin/src/index.ts:74`
- Create: `packages/opencode-plugin/src/delegation/__tests__/rehydration.test.ts`

**Depends on:** Task 3 (Zod-validated task storage) — rehydration reads from disk, so it benefits from validated reads.

### Step 1: Write failing tests for rehydration

Create `packages/opencode-plugin/src/delegation/__tests__/rehydration.test.ts`:

```typescript
import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { writeTask } from "../task-storage";
import type { BackgroundTask } from "../types";
import { BackgroundTaskManager } from "../background-manager";

let workspace: string;

function makeTask(overrides: Partial<BackgroundTask> = {}): BackgroundTask {
  return {
    id: "bg_abc12345",
    status: "completed",
    agent: "explore",
    model: "anthropic/claude-sonnet-4-20250514",
    description: "test task",
    sessionID: "ses_test123",
    createdAt: Date.now(),
    completedAt: Date.now(),
    ...overrides,
  };
}

function createManager(dir: string): BackgroundTaskManager {
  const mockCtx = {
    client: {
      session: {
        create: async () => ({ data: { id: "ses_mock" } }),
        messages: async () => ({ data: [] }),
        abort: async () => {},
        get: async () => ({ data: null }),
        status: async () => ({ data: {} }),
        promptAsync: async () => ({}),
      },
    },
    directory: dir,
  } as any;
  return new BackgroundTaskManager(mockCtx);
}

beforeEach(() => {
  workspace = fs.mkdtempSync(path.join(os.tmpdir(), "rehydration-test-"));
});

afterEach(() => {
  fs.rmSync(workspace, { recursive: true, force: true });
});

describe("BackgroundTaskManager.rehydrate", () => {
  it("loads completed tasks from disk", async () => {
    const task = makeTask({ id: "bg_done1", status: "completed" });
    await writeTask(workspace, task);

    const manager = createManager(workspace);
    await manager.rehydrate();

    const output = await manager.getTaskOutput("bg_done1");
    expect(output).not.toContain("Task not found");
  });

  it("loads failed tasks from disk", async () => {
    const task = makeTask({ id: "bg_fail1", status: "failed", error: "something broke" });
    await writeTask(workspace, task);

    const manager = createManager(workspace);
    await manager.rehydrate();

    const output = await manager.getTaskOutput("bg_fail1");
    expect(output).toContain("Task failed");
  });

  it("rebuilds sessionId index", async () => {
    const task = makeTask({ id: "bg_ses1", sessionID: "ses_abc" });
    await writeTask(workspace, task);

    const manager = createManager(workspace);
    await manager.rehydrate();

    expect(manager.isBackgroundSession("ses_abc")).toBe(true);
  });

  it("skips tasks older than taskRetentionMs", async () => {
    const oldTask = makeTask({
      id: "bg_old1",
      createdAt: Date.now() - 2 * 60 * 60 * 1000, // 2 hours ago
      completedAt: Date.now() - 2 * 60 * 60 * 1000,
    });
    await writeTask(workspace, oldTask);

    const manager = createManager(workspace);
    await manager.rehydrate({ taskRetentionMs: 60 * 60 * 1000 }); // 1 hour TTL

    const output = await manager.getTaskOutput("bg_old1");
    expect(output).toContain("Task not found");

    // File should be deleted from disk too
    const tasksDir = path.join(workspace, ".legion", "tasks");
    const files = fs.readdirSync(tasksDir);
    expect(files.filter((f) => f.includes("bg_old1"))).toHaveLength(0);
  });

  it("works with empty tasks directory", async () => {
    const manager = createManager(workspace);
    await manager.rehydrate();
    // Should not throw
  });

  it("finalizes running tasks with no live session as failed", async () => {
    const task = makeTask({ id: "bg_orphan", status: "running", sessionID: "ses_gone" });
    await writeTask(workspace, task);

    const manager = createManager(workspace);
    await manager.rehydrate();

    const output = await manager.getTaskOutput("bg_orphan");
    expect(output).toContain("Task failed");
  });
});
```

### Step 2: Run tests to verify they fail

Run: `bun test packages/opencode-plugin/src/delegation/__tests__/rehydration.test.ts`
Expected: Failures — `rehydrate()` method doesn't exist

### Step 3: Implement rehydrate() method

In `packages/opencode-plugin/src/delegation/background-manager.ts`:

1. Add import:
```typescript
import { deleteTask, listTasks } from "./task-storage";
```

2. Add the `rehydrate` method to `BackgroundTaskManager`:

```typescript
  /**
   * Rehydrate in-memory state from persisted task files.
   * Call once during plugin init to restore task visibility across restarts.
   */
  async rehydrate(opts?: { taskRetentionMs?: number }): Promise<void> {
    const tasks = await listTasks(this.directory);
    const now = Date.now();
    const ttl = opts?.taskRetentionMs;

    for (const task of tasks) {
      // Prune expired tasks
      const taskAge = now - (task.completedAt ?? task.createdAt);
      if (ttl && taskAge > ttl) {
        await deleteTask(this.directory, task.id).catch(() => {});
        continue;
      }

      // Non-terminal tasks from a previous run are orphaned — finalize them
      if (task.status === "pending" || task.status === "running") {
        task.status = "failed";
        task.error = "Interrupted: plugin restarted while task was in progress";
        task.completedAt = now;
      }

      this.tasks.set(task.id, task);
      if (task.sessionID) {
        this.tasksBySessionId.set(task.sessionID, task.id);
      }
    }
  }
```

### Step 4: Wire rehydration into plugin init

In `packages/opencode-plugin/src/index.ts`, after line 74:

```typescript
// BEFORE:
const manager = new BackgroundTaskManager(ctx);

// AFTER:
const manager = new BackgroundTaskManager(ctx);
await manager.rehydrate({ taskRetentionMs: pluginConfig.taskRetentionMs });
```

### Step 5: Run rehydration tests

Run: `bun test packages/opencode-plugin/src/delegation/__tests__/rehydration.test.ts`
Expected: All pass

### Step 6: Run full plugin test suite

Run: `bun test packages/opencode-plugin/`
Expected: All pass

### Step 7: Commit

```
feat(plugin): bidirectional task persistence with rehydration

BackgroundTaskManager.rehydrate() loads persisted tasks from disk on
plugin init. Completed/failed tasks are restored for background_output.
Running tasks from previous sessions are finalized as failed. Tasks
older than taskRetentionMs are pruned from disk.
```

---

## Task 5: Fix documentation drift

Update the 3 files with outdated references to the old per-worker process model.

**Files:**
- Modify: `.opencode/skills/AGENTS.md:59`
- Modify: `docs/solutions/integration-patterns/controller-worker-protocol.md:15`
- Modify: `docs/brainstorms/2026-02-12-opencode-legion-mvp-phases-2-4-brainstorm.md:215` (if it exists)

### Step 1: Fix skills AGENTS.md

In `.opencode/skills/AGENTS.md`, line 59:

```markdown
## BEFORE:
- **Dispatch** = `POST /workers` → new OpenCode serve process + `prompt_async`
- **Resume** = `POST /session/{id}/prompt_async` on existing worker port

## AFTER:
- **Dispatch** = `POST /workers` → new session on shared serve (idempotent, deterministic session ID)
- **Resume** = `POST /session/{id}/prompt_async` on shared serve
```

### Step 2: Fix controller-worker-protocol.md

In `docs/solutions/integration-patterns/controller-worker-protocol.md`, line 15:

```markdown
## BEFORE:
- **Dispatch** = new process + new session via `POST /workers`

## AFTER:
- **Dispatch** = new session on shared serve via `POST /workers` (idempotent, ~10ms)
```

### Step 3: Fix brainstorm doc (if applicable)

In `docs/brainstorms/2026-02-12-opencode-legion-mvp-phases-2-4-brainstorm.md`, line 215 (if the file exists):

```markdown
## BEFORE:
oh-my-opencode.json

## AFTER:
opencode-legion.json
```

### Step 4: Verify no other stale references

Run: `grep -r "new.*serve process" .opencode/skills/ docs/solutions/`
Run: `grep -r "new process.*new session" docs/solutions/`
Expected: No remaining matches (or only historical docs marked `[HISTORICAL]`)

### Step 5: Commit

```
docs: fix shared-serve model references in skills and solutions

AGENTS.md and controller-worker-protocol.md still described the old
per-worker process dispatch model. Updated to reflect shared serve
architecture.
```

---

## Task 6: Add timeouts to hang-risk network paths

Add `AbortSignal.timeout()` to the 4 identified paths that can stall the controller loop indefinitely.

**Files:**
- Modify: `packages/daemon/src/daemon/serve-manager.ts` (createSession)
- Modify: `packages/daemon/src/state/fetch.ts` (defaultRunner, getLiveWorkers)
- Modify: `packages/daemon/src/cli/team-resolver.ts` (resolveTeamId fetch)

### Step 1: Identify current timeout patterns

Check existing timeouts in `serve-manager.ts` for the pattern to follow. `healthCheck()` already uses `AbortSignal.timeout(timeoutMs)` and `stopServe()` uses `AbortSignal.timeout(5000)`. Follow this pattern.

### Step 2: Add timeout to createSession

In `packages/daemon/src/daemon/serve-manager.ts`, line 83. The current code is a bare fetch with no timeout:

```typescript
// BEFORE (serve-manager.ts:83-89):
const res = await fetch(`${baseUrl}/session`, {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "x-opencode-directory": encodeURIComponent(workspace),
  },
  body: JSON.stringify({ id: sessionId }),
});

// AFTER:
const res = await fetch(`${baseUrl}/session`, {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "x-opencode-directory": encodeURIComponent(workspace),
  },
  body: JSON.stringify({ id: sessionId }),
  signal: AbortSignal.timeout(10_000), // 10s — session creation is a local call
});
```

This follows the existing pattern used by `healthCheck()` (line 141) and `stopServe()` (line 110) in the same file.

### Step 3: Add timeout to defaultRunner in fetch.ts

In `packages/daemon/src/state/fetch.ts`, lines 45-58. Currently uses `Bun.spawn` with no timeout:

```typescript
// BEFORE (fetch.ts:45-58):
export async function defaultRunner(cmd: string[]): Promise<CommandResult> {
  const proc = Bun.spawn(cmd, {
    stdout: "pipe",
    stderr: "pipe",
  });

  const [stdout, stderr] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ]);

  const exitCode = await proc.exited;
  return { stdout, stderr, exitCode };
}

// AFTER:
export async function defaultRunner(cmd: string[]): Promise<CommandResult> {
  const proc = Bun.spawn(cmd, {
    stdout: "pipe",
    stderr: "pipe",
  });

  const killTimeout = setTimeout(() => proc.kill(), 30_000); // 30s for gh api graphql

  const [stdout, stderr] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ]);

  const exitCode = await proc.exited;
  clearTimeout(killTimeout);
  return { stdout, stderr, exitCode };
}
```

Note: `proc.exited` resolves even after kill (with a non-zero exit code), so the `clearTimeout` is always reached. The caller (`getPrDraftStatusBatch`) already handles non-zero exit codes via retry logic.

### Step 4: Add timeout to getLiveWorkers fetch

In `packages/daemon/src/state/fetch.ts`, line 96. Currently a bare fetch. Note: the function already has a try-catch that returns `{}` on any error (lines 120-123), so the `AbortError` from a timeout will be caught and handled gracefully — no caller changes needed.

```typescript
// BEFORE (fetch.ts:96):
const response = await fetch(`${daemonUrl}/workers`);

// AFTER:
const response = await fetch(`${daemonUrl}/workers`, {
  signal: AbortSignal.timeout(5_000), // 5s — local daemon should respond fast
});
```

### Step 5: Add timeout to team-resolver Linear fetch

In `packages/daemon/src/cli/team-resolver.ts`, line 73. The `lookupTeamViaApi()` function already has a try-catch (lines 72-97) that wraps errors, so `AbortError` will be caught and re-thrown as a descriptive error.

```typescript
// BEFORE (team-resolver.ts:73-80):
const response = await fetch("https://api.linear.app/graphql", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    Authorization: apiKey,
  },
  body: payload,
});

// AFTER:
const response = await fetch("https://api.linear.app/graphql", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    Authorization: apiKey,
  },
  body: payload,
  signal: AbortSignal.timeout(15_000), // 15s for external API
});
```

### Step 6: Run daemon tests

Run: `bun test packages/daemon/`
Expected: All pass (timeouts only fire on actual hangs, not in tests)

### Step 7: Run typecheck

Run: `bunx tsc --noEmit`
Expected: No errors

### Step 8: Commit

```
feat(daemon): add timeouts to hang-risk network paths

createSession (10s), gh api graphql runner (30s), getLiveWorkers (5s),
and Linear team resolver (15s) now have AbortSignal timeouts. Prevents
the controller loop from stalling indefinitely on network issues.
```

---

## Verification

After all tasks:

```bash
bun test                    # All 491+ tests pass
bunx biome check .          # No lint errors
bunx tsc --noEmit           # No type errors
```
