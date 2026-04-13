#!/usr/bin/env bun
import { spawn } from "node:child_process";
import fs from "node:fs";
import { readdir, readFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { defineCommand, runMain } from "citty";
import {
  type DaemonConfig,
  type LoadedConfigFile,
  loadConfigFromFile,
  resolveDaemonConfig,
  validateBackend,
  validateControllerPrompt,
  validateRuntime,
} from "../daemon/config";
import { startDaemon } from "../daemon/index";
import { findLegionByProjectId } from "../daemon/legions-registry";
import { resolveLegionPaths } from "../daemon/paths";
import {
  readAllHandoffs,
  readMessages,
  readPhaseHandoff,
  writeMessage,
  writePhaseHandoff,
} from "../handoff/ledger";
import { HANDOFF_PHASES, isHandoffPhase } from "../handoff/schema";
import type { HandoffPhase } from "../handoff/types";
import { consolidateKnowledge } from "../knowledge/consolidate";
import {
  formatConsolidationReportHuman,
  formatConsolidationReportJson,
} from "../knowledge/reporter";
import { SESSION_ID_PATTERN } from "../state/types";
import { resolveLegionId } from "./legion-resolver";
import { formatPollOutput } from "./poll-formatter";

export class CliError extends Error {
  constructor(
    message: string,
    public code = 1
  ) {
    super(message);
    this.name = "CliError";
  }
}

export function parseEnvJson(raw: string): Record<string, string> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new CliError("Invalid --env value: must be valid JSON");
  }

  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new CliError("Invalid --env value: must be a JSON object");
  }

  const obj = parsed as Record<string, unknown>;
  const result: Record<string, string> = {};
  for (const [key, value] of Object.entries(obj)) {
    if (typeof value !== "string") {
      throw new CliError(`Invalid --env value: key "${key}" must have a string value`);
    }
    result[key] = value;
  }

  return result;
}

async function readStdin(): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of Bun.stdin.stream()) {
    chunks.push(Buffer.from(chunk));
  }
  const text = Buffer.concat(chunks).toString("utf-8").trim();
  if (!text) {
    throw new CliError("No data provided via --data or stdin");
  }
  return text;
}

const DEFAULT_DAEMON_PORT = 13370;
const SAFE_IDENTIFIER_RE = /^[a-zA-Z0-9_-]+$/;

interface LegionInfo {
  id: string;
  name: string;
}

interface WorkerInfo {
  id: string;
  port: number;
}

interface WorkerStatusInfo extends WorkerInfo {
  sessionId: string;
  status: string;
}

interface DispatchOptions {
  // Legacy transition fallback for dispatching without explicit --workspace.
  legionDir?: string;
  daemonPort?: number;
  prompt?: string;
  repo?: string;
  workspace?: string;
  version?: number;
  env?: Record<string, string>;
  issueNumber?: number;
  force?: boolean;
}

interface PromptOptions {
  daemonPort?: number;
  mode?: string;
}

interface ResetCrashesOptions {
  daemonPort?: number;
}

interface DaemonHealth {
  running: boolean;
  workerCount?: number;
  runtime?: string;
  tmuxSession?: string;
}

export type LegionsCache = Record<string, LegionInfo>;

export async function getDaemonPort(projectId?: string): Promise<number> {
  const raw = process.env.LEGION_DAEMON_PORT;
  if (!raw) {
    if (!projectId) {
      return DEFAULT_DAEMON_PORT;
    }

    const paths = resolveLegionPaths(process.env, os.homedir());
    const entry = await findLegionByProjectId(paths.legionsFile, projectId);
    if (entry) {
      return entry.port;
    }
    return DEFAULT_DAEMON_PORT;
  }

  const parsed = Number(raw);
  if (!Number.isFinite(parsed)) {
    return DEFAULT_DAEMON_PORT;
  }
  return parsed;
}

export function loadLegionsCache(
  cacheDir = resolveLegionPaths(process.env, os.homedir()).stateDir
): LegionsCache | null {
  const cacheFile = path.join(cacheDir, "project-cache.json");
  if (!fs.existsSync(cacheFile)) {
    return null;
  }

  const raw = fs.readFileSync(cacheFile, "utf-8");
  if (!raw.trim()) {
    return null;
  }

  const parsed = JSON.parse(raw) as unknown;
  if (!parsed || typeof parsed !== "object") {
    throw new Error(`Invalid legions cache at ${cacheFile}`);
  }

  return parsed as LegionsCache;
}

interface StartOptions {
  workspace?: string;
  prompt?: string;
  backend?: string;
  runtime?: string;
  config?: string;
}

interface StartDependencies {
  startDaemon: typeof startDaemon;
  resolveLegionId: typeof resolveLegionId;
}

export function discoverConfigPath(
  cwd: string,
  env: Record<string, string | undefined>,
  homeDir: string
): string | undefined {
  const xdgConfigHome = env.XDG_CONFIG_HOME;
  if (xdgConfigHome) {
    const xdgConfigPath = path.join(xdgConfigHome, "legion", "legion.yaml");
    if (fs.existsSync(xdgConfigPath)) {
      return xdgConfigPath;
    }
  }

  const homeConfigPath = path.join(homeDir, ".config", "legion", "legion.yaml");
  if (fs.existsSync(homeConfigPath)) {
    return homeConfigPath;
  }

  const localConfigPath = path.join(cwd, "legion.yaml");
  if (fs.existsSync(localConfigPath)) {
    return localConfigPath;
  }

  return undefined;
}

export async function cmdStart(
  team: string | undefined,
  opts: StartOptions,
  deps: StartDependencies = { startDaemon, resolveLegionId }
): Promise<void> {
  validateControllerPrompt(opts.prompt);

  let configFile: LoadedConfigFile | undefined;
  if (opts.config) {
    const configPath = path.resolve(process.cwd(), opts.config);
    let yamlText: string;
    try {
      yamlText = fs.readFileSync(configPath, "utf-8");
    } catch {
      throw new CliError(`Config file not found: ${configPath}`);
    }
    configFile = loadConfigFromFile(yamlText, path.dirname(configPath));
  } else {
    const configPath = discoverConfigPath(process.cwd(), process.env, os.homedir());
    if (configPath) {
      console.log(`Using config: ${configPath}`);
      const yamlText = fs.readFileSync(configPath, "utf-8");
      configFile = loadConfigFromFile(yamlText, path.dirname(configPath));
    }
  }

  const cliOverrides: Partial<DaemonConfig> = {};
  if (opts.workspace) {
    cliOverrides.legionDir = opts.workspace;
  }
  if (opts.prompt !== undefined) {
    cliOverrides.controllerPrompt = opts.prompt;
  }
  if (opts.backend) {
    const validated = validateBackend(opts.backend, "--backend");
    if (validated) cliOverrides.issueBackend = validated;
  }
  if (opts.runtime) {
    const validated = validateRuntime(opts.runtime, "--runtime");
    if (validated) cliOverrides.runtime = validated;
  }
  if (team) {
    cliOverrides.legionId = await deps.resolveLegionId(team, { backend: opts.backend });
  }

  const { config, warnings } = resolveDaemonConfig({
    env: process.env,
    configFile,
    cliOverrides,
  });

  if (!config.legionId) {
    throw new CliError("Missing project: provide positional team arg or 'project' in config file");
  }

  for (const warning of warnings) {
    console.error(`Warning: ${warning}`);
  }

  const instancePaths = config.paths.forLegion(config.legionId);

  console.log(`Starting legion: ${config.legionId}`);
  console.log(`Workspace: ${config.legionDir ?? process.cwd()}`);
  console.log(`State directory: ${instancePaths.legionStateDir}`);

  fs.mkdirSync(instancePaths.legionStateDir, { recursive: true });
  config.stateFilePath = instancePaths.workersFile;

  const handle = await deps.startDaemon(config);

  console.log(`Daemon started on port ${handle.config.daemonPort}`);
  console.log(`\nTo check status: legion status ${team ?? config.legionId}`);
  console.log(`To stop: legion stop ${team ?? config.legionId}`);

  await new Promise(() => {});
}

async function cmdStop(team: string, _stateDir?: string, backend?: string): Promise<void> {
  const legionId = await resolveLegionId(team, { backend });
  const instancePaths = resolveLegionPaths(process.env, os.homedir()).forLegion(legionId);

  console.log(`Stopping legion: ${legionId}`);

  const stateFilePath = instancePaths.workersFile;
  if (!fs.existsSync(stateFilePath)) {
    console.log("No state file found — attempting HTTP shutdown anyway.");
  }

  const daemonPort = await getDaemonPort(legionId);
  try {
    const response = await fetch(`http://127.0.0.1:${daemonPort}/shutdown`, {
      method: "POST",
    });
    if (response.ok) {
      console.log("Daemon stopped successfully.");
    } else {
      console.log("Failed to stop daemon. It may not be running.");
    }
  } catch (_error) {
    console.log("Could not connect to daemon. It may not be running.");
  }
}

async function cmdStatus(team: string, _stateDir?: string, backend?: string): Promise<void> {
  const legionId = await resolveLegionId(team, { backend });
  const instancePaths = resolveLegionPaths(process.env, os.homedir()).forLegion(legionId);

  console.log(`Legion Status: ${legionId}`);
  console.log("=".repeat(40));

  const daemonPort = await getDaemonPort(legionId);
  try {
    const response = await fetch(`http://127.0.0.1:${daemonPort}/workers`);
    if (response.ok) {
      const workers = (await response.json()) as WorkerInfo[];
      console.log(`Daemon: RUNNING (port ${daemonPort})`);
      console.log(`Workers: ${workers.length}`);
      for (const worker of workers) {
        console.log(`  - ${worker.id} (port ${worker.port})`);
      }
    } else {
      console.log("Daemon: NOT RUNNING");
    }
  } catch (_error) {
    console.log("Daemon: NOT RUNNING");
  }

  const stateFilePath = instancePaths.workersFile;
  if (fs.existsSync(stateFilePath)) {
    const stat = fs.statSync(stateFilePath);
    const age = Math.floor((Date.now() - stat.mtimeMs) / 1000);
    console.log(`\nState file: ${stateFilePath}`);
    console.log(`Last updated: ${age}s ago`);
  } else {
    console.log(`\nState file: NOT FOUND`);
  }
}

async function cmdAttach(team: string, issue: string, backend?: string): Promise<void> {
  const legionId = await resolveLegionId(team, { backend });

  console.log(`Attaching to worker for issue: ${issue}`);
  console.log(`Legion: ${legionId}`);

  const daemonPort = await getDaemonPort(legionId);
  try {
    const response = await fetch(`http://127.0.0.1:${daemonPort}/workers`);
    if (!response.ok) {
      throw new CliError("Could not connect to daemon. Is it running?");
    }

    const workers = (await response.json()) as Array<{ id: string; port: number }>;
    const normalizedIssue = issue.toLowerCase();
    const matches = workers.filter(
      (worker) => worker.id === normalizedIssue || worker.id.startsWith(`${normalizedIssue}-`)
    );

    if (matches.length === 0) {
      let msg = `No worker found for issue: ${issue}`;
      msg += "\n\nAvailable workers:";
      for (const worker of workers) {
        msg += `\n  - ${worker.id}`;
      }
      throw new CliError(msg);
    }

    if (matches.length > 1) {
      let msg = `Multiple workers found for ${issue}:`;
      for (const worker of matches) {
        msg += `\n  - ${worker.id} (port ${worker.port})`;
      }
      msg += "\nBe more specific, e.g.: legion attach eng-21-implement";
      throw new CliError(msg);
    }

    const worker = matches[0];
    // Check runtime from daemon health
    const health = await checkDaemonHealth(daemonPort);

    if (health.runtime === "claude-code" && health.tmuxSession) {
      console.log(`Found worker: ${worker.id}`);
      console.log(`Attaching with: tmux attach -t ${health.tmuxSession}`);

      const child = spawn("tmux", ["attach", "-t", health.tmuxSession], {
        stdio: "inherit",
      });

      child.on("exit", (code) => {
        process.exit(code ?? 0);
      });
    } else {
      console.log(`Found worker on port ${worker.port}`);
      console.log(`Attaching with: opencode attach http://localhost:${worker.port}`);
      const child = spawn("opencode", ["attach", `http://localhost:${worker.port}`], {
        stdio: "inherit",
      });
      child.on("exit", (code) => {
        process.exit(code ?? 0);
      });
    }
  } catch (error) {
    if (error instanceof CliError) {
      throw error;
    }
    const message = error instanceof Error ? error.message : String(error);
    throw new CliError(`Failed to attach: ${message}`);
  }
}

export async function cmdDispatch(
  issue: string,
  mode: string,
  opts: DispatchOptions
): Promise<void> {
  if (!SAFE_IDENTIFIER_RE.test(issue)) {
    throw new CliError(`Invalid issue identifier: ${issue} (must match [a-zA-Z0-9_-]+)`);
  }
  if (!SAFE_IDENTIFIER_RE.test(mode)) {
    throw new CliError(`Invalid mode: ${mode} (must match [a-zA-Z0-9_-]+)`);
  }
  if (
    opts.version !== undefined &&
    (!Number.isInteger(opts.version) || opts.version < 0 || !Number.isSafeInteger(opts.version))
  ) {
    throw new CliError("Invalid version: must be a non-negative integer");
  }
  const daemonPort = opts.daemonPort ?? (await getDaemonPort());
  const baseUrl = `http://127.0.0.1:${daemonPort}`;

  // Verify daemon is reachable before creating workspace to avoid orphan directories
  try {
    const healthResp = await fetch(`${baseUrl}/health`);
    if (!healthResp.ok) {
      throw new CliError("Daemon is not healthy. Is it running?");
    }
  } catch (error) {
    if (error instanceof CliError) {
      throw error;
    }
    throw new CliError(`Could not connect to daemon. Is it running?\nTried: ${baseUrl}/health`);
  }

  const body: Record<string, unknown> = { issueId: issue, mode, version: opts.version };
  if (opts.repo) {
    body.repo = opts.repo;
  } else if (opts.workspace) {
    body.workspace = opts.workspace;
  } else if (opts.legionDir) {
    // Legacy: map LEGION_DIR to workspace while repo-based dispatch migration completes.
    body.workspace = opts.legionDir;
  } else {
    throw new CliError("Either --repo or --workspace is required");
  }
  if (opts.env) {
    body.env = opts.env;
  }
  if (opts.issueNumber !== undefined) {
    body.issueNumber = opts.issueNumber;
  }
  if (opts.force) {
    body.force = true;
  }
  // Include prompt in the POST /workers body so the server handles delivery
  // with the bootstrap delay + retry mechanism (#237)
  const initialPrompt = opts.prompt ?? `/legion-worker ${mode} mode for ${issue}`;
  body.prompt = initialPrompt;

  let response: Response;
  try {
    response = await fetch(`${baseUrl}/workers`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch (_error) {
    throw new CliError(`Could not connect to daemon. Is it running?\nTried: ${baseUrl}/workers`);
  }

  let responseBody: Record<string, unknown>;
  try {
    responseBody = (await response.json()) as Record<string, unknown>;
  } catch {
    throw new CliError(`Daemon returned non-JSON response (status ${response.status})`);
  }

  if (response.status === 409) {
    console.log(`Worker already running: ${responseBody.id}`);
    console.log(`  port: ${responseBody.port}`);
    console.log(`  session: ${responseBody.sessionId}`);
    console.log(
      `\nTo resume: legion prompt ${issue} "${
        opts.prompt ?? `/legion-worker ${mode} mode for ${issue}`
      }"`
    );
    return;
  }

  if (response.status === 429) {
    throw new CliError(
      `Crash limit exceeded for ${responseBody.id} (${responseBody.crashCount} crashes)\n` +
        `Reset with: legion reset-crashes ${issue} ${mode}`
    );
  }

  if (response.status === 422) {
    const detail = responseBody as {
      error: string;
      attemptedMode: string;
      suggestedAction: string;
      reason: string;
    };
    throw new CliError(
      `Phase prerequisite not met for "${detail.attemptedMode}":\n` +
        `  ${detail.reason}\n\n` +
        `To force dispatch: legion dispatch ${issue} ${mode} --force`
    );
  }

  if (!response.ok) {
    throw new CliError(`Failed to dispatch: ${JSON.stringify(responseBody)}`);
  }

  const workerId = responseBody.id as string;
  const workerPort = responseBody.port as number;
  const sessionId = responseBody.sessionId as string;

  console.log(`Worker dispatched: ${workerId}`);
  console.log(`  port: ${workerPort}`);
  console.log(`  session: ${sessionId}`);

  if (responseBody.promptDelivered === true) {
    console.log(`Prompt sent: ${initialPrompt}`);
  } else if (responseBody.promptDelivered === false) {
    console.warn("Worker spawned but prompt delivery failed. Send manually:");
    console.warn(`  legion prompt ${issue} "${initialPrompt}"`);
  }

  console.log(`\nTo attach: legion attach <legion> ${issue}`);
}

export async function cmdPrompt(issue: string, prompt: string, opts: PromptOptions): Promise<void> {
  if (!SAFE_IDENTIFIER_RE.test(issue)) {
    throw new CliError(`Invalid issue identifier: ${issue} (must match [a-zA-Z0-9_-]+)`);
  }
  const daemonPort = opts.daemonPort ?? (await getDaemonPort());
  const baseUrl = `http://127.0.0.1:${daemonPort}`;

  let workers: WorkerStatusInfo[];
  try {
    const response = await fetch(`${baseUrl}/workers`);
    if (!response.ok) {
      throw new CliError("Could not connect to daemon.");
    }
    workers = (await response.json()) as typeof workers;
  } catch (error) {
    if (error instanceof CliError) {
      throw error;
    }
    throw new CliError("Could not connect to daemon. Is it running?");
  }

  const normalized = issue.toLowerCase();
  let matches = workers.filter(
    (worker) => worker.id === normalized || worker.id.startsWith(`${normalized}-`)
  );

  if (opts.mode) {
    matches = matches.filter((worker) => worker.id.endsWith(`-${opts.mode}`));
  }

  matches = matches.filter((worker) => worker.status === "running" || worker.status === "starting");

  if (matches.length === 0) {
    let msg = `No active worker found for: ${issue}${opts.mode ? ` (mode: ${opts.mode})` : ""}`;
    const alive = workers.filter(
      (worker) => worker.status === "running" || worker.status === "starting"
    );
    if (alive.length > 0) {
      msg += "\n\nActive workers:";
      for (const worker of alive) {
        msg += `\n  - ${worker.id}`;
      }
    }
    throw new CliError(msg);
  }

  if (matches.length > 1) {
    let msg = `Multiple workers found for ${issue}:`;
    for (const worker of matches) {
      msg += `\n  - ${worker.id}`;
    }
    msg += `\n\nSpecify mode: legion prompt ${issue} --mode <mode> "${prompt}"`;
    throw new CliError(msg);
  }

  const worker = matches[0];

  try {
    const promptResponse = await fetch(
      `${baseUrl}/workers/${encodeURIComponent(worker.id)}/prompt`,
      {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ text: prompt }),
      }
    );
    if (!promptResponse.ok) {
      throw new CliError(`Worker rejected prompt (status ${promptResponse.status}): ${worker.id}`);
    }
    console.log(`Prompt sent to ${worker.id}: ${prompt}`);
  } catch (error) {
    if (error instanceof CliError) {
      throw error;
    }
    throw new CliError(`Failed to send prompt to ${worker.id} (port ${worker.port})`);
  }
}

export async function cmdResetCrashes(
  issue: string,
  mode: string,
  opts?: ResetCrashesOptions
): Promise<void> {
  if (!SAFE_IDENTIFIER_RE.test(issue)) {
    throw new CliError(`Invalid issue identifier: ${issue} (must match [a-zA-Z0-9_-]+)`);
  }
  if (!SAFE_IDENTIFIER_RE.test(mode)) {
    throw new CliError(`Invalid mode: ${mode} (must match [a-zA-Z0-9_-]+)`);
  }
  const daemonPort = opts?.daemonPort ?? (await getDaemonPort());
  const workerId = `${issue.toLowerCase()}-${mode}`;

  try {
    const response = await fetch(
      `http://127.0.0.1:${daemonPort}/workers/${encodeURIComponent(workerId)}/crashes`,
      {
        method: "DELETE",
      }
    );
    if (response.ok) {
      console.log(`Crash history cleared for ${workerId}`);
      console.log(`You can now dispatch: legion dispatch ${issue} ${mode}`);
    } else {
      throw new CliError(`Failed to reset crashes: ${response.status}`);
    }
  } catch (error) {
    if (error instanceof CliError) {
      throw error;
    }
    throw new CliError("Could not connect to daemon. Is it running?");
  }
}

interface OcRegistryEntry {
  pid: number;
  dir: string;
}

export async function scanOcRegistry(
  sessionId: string,
  registryDir?: string
): Promise<OcRegistryEntry | null> {
  const dir =
    registryDir ??
    (() => {
      const uid = process.getuid?.();
      if (uid === undefined) return null;
      return `/run/user/${uid}/opencode-${uid}`;
    })();
  if (!dir) return null;

  let files: string[];
  try {
    files = await readdir(dir);
  } catch {
    return null;
  }

  for (const file of files) {
    if (!file.endsWith(".json")) continue;
    try {
      const content = await readFile(path.join(dir, file), "utf-8");
      const entry = JSON.parse(content) as Record<string, unknown>;
      const session = entry.session as Record<string, unknown> | undefined;
      if (session?.id === sessionId) {
        const pid = typeof entry.pid === "number" ? entry.pid : undefined;
        const entryDir = typeof entry.dir === "string" ? entry.dir : undefined;
        if (pid !== undefined && entryDir !== undefined) {
          // Validate path doesn't contain traversal sequences
          if (entryDir.includes("..") || !entryDir.startsWith("/")) {
            continue; // Skip malicious entries
          }
          return { pid, dir: entryDir };
        }
      }
    } catch {}
  }

  return null;
}

interface AdoptOptions {
  mode: string;
  issue: string;
  workspace?: string;
  daemonPort?: number;
}

export async function cmdAdopt(team: string, session: string, opts: AdoptOptions): Promise<void> {
  if (!SESSION_ID_PATTERN.test(session)) {
    throw new CliError(
      `Invalid session ID format: ${session}\nExpected: ses_ + 12 hex + 14 Base62`
    );
  }
  if (!SAFE_IDENTIFIER_RE.test(opts.issue)) {
    throw new CliError(`Invalid issue identifier: ${opts.issue} (must match [a-zA-Z0-9_-]+)`);
  }
  if (!SAFE_IDENTIFIER_RE.test(opts.mode)) {
    throw new CliError(`Invalid mode: ${opts.mode} (must match [a-zA-Z0-9_-]+)`);
  }

  const legionId = await resolveLegionId(team);
  const daemonPort = opts.daemonPort ?? (await getDaemonPort(legionId));
  const baseUrl = `http://127.0.0.1:${daemonPort}`;

  try {
    const healthResp = await fetch(`${baseUrl}/health`);
    if (!healthResp.ok) {
      throw new CliError("Daemon is not healthy. Is it running?");
    }
  } catch (error) {
    if (error instanceof CliError) throw error;
    throw new CliError(`Could not connect to daemon. Is it running?\nTried: ${baseUrl}/health`);
  }

  let workspace = opts.workspace;
  let registryPid: number | undefined;
  if (!workspace) {
    const registryEntry = await scanOcRegistry(session);
    if (registryEntry?.dir) {
      workspace = registryEntry.dir;
      registryPid = registryEntry.pid;
      console.log(`Resolved workspace from OC registry: ${workspace}`);
    }
  }

  if (!workspace) {
    throw new CliError(
      "Could not resolve workspace. Provide --workspace or ensure the session is in the OC registry."
    );
  }

  const body: Record<string, unknown> = {
    issueId: opts.issue,
    mode: opts.mode,
    workspace,
    sessionId: session,
    force: true,
    prompt: `/legion-worker ${opts.mode} mode for ${opts.issue}`,
  };

  let response: Response;
  try {
    response = await fetch(`${baseUrl}/workers`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    throw new CliError(`Could not connect to daemon. Is it running?\nTried: ${baseUrl}/workers`);
  }

  let responseBody: Record<string, unknown>;
  try {
    responseBody = (await response.json()) as Record<string, unknown>;
  } catch {
    throw new CliError(`Daemon returned non-JSON response (status ${response.status})`);
  }

  if (response.status === 409) {
    if (responseBody.error === "session_already_adopted") {
      throw new CliError(`Session ${session} is already tracked by worker ${responseBody.id}`);
    }
    console.log(`Worker already running: ${responseBody.id}`);
    console.log(`  port: ${responseBody.port}`);
    console.log(`  session: ${responseBody.sessionId}`);
    return;
  }

  if (response.status === 422) {
    throw new CliError(`Invalid request: ${JSON.stringify(responseBody)}`);
  }

  if (!response.ok) {
    throw new CliError(`Failed to adopt: ${JSON.stringify(responseBody)}`);
  }

  const workerId = responseBody.id as string;
  const workerPort = responseBody.port as number;
  const adoptedSessionId = responseBody.sessionId as string;

  console.log(`Session adopted: ${workerId}`);
  console.log(`  port: ${workerPort}`);
  console.log(`  session: ${adoptedSessionId}`);

  if (responseBody.promptDelivered === true) {
    console.log(`Prompt sent: /legion-worker ${opts.mode} mode for ${opts.issue}`);
  } else if (responseBody.promptDelivered === false) {
    console.warn("Session adopted but prompt delivery failed. Send manually:");
    console.warn(
      `  legion prompt ${opts.issue} "/legion-worker ${opts.mode} mode for ${opts.issue}"`
    );
  }

  if (registryPid) {
    try {
      process.kill(registryPid, "SIGHUP");
      console.log(`Sent SIGHUP to original process (PID ${registryPid})`);
    } catch (error) {
      console.warn(`Could not send SIGHUP to PID ${registryPid}: ${(error as Error).message}`);
    }
  }

  console.log(`\nTo attach: legion attach ${team} ${opts.issue}`);
}

async function checkDaemonHealth(port: number): Promise<DaemonHealth> {
  try {
    const response = await fetch(`http://127.0.0.1:${port}/health`);
    if (!response.ok) {
      return { running: false };
    }
    const data = (await response.json()) as {
      status?: string;
      workerCount?: number;
      runtime?: string;
      tmuxSession?: string;
    };
    if (data.status !== "ok") {
      return { running: false };
    }
    return {
      running: true,
      workerCount: data.workerCount ?? 0,
      runtime: data.runtime,
      tmuxSession: data.tmuxSession,
    };
  } catch (_error) {
    return { running: false };
  }
}

async function cmdLegions(includeAll: boolean): Promise<void> {
  const cache = loadLegionsCache();
  if (!cache || Object.keys(cache).length === 0) {
    console.log("No legions cached. Start a swarm first.");
    return;
  }

  const entries = await Promise.all(
    Object.entries(cache).map(async ([key, team]) => {
      const daemonPort = await getDaemonPort(team.id);
      const health = await checkDaemonHealth(daemonPort);
      const stateFilePath = resolveLegionPaths(process.env, os.homedir()).forLegion(
        team.id
      ).workersFile;
      if (!fs.existsSync(stateFilePath)) {
        return { key, team, running: false, workerCount: 0 };
      }
      return { key, team, running: health.running, workerCount: health.workerCount ?? 0 };
    })
  );

  const filtered = includeAll ? entries : entries.filter((entry) => entry.running);
  if (filtered.length === 0) {
    console.log("No active daemons. Use --all to show cached legions.");
    return;
  }

  console.log("Legions:");
  for (const entry of filtered) {
    const status = entry.running ? `RUNNING (workers: ${entry.workerCount})` : "STOPPED";
    console.log(`  ${entry.key}: ${entry.team.name} (${entry.team.id}) - ${status}`);
  }
}

async function cmdCollectState(backend: string): Promise<void> {
  if (backend !== "linear" && backend !== "github") {
    throw new CliError(`Invalid backend: ${backend}. Must be 'linear' or 'github'.`);
  }

  const stdinText = await new Response(Bun.stdin.stream()).text();
  if (!stdinText.trim()) {
    throw new CliError("No input on stdin. Usage: echo '$JSON' | legion collect-state <backend>");
  }
  let issues: unknown;
  try {
    issues = JSON.parse(stdinText);
  } catch {
    throw new CliError("Failed to parse stdin as JSON");
  }

  const daemonPort = await getDaemonPort();
  const baseUrl = `http://127.0.0.1:${daemonPort}`;

  try {
    const response = await fetch(`${baseUrl}/state/collect`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ backend, issues }),
    });

    if (!response.ok) {
      const body = await response.text();
      throw new CliError(`Daemon returned ${response.status}: ${body}`);
    }

    const result = await response.text();
    process.stdout.write(`${result}\n`);
  } catch (error) {
    if (error instanceof CliError) throw error;
    throw new CliError(`Could not connect to daemon at ${baseUrl}/state/collect. Is it running?`);
  }
}

function requireHandoffPhase(value: string | undefined, argName: string): HandoffPhase {
  if (!value) {
    throw new CliError(`Missing required argument: --${argName}`);
  }

  if (!isHandoffPhase(value)) {
    throw new CliError(`Invalid phase: ${value}. Must be one of: ${HANDOFF_PHASES.join(", ")}`);
  }

  return value;
}

function parseHandoffData(data: string | undefined): Record<string, unknown> {
  if (!data) {
    throw new CliError("Missing required argument: --data");
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    throw new CliError("Invalid JSON in --data");
  }

  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new CliError("Invalid --data: must be a JSON object");
  }

  const payload = parsed as Record<string, unknown>;
  if ("schemaVersion" in payload || "phase" in payload || "completed" in payload) {
    throw new CliError(
      "Invalid --data: schemaVersion, phase, and completed are auto-populated and not allowed"
    );
  }

  return payload;
}

export function resolveKnowledgeWorkspaceContext(
  explicitWorkspaceRoot?: string,
  env: Record<string, string | undefined> = process.env,
  homeDir: string = os.homedir()
): {
  legionId: string;
  workspaceRoot: string;
} {
  const { workspacesDir } = resolveLegionPaths(env, homeDir);
  const candidatePath = path.resolve(explicitWorkspaceRoot ?? process.cwd());
  const relativePath = path.relative(workspacesDir, candidatePath);

  if (
    relativePath === "" ||
    relativePath === ".." ||
    relativePath.startsWith(`..${path.sep}`) ||
    path.isAbsolute(relativePath)
  ) {
    throw new CliError(`Not inside a Legion workspace: ${candidatePath}`);
  }

  const parts = relativePath.split(path.sep).filter(Boolean);
  if (parts.length < 3) {
    throw new CliError(`Workspace path does not include a Legion workspace root: ${candidatePath}`);
  }

  return {
    legionId: `${parts[0]}/${parts[1]}`,
    workspaceRoot: path.join(workspacesDir, ...parts.slice(0, 3)),
  };
}

interface KnowledgeConsolidateOptions {
  apply?: boolean;
  json?: boolean;
  workspace?: string;
}

export async function cmdKnowledgeConsolidate(opts: KnowledgeConsolidateOptions): Promise<void> {
  const { legionId, workspaceRoot } = resolveKnowledgeWorkspaceContext(opts.workspace);
  const report = await consolidateKnowledge({
    legionId,
    workspaceRoot,
    apply: Boolean(opts.apply),
  });

  console.log(
    opts.json ? formatConsolidationReportJson(report) : formatConsolidationReportHuman(report)
  );
}

export const startCommand = defineCommand({
  meta: { name: "start", description: "Start the Legion swarm" },
  args: {
    team: { type: "positional", description: "Legion key or UUID", required: false },
    workspace: {
      type: "string",
      alias: "w",
      description: "Workspace path",
    },
    "state-dir": { type: "string", description: "State directory path" },
    prompt: {
      type: "string",
      alias: "p",
      description: "Custom prompt appended to the controller's initial /legion-controller prompt",
    },
    backend: {
      type: "string",
      alias: "b",
      description: "Issue tracker backend (linear or github)",
    },
    runtime: {
      type: "string",
      alias: "r",
      description: "Agent runtime (opencode or claude-code)",
    },
    config: { type: "string", alias: "c", description: "Config file path" },
  },
  async run({ args }) {
    await cmdStart(args.team, {
      workspace: args.workspace,
      prompt: args.prompt,
      backend: args.backend,
      runtime: args.runtime,
      config: args.config,
    });
  },
});

export const stopCommand = defineCommand({
  meta: { name: "stop", description: "Stop the Legion swarm" },
  args: {
    team: { type: "positional", description: "Legion key or UUID", required: true },
    "state-dir": { type: "string", description: "State directory path" },
    backend: {
      type: "string",
      alias: "b",
      description: "Issue tracker backend (linear or github)",
    },
  },
  async run({ args }) {
    await cmdStop(args.team, args["state-dir"], args.backend);
  },
});

export const statusCommand = defineCommand({
  meta: { name: "status", description: "Show Legion swarm status" },
  args: {
    team: { type: "positional", description: "Legion key or UUID", required: true },
    "state-dir": { type: "string", description: "State directory path" },
    backend: {
      type: "string",
      alias: "b",
      description: "Issue tracker backend (linear or github)",
    },
  },
  async run({ args }) {
    await cmdStatus(args.team, args["state-dir"], args.backend);
  },
});

export const attachCommand = defineCommand({
  meta: { name: "attach", description: "Attach to a worker session" },
  args: {
    team: { type: "positional", description: "Legion key or UUID", required: true },
    issue: { type: "positional", description: "Issue key or identifier", required: true },
    backend: {
      type: "string",
      alias: "b",
      description: "Issue tracker backend (linear or github)",
    },
  },
  async run({ args }) {
    try {
      await cmdAttach(args.team, args.issue, args.backend);
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});

export const adoptCommand = defineCommand({
  meta: {
    name: "adopt",
    description: "Adopt an existing OpenCode session as a Legion worker",
  },
  args: {
    team: {
      type: "positional",
      description: "Legion key or ID (e.g., sjawhar/5)",
      required: true,
    },
    session: {
      type: "positional",
      description: "OpenCode session ID (e.g., ses_...)",
      required: true,
    },
    mode: {
      type: "string",
      alias: "m",
      description: "Worker mode (architect, plan, implement, test, review, merge)",
      required: true,
    },
    issue: {
      type: "string",
      alias: "i",
      description: "Issue identifier (e.g., eng-21, gh-42)",
      required: true,
    },
    workspace: {
      type: "string",
      alias: "w",
      description: "Override workspace path (default: resolved from OC registry)",
    },
  },
  async run({ args }) {
    try {
      await cmdAdopt(args.team, args.session, {
        mode: args.mode as string,
        issue: args.issue as string,
        workspace: args.workspace as string | undefined,
      });
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});

export const legionsCommand = defineCommand({
  meta: { name: "teams", description: "List legions and their daemon status" },
  args: {
    all: {
      type: "boolean",
      description: "Include cached legions without running daemons",
      default: false,
    },
  },
  async run({ args }) {
    await cmdLegions(args.all);
  },
});

export const dispatchCommand = defineCommand({
  meta: { name: "dispatch", description: "Dispatch a worker for an issue" },
  args: {
    issue: {
      type: "positional",
      description: "Issue identifier (e.g., LEG-42)",
      required: true,
    },
    mode: {
      type: "positional",
      description: "Worker mode (architect, plan, implement, test, review, merge)",
      required: true,
    },
    prompt: { type: "string", description: "Custom initial prompt (default: /legion-worker)" },
    repo: { type: "string", alias: "r", description: "Repository (owner/repo)" },
    workspace: { type: "string", alias: "w", description: "Override workspace path" },
    version: {
      type: "string",
      alias: "v",
      description:
        "Session version override for fresh dispatch (e.g., --version 1, then --version 2)",
    },
    env: {
      type: "string",
      description: 'Worker env as JSON object (e.g. --env \'{"KEY":"VALUE"}\')',
    },
    "issue-number": {
      type: "string",
      description: "GitHub issue number for Envoy subscription",
    },
    force: {
      type: "boolean",
      alias: "f",
      description: "Bypass phase prerequisite validation (emergency use only)",
    },
  },
  async run({ args }) {
    try {
      let version: number | undefined;
      if (args.version !== undefined) {
        const parsed = Number(args.version);
        if (!Number.isInteger(parsed) || parsed < 0 || !Number.isSafeInteger(parsed)) {
          throw new CliError("Invalid version: must be a non-negative integer");
        }
        version = parsed;
      }
      const dispatchOpts: DispatchOptions = {
        legionDir: process.env.LEGION_DIR,
        prompt: args.prompt,
        repo: args.repo,
        workspace: args.workspace,
        version,
      };
      if (args.env) {
        const envObj = parseEnvJson(args.env as string);
        dispatchOpts.env = envObj;
      }
      if (args["issue-number"] !== undefined) {
        const parsed = Number(args["issue-number"]);
        if (!Number.isInteger(parsed) || parsed <= 0) {
          throw new CliError("Invalid issue-number: must be a positive integer");
        }
        dispatchOpts.issueNumber = parsed;
      }
      if (args.force) {
        dispatchOpts.force = true;
      }
      await cmdDispatch(args.issue, args.mode, dispatchOpts);
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});

export const promptCommand = defineCommand({
  meta: { name: "prompt", description: "Send a prompt to an existing worker" },
  args: {
    issue: {
      type: "positional",
      description: "Issue identifier (e.g., LEG-42)",
      required: true,
    },
    prompt: { type: "positional", description: "Prompt text to send", required: true },
    mode: { type: "string", description: "Worker mode (to disambiguate)" },
  },
  async run({ args }) {
    try {
      await cmdPrompt(args.issue, args.prompt, { mode: args.mode });
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});

export const resetCrashesCommand = defineCommand({
  meta: { name: "reset-crashes", description: "Reset crash history for a worker" },
  args: {
    issue: { type: "positional", description: "Issue identifier", required: true },
    mode: { type: "positional", description: "Worker mode", required: true },
  },
  async run({ args }) {
    try {
      await cmdResetCrashes(args.issue, args.mode);
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});

export const collectStateCommand = defineCommand({
  meta: { name: "collect-state", description: "Collect and analyze issue state via daemon" },
  args: {
    backend: {
      type: "positional",
      description: "Issue tracker backend (linear or github)",
      required: true,
    },
  },
  async run({ args }) {
    try {
      await cmdCollectState(args.backend);
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});

export async function cmdPoll(team: string, opts: { json: boolean }): Promise<void> {
  const legionId = await resolveLegionId(team, {});
  const daemonPort = await getDaemonPort(legionId);
  const baseUrl = `http://127.0.0.1:${daemonPort}`;

  let response: Response;
  try {
    response = await fetch(`${baseUrl}/state/materialized`);
  } catch (_error) {
    throw new CliError(
      `Could not connect to daemon. Is it running?\nTried: ${baseUrl}/state/materialized`
    );
  }

  if (!response.ok) {
    const body = await response.text();
    throw new CliError(`Daemon returned ${response.status}: ${body}`);
  }

  const data = (await response.json()) as {
    issues: Record<string, import("../state/types").IssueStateDict>;
    titles?: Record<string, string>;
    newIssues?: Array<{ issueId: string; state: import("../state/types").IssueStateDict }>;
  };

  if (opts.json) {
    process.stdout.write(`${JSON.stringify(data, null, 2)}\n`);
    return;
  }

  const output = formatPollOutput(data.issues, data.titles ?? {});
  if (output) {
    console.log(output);
  } else {
    console.log("No issues found.");
  }

  const newIssues = data.newIssues ?? [];
  if (newIssues.length > 0) {
    console.log(`\nNEW (${newIssues.length}):`);
    for (const entry of newIssues) {
      const title = data.titles?.[entry.issueId];
      const titlePart = title ? `  "${title}"` : "";
      console.log(`  ${entry.issueId}  ${entry.state.status}${titlePart}`);
    }
  }
}

export const pollCommand = defineCommand({
  meta: { name: "poll", description: "Poll state machine for compact actionable summary" },
  args: {
    team: { type: "positional", description: "Legion key or UUID", required: true },
    json: {
      type: "boolean",
      description: "Output raw JSON from fetch-and-collect",
      default: false,
    },
  },
  async run({ args }) {
    try {
      await cmdPoll(args.team, { json: args.json });
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});

export const handoffCommand = defineCommand({
  meta: { name: "handoff", description: "Read and write local handoff files" },
  subCommands: {
    write: defineCommand({
      meta: { name: "write", description: "Write phase handoff data" },
      args: {
        phase: { type: "string", required: true, description: "Handoff phase" },
        data: {
          type: "string",
          description: "JSON string of phase fields (reads stdin if omitted)",
        },
        workspace: { type: "string", description: "Workspace directory (defaults to cwd)" },
      },
      async run({ args }) {
        try {
          const phase = requireHandoffPhase(args.phase, "phase");
          const workspaceDir = (args.workspace as string) || process.cwd();
          let rawData: string;
          if (args.data) {
            rawData = args.data as string;
          } else {
            rawData = await readStdin();
          }
          const data = parseHandoffData(rawData);
          writePhaseHandoff(workspaceDir, phase, data);
          const filePath = path.join(workspaceDir, ".legion", `${phase}.json`);
          console.log(`[handoff] Wrote ${phase} handoff to ${filePath}`);
        } catch (e) {
          if (e instanceof CliError) {
            console.error(e.message);
            process.exit(e.code);
          }
          console.error(
            `[handoff] Failed to write handoff: ${e instanceof Error ? e.message : String(e)}`
          );
          process.exit(1);
        }
      },
    }),
    read: defineCommand({
      meta: { name: "read", description: "Read phase handoff data" },
      args: {
        phase: { type: "string", description: "Optional handoff phase" },
        workspace: { type: "string", description: "Workspace directory (defaults to cwd)" },
      },
      async run({ args }) {
        try {
          const workspaceDir = (args.workspace as string) || process.cwd();
          const output = args.phase
            ? readPhaseHandoff(workspaceDir, requireHandoffPhase(args.phase, "phase"))
            : readAllHandoffs(workspaceDir);
          console.log(JSON.stringify(output, null, 2));
        } catch (e) {
          if (e instanceof CliError) {
            console.error(e.message);
            process.exit(e.code);
          }
          throw e;
        }
      },
    }),
    messages: defineCommand({
      meta: { name: "messages", description: "Read cross-phase handoff messages" },
      args: {
        workspace: { type: "string", description: "Workspace directory (defaults to cwd)" },
      },
      async run({ args }) {
        try {
          const workspaceDir = (args.workspace as string) || process.cwd();
          const messages = readMessages(workspaceDir);
          console.log(JSON.stringify(messages, null, 2));
        } catch (e) {
          if (e instanceof CliError) {
            console.error(e.message);
            process.exit(e.code);
          }
          throw e;
        }
      },
    }),
    message: defineCommand({
      meta: { name: "message", description: "Write a cross-phase handoff message" },
      args: {
        from: { type: "string", required: true, description: "Source phase" },
        to: { type: "string", required: true, description: "Destination phase" },
        body: { type: "string", required: true, description: "Message body" },
        workspace: { type: "string", description: "Workspace directory (defaults to cwd)" },
      },
      async run({ args }) {
        try {
          const from = requireHandoffPhase(args.from, "from");
          const to = requireHandoffPhase(args.to, "to");
          if (!args.body) {
            throw new CliError("Missing required argument: --body");
          }
          const workspaceDir = (args.workspace as string) || process.cwd();
          writeMessage(workspaceDir, {
            from,
            to,
            body: args.body,
          });
          console.log(`[handoff] Wrote message from ${from} to ${to}`);
        } catch (e) {
          if (e instanceof CliError) {
            console.error(e.message);
            process.exit(e.code);
          }
          console.error(
            `[handoff] Failed to write message: ${e instanceof Error ? e.message : String(e)}`
          );
          process.exit(1);
        }
      },
    }),
  },
});

export const knowledgeCommand = defineCommand({
  meta: { name: "knowledge", description: "Knowledge management utilities" },
  subCommands: {
    consolidate: defineCommand({
      meta: {
        name: "consolidate",
        description: "Aggregate learning feedback and apply knowledge mutations",
      },
      args: {
        workspace: { type: "string", alias: "w", description: "Workspace directory" },
        apply: {
          type: "boolean",
          description: "Apply index and front-matter mutations",
          default: false,
        },
        json: {
          type: "boolean",
          description: "Output the consolidation report as JSON",
          default: false,
        },
      },
      async run({ args }) {
        try {
          await cmdKnowledgeConsolidate({
            apply: Boolean(args.apply),
            json: Boolean(args.json),
            workspace: args.workspace as string | undefined,
          });
        } catch (e) {
          if (e instanceof CliError) {
            console.error(e.message);
            process.exit(e.code);
          }
          throw e;
        }
      },
    }),
  },
});

export const mainCommand = defineCommand({
  meta: { name: "legion", description: "Autonomous development swarm", version: "0.1.0" },
  subCommands: {
    start: startCommand,
    stop: stopCommand,
    status: statusCommand,
    attach: attachCommand,
    adopt: adoptCommand,
    dispatch: dispatchCommand,
    prompt: promptCommand,
    "reset-crashes": resetCrashesCommand,
    teams: legionsCommand,
    "collect-state": collectStateCommand,
    poll: pollCommand,
    handoff: handoffCommand,
    knowledge: knowledgeCommand,
  },
});

if (import.meta.main) {
  runMain(mainCommand);
}
