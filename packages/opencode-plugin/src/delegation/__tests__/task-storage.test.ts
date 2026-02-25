import { afterEach, beforeEach, describe, expect, it, spyOn } from "bun:test";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { deleteTask, listTasks, readTask, writeTask } from "../task-storage";
import type { BackgroundTask } from "../types";

let workspace: string;

function makeTask(overrides: Partial<BackgroundTask> = {}): BackgroundTask {
  return {
    id: "bg_abc12345",
    status: "running",
    agent: "explore",
    model: "anthropic/claude-sonnet-4-20250514",
    description: "test task",
    createdAt: Date.now(),
    ...overrides,
  };
}

beforeEach(() => {
  workspace = fs.mkdtempSync(path.join(os.tmpdir(), "task-storage-test-"));
});

afterEach(() => {
  fs.rmSync(workspace, { recursive: true, force: true });
});

describe("writeTask + readTask", () => {
  it("writes task file and reads it back with correct content", async () => {
    const task = makeTask();
    await writeTask(workspace, task);

    const filePath = path.join(workspace, ".legion", "tasks", `${task.id}.json`);
    expect(fs.existsSync(filePath)).toBe(true);

    const raw = fs.readFileSync(filePath, "utf-8");
    const parsed = JSON.parse(raw);
    expect(parsed.id).toBe(task.id);
    expect(parsed.status).toBe("running");
    expect(parsed.agent).toBe("explore");

    const readBack = await readTask(workspace, task.id);
    expect(readBack).not.toBeNull();
    expect(readBack?.id).toBe(task.id);
    expect(readBack?.description).toBe("test task");
  });

  it("overwrites existing task file on re-write", async () => {
    const task = makeTask();
    await writeTask(workspace, task);

    task.status = "completed";
    task.completedAt = Date.now();
    await writeTask(workspace, task);

    const readBack = await readTask(workspace, task.id);
    expect(readBack?.status).toBe("completed");
    expect(readBack?.completedAt).toBeDefined();
  });
});

describe("directory creation", () => {
  it("creates .legion/tasks/ on first write", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    expect(fs.existsSync(tasksDir)).toBe(false);

    await writeTask(workspace, makeTask());
    expect(fs.existsSync(tasksDir)).toBe(true);
  });
});

describe("crash safety", () => {
  it("listTasks ignores .tmp files", async () => {
    await writeTask(workspace, makeTask());

    const tmpFile = path.join(workspace, ".legion", "tasks", "bg_orphaned.tmp");
    fs.writeFileSync(tmpFile, '{"id": "bg_orphaned"}');

    const tasks = await listTasks(workspace);
    expect(tasks).toHaveLength(1);
    expect(tasks[0].id).toBe("bg_abc12345");
  });
});

describe("malformed JSON handling", () => {
  it("listTasks skips malformed files with warning", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    fs.mkdirSync(tasksDir, { recursive: true });
    fs.writeFileSync(path.join(tasksDir, "bg_bad.json"), "NOT VALID JSON{{{");

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});

    const tasks = await listTasks(workspace);
    expect(tasks).toHaveLength(0);
    expect(warnSpy).toHaveBeenCalled();

    const calls = warnSpy.mock.calls;
    expect(String(calls[0][0])).toContain("task-storage");

    warnSpy.mockRestore();
  });

  it("readTask returns null for malformed JSON", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    fs.mkdirSync(tasksDir, { recursive: true });
    fs.writeFileSync(path.join(tasksDir, "bg_corrupt.json"), "}{bad");

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    const result = await readTask(workspace, "bg_corrupt");
    expect(result).toBeNull();
    warnSpy.mockRestore();
  });
});

describe("ENOENT handling", () => {
  it("readTask returns null for nonexistent task", async () => {
    const result = await readTask(workspace, "bg_missing");
    expect(result).toBeNull();
  });

  it("listTasks returns empty for nonexistent directory", async () => {
    const tasks = await listTasks(workspace);
    expect(tasks).toHaveLength(0);
  });

  it("deleteTask is no-op for nonexistent task", async () => {
    await expect(deleteTask(workspace, "bg_gone")).resolves.toBeUndefined();
  });

  it("listTasks handles file deleted between readdir and readFile", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    fs.mkdirSync(tasksDir, { recursive: true });
    fs.writeFileSync(path.join(tasksDir, "bg_vanish.json"), '{"id":"bg_vanish"}');

    const origLstat = fsp.lstat.bind(fsp);
    let intercepted = false;
    // @ts-expect-error overload signature mismatch in mock is acceptable
    const lstatSpy = spyOn(fsp, "lstat").mockImplementation(async (p: unknown) => {
      if (!intercepted && String(p).includes("bg_vanish")) {
        intercepted = true;
        fs.unlinkSync(path.join(tasksDir, "bg_vanish.json"));
      }
      return origLstat(p as string);
    });

    const tasks = await listTasks(workspace);
    expect(tasks).toHaveLength(0);

    lstatSpy.mockRestore();
  });
});

describe("path traversal prevention", () => {
  it("rejects task ID with ../", async () => {
    await expect(writeTask(workspace, makeTask({ id: "../etc/passwd" }))).rejects.toThrow(
      "Invalid task ID"
    );
  });

  it("rejects task ID with /", async () => {
    await expect(writeTask(workspace, makeTask({ id: "foo/bar" }))).rejects.toThrow(
      "Invalid task ID"
    );
  });

  it("rejects task ID with \\", async () => {
    await expect(writeTask(workspace, makeTask({ id: "foo\\bar" }))).rejects.toThrow(
      "Invalid task ID"
    );
  });

  it("rejects . as task ID", async () => {
    await expect(readTask(workspace, ".")).rejects.toThrow("Invalid task ID");
  });

  it("rejects .. as task ID", async () => {
    await expect(readTask(workspace, "..")).rejects.toThrow("Invalid task ID");
  });

  it("rejects embedded .. in task ID", async () => {
    await expect(deleteTask(workspace, "bg_a..b")).rejects.toThrow("Invalid task ID");
  });
});

describe("listTasks", () => {
  it("returns all valid tasks", async () => {
    await writeTask(workspace, makeTask({ id: "bg_task1", description: "first" }));
    await writeTask(workspace, makeTask({ id: "bg_task2", description: "second" }));
    await writeTask(workspace, makeTask({ id: "bg_task3", description: "third" }));

    const tasks = await listTasks(workspace);
    expect(tasks).toHaveLength(3);

    const ids = tasks.map((t) => t.id).sort();
    expect(ids).toEqual(["bg_task1", "bg_task2", "bg_task3"]);
  });
});

describe("deleteTask", () => {
  it("removes task file from disk", async () => {
    const task = makeTask();
    await writeTask(workspace, task);

    const filePath = path.join(workspace, ".legion", "tasks", `${task.id}.json`);
    expect(fs.existsSync(filePath)).toBe(true);

    await deleteTask(workspace, task.id);
    expect(fs.existsSync(filePath)).toBe(false);
  });
});

describe("symlink protection", () => {
  it("readTask skips symlinks", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    fs.mkdirSync(tasksDir, { recursive: true });

    const realFile = path.join(workspace, "real-task.json");
    fs.writeFileSync(realFile, JSON.stringify(makeTask({ id: "bg_sym" })));
    fs.symlinkSync(realFile, path.join(tasksDir, "bg_sym.json"));

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    const result = await readTask(workspace, "bg_sym");
    expect(result).toBeNull();
    warnSpy.mockRestore();
  });

  it("listTasks skips symlinks", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    fs.mkdirSync(tasksDir, { recursive: true });

    await writeTask(workspace, makeTask({ id: "bg_real" }));

    const realFile = path.join(workspace, "decoy.json");
    fs.writeFileSync(realFile, JSON.stringify(makeTask({ id: "bg_link" })));
    fs.symlinkSync(realFile, path.join(tasksDir, "bg_link.json"));

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    const tasks = await listTasks(workspace);
    expect(tasks).toHaveLength(1);
    expect(tasks[0].id).toBe("bg_real");
    warnSpy.mockRestore();
  });
});

describe("schema validation", () => {
  it("listTasks skips tasks with invalid schema", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    fs.mkdirSync(tasksDir, { recursive: true });

    // Valid JSON, but missing required fields (no status, no agent)
    fs.writeFileSync(
      path.join(tasksDir, "bg_invalid.json"),
      JSON.stringify({ id: "bg_invalid", description: "missing fields" })
    );
    // Also write a valid task
    await writeTask(workspace, makeTask({ id: "bg_valid" }));

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    const tasks = await listTasks(workspace);
    expect(tasks).toHaveLength(1);
    expect(tasks[0].id).toBe("bg_valid");
    warnSpy.mockRestore();
  });

  it("readTask returns null for schema-invalid task", async () => {
    const tasksDir = path.join(workspace, ".legion", "tasks");
    fs.mkdirSync(tasksDir, { recursive: true });

    fs.writeFileSync(
      path.join(tasksDir, "bg_badschema.json"),
      JSON.stringify({ id: "bg_badschema", wrong: "shape" })
    );

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    const result = await readTask(workspace, "bg_badschema");
    expect(result).toBeNull();
    warnSpy.mockRestore();
  });
});
