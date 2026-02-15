import { afterEach, beforeEach, describe, expect, it, spyOn } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { PluginInput } from "@opencode-ai/plugin";
import { BackgroundTaskManager } from "../background-manager";
import type { BackgroundTask } from "../types";

let workspace: string;

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

function getSessions(manager: BackgroundTaskManager): Map<string, string> {
  return (manager as unknown as { tasksBySessionId: Map<string, string> }).tasksBySessionId;
}

function createManager(
  overrides: Partial<{
    create: () => Promise<{ data?: { id?: string } }>;
    promptAsync: () => Promise<void>;
    messages: () => Promise<{ data?: unknown[] }>;
    abort: () => Promise<void>;
  }> = {}
): {
  manager: BackgroundTaskManager;
  session: {
    create: () => Promise<{ data?: { id?: string } }>;
    promptAsync: () => Promise<void>;
    messages: () => Promise<{ data?: unknown[] }>;
    abort: () => Promise<void>;
  };
} {
  const session = {
    create: async () => ({ data: { id: "session-1" } }),
    promptAsync: async () => {},
    messages: async () => ({ data: [] }),
    abort: async () => {},
    ...overrides,
  };
  const client = { session };
  const manager = new BackgroundTaskManager({
    client,
    directory: workspace,
  } as unknown as PluginInput);

  return { manager, session };
}

beforeEach(() => {
  workspace = fs.mkdtempSync(path.join(os.tmpdir(), "bg-manager-test-"));
});

afterEach(() => {
  fs.rmSync(workspace, { recursive: true, force: true });
});

describe("finalize", () => {
  it("is idempotent and keeps first terminal status", async () => {
    const { manager } = createManager();
    const task = makeTask({ status: "running" });
    getTasks(manager).set(task.id, task);

    await manager.finalize(task, "completed");
    const firstCompletedAt = task.completedAt;

    await manager.finalize(task, "cancelled");

    expect(task.status).toBe("completed");
    expect(task.completedAt).toBe(firstCompletedAt);
  });

  it("eagerly caches output on completion", async () => {
    const { manager, session } = createManager({
      messages: async () => ({
        data: [
          { info: { role: "assistant" }, parts: [{ type: "text", text: "Hello" }] },
          { info: { role: "assistant" }, parts: [{ type: "text", text: "World" }] },
        ],
      }),
    });
    const messagesSpy = spyOn(session, "messages");
    const task = makeTask({ sessionID: "session-1" });
    getTasks(manager).set(task.id, task);
    getSessions(manager).set("session-1", task.id);

    await manager.finalize(task, "completed");

    expect(task.result).toBe("Hello\n\nWorld");
    expect(messagesSpy).toHaveBeenCalledTimes(1);
  });

  it("removes failed tasks without session IDs from the task map", async () => {
    const { manager } = createManager();
    const task = makeTask({ sessionID: undefined });
    getTasks(manager).set(task.id, task);

    await manager.finalize(task, "failed", { error: "boom" });

    expect(manager.getTask(task.id)).toBeUndefined();
  });
});

describe("cleanup", () => {
  it("finalizes running tasks when session is deleted", async () => {
    const { manager } = createManager();
    const task = makeTask({ status: "running", sessionID: "session-1" });
    getTasks(manager).set(task.id, task);
    getSessions(manager).set("session-1", task.id);

    await manager.cleanup("session-1");

    expect(task.status).toBe("failed");
    expect(task.error).toBe("Session deleted before completion");
    expect(task.completedAt).toBeDefined();
  });

  it("leaves completed tasks untouched but removes map entries", async () => {
    const { manager } = createManager();
    const task = makeTask({ status: "completed", sessionID: "session-1" });
    getTasks(manager).set(task.id, task);
    getSessions(manager).set("session-1", task.id);

    await manager.cleanup("session-1");

    expect(task.status).toBe("completed");
    expect(manager.getTask(task.id)).toBeUndefined();
    expect(getSessions(manager).has("session-1")).toBe(false);
  });
});

describe("handleSessionStatus", () => {
  it("does not complete pending tasks on idle status", async () => {
    const { manager } = createManager();
    const task = makeTask({ status: "pending", sessionID: "session-1" });
    getTasks(manager).set(task.id, task);
    getSessions(manager).set("session-1", task.id);

    await manager.handleSessionStatus({
      type: "session.status",
      properties: { sessionID: "session-1", status: { type: "idle" } },
    });

    expect(task.status).toBe("pending");
  });

  it("completes running tasks on idle status", async () => {
    const { manager } = createManager();
    const task = makeTask({ status: "running", sessionID: "session-1" });
    getTasks(manager).set(task.id, task);
    getSessions(manager).set("session-1", task.id);

    await manager.handleSessionStatus({
      type: "session.status",
      properties: { sessionID: "session-1", status: { type: "idle" } },
    });

    expect(task.status).toBe("completed");
  });
});

describe("cancel", () => {
  it("finalizes running tasks and aborts the session", async () => {
    const { manager, session } = createManager();
    const abortSpy = spyOn(session, "abort");
    const task = makeTask({ status: "running", sessionID: "session-1" });
    getTasks(manager).set(task.id, task);
    getSessions(manager).set("session-1", task.id);

    const result = await manager.cancel(task.id);

    expect(result).toBe(true);
    expect(task.status).toBe("cancelled");
    expect(abortSpy).toHaveBeenCalledTimes(1);
  });

  it("returns false for already completed tasks", async () => {
    const { manager, session } = createManager();
    const abortSpy = spyOn(session, "abort");
    const task = makeTask({ status: "completed", sessionID: "session-1" });
    getTasks(manager).set(task.id, task);

    const result = await manager.cancel(task.id);

    expect(result).toBe(false);
    expect(task.status).toBe("completed");
    expect(abortSpy).not.toHaveBeenCalled();
  });
});

describe("launch", () => {
  it("finalizes failed session creation and removes task", async () => {
    const { manager } = createManager({
      create: async () => {
        throw new Error("create failed");
      },
    });

    const task = await manager.launch({
      agent: "explore",
      prompt: "hello",
      description: "test launch",
    });

    expect(task.status).toBe("failed");
    expect(task.error).toContain("create failed");
    expect(task.completedAt).toBeDefined();
    expect(manager.getTask(task.id)).toBeUndefined();
  });
});
