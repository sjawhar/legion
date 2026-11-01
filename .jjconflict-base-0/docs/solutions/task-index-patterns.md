---
title: File-Based Index Optimization Patterns
category: general
tags:
  - file-based-index
  - caching
  - performance
  - active-index
  - task-persistence
  - fallback-pattern
date: 2026-02-15
status: active
module: daemon
---
# File-Based Index Optimization Patterns

**Context:** PR #51 optimized task persistence by adding an `active-index.json` file to avoid full directory scans on hot paths (list, claim). This document captures concrete patterns for similar file-based index optimizations.

**Problem:** File-per-entity storage (e.g., `T-123.json`, `T-456.json`) requires full directory scans to list active entities, which becomes slow as completed/cancelled entities accumulate.

**Solution:** A compact index file tracking only active entity IDs, with on-demand resolution of dependencies.

---

## Core Architecture Patterns

### 1. Index-Aware Read Path with Fallback

**Pattern:** Provide two read functions — one index-aware for hot paths, one full-scan for operations needing complete data.

```typescript
// Hot path: reads only active entities using index
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

// Full scan: used when complete graph is needed (e.g., cycle detection)
export function readAllTasks(taskDir: string): Task[] {
  const fileIds = listTaskFiles(taskDir);
  const tasks: Task[] = [];
  for (const fileId of fileIds) {
    const task = readJsonSafe(join(taskDir, `${fileId}.json`), TaskSchema);
    if (task) tasks.push(task);
  }
  return tasks;
}
```

**Why this works:**
- Index-aware path skips reading completed/cancelled files (performance win)
- Falls back to full scan when index is missing/corrupt (correctness preserved)
- Full-scan function preserved for operations that need ALL entities (cycle detection)

**Pitfall avoided:** Don't try to make one function do both — the performance/correctness trade-offs are different. Keep them separate and name them clearly.

---

### 2. On-Demand Dependency Resolution

**Pattern:** When entities have dependencies (blockedBy, references), resolve missing dependencies individually rather than loading all entities.

```typescript
export function buildTaskMapWithBlockers(
  taskDir: string,
  activeTasks: Task[]
): Map<string, Task> {
  const taskMap = new Map(activeTasks.map((t) => [t.id, t]));

  // Collect missing blocker IDs
  const missingBlockerIds = new Set<string>();
  for (const task of activeTasks) {
    for (const blockerId of task.blockedBy) {
      if (!taskMap.has(blockerId)) {
        missingBlockerIds.add(blockerId);
      }
    }
  }

  // Read only the missing blockers from disk
  for (const blockerId of missingBlockerIds) {
    const blocker = readJsonSafe(join(taskDir, `${blockerId}.json`), TaskSchema);
    if (blocker) {
      taskMap.set(blocker.id, blocker);
    }
  }

  return taskMap;
}
```

**Why this works:**
- Avoids reading ALL files just to resolve a few dependencies
- Typically 0-3 blockers per task — cheap to read individually
- Correctly handles completed/cancelled dependencies (needed for "ready" filtering)

**Pitfall avoided:** Don't assume all dependencies are in the active set. Missing dependencies might be completed (satisfied) or truly missing (unsatisfied). Read them to know.

---

### 3. Index Updates Inside Locks, After File Writes

**Pattern:** Update the index immediately after writing the entity file, inside the same lock.

```typescript
// In task-create.ts, task-update.ts, task-claim.ts:
try {
  const computeResult = () => {
    // ... compute changes ...
    
    validatedTask = TaskSchema.parse(task);
    writeJsonAtomic(join(taskDir, `${taskId}.json`), validatedTask);
    
    // Index update immediately after, still inside lock
    upsertIndexEntry(indexPathFor(taskDir), {
      id: validatedTask.id,
      status: validatedTask.status,
    });
    
    return JSON.stringify({ task: validatedTask });
  };
  
  result = computeResult();
} finally {
  lock.release();
}
```

**Why this works:**
- Both writes happen inside the lock (milliseconds apart)
- Minimizes window for crash between writes
- Index stays consistent with file state

**Known gap acknowledged:** If process crashes between file write and index update, that entity is invisible until next update. This is rare (both writes inside lock) and self-correcting (any update fixes it). A reconcile pass could fix this if it becomes a problem, but defer until there's evidence it's needed.

**Pitfall avoided:** Don't update the index before writing the file — if the file write fails, the index points to a non-existent entity.

---

### 4. Upsert Pattern for Index Maintenance

**Pattern:** Single upsert function handles add/update/remove based on entity state.

```typescript
export function upsertIndexEntry(indexPath: string, entry: TaskIndexEntry): void {
  const validated = TaskIndexEntrySchema.parse(entry);
  const index = readTaskIndex(indexPath) ?? rebuildIndexFromDisk(dirname(indexPath));
  const entries = index.entries.filter((e) => e.id !== validated.id);

  // Add/update for active statuses, remove for terminal statuses
  if (validated.status === "pending" || validated.status === "in_progress") {
    entries.push(validated);
  }

  writeTaskIndexAtomic(indexPath, { version: 1, entries });
}
```

**Why this works:**
- Single function for all index updates (consistency)
- Automatically removes entries when entity reaches terminal state
- Rebuilds from disk if index is missing (crash recovery)

**Pitfall avoided:** Don't have separate add/update/remove functions — you'll forget to call the right one. Make the function smart enough to figure it out.

---

### 5. Rebuild-from-Disk Fallback

**Pattern:** When index is missing or corrupt, rebuild it by scanning entity files.

```typescript
function rebuildIndexFromDisk(taskDir: string): TaskIndex {
  const entries: TaskIndexEntry[] = [];
  try {
    const files = readdirSync(taskDir).filter((f) => f.endsWith(".json") && f.startsWith("T-"));
    for (const file of files) {
      const task = readJsonSafe(join(taskDir, file), TaskSchema);
      if (task && (task.status === "pending" || task.status === "in_progress")) {
        entries.push({ id: task.id, status: task.status });
      }
    }
  } catch {
    // Directory unreadable — return empty index
  }
  return { version: 1, entries };
}
```

**Why this works:**
- Automatically recovers from index corruption or deletion
- Called lazily (only when index is needed but missing)
- Prevents hiding existing entities due to index issues

**Pitfall avoided:** Don't fail hard when index is missing — rebuild it. The entity files are the source of truth, not the index.

---

## Testing Patterns

### 1. Integration Tests for Index Updates

**Pattern:** Test that index is updated correctly after each write operation.

```typescript
describe("index integration: task_create", () => {
  it("adds new task to index on create", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({ subject: "Indexed" }, makeContext()));
    
    const index = readTaskIndex(idxPath());
    expect(index).not.toBeNull();
    const entry = index?.entries.find((e) => e.id === result.task.id);
    expect(entry).toBeTruthy();
    expect(entry?.status).toBe("pending");
  });
});
```

**Why this works:**
- Tests the actual integration, not just the index functions in isolation
- Catches missing upsert calls in write paths
- Verifies index state after real operations

---

### 2. Regression Tests for Behavior Preservation

**Pattern:** Write tests that pass before and after the optimization, proving behavior is unchanged.

```typescript
describe("index-aware listing", () => {
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
});
```

**Why this works:**
- Tests pass with old code (reads all files) and new code (uses index)
- Proves correctness is preserved despite structural change
- Catches subtle bugs in dependency resolution

---

### 3. Fallback and Recovery Tests

**Pattern:** Test that system works correctly when index is missing or corrupt.

```typescript
it("recovers from corrupted index by falling back to full scan", async () => {
  writeTask(tempDir, makeTask({ id: "T-a", status: "pending" }));
  writeTask(tempDir, makeTask({ id: "T-b", status: "in_progress" }));

  fs.writeFileSync(indexPathFor(tempDir), "{invalid json");

  const tool = createTaskListTool(tempDir);
  const result = JSON.parse(await tool.execute({}, makeContext()));

  const ids = result.tasks.map((t: { id: string }) => t.id);
  expect(ids).toContain("T-a");
  expect(ids).toContain("T-b");
});
```

**Why this works:**
- Proves system degrades gracefully when index is broken
- Prevents hiding entities due to index corruption
- Tests the rebuild-from-disk path

---

## Schema and Type Patterns

### 1. Minimal Index Schema

**Pattern:** Index stores only what's needed for filtering, not full entity data.

```typescript
export const TaskIndexEntrySchema = z.object({
  id: z.string(),
  status: TaskStatusSchema,  // Only field needed for active/inactive filtering
});

export const TaskIndexSchema = z.object({
  version: z.literal(1),  // For future schema migrations
  entries: z.array(TaskIndexEntrySchema).default([]),
});
```

**Why this works:**
- Keeps index file small (scales to thousands of entities)
- Only stores data needed for hot path decisions
- Version field enables future migrations without breaking old code

**Pitfall avoided:** Don't store full entity data in the index — that defeats the purpose. Store only what you need to decide which files to read.

---

### 2. Guardrails Alongside Performance Work

**Pattern:** Add simple guardrails (size limits) in the same PR as performance work.

```typescript
export const MAX_DESCRIPTION_CHARS = 2_000;

export const TaskCreateInputSchema = z.object({
  subject: z.string(),
  description: z.string().max(MAX_DESCRIPTION_CHARS).optional(),
  // ...
});
```

**Why this works:**
- Prevents unbounded growth that would negate performance gains
- Simple to implement (Zod validation)
- Catches issues early (at write time, not read time)

**Pitfall avoided:** Don't optimize reads without limiting writes — you'll just delay the problem.

---

## Implementation Sequence

**Lesson from PR #51:** The implementation followed a strict TDD sequence that minimized risk:

1. **Add schemas** (types, validation) — establishes contracts
2. **Add index helpers** (read/write/upsert) — isolated, testable
3. **Integrate into write paths** (create/update/claim) — index stays consistent
4. **Add index-aware read paths** (list/claim) — performance win
5. **Add guardrails** (size limits) — prevents future issues
6. **Export symbols** — makes functionality available

**Why this sequence works:**
- Each step is independently testable
- Tests written before implementation (TDD)
- Integration happens after helpers are proven correct
- Performance optimization comes after correctness is established

**Pitfall avoided:** Don't optimize reads before fixing writes — you'll get inconsistent index state.

---

## When to Use This Pattern

**Good fit:**
- File-per-entity storage with growing number of inactive entities
- Hot paths that list/filter entities frequently
- Entities have clear active/inactive states
- Dependencies between entities are sparse (few per entity)

**Poor fit:**
- Entities don't have clear active/inactive distinction
- Most operations need all entities anyway
- Dependencies are dense (many per entity, would require reading most files)
- Write volume is extremely high (index updates become bottleneck)

---

## Complexity That Could Have Been Avoided

### 1. Rebuild-from-Disk Complexity

**What happened:** Initial implementation had `upsertIndexEntry` create an empty index if missing. This could hide existing tasks if the index was accidentally deleted.

**Fix:** Added `rebuildIndexFromDisk` to scan entity files when index is missing.

**Lesson:** For file-based storage, the files are the source of truth. When index is missing, rebuild it from files rather than starting empty.

**Simpler alternative:** Could have required manual index rebuild via CLI command, but automatic recovery is more robust.

---

### 2. Type Annotations for Return Types

**What happened:** Added explicit `Promise<string>` return types to tool execute functions.

**Why:** TypeScript inference works, but explicit types catch mistakes where you accidentally return the wrong type.

**Lesson:** For public APIs (tool execute functions), explicit return types are documentation and safety, even when inference works.

---

## Good Patterns Worth Replicating

### 1. Atomic Writes with Temp+Rename

**Pattern:** Write to temp file, then rename to final location (atomic on POSIX).

```typescript
export function writeJsonAtomic(filePath: string, data: unknown): void {
  const tempPath = `${filePath}.tmp`;
  writeFileSync(tempPath, JSON.stringify(data, null, 2));
  renameSync(tempPath, filePath);
}
```

**Why replicate:** Prevents partial writes from being read. Rename is atomic on POSIX systems.

---

### 2. Safe JSON Reads with Schema Validation

**Pattern:** Read JSON, validate with Zod, return null on any error.

```typescript
export function readJsonSafe<T>(filePath: string, schema: z.ZodType<T>): T | null {
  try {
    const parsed = JSON.parse(readFileSync(filePath, "utf-8"));
    const result = schema.safeParse(parsed);
    return result.success ? result.data : null;
  } catch {
    return null;
  }
}
```

**Why replicate:** Handles missing files, malformed JSON, and schema violations uniformly. Caller doesn't need to distinguish error types.

---

### 3. Lock-Scoped Operations with Try-Finally

**Pattern:** Acquire lock, compute result in try block, release in finally.

```typescript
const lock = acquireLock(taskDir);
if (!lock) {
  return JSON.stringify({ error: "lock_failed" });
}

let result: string;
try {
  result = computeResult();
} finally {
  lock.release();
}
return result;
```

**Why replicate:** Ensures lock is always released, even if computation throws. Prevents deadlocks.

---

## Summary: What to Tell Someone Implementing Similar Work

1. **Keep two read paths:** Index-aware for hot paths, full-scan for operations needing complete data. Don't try to make one function do both.

2. **Resolve dependencies on-demand:** Don't load all entities just to check a few dependencies. Read missing dependencies individually.

3. **Update index inside locks, after file writes:** Minimize crash window, keep index consistent with files.

4. **Rebuild from disk when index is missing:** Files are source of truth. Don't hide entities due to index issues.

5. **Test integration, not just units:** Verify index is updated after real operations, not just in isolation.

6. **Write regression tests:** Tests that pass before and after prove behavior is preserved.

7. **Add guardrails alongside performance work:** Size limits prevent unbounded growth that negates optimization.

8. **Follow TDD sequence:** Schemas → helpers → write integration → read optimization → guardrails. Each step independently testable.

9. **Store minimal data in index:** Only what's needed for filtering decisions. Full data stays in entity files.

10. **Acknowledge known gaps explicitly:** Crash recovery gap is rare and self-correcting. Document it, defer fix until there's evidence it's needed.
