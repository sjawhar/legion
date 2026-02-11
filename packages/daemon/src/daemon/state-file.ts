import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { WorkerEntry } from "./serve-manager";

export interface CrashHistoryEntry {
  crashCount: number;
  lastCrashAt: string | null;
}

export interface PersistedWorkerState {
  workers: Record<string, WorkerEntry>;
  crashHistory: Record<string, CrashHistoryEntry>;
}

export type WorkerState = Record<string, WorkerEntry>;

function resolveHome(filePath: string): string {
  if (filePath.startsWith("~/")) {
    return path.join(os.homedir(), filePath.slice(2));
  }
  return filePath;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function normalizeState(raw: unknown): PersistedWorkerState {
  if (isRecord(raw) && isRecord(raw.workers)) {
    return {
      workers: raw.workers as Record<string, WorkerEntry>,
      crashHistory: isRecord(raw.crashHistory)
        ? (raw.crashHistory as Record<string, CrashHistoryEntry>)
        : {},
    };
  }

  return {
    workers: (raw ?? {}) as Record<string, WorkerEntry>,
    crashHistory: {},
  };
}

export async function readStateFile(filePath: string): Promise<PersistedWorkerState> {
  const resolvedPath = resolveHome(filePath);
  try {
    const raw = await readFile(resolvedPath, "utf-8");
    if (!raw.trim()) {
      return { workers: {}, crashHistory: {} };
    }
    return normalizeState(JSON.parse(raw));
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") {
      return { workers: {}, crashHistory: {} };
    }
    throw error;
  }
}

export async function writeStateFile(filePath: string, state: PersistedWorkerState): Promise<void> {
  const resolvedPath = resolveHome(filePath);
  const dir = path.dirname(resolvedPath);
  await mkdir(dir, { recursive: true });

  const tempPath = `${resolvedPath}.tmp-${process.pid}-${Date.now()}`;
  const payload = JSON.stringify(state, null, 2);
  await writeFile(tempPath, payload, "utf-8");
  await rename(tempPath, resolvedPath);
}
