import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ToolContext } from "@opencode-ai/plugin";
import { writeJsonAtomic } from "../storage";
import { createTaskClaimNextTool } from "../task-claim";
import { createTaskCreateTool } from "../task-create";
import { indexPathFor, readTaskIndex, upsertIndexEntry, writeTaskIndexAtomic } from "../task-index";
import { createTaskUpdateTool } from "../task-update";
import type { Task } from "../types";

let tempDir: string;

beforeEach(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "task-index-test-"));
});

afterEach(() => {
  fs.rmSync(tempDir, { recursive: true, force: true });
});

function idxPath(): string {
  return indexPathFor(tempDir);
}

describe("readTaskIndex", () => {
  it("returns null for non-existent file", () => {
    expect(readTaskIndex(idxPath())).toBeNull();
  });

  it("returns null for malformed JSON", () => {
    fs.writeFileSync(idxPath(), "{not json");
    expect(readTaskIndex(idxPath())).toBeNull();
  });

  it("returns null for invalid schema", () => {
    fs.writeFileSync(idxPath(), JSON.stringify({ wrong: true }));
    expect(readTaskIndex(idxPath())).toBeNull();
  });

  it("returns parsed index for valid file", () => {
    fs.writeFileSync(
      idxPath(),
      JSON.stringify({ version: 1, entries: [{ id: "T-1", status: "pending" }] })
    );
    const result = readTaskIndex(idxPath());
    expect(result).not.toBeNull();
    expect(result?.version).toBe(1);
    expect(result?.entries).toHaveLength(1);
  });
});

describe("writeTaskIndexAtomic", () => {
  it("writes valid index file", () => {
    writeTaskIndexAtomic(idxPath(), { version: 1, entries: [] });
    const content = JSON.parse(fs.readFileSync(idxPath(), "utf-8"));
    expect(content.version).toBe(1);
    expect(content.entries).toEqual([]);
  });

  it("creates parent directories", () => {
    const nested = path.join(tempDir, "a", "b", "active-index.json");
    writeTaskIndexAtomic(nested, { version: 1, entries: [] });
    expect(fs.existsSync(nested)).toBe(true);
  });

  it("overwrites existing file atomically", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-old", status: "pending" }],
    });
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-new", status: "in_progress" }],
    });
    const result = readTaskIndex(idxPath());
    expect(result?.entries).toHaveLength(1);
    expect(result?.entries[0].id).toBe("T-new");
  });
});

describe("upsertIndexEntry", () => {
  it("adds a new pending entry", () => {
    writeTaskIndexAtomic(idxPath(), { version: 1, entries: [] });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "pending" });
    const result = readTaskIndex(idxPath());
    expect(result?.entries).toHaveLength(1);
    expect(result?.entries[0]).toEqual({ id: "T-1", status: "pending" });
  });

  it("updates existing entry status", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-1", status: "pending" }],
    });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "in_progress" });
    const result = readTaskIndex(idxPath());
    expect(result?.entries).toHaveLength(1);
    expect(result?.entries[0].status).toBe("in_progress");
  });

  it("removes entry when status is completed", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-1", status: "pending" }],
    });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "completed" });
    const result = readTaskIndex(idxPath());
    expect(result?.entries).toHaveLength(0);
  });

  it("removes entry when status is cancelled", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [{ id: "T-1", status: "in_progress" }],
    });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "cancelled" });
    const result = readTaskIndex(idxPath());
    expect(result?.entries).toHaveLength(0);
  });

  it("creates index if it does not exist", () => {
    upsertIndexEntry(idxPath(), { id: "T-1", status: "pending" });
    const result = readTaskIndex(idxPath());
    expect(result).not.toBeNull();
    expect(result?.entries).toHaveLength(1);
  });

  it("preserves other entries", () => {
    writeTaskIndexAtomic(idxPath(), {
      version: 1,
      entries: [
        { id: "T-1", status: "pending" },
        { id: "T-2", status: "in_progress" },
      ],
    });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "completed" });
    const result = readTaskIndex(idxPath());
    expect(result?.entries).toHaveLength(1);
    expect(result?.entries[0].id).toBe("T-2");
  });

  it("handles multiple upserts to same entry", () => {
    writeTaskIndexAtomic(idxPath(), { version: 1, entries: [] });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "pending" });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "in_progress" });
    upsertIndexEntry(idxPath(), { id: "T-1", status: "pending" });
    const result = readTaskIndex(idxPath());
    expect(result?.entries).toHaveLength(1);
    expect(result?.entries[0].status).toBe("pending");
  });

  it("handles missing index file gracefully", () => {
    expect(fs.existsSync(idxPath())).toBe(false);
    upsertIndexEntry(idxPath(), { id: "T-1", status: "pending" });
    const result = readTaskIndex(idxPath());
    expect(result).not.toBeNull();
    expect(result?.entries).toHaveLength(1);
  });
});

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

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: "T-index-integ",
    subject: "Test task",
    description: "",
    status: "pending",
    blocks: [],
    blockedBy: [],
    threadID: "session-1",
    ...overrides,
  };
}

describe("index integration: task_create", () => {
  it("adds new task to index on create", async () => {
    const tool = createTaskCreateTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({ subject: "Indexed" }, makeContext()));
    expect(result.task).toBeTruthy();

    const index = readTaskIndex(idxPath());
    expect(index).not.toBeNull();
    const entry = index?.entries.find((e) => e.id === result.task.id);
    expect(entry).toBeTruthy();
    expect(entry?.status).toBe("pending");
  });
});

describe("index integration: task_update", () => {
  it("removes task from index on completion", async () => {
    const createTool = createTaskCreateTool(undefined, tempDir);
    const created = JSON.parse(
      await createTool.execute({ subject: "Will complete" }, makeContext())
    );
    const taskId = created.task.id;

    const updateTool = createTaskUpdateTool(undefined, tempDir);
    await updateTool.execute({ id: taskId, status: "completed" }, makeContext());

    const index = readTaskIndex(idxPath());
    expect(index).not.toBeNull();
    const entry = index?.entries.find((e) => e.id === taskId);
    expect(entry).toBeUndefined();
  });

  it("updates status in index on status change", async () => {
    const createTool = createTaskCreateTool(undefined, tempDir);
    const created = JSON.parse(
      await createTool.execute({ subject: "Will progress" }, makeContext())
    );
    const taskId = created.task.id;

    const updateTool = createTaskUpdateTool(undefined, tempDir);
    await updateTool.execute({ id: taskId, status: "in_progress" }, makeContext());

    const index = readTaskIndex(idxPath());
    const entry = index?.entries.find((e) => e.id === taskId);
    expect(entry).toBeTruthy();
    expect(entry?.status).toBe("in_progress");
  });
});

describe("index integration: task_claim_next", () => {
  it("updates index entry to in_progress on claim", async () => {
    writeJsonAtomic(path.join(tempDir, "T-claimable.json"), makeTask({ id: "T-claimable" }));
    upsertIndexEntry(idxPath(), { id: "T-claimable", status: "pending" });

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext("agent-1")));

    expect(result.task).toBeTruthy();
    expect(result.task.id).toBe("T-claimable");

    const index = readTaskIndex(idxPath());
    const entry = index?.entries.find((e) => e.id === "T-claimable");
    expect(entry).toBeTruthy();
    expect(entry?.status).toBe("in_progress");
  });
});
