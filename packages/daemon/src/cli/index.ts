#!/usr/bin/env bun
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { defineCommand, runMain } from "citty";
import type { DaemonConfig } from "../daemon/config";
import { startDaemon } from "../daemon/index";
import { resolveTeamId } from "./team-resolver";

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

interface TeamInfo {
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
  legionDir?: string;
  daemonPort?: number;
  prompt?: string;
  workspace?: string;
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
}

export type TeamsCache = Record<string, TeamInfo>;

export function getDaemonPort(env: NodeJS.ProcessEnv = process.env): number {
  const raw = env.LEGION_DAEMON_PORT;
  if (!raw) {
    return DEFAULT_DAEMON_PORT;
  }
  const parsed = Number(raw);
  if (!Number.isFinite(parsed)) {
    return DEFAULT_DAEMON_PORT;
  }
  return parsed;
}

export function loadTeamsCache(cacheDir = path.join(os.homedir(), ".legion")): TeamsCache | null {
  const cacheFile = path.join(cacheDir, "teams.json");
  if (!fs.existsSync(cacheFile)) {
    return null;
  }

  const raw = fs.readFileSync(cacheFile, "utf-8");
  if (!raw.trim()) {
    return null;
  }

  const parsed = JSON.parse(raw) as unknown;
  if (!parsed || typeof parsed !== "object") {
    throw new Error(`Invalid teams cache at ${cacheFile}`);
  }

  return parsed as TeamsCache;
}

function resolveStateDir(teamId: string, stateDir?: string): string {
  return stateDir ?? path.join(os.homedir(), ".legion", teamId);
}

interface StartOptions {
  workspace: string;
  stateDir?: string;
  prompt?: string;
  backend?: string;
}

async function cmdStart(team: string, opts: StartOptions): Promise<void> {
  const teamId = await resolveTeamId(team);
  const resolvedStateDir = resolveStateDir(teamId, opts.stateDir);

  if (opts.prompt && opts.prompt.length > 10000) {
    throw new CliError(
      `Controller prompt exceeds maximum length of 10000 characters (got ${opts.prompt.length})`
    );
  }

  if (opts.prompt && /[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(opts.prompt)) {
    throw new CliError("Controller prompt contains invalid control characters");
  }

  console.log(`Starting Legion for team: ${teamId}`);
  console.log(`Workspace: ${opts.workspace}`);
  console.log(`State directory: ${resolvedStateDir}`);

  fs.mkdirSync(resolvedStateDir, { recursive: true });

  if (opts.backend) {
    process.env.LEGION_ISSUE_BACKEND = opts.backend;
  }

  const overrides: Partial<DaemonConfig> = {
    teamId,
    legionDir: opts.workspace,
    stateFilePath: path.join(resolvedStateDir, "workers.json"),
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

async function cmdStop(team: string, stateDir?: string): Promise<void> {
  const teamId = await resolveTeamId(team);
  const resolvedStateDir = resolveStateDir(teamId, stateDir);

  console.log(`Stopping Legion for team: ${teamId}`);

  const stateFilePath = path.join(resolvedStateDir, "workers.json");
  if (!fs.existsSync(stateFilePath)) {
    console.log("No state file found. Daemon may not be running.");
    return;
  }

  const daemonPort = getDaemonPort();
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

async function cmdStatus(team: string, stateDir?: string): Promise<void> {
  const teamId = await resolveTeamId(team);
  const resolvedStateDir = resolveStateDir(teamId, stateDir);

  console.log(`Legion Status: ${teamId}`);
  console.log("=".repeat(40));

  const daemonPort = getDaemonPort();
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

  const stateFilePath = path.join(resolvedStateDir, "workers.json");
  if (fs.existsSync(stateFilePath)) {
    const stat = fs.statSync(stateFilePath);
    const age = Math.floor((Date.now() - stat.mtimeMs) / 1000);
    console.log(`\nState file: ${stateFilePath}`);
    console.log(`Last updated: ${age}s ago`);
  } else {
    console.log(`\nState file: NOT FOUND`);
  }
}

async function cmdAttach(team: string, issue: string): Promise<void> {
  const teamId = await resolveTeamId(team);

  console.log(`Attaching to worker for issue: ${issue}`);
  console.log(`Team: ${teamId}`);

  const daemonPort = getDaemonPort();
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

    console.log(`Found worker on port ${worker.port}`);
    console.log(`Attaching with: opencode attach http://localhost:${worker.port}`);

    const child = spawn("opencode", ["attach", `http://localhost:${worker.port}`], {
      stdio: "inherit",
    });

    child.on("exit", (code) => {
      process.exit(code ?? 0);
    });
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
  const daemonPort = opts.daemonPort ?? getDaemonPort();
  const legionDir = opts.legionDir ?? process.env.LEGION_DIR ?? process.cwd();
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

  const issueLower = issue.toLowerCase();
  const workspacePath = opts.workspace ?? path.join(path.dirname(legionDir), issueLower);

  if (!fs.existsSync(workspacePath)) {
    console.log(`Creating workspace: ${workspacePath}`);
    const jjResult = Bun.spawnSync(
      ["jj", "workspace", "add", workspacePath, "--name", issueLower, "-R", legionDir],
      { stdio: ["ignore", "pipe", "pipe"], timeout: 30_000 }
    );
    if (jjResult.exitCode !== 0) {
      const stderr = jjResult.stderr.toString();
      throw new CliError(`Failed to create workspace: ${stderr}`);
    }
  }

  let response: Response;
  try {
    response = await fetch(`${baseUrl}/workers`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ issueId: issue, mode, workspace: workspacePath }),
    });
  } catch (_error) {
    throw new CliError(`Could not connect to daemon. Is it running?\nTried: ${baseUrl}/workers`);
  }

  let body: Record<string, unknown>;
  try {
    body = (await response.json()) as Record<string, unknown>;
  } catch {
    throw new CliError(`Daemon returned non-JSON response (status ${response.status})`);
  }

  if (response.status === 409) {
    console.log(`Worker already running: ${body.id}`);
    console.log(`  port: ${body.port}`);
    console.log(`  session: ${body.sessionId}`);
    console.log(
      `\nTo resume: legion prompt ${issue} "${
        opts.prompt ?? `/legion-worker ${mode} mode for ${issue}`
      }"`
    );
    return;
  }

  if (response.status === 429) {
    throw new CliError(
      `Crash limit exceeded for ${body.id} (${body.crashCount} crashes)\n` +
        `Reset with: legion reset-crashes ${issue} ${mode}`
    );
  }

  if (!response.ok) {
    throw new CliError(`Failed to dispatch: ${JSON.stringify(body)}`);
  }

  const workerId = body.id as string;
  const workerPort = body.port as number;
  const sessionId = body.sessionId as string;

  console.log(`Worker dispatched: ${workerId}`);
  console.log(`  port: ${workerPort}`);
  console.log(`  session: ${sessionId}`);

  const initialPrompt = opts.prompt ?? `/legion-worker ${mode} mode for ${issue}`;
  try {
    await fetch(`http://127.0.0.1:${workerPort}/session/${sessionId}/prompt_async`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ parts: [{ type: "text", text: initialPrompt }] }),
    });
    console.log(`Prompt sent: ${initialPrompt}`);
  } catch (_error) {
    console.warn("Worker spawned but prompt delivery failed. Send manually:");
    console.warn(`  legion prompt ${issue} "${initialPrompt}"`);
  }

  console.log(`\nTo attach: legion attach <team> ${issue}`);
}

export async function cmdPrompt(issue: string, prompt: string, opts: PromptOptions): Promise<void> {
  if (!SAFE_IDENTIFIER_RE.test(issue)) {
    throw new CliError(`Invalid issue identifier: ${issue} (must match [a-zA-Z0-9_-]+)`);
  }
  const daemonPort = opts.daemonPort ?? getDaemonPort();
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
      `http://127.0.0.1:${worker.port}/session/${worker.sessionId}/prompt_async`,
      {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ parts: [{ type: "text", text: prompt }] }),
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
  const daemonPort = opts?.daemonPort ?? getDaemonPort();
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
    const data = (await response.json()) as { status?: string; workerCount?: number };
    if (data.status !== "ok") {
      return { running: false };
    }
    return { running: true, workerCount: data.workerCount ?? 0 };
  } catch (_error) {
    return { running: false };
  }
}

async function cmdTeams(includeAll: boolean): Promise<void> {
  const cache = loadTeamsCache();
  if (!cache || Object.keys(cache).length === 0) {
    console.log("No teams cached. Start a swarm first.");
    return;
  }

  const daemonPort = getDaemonPort();
  const health = await checkDaemonHealth(daemonPort);
  const entries = await Promise.all(
    Object.entries(cache).map(async ([key, team]) => {
      const stateFilePath = path.join(os.homedir(), ".legion", team.id, "workers.json");
      if (!fs.existsSync(stateFilePath)) {
        return { key, team, running: false, workerCount: 0 };
      }
      return { key, team, running: health.running, workerCount: health.workerCount ?? 0 };
    })
  );

  const filtered = includeAll ? entries : entries.filter((entry) => entry.running);
  if (filtered.length === 0) {
    console.log("No active daemons. Use --all to show cached teams.");
    return;
  }

  console.log("Teams:");
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
  let issues: unknown;
  try {
    issues = JSON.parse(stdinText);
  } catch {
    throw new CliError("Failed to parse stdin as JSON");
  }

  const daemonPort = getDaemonPort();
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
    team: { type: "positional", description: "Team key or UUID", required: true },
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
  },
  async run({ args }) {
    await cmdStart(args.team, {
      workspace: args.workspace,
      stateDir: args["state-dir"],
      prompt: args.prompt,
      backend: args.backend,
    });
  },
});

export const stopCommand = defineCommand({
  meta: { name: "stop", description: "Stop the Legion swarm" },
  args: {
    team: { type: "positional", description: "Team key or UUID", required: true },
    "state-dir": { type: "string", description: "State directory path" },
  },
  async run({ args }) {
    await cmdStop(args.team, args["state-dir"]);
  },
});

export const statusCommand = defineCommand({
  meta: { name: "status", description: "Show Legion swarm status" },
  args: {
    team: { type: "positional", description: "Team key or UUID", required: true },
    "state-dir": { type: "string", description: "State directory path" },
  },
  async run({ args }) {
    await cmdStatus(args.team, args["state-dir"]);
  },
});

export const attachCommand = defineCommand({
  meta: { name: "attach", description: "Attach to a worker session" },
  args: {
    team: { type: "positional", description: "Team key or UUID", required: true },
    issue: { type: "positional", description: "Issue key or identifier", required: true },
  },
  async run({ args }) {
    try {
      await cmdAttach(args.team, args.issue);
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});

export const teamsCommand = defineCommand({
  meta: { name: "teams", description: "List teams and their daemon status" },
  args: {
    all: {
      type: "boolean",
      description: "Include cached teams without running daemons",
      default: false,
    },
  },
  async run({ args }) {
    await cmdTeams(args.all);
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
      description: "Worker mode (architect, plan, implement, review, merge)",
      required: true,
    },
    prompt: { type: "string", description: "Custom initial prompt (default: /legion-worker)" },
    workspace: { type: "string", alias: "w", description: "Override workspace path" },
  },
  async run({ args }) {
    try {
      await cmdDispatch(args.issue, args.mode, {
        legionDir: process.env.LEGION_DIR,
        prompt: args.prompt,
        workspace: args.workspace,
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
    teams: teamsCommand,
    "collect-state": collectStateCommand,
  },
});

if (import.meta.main) {
  runMain(mainCommand);
}
