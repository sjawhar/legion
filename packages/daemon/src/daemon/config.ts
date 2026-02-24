import os from "node:os";
import path from "node:path";

export interface DaemonConfig {
  daemonPort: number;
  teamId?: string;
  legionDir?: string;
  checkIntervalMs: number;
  baseWorkerPort: number;
  stateFilePath: string;
  logDir: string;
  controllerSessionId?: string;
  controllerPrompt?: string;
  issueBackend: "linear" | "github";
  runtime: "opencode" | "claude-code";
}

const DEFAULT_DAEMON_PORT = 13370;
const DEFAULT_CHECK_INTERVAL_MS = 60_000;
const DEFAULT_BASE_WORKER_PORT = 13381;

function parseNumber(value: string | undefined, fallback: number): number {
  if (!value) {
    return fallback;
  }
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) {
    return fallback;
  }
  return parsed;
}

function resolveStateFilePath(legionDir?: string): string {
  const baseDir = legionDir ?? os.homedir();
  return path.join(baseDir, ".legion", "daemon", "workers.json");
}

export function validateControllerPrompt(prompt: string | undefined): void {
  if (!prompt) {
    return;
  }
  if (prompt.length > 10000) {
    throw new Error(
      `Controller prompt exceeds maximum length of 10000 characters (got ${prompt.length})`
    );
  }
  const hasControlChars = [...prompt].some((ch) => {
    const code = ch.charCodeAt(0);
    return (
      (code >= 0 && code <= 8) ||
      code === 11 ||
      code === 12 ||
      (code >= 14 && code <= 31) ||
      code === 127
    );
  });
  if (hasControlChars) {
    throw new Error("Controller prompt contains invalid control characters");
  }
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): DaemonConfig {
  const legionDir = env.LEGION_DIR;
  const stateFilePath = resolveStateFilePath(legionDir);
  const stateDir = path.dirname(stateFilePath);
  const controllerSessionId = env.LEGION_CONTROLLER_SESSION_ID || undefined;
  const controllerPrompt = env.LEGION_CONTROLLER_PROMPT || undefined;

  if (controllerSessionId && !controllerSessionId.startsWith("ses_")) {
    throw new Error(
      `LEGION_CONTROLLER_SESSION_ID must start with 'ses_' (got: ${controllerSessionId})`
    );
  }

  validateControllerPrompt(controllerPrompt);

  const rawBackend = env.LEGION_ISSUE_BACKEND;
  if (rawBackend !== undefined && rawBackend !== "linear" && rawBackend !== "github") {
    throw new Error(`LEGION_ISSUE_BACKEND must be 'linear' or 'github' (got: ${rawBackend})`);
  }
  const issueBackend = rawBackend === "github" ? "github" : "linear";


  const rawRuntime = env.LEGION_RUNTIME;
  if (rawRuntime !== undefined && rawRuntime !== "opencode" && rawRuntime !== "claude-code") {
    throw new Error(`LEGION_RUNTIME must be 'opencode' or 'claude-code' (got: ${rawRuntime})`);
  }
  const runtime = rawRuntime === "claude-code" ? "claude-code" : "opencode";
  return {
    daemonPort: parseNumber(env.LEGION_DAEMON_PORT, DEFAULT_DAEMON_PORT),
    teamId: env.LEGION_TEAM_ID,
    legionDir,
    checkIntervalMs: DEFAULT_CHECK_INTERVAL_MS,
    baseWorkerPort: DEFAULT_BASE_WORKER_PORT,
    stateFilePath,
    logDir: path.join(stateDir, "logs"),
    controllerSessionId,
    controllerPrompt,
    issueBackend,
    runtime,
  };
}
