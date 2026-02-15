# Delegation Hardening Patterns

**Issue**: LEG-133  
**PR**: https://github.com/sjawhar/legion/pull/48  
**Date**: 2026-02-15

## Overview

This document captures patterns and learnings from hardening the opencode-legion delegation system for production use. The work focused on three core areas: task persistence, agent restrictions, and config schema extension.

## Design Patterns

### 1. File-Based Task Persistence with In-Memory Cache

**Pattern**: Dual-layer storage with file system as source of truth and in-memory cache for performance.

**Implementation** (`task-storage.ts`):
- Tasks stored in `.legion/tasks/{id}.json`
- Atomic writes via tmp+rename pattern
- In-memory cache in `BackgroundTaskManager` for fast access
- Persistence survives session deletion

**Why This Approach**:
- File system provides durability across process restarts
- In-memory cache avoids repeated disk I/O during active operations
- Atomic writes prevent partial state corruption
- Separation of concerns: storage layer is pure I/O, manager handles business logic

**Key Implementation Details**:
```typescript
// Atomic write pattern
await fs.writeFile(tmp, data, { encoding: "utf-8", mode: FILE_MODE });
await fs.rename(tmp, dest);
```

### 2. Centralized Agent Restrictions

**Pattern**: Single source of truth for agent capabilities with defense-in-depth enforcement.

**Implementation** (`agent-restrictions.ts`):
- `AGENT_RESTRICTIONS` record maps agent names to tool deny lists
- `getAgentToolRestrictions()` provides case-insensitive lookup
- `isLeafAgent()` helper identifies agents that cannot delegate
- Restrictions applied at SDK level via `startPrompt()` tools parameter

**Why This Approach**:
- Centralization prevents drift between delegation checks and actual restrictions
- Defense-in-depth: restrictions enforced both at tool invocation and SDK level
- Case-insensitive matching handles agent name variations
- Unknown agents default to open (fail-open for extensibility)

**Migration from Set to Function**:
```typescript
// Before: hardcoded Set in delegation-tool.ts
const LEAF_AGENTS = new Set(["explorer", "librarian", ...]);

// After: centralized function with richer semantics
if (callingAgent && isLeafAgent(callingAgent)) {
  return `Error: Agent '${callingAgent}' cannot delegate...`;
}
```

### 3. Config Schema with Defaults and Merging

**Pattern**: Layered configuration with explicit defaults and precedence rules.

**Implementation** (`config/index.ts`):
- `DEFAULT_CONFIG` defines baseline values
- `applyDefaults()` ensures all fields have values
- `mergeConfig()` handles user + repo config precedence
- Zod schemas validate structure at load time

**Why This Approach**:
- Explicit defaults make behavior predictable
- Layered merging supports user-level and repo-level overrides
- Validation at load time catches errors early
- Separation of merge logic from default application

**Key Pattern**:
```typescript
// Merge first, apply defaults last
let merged: PluginConfig = {};
if (userConfig) merged = mergeConfig(merged, userConfig);
if (repoConfig) merged = mergeConfig(merged, repoConfig);
return applyDefaults(merged);
```

### 4. Idempotent Finalization

**Pattern**: Terminal state transitions are idempotent and preserve first outcome.

**Implementation** (`background-manager.ts`):
- `finalize()` method handles all terminal state transitions
- Early return if already in terminal state
- Eager output caching on completion
- Cleanup of runtime state (maps, subagent sessions)

**Why This Approach**:
- Idempotency prevents double-cleanup bugs
- First terminal state wins (prevents status thrashing)
- Eager caching avoids repeated API calls
- Centralized cleanup reduces error surface

**Key Pattern**:
```typescript
async finalize(task, status, opts) {
  // Idempotency guard
  if (task.status === "completed" || task.status === "failed" || task.status === "cancelled") {
    return;
  }
  
  // State transition
  task.status = status;
  task.completedAt = Date.now();
  
  // Eager caching for completed tasks
  if (status === "completed" && task.sessionID) {
    const output = await this.fetchSessionOutput(task.sessionID);
    if (output) task.result = output;
  }
  
  // Cleanup
  if (task.sessionID) {
    this.tasksBySessionId.delete(task.sessionID);
    unregisterSubagentSession(task.sessionID);
  }
  
  // Persist
  await writeTask(this.directory, task);
}
```

## Edge Cases Handled

### 1. Path Traversal Prevention

**Risk**: Malicious task IDs could write outside `.legion/tasks/`

**Mitigation**:
```typescript
function validateTaskId(taskId: string): void {
  if (
    taskId.includes("/") ||
    taskId.includes("\\") ||
    taskId === "." ||
    taskId === ".." ||
    taskId.includes("..")
  ) {
    throw new Error(`Invalid task ID: ${taskId}`);
  }
}
```

**Tests**: 6 test cases covering `../`, `/`, `\`, `.`, `..`, embedded `..`

### 2. Symlink Protection

**Risk**: Symlinks could expose files outside task directory

**Mitigation**:
```typescript
const stat = await fs.lstat(filePath);
if (!stat.isFile()) {
  console.warn(`[task-storage] Skipping non-regular file: ${filePath}`);
  return null;
}
```

**Tests**: Verified both `readTask()` and `listTasks()` skip symlinks

### 3. Crash Safety

**Risk**: Process crash during write leaves partial state

**Mitigation**:
- Atomic writes via tmp+rename
- `.tmp` files ignored by `listTasks()`
- Orphaned tmp files cleaned up on next write

**Tests**: Verified `.tmp` files don't appear in task listings

### 4. Race Conditions

**Risk**: File deleted between `readdir()` and `readFile()`

**Mitigation**:
```typescript
try {
  const stat = await fs.lstat(filePath);
  const data = await fs.readFile(filePath, "utf-8");
  // ...
} catch (err: unknown) {
  if (isEnoent(err)) {
    continue; // Skip gracefully
  }
  // ...
}
```

**Tests**: Mock-based test simulating file deletion mid-iteration

### 5. Malformed JSON

**Risk**: Corrupted task files break listing

**Mitigation**:
- `try/catch` around `JSON.parse()`
- Warning logged, file skipped
- Other tasks still returned

**Tests**: Verified both `readTask()` and `listTasks()` handle malformed JSON

### 6. Session Deletion Before Completion

**Risk**: Session deleted while task still running

**Mitigation**:
```typescript
async cleanup(sessionID: string): Promise<void> {
  const taskId = this.tasksBySessionId.get(sessionID);
  if (!taskId) {
    unregisterSubagentSession(sessionID);
    return;
  }

  const task = this.tasks.get(taskId);
  if (task && (task.status === "pending" || task.status === "running")) {
    await this.finalize(task, "failed", { error: "Session deleted before completion" });
  }

  // Clean up runtime state
  this.tasks.delete(taskId);
  this.tasksBySessionId.delete(sessionID);
  unregisterSubagentSession(sessionID);
}
```

**Tests**: Verified running tasks finalized as failed, completed tasks left untouched

### 7. Failed Session Creation

**Risk**: Session creation fails, leaving orphaned task

**Mitigation**:
```typescript
try {
  const session = await this.client.session.create(...);
  // ...
} catch (err) {
  await this.finalize(task, "failed", {
    error: err instanceof Error ? err.message : String(err),
  });
  this.tasks.delete(task.id); // Remove from memory
}
```

**Tests**: Verified task finalized and removed from map on creation failure

## Testing Strategies

### 1. Comprehensive Edge Case Coverage

**Approach**: Dedicated test suites for each module with focus on failure modes

**Coverage**:
- `task-storage.test.ts`: 246 lines, 15 test cases
- `agent-restrictions.test.ts`: 173 lines, 12 test cases
- `finalize.test.ts`: 223 lines, 8 test cases
- `config/index.test.ts`: 199 lines, 9 test cases

**Key Patterns**:
- Test both success and failure paths
- Use temp directories for isolation
- Mock file system operations for race condition tests
- Verify warnings logged for non-fatal errors

### 2. Test Helpers for Readability

**Pattern**: Factory functions and accessors reduce boilerplate

```typescript
function makeTask(overrides: Partial<BackgroundTask> = {}): BackgroundTask {
  return {
    id: "bg_test",
    status: "running",
    agent: "explore",
    model: "anthropic/claude-sonnet-4-20250514",
    description: "test task",
    createdAt: Date.now(),
    ...overrides,
  };
}

function getTasks(manager: BackgroundTaskManager): Map<string, BackgroundTask> {
  return (manager as unknown as { tasks: Map<string, BackgroundTask> }).tasks;
}
```

### 3. Spy-Based Verification

**Pattern**: Verify side effects without full integration

```typescript
const abortSpy = spyOn(session, "abort");
await manager.cancel(task.id);
expect(abortSpy).toHaveBeenCalledTimes(1);
```

### 4. Idempotency Tests

**Pattern**: Call operation twice, verify first result preserved

```typescript
await manager.finalize(task, "completed");
const firstCompletedAt = task.completedAt;

await manager.finalize(task, "cancelled");

expect(task.status).toBe("completed");
expect(task.completedAt).toBe(firstCompletedAt);
```

## Architecture Decisions

### 1. Async Finalization

**Decision**: Make `finalize()`, `cancel()`, `cancelAll()`, `cleanup()`, and `handleSessionStatus()` async

**Rationale**:
- Persistence requires async I/O
- Eager output caching requires async API calls
- Consistent async interface simplifies error handling

**Impact**:
- All callers must `await` these methods
- Error handling via try/catch instead of sync throws
- Enables future optimizations (batching, retries)

### 2. Fail-Open for Unknown Agents

**Decision**: Unknown agents get empty restrictions (all tools allowed)

**Rationale**:
- Extensibility: new agents don't require code changes
- Development: easier to prototype new agent types
- Safety: leaf agents explicitly restricted, orchestrators open by default

**Trade-off**: Typos in agent names won't be caught by restrictions

### 3. Separate Persistence and Cache

**Decision**: File system is source of truth, in-memory cache is ephemeral

**Rationale**:
- Durability: tasks survive process restarts
- Performance: cache avoids repeated disk I/O
- Simplicity: clear separation of concerns

**Future Work**: Startup reconciliation to load persisted tasks into cache

### 4. Eager Output Caching

**Decision**: Fetch and cache output immediately on completion

**Rationale**:
- Avoids repeated API calls for `background_output`
- Captures output before session cleanup
- Simplifies `getOutput()` logic (check cache first)

**Trade-off**: Completion takes slightly longer due to API call

### 5. Config Defaults Applied Last

**Decision**: Merge configs first, apply defaults as final step

**Rationale**:
- Preserves user intent (explicit `undefined` vs missing field)
- Simplifies merge logic (no need to handle defaults)
- Clear precedence: user > repo > defaults

**Pattern**:
```typescript
let merged: PluginConfig = {};
if (userConfig) merged = mergeConfig(merged, userConfig);
if (repoConfig) merged = mergeConfig(merged, repoConfig);
return applyDefaults(merged);
```

## Documentation Patterns

### 1. Migration Guide

**Location**: `packages/opencode-plugin/docs/migration.md`

**Content**:
- Configuration options with examples
- Default values clearly stated
- Complete example showing all fields
- Rollback instructions

**Why**: Users need clear guidance on new features without reading code

### 2. Inline Comments for Non-Obvious Logic

**Examples**:
```typescript
// Clean up runtime state. Task file persists on disk for TTL.
// finalize() already cleaned tasksBySessionId and subagent session,
// but we also remove from tasks Map to prevent memory accumulation.
this.tasks.delete(taskId);
```

**Why**: Explains intent behind seemingly redundant operations

### 3. Skill Discipline Documentation

**Addition**: New section in `legion-worker/SKILL.md`

**Content**:
```markdown
## Skill Discipline

You are executing work with an approved plan. Do NOT invoke the brainstorming 
or writing-plans skills — your workflow has already been designed. Follow your 
assigned workflow file. The individual skills referenced in your workflow 
(TDD, subagent-driven-development, etc.) are appropriate to load and use.
```

**Why**: Prevents workers from re-planning already-planned work

## Remaining Work (Not in PR)

The PR description explicitly lists deferred features:

1. **Parent push notification** — notify parent when child completes
2. **Concurrency limiting** — enforce `perModel` and `global` limits
3. **Stale timeout** — detect and alert on inactive tasks
4. **Retry with model fallback** — retry failed tasks with different model
5. **Signal handling** — graceful shutdown on SIGTERM/SIGINT
6. **Retention/cleanup** — delete tasks after `taskRetentionMs`
7. **Startup reconciliation** — load persisted tasks into cache on startup

**Why Deferred**: Wave 1 focused on foundational infrastructure. These features build on the persistence and config layers.

## Key Takeaways

1. **Atomic operations prevent corruption**: tmp+rename pattern ensures writes are all-or-nothing
2. **Idempotency simplifies error handling**: operations can be safely retried
3. **Defense-in-depth for security**: validate at multiple layers (tool invocation, SDK, file system)
4. **Explicit defaults beat implicit**: `applyDefaults()` makes behavior predictable
5. **Test edge cases exhaustively**: path traversal, symlinks, race conditions, malformed data
6. **Separate concerns cleanly**: storage layer is pure I/O, manager handles business logic
7. **Document non-obvious decisions**: inline comments explain "why" not just "what"
8. **Fail gracefully**: log warnings, skip bad data, continue processing

## References

- Plan: `.sisyphus/plans/delegation-hardening.md`
- Oracle/Ultrabrain review: `~/.agent-mail/delegation-hardening-review.md`
- Controller feedback: `~/.agent-mail/delegation-hardening-feedback.md`
- PR: https://github.com/sjawhar/legion/pull/48
