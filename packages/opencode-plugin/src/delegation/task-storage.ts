import fs from "node:fs/promises";
import path from "node:path";
import type { BackgroundTask } from "./types";

const LEGION_DIR = ".legion";
const TASKS_DIR = "tasks";
const DIR_MODE = 0o700;
const FILE_MODE = 0o600;

function validateTaskId(taskId: string): void {
  if (
    taskId.includes("/") ||
    taskId.includes("\\") ||
    taskId === "." ||
    taskId === ".." ||
    taskId.includes("..")
  ) {
    throw new Error(`Invalid task ID: ${taskId}`);
  }
}

function tasksDir(workspace: string): string {
  return path.join(workspace, LEGION_DIR, TASKS_DIR);
}

function taskPath(workspace: string, taskId: string): string {
  return path.join(tasksDir(workspace), `${taskId}.json`);
}

function tmpPath(workspace: string, taskId: string): string {
  return path.join(tasksDir(workspace), `${taskId}.tmp`);
}

async function ensureTasksDir(workspace: string): Promise<void> {
  await fs.mkdir(tasksDir(workspace), { recursive: true, mode: DIR_MODE });
}

/** Atomic write via tmp+rename. Creates `.legion/tasks/` on first write. */
export async function writeTask(workspace: string, task: BackgroundTask): Promise<void> {
  validateTaskId(task.id);
  await ensureTasksDir(workspace);

  const tmp = tmpPath(workspace, task.id);
  const dest = taskPath(workspace, task.id);
  const data = JSON.stringify(task, null, 2);

  await fs.writeFile(tmp, data, { encoding: "utf-8", mode: FILE_MODE });
  await fs.rename(tmp, dest);
}

/** Returns null on ENOENT or malformed JSON (logs warning). */
export async function readTask(workspace: string, taskId: string): Promise<BackgroundTask | null> {
  validateTaskId(taskId);
  const filePath = taskPath(workspace, taskId);

  try {
    const stat = await fs.lstat(filePath);
    if (!stat.isFile()) {
      console.warn(`[task-storage] Skipping non-regular file: ${filePath}`);
      return null;
    }

    const data = await fs.readFile(filePath, "utf-8");
    return JSON.parse(data) as BackgroundTask;
  } catch (err: unknown) {
    if (isEnoent(err)) {
      return null;
    }
    console.warn(`[task-storage] Failed to read task ${taskId}:`, err);
    return null;
  }
}

/** Reads all `.json` task files, skipping `.tmp` and malformed entries. */
export async function listTasks(workspace: string): Promise<BackgroundTask[]> {
  const dir = tasksDir(workspace);
  let entries: string[];

  try {
    entries = await fs.readdir(dir);
  } catch (err: unknown) {
    if (isEnoent(err)) {
      return [];
    }
    throw err;
  }

  const tasks: BackgroundTask[] = [];

  for (const entry of entries) {
    if (!entry.endsWith(".json")) continue;

    const filePath = path.join(dir, entry);

    try {
      const stat = await fs.lstat(filePath);
      if (!stat.isFile()) {
        console.warn(`[task-storage] Skipping non-regular file: ${filePath}`);
        continue;
      }

      const data = await fs.readFile(filePath, "utf-8");
      const task = JSON.parse(data) as BackgroundTask;
      tasks.push(task);
    } catch (err: unknown) {
      if (isEnoent(err)) {
        continue;
      }
      console.warn(`[task-storage] Skipping malformed task file ${entry}:`, err);
    }
  }

  return tasks;
}

export async function deleteTask(workspace: string, taskId: string): Promise<void> {
  validateTaskId(taskId);

  try {
    await fs.unlink(taskPath(workspace, taskId));
  } catch (err: unknown) {
    if (isEnoent(err)) {
      return;
    }
    throw err;
  }
}

function isEnoent(err: unknown): boolean {
  return (
    typeof err === "object" &&
    err !== null &&
    "code" in err &&
    (err as { code: string }).code === "ENOENT"
  );
}
