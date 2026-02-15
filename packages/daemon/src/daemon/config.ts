import os from "node:os";
import path from "node:path";

export interface DaemonConfig {
  daemonPort: number;
  teamId?: string;
  legionDir?: string;
  shortId?: string;
  checkIntervalMs: number;
  baseWorkerPort: number;
  stateFilePath: string;
  logDir: string;
  controllerSessionId?: string;
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

export function loadConfig(env: NodeJS.ProcessEnv = process.env): DaemonConfig {
  const legionDir = env.LEGION_DIR;
  const stateFilePath = resolveStateFilePath(legionDir);
  const stateDir = path.dirname(stateFilePath);
  const controllerSessionId = env.LEGION_CONTROLLER_SESSION_ID || undefined;

  if (controllerSessionId && !controllerSessionId.startsWith("ses_")) {
    throw new Error(
      `LEGION_CONTROLLER_SESSION_ID must start with 'ses_' (got: ${controllerSessionId})`
    );
  }

  return {
    daemonPort: parseNumber(env.LEGION_DAEMON_PORT, DEFAULT_DAEMON_PORT),
    teamId: env.LEGION_TEAM_ID,
    legionDir,
    shortId: env.LEGION_SHORT_ID,
    checkIntervalMs: DEFAULT_CHECK_INTERVAL_MS,
    baseWorkerPort: DEFAULT_BASE_WORKER_PORT,
    stateFilePath,
    logDir: path.join(stateDir, "logs"),
    controllerSessionId,
  };
}
