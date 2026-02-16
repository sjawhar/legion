import { mkdirSync, openSync } from "node:fs";
import { join } from "node:path";
import { createOpencodeClient, type OpencodeClient } from "@opencode-ai/sdk/v2";

export interface SharedServeState {
  port: number;
  pid: number;
  status: "starting" | "running" | "dead";
}

export interface SharedServeOptions {
  port: number;
  workspace: string;
  logDir?: string;
  env?: Record<string, string>;
}

export interface WorkerEntry {
  id: string;
  port: number;
  pid?: number;
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

export async function spawnSharedServe(opts: SharedServeOptions): Promise<SharedServeState> {
  let stderr: "ignore" | number = "ignore";
  if (opts.logDir) {
    mkdirSync(opts.logDir, { recursive: true });
    const logFile = join(opts.logDir, "shared-serve.stderr.log");
    stderr = openSync(logFile, "a");
  }

  const { OPENCODE_PERMISSION: _, ...baseEnv } = process.env;
  const subprocess = Bun.spawn(["opencode", "serve", "--port", String(opts.port)], {
    cwd: opts.workspace,
    env: {
      ...baseEnv,
      ...opts.env,
      SUPERPOWERS_SKIP_BOOTSTRAP: "1",
    },
    stdio: ["ignore", "ignore", stderr],
  });

  const pid = subprocess.pid;
  if (pid === undefined) {
    throw new Error("Failed to spawn shared opencode serve process");
  }

  return { port: opts.port, pid, status: "starting" };
}

export async function waitForHealthy(port: number, maxRetries = 30, delayMs = 500): Promise<void> {
  for (let i = 0; i < maxRetries; i++) {
    const healthy = await healthCheck(port);
    if (healthy) {
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, delayMs));
  }
  throw new Error(
    `Shared serve on port ${port} did not become healthy after ${maxRetries} retries`
  );
}

export async function createSession(
  port: number,
  sessionId: string,
  workspace: string
): Promise<void> {
  const baseUrl = `http://127.0.0.1:${port}`;
  const res = await fetch(`${baseUrl}/session`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "x-opencode-directory": encodeURIComponent(workspace),
    },
    body: JSON.stringify({ id: sessionId }),
    signal: AbortSignal.timeout(10_000), // 10s — session creation is a local call
  });
  if (res.ok) {
    return;
  }
  const body = (await res.json().catch(() => ({}))) as Record<string, unknown>;
  if (res.status === 409 || body.name === "DuplicateIDError") {
    return;
  }
  throw new Error(`Failed to create session ${sessionId}: ${JSON.stringify(body)}`);
}

export async function stopServe(
  port: number,
  pid: number,
  waitTimeoutMs = 5000,
  pollIntervalMs = 200,
  disposeTimeoutMs = 3000
): Promise<void> {
  try {
    await fetch(`http://127.0.0.1:${port}/global/dispose`, {
      method: "POST",
      signal: AbortSignal.timeout(disposeTimeoutMs),
    });
  } catch {
    // Dispose is best-effort; proceed to poll + SIGKILL
  }

  const deadline = Date.now() + waitTimeoutMs;
  while (Date.now() < deadline) {
    try {
      process.kill(pid, 0);
    } catch {
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, pollIntervalMs));
  }

  try {
    process.kill(pid, "SIGKILL");
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
