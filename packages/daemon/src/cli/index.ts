#!/usr/bin/env bun
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { defineCommand, runMain } from "citty";
import { type DaemonConfig, validateControllerPrompt } from "../daemon/config";
import { startDaemon } from "../daemon/index";
import { findLegionByProjectId } from "../daemon/legions-registry";
import { resolveLegionPaths } from "../daemon/paths";
import { resolveLegionId } from "./legion-resolver";

export class CliError extends Error {
  constructor(
    message: string,
    public code = 1
  ) {
    super(message);
    this.name = "CliError";
  }
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
  workspace: string;
  prompt?: string;
  backend?: string;
  runtime?: string;
}

async function cmdStart(team: string, opts: StartOptions): Promise<void> {
  const legionId = await resolveLegionId(team, { backend: opts.backend });
  const instancePaths = resolveLegionPaths(process.env, os.homedir()).forLegion(legionId);

  validateControllerPrompt(opts.prompt);

  console.log(`Starting legion: ${legionId}`);
  console.log(`Workspace: ${opts.workspace}`);
  console.log(`State directory: ${instancePaths.legionStateDir}`);

  fs.mkdirSync(instancePaths.legionStateDir, { recursive: true });

  if (opts.backend) {
    process.env.LEGION_ISSUE_BACKEND = opts.backend;
  }

  if (opts.runtime) {
    process.env.LEGION_RUNTIME = opts.runtime;
  }
  const overrides: Partial<DaemonConfig> = {
    legionId,
    legionDir: opts.workspace,
    stateFilePath: instancePaths.workersFile,
  };
  if (opts.prompt !== undefined) {
    overrides.controllerPrompt = opts.prompt;
  }

  const handle = await startDaemon(overrides);

  console.log(`Daemon started on port ${handle.config.daemonPort}`);
  console.log(`\nTo check status: legion status ${team}`);
  console.log(`To stop: legion stop ${team}`);

  await new Promise(() => {});
}

async function cmdStop(team: string, _stateDir?: string, backend?: string): Promise<void> {
  const legionId = await resolveLegionId(team, { backend });
  const instancePaths = resolveLegionPaths(process.env, os.homedir()).forLegion(legionId);

  console.log(`Stopping legion: ${legionId}`);

  const stateFilePath = instancePaths.workersFile;
  if (!fs.existsSync(stateFilePath)) {
    console.log("No state file found. Daemon may not be running.");
    return;
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

  if (!response.ok) {
    throw new CliError(`Failed to dispatch: ${JSON.stringify(responseBody)}`);
  }

  const workerId = responseBody.id as string;
  const workerPort = responseBody.port as number;
  const sessionId = responseBody.sessionId as string;

  console.log(`Worker dispatched: ${workerId}`);
  console.log(`  port: ${workerPort}`);
  console.log(`  session: ${sessionId}`);

  const initialPrompt = opts.prompt ?? `/legion-worker ${mode} mode for ${issue}`;
  try {
    await fetch(`${baseUrl}/workers/${encodeURIComponent(workerId)}/prompt`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ text: initialPrompt }),
    });
    console.log(`Prompt sent: ${initialPrompt}`);
  } catch (_error) {
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

export const startCommand = defineCommand({
  meta: { name: "start", description: "Start the Legion swarm" },
  args: {
    team: { type: "positional", description: "Legion key or UUID", required: true },
    workspace: {
      type: "string",
      alias: "w",
      description: "Workspace path",
      default: process.cwd(),
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
  },
  async run({ args }) {
    await cmdStart(args.team, {
      workspace: args.workspace,
      prompt: args.prompt,
      backend: args.backend,
      runtime: args.runtime,
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
    version: { type: "string", description: "Session version (default: 0)" },
  },
  async run({ args }) {
    try {
      const parsedVersion =
        args.version !== undefined && args.version !== "" ? Number(args.version) : undefined;
      if (parsedVersion !== undefined && (!Number.isInteger(parsedVersion) || parsedVersion < 0)) {
        throw new CliError(`Invalid --version: ${args.version} (must be a non-negative integer)`);
      }
      await cmdDispatch(args.issue, args.mode, {
        // Legacy transition: keep LEGION_DIR fallback for users not passing --workspace.
        legionDir: process.env.LEGION_DIR,
        prompt: args.prompt,
        repo: args.repo,
        workspace: args.workspace,
        version: parsedVersion,
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

export const mainCommand = defineCommand({
  meta: { name: "legion", description: "Autonomous development swarm", version: "0.1.0" },
  subCommands: {
    start: startCommand,
    stop: stopCommand,
    status: statusCommand,
    attach: attachCommand,
    dispatch: dispatchCommand,
    prompt: promptCommand,
    "reset-crashes": resetCrashesCommand,
    teams: legionsCommand,
    "collect-state": collectStateCommand,
  },
});

if (import.meta.main) {
  runMain(mainCommand);
}
