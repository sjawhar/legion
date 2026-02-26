import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { PluginInput } from "@opencode-ai/plugin";
import { BackgroundTaskManager } from "../background-manager";
import { writeTask } from "../task-storage";
import type { BackgroundTask } from "../types";

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
  } as unknown as PluginInput;
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

  it("does not index completed/failed tasks in sessionId map", async () => {
    const task = makeTask({ id: "bg_ses1", sessionID: "ses_abc", status: "completed" });
    await writeTask(workspace, task);

    const manager = createManager(workspace);
    await manager.rehydrate();

    expect(manager.isBackgroundSession("ses_abc")).toBe(false);
  });

  it("skips tasks older than taskRetentionMs", async () => {
    const oldTask = makeTask({
      id: "bg_old1",
      createdAt: Date.now() - 2 * 60 * 60 * 1000,
      completedAt: Date.now() - 2 * 60 * 60 * 1000,
    });
    await writeTask(workspace, oldTask);

    const manager = createManager(workspace);
    await manager.rehydrate({ taskRetentionMs: 60 * 60 * 1000 });

    const output = await manager.getTaskOutput("bg_old1");
    expect(output).toContain("Task not found");

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
