import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { WorkerEntry } from "./serve-manager";

export type WorkerState = Record<string, WorkerEntry>;

function resolveHome(filePath: string): string {
  if (filePath.startsWith("~/")) {
    return path.join(os.homedir(), filePath.slice(2));
  }
  return filePath;
}

export async function readStateFile(filePath: string): Promise<WorkerState> {
  const resolvedPath = resolveHome(filePath);
  try {
    const raw = await readFile(resolvedPath, "utf-8");
    if (!raw.trim()) {
      return {};
    }
    return JSON.parse(raw) as WorkerState;
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") {
      return {};
    }
    throw error;
  }
}

export async function writeStateFile(filePath: string, state: WorkerState): Promise<void> {
  const resolvedPath = resolveHome(filePath);
  const dir = path.dirname(resolvedPath);
  await mkdir(dir, { recursive: true });

  const tempPath = `${resolvedPath}.tmp-${process.pid}-${Date.now()}`;
  const payload = JSON.stringify(state, null, 2);
  await writeFile(tempPath, payload, "utf-8");
  await rename(tempPath, resolvedPath);
}
