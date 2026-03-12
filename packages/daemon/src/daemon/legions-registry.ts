import { closeSync, constants, openSync, readFileSync, writeSync } from "node:fs";
import { copyFile, mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
import path from "node:path";
import { LegionsRegistrySchema } from "./schemas";

export interface LegionEntry {
  port: number;
  servePort: number;
  pid: number;
  startedAt: string;
}

export type LegionsRegistry = Record<string, LegionEntry>;

const BASE_DAEMON_PORT = 13370;
const BASE_SERVE_PORT = 13381;

export function isPidAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
  } catch {
    return false;
  }
  // On Linux, verify the PID belongs to a Legion/Bun process via /proc/cmdline
  if (process.platform === "linux") {
    try {
      const cmdline = readFileSync(`/proc/${pid}/cmdline`, "utf-8");
      return cmdline.includes("legion") || cmdline.includes("bun");
    } catch {
      // /proc read failed — fall back gracefully (treat as alive)
    }
  }
  return true;
}

export async function readLegionsRegistry(filePath: string): Promise<LegionsRegistry> {
  try {
    const raw = await readFile(filePath, "utf-8");
    if (!raw.trim()) {
      return {};
    }

    const parsed = JSON.parse(raw);
    const result = LegionsRegistrySchema.safeParse(parsed);
    if (!result.success) {
      console.warn(
        `[legions-registry] Schema validation failed for ${filePath}, backing up and resetting. Error: ${result.error.message}`
      );
      try {
        await copyFile(filePath, `${filePath}.bak.${Date.now()}`);
      } catch {
        // backup failure is non-fatal
      }
      return {};
    }
    return result.data;
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") {
      return {};
    }
    throw error;
  }
}

export async function writeLegionEntry(
  filePath: string,
  projectId: string,
  entry: LegionEntry
): Promise<void> {
  await withRegistryLock(filePath, async () => {
    const registry = await readLegionsRegistry(filePath);
    registry[projectId] = entry;
    await writeRegistry(filePath, registry);
  });
}

export async function removeLegionEntry(filePath: string, projectId: string): Promise<void> {
  await withRegistryLock(filePath, async () => {
    const registry = await readLegionsRegistry(filePath);
    delete registry[projectId];
    await writeRegistry(filePath, registry);
  });
}

export function allocatePort(registry: LegionsRegistry): { daemonPort: number; servePort: number } {
  const usedDaemonPorts = new Set<number>();
  const usedServePorts = new Set<number>();

  for (const entry of Object.values(registry)) {
    if (isPidAlive(entry.pid)) {
      usedDaemonPorts.add(entry.port);
      usedServePorts.add(entry.servePort);
    }
  }

  let daemonPort = BASE_DAEMON_PORT;
  while (usedDaemonPorts.has(daemonPort)) {
    daemonPort++;
  }

  let servePort = BASE_SERVE_PORT;
  while (usedServePorts.has(servePort)) {
    servePort++;
  }

  return { daemonPort, servePort };
}

export async function findLegionByProjectId(
  filePath: string,
  projectId: string
): Promise<LegionEntry | undefined> {
  const registry = await readLegionsRegistry(filePath);
  return registry[projectId];
}

async function writeRegistry(filePath: string, registry: LegionsRegistry): Promise<void> {
  const dir = path.dirname(filePath);
  await mkdir(dir, { recursive: true });
  const tempPath = `${filePath}.tmp-${process.pid}-${Date.now()}`;
  await writeFile(tempPath, JSON.stringify(registry, null, 2), "utf-8");
  await rename(tempPath, filePath);
}

export async function withRegistryLock<T>(filePath: string, fn: () => Promise<T>): Promise<T> {
  const lockPath = `${filePath}.lock`;
  const start = Date.now();
  let delayMs = 50;

  while (Date.now() - start < 3000) {
    // Atomic lock acquisition via O_CREAT | O_EXCL (fails if file exists)
    let acquired = false;
    try {
      const fd = openSync(lockPath, constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY);
      writeSync(fd, JSON.stringify({ pid: process.pid, timestamp: Date.now() }));
      closeSync(fd);
      acquired = true;
    } catch (error) {
      const err = error as NodeJS.ErrnoException;
      if (err.code !== "EEXIST") {
        throw error;
      }
    }

    if (acquired) {
      try {
        return await fn();
      } finally {
        await unlink(lockPath).catch(() => {});
      }
    }

    // Lock exists — check if stale
    try {
      const raw = await readFile(lockPath, "utf-8");
      const parsed = JSON.parse(raw) as { pid?: unknown };
      if (typeof parsed.pid === "number" && !isPidAlive(parsed.pid)) {
        await unlink(lockPath).catch(() => {});
        continue;
      }
    } catch {
      // Lock file unreadable or gone — retry
      continue;
    }

    await new Promise((resolve) => setTimeout(resolve, delayMs));
    delayMs = Math.min(delayMs * 2, 400);
  }

  throw new Error(`Timed out acquiring registry lock: ${lockPath}`);
}
