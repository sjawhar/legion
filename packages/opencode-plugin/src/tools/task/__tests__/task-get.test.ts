import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ToolContext } from "@opencode-ai/plugin";
import { writeJsonAtomic } from "../storage";
import { createTaskGetTool } from "../task-get";

let tempDir: string;

function makeContext(): ToolContext {
  return {
    sessionID: "session-1",
    messageID: "msg-1",
    agent: "orchestrator",
    directory: "/tmp",
    worktree: "/tmp",
    abort: new AbortController().signal,
    metadata: () => {},
    ask: async () => {},
  };
}

beforeEach(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "task-get-test-"));
});

afterEach(() => {
  fs.rmSync(tempDir, { recursive: true, force: true });
});

describe("task_get", () => {
  it("retrieves an existing task", async () => {
    const task = {
      id: "T-test-get",
      subject: "Test get",
      description: "A test task",
      status: "pending",
      blocks: [],
      blockedBy: [],
      threadID: "session-1",
    };
    writeJsonAtomic(path.join(tempDir, "T-test-get.json"), task);

    const tool = createTaskGetTool(tempDir);
    const result = JSON.parse(await tool.execute({ id: "T-test-get" }, makeContext()));

    expect(result.task).toBeTruthy();
    expect(result.task.id).toBe("T-test-get");
    expect(result.task.subject).toBe("Test get");
  });

  it("returns null for non-existent task", async () => {
    const tool = createTaskGetTool(tempDir);
    const result = JSON.parse(await tool.execute({ id: "T-nope" }, makeContext()));
    expect(result.task).toBeNull();
  });

  it("rejects invalid task ID format", async () => {
    const tool = createTaskGetTool(tempDir);
    const result = JSON.parse(await tool.execute({ id: "invalid-format" }, makeContext()));
    expect(result.error).toBe("invalid_task_id");
  });
});
