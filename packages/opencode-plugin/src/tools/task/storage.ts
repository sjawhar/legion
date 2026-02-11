import { randomUUID } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  renameSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import { basename, dirname, isAbsolute, join } from "node:path";
import type { z } from "zod";

/**
 * Read process start time from /proc to guard against PID reuse.
 * Returns null on non-Linux or if the proc file is unreadable.
 */
function getProcessStartTime(pid: number): string | null {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf-8");
    const afterComm = stat.indexOf(") ");
    if (afterComm === -1) return null;
    const fields = stat.substring(afterComm + 2).split(" ");
    return fields[19] ?? null;
  } catch {
    return null;
  }
}

const STALE_LOCK_THRESHOLD_MS = 30_000;

export function sanitizePathSegment(value: string): string {
  return value.replace(/[^a-zA-Z0-9_-]/g, "-") || "default";
}

export function resolveTaskListId(): string {
  const envId = process.env.OPENCODE_TASK_LIST_ID?.trim();
  if (envId) return sanitizePathSegment(envId);
  const claudeEnvId = process.env.CLAUDE_CODE_TASK_LIST_ID?.trim();
  if (claudeEnvId) return sanitizePathSegment(claudeEnvId);
  return sanitizePathSegment(basename(process.cwd()));
}

export function getTaskDir(listId?: string): string {
  if (listId && isAbsolute(listId)) return listId;
  const resolved = listId ?? resolveTaskListId();
  return join(homedir(), ".config", "opencode", "tasks", resolved);
}

export function ensureDir(dirPath: string): void {
  if (!existsSync(dirPath)) {
    mkdirSync(dirPath, { recursive: true });
  }
}

export function readJsonSafe<T>(filePath: string, schema: z.ZodType<T>): T | null {
  try {
    if (!existsSync(filePath)) {
      return null;
    }
    const content = readFileSync(filePath, "utf-8");
    const parsed = JSON.parse(content);
    const result = schema.safeParse(parsed);
    if (!result.success) {
      return null;
    }
    return result.data;
  } catch {
    return null;
  }
}

export function writeJsonAtomic(filePath: string, data: unknown): void {
  const dir = dirname(filePath);
  ensureDir(dir);

  const tempPath = `${filePath}.tmp.${Date.now()}`;
  try {
    writeFileSync(tempPath, JSON.stringify(data, null, 2), "utf-8");
    renameSync(tempPath, filePath);
  } catch (error) {
    try {
      if (existsSync(tempPath)) {
        unlinkSync(tempPath);
      }
    } catch {
      // Ignore cleanup errors
    }
    throw error;
  }
}

export function generateTaskId(): string {
  return `T-${randomUUID()}`;
}

export function listTaskFiles(taskDir: string): string[] {
  if (!existsSync(taskDir)) return [];
  return readdirSync(taskDir)
    .filter((f) => f.endsWith(".json") && f.startsWith("T-"))
    .map((f) => f.replace(".json", ""));
}

interface Lock {
  acquired: boolean;
  release: () => void;
}

export function acquireLock(dirPath: string): Lock {
  const lockPath = join(dirPath, ".lock");
  const lockId = randomUUID();
  const startTime = getProcessStartTime(process.pid);

  const createLockExclusive = (timestamp: number) => {
    writeFileSync(
      lockPath,
      JSON.stringify({ id: lockId, timestamp, pid: process.pid, startTime }),
      {
        encoding: "utf-8",
        flag: "wx",
      }
    );
  };

  const isStale = () => {
    try {
      const lockContent = readFileSync(lockPath, "utf-8");
      const lockData = JSON.parse(lockContent);
      const lockAge = Date.now() - lockData.timestamp;
      if (lockAge <= STALE_LOCK_THRESHOLD_MS) return false;

      if (typeof lockData.pid === "number") {
        try {
          process.kill(lockData.pid, 0);
        } catch (err: unknown) {
          if (err && typeof err === "object" && "code" in err) {
            return (err as { code: string }).code === "ESRCH";
          }
          return true;
        }

        if (lockData.startTime != null) {
          const currentStartTime = getProcessStartTime(lockData.pid);
          if (currentStartTime !== null && currentStartTime !== lockData.startTime) {
            return true;
          }
        }

        return false;
      }

      return true;
    } catch {
      return true;
    }
  };

  const tryAcquire = () => {
    try {
      createLockExclusive(Date.now());
      return true;
    } catch (error) {
      if (error && typeof error === "object" && "code" in error && error.code === "EEXIST") {
        return false;
      }
      throw error;
    }
  };

  const tryReclaimStale = () => {
    const tempLockPath = `${lockPath}.${lockId}`;
    const now = Date.now();
    try {
      writeFileSync(
        tempLockPath,
        JSON.stringify({ id: lockId, timestamp: now, pid: process.pid, startTime }),
        "utf-8"
      );
      renameSync(tempLockPath, lockPath);
      const verification = readFileSync(lockPath, "utf-8");
      const verifiedData = JSON.parse(verification);
      return verifiedData.id === lockId;
    } catch {
      try {
        if (existsSync(tempLockPath)) unlinkSync(tempLockPath);
      } catch {
        // Ignore cleanup errors
      }
      return false;
    }
  };

  ensureDir(dirPath);

  let acquired = tryAcquire();
  if (!acquired && isStale()) {
    acquired = tryReclaimStale();
  }

  if (!acquired) {
    return { acquired: false, release: () => {} };
  }

  return {
    acquired: true,
    release: () => {
      try {
        if (!existsSync(lockPath)) return;
        const lockContent = readFileSync(lockPath, "utf-8");
        const lockData = JSON.parse(lockContent);
        if (lockData.id !== lockId) return;
        unlinkSync(lockPath);
      } catch {
        // Ignore cleanup errors
      }
    },
  };
}
