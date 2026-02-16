import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ToolContext } from "@opencode-ai/plugin";
import { writeJsonAtomic } from "../storage";
import { indexPathFor, writeTaskIndexAtomic } from "../task-index";
import { createTaskListTool } from "../task-list";
import type { Task } from "../types";

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

function writeTask(dir: string, task: Task): void {
  writeJsonAtomic(path.join(dir, `${task.id}.json`), task);
}

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: "T-list-test",
    subject: "Test task",
    description: "",
    status: "pending",
    blocks: [],
    blockedBy: [],
    threadID: "session-1",
    ...overrides,
  };
}

beforeEach(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "task-list-test-"));
});

afterEach(() => {
  fs.rmSync(tempDir, { recursive: true, force: true });
});

describe("task_list", () => {
  it("returns empty list when no tasks", async () => {
    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));
    expect(result.tasks).toEqual([]);
  });

  it("excludes completed and cancelled tasks", async () => {
    writeTask(tempDir, makeTask({ id: "T-pending", status: "pending" }));
    writeTask(tempDir, makeTask({ id: "T-done", status: "completed" }));
    writeTask(tempDir, makeTask({ id: "T-cancelled", status: "cancelled" }));
    writeTask(tempDir, makeTask({ id: "T-active", status: "in_progress" }));

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-pending");
    expect(ids).toContain("T-active");
    expect(ids).not.toContain("T-done");
    expect(ids).not.toContain("T-cancelled");
  });

  it("filters unresolved blockers only", async () => {
    writeTask(tempDir, makeTask({ id: "T-blocker", status: "completed" }));
    writeTask(tempDir, makeTask({ id: "T-active-blocker", status: "in_progress" }));
    writeTask(
      tempDir,
      makeTask({
        id: "T-blocked",
        status: "pending",
        blockedBy: ["T-blocker", "T-active-blocker"],
      })
    );

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    const blocked = result.tasks.find((t: { id: string }) => t.id === "T-blocked");
    expect(blocked.blockedBy).toEqual(["T-active-blocker"]);
  });

  it("cancelled satisfies dependencies", async () => {
    writeTask(tempDir, makeTask({ id: "T-cancelled-dep", status: "cancelled" }));
    writeTask(
      tempDir,
      makeTask({
        id: "T-waiting",
        status: "pending",
        blockedBy: ["T-cancelled-dep"],
      })
    );

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({ ready: true }, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-waiting");
  });

  it("ready=true filters to tasks with all deps satisfied", async () => {
    writeTask(tempDir, makeTask({ id: "T-dep", status: "completed" }));
    writeTask(tempDir, makeTask({ id: "T-ready", status: "pending", blockedBy: ["T-dep"] }));
    writeTask(
      tempDir,
      makeTask({ id: "T-blocked", status: "pending", blockedBy: ["T-still-pending"] })
    );
    writeTask(tempDir, makeTask({ id: "T-still-pending", status: "pending" }));

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({ ready: true }, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-ready");
    expect(ids).toContain("T-still-pending");
    expect(ids).not.toContain("T-blocked");
  });

  it("without ready filter, includes blocked tasks", async () => {
    writeTask(tempDir, makeTask({ id: "T-dep", status: "completed" }));
    writeTask(tempDir, makeTask({ id: "T-ready", status: "pending", blockedBy: ["T-dep"] }));
    writeTask(
      tempDir,
      makeTask({ id: "T-blocked", status: "pending", blockedBy: ["T-still-pending"] })
    );
    writeTask(tempDir, makeTask({ id: "T-still-pending", status: "pending" }));

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-ready");
    expect(ids).toContain("T-blocked");
    expect(ids).toContain("T-still-pending");
  });

  it("missing dep = blocked (safe default)", async () => {
    writeTask(tempDir, makeTask({ id: "T-orphan", status: "pending", blockedBy: ["T-ghost"] }));

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({ ready: true }, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).not.toContain("T-orphan");
  });

  it("filters by parentID", async () => {
    writeTask(tempDir, makeTask({ id: "T-child", parentID: "T-parent" }));
    writeTask(tempDir, makeTask({ id: "T-other" }));

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({ parentID: "T-parent" }, makeContext()));

    expect(result.tasks).toHaveLength(1);
    expect(result.tasks[0].id).toBe("T-child");
  });
});

describe("index-aware listing", () => {
  it("skips reading completed task files when index exists", async () => {
    for (let i = 0; i < 3; i++) {
      writeTask(tempDir, makeTask({ id: `T-done-${i}`, status: "completed" }));
    }
    writeTask(tempDir, makeTask({ id: "T-active", status: "pending" }));

    writeTaskIndexAtomic(indexPathFor(tempDir), {
      version: 1,
      entries: [{ id: "T-active", status: "pending" }],
    });

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.tasks).toHaveLength(1);
    expect(result.tasks[0].id).toBe("T-active");
  });

  it("resolves completed blockers not in index", async () => {
    writeTask(tempDir, makeTask({ id: "T-dep", status: "completed" }));
    writeTask(tempDir, makeTask({ id: "T-waiting", status: "pending", blockedBy: ["T-dep"] }));

    writeTaskIndexAtomic(indexPathFor(tempDir), {
      version: 1,
      entries: [{ id: "T-waiting", status: "pending" }],
    });

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({ ready: true }, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-waiting");
  });

  it("treats missing blocker file as blocking even with index", async () => {
    writeTask(tempDir, makeTask({ id: "T-orphan", status: "pending", blockedBy: ["T-ghost"] }));

    writeTaskIndexAtomic(indexPathFor(tempDir), {
      version: 1,
      entries: [{ id: "T-orphan", status: "pending" }],
    });

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({ ready: true }, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).not.toContain("T-orphan");
  });

  it("falls back to full scan when no index exists", async () => {
    writeTask(tempDir, makeTask({ id: "T-a", status: "pending" }));
    writeTask(tempDir, makeTask({ id: "T-b", status: "in_progress" }));

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-a");
    expect(ids).toContain("T-b");
  });

  it("skips missing task files referenced in index", async () => {
    writeTask(tempDir, makeTask({ id: "T-exists", status: "pending" }));
    writeTaskIndexAtomic(indexPathFor(tempDir), {
      version: 1,
      entries: [
        { id: "T-exists", status: "pending" },
        { id: "T-missing", status: "pending" },
      ],
    });

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-exists");
    expect(ids).not.toContain("T-missing");
  });

  it("recovers from corrupted index by falling back to full scan", async () => {
    writeTask(tempDir, makeTask({ id: "T-a", status: "pending" }));
    writeTask(tempDir, makeTask({ id: "T-b", status: "in_progress" }));

    fs.writeFileSync(indexPathFor(tempDir), "{invalid json");

    const tool = createTaskListTool(tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    const ids = result.tasks.map((t: { id: string }) => t.id);
    expect(ids).toContain("T-a");
    expect(ids).toContain("T-b");
  });
});
