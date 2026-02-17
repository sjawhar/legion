import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { PersistedWorkerStateSchema } from "./schemas";
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

async function moveCorruptFile(filePath: string): Promise<void> {
  const timestamp = new Date().toISOString().replace(/[:.]/g, "-");
  const corruptPath = `${filePath}.corrupt.${timestamp}`;
  try {
    await rename(filePath, corruptPath);
  } catch (err) {
    console.warn(`[state-file] Failed to rename corrupt file ${filePath}:`, err);
  }
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
    } else if (legacyController && isRecord(legacyController)) {
      if (typeof legacyController.sessionId === "string") {
        result.controller = {
          sessionId: legacyController.sessionId,
          port: typeof legacyController.port === "number" ? legacyController.port : undefined,
          pid: typeof legacyController.pid === "number" ? legacyController.pid : undefined,
        };
      }
    }

    return result;
  }

  return { workers: {}, crashHistory: {} };
}

export async function readStateFile(filePath: string): Promise<PersistedWorkerState> {
  const resolvedPath = resolveHome(filePath);
  try {
    const raw = await readFile(resolvedPath, "utf-8");
    if (!raw.trim()) {
      return { workers: {}, crashHistory: {}, controller: undefined };
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      console.warn(`[state-file] Corrupt JSON in ${resolvedPath}, recovering to empty state`);
      await moveCorruptFile(resolvedPath);
      return { workers: {}, crashHistory: {} };
    }

    const normalized = normalizeState(parsed);
    const validation = PersistedWorkerStateSchema.safeParse(normalized);

    if (!validation.success) {
      const issues = validation.error.issues.map((i) => i.message).join(", ");
      console.warn(`[state-file] Schema validation failed for ${resolvedPath}: ${issues}`);
      await moveCorruptFile(resolvedPath);
      return { workers: {}, crashHistory: {} };
    }

    return validation.data;
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
