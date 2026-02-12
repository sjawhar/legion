# OpenCode-Legion Plugin: MVP Phases 2-4 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Complete the opencode-legion plugin MVP with auto-continuation on incomplete todos, compaction resilience, stop-continuation wiring, and a delegate-only conductor agent.

**Architecture:** 5 items extending the existing plugin. All new hooks follow the factory pattern (`createXxxHook(ctx)` returning handler objects). The plugin's `index.ts` wires them into lifecycle hooks. Tests use the existing stub infrastructure in `__tests__/integration.test.ts`.

**Tech Stack:** TypeScript on Bun, `@opencode-ai/plugin` API, Bun test runner, Biome lint

**Ref:** `docs/brainstorms/2026-02-12-opencode-legion-mvp-phases-2-4-brainstorm.md`

**Review reconciliation (Metis + Oracle findings addressed):**
- Bug #1 (duplicate timer): `handleIdle` cancels existing timer before scheduling new one
- Bug #2 (isBackgroundSession unused): Checked early — skip continuation for background sessions
- Bug #3 (agent gating fails open): Require resolved agent in CONTINUATION_AGENTS; bail if unresolved
- Bug #4 (todoUpdate assumed): Feature-detect via `sessionApi.todoUpdate` per `tools/task/todo-sync.ts:79`
- Bug #5 (session-recovery): Add `isRecovering` callback; wire from session-recovery hook
- Test #6 (Task 3 empty): Replaced with end-to-end test through OpenCodeLegion() proving stop→idle→no-continuation→user-message→idle→continuation
- Test #7 (flaky sleeps): `gracePeriodMs: 0` for synchronous-like behavior; only cancel/cleanup tests need real timer (100ms with generous margin)
- Test #8 (mock shapes): Tests assert `path.id` and `query.directory` on promptAsync calls
- Test #9 (no wiring test): Added end-to-end integration test through plugin entry point
- Drift #11 (prompt): Simplified to match brainstorm "minimal" decision
- Drift #12 (conductor temp): Changed to 0.7 per brainstorm
- Drift #13 (stop-continuation): Slashcommand interception is correct — plugins can't register command templates (OMO-internal). Documented.

---

### Task 1: Compaction Context Injector

Pure template string injected into compaction summaries via `experimental.session.compacting`. No state, no dependencies.

**Files:**
- Create: `packages/opencode-plugin/src/hooks/compaction-context-injector.ts`
- Modify: `packages/opencode-plugin/src/hooks/index.ts`
- Modify: `packages/opencode-plugin/src/index.ts`
- Test: `packages/opencode-plugin/src/__tests__/integration.test.ts`

**Step 1: Write the failing test**

Add to the `describe("opencode-legion plugin")` block in integration.test.ts:

```typescript
describe("compaction context injector", () => {
  it("injects context template into compaction output via plugin", async () => {
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-compact-ctx-"));
    try {
      const ctx = createStubContext(tempRoot);
      const hooks = await OpenCodeLegion(ctx);
      const compactingHook = hooks["experimental.session.compacting"];
      expect(compactingHook).toBeTruthy();
      if (!compactingHook) throw new Error("Missing compacting hook");

      const output = { context: [] as string[] };
      await compactingHook({ sessionID: "session" }, output);

      expect(output.context.length).toBeGreaterThan(0);
      const template = output.context[0];
      expect(template).toContain("User Requests");
      expect(template).toContain("Remaining Tasks");
      expect(template).toContain("Active Working Context");
      expect(template).toContain("Explicit Constraints");
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/opencode-plugin/src/__tests__/integration.test.ts --test-name-pattern "compaction context injector"`

Expected: FAIL — `hooks["experimental.session.compacting"]` is undefined.

**Step 3: Write the hook**

Create `packages/opencode-plugin/src/hooks/compaction-context-injector.ts`:

```typescript
const COMPACTION_CONTEXT_TEMPLATE = `When summarizing this session, you MUST include the following sections in your summary:

## 1. User Requests (As-Is)
- List all original user requests exactly as they were stated
- Preserve the user's exact wording and intent

## 2. Final Goal
- What the user ultimately wanted to achieve
- The end result or deliverable expected

## 3. Work Completed
- What has been done so far
- Files created/modified
- Features implemented
- Problems solved

## 4. Remaining Tasks
- What still needs to be done
- Pending items from the original request
- Follow-up tasks identified during the work

## 5. Active Working Context (For Seamless Continuation)
- **Files**: Paths of files currently being edited or frequently referenced
- **Code in Progress**: Key code snippets, function signatures, or data structures under active development
- **External References**: Documentation URLs, library APIs, or external resources being consulted
- **State & Variables**: Important variable names, configuration values, or runtime state relevant to ongoing work

## 6. Explicit Constraints (Verbatim Only)
- Include ONLY constraints explicitly stated by the user or in existing AGENTS.md context
- Quote constraints verbatim (do not paraphrase)
- Do NOT invent, add, or modify constraints
- If no explicit constraints exist, write "None"

This context is critical for maintaining continuity after compaction.`;

export function createCompactionContextInjector(): () => string {
  return () => COMPACTION_CONTEXT_TEMPLATE;
}
```

**Step 4: Export from hooks/index.ts**

Add to `packages/opencode-plugin/src/hooks/index.ts`:

```typescript
export { createCompactionContextInjector } from "./compaction-context-injector";
```

**Step 5: Wire into index.ts**

Import `createCompactionContextInjector` from `"./hooks"`.

After `const stopContinuationGuardHook = createStopContinuationGuardHook();`, add:

```typescript
const compactionContextInjector = createCompactionContextInjector();
```

In the return object, add after `"experimental.chat.system.transform"`:

```typescript
"experimental.session.compacting": async (
  _input: { sessionID: string },
  output: { context: string[] },
): Promise<void> => {
  output.context.push(compactionContextInjector());
},
```

**Step 6: Run test to verify it passes**

Run: `bun test packages/opencode-plugin/src/__tests__/integration.test.ts --test-name-pattern "compaction context injector"`

Expected: PASS

**Step 7: Run full test suite + lint + typecheck**

Run: `bun test packages/opencode-plugin && bunx tsc --noEmit -p packages/opencode-plugin/tsconfig.json && bunx biome check packages/opencode-plugin/src/`

Expected: All pass

**Step 8: Commit**

```
feat(opencode-plugin): add compaction context injector

Injects structured template into compaction summaries via
experimental.session.compacting hook. Ensures continuity across
compaction by preserving user requests, goals, work completed,
remaining tasks, and active working context.
```

---

### Task 2: Compaction Todo Preserver

Captures todo state before compaction, restores after if todos were lost. Feature-detects `todoUpdate` API availability per the pattern in `tools/task/todo-sync.ts:79`.

**Files:**
- Create: `packages/opencode-plugin/src/hooks/compaction-todo-preserver.ts`
- Modify: `packages/opencode-plugin/src/hooks/index.ts`
- Modify: `packages/opencode-plugin/src/index.ts`
- Test: `packages/opencode-plugin/src/__tests__/integration.test.ts`

**Step 1: Write the failing tests**

```typescript
describe("compaction todo preserver", () => {
  it("restores todos after compaction when missing", async () => {
    const sessionID = "session-compact-missing";
    let capturedRestoreCall: { sessionID: string; todos: unknown[] } | undefined;
    let todoCalls = 0;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-todo-preserve-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => {
            todoCalls++;
            // Call 1 (capture): return todos. Call 2 (restore check): return empty
            if (todoCalls === 1) {
              return { data: [
                { id: "1", content: "Task A", status: "pending", priority: "high" },
                { id: "2", content: "Task B", status: "completed", priority: "low" },
              ] };
            }
            return { data: [] };
          },
          todoUpdate: async ({ path: p, body }: { path: { id: string }; body: { todos: unknown[] } }) => {
            capturedRestoreCall = { sessionID: p.id, todos: body.todos };
          },
        },
      });

      const { createCompactionTodoPreserverHook } = await import("../hooks/compaction-todo-preserver");
      const hook = createCompactionTodoPreserverHook(ctx);

      await hook.capture(sessionID);
      await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });

      expect(capturedRestoreCall).toBeTruthy();
      expect(capturedRestoreCall!.sessionID).toBe(sessionID);
      expect(capturedRestoreCall!.todos).toHaveLength(2);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("skips restore when todos still exist post-compaction", async () => {
    const sessionID = "session-compact-present";
    let todoUpdateCalled = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-todo-skip-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({
            data: [{ id: "1", content: "Still here", status: "pending", priority: "high" }],
          }),
          todoUpdate: async () => { todoUpdateCalled = true; },
        },
      });

      const { createCompactionTodoPreserverHook } = await import("../hooks/compaction-todo-preserver");
      const hook = createCompactionTodoPreserverHook(ctx);

      await hook.capture(sessionID);
      await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });

      expect(todoUpdateCalled).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("cleans up snapshot on session.deleted", async () => {
    const sessionID = "session-compact-cleanup";
    let todoUpdateCalled = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-todo-cleanup-"));
    try {
      let todoCalls = 0;
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => {
            todoCalls++;
            return todoCalls === 1
              ? { data: [{ id: "1", content: "Task", status: "pending" }] }
              : { data: [] };
          },
          todoUpdate: async () => { todoUpdateCalled = true; },
        },
      });

      const { createCompactionTodoPreserverHook } = await import("../hooks/compaction-todo-preserver");
      const hook = createCompactionTodoPreserverHook(ctx);

      await hook.capture(sessionID);
      await hook.event({ event: { type: "session.deleted", properties: { info: { id: sessionID } } } });
      // Compact after delete — snapshot gone, no restore
      await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });

      expect(todoUpdateCalled).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("skips restore when todoUpdate is unavailable", async () => {
    const sessionID = "session-compact-no-api";
    let todoCalls = 0;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-todo-noapi-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => {
            todoCalls++;
            return todoCalls === 1
              ? { data: [{ id: "1", content: "Task", status: "pending" }] }
              : { data: [] };
          },
          // No todoUpdate — feature detection should skip restore gracefully
        },
      });

      const { createCompactionTodoPreserverHook } = await import("../hooks/compaction-todo-preserver");
      const hook = createCompactionTodoPreserverHook(ctx);

      await hook.capture(sessionID);
      // Should not throw
      await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });
});
```

**Step 2: Run test to verify it fails**

Run: `bun test packages/opencode-plugin/src/__tests__/integration.test.ts --test-name-pattern "compaction todo preserver"`

Expected: FAIL — module not found.

**Step 3: Write the hook**

Create `packages/opencode-plugin/src/hooks/compaction-todo-preserver.ts`:

```typescript
import type { PluginInput } from "@opencode-ai/plugin";

interface TodoSnapshot {
  id: string;
  content: string;
  status: "pending" | "in_progress" | "completed" | "cancelled";
  priority?: "low" | "medium" | "high";
}

function extractTodos(response: unknown): TodoSnapshot[] {
  const payload = response as { data?: unknown };
  if (Array.isArray(payload?.data)) {
    return payload.data as TodoSnapshot[];
  }
  if (Array.isArray(response)) {
    return response as TodoSnapshot[];
  }
  return [];
}

function resolveSessionID(props?: Record<string, unknown>): string | undefined {
  return (props?.sessionID ??
    (props?.info as { id?: string } | undefined)?.id) as string | undefined;
}

export interface CompactionTodoPreserver {
  capture: (sessionID: string) => Promise<void>;
  event: (input: { event: { type: string; properties?: unknown } }) => Promise<void>;
}

export function createCompactionTodoPreserverHook(
  ctx: PluginInput,
): CompactionTodoPreserver {
  const snapshots = new Map<string, TodoSnapshot[]>();

  const capture = async (sessionID: string): Promise<void> => {
    if (!sessionID) return;
    try {
      const response = await ctx.client.session.todo({ path: { id: sessionID } });
      const todos = extractTodos(response);
      if (todos.length === 0) return;
      snapshots.set(sessionID, todos);
    } catch {
      // best-effort capture
    }
  };

  const restore = async (sessionID: string): Promise<void> => {
    const snapshot = snapshots.get(sessionID);
    if (!snapshot || snapshot.length === 0) return;

    try {
      const response = await ctx.client.session.todo({ path: { id: sessionID } });
      const currentTodos = extractTodos(response);
      if (currentTodos.length > 0) {
        snapshots.delete(sessionID);
        return;
      }
    } catch {
      // if we can't check, attempt restore anyway
    }

    // Feature-detect todoUpdate (same pattern as tools/task/todo-sync.ts:79)
    const sessionApi = ctx.client.session as unknown as Record<string, CallableFunction>;
    if (!sessionApi.todoUpdate) {
      snapshots.delete(sessionID);
      return;
    }

    try {
      await sessionApi.todoUpdate({
        path: { id: sessionID },
        body: { todos: snapshot },
      });
    } catch {
      // best-effort restore
    } finally {
      snapshots.delete(sessionID);
    }
  };

  const event = async ({ event }: { event: { type: string; properties?: unknown } }): Promise<void> => {
    const props = event.properties as Record<string, unknown> | undefined;

    if (event.type === "session.deleted") {
      const sessionID = resolveSessionID(props);
      if (sessionID) {
        snapshots.delete(sessionID);
      }
      return;
    }

    if (event.type === "session.compacted") {
      const sessionID = resolveSessionID(props);
      if (sessionID) {
        await restore(sessionID);
      }
    }
  };

  return { capture, event };
}
```

**Step 4: Export from hooks/index.ts**

Add to `packages/opencode-plugin/src/hooks/index.ts`:

```typescript
export {
  createCompactionTodoPreserverHook,
  type CompactionTodoPreserver,
} from "./compaction-todo-preserver";
```

**Step 5: Wire into index.ts**

Import `createCompactionTodoPreserverHook` from `"./hooks"`.

After `const compactionContextInjector = createCompactionContextInjector();`, add:

```typescript
const compactionTodoPreserver = createCompactionTodoPreserverHook(ctx);
```

Add to `event` handler:

```typescript
await compactionTodoPreserver.event(
  input as { event: { type: string; properties?: unknown } }
);
```

Update `experimental.session.compacting` to capture before context injection:

```typescript
"experimental.session.compacting": async (
  _input: { sessionID: string },
  output: { context: string[] },
): Promise<void> => {
  await compactionTodoPreserver.capture(_input.sessionID);
  output.context.push(compactionContextInjector());
},
```

**Step 6: Run tests, full suite, lint, typecheck**

Run: `bun test packages/opencode-plugin && bunx tsc --noEmit -p packages/opencode-plugin/tsconfig.json && bunx biome check packages/opencode-plugin/src/`

Expected: All pass

**Step 7: Commit**

```
feat(opencode-plugin): add compaction todo preserver

Captures todo state before compaction and restores after if lost.
Feature-detects todoUpdate API availability. Integrates with
experimental.session.compacting for capture and session.compacted
event for restore.
```

---

### Task 3: Wire Stop-Continuation + Todo Continuation Enforcer

These two items are tightly coupled (review finding: "mark as coupled, require end-to-end validation"). Implemented together with an integration test through the plugin entry point.

**Design note — stop-continuation mechanism:** The brainstorm says "create command template." However, plugins cannot register command templates in OpenCode — that's an OMO-internal mechanism. The correct plugin approach is slashcommand interception in `tool.execute.before`, which is how OMO wires it (`oh-my-opencode/src/plugin/tool-execute-before.ts:88`).

**Files:**
- Create: `packages/opencode-plugin/src/hooks/todo-continuation-enforcer.ts`
- Modify: `packages/opencode-plugin/src/hooks/index.ts`
- Modify: `packages/opencode-plugin/src/index.ts`
- Test: `packages/opencode-plugin/src/__tests__/integration.test.ts`

**Step 1: Write the failing tests**

Unit tests for the enforcer hook (direct import):

```typescript
describe("todo continuation enforcer", () => {
  it("injects continuation when session idles with incomplete todos", async () => {
    const sessionID = "session-continue";
    let capturedPrompt: { agent?: string; model?: unknown; pathId: string; directory: string } | undefined;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-continue-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({
            data: [
              { id: "1", content: "Task A", status: "in_progress", priority: "high" },
              { id: "2", content: "Task B", status: "pending", priority: "medium" },
            ],
          }),
          messages: async () => ({
            data: [{
              info: {
                role: "assistant",
                agent: "orchestrator",
                providerID: "anthropic",
                modelID: "claude-sonnet-4-20250514",
              },
            }],
          }),
          promptAsync: async ({ path: p, body, query }: {
            path: { id: string };
            body: { agent?: string; model?: unknown; parts?: unknown[] };
            query: { directory: string };
          }) => {
            capturedPrompt = {
              agent: body.agent,
              model: body.model,
              pathId: p.id,
              directory: query.directory,
            };
            return {};
          },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => false,
        isBackgroundSession: () => false,
        isRecovering: () => false,
        gracePeriodMs: 0,
      });

      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await new Promise((r) => setTimeout(r, 20));

      expect(capturedPrompt).toBeTruthy();
      expect(capturedPrompt!.agent).toBe("orchestrator");
      expect(capturedPrompt!.pathId).toBe(sessionID);
      expect(capturedPrompt!.directory).toBe(tempRoot);
      expect(capturedPrompt!.model).toEqual({ providerID: "anthropic", modelID: "claude-sonnet-4-20250514" });
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("skips when all todos are complete", async () => {
    const sessionID = "session-all-done";
    let promptInjected = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-all-done-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({ data: [{ id: "1", content: "Task A", status: "completed" }] }),
          messages: async () => ({ data: [] }),
          promptAsync: async () => { promptInjected = true; return {}; },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => false,
        isBackgroundSession: () => false,
        isRecovering: () => false,
        gracePeriodMs: 0,
      });

      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await new Promise((r) => setTimeout(r, 20));
      expect(promptInjected).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("skips when continuation is stopped", async () => {
    const sessionID = "session-stopped";
    let promptInjected = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-stopped-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({ data: [{ id: "1", content: "Task A", status: "pending" }] }),
          messages: async () => ({ data: [{ info: { role: "assistant", agent: "orchestrator" } }] }),
          promptAsync: async () => { promptInjected = true; return {}; },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => true,
        isBackgroundSession: () => false,
        isRecovering: () => false,
        gracePeriodMs: 0,
      });

      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await new Promise((r) => setTimeout(r, 20));
      expect(promptInjected).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("skips for non-continuation agents", async () => {
    const sessionID = "session-leaf";
    let promptInjected = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-leaf-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({ data: [{ id: "1", content: "Task A", status: "pending" }] }),
          messages: async () => ({ data: [{ info: { role: "assistant", agent: "explorer" } }] }),
          promptAsync: async () => { promptInjected = true; return {}; },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => false,
        isBackgroundSession: () => false,
        isRecovering: () => false,
        gracePeriodMs: 0,
      });

      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await new Promise((r) => setTimeout(r, 20));
      expect(promptInjected).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("fails closed when agent cannot be resolved", async () => {
    const sessionID = "session-no-agent";
    let promptInjected = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-no-agent-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({ data: [{ id: "1", content: "Task A", status: "pending" }] }),
          messages: async () => ({ data: [] }), // empty — can't resolve agent
          promptAsync: async () => { promptInjected = true; return {}; },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => false,
        isBackgroundSession: () => false,
        isRecovering: () => false,
        gracePeriodMs: 0,
      });

      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await new Promise((r) => setTimeout(r, 20));
      expect(promptInjected).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("skips for background sessions", async () => {
    const sessionID = "session-bg";
    let promptInjected = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-bg-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({ data: [{ id: "1", content: "Task A", status: "pending" }] }),
          messages: async () => ({ data: [{ info: { role: "assistant", agent: "orchestrator" } }] }),
          promptAsync: async () => { promptInjected = true; return {}; },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => false,
        isBackgroundSession: () => true,
        isRecovering: () => false,
        gracePeriodMs: 0,
      });

      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await new Promise((r) => setTimeout(r, 20));
      expect(promptInjected).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("skips during recovery", async () => {
    const sessionID = "session-recovering";
    let promptInjected = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-recovering-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({ data: [{ id: "1", content: "Task A", status: "pending" }] }),
          messages: async () => ({ data: [{ info: { role: "assistant", agent: "orchestrator" } }] }),
          promptAsync: async () => { promptInjected = true; return {}; },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => false,
        isBackgroundSession: () => false,
        isRecovering: () => true,
        gracePeriodMs: 0,
      });

      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await new Promise((r) => setTimeout(r, 20));
      expect(promptInjected).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("replaces timer on repeated idle (no duplicate injection)", async () => {
    const sessionID = "session-double-idle";
    let promptCount = 0;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-double-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({ data: [{ id: "1", content: "Task A", status: "pending" }] }),
          messages: async () => ({ data: [{ info: { role: "assistant", agent: "executor" } }] }),
          promptAsync: async () => { promptCount++; return {}; },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => false,
        isBackgroundSession: () => false,
        isRecovering: () => false,
        gracePeriodMs: 50,
      });

      // Fire idle twice rapidly
      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await new Promise((r) => setTimeout(r, 150));

      expect(promptCount).toBe(1);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("cancels pending continuation on user message", async () => {
    const sessionID = "session-cancel";
    let promptInjected = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-cancel-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({ data: [{ id: "1", content: "Task A", status: "pending" }] }),
          messages: async () => ({ data: [{ info: { role: "assistant", agent: "orchestrator" } }] }),
          promptAsync: async () => { promptInjected = true; return {}; },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => false,
        isBackgroundSession: () => false,
        isRecovering: () => false,
        gracePeriodMs: 100,
      });

      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await hook.chatMessage({ sessionID });
      await new Promise((r) => setTimeout(r, 200));
      expect(promptInjected).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("cleans up on session.deleted", async () => {
    const sessionID = "session-cleanup";
    let promptInjected = false;
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-cleanup-"));
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({ data: [{ id: "1", content: "Task A", status: "pending" }] }),
          messages: async () => ({ data: [{ info: { role: "assistant", agent: "orchestrator" } }] }),
          promptAsync: async () => { promptInjected = true; return {}; },
        },
      });

      const { createTodoContinuationEnforcerHook } = await import("../hooks/todo-continuation-enforcer");
      const hook = createTodoContinuationEnforcerHook(ctx, {
        isContinuationStopped: () => false,
        isBackgroundSession: () => false,
        isRecovering: () => false,
        gracePeriodMs: 100,
      });

      await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
      await hook.event({ event: { type: "session.deleted", properties: { info: { id: sessionID } } } });
      await new Promise((r) => setTimeout(r, 200));
      expect(promptInjected).toBe(false);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });
});
```

End-to-end integration test through plugin entry point (tests Task 3 + Task 4 together):

```typescript
describe("stop-continuation + continuation integration (e2e)", () => {
  it("stop prevents continuation; user message clears stop; idle then continues", async () => {
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-e2e-stop-"));
    let promptCount = 0;
    try {
      const ctx = createStubContext(tempRoot, {
        session: {
          todo: async () => ({
            data: [{ id: "1", content: "Task A", status: "pending", priority: "high" }],
          }),
          messages: async () => ({
            data: [{ info: { role: "assistant", agent: "orchestrator", providerID: "anthropic", modelID: "test" } }],
          }),
          promptAsync: async () => { promptCount++; return {}; },
        },
      });

      const hooks = await OpenCodeLegion(ctx);

      // 1. Fire /stop-continuation
      await hooks["tool.execute.before"]!(
        { tool: "slashcommand", sessionID: "ses1", callID: "1" },
        { args: { command: "stop-continuation" } },
      );

      // 2. Idle — should NOT continue (stopped)
      await hooks.event!({ event: { type: "session.idle", properties: { sessionID: "ses1" } } } as any);
      await new Promise((r) => setTimeout(r, 50));
      expect(promptCount).toBe(0);

      // 3. User message — clears stop state
      await hooks["chat.message"]!({ sessionID: "ses1" } as any);

      // 4. Idle again — should NOW continue
      await hooks.event!({ event: { type: "session.idle", properties: { sessionID: "ses1" } } } as any);
      await new Promise((r) => setTimeout(r, 50));
      expect(promptCount).toBe(1);
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/opencode-plugin/src/__tests__/integration.test.ts --test-name-pattern "todo continuation enforcer|stop-continuation"`

Expected: FAIL — module not found.

**Step 3: Write the continuation enforcer hook**

Create `packages/opencode-plugin/src/hooks/todo-continuation-enforcer.ts`:

```typescript
import type { PluginInput } from "@opencode-ai/plugin";

const CONTINUATION_AGENTS = new Set(["orchestrator", "executor", "builder", "conductor"]);
const SKIP_AGENTS = new Set(["compaction"]);
const DEFAULT_GRACE_PERIOD_MS = 2000;

const CONTINUATION_PROMPT =
  "Continue working on the next incomplete task. Pick up where you left off.";

interface TodoItem {
  id: string;
  content: string;
  status: string;
  priority?: string;
}

interface MessageInfo {
  role?: string;
  agent?: string;
  providerID?: string;
  modelID?: string;
  model?: { providerID?: string; modelID?: string };
}

interface ResolvedAgent {
  agent: string;
  model?: { providerID: string; modelID: string };
}

function extractTodos(response: unknown): TodoItem[] {
  const payload = response as { data?: unknown };
  if (Array.isArray(payload?.data)) return payload.data as TodoItem[];
  if (Array.isArray(response)) return response as TodoItem[];
  return [];
}

function getIncompleteCount(todos: TodoItem[]): number {
  return todos.filter(
    (t) => t.status !== "completed" && t.status !== "cancelled",
  ).length;
}

function resolveAgentFromMessages(
  messages: Array<{ info?: MessageInfo }>,
): ResolvedAgent | undefined {
  for (let i = messages.length - 1; i >= 0; i--) {
    const info = messages[i].info;
    if (!info) continue;
    if (info.agent === "compaction") continue;
    // Require agent name — fail closed
    if (info.agent) {
      const providerID = info.providerID ?? info.model?.providerID;
      const modelID = info.modelID ?? info.model?.modelID;
      return {
        agent: info.agent,
        model:
          providerID && modelID ? { providerID, modelID } : undefined,
      };
    }
  }
  return undefined;
}

export interface TodoContinuationEnforcerOptions {
  isContinuationStopped: (sessionID: string) => boolean;
  isBackgroundSession: (sessionID: string) => boolean;
  isRecovering: (sessionID: string) => boolean;
  gracePeriodMs?: number;
}

export interface TodoContinuationEnforcer {
  event: (input: { event: { type: string; properties?: unknown } }) => Promise<void>;
  chatMessage: (input: { sessionID?: string }) => Promise<void>;
}

export function createTodoContinuationEnforcerHook(
  ctx: PluginInput,
  options: TodoContinuationEnforcerOptions,
): TodoContinuationEnforcer {
  const pendingTimers = new Map<string, ReturnType<typeof setTimeout>>();
  const gracePeriodMs = options.gracePeriodMs ?? DEFAULT_GRACE_PERIOD_MS;

  const cancelPending = (sessionID: string): void => {
    const existing = pendingTimers.get(sessionID);
    if (existing) {
      clearTimeout(existing);
      pendingTimers.delete(sessionID);
    }
  };

  const handleIdle = async (sessionID: string): Promise<void> => {
    if (options.isContinuationStopped(sessionID)) return;
    if (options.isBackgroundSession(sessionID)) return;
    if (options.isRecovering(sessionID)) return;

    let todos: TodoItem[];
    try {
      const response = await ctx.client.session.todo({ path: { id: sessionID } });
      todos = extractTodos(response);
    } catch {
      return;
    }

    if (todos.length === 0 || getIncompleteCount(todos) === 0) return;

    let resolved: ResolvedAgent | undefined;
    try {
      const messagesResp = await ctx.client.session.messages({
        path: { id: sessionID },
        query: { directory: ctx.directory },
      });
      const messages = ((messagesResp as { data?: unknown[] }).data ?? []) as Array<{
        info?: MessageInfo;
      }>;
      resolved = resolveAgentFromMessages(messages);
    } catch {
      return;
    }

    // Fail closed: no resolved agent → no continuation
    if (!resolved) return;
    if (SKIP_AGENTS.has(resolved.agent)) return;
    if (!CONTINUATION_AGENTS.has(resolved.agent)) return;

    // Cancel existing timer for this session (prevent duplicates)
    cancelPending(sessionID);

    const timer = setTimeout(async () => {
      pendingTimers.delete(sessionID);

      try {
        const freshResp = await ctx.client.session.todo({ path: { id: sessionID } });
        const freshTodos = extractTodos(freshResp);
        if (getIncompleteCount(freshTodos) === 0) return;
      } catch {
        return;
      }

      try {
        await ctx.client.session.promptAsync({
          path: { id: sessionID },
          body: {
            agent: resolved.agent,
            ...(resolved.model ? { model: resolved.model } : {}),
            parts: [{ type: "text" as const, text: CONTINUATION_PROMPT }],
          },
          query: { directory: ctx.directory },
        });
      } catch {
        // best-effort
      }
    }, gracePeriodMs);

    pendingTimers.set(sessionID, timer);
  };

  const event = async ({
    event,
  }: {
    event: { type: string; properties?: unknown };
  }): Promise<void> => {
    const props = event.properties as Record<string, unknown> | undefined;

    if (event.type === "session.idle") {
      const sessionID = props?.sessionID as string | undefined;
      if (sessionID) {
        await handleIdle(sessionID);
      }
      return;
    }

    if (event.type === "session.deleted") {
      const sessionInfo = props?.info as { id?: string } | undefined;
      if (sessionInfo?.id) {
        cancelPending(sessionInfo.id);
      }
    }
  };

  const chatMessage = async ({ sessionID }: { sessionID?: string }): Promise<void> => {
    if (sessionID) {
      cancelPending(sessionID);
    }
  };

  return { event, chatMessage };
}
```

**Step 4: Export from hooks/index.ts**

Add to `packages/opencode-plugin/src/hooks/index.ts`:

```typescript
export {
  createTodoContinuationEnforcerHook,
  type TodoContinuationEnforcer,
  type TodoContinuationEnforcerOptions,
} from "./todo-continuation-enforcer";
```

**Step 5: Wire into index.ts**

Import `createTodoContinuationEnforcerHook` from `"./hooks"`.

After `const compactionTodoPreserver = ...`, add:

```typescript
const todoContinuationEnforcer = createTodoContinuationEnforcerHook(ctx, {
  isContinuationStopped: (sessionID) => stopContinuationGuardHook.isStopped(sessionID),
  isBackgroundSession: (sessionID) => manager.isBackgroundSession(sessionID),
  isRecovering: (sessionID) => sessionRecoveryHook.isRecovering?.(sessionID) ?? false,
});
```

**Note on isRecovering:** Check if `sessionRecoveryHook` exposes `isRecovering`. If not, add a `recoveringSessions` Set tracked during session.error → recovery → completion. The implementer should check the session-recovery hook interface and adapt accordingly. The key contract: `isRecovering(sessionID)` returns true while a recovery is in-flight for that session.

In the `event` handler, add:

```typescript
await todoContinuationEnforcer.event(
  input as { event: { type: string; properties?: unknown } }
);
```

In `"chat.message"`, add:

```typescript
await todoContinuationEnforcer.chatMessage(input as { sessionID?: string });
```

In `"tool.execute.before"`, add slashcommand detection after `subagentQuestionBlockerHook`:

```typescript
const toolInput = input as { tool: string; sessionID?: string };
if (toolInput.tool === "slashcommand") {
  const args = (output as { args?: { command?: string } }).args;
  const command = args?.command?.replace(/^\//, "").toLowerCase();
  if (command === "stop-continuation" && toolInput.sessionID) {
    stopContinuationGuardHook.stop(toolInput.sessionID);
  }
}
```

**Step 6: Run tests**

Run: `bun test packages/opencode-plugin/src/__tests__/integration.test.ts --test-name-pattern "todo continuation enforcer|stop-continuation"`

Expected: PASS

**Step 7: Full suite + lint + typecheck**

Run: `bun test packages/opencode-plugin && bunx tsc --noEmit -p packages/opencode-plugin/tsconfig.json && bunx biome check packages/opencode-plugin/src/`

Expected: All pass

**Step 8: Commit**

```
feat(opencode-plugin): add todo continuation enforcer + wire stop-continuation

Auto-continue when session idles with incomplete todos. Fails closed
(requires resolved continuation-eligible agent). Cancels existing
timer on repeated idle. Skips background sessions and recovery.
Wires /stop-continuation to guard.stop() via slashcommand detection.
```

---

### Task 4: Conductor Agent (Delegate Mode)

New first-class agent. Reads, searches, plans, delegates. Cannot edit/write/bash.

**Files:**
- Create: `packages/opencode-plugin/src/agents/conductor.ts`
- Modify: `packages/opencode-plugin/src/agents/index.ts`
- Modify: `packages/opencode-plugin/src/index.ts`
- Test: `packages/opencode-plugin/src/__tests__/integration.test.ts`

**Step 1: Write the failing tests**

Update existing tests: `toHaveLength(8)` → `toHaveLength(9)`, add `"conductor"` to expected names.

```typescript
describe("conductor agent", () => {
  it("is included in agent list", () => {
    const agents = createAgents();
    expect(agents.map((a) => a.name)).toContain("conductor");
  });

  it("has delegation-only permissions via plugin config", async () => {
    const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-conductor-"));
    try {
      const ctx = createStubContext(tempRoot);
      const hooks = await OpenCodeLegion(ctx);
      const config: Record<string, unknown> = {};
      await hooks.config?.(config);

      const agentMap = config.agent as Record<string, { permission?: Record<string, string> }>;
      const conductor = agentMap?.conductor;
      expect(conductor).toBeTruthy();
      expect(conductor.permission?.edit).toBe("deny");
      expect(conductor.permission?.write).toBe("deny");
      expect(conductor.permission?.bash).toBe("deny");
      expect(conductor.permission?.task).toBe("allow");
      expect(conductor.permission?.read).toBe("allow");
      expect(conductor.permission?.glob).toBe("allow");
      expect(conductor.permission?.list).toBe("allow");
    } finally {
      fs.rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  it("prompt enforces delegation-only constraints", () => {
    const agents = createAgents();
    const conductor = agents.find((a) => a.name === "conductor");
    expect(conductor).toBeTruthy();
    expect(conductor!.config.prompt).toContain("MUST NOT");
    expect(conductor!.config.prompt).toContain("background_task");
    expect(conductor!.config.prompt).not.toMatch(/claude|anthropic|gpt|openai|gemini|google/i);
  });

  it("prompt is within token budget (<3000 tokens)", () => {
    const agents = createAgents();
    const conductor = agents.find((a) => a.name === "conductor");
    expect(conductor!.config.prompt.length / 4).toBeLessThan(3000);
  });
});
```

**Step 2: Run tests → FAIL**

**Step 3: Write the agent**

Create `packages/opencode-plugin/src/agents/conductor.ts`:

```typescript
import type { AgentDefinition } from "./types";

const CONDUCTOR_PROMPT = `<identity>
You are a conductor — an orchestrator that works exclusively through delegation. You coordinate specialist agents to accomplish complex tasks but never modify code or files directly.
</identity>

<constraint>
You MUST NOT modify code, files, or run shell commands directly. Your only mechanism for making changes is delegation via background_task. If you find yourself wanting to edit a file, STOP and delegate instead.

Allowed direct actions:
- Reading files (to understand code, review changes)
- Searching codebase (grep, glob, AST-grep, LSP)
- Analyzing and reasoning about code
- Planning and breaking down work
- Communicating with the user
- Managing todos

Forbidden direct actions:
- Editing files
- Writing new files
- Running bash/shell commands
- Any action that modifies the codebase
</constraint>

<agents>

@executor
- Implements well-defined tasks with clear specs
- Delegate when: you know exactly what to change and how

@explorer
- Fast codebase search: grep, glob, AST-grep
- Delegate when: need parallel searches or broad discovery

@librarian
- External documentation and API research
- Delegate when: need library docs or version-specific behavior

@oracle
- Strategic decisions and architecture review
- Delegate when: high-stakes decisions or persistent bugs

@metis
- Gap analysis and spec validation
- Delegate when: reviewing plans or checking completeness

@momus
- Critical review and quality assessment
- Delegate when: need honest critique or quality review

@multimodal
- PDF/image analysis
- Delegate when: visual content interpretation needed

</agents>

<workflow>

## 1. Understand
Parse the request. Identify what needs to change and why.

## 2. Investigate
Read relevant files and search the codebase to build context.
Use parallel delegation to @explorer for broad discovery.

## 3. Plan
Break the work into delegatable units. Each unit should be:
- Self-contained with clear inputs and outputs
- Small enough for one specialist to complete
- Independent where possible (enables parallelism)

## 4. Delegate
For each unit:
- Choose the right specialist
- Provide precise context: file paths, line numbers, patterns to follow
- State the expected outcome clearly
- Fire independent tasks in parallel

## 5. Verify
After delegation completes:
- Read modified files to verify changes
- Run lsp_diagnostics on changed files
- Check results against requirements
- If issues found, delegate fixes (do NOT fix directly)

## 6. Report
Summarize what was done and any issues found.

</workflow>

<communication>
- Brief delegation notices: "Sending to @executor..." not lengthy explanations
- Report results concisely after each delegation round
- Flag concerns early rather than discovering them late
</communication>`;

export function createConductorAgent(model: string): AgentDefinition {
  return {
    name: "conductor",
    description:
      "Delegation-only orchestrator. Coordinates work exclusively through specialist agents. " +
      "Cannot modify code directly — reads, searches, plans, and delegates. " +
      "Use when you want all changes made through delegation.",
    config: {
      model,
      temperature: 0.7,
      prompt: CONDUCTOR_PROMPT,
    },
  };
}
```

**Step 4: Wire into agents/index.ts**

Import: `import { createConductorAgent } from "./conductor";`

Add to `DEFAULT_MODELS`: `conductor: "anthropic/claude-sonnet-4-20250514",`

Add to `agents` array: `createConductorAgent(getModel(config, "conductor")),`

**Step 5: Add permission in config hook (index.ts)**

After the `Object.assign(opencodeConfig.agent, agentMap)` block:

```typescript
// Conductor: delegation-only permissions
const conductorEntry = (opencodeConfig.agent as Record<string, Record<string, unknown>>)?.conductor;
if (conductorEntry) {
  conductorEntry.permission = {
    read: "allow",
    glob: "allow",
    list: "allow",
    edit: "deny",
    write: "deny",
    bash: "deny",
    task: "allow",
  };
}
```

**Step 6: Run tests → PASS**

**Step 7: Full suite + lint + typecheck → All pass**

**Step 8: Commit**

```
feat(opencode-plugin): add conductor agent (delegate mode)

Delegation-only orchestrator. Edit, write, bash denied at platform
level. Temperature 0.7 for flexible coordination. Eligible for
auto-continuation.
```

---

## Verification Checklist

```bash
bun test packages/opencode-plugin
# exit code 0, 0 failures

bunx tsc --noEmit -p packages/opencode-plugin/tsconfig.json
# exit code 0

bunx biome check packages/opencode-plugin/src/
# exit code 0
```

Verify:
1. Agent count: 9 (8 original + conductor)
2. New hooks: `compaction-context-injector`, `compaction-todo-preserver`, `todo-continuation-enforcer`
3. `experimental.session.compacting` calls todo capture then context injection
4. `/stop-continuation` detected in `tool.execute.before`
5. `session.idle` triggers continuation for `orchestrator`, `executor`, `builder`, `conductor` only
6. Continuation fails closed (no agent → no injection)
7. Duplicate idle cancels old timer (single injection)
8. Background sessions and recovering sessions skip continuation
9. End-to-end: stop → idle → no continuation → user message → idle → continuation
10. Conductor permissions: edit=deny, write=deny, bash=deny, task=allow
11. todoUpdate feature-detected, not assumed
