# Plan: Remove Dead Code from the Codebase (OKM-40)

## Overview

Audit-confirmed dead code removal across both packages (`packages/daemon` and `packages/opencode-plugin`). All findings have been verified via cross-file import analysis. The changes are purely structural — no runtime behavior is modified.

**Assumptions made (not blocked on user input):**
- Commented-out code blocks: none found — no action needed (the codebase has no disabled code, only documentation/TODO comments)
- `util/short-id.ts` and `state/cli.ts` are confirmed orphans — safe to delete (no bin entry, not imported in production code)
- Unused exported symbols that are only `export`ed but used internally have `export` keyword removed (making them module-private) rather than deleting the implementation
- `_`-prefixed parameters (`_stateDir`, `_owner`, `_repo`) follow the intentional discard pattern — keep as-is, they are idiomatic TypeScript
- No dynamic dispatch patterns found in this codebase (no string-keyed method access, no `require()` calls on symbol names) — static analysis is authoritative

---

## Task 1: Remove orphaned legacy file `state/cli.ts` and its test — Independent

**Files:**
- `packages/daemon/src/state/cli.ts` — confirmed not imported by any production file
- `packages/daemon/src/state/__tests__/cli.test.ts` — only imports from `../cli` (the deleted file)

The test file at `packages/daemon/src/state/__tests__/cli.test.ts` imports only from `../cli` and `../fetch` and `../types`. After deleting `state/cli.ts`, the test file will have a broken import and must also be deleted.

### Steps

1. Delete both files:
   ```bash
   rm packages/daemon/src/state/cli.ts
   rm packages/daemon/src/state/__tests__/cli.test.ts
   ```

2. Run tests to confirm nothing else broke:
   ```bash
   cd packages/daemon && bun test
   ```
   Expected: All remaining tests pass (the deleted test file is simply gone from the suite).

3. Run typecheck:
   ```bash
   cd packages/daemon && bunx tsc --noEmit
   ```
   Expected: Zero errors.

---

## Task 2: Remove orphaned utility file `util/short-id.ts` and its test — Independent

**Files:**
- `packages/daemon/src/util/short-id.ts` — not imported by any production file
- `packages/daemon/src/util/__tests__/short-id.test.ts` — only tests this file

### Steps

1. Delete both files:
   ```bash
   rm packages/daemon/src/util/short-id.ts
   rm packages/daemon/src/util/__tests__/short-id.test.ts
   ```

2. Run tests and typecheck:
   ```bash
   cd packages/daemon && bun test && bunx tsc --noEmit
   ```
   Expected: All pass.

---

## Task 3: Remove unused types from `state/types.ts` — Independent

**File:** `packages/daemon/src/state/types.ts`

### 3a. Delete `GitHubLabel` and `GitHubPR` interfaces

These interfaces are defined and exported but never imported anywhere outside this file.

Current code (lines ~175–181):
```typescript
export interface GitHubLabel {
  name: string;
}

export interface GitHubPR {
  labels: GitHubLabel[] | null;
}
```

Delete both interface blocks entirely (including the blank line between them).

Verify no other file references them:
```bash
grep -r "GitHubLabel\|GitHubPR\b" packages/daemon/src --include="*.ts" | grep -v "__tests__"
```
Expected: No output (after deletion).

### 3b. Remove unused Linear type re-exports

Current code (lines ~183–190):
```typescript
export type {
  LinearAttachment,
  LinearIssue,
  LinearIssueRaw,
  LinearLabelNode,
  LinearLabelsContainer,
  LinearStateDict,
} from "./backends/linear";
```

Replace with (keeping only `LinearIssueRaw` which is used by `state/fetch.ts`):
```typescript
export type { LinearIssueRaw } from "./backends/linear";
```

### 3c. Remove `export` from `IssueStateDict` and `CollectedStateDict`

Current code (lines 360–380):
```typescript
export interface IssueStateDict {
  status: IssueStatusLiteral | string;
  labels: string[];
  hasPr: boolean;
  prIsDraft: boolean | null;
  ciStatus: CiStatusLiteral | null;
  hasLiveWorker: boolean;
  workerMode: string | null;
  workerStatus: string | null;
  suggestedAction: ActionType;
  sessionId: string;
  hasUserFeedback: boolean;
  source: IssueSource | null;
}

/**
 * Serialized form of CollectedState.
 */
export interface CollectedStateDict {
  issues: Record<string, IssueStateDict>;
}
```

Remove `export` from both (keep the interfaces and their JSDoc):
```typescript
interface IssueStateDict {
  status: IssueStatusLiteral | string;
  labels: string[];
  hasPr: boolean;
  prIsDraft: boolean | null;
  ciStatus: CiStatusLiteral | null;
  hasLiveWorker: boolean;
  workerMode: string | null;
  workerStatus: string | null;
  suggestedAction: ActionType;
  sessionId: string;
  hasUserFeedback: boolean;
  source: IssueSource | null;
}

/**
 * Serialized form of CollectedState.
 */
interface CollectedStateDict {
  issues: Record<string, IssueStateDict>;
}
```

### Verification
```bash
cd packages/daemon && bunx tsc --noEmit && bun test
```
Expected: Zero TypeScript errors, all tests pass.

---

## Task 4: Remove unused type aliases from `daemon/schemas.ts` — Independent

**File:** `packages/daemon/src/daemon/schemas.ts`

Three exported type aliases are never imported:
- `LinearTeamsResponse`
- `SessionCreateResponse`
- `HealthCheckResponse`

Verify they are unused:
```bash
grep -r "LinearTeamsResponse\|SessionCreateResponse\|HealthCheckResponse" packages/daemon/src --include="*.ts" | grep -v "__tests__" | grep -v "schemas.ts"
```
Expected: No output.

Then remove these three `export type` lines from `packages/daemon/src/daemon/schemas.ts`.

```bash
cd packages/daemon && bunx tsc --noEmit
```

---

## Task 5: Remove unused exports from `daemon/legions-registry.ts` — Independent

**File:** `packages/daemon/src/daemon/legions-registry.ts`

The following are exported but never imported by any non-test file:
- `LegionEntry` interface (line 6) — used as a type within this file only (not imported externally)
- `LegionsRegistry` type alias (line 13) — used as a type within this file only; `daemon/index.ts` imports `readLegionsRegistry` (a function) but not the `LegionsRegistry` type itself
- `withRegistryLock` function (~line 126) — used internally only

Note: `isPidAlive` IS imported by the test file `packages/daemon/src/daemon/__tests__/legions-registry.test.ts` (line 8 in a multi-line import block). It has unit tests exercising it directly. **Do NOT remove `export` from `isPidAlive`.**

Verify `LegionEntry`, `LegionsRegistry`, and `withRegistryLock` are not imported externally (including tests):
```bash
grep -rn "LegionEntry\|LegionsRegistry\b\|withRegistryLock" packages/daemon/src --include="*.ts" | grep -v "legions-registry.ts" | grep -v "schemas.ts"
```
Expected: No output (these symbols do not appear in any other file).

Edit `packages/daemon/src/daemon/legions-registry.ts` — remove `export` from:
- `export interface LegionEntry {` → `interface LegionEntry {`
- `export type LegionsRegistry = Record<string, LegionEntry>;` → `type LegionsRegistry = Record<string, LegionEntry>;`
- `export async function withRegistryLock<T>(` → `async function withRegistryLock<T>(`

Leave `isPidAlive` as `export function isPidAlive(` — it is legitimately tested and exported.

```bash
cd packages/daemon && bunx tsc --noEmit && bun test
```

---

## Task 6: Remove unused `WorkerState` export from `daemon/state-file.ts` — Independent

**File:** `packages/daemon/src/daemon/state-file.ts`

Verify `WorkerState` is not imported elsewhere:
```bash
grep -r "import.*WorkerState\b" packages/daemon/src --include="*.ts"
```
Expected: No output.

Edit: Remove `export` keyword from the type alias.

Current code (~line 24):
```typescript
export type WorkerState = Record<string, WorkerEntry>;
```

After:
```typescript
type WorkerState = Record<string, WorkerEntry>;
```

```bash
cd packages/daemon && bunx tsc --noEmit
```

---

## Task 7: Remove unused exports from `daemon/serve-manager.ts` — Independent

**File:** `packages/daemon/src/daemon/serve-manager.ts`

Verify `SharedServeState` and `SharedServeOptions` are not imported elsewhere:
```bash
grep -r "import.*SharedServeState\|import.*SharedServeOptions" packages/daemon/src --include="*.ts"
```
Expected: No output.

Edit `packages/daemon/src/daemon/serve-manager.ts`:
- `export interface SharedServeState {` → `interface SharedServeState {`
- `export interface SharedServeOptions {` → `interface SharedServeOptions {`

```bash
cd packages/daemon && bunx tsc --noEmit
```

---

## Task 8: Remove unused exports from `opencode-plugin/hooks/session-recovery/index.ts` — Independent

**File:** `packages/opencode-plugin/src/hooks/session-recovery/index.ts`

`detectErrorType` function and `SessionRecoveryHook` interface are exported but never imported by external consumers. The plugin's `src/index.ts` only imports `createSessionRecoveryHook`.

Verify:
```bash
grep -r "import.*session-recovery" packages/opencode-plugin/src --include="*.ts"
```
Expected: One result — only `createSessionRecoveryHook` imported in `src/index.ts`.

Edit `packages/opencode-plugin/src/hooks/session-recovery/index.ts`:
- `export function detectErrorType(` → `function detectErrorType(`
- `export interface SessionRecoveryHook {` → `interface SessionRecoveryHook {`

```bash
cd packages/opencode-plugin && bunx tsc --noEmit && bun test
```

---

## Task 9: Remove unused `CompressionStats` export from `output-compression.ts` — Independent

**File:** `packages/opencode-plugin/src/hooks/output-compression.ts`

Verify `CompressionStats` is not imported elsewhere:
```bash
grep -r "import.*CompressionStats" packages/opencode-plugin/src --include="*.ts"
```
Expected: No output.

Edit: `export interface CompressionStats {` → `interface CompressionStats {`

```bash
cd packages/opencode-plugin && bunx tsc --noEmit
```

---

## Task 10: Remove unused exports from `opencode-plugin/store/content-store.ts` — Independent

**File:** `packages/opencode-plugin/src/store/content-store.ts`

`IndexResult`, `SearchResult`, and `ContentStoreStats` are exported but never imported externally. These are return type annotations for methods used only within the class and its callers, which rely on inference.

Verify:
```bash
grep -r "import.*IndexResult\|import.*SearchResult\|import.*ContentStoreStats" packages/opencode-plugin/src --include="*.ts"
```
Expected: No output.

Edit: Remove `export` from all three interfaces:
- `export interface IndexResult {` → `interface IndexResult {`
- `export interface SearchResult {` → `interface SearchResult {`
- `export interface ContentStoreStats {` → `interface ContentStoreStats {`

```bash
cd packages/opencode-plugin && bunx tsc --noEmit && bun test
```

---

## Task 11: Remove unused re-exports from `opencode-plugin/tools/task/index.ts` — Independent

**File:** `packages/opencode-plugin/src/tools/task/index.ts`

Current full file contents (43 lines):
```typescript
import type { PluginInput, ToolDefinition } from "@opencode-ai/plugin";
import { createTaskClaimNextTool } from "./task-claim";
import { createTaskCreateTool } from "./task-create";
import { createTaskGetTool } from "./task-get";
import { createTaskListTool } from "./task-list";
import { createTaskUpdateTool } from "./task-update";

export { detectCycle } from "./graph";
export {
  indexPathFor,
  readTaskIndex,
  upsertIndexEntry,
  writeTaskIndexAtomic,
} from "./task-index";
export { readActiveTasks, readAllTasks } from "./task-list";
export type { TodoInfo } from "./todo-sync";
export { syncTaskTodoUpdate, syncTaskToTodo } from "./todo-sync";
export type {
  Task,
  TaskCreateInput,
  TaskIndex,
  TaskIndexEntry,
  TaskStatus,
  TaskUpdateInput,
} from "./types";

interface TaskTools {
  task_create: ToolDefinition;
  task_get: ToolDefinition;
  task_update: ToolDefinition;
  task_list: ToolDefinition;
  task_claim_next: ToolDefinition;
}

export function createTaskTools(ctx?: PluginInput, listId?: string): TaskTools {
  return {
    task_create: createTaskCreateTool(ctx, listId),
    task_get: createTaskGetTool(listId),
    task_update: createTaskUpdateTool(ctx, listId),
    task_list: createTaskListTool(listId),
    task_claim_next: createTaskClaimNextTool(ctx, listId),
  };
}
```

Only `createTaskTools` is consumed externally (`src/index.ts` imports it). All other re-exports at the top are unused.

Remove all re-export lines (lines 8–25). The file after edit should be:
```typescript
import type { PluginInput, ToolDefinition } from "@opencode-ai/plugin";
import { createTaskClaimNextTool } from "./task-claim";
import { createTaskCreateTool } from "./task-create";
import { createTaskGetTool } from "./task-get";
import { createTaskListTool } from "./task-list";
import { createTaskUpdateTool } from "./task-update";

interface TaskTools {
  task_create: ToolDefinition;
  task_get: ToolDefinition;
  task_update: ToolDefinition;
  task_list: ToolDefinition;
  task_claim_next: ToolDefinition;
}

export function createTaskTools(ctx?: PluginInput, listId?: string): TaskTools {
  return {
    task_create: createTaskCreateTool(ctx, listId),
    task_get: createTaskGetTool(listId),
    task_update: createTaskUpdateTool(ctx, listId),
    task_list: createTaskListTool(listId),
    task_claim_next: createTaskClaimNextTool(ctx, listId),
  };
}
```

Verify:
```bash
grep -r "import.*from.*tools/task" packages/opencode-plugin/src --include="*.ts" | grep -v "__tests__"
```
Expected: Only one result — `import { createTaskTools } from "./tools/task";` in `src/index.ts`.

```bash
cd packages/opencode-plugin && bunx tsc --noEmit && bun test
```

---

## Task 12: Remove unused items from `opencode-plugin/tools/task/types.ts` — Independent

**File:** `packages/opencode-plugin/src/tools/task/types.ts`

Verify these are unused externally:
```bash
grep -r "TaskListInput\|TaskListInputSchema\|TaskGetInput" packages/opencode-plugin/src --include="*.ts" | grep -v "types.ts" | grep -v "__tests__"
```
Expected: No output.

Edit the file — remove these items:
- `export const TaskListInputSchema = z.object({ ... });` — the entire const block (find the exact block by searching for `TaskListInputSchema`)
- `export type TaskListInput = z.infer<typeof TaskListInputSchema>;` — the entire line
- Remove `export` from `export type TaskGetInput = ...` → `type TaskGetInput = ...`

```bash
cd packages/opencode-plugin && bunx tsc --noEmit
```

---

## Task 13: Remove unused `INDEX_FILENAME` export from `task-index.ts` — Independent

**File:** `packages/opencode-plugin/src/tools/task/task-index.ts`

Verify `INDEX_FILENAME` is not imported elsewhere:
```bash
grep -r "import.*INDEX_FILENAME" packages/opencode-plugin/src --include="*.ts"
```
Expected: No output.

Edit: Remove `export` from the constant.

Current code (~line 12):
```typescript
export const INDEX_FILENAME = "active-index.json";
```

After:
```typescript
const INDEX_FILENAME = "active-index.json";
```

```bash
cd packages/opencode-plugin && bunx tsc --noEmit
```

---

## Task 14: Remove unused fields from delegation types and schema — Independent

**Files:**
- `packages/opencode-plugin/src/delegation/types.ts`
- `packages/opencode-plugin/src/delegation/schemas.ts`

The `BackgroundTaskSchema` uses `.passthrough()` (confirmed at line 30 of `schemas.ts`). This means any persisted task data that contains these fields will not cause parse errors — Zod passes them through. Removing the fields from the schema is safe; existing persisted data is unaffected.

### Edit `packages/opencode-plugin/src/delegation/types.ts`

Current file (28 lines total):
```typescript
export interface BackgroundTask {
  id: string;
  status: "pending" | "running" | "completed" | "failed" | "cancelled";
  agent: string;
  model: string;
  description: string;
  sessionID?: string;
  parentSessionID?: string;
  result?: string;
  error?: string;
  createdAt: number;
  completedAt?: number;
  retryCount?: number;
  concurrencyKey?: string;
  lastMessageCount?: number;
  lastActivityAt?: number;
  staleAlertSent?: boolean;
}

export interface LaunchOptions {
  agent: string;
  prompt: string;
  description: string;
  parentSessionId?: string;
  model?: string;
  skills?: string[];
  systemPrompt?: string;
}
```

Remove these lines from `BackgroundTask`:
- `retryCount?: number;`
- `concurrencyKey?: string;`
- `lastMessageCount?: number;`
- `lastActivityAt?: number;`
- `staleAlertSent?: boolean;`

Remove this line from `LaunchOptions`:
- `skills?: string[];`

### Edit `packages/opencode-plugin/src/delegation/schemas.ts`

Current file (30 lines total):
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

Remove these five lines from `BackgroundTaskSchema`:
- `retryCount: z.number().optional(),`
- `concurrencyKey: z.string().optional(),`
- `lastMessageCount: z.number().optional(),`
- `lastActivityAt: z.number().optional(),`
- `staleAlertSent: z.boolean().optional(),`

### Verification
```bash
grep -r "retryCount\|concurrencyKey\|lastMessageCount\|lastActivityAt\|staleAlertSent\|\.skills" packages/opencode-plugin/src --include="*.ts" | grep -v "types.ts" | grep -v "schemas.ts" | grep -v "__tests__"
```
Expected: No output.

```bash
cd packages/opencode-plugin && bunx tsc --noEmit && bun test
```

---

## Task 15: Remove unused `run_in_background` tool argument — Independent

**File:** `packages/opencode-plugin/src/delegation/delegation-tool.ts`

The `run_in_background` arg is declared in the schema (line 43) but is never referenced in the `execute` function body.

Current code around line 43:
```typescript
    args: {
      prompt: z.string().describe("Task prompt for the agent"),
      category: z
        .string()
        .optional()
        .describe(
          "Category: ultrabrain, deep, visual-engineering, artistry, " +
            "quick, writing, unspecified-low, unspecified-high"
        ),
      model: z.string().optional().describe("Model override (e.g. 'anthropic/claude-opus-4-6')"),
      subagent_type: z
        .string()
        .optional()
        .describe("Specific agent name (e.g. executor, explorer, oracle)"),
      description: z.string().describe("Short task description (5-10 words)"),
      run_in_background: z.boolean().optional().default(true),
    },
```

Remove only the `run_in_background` line:
```typescript
      run_in_background: z.boolean().optional().default(true),
```

The `execute` function body does not reference `run_in_background` in `args` — no further changes needed.

```bash
cd packages/opencode-plugin && bunx tsc --noEmit && bun test
```

---

## Task 16: Remove unused `include_transcript` tool argument — Independent

**File:** `packages/opencode-plugin/src/tools/session/tools.ts`

`include_transcript` is declared on line 59 in the `session_read` args schema and on line 67 in the `typedArgs` type, but is never read in the execute body.

**Step 1:** Remove the schema declaration. Current code (lines 56–59):
```typescript
      session_id: z.string().describe("Session ID to read"),
      limit: z.number().optional().describe("Maximum number of messages to return"),
      include_todos: z.boolean().optional().describe("Include todo list if available"),
      include_transcript: z.boolean().optional().describe("Include transcript log if available"),
```

After removal:
```typescript
      session_id: z.string().describe("Session ID to read"),
      limit: z.number().optional().describe("Maximum number of messages to return"),
      include_todos: z.boolean().optional().describe("Include todo list if available"),
```

**Step 2:** Remove the `typedArgs` type field. Current code (lines 63–68):
```typescript
        const typedArgs = args as {
          session_id: string;
          limit?: number;
          include_todos?: boolean;
          include_transcript?: boolean;
        };
```

After removal:
```typescript
        const typedArgs = args as {
          session_id: string;
          limit?: number;
          include_todos?: boolean;
        };
```

```bash
cd packages/opencode-plugin && bunx tsc --noEmit && bun test
```

---

## Task 17: Clean up `uuid.d.ts` — Remove unused `validate` declaration — Independent

**File:** `packages/daemon/src/types/uuid.d.ts`

Current file:
```typescript
declare module "uuid" {
  export function v5(name: string, namespace: string): string;
  export function validate(uuid: string): boolean;
}
```

Only `v5` is imported anywhere in the codebase. Remove the `validate` declaration:
```typescript
declare module "uuid" {
  export function v5(name: string, namespace: string): string;
}
```

```bash
cd packages/daemon && bunx tsc --noEmit
```

---

## Task 18: Final full test run — Depends on: Tasks 1–17

Run the complete test suite and CI checks for both packages:

```bash
cd /home/ec2-user/legion/packages/daemon && bun test && bunx tsc --noEmit && bunx biome check src/
```
```bash
cd /home/ec2-user/legion/packages/opencode-plugin && bun test && bunx tsc --noEmit && bunx biome check src/
```

Expected output:
- `bun test`: All test suites pass, zero failures
- `bunx tsc --noEmit`: Zero errors
- `bunx biome check src/`: Zero errors or warnings

If Biome reports any issues after the edits (e.g., trailing commas after removing a last element), fix them by removing the trailing comma.

---

## Task 19: Add `noUnusedLocals` and `noUnusedParameters` to tsconfig — Depends on: Task 18

Add TypeScript compiler flags to prevent dead code regression going forward.

**Edit `packages/daemon/tsconfig.json`** — add inside `compilerOptions`:
```json
"noUnusedLocals": true,
"noUnusedParameters": true
```

**Edit `packages/opencode-plugin/tsconfig.json`** — add inside `compilerOptions`:
```json
"noUnusedLocals": true,
"noUnusedParameters": true
```

Run typecheck to surface any remaining issues:
```bash
cd /home/ec2-user/legion/packages/daemon && bunx tsc --noEmit
cd /home/ec2-user/legion/packages/opencode-plugin && bunx tsc --noEmit
```

**How to handle errors:**
- Parameters starting with `_` (e.g., `_stateDir`) are exempt from `noUnusedParameters` — TypeScript respects the underscore convention. These require no changes.
- If any new *non-underscore* unused locals/parameters surface, remove them (or prefix with `_` if they are required by an interface signature).
- If the count of new errors is large (>10), stop, revert the tsconfig change, and document the count in a comment on the issue rather than attempting bulk fixes in this PR.

Run the full suite one final time:
```bash
cd /home/ec2-user/legion/packages/daemon && bun test && bunx tsc --noEmit
cd /home/ec2-user/legion/packages/opencode-plugin && bun test && bunx tsc --noEmit
```

---

## Dependency Graph

```
Task 1  — Independent
Task 2  — Independent
Task 3  — Independent
Task 4  — Independent
Task 5  — Independent
Task 6  — Independent
Task 7  — Independent
Task 8  — Independent
Task 9  — Independent
Task 10 — Independent
Task 11 — Independent
Task 12 — Independent
Task 13 — Independent
Task 14 — Independent
Task 15 — Independent
Task 16 — Independent
Task 17 — Independent
Task 18 — Depends on: Tasks 1–17
Task 19 — Depends on: Task 18
```

Tasks 1–17 are all independent and can execute in parallel. Task 18 aggregates them with a final test run. Task 19 adds regression prevention tooling.

---

## Testing Plan

### Setup
```bash
cd /home/ec2-user/legion
bun install
```

### Health Check
Run typechecks before making any changes (baseline):
```bash
cd packages/daemon && bunx tsc --noEmit
cd packages/opencode-plugin && bunx tsc --noEmit
```
Expected: Zero errors (confirms clean baseline before changes).

### Verification Steps

**1. Orphaned file deletion successful**
- Action: After Tasks 1 and 2, run `cd packages/daemon && bunx tsc --noEmit`
- Expected: No `Cannot find module` errors for deleted files
- Tool: TypeScript compiler

**2. No broken imports from removed exports**
- Action: After Tasks 3–17, run `bunx tsc --noEmit` in both packages
- Expected: Zero errors — confirms no file was importing the removed symbols
- Tool: TypeScript compiler

**3. All tests pass**
- Action: `cd packages/daemon && bun test` then `cd packages/opencode-plugin && bun test`
- Expected: All test suites pass with zero failures
- Tool: Bun test runner

**4. Lint clean**
- Action: `cd packages/daemon && bunx biome check src/` and `cd packages/opencode-plugin && bunx biome check src/`
- Expected: No lint errors or warnings
- Tool: Biome

**5. `noUnusedLocals`/`noUnusedParameters` clean after Task 19**
- Action: `bunx tsc --noEmit` in both packages after adding tsconfig flags
- Expected: Zero new errors — confirms no remaining unused locals/params were introduced
- Tool: TypeScript compiler

### Tools Needed
- Bun (test runner and package manager)
- TypeScript compiler (`bunx tsc`)
- Biome (`bunx biome check`)
