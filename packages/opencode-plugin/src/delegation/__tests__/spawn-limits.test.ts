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
    const manager = createManager({}, { maxDepth: 3 });
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

    // depth=3 should fail (maxDepth=3 means max allowed depth index is 2)
    await expect(
      manager.launch({
        agent: "explore",
        prompt: "test",
        description: "rejected",
        parentSessionId: grandchild.sessionID,
      })
    ).rejects.toThrow("Spawn rejected: max depth 3 reached (current depth: 3)");

    // After rejection, a sibling of grandchild (depth=3) should still fail too
    // (counters not corrupted — limit still enforced correctly)
    await expect(
      manager.launch({
        agent: "explore",
        prompt: "test",
        description: "also-rejected",
        parentSessionId: grandchild.sessionID,
      })
    ).rejects.toThrow("Spawn rejected: max depth 3 reached (current depth: 3)");
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
    ).rejects.toThrow(
      `Spawn rejected: max descendants 20 reached for root session ${root.sessionID}`
    );
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
    ).rejects.toThrow(
      `Spawn rejected: max descendants 2 reached for root session ${root.sessionID}`
    );

    // 4th should also fail (counter not corrupted by first rejection)
    await expect(
      manager.launch({
        agent: "explore",
        prompt: "test",
        description: "child-4",
        parentSessionId: root.sessionID,
      })
    ).rejects.toThrow(
      `Spawn rejected: max descendants 2 reached for root session ${root.sessionID}`
    );
  });
});

describe("config overrides", () => {
  it("custom maxDepth=4 rejects at depth=4", async () => {
    const manager = createManager({}, { maxDepth: 4 });
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
    ).rejects.toThrow("Spawn rejected: max depth 4 reached (current depth: 4)");
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
    ).rejects.toThrow(
      `Spawn rejected: max descendants 10 reached for root session ${root.sessionID}`
    );
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
    ).rejects.toThrow(
      `Spawn rejected: max descendants 1 reached for root session ${root.sessionID}`
    );
  });
});

describe("error message format", () => {
  it("depth rejection includes limit type and current value", async () => {
    const manager = createManager({}, { maxDepth: 2 });
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
    ).rejects.toThrow("Spawn rejected: max depth 2 reached (current depth: 2)");
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
    ).rejects.toThrow(
      `Spawn rejected: max descendants 1 reached for root session ${root.sessionID}`
    );
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
    ).rejects.toThrow(
      `Spawn rejected: max descendants 20 reached for root session ${root.sessionID}`
    );
  });
});
