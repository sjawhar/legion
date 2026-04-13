# Plan: Spawn Depth and Descendant Limits (Issue #271)

**Feature**: Add spawn depth tracking and descendant limits to `BackgroundTaskManager`  
**Issue**: #271 — OMO Replacement: Spawn Depth and Descendant Limits (T2)  
**Commit message**: `feat(legion): add spawn depth and descendant limits`

## Context

`BackgroundTaskManager` in `packages/opencode-plugin/src/delegation/background-manager.ts` currently has no limits on how deeply agents can nest (spawn children that spawn children). This allows infinite recursion. We need:
- Max spawn depth (default 5): depth=0 is root, depth=4 is the deepest allowed child
- Max descendants per root (default 20): all transitive descendants of a root task

**Key design decisions** (from Metis analysis + architect handoff):
- In-memory Maps on the manager instance for depth/descendant counters
- `depth` and `rootSessionID` stored on `BackgroundTask` for persistence/rehydration
- Synchronous check+increment before any `await` to prevent race window
- Orphan tasks (unknown `parentSessionID`) treated as roots (depth=0)
- `launch()` throws `Error` on rejection (consistent with existing error handling)
- Config via new `spawnLimits` sub-object in `ConcurrencyConfigSchema`

**Critical pattern from `delegation-hardening-retro.md`**: Always assert accounting state after every rejection path, not just the rejection itself. The previous concurrency bugs were invisible because tests only checked the rejection, not the counter state afterward.

## File Map

| File | Change |
|------|--------|
| `packages/opencode-plugin/src/delegation/types.ts` | Add `depth?: number`, `rootSessionID?: string` to `BackgroundTask` |
| `packages/opencode-plugin/src/config/index.ts` | Add `SpawnLimitsConfigSchema`, `SpawnLimitsConfig`, update `PluginConfig`, `DEFAULT_CONFIG`, `applyDefaults`, `mergeConfig` |
| `packages/opencode-plugin/src/delegation/background-manager.ts` | Add spawn limit tracking, validation in `launch()`, rehydration support |
| `packages/opencode-plugin/src/index.ts` | Pass `spawnLimits` config to `BackgroundTaskManager` constructor |
| `packages/opencode-plugin/src/delegation/__tests__/spawn-limits.test.ts` | New test file: all unit + integration tests for spawn limits |
| `packages/opencode-plugin/src/config/__tests__/spawn-limits-config.test.ts` | New test file: config schema tests for spawnLimits |

---

## Task 1: Add `depth` and `rootSessionID` to `BackgroundTask` type

**Files:**
- Modify: `packages/opencode-plugin/src/delegation/types.ts`

- [ ] **Step 1: Write the failing test** (in a new file we'll create)

  We'll write the test in Task 2. This task is just the type change — no test needed for a type addition.

- [ ] **Step 2: Add fields to `BackgroundTask`**

  In `packages/opencode-plugin/src/delegation/types.ts`, add after `parentSessionID?: string;`:

  ```typescript
  /** Spawn depth: 0 for root tasks, parent.depth + 1 for children. */
  depth?: number;
  /** Session ID of the root ancestor task. Same as sessionID for root tasks. */
  rootSessionID?: string;
  ```

- [ ] **Step 3: Verify TypeScript compiles**

  Run: `bunx tsc --noEmit`
  Expected: No errors

- [ ] **Step 4: Commit**

  ```bash
  jj describe -m "feat(delegation): add depth and rootSessionID fields to BackgroundTask"
  jj new
  ```

---

## Task 2: Add `spawnLimits` to config schema

**Files:**
- Modify: `packages/opencode-plugin/src/config/index.ts`

- [ ] **Step 1: Write the failing test**

  Create `packages/opencode-plugin/src/config/__tests__/spawn-limits-config.test.ts`:

  ```typescript
  import { describe, expect, it } from "bun:test";
  import { loadPluginConfig } from "../index";
  import fs from "node:fs";
  import os from "node:os";
  import path from "node:path";

  describe("spawnLimits config", () => {
    it("applies default spawnLimits when not configured", async () => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "config-test-"));
      try {
        const config = await loadPluginConfig(dir);
        expect(config.spawnLimits?.maxDepth).toBe(5);
        expect(config.spawnLimits?.maxDescendants).toBe(20);
      } finally {
        fs.rmSync(dir, { recursive: true, force: true });
      }
    });

    it("respects custom spawnLimits from config file", async () => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "config-test-"));
      try {
        fs.mkdirSync(path.join(dir, ".opencode"), { recursive: true });
        fs.writeFileSync(
          path.join(dir, ".opencode", "opencode-legion.json"),
          JSON.stringify({ spawnLimits: { maxDepth: 3, maxDescendants: 10 } })
        );
        const config = await loadPluginConfig(dir);
        expect(config.spawnLimits?.maxDepth).toBe(3);
        expect(config.spawnLimits?.maxDescendants).toBe(10);
      } finally {
        fs.rmSync(dir, { recursive: true, force: true });
      }
    });

    it("merges partial spawnLimits override (only maxDepth)", async () => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "config-test-"));
      try {
        fs.mkdirSync(path.join(dir, ".opencode"), { recursive: true });
        fs.writeFileSync(
          path.join(dir, ".opencode", "opencode-legion.json"),
          JSON.stringify({ spawnLimits: { maxDepth: 2 } })
        );
        const config = await loadPluginConfig(dir);
        expect(config.spawnLimits?.maxDepth).toBe(2);
        expect(config.spawnLimits?.maxDescendants).toBe(20); // default preserved
      } finally {
        fs.rmSync(dir, { recursive: true, force: true });
      }
    });
  });
  ```

- [ ] **Step 2: Run test to verify it fails**

  Run: `bun test packages/opencode-plugin/src/config/__tests__/spawn-limits-config.test.ts`
  Expected: FAIL — `config.spawnLimits` is undefined

- [ ] **Step 3: Add `SpawnLimitsConfigSchema` to config**

  In `packages/opencode-plugin/src/config/index.ts`:

  After `ConcurrencyConfigSchema`, add:
  ```typescript
  const SpawnLimitsConfigSchema = z
    .object({
      maxDepth: z.number().optional(),
      maxDescendants: z.number().optional(),
    })
    .strict();
  ```

  In `PluginConfigSchema`, add inside the `.object({...})`:
  ```typescript
  spawnLimits: SpawnLimitsConfigSchema.optional(),
  ```

  Add `SpawnLimitsConfig` interface after `ConcurrencyConfig`:
  ```typescript
  export interface SpawnLimitsConfig {
    maxDepth?: number;
    maxDescendants?: number;
  }
  ```

  Add `spawnLimits?: SpawnLimitsConfig;` to `PluginConfig` interface.

  In `DEFAULT_CONFIG`, add:
  ```typescript
  spawnLimits: {
    maxDepth: 5,
    maxDescendants: 20,
  },
  ```

  Add `mergeSpawnLimits` function after `mergeConcurrency`:
  ```typescript
  function mergeSpawnLimits(
    base?: SpawnLimitsConfig,
    override?: SpawnLimitsConfig
  ): SpawnLimitsConfig | undefined {
    if (!base) return override;
    if (!override) return base;
    return { ...base, ...override };
  }
  ```

  In `mergeConfig`, add:
  ```typescript
  spawnLimits: mergeSpawnLimits(base.spawnLimits, override.spawnLimits),
  ```

  In `applyDefaults`, add:
  ```typescript
  spawnLimits: {
    maxDepth: config.spawnLimits?.maxDepth ?? DEFAULT_CONFIG.spawnLimits?.maxDepth,
    maxDescendants: config.spawnLimits?.maxDescendants ?? DEFAULT_CONFIG.spawnLimits?.maxDescendants,
  },
  ```

- [ ] **Step 4: Run test to verify it passes**

  Run: `bun test packages/opencode-plugin/src/config/__tests__/spawn-limits-config.test.ts`
  Expected: PASS (3 tests)

- [ ] **Step 5: Verify TypeScript compiles**

  Run: `bunx tsc --noEmit`
  Expected: No errors

- [ ] **Step 6: Commit**

  ```bash
  jj describe -m "feat(config): add spawnLimits config schema with maxDepth and maxDescendants"
  jj new
  ```

---

## Task 3: Write failing spawn-limits tests (TDD — tests first)

**Files:**
- Create: `packages/opencode-plugin/src/delegation/__tests__/spawn-limits.test.ts`

This is the TDD step: write ALL tests before any implementation. Tests will fail until Task 4.

- [ ] **Step 1: Create the test file**

  Create `packages/opencode-plugin/src/delegation/__tests__/spawn-limits.test.ts`:

  ```typescript
  import { afterEach, beforeEach, describe, expect, it } from "bun:test";
  import fs from "node:fs";
  import os from "node:os";
  import path from "node:path";
  import type { PluginInput } from "@opencode-ai/plugin";
  import { BackgroundTaskManager } from "../background-manager";
  import type { BackgroundTask } from "../types";

  let workspace: string;

  function createManager(
    overrides: Partial<{
      create: () => Promise<{ data?: { id?: string } }>;
      promptAsync: () => Promise<void>;
      messages: () => Promise<{ data?: unknown[] }>;
      abort: () => Promise<void>;
    }> = {},
    spawnLimits?: { maxDepth?: number; maxDescendants?: number }
  ): BackgroundTaskManager {
    let sessionCounter = 0;
    const session = {
      create: async () => ({ data: { id: `session-${++sessionCounter}` } }),
      promptAsync: async () => {},
      messages: async () => ({ data: [] }),
      abort: async () => {},
      ...overrides,
    };
    const client = { session };
    return new BackgroundTaskManager(
      { client, directory: workspace } as unknown as PluginInput,
      spawnLimits
    );
  }

  beforeEach(() => {
    workspace = fs.mkdtempSync(path.join(os.tmpdir(), "spawn-limits-test-"));
  });

  afterEach(() => {
    fs.rmSync(workspace, { recursive: true, force: true });
  });

  describe("depth tracking", () => {
    it("root task has depth=0", async () => {
      const manager = createManager();
      const task = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root task",
      });
      expect(task.depth).toBe(0);
    });

    it("child task has depth=1", async () => {
      const manager = createManager();
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      const child = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "child",
        parentSessionId: root.sessionID,
      });
      expect(child.depth).toBe(1);
    });

    it("grandchild task has depth=2", async () => {
      const manager = createManager();
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      const child = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "child",
        parentSessionId: root.sessionID,
      });
      const grandchild = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "grandchild",
        parentSessionId: child.sessionID,
      });
      expect(grandchild.depth).toBe(2);
    });

    it("orphan task (unknown parentSessionID) treated as root (depth=0)", async () => {
      const manager = createManager();
      const orphan = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "orphan",
        parentSessionId: "unknown-session-id",
      });
      expect(orphan.depth).toBe(0);
    });
  });

  describe("depth boundary", () => {
    it("spawning at depth=4 (default max=5) succeeds", async () => {
      const manager = createManager();
      let current = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "depth-0",
      });
      for (let i = 1; i <= 4; i++) {
        current = await manager.launch({
          agent: "explore",
          prompt: "test",
          description: `depth-${i}`,
          parentSessionId: current.sessionID,
        });
      }
      expect(current.depth).toBe(4);
    });

    it("spawning at depth=5 (default max=5) throws rejection error", async () => {
      const manager = createManager();
      let current = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "depth-0",
      });
      for (let i = 1; i <= 4; i++) {
        current = await manager.launch({
          agent: "explore",
          prompt: "test",
          description: `depth-${i}`,
          parentSessionId: current.sessionID,
        });
      }
      // current is at depth=4, trying to spawn depth=5 should fail
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "depth-5-rejected",
          parentSessionId: current.sessionID,
        })
      ).rejects.toThrow("Spawn rejected: max depth 5 reached (current depth: 5)");
    });

    it("depth counters unchanged after rejection — subsequent valid spawn still works", async () => {
      const manager = createManager({}, { maxDepth: 2 });
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "depth-0",
      });
      const child = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "depth-1",
        parentSessionId: root.sessionID,
      });
      const grandchild = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "depth-2",
        parentSessionId: child.sessionID,
      });

      // depth=3 should fail (maxDepth=2 means max allowed depth index is 1)
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "rejected",
          parentSessionId: grandchild.sessionID,
        })
      ).rejects.toThrow("Spawn rejected: max depth 2 reached (current depth: 3)");

      // After rejection, a sibling of grandchild (depth=2) should still fail too
      // (counters not corrupted — limit still enforced correctly)
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "also-rejected",
          parentSessionId: grandchild.sessionID,
        })
      ).rejects.toThrow("Spawn rejected: max depth 2 reached (current depth: 3)");
    });
  });

  describe("descendant boundary", () => {
    it("20th descendant of a root succeeds", async () => {
      const manager = createManager();
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      let last: BackgroundTask = root;
      // Spawn 20 descendants (all as direct children of root for simplicity)
      for (let i = 1; i <= 20; i++) {
        last = await manager.launch({
          agent: "explore",
          prompt: "test",
          description: `descendant-${i}`,
          parentSessionId: root.sessionID,
        });
      }
      expect(last.depth).toBe(1);
    });

    it("21st descendant of a root throws rejection error", async () => {
      const manager = createManager();
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      for (let i = 1; i <= 20; i++) {
        await manager.launch({
          agent: "explore",
          prompt: "test",
          description: `descendant-${i}`,
          parentSessionId: root.sessionID,
        });
      }
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "descendant-21-rejected",
          parentSessionId: root.sessionID,
        })
      ).rejects.toThrow(`Spawn rejected: max descendants 20 reached for root session ${root.sessionID}`);
    });

    it("descendant counters unchanged after rejection — limit still enforced on retry", async () => {
      const manager = createManager({}, { maxDescendants: 2 });
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "child-1",
        parentSessionId: root.sessionID,
      });
      await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "child-2",
        parentSessionId: root.sessionID,
      });

      // 3rd should fail
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "child-3",
          parentSessionId: root.sessionID,
        })
      ).rejects.toThrow(`Spawn rejected: max descendants 2 reached for root session ${root.sessionID}`);

      // 4th should also fail (counter not corrupted by first rejection)
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "child-4",
          parentSessionId: root.sessionID,
        })
      ).rejects.toThrow(`Spawn rejected: max descendants 2 reached for root session ${root.sessionID}`);
    });
  });

  describe("config overrides", () => {
    it("custom maxDepth=3 rejects at 4th level", async () => {
      const manager = createManager({}, { maxDepth: 3 });
      let current = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "depth-0",
      });
      for (let i = 1; i <= 3; i++) {
        current = await manager.launch({
          agent: "explore",
          prompt: "test",
          description: `depth-${i}`,
          parentSessionId: current.sessionID,
        });
      }
      // current is at depth=3, trying depth=4 should fail with custom limit
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "depth-4-rejected",
          parentSessionId: current.sessionID,
        })
      ).rejects.toThrow("Spawn rejected: max depth 3 reached (current depth: 4)");
    });

    it("custom maxDescendants=10 rejects at 11th descendant", async () => {
      const manager = createManager({}, { maxDescendants: 10 });
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      for (let i = 1; i <= 10; i++) {
        await manager.launch({
          agent: "explore",
          prompt: "test",
          description: `descendant-${i}`,
          parentSessionId: root.sessionID,
        });
      }
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "descendant-11-rejected",
          parentSessionId: root.sessionID,
        })
      ).rejects.toThrow(`Spawn rejected: max descendants 10 reached for root session ${root.sessionID}`);
    });
  });

  describe("root resolution", () => {
    it("deeply nested task correctly identifies its root ancestor", async () => {
      const manager = createManager();
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      let current = root;
      for (let i = 1; i <= 3; i++) {
        current = await manager.launch({
          agent: "explore",
          prompt: "test",
          description: `depth-${i}`,
          parentSessionId: current.sessionID,
        });
      }
      expect(current.rootSessionID).toBe(root.sessionID);
    });

    it("root task has rootSessionID equal to its own sessionID", async () => {
      const manager = createManager();
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      expect(root.rootSessionID).toBe(root.sessionID);
    });
  });

  describe("concurrent spawns", () => {
    it("two simultaneous spawns at limit — exactly one succeeds, one fails", async () => {
      const manager = createManager({}, { maxDescendants: 1 });
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });

      // Launch two children simultaneously — only one should succeed
      const results = await Promise.allSettled([
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "child-1",
          parentSessionId: root.sessionID,
        }),
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "child-2",
          parentSessionId: root.sessionID,
        }),
      ]);

      const fulfilled = results.filter((r) => r.status === "fulfilled");
      const rejected = results.filter((r) => r.status === "rejected");
      expect(fulfilled).toHaveLength(1);
      expect(rejected).toHaveLength(1);

      // After the race, the limit is still enforced (counter not double-incremented)
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "child-3",
          parentSessionId: root.sessionID,
        })
      ).rejects.toThrow(`Spawn rejected: max descendants 1 reached for root session ${root.sessionID}`);
    });
  });

  describe("error message format", () => {
    it("depth rejection includes limit type and current value", async () => {
      const manager = createManager({}, { maxDepth: 1 });
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      const child = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "child",
        parentSessionId: root.sessionID,
      });
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "grandchild",
          parentSessionId: child.sessionID,
        })
      ).rejects.toThrow("Spawn rejected: max depth 1 reached (current depth: 2)");
    });

    it("descendant rejection includes limit type and root session ID", async () => {
      const manager = createManager({}, { maxDescendants: 1 });
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });
      await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "child-1",
        parentSessionId: root.sessionID,
      });
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "child-2",
          parentSessionId: root.sessionID,
        })
      ).rejects.toThrow(`Spawn rejected: max descendants 1 reached for root session ${root.sessionID}`);
    });
  });

  describe("integration: spawn chain A→B→C→D→E→F", () => {
    it("E succeeds (depth=4), F is rejected (depth=5)", async () => {
      const manager = createManager(); // default maxDepth=5
      const sessions: BackgroundTask[] = [];
      let current = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "A",
      });
      sessions.push(current);

      for (const label of ["B", "C", "D", "E"]) {
        current = await manager.launch({
          agent: "explore",
          prompt: "test",
          description: label,
          parentSessionId: current.sessionID,
        });
        sessions.push(current);
      }

      expect(sessions[4].depth).toBe(4); // E is at depth 4
      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "F",
          parentSessionId: current.sessionID,
        })
      ).rejects.toThrow("Spawn rejected: max depth 5 reached (current depth: 5)");
    });
  });

  describe("integration: descendant flood", () => {
    it("20th descendant succeeds, 21st is rejected", async () => {
      const manager = createManager(); // default maxDescendants=20
      const root = await manager.launch({
        agent: "explore",
        prompt: "test",
        description: "root",
      });

      for (let i = 1; i <= 20; i++) {
        await manager.launch({
          agent: "explore",
          prompt: "test",
          description: `task-${i}`,
          parentSessionId: root.sessionID,
        });
      }

      await expect(
        manager.launch({
          agent: "explore",
          prompt: "test",
          description: "task-21",
          parentSessionId: root.sessionID,
        })
      ).rejects.toThrow(`Spawn rejected: max descendants 20 reached for root session ${root.sessionID}`);
    });
  });
  ```

- [ ] **Step 2: Run tests to verify they all fail**

  Run: `bun test packages/opencode-plugin/src/delegation/__tests__/spawn-limits.test.ts`
  Expected: All tests FAIL (BackgroundTaskManager constructor doesn't accept spawnLimits yet)

- [ ] **Step 3: Commit the failing tests**

  ```bash
  jj describe -m "test(delegation): add failing spawn limits tests (TDD)"
  jj new
  ```

---

## Task 4: Implement spawn depth and descendant limits in `BackgroundTaskManager`

**Files:**
- Modify: `packages/opencode-plugin/src/delegation/background-manager.ts`
- Modify: `packages/opencode-plugin/src/index.ts`

- [ ] **Step 1: Add spawn limit state and constructor parameter**

  In `packages/opencode-plugin/src/delegation/background-manager.ts`:

  Add import at top:
  ```typescript
  import type { SpawnLimitsConfig } from "../config/index";
  ```

  Add private fields to `BackgroundTaskManager` class (after existing private fields):
  ```typescript
  private taskDepths = new Map<string, number>();
  private rootDescendantCounts = new Map<string, number>();
  private spawnLimits: Required<SpawnLimitsConfig>;
  ```

  Update constructor signature and body:
  ```typescript
  constructor(ctx: PluginInput, spawnLimits?: SpawnLimitsConfig) {
    this.client = ctx.client;
    this.directory = ctx.directory;
    this.spawnLimits = {
      maxDepth: spawnLimits?.maxDepth ?? 5,
      maxDescendants: spawnLimits?.maxDescendants ?? 20,
    };
  }
  ```

- [ ] **Step 2: Add `resolveSpawnContext` private method**

  This method computes depth and rootSessionID for a new task, given its parentSessionId.
  It must be synchronous (no awaits) to prevent race windows.

  Add after the constructor:
  ```typescript
  /**
   * Resolve spawn context for a new task.
   * Returns { depth, rootSessionID } based on parent's tracked depth.
   * Orphan tasks (unknown parent) are treated as roots (depth=0).
   * MUST be called synchronously before any await to prevent race conditions.
   */
  private resolveSpawnContext(parentSessionId?: string): { depth: number; rootSessionID: string | undefined } {
    if (!parentSessionId) {
      return { depth: 0, rootSessionID: undefined };
    }
    const parentDepth = this.taskDepths.get(parentSessionId);
    if (parentDepth === undefined) {
      // Orphan: parent not tracked (unknown or from different manager instance)
      return { depth: 0, rootSessionID: undefined };
    }
    // Find root: walk up via rootSessionID stored on parent's task
    const parentTaskId = this.tasksBySessionId.get(parentSessionId);
    const parentTask = parentTaskId ? this.tasks.get(parentTaskId) : undefined;
    const rootSessionID = parentTask?.rootSessionID ?? parentSessionId;
    return { depth: parentDepth + 1, rootSessionID };
  }
  ```

- [ ] **Step 3: Add `validateAndReserveSpawn` private method**

  This method performs the check-and-increment atomically (synchronously, before any await):
  ```typescript
  /**
   * Validate spawn limits and atomically reserve slots.
   * Throws if limits exceeded. Must be called synchronously before any await.
   * Returns a rollback function to call if the spawn subsequently fails.
   */
  private validateAndReserveSpawn(
    depth: number,
    rootSessionID: string | undefined
  ): () => void {
    // Check depth limit
    if (depth >= this.spawnLimits.maxDepth) {
      throw new Error(
        `Spawn rejected: max depth ${this.spawnLimits.maxDepth} reached (current depth: ${depth})`
      );
    }

    // Check descendant limit (only for non-root tasks with a known root)
    if (rootSessionID !== undefined) {
      const currentCount = this.rootDescendantCounts.get(rootSessionID) ?? 0;
      if (currentCount >= this.spawnLimits.maxDescendants) {
        throw new Error(
          `Spawn rejected: max descendants ${this.spawnLimits.maxDescendants} reached for root session ${rootSessionID}`
        );
      }
      // Reserve the slot atomically
      this.rootDescendantCounts.set(rootSessionID, currentCount + 1);
      return () => {
        // Rollback: decrement if spawn fails
        const count = this.rootDescendantCounts.get(rootSessionID) ?? 0;
        if (count > 0) {
          this.rootDescendantCounts.set(rootSessionID, count - 1);
        }
      };
    }

    return () => {}; // No-op rollback for root tasks
  }
  ```

- [ ] **Step 4: Update `launch()` to use spawn limits**

  Replace the **entire `launch()` method** (from `async launch(opts: LaunchOptions)` through its closing `}`) with the following complete replacement:

  ```typescript
  async launch(opts: LaunchOptions): Promise<BackgroundTask> {
    // Resolve spawn context SYNCHRONOUSLY before any await (prevents race window)
    const { depth, rootSessionID } = this.resolveSpawnContext(opts.parentSessionId);
    const rollback = this.validateAndReserveSpawn(depth, rootSessionID);

    const task: BackgroundTask = {
      id: generateTaskId(),
      status: "pending",
      agent: opts.agent,
      model: opts.model ?? "anthropic/claude-sonnet-4-20250514",
      description: opts.description,
      parentSessionID: opts.parentSessionId,
      depth,
      createdAt: Date.now(),
    };

    this.tasks.set(task.id, task);

    try {
      const session = await this.client.session.create({
        body: {
          parentID: opts.parentSessionId,
          title: `Background: ${opts.description}`,
        },
        query: { directory: this.directory },
      });

      if (!session.data?.id) {
        throw new Error("Failed to create background session");
      }

      task.sessionID = session.data.id;
      task.rootSessionID = rootSessionID ?? session.data.id; // root points to itself
      task.timeoutMs = opts.timeoutMs;
      this.tasksBySessionId.set(session.data.id, task.id);
      this.taskDepths.set(session.data.id, depth); // Track depth by sessionID
      registerSubagentSession(session.data.id);

      await writeTask(this.directory, task).catch((err) => {
        console.warn(`[background-manager] Failed to persist task ${task.id}:`, err);
      });

      if (opts.timeoutMs !== undefined && opts.timeoutMs > 0) {
        this.scheduleTimeout(task.id, opts.timeoutMs);
      }

      this.startPrompt(task, opts).catch(() => {});
    } catch (err) {
      rollback();
      await this.finalize(task, "failed", {
        error: err instanceof Error ? err.message : String(err),
      });
      this.tasks.delete(task.id);
    }

    return task;
  }
  ```

  **Important**: The `rollback()` call in the catch block handles the case where session creation fails after we've already reserved a descendant slot.

- [ ] **Step 5: Update `src/index.ts` to pass spawnLimits to manager**

  In `packages/opencode-plugin/src/index.ts`, change:
  ```typescript
  const manager = new BackgroundTaskManager(ctx);
  ```
  to:
  ```typescript
  const manager = new BackgroundTaskManager(ctx, pluginConfig.spawnLimits);
  ```

- [ ] **Step 6: Run spawn-limits tests to verify they pass**

  Run: `bun test packages/opencode-plugin/src/delegation/__tests__/spawn-limits.test.ts`
  Expected: All tests PASS

- [ ] **Step 7: Run all delegation tests to verify no regressions**

  Run: `bun test packages/opencode-plugin/src/delegation/__tests__/`
  Expected: All tests PASS (including the 4 existing test files)

- [ ] **Step 8: Run full test suite**

  Run: `bun test`
  Expected: All tests PASS

- [ ] **Step 9: Verify TypeScript compiles**

  Run: `bunx tsc --noEmit`
  Expected: No errors

- [ ] **Step 10: Commit**

  ```bash
  jj describe -m "feat(delegation): implement spawn depth and descendant limits in BackgroundTaskManager"
  jj new
  ```

---

## Task 5: Final verification

- [ ] **Step 1: Run linting**

  Run: `bunx biome check packages/opencode-plugin/src/`
  Expected: No errors

- [ ] **Step 2: Final full test run**

  Run: `bun test`
  Expected: All tests pass

  Capture output — this goes in the PR body as evidence.

- [ ] **Step 3: Commit**

  ```bash
  jj describe -m "feat(legion): add spawn depth and descendant limits"
  jj new
  ```

---

## Testing Plan

### Skills to Invoke
No project-specific testing skills identified. Standard `bun test` applies.

### Test Coverage Summary

| Test Category | Count | Location |
|--------------|-------|----------|
| Depth tracking (root/child/grandchild/orphan) | 4 | spawn-limits.test.ts |
| Depth boundary (success at 4, rejection at 5) | 3 | spawn-limits.test.ts |
| Descendant boundary (success at 20, rejection at 21) | 3 | spawn-limits.test.ts |
| Config overrides (custom depth/descendants) | 2 | spawn-limits.test.ts |
| Root resolution (rootSessionID tracking) | 2 | spawn-limits.test.ts |
| Concurrent spawns (race condition) | 1 | spawn-limits.test.ts |
| Error message format | 2 | spawn-limits.test.ts |
| Integration: spawn chain A→B→C→D→E→F | 1 | spawn-limits.test.ts |
| Integration: descendant flood (21 tasks) | 1 | spawn-limits.test.ts |
| Config schema (defaults, overrides, partial merge) | 3 | spawn-limits-config.test.ts |
| Regression: existing delegation tests | 4 files | existing __tests__/ |

### Evidence Requirements
PR body must include:
1. `bun test` output showing all new tests passing
2. `bun test` output showing all existing delegation tests still passing
3. Smoke test result (real worker run or equivalent)
