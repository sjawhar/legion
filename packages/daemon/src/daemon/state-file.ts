import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { WorkerEntry } from "./serve-manager";

export interface CrashHistoryEntry {
  crashCount: number;
  lastCrashAt: string | null;
}

export interface ControllerState {
  sessionId: string;
  port?: number;
  pid?: number;
}

export interface PersistedWorkerState {
  workers: Record<string, WorkerEntry>;
  crashHistory: Record<string, CrashHistoryEntry>;
  controller?: ControllerState;
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
    const workers = raw.workers as Record<string, WorkerEntry>;
    const legacyController = workers["controller-controller"];
    delete workers["controller-controller"];

    const crashHistory = isRecord(raw.crashHistory)
      ? (raw.crashHistory as Record<string, CrashHistoryEntry>)
      : {};
    delete crashHistory["controller-controller"];

    const result: PersistedWorkerState = { workers, crashHistory };

    if (isRecord(raw.controller)) {
      const ctrl = raw.controller as Record<string, unknown>;
      if (typeof ctrl.sessionId === "string") {
        result.controller = {
          sessionId: ctrl.sessionId,
          port: typeof ctrl.port === "number" ? ctrl.port : undefined,
          pid: typeof ctrl.pid === "number" ? ctrl.pid : undefined,
        };
      }
    } else if (legacyController) {
      result.controller = {
        sessionId: legacyController.sessionId,
        port: legacyController.port,
        pid: legacyController.pid,
      };
    }

    return result;
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
      return { workers: {}, crashHistory: {}, controller: undefined };
    }
    return normalizeState(JSON.parse(raw));
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") {
      return { workers: {}, crashHistory: {}, controller: undefined };
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
