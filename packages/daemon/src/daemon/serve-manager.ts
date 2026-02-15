import { mkdirSync, openSync } from "node:fs";
import { join } from "node:path";
import { createOpencodeClient, type OpencodeClient } from "@opencode-ai/sdk/v2";
import { type CrashHistoryEntry, readStateFile } from "./state-file";

export interface SpawnOptions {
  issueId: string;
  mode: string;
  workspace: string;
  port: number;
  sessionId: string;
  logDir?: string;
  env?: Record<string, string>;
}

export interface WorkerEntry {
  id: string;
  port: number;
  pid: number;
  sessionId: string;
  workspace: string;
  startedAt: string;
  status: "starting" | "running" | "stopped" | "dead";
  crashCount: number;
  lastCrashAt: string | null;
}

export function createWorkerClient(port: number, workspace: string): OpencodeClient {
  return createOpencodeClient({
    baseUrl: `http://127.0.0.1:${port}`,
    directory: workspace,
  });
}

const DENIED_SKILLS_BY_MODE: Record<string, string[]> = {
  architect: ["superpowers/writing-plans", "superpowers/executing-plans"],
  plan: ["superpowers/brainstorming", "superpowers/executing-plans"],
  implement: ["superpowers/brainstorming", "superpowers/writing-plans"],
  review: ["superpowers/brainstorming", "superpowers/writing-plans", "superpowers/executing-plans"],
  merge: [],
};

export async function spawnServe(opts: SpawnOptions): Promise<WorkerEntry> {
  let stderr: "ignore" | number = "ignore";
  if (opts.logDir) {
    mkdirSync(opts.logDir, { recursive: true });
    const logFile = join(opts.logDir, `${opts.issueId}-${opts.mode}.stderr.log`);
    stderr = openSync(logFile, "a");
  }

  const deniedSkills = DENIED_SKILLS_BY_MODE[opts.mode] ?? [];
  const skillPermissions: Record<string, string> = {};
  for (const skill of deniedSkills) {
    skillPermissions[skill] = "deny";
  }
  const permissionEnv =
    Object.keys(skillPermissions).length > 0
      ? { OPENCODE_PERMISSION: JSON.stringify({ skill: skillPermissions }) }
      : {};

  const { OPENCODE_PERMISSION: _, ...baseEnv } = process.env;
  const subprocess = Bun.spawn(["opencode", "serve", "--port", String(opts.port)], {
    cwd: opts.workspace,
    env: {
      ...baseEnv,
      ...opts.env,
      SUPERPOWERS_SKIP_BOOTSTRAP: "1",
      ...permissionEnv,
    },
    stdio: ["ignore", "ignore", stderr],
  });

  const pid = subprocess.pid;
  if (pid === undefined) {
    throw new Error("Failed to spawn opencode serve process");
  }

  return {
    id: `${opts.issueId}-${opts.mode}`.toLowerCase(),
    port: opts.port,
    pid,
    sessionId: opts.sessionId,
    workspace: opts.workspace,
    startedAt: new Date().toISOString(),
    status: "starting",
    crashCount: 0,
    lastCrashAt: null,
  };
}

export async function initializeSession(
  port: number,
  sessionId: string,
  workspace: string,
  maxRetries = 30,
  delayMs = 500
): Promise<void> {
  for (let i = 0; i < maxRetries; i++) {
    const healthy = await healthCheck(port);
    if (healthy) {
      const client = createWorkerClient(port, workspace);
      const { error } = await client.session.create({ id: sessionId }, { throwOnError: false });
      if (error) {
        if ("name" in error && error.name === "DuplicateIDError") {
          return;
        }
        throw new Error(`Failed to create session ${sessionId}: ${JSON.stringify(error)}`);
      }
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, delayMs));
  }
  throw new Error(
    `OpenCode serve on port ${port} did not become healthy after ${maxRetries} retries`
  );
}

export async function killWorker(entry: WorkerEntry): Promise<void> {
  try {
    process.kill(entry.pid, "SIGTERM");
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ESRCH") {
      return;
    }
    throw error;
  }
}

export async function healthCheck(port: number, timeoutMs = 5000): Promise<boolean> {
  try {
    const response = await fetch(`http://127.0.0.1:${port}/global/health`, {
      signal: AbortSignal.timeout(timeoutMs),
    });
    if (!response.ok) {
      return false;
    }
    const data = (await response.json()) as { healthy?: boolean };
    return data.healthy === true;
  } catch {
    return false;
  }
}

export interface AdoptedWorkers {
  workers: Map<string, WorkerEntry>;
  crashHistory: Record<string, CrashHistoryEntry>;
}

export async function adoptExistingWorkers(stateFilePath: string): Promise<AdoptedWorkers> {
  const state = await readStateFile(stateFilePath);
  const entries = Object.entries(state.workers);
  const results = await Promise.all(
    entries.map(async ([id, entry]) => ({
      id,
      entry,
      healthy: await healthCheck(entry.port),
    }))
  );

  const adopted = new Map<string, WorkerEntry>();
  for (const result of results) {
    if (result.healthy) {
      const normalizedId = result.id.toLowerCase();
      adopted.set(normalizedId, { ...result.entry, id: normalizedId, status: "running" });
    }
  }
  return { workers: adopted, crashHistory: state.crashHistory };
}
