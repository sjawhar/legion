import os from "node:os";
import path from "node:path";
import type { LegionPaths } from "./paths";
import { resolveLegionPaths } from "./paths";

export type GitHubAppRole = "impl" | "review";

export interface GitHubAppRoleConfig {
  appId: string;
  privateKeyPath: string;
  installationId: string;
}

export type GitHubAppsConfig = Partial<Record<GitHubAppRole, GitHubAppRoleConfig>>;

export interface DaemonConfig {
  daemonPort: number;
  legionId?: string;
  legionDir?: string;
  paths: LegionPaths;
  checkIntervalMs: number;
  baseWorkerPort: number;
  stateFilePath: string;
  logDir: string;
  controllerSessionId?: string;
  controllerPrompt?: string;
  issueBackend: "linear" | "github";
  extraProjects?: string[];
  runtime: "opencode" | "claude-code";
  githubApps?: GitHubAppsConfig;
  /** RSS threshold in bytes; serve restarts when exceeded. 0 = disabled. */
  maxRssBytes: number;
  /** Minimum interval between RSS checks in ms. */
  rssCheckIntervalMs: number;
}

const BASE_DAEMON_PORT = 13370;
const DEFAULT_CHECK_INTERVAL_MS = 60_000;
const DEFAULT_BASE_WORKER_PORT = 13381;
const DEFAULT_MAX_RSS_GB = 20;
const DEFAULT_RSS_CHECK_INTERVAL_S = 60;
const EXTRA_PROJECT_PATTERN = /^[^/]+\/\d+$/;

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

function parseExtraProjects(value: string | undefined): string[] | undefined {
  if (value === undefined || value === "") {
    return undefined;
  }

  const extraProjects: string[] = [];
  const seen = new Set<string>();

  for (const rawEntry of value.split(",")) {
    const entry = rawEntry.trim();
    if (!entry) {
      continue;
    }
    if (!EXTRA_PROJECT_PATTERN.test(entry)) {
      throw new Error(`LEGION_EXTRA_PROJECTS entries must match owner/number (got: ${entry})`);
    }
    if (!seen.has(entry)) {
      seen.add(entry);
      extraProjects.push(entry);
    }
  }

  return extraProjects.length > 0 ? extraProjects : undefined;
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): DaemonConfig {
  const legionDir = env.LEGION_DIR;
  const legionId = env.LEGION_ID;
  const paths = resolveLegionPaths(env, os.homedir());
  const stateFilePath = legionId
    ? paths.forLegion(legionId).workersFile
    : path.join(paths.stateDir, "daemon", "workers.json");
  const logDir = legionId
    ? paths.forLegion(legionId).logDir
    : path.join(paths.stateDir, "daemon", "logs");
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
  const githubApps = loadGitHubApps(env);
  const extraProjects = parseExtraProjects(env.LEGION_EXTRA_PROJECTS);
  const maxRssGb = parseNumber(env.OPENCODE_MAX_RSS_GB, DEFAULT_MAX_RSS_GB);
  const maxRssBytes = maxRssGb > 0 ? maxRssGb * 1024 * 1024 * 1024 : 0;
  const rssCheckIntervalS = parseNumber(
    env.OPENCODE_RSS_CHECK_INTERVAL,
    DEFAULT_RSS_CHECK_INTERVAL_S
  );
  const rssCheckIntervalMs =
    rssCheckIntervalS > 0 ? rssCheckIntervalS * 1000 : DEFAULT_RSS_CHECK_INTERVAL_S * 1000;

  return {
    daemonPort: parseNumber(env.LEGION_DAEMON_PORT, BASE_DAEMON_PORT),
    legionId,
    legionDir,
    paths,
    checkIntervalMs: DEFAULT_CHECK_INTERVAL_MS,
    baseWorkerPort: DEFAULT_BASE_WORKER_PORT,
    maxRssBytes,
    rssCheckIntervalMs,
    stateFilePath,
    logDir,
    controllerSessionId,
    controllerPrompt,
    issueBackend,
    extraProjects,
    runtime,
    githubApps,
  };
}

const GITHUB_APP_ROLES: GitHubAppRole[] = ["impl", "review"];

function loadGitHubApps(env: NodeJS.ProcessEnv): GitHubAppsConfig | undefined {
  const config: GitHubAppsConfig = {};
  let hasAny = false;

  for (const role of GITHUB_APP_ROLES) {
    const prefix = `LEGION_GITHUB_APP_${role.toUpperCase()}`;
    const appId = env[`${prefix}_ID`];
    const privateKeyPath = env[`${prefix}_PRIVATE_KEY_PATH`];
    const installationId = env[`${prefix}_INSTALLATION_ID`];

    if (appId && privateKeyPath && installationId) {
      config[role] = { appId, privateKeyPath, installationId };
      hasAny = true;
    }
  }

  return hasAny ? config : undefined;
}
