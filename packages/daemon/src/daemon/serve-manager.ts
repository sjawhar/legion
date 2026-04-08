import { mkdirSync, openSync } from "node:fs";
import { join } from "node:path";
import { createOpencodeClient, type OpencodeClient } from "@opencode-ai/sdk/v2";
import { isPortFree } from "./ports";
import { HealthCheckResponseSchema, SessionCreateResponseSchema } from "./schemas";

interface SharedServeState {
  port: number;
  pid: number;
  status: "starting" | "running" | "dead";
}

interface SharedServeOptions {
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
  version?: number;
  envoyTopics?: string[];
  repo?: string;
  issueNumber?: number;
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

export async function waitForHealthy(port: number, maxRetries = 90, delayMs = 500): Promise<void> {
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
): Promise<string> {
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
    let body: unknown;
    try {
      body = await res.json();
    } catch {
      throw new Error(`createSession: response was not valid JSON (status ${res.status})`);
    }
    const parsed = SessionCreateResponseSchema.safeParse(body);
    if (!parsed.success) {
      throw new Error(
        `createSession: response body missing or invalid id field: ${JSON.stringify(body)}`
      );
    }
    if (parsed.data.id !== sessionId) {
      console.warn(
        `createSession: session ID mismatch: requested=${sessionId} actual=${parsed.data.id}`
      );
    }
    return parsed.data.id;
  }
  const body = (await res.json().catch(() => ({}))) as Record<string, unknown>;
  if (res.status === 409 && body.name === "DuplicateIDError") {
    if (typeof body.id === "string") {
      if (body.id !== sessionId) {
        console.warn(
          `createSession: 409 session ID mismatch: requested=${sessionId} actual=${body.id}`
        );
      }
      return body.id;
    }
    return sessionId;
  }
  throw new Error(`Failed to create session ${sessionId}: ${JSON.stringify(body)}`);
}

/**
 * Delete a session from the shared serve, releasing its resources (SQLite FDs, memory).
 * Best-effort: failures are logged but never propagated to callers.
 */
export async function deleteSession(port: number, sessionId: string): Promise<void> {
  try {
    const res = await fetch(`http://127.0.0.1:${port}/session/${sessionId}`, {
      method: "DELETE",
      signal: AbortSignal.timeout(10_000),
    });
    if (!res.ok) {
      console.warn(`deleteSession: non-OK response for ${sessionId}: ${res.status}`);
    }
  } catch (error) {
    console.warn(`deleteSession: failed for ${sessionId}:`, error);
  }
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
    const data = HealthCheckResponseSchema.safeParse(await response.json().catch(() => ({})));
    if (!data.success) {
      return false;
    }
    return data.data.healthy === true;
  } catch {
    return false;
  }
}

/**
 * Kill a stale serve process on a given port.
 * Used during daemon startup to clean up orphaned serves from previous runs.
 *
 * Strategy:
 *   1. If port is free, return immediately (nothing to clean up)
 *   2. If PID is known, use stopServe() (dispose → poll PID → SIGKILL)
 *   3. If PID is unknown, try dispose endpoint and wait for port to become free
 */
export async function killStaleServe(
  port: number,
  pid?: number,
  waitTimeoutMs = 5000,
  pollIntervalMs = 200,
  disposeTimeoutMs = 3000
): Promise<boolean> {
  if (await isPortFree(port)) {
    return true;
  }

  console.log(`Cleaning up stale serve on port ${port}${pid ? ` (PID ${pid})` : ""}...`);

  // If we have the PID, use the existing stopServe flow
  if (pid) {
    try {
      await stopServe(port, pid, waitTimeoutMs, pollIntervalMs, disposeTimeoutMs);
      console.log(`Stale serve on port ${port} (PID ${pid}) stopped`);
      return true;
    } catch (error) {
      console.warn(`stopServe failed for stale PID ${pid}: ${error}`);
      // Fall through to dispose-only path
    }
  }

  // No PID or PID-based cleanup failed — try dispose endpoint directly
  try {
    await fetch(`http://127.0.0.1:${port}/global/dispose`, {
      method: "POST",
      signal: AbortSignal.timeout(disposeTimeoutMs),
    });
  } catch {
    // Dispose is best-effort
  }

  // Wait for port to become free
  const deadline = Date.now() + waitTimeoutMs;
  while (Date.now() < deadline) {
    if (await isPortFree(port)) {
      console.log(`Stale serve on port ${port} cleaned up via dispose`);
      return true;
    }
    await new Promise((resolve) => setTimeout(resolve, pollIntervalMs));
  }

  console.warn(`Failed to clean up stale serve on port ${port} — port still occupied`);
  return false;
}
