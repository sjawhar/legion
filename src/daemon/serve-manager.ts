import { readStateFile } from "./state-file";

export interface SpawnOptions {
  issueId: string;
  mode: string;
  workspace: string;
  port: number;
  sessionId: string;
  env?: Record<string, string>;
}

export interface WorkerEntry {
  id: string;
  port: number;
  pid: number;
  sessionId: string;
  startedAt: string;
  status: "starting" | "running" | "stopped" | "dead";
}

export async function spawnServe(opts: SpawnOptions): Promise<WorkerEntry> {
  const subprocess = Bun.spawn(["opencode", "serve", "--port", String(opts.port)], {
    cwd: opts.workspace,
    env: {
      ...process.env,
      ...opts.env,
    },
    stdio: ["ignore", "ignore", "ignore"],
  });

  const pid = subprocess.pid;
  if (pid === undefined) {
    throw new Error("Failed to spawn opencode serve process");
  }

  return {
    id: `${opts.issueId}-${opts.mode}`,
    port: opts.port,
    pid,
    sessionId: opts.sessionId,
    startedAt: new Date().toISOString(),
    status: "starting",
  };
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

export async function adoptExistingWorkers(
  stateFilePath: string
): Promise<Map<string, WorkerEntry>> {
  const state = await readStateFile(stateFilePath);
  const entries = Object.entries(state);
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
      adopted.set(result.id, { ...result.entry, status: "running" });
    }
  }
  return adopted;
}
