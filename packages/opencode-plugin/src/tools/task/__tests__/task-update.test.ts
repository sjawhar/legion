import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ToolContext } from "@opencode-ai/plugin";
import { writeJsonAtomic } from "../storage";
import { createTaskUpdateTool } from "../task-update";
import type { Task } from "../types";
import { MAX_DESCRIPTION_CHARS } from "../types";

let tempDir: string;

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

function writeTask(dir: string, task: Task): void {
  writeJsonAtomic(path.join(dir, `${task.id}.json`), task);
}

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: "T-update-test",
    subject: "Original",
    description: "Original desc",
    status: "pending",
    blocks: [],
    blockedBy: [],
    threadID: "session-1",
    ...overrides,
  };
}

beforeEach(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "task-update-test-"));
});

afterEach(() => {
  fs.rmSync(tempDir, { recursive: true, force: true });
});

describe("task_update", () => {
  it("updates subject and description", async () => {
    writeTask(tempDir, makeTask());
    const tool = createTaskUpdateTool(undefined, tempDir);

    const result = JSON.parse(
      await tool.execute(
        { id: "T-update-test", subject: "Updated", description: "New desc" },
        makeContext()
      )
    );

    expect(result.task.subject).toBe("Updated");
    expect(result.task.description).toBe("New desc");
  });

  it("updates status to cancelled", async () => {
    writeTask(tempDir, makeTask());
    const tool = createTaskUpdateTool(undefined, tempDir);

    const result = JSON.parse(
      await tool.execute({ id: "T-update-test", status: "cancelled" }, makeContext())
    );

    expect(result.task.status).toBe("cancelled");
  });

  it("additively appends blockedBy", async () => {
    writeTask(tempDir, makeTask({ blockedBy: ["T-existing"] }));
    const tool = createTaskUpdateTool(undefined, tempDir);

    const result = JSON.parse(
      await tool.execute({ id: "T-update-test", addBlockedBy: ["T-new"] }, makeContext())
    );

    expect(result.task.blockedBy).toContain("T-existing");
    expect(result.task.blockedBy).toContain("T-new");
  });

  it("additively appends blocks", async () => {
    writeTask(tempDir, makeTask({ blocks: ["T-a"] }));
    const tool = createTaskUpdateTool(undefined, tempDir);

    const result = JSON.parse(
      await tool.execute({ id: "T-update-test", addBlocks: ["T-b"] }, makeContext())
    );

    expect(result.task.blocks).toContain("T-a");
    expect(result.task.blocks).toContain("T-b");
  });

  it("deduplicates additive deps", async () => {
    writeTask(tempDir, makeTask({ blockedBy: ["T-dup"] }));
    const tool = createTaskUpdateTool(undefined, tempDir);

    const result = JSON.parse(
      await tool.execute({ id: "T-update-test", addBlockedBy: ["T-dup", "T-new"] }, makeContext())
    );

    const count = result.task.blockedBy.filter((id: string) => id === "T-dup").length;
    expect(count).toBe(1);
    expect(result.task.blockedBy).toContain("T-new");
  });

  it("merges metadata and deletes null keys", async () => {
    writeTask(tempDir, makeTask({ metadata: { priority: "high", keep: "yes" } }));
    const tool = createTaskUpdateTool(undefined, tempDir);

    const result = JSON.parse(
      await tool.execute(
        { id: "T-update-test", metadata: { priority: null, added: "new" } },
        makeContext()
      )
    );

    expect(result.task.metadata.priority).toBeUndefined();
    expect(result.task.metadata.keep).toBe("yes");
    expect(result.task.metadata.added).toBe("new");
  });

  it("returns error for non-existent task", async () => {
    const tool = createTaskUpdateTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({ id: "T-nope" }, makeContext()));
    expect(result.error).toBe("task_not_found");
  });

  it("rejects cycle when adding blockedBy", async () => {
    writeTask(tempDir, makeTask({ id: "T-a", blockedBy: [] }));
    writeTask(tempDir, makeTask({ id: "T-b", blockedBy: ["T-a"] }));

    const tool = createTaskUpdateTool(undefined, tempDir);

    const result = JSON.parse(
      await tool.execute({ id: "T-a", addBlockedBy: ["T-b"] }, makeContext())
    );

    expect(result.error).toBe("cycle_detected");
    expect(result.cycle).toBeTruthy();
  });
});

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

  it("accepts description at the limit", async () => {
    writeTask(tempDir, makeTask());
    const tool = createTaskUpdateTool(undefined, tempDir);
    const result = JSON.parse(
      await tool.execute(
        { id: "T-update-test", description: "x".repeat(MAX_DESCRIPTION_CHARS) },
        makeContext()
      )
    );
    expect(result.task).toBeTruthy();
    expect(result.task.description).toHaveLength(MAX_DESCRIPTION_CHARS);
  });

  it("accepts empty description update", async () => {
    writeTask(tempDir, makeTask());
    const tool = createTaskUpdateTool(undefined, tempDir);
    const result = JSON.parse(
      await tool.execute({ id: "T-update-test", description: "" }, makeContext())
    );
    expect(result.task).toBeTruthy();
    expect(result.task.description).toBe("");
  });
});
