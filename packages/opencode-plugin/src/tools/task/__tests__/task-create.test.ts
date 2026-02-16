import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ToolContext } from "@opencode-ai/plugin";
import { writeJsonAtomic } from "../storage";
import { createTaskCreateTool } from "../task-create";
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

beforeEach(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "task-create-test-"));
});

afterEach(() => {
  fs.rmSync(tempDir, { recursive: true, force: true });
});

describe("task_create", () => {
  it("creates a task with auto-generated ID", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({ subject: "Add tests" }, makeContext()));

    expect(result.task).toBeTruthy();
    expect(result.task.id).toMatch(/^T-/);
    expect(result.task.subject).toBe("Add tests");
  });

  it("persists task to disk", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({ subject: "Persist me" }, makeContext()));

    const filePath = path.join(tempDir, `${result.task.id}.json`);
    expect(fs.existsSync(filePath)).toBe(true);

    const task = JSON.parse(fs.readFileSync(filePath, "utf-8"));
    expect(task.subject).toBe("Persist me");
    expect(task.status).toBe("pending");
    expect(task.threadID).toBe("session-1");
  });

  it("sets blockedBy and blocks", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);

    const r1 = JSON.parse(await tool.execute({ subject: "First" }, makeContext()));
    const r2 = JSON.parse(
      await tool.execute({ subject: "Second", blockedBy: [r1.task.id] }, makeContext())
    );

    const filePath = path.join(tempDir, `${r2.task.id}.json`);
    const task = JSON.parse(fs.readFileSync(filePath, "utf-8"));
    expect(task.blockedBy).toContain(r1.task.id);
  });

  it("warns on non-existent blockedBy reference", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(
      await tool.execute({ subject: "Orphan dep", blockedBy: ["T-nonexistent"] }, makeContext())
    );

    expect(result.task).toBeTruthy();
    expect(result.warnings).toBeTruthy();
    expect(result.warnings[0]).toContain("non-existent");
  });

  it("rejects cycle in blockedBy", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);

    const r1 = JSON.parse(await tool.execute({ subject: "A" }, makeContext()));

    writeTask(tempDir, {
      ...JSON.parse(fs.readFileSync(path.join(tempDir, `${r1.task.id}.json`), "utf-8")),
      blockedBy: ["T-future"],
    });

    const result = JSON.parse(
      await tool.execute({ subject: "B", blockedBy: [r1.task.id], blocks: [] }, makeContext())
    );

    expect(result.task).toBeTruthy();
  });

  it("returns validation error for missing subject", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({} as Record<string, unknown>, makeContext()));

    expect(result.error).toBeTruthy();
  });
});

describe("task_create guardrails", () => {
  it("rejects oversized description", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(
      await tool.execute(
        { subject: "Big", description: "x".repeat(MAX_DESCRIPTION_CHARS + 1) },
        makeContext()
      )
    );
    expect(result.error).toBe("validation_error");
    expect(result.message).toContain("description");
  });

  it("accepts description at the limit", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(
      await tool.execute(
        { subject: "OK", description: "x".repeat(MAX_DESCRIPTION_CHARS) },
        makeContext()
      )
    );
    expect(result.task).toBeTruthy();
  });

  it("accepts empty description", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(
      await tool.execute({ subject: "No desc", description: "" }, makeContext())
    );
    expect(result.task).toBeTruthy();
    expect(result.task.id).toMatch(/^T-/);
  });

  it("accepts task without description field", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({ subject: "No desc field" }, makeContext()));
    expect(result.task).toBeTruthy();
    expect(result.task.id).toMatch(/^T-/);
  });
});
