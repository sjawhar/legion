# Task Persistence Performance Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Keep per-task JSON storage under `~/.config/opencode/tasks/` fast by avoiding full directory scans on hot paths (list, claim) and enforcing simple guardrails — without new dependencies.

**Architecture:** Retain file-per-task JSON and atomic temp+rename writes. Add a compact `active-index.json` that tracks `pending`/`in_progress` task IDs so list and claim skip reading completed/cancelled files. Integrate index updates into write paths (create, update, claim). Guardrails cap description sizes.

**Known gap — crash recovery:** If a process crashes between writing a task file and updating the index, that task is invisible to list/claim until its next update. This is rare (both writes are inside the lock, milliseconds apart) and self-correcting on any task update. A bounded reconcile pass could fix this if it becomes a problem, but is deferred until there's evidence it's needed.

**Tech Stack:** TypeScript, Bun runtime, node:fs, Zod, Bun test, Biome.

**Prerequisite:** Run `bun install` from the repo root before starting.

**Test command:** `bun test packages/opencode-plugin/src/tools/task/` from the repo root.

**Existing helpers you'll reuse from `storage.ts`:** `ensureDir`, `readJsonSafe`, `writeJsonAtomic`, `listTaskFiles`, `acquireLock`, `getTaskDir`.

**Important:** `readAllTasks` is used by `task-create.ts` and `task-update.ts` for cycle detection — these need ALL tasks. The new `readActiveTasks` is only for the list/claim hot paths, with a separate blocker-resolution step that reads individual completed task files on demand.

---

### Task 1: Add TaskIndex schemas to types.ts

**Files:**
- Modify: `packages/opencode-plugin/src/tools/task/types.ts` (append at end)
- Test: `packages/opencode-plugin/src/tools/task/__tests__/types.test.ts`

**Step 1: Write the failing test**

Add these imports to the top of `__tests__/types.test.ts` (merge with existing import):

```ts
import {
  TaskCreateInputSchema,
  TaskIndexEntrySchema,
  TaskIndexSchema,
  TaskSchema,
  TaskStatusSchema,
  TaskUpdateInputSchema,
} from "../types";
```

Append these describe blocks at the end of the file:

```ts
describe("TaskIndexEntrySchema", () => {
  it("accepts a valid entry", () => {
    const result = TaskIndexEntrySchema.safeParse({
      id: "T-abc",
      status: "pending",
    });
    expect(result.success).toBe(true);
  });

  it("rejects missing id", () => {
    expect(TaskIndexEntrySchema.safeParse({ status: "pending" }).success).toBe(false);
  });

  it("rejects invalid status", () => {
    expect(TaskIndexEntrySchema.safeParse({ id: "T-1", status: "deleted" }).success).toBe(false);
  });
});

describe("TaskIndexSchema", () => {
  it("accepts a valid index", () => {
    const result = TaskIndexSchema.safeParse({
      version: 1,
      entries: [{ id: "T-1", status: "pending" }],
    });
    expect(result.success).toBe(true);
  });

  it("defaults entries to empty array", () => {
    const result = TaskIndexSchema.parse({ version: 1 });
    expect(result.entries).toEqual([]);
  });

  it("rejects missing version", () => {
    expect(TaskIndexSchema.safeParse({ entries: [] }).success).toBe(false);
  });
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/types.test.ts`
Expected: FAIL — `TaskIndexEntrySchema` and `TaskIndexSchema` not exported from `../types`.

**Step 3: Write minimal implementation**

Append to the end of `packages/opencode-plugin/src/tools/task/types.ts`:

```ts
export const TaskIndexEntrySchema = z.object({
  id: z.string(),
  status: TaskStatusSchema,
});

export type TaskIndexEntry = z.infer<typeof TaskIndexEntrySchema>;

export const TaskIndexSchema = z.object({
  version: z.literal(1),
  entries: z.array(TaskIndexEntrySchema).default([]),
});

export type TaskIndex = z.infer<typeof TaskIndexSchema>;
```

**Step 4: Run test to verify it passes**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/types.test.ts`
Expected: PASS

**Step 5: Commit**

Run: `jj describe -m "feat: add TaskIndex schemas to types.ts"`

---

### Task 2: Add task-index.ts with read/write/upsert helpers

**Files:**
- Create: `packages/opencode-plugin/src/tools/task/task-index.ts`
- Test: `packages/opencode-plugin/src/tools/task/__tests__/task-index.test.ts`

**Step 1: Write the failing test**

Create `packages/opencode-plugin/src/tools/task/__tests__/task-index.test.ts`:

```ts
import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { indexPathFor, readTaskIndex, upsertIndexEntry, writeTaskIndexAtomic } from "../task-index";

let tempDir: string;

beforeEach(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "task-index-test-"));
});

afterEach(() => {
  fs.rmSync(tempDir, { recursive: true, force: true });
});

function idxPath(): string {
  return indexPathFor(tempDir);
}

describe("readTaskIndex", () => {
  it("returns null for non-existent file", () => {
    expect(readTaskIndex(idxPath())).toBeNull();
  });

  it("returns null for malformed JSON", () => {
    fs.writeFileSync(idxPath(), "{not json");
    expect(readTaskIndex(idxPath())).toBeNull();
  });

  it("returns null for invalid schema", () => {
    fs.writeFileSync(idxPath(), JSON.stringify({ wrong: true }));
    expect(readTaskIndex(idxPath())).toBeNull();
  });

  it("returns parsed index for valid file", () => {
    fs.writeFileSync(
      idxPath(),
      JSON.stringify({ version: 1, entries: [{ id: "T-1", status: "pending" }] })
    );
    const result = readTaskIndex(idxPath());
    expect(result).not.toBeNull();
    expect(result!.version).toBe(1);
    expect(result!.entries).toHaveLength(1);
  });
});

describe("writeTaskIndexAtomic", () => {
  it("writes valid index file", () => {
    writeTaskIndexAtomic(idxPath(), { version: 1, entries: [] });
    const content = JSON.parse(fs.readFileSync(idxPath(), "utf-8"));
    expect(content.version).toBe(1);
    expect(content.entries).toEqual([]);
  });

  it("creates parent directories", () => {
    const nested = path.join(tempDir, "a", "b", "active-index.json");
    writeTaskIndexAtomic(nested, { version: 1, entries: [] });
    expect(fs.existsSync(nested)).toBe(true);
  });

  it("overwrites existing file atomically", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-old", status: "pending" }],
    });
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-new", status: "in_progress" }],
    });
    const result = readTaskIndex(idxPath());
    expect(result!.entries).toHaveLength(1);
    expect(result!.entries[0].id).toBe("T-new");
  });
});

describe("upsertIndexEntry", () => {
  it("adds a new pending entry", () => {
    writeTaskIndexAtomic(idxPath(), { version: 1, entries: [] });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "pending" });
    const result = readTaskIndex(idxPath());
    expect(result!.entries).toHaveLength(1);
    expect(result!.entries[0]).toEqual({ id: "T-1", status: "pending" });
  });

  it("updates existing entry status", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-1", status: "pending" }],
    });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "in_progress" });
    const result = readTaskIndex(idxPath());
    expect(result!.entries).toHaveLength(1);
    expect(result!.entries[0].status).toBe("in_progress");
  });

  it("removes entry when status is completed", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-1", status: "pending" }],
    });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "completed" });
    const result = readTaskIndex(idxPath());
    expect(result!.entries).toHaveLength(0);
  });

  it("removes entry when status is cancelled", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-1", status: "in_progress" }],
    });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "cancelled" });
    const result = readTaskIndex(idxPath());
    expect(result!.entries).toHaveLength(0);
  });

  it("creates index if it does not exist", () => {
    upsertIndexEntry(idxPath(), { id: "T-1", status: "pending" });
    const result = readTaskIndex(idxPath());
    expect(result).not.toBeNull();
    expect(result!.entries).toHaveLength(1);
  });

  it("preserves other entries", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [
        { id: "T-1", status: "pending" },
        { id: "T-2", status: "in_progress" },
      ],
    });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "completed" });
    const result = readTaskIndex(idxPath());
    expect(result!.entries).toHaveLength(1);
    expect(result!.entries[0].id).toBe("T-2");
  });
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/task-index.test.ts`
Expected: FAIL — module `../task-index` does not exist.

**Step 3: Write minimal implementation**

Create `packages/opencode-plugin/src/tools/task/task-index.ts`:

```ts
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { writeJsonAtomic } from "./storage";
import {
  type TaskIndex,
  type TaskIndexEntry,
  TaskIndexEntrySchema,
  TaskIndexSchema,
} from "./types";

/** Filename for the active task index within a task directory. */
export const INDEX_FILENAME = "active-index.json";

/** Resolve the index path for a task directory. */
export function indexPathFor(taskDir: string): string {
  return join(taskDir, INDEX_FILENAME);
}

/**
 * Read and validate the active task index.
 * Returns null if the file is missing, malformed, or invalid.
 */
export function readTaskIndex(indexPath: string): TaskIndex | null {
  if (!existsSync(indexPath)) return null;
  try {
    const parsed = JSON.parse(readFileSync(indexPath, "utf-8"));
    const result = TaskIndexSchema.safeParse(parsed);
    return result.success ? result.data : null;
  } catch {
    return null;
  }
}

/**
 * Atomically write the full task index to disk.
 * Reuses the same temp+rename pattern as task files.
 */
export function writeTaskIndexAtomic(indexPath: string, data: TaskIndex): void {
  writeJsonAtomic(indexPath, TaskIndexSchema.parse(data));
}

/**
 * Upsert a single entry in the active index.
 *
 * - pending/in_progress: add or update the entry
 * - completed/cancelled: remove the entry (task is no longer active)
 *
 * Creates the index file if it does not exist.
 */
export function upsertIndexEntry(indexPath: string, entry: TaskIndexEntry): void {
  const validated = TaskIndexEntrySchema.parse(entry);
  const index = readTaskIndex(indexPath) ?? { version: 1 as const, entries: [] };
  const entries = index.entries.filter((e) => e.id !== validated.id);

  if (validated.status === "pending" || validated.status === "in_progress") {
    entries.push(validated);
  }

  writeTaskIndexAtomic(indexPath, { version: 1, entries });
}
```

**Step 4: Run test to verify it passes**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/task-index.test.ts`
Expected: PASS

**Step 5: Commit**

Run: `jj describe -m "feat: add task-index.ts with read/write/upsert helpers"`

---

### Task 3: Integrate index updates into write paths

**Files:**
- Modify: `packages/opencode-plugin/src/tools/task/task-create.ts`
- Modify: `packages/opencode-plugin/src/tools/task/task-update.ts`
- Modify: `packages/opencode-plugin/src/tools/task/task-claim.ts`
- Test: `packages/opencode-plugin/src/tools/task/__tests__/task-index.test.ts` (append integration tests)

The integration pattern is identical in each file: after the task file is written (inside the lock), call `upsertIndexEntry` with the task's current ID and status.

**Step 1: Write the failing integration tests**

In `__tests__/task-index.test.ts`, add these imports at the **top of the file** (merge with existing imports):

```ts
import { writeJsonAtomic } from "../storage";
import { createTaskClaimNextTool } from "../task-claim";
import { createTaskCreateTool } from "../task-create";
import { createTaskUpdateTool } from "../task-update";
import type { ToolContext } from "@opencode-ai/plugin";
import type { Task } from "../types";
```

Then add these helpers and describe blocks at the **end of the file**:

```ts
function makeContext(sessionID = "session-1"): ToolContext {
  return {
    sessionID,
    messageID: "msg-1",
    agent: "orchestrator",
    directory: "/tmp",
    worktree: "/tmp",
    abort: new AbortController().signal,
    metadata: () => {},
    ask: async () => {},
  };
}

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: "T-index-integ",
    subject: "Test task",
    description: "",
    status: "pending",
    blocks: [],
    blockedBy: [],
    threadID: "session-1",
    ...overrides,
  };
}

describe("index integration: task_create", () => {
  it("adds new task to index on create", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({ subject: "Indexed" }, makeContext()));
    expect(result.task).toBeTruthy();

    const index = readTaskIndex(idxPath());
    expect(index).not.toBeNull();
    const entry = index!.entries.find((e) => e.id === result.task.id);
    expect(entry).toBeTruthy();
    expect(entry!.status).toBe("pending");
  });
});

describe("index integration: task_update", () => {
  it("removes task from index on completion", async () => {
    const createTool = createTaskCreateTool(undefined, tempDir);
    const created = JSON.parse(
      await createTool.execute({ subject: "Will complete" }, makeContext())
    );
    const taskId = created.task.id;

    const updateTool = createTaskUpdateTool(undefined, tempDir);
    await updateTool.execute({ id: taskId, status: "completed" }, makeContext());

    const index = readTaskIndex(idxPath());
    expect(index).not.toBeNull();
    const entry = index!.entries.find((e) => e.id === taskId);
    expect(entry).toBeUndefined();
  });

  it("updates status in index on status change", async () => {
    const createTool = createTaskCreateTool(undefined, tempDir);
    const created = JSON.parse(
      await createTool.execute({ subject: "Will progress" }, makeContext())
    );
    const taskId = created.task.id;

    const updateTool = createTaskUpdateTool(undefined, tempDir);
    await updateTool.execute({ id: taskId, status: "in_progress" }, makeContext());

    const index = readTaskIndex(idxPath());
    const entry = index!.entries.find((e) => e.id === taskId);
    expect(entry).toBeTruthy();
    expect(entry!.status).toBe("in_progress");
  });
});

describe("index integration: task_claim_next", () => {
  it("updates index entry to in_progress on claim", async () => {
    writeJsonAtomic(
      path.join(tempDir, "T-claimable.json"),
      makeTask({ id: "T-claimable" })
    );
    // Seed the index so we can verify the upsert updates it after claim
    upsertIndexEntry(idxPath(), { id: "T-claimable", status: "pending" });

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext("agent-1")));

    expect(result.task).toBeTruthy();
    expect(result.task.id).toBe("T-claimable");

    const index = readTaskIndex(idxPath());
    const entry = index!.entries.find((e) => e.id === "T-claimable");
    expect(entry).toBeTruthy();
    expect(entry!.status).toBe("in_progress");
  });
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/task-index.test.ts`
Expected: FAIL — index is null after create/update/claim (integration not wired yet).

**Step 3: Write minimal implementation**

**In `task-create.ts`:**

Add import at top (after existing imports):
```ts
import { indexPathFor, upsertIndexEntry } from "./task-index";
```

Add the upsert call immediately after the `writeJsonAtomic(join(taskDir, ...` call that persists the new task (inside `computeResult`, still inside the lock):

```ts
            upsertIndexEntry(indexPathFor(taskDir), {
              id: validatedTask.id,
              status: validatedTask.status,
            });
```

**In `task-update.ts`:**

Add import at top (after existing imports):
```ts
import { indexPathFor, upsertIndexEntry } from "./task-index";
```

Add the upsert call immediately after `writeJsonAtomic(taskPath, validatedTask);` (inside `computeResult`, still inside the lock):

```ts
            upsertIndexEntry(indexPathFor(taskDir), {
              id: validatedTask.id,
              status: validatedTask.status,
            });
```

**In `task-claim.ts`:**

Add import at top (after existing imports):
```ts
import { indexPathFor, upsertIndexEntry } from "./task-index";
```

Add upsert after the `writeJsonAtomic(...)` call inside the `reclaimExpired` loop (the one that resets expired leases to pending):

```ts
                upsertIndexEntry(indexPathFor(taskDir), { id: task.id, status: task.status });
```

Add upsert after the `writeJsonAtomic(...)` call that persists the claimed task (the one inside `computeResult` after `target` is set to `in_progress`):

```ts
            upsertIndexEntry(indexPathFor(taskDir), {
              id: validatedTask.id,
              status: validatedTask.status,
            });
```

**Step 4: Run all task tests to verify**

Run: `bun test packages/opencode-plugin/src/tools/task/`
Expected: ALL PASS — existing tests still pass, new integration tests pass.

**Step 5: Commit**

Run: `jj describe -m "feat: integrate task index updates into create/update/claim"`

---

### Task 4: Index-aware hot paths for list and claim

**Files:**
- Modify: `packages/opencode-plugin/src/tools/task/task-list.ts`
- Modify: `packages/opencode-plugin/src/tools/task/task-claim.ts`
- Test: `packages/opencode-plugin/src/tools/task/__tests__/task-list.test.ts` (append)

**Correctness constraint:** Dependency resolution requires knowing whether a blocker is completed/cancelled. If a blocker isn't in the active index, we read its individual file on demand. This is cheap — typically 0-3 blockers per task.

**Important:** Keep the existing `readAllTasks` function unchanged — it's used by `task-create.ts` and `task-update.ts` for cycle detection, which needs ALL tasks. Add a new `readActiveTasks` function alongside it.

**Step 1: Write the failing tests**

Append to `__tests__/task-list.test.ts`. First add the import (merge with existing imports):

```ts
import { indexPathFor, writeTaskIndexAtomic } from "../task-index";
```

Then append these describe blocks:

```ts
describe("index-aware listing", () => {
  it("skips reading completed task files when index exists", async () => {
    for (let i = 0; i < 3; i++) {
      writeTask(tempDir, makeTask({ id: `T-done-${i}`, status: "completed" }));
    }
    writeTask(tempDir, makeTask({ id: "T-active", status: "pending" }));

    writeTaskIndexAtomic(indexPathFor(tempDir), {
      version: 1,
      entries: [{ id: "T-active", status: "pending" }],
    });

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.tasks).toHaveLength(1);
    expect(result.tasks[0].id).toBe("T-active");
  });

  it("resolves completed blockers not in index", async () => {
    writeTask(tempDir, makeTask({ id: "T-dep", status: "completed" }));
    writeTask(tempDir, makeTask({ id: "T-waiting", status: "pending", blockedBy: ["T-dep"] }));

    writeTaskIndexAtomic(indexPathFor(tempDir), {
      version: 1,
      entries: [{ id: "T-waiting", status: "pending" }],
    });

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({ ready: true }, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-waiting");
  });

  it("treats missing blocker file as blocking even with index", async () => {
    writeTask(tempDir, makeTask({ id: "T-orphan", status: "pending", blockedBy: ["T-ghost"] }));

    writeTaskIndexAtomic(indexPathFor(tempDir), {
      version: 1,
      entries: [{ id: "T-orphan", status: "pending" }],
    });

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({ ready: true }, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).not.toContain("T-orphan");
  });

  it("falls back to full scan when no index exists", async () => {
    writeTask(tempDir, makeTask({ id: "T-a", status: "pending" }));
    writeTask(tempDir, makeTask({ id: "T-b", status: "in_progress" }));

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-a");
    expect(ids).toContain("T-b");
  });
});
```

**Step 2: Run tests as baseline**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/task-list.test.ts`
Expected: All four new tests PASS (the old code produces the same output — it reads all files but filters them). These are regression tests that ensure behavior is preserved after the refactor. The actual performance improvement (fewer file reads) is structural, not behaviorally observable.

**Step 3: Rewrite task-list.ts**

Replace the entire contents of `packages/opencode-plugin/src/tools/task/task-list.ts`:

```ts
import { join } from "node:path";
import type { ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { getTaskDir, listTaskFiles, readJsonSafe } from "./storage";
import { indexPathFor, readTaskIndex } from "./task-index";
import { SATISFYING_STATUSES, type Task, TaskSchema, type TaskStatus } from "./types";

const z = tool.schema;

/**
 * Read active (pending/in_progress) tasks using the index when available.
 * Falls back to a full directory scan if no index exists.
 */
export function readActiveTasks(taskDir: string): Task[] {
  const index = readTaskIndex(indexPathFor(taskDir));
  const fileIds = index ? index.entries.map((e) => e.id) : listTaskFiles(taskDir);
  const tasks: Task[] = [];
  for (const fileId of fileIds) {
    const task = readJsonSafe(join(taskDir, `${fileId}.json`), TaskSchema);
    if (task && task.status !== "completed" && task.status !== "cancelled") {
      tasks.push(task);
    }
  }
  return tasks;
}

/**
 * Read ALL tasks from disk (full directory scan). Used when the complete
 * task graph is needed, e.g. for cycle detection in create/update.
 */
export function readAllTasks(taskDir: string): Task[] {
  const fileIds = listTaskFiles(taskDir);
  const tasks: Task[] = [];
  for (const fileId of fileIds) {
    const task = readJsonSafe(join(taskDir, `${fileId}.json`), TaskSchema);
    if (task) {
      tasks.push(task);
    }
  }
  return tasks;
}

/**
 * Build a task map from active tasks, resolving any blockers that aren't
 * in the active set by reading their individual files from disk.
 *
 * This avoids reading ALL files while still correctly resolving whether
 * blockers are completed/cancelled (dependency satisfied) vs missing
 * (dependency not satisfied).
 */
export function buildTaskMapWithBlockers(
  taskDir: string,
  activeTasks: Task[]
): Map<string, Task> {
  const taskMap = new Map(activeTasks.map((t) => [t.id, t]));

  const missingBlockerIds = new Set<string>();
  for (const task of activeTasks) {
    for (const blockerId of task.blockedBy) {
      if (!taskMap.has(blockerId)) {
        missingBlockerIds.add(blockerId);
      }
    }
  }

  for (const blockerId of missingBlockerIds) {
    const blocker = readJsonSafe(join(taskDir, `${blockerId}.json`), TaskSchema);
    if (blocker) {
      taskMap.set(blocker.id, blocker);
    }
  }

  return taskMap;
}

interface TaskSummary {
  id: string;
  subject: string;
  status: TaskStatus;
  owner?: string;
  blockedBy: string[];
  parentID?: string;
}

export function createTaskListTool(listId?: string): ToolDefinition {
  return tool({
    description:
      "List active tasks with summary information.\n\n" +
      "Excludes completed and cancelled by default.\n" +
      "Use ready=true to filter to tasks whose blockedBy are all completed/cancelled.",
    args: {
      ready: z.boolean().optional().describe("Filter to tasks with all dependencies satisfied"),
      parentID: z.string().optional().describe("Filter by parent task ID"),
    },
    execute: async (args) => {
      const typedArgs = args as { ready?: boolean; parentID?: string };
      const taskDir = getTaskDir(listId);
      const activeTasks = readActiveTasks(taskDir);

      let filtered = typedArgs.parentID
        ? activeTasks.filter((task) => task.parentID === typedArgs.parentID)
        : activeTasks;

      const taskMap = buildTaskMapWithBlockers(taskDir, activeTasks);

      const summaries: TaskSummary[] = filtered.map((task) => {
        const unresolvedBlockers = task.blockedBy.filter((blockerId) => {
          const blocker = taskMap.get(blockerId);
          return !blocker || !SATISFYING_STATUSES.has(blocker.status);
        });

        return {
          id: task.id,
          subject: task.subject,
          status: task.status,
          owner: task.owner,
          blockedBy: unresolvedBlockers,
          parentID: task.parentID,
        };
      });

      const result = typedArgs.ready
        ? summaries.filter((s) => s.blockedBy.length === 0)
        : summaries;

      return JSON.stringify({ tasks: result });
    },
  });
}
```

**Step 4: Update task-claim.ts to use readActiveTasks + buildTaskMapWithBlockers**

In `task-claim.ts`, change the import from:
```ts
import { readAllTasks } from "./task-list";
```
to:
```ts
import { buildTaskMapWithBlockers, readActiveTasks } from "./task-list";
```

In the `computeResult` function body, change:
```ts
          const allTasks = readAllTasks(taskDir);
          const taskMap = new Map(allTasks.map((t) => [t.id, t]));
```
to:
```ts
          const allTasks = readActiveTasks(taskDir);
          const taskMap = buildTaskMapWithBlockers(taskDir, allTasks);
```

The rest of the claim logic is unchanged — `reclaimExpired` iterates `allTasks` (now only active tasks, which is correct since only `in_progress` tasks have leases), and the blocker check uses `taskMap` which now correctly includes resolved completed/cancelled blockers.

**Step 5: Run all task tests to verify**

Run: `bun test packages/opencode-plugin/src/tools/task/`
Expected: ALL PASS — existing behavior preserved, index-aware paths tested.

**Step 6: Commit**

Run: `jj describe -m "feat: index-aware hot paths for task list and claim"`

---

### Task 5: Description size guardrail

**Files:**
- Modify: `packages/opencode-plugin/src/tools/task/types.ts` (add constant)
- Modify: `packages/opencode-plugin/src/tools/task/task-create.ts` (add validation)
- Modify: `packages/opencode-plugin/src/tools/task/task-update.ts` (add validation)
- Test: `packages/opencode-plugin/src/tools/task/__tests__/task-create.test.ts` (append)
- Test: `packages/opencode-plugin/src/tools/task/__tests__/task-update.test.ts` (append)

**Step 1: Add the constant to types.ts**

Append to the end of `packages/opencode-plugin/src/tools/task/types.ts`:

```ts
/** Maximum description length in characters. */
export const MAX_DESCRIPTION_CHARS = 2_000;
```

**Step 2: Write the failing tests**

Append to `__tests__/task-create.test.ts`. First add import (merge with existing):

```ts
import { MAX_DESCRIPTION_CHARS } from "../types";
```

Then append:

```ts
describe("task_create guardrails", () => {
  it("rejects oversized description", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(
      await tool.execute(
        { subject: "Big", description: "x".repeat(MAX_DESCRIPTION_CHARS + 1) },
        makeContext()
      )
    );
    expect(result.error).toBe("validation_error");
    expect(result.message).toContain("description");
  });

  it("accepts description at the limit", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(
      await tool.execute(
        { subject: "OK", description: "x".repeat(MAX_DESCRIPTION_CHARS) },
        makeContext()
      )
    );
    expect(result.task).toBeTruthy();
  });
});
```

Append to `__tests__/task-update.test.ts`. First add import (merge with existing):

```ts
import { MAX_DESCRIPTION_CHARS } from "../types";
```

Then append:

```ts
describe("task_update guardrails", () => {
  it("rejects oversized description update", async () => {
    writeTask(tempDir, makeTask());
    const tool = createTaskUpdateTool(undefined, tempDir);
    const result = JSON.parse(
      await tool.execute(
        { id: "T-update-test", description: "x".repeat(MAX_DESCRIPTION_CHARS + 1) },
        makeContext()
      )
    );
    expect(result.error).toBe("validation_error");
    expect(result.message).toContain("description");
  });
});
```

**Step 3: Run tests to verify they fail**

Run: `bun test packages/opencode-plugin/src/tools/task/__tests__/task-create.test.ts packages/opencode-plugin/src/tools/task/__tests__/task-update.test.ts`
Expected: FAIL — oversized descriptions currently accepted.

**Step 4: Write minimal implementation**

**In `task-create.ts`:**

Add `MAX_DESCRIPTION_CHARS` to the import from `./types`:
```ts
import { type Task, TaskCreateInputSchema, TaskSchema, MAX_DESCRIPTION_CHARS } from "./types";
```

Add validation right after `const validated = TaskCreateInputSchema.parse(args);`, before the lock acquisition:

```ts
        if (validated.description && validated.description.length > MAX_DESCRIPTION_CHARS) {
          return JSON.stringify({
            error: "validation_error",
            message: `description exceeds ${MAX_DESCRIPTION_CHARS} characters`,
          });
        }
```

**In `task-update.ts`:**

Add `MAX_DESCRIPTION_CHARS` to the import from `./types`:
```ts
import { MAX_DESCRIPTION_CHARS, TaskSchema, TaskUpdateInputSchema } from "./types";
```

Add validation right after `const validated = TaskUpdateInputSchema.parse(args);`, before the lock acquisition:

```ts
        if (validated.description && validated.description.length > MAX_DESCRIPTION_CHARS) {
          return JSON.stringify({
            error: "validation_error",
            message: `description exceeds ${MAX_DESCRIPTION_CHARS} characters`,
          });
        }
```

**Step 5: Run tests to verify they pass**

Run: `bun test packages/opencode-plugin/src/tools/task/`
Expected: ALL PASS

**Step 6: Commit**

Run: `jj describe -m "feat: add description size guardrail"`

---

### Task 6: Export new symbols, lint, typecheck

**Files:**
- Modify: `packages/opencode-plugin/src/tools/task/index.ts`

**Step 1: Update exports in index.ts**

Add these exports to `packages/opencode-plugin/src/tools/task/index.ts`:

```ts
export {
  indexPathFor,
  readTaskIndex,
  upsertIndexEntry,
  writeTaskIndexAtomic,
} from "./task-index";
export { readActiveTasks } from "./task-list";
export type { TaskIndex, TaskIndexEntry } from "./types";
```

**Step 2: Run the full test suite**

Run: `bun test packages/opencode-plugin/src/tools/task/`
Expected: ALL PASS

**Step 3: Run lint and typecheck**

Run: `bunx biome check packages/opencode-plugin/src/tools/task/`
Expected: No errors. Fix any formatting issues Biome reports.

Run: `bunx tsc --noEmit`
Expected: No type errors.

**Step 4: Commit**

Run: `jj describe -m "chore: export task index symbols and finalize"`
