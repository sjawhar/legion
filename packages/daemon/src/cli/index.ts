#!/usr/bin/env bun
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { defineCommand, runMain } from "citty";
import { z } from "zod";
import {
  type DaemonConfig,
  type LoadedConfigFile,
  loadConfigFromFile,
  resolveDaemonConfig,
} from "../daemon/config";
import { startDaemon } from "../daemon/index";
import {
  findLegionByProjectId,
  isPidAlive,
  readLegionsRegistry,
  removeLegionEntry,
  writeLegionEntry,
} from "../daemon/legions-registry";
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

export class CliError extends Error {
  constructor(
    message: string,
    readonly code = 1
  ) {
    super(message);
    this.name = "CliError";
  }
}

type Fetch = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;

const GhTokenResponseSchema = z.object({ token: z.string().min(1) });

interface GhCommandDeps {
  env: NodeJS.ProcessEnv;
  fetch: Fetch;
  spawnGh(args: string[], env: NodeJS.ProcessEnv): Promise<number>;
  daemonUrl?: string;
}

interface CredentialCommandDeps {
  env: NodeJS.ProcessEnv;
  fetch: Fetch;
  readStdin(): Promise<string>;
  write(value: string): void;
  daemonUrl?: string;
}

function daemonUrl(env: NodeJS.ProcessEnv, explicit?: string): string {
  return (
    explicit ?? env.LEGION_DAEMON_URL ?? `http://127.0.0.1:${env.LEGION_DAEMON_PORT ?? "13370"}`
  );
}

function grantFrom(env: NodeJS.ProcessEnv): string {
  const grant = env.LEGION_GRANT;
  if (!grant) {
    throw new CliError(
      "LEGION_GRANT is missing: the Legion worker extension injects it before credential commands run"
    );
  }
  return grant;
}

async function spawnGh(args: string[], env: NodeJS.ProcessEnv): Promise<number> {
  const child = spawn("gh", args, { env, stdio: "inherit" });
  const completion = Promise.withResolvers<number>();
  child.once("error", completion.reject);
  child.once("close", (code) => completion.resolve(code ?? 1));
  return completion.promise;
}

export async function cmdGh(args: string[], deps: GhCommandDeps): Promise<void> {
  const response = await deps.fetch(`${daemonUrl(deps.env, deps.daemonUrl)}/legion/v1/gh-token`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ grantId: grantFrom(deps.env) }),
  });
  if (!response.ok) {
    throw new CliError(`Unable to redeem LEGION_GRANT (${response.status})`);
  }
  const payload = GhTokenResponseSchema.safeParse(await response.json());
  if (!payload.success) {
    throw new CliError("Daemon returned an invalid GitHub credential response");
  }
  const exitCode = await deps.spawnGh(args, { ...deps.env, GH_TOKEN: payload.data.token });
  if (exitCode !== 0) throw new CliError(`gh exited with status ${exitCode}`, exitCode);
}

export async function cmdCredential(deps: CredentialCommandDeps): Promise<void> {
  await deps.readStdin();
  const response = await deps.fetch(
    `${daemonUrl(deps.env, deps.daemonUrl)}/legion/v1/git-credential`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ grantId: grantFrom(deps.env) }),
    }
  );
  if (!response.ok) {
    throw new CliError(`Unable to redeem LEGION_GRANT (${response.status})`);
  }
  const credential = await response.text();
  if (!credential.startsWith("username=") || !credential.includes("\npassword=")) {
    throw new CliError("Daemon returned an invalid git credential response");
  }
  deps.write(credential.endsWith("\n") ? credential : `${credential}\n`);
}

async function readStdin(): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of Bun.stdin.stream()) chunks.push(Buffer.from(chunk));
  return Buffer.concat(chunks).toString("utf8");
}

function requireHandoffPhase(value: unknown, name: string): HandoffPhase {
  if (!isHandoffPhase(value)) {
    throw new CliError(`Invalid phase for ${name}: expected one of ${HANDOFF_PHASES.join(", ")}`);
  }
  return value;
}

function parseHandoffData(raw: string): Record<string, unknown> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new CliError("Invalid JSON handoff data");
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new CliError("Handoff data must be a JSON object");
  }
  for (const field of ["schemaVersion", "phase", "completed"]) {
    if (field in parsed) throw new CliError(`Handoff data field ${field} is not allowed`);
  }
  return parsed as Record<string, unknown>;
}

function loadStartConfig(
  project: string | undefined,
  configPath: string | undefined
): DaemonConfig {
  let configFile: LoadedConfigFile | undefined;
  if (configPath) {
    const absolutePath = fs.realpathSync(configPath);
    configFile = loadConfigFromFile(fs.readFileSync(absolutePath, "utf8"), fs.realpathSync("."));
  } else if (fs.existsSync("legion.yaml")) {
    configFile = loadConfigFromFile(fs.readFileSync("legion.yaml", "utf8"), process.cwd());
  }
  return resolveDaemonConfig({
    env: process.env,
    configFile,
    cliOverrides: project ? { legionId: project } : undefined,
  }).config;
}

async function cmdStart(
  project: string | undefined,
  configPath: string | undefined
): Promise<void> {
  const config = loadStartConfig(project, configPath);
  const daemon = await startDaemon(config);
  const paths = resolveLegionPaths(process.env, os.homedir());
  const port = daemon.server.port;
  if (!port) throw new Error("Daemon did not bind a TCP port");
  await writeLegionEntry(paths.legionsFile, config.legionId, {
    port,
    pid: process.pid,
    startedAt: new Date().toISOString(),
  });
}

async function cmdStop(project: string): Promise<void> {
  const paths = resolveLegionPaths(process.env, os.homedir());
  const entry = await findLegionByProjectId(paths.legionsFile, project);
  if (!entry) throw new CliError(`No daemon registered for ${project}`);
  if (isPidAlive(entry.pid)) process.kill(entry.pid, "SIGTERM");
  await removeLegionEntry(paths.legionsFile, project);
}

async function cmdRestart(project: string, configPath: string | undefined): Promise<void> {
  await cmdStop(project);
  await cmdStart(project, configPath);
}

async function cmdStatus(project: string): Promise<void> {
  const paths = resolveLegionPaths(process.env, os.homedir());
  const entry = await findLegionByProjectId(paths.legionsFile, project);
  if (!entry || !isPidAlive(entry.pid)) {
    console.log(`Legion ${project}: not running`);
    return;
  }
  const response = await fetch(`http://127.0.0.1:${entry.port}/legion/v1/state`);
  if (!response.ok) throw new CliError(`Daemon for ${project} returned ${response.status}`);
  const state: unknown = await response.json();
  console.log(JSON.stringify({ project, pid: entry.pid, port: entry.port, state }, null, 2));
}

async function cmdLegions(): Promise<void> {
  const paths = resolveLegionPaths(process.env, os.homedir());
  console.log(JSON.stringify(await readLegionsRegistry(paths.legionsFile), null, 2));
}

function controllerSecret(): string {
  const secret = process.env.LEGION_CONTROLLER_SECRET;
  if (!secret) throw new CliError("LEGION_CONTROLLER_SECRET is required for controller commands");
  return secret;
}

async function postController(pathname: string, body: Record<string, unknown>): Promise<unknown> {
  const response = await fetch(`${daemonUrl(process.env)}${pathname}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ ...body, secret: controllerSecret() }),
  });
  const payload: unknown = await response.json();
  if (!response.ok)
    throw new CliError(`Daemon request failed (${response.status}): ${JSON.stringify(payload)}`);
  return payload;
}

async function runCli(action: () => Promise<void>): Promise<void> {
  try {
    await action();
  } catch (error) {
    if (error instanceof CliError) {
      console.error(error.message);
      process.exit(error.code);
      return;
    }
    throw error;
  }
}
async function runHandoff(action: () => Promise<void>, operation: string): Promise<void> {
  try {
    await action();
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    console.error(`[handoff] Failed to ${operation}: ${message}`);
    process.exit(error instanceof CliError ? error.code : 1);
  }
}

export const handoffCommand = defineCommand({
  meta: { name: "handoff", description: "Read and write local handoff files" },
  subCommands: {
    write: defineCommand({
      meta: { name: "write", description: "Write phase handoff data" },
      args: {
        phase: { type: "string", required: true, description: "Handoff phase" },
        data: { type: "string", description: "JSON data (reads stdin when omitted)" },
        workspace: { type: "string", description: "Workspace directory" },
      },
      run: ({ args }) =>
        runHandoff(async () => {
          const workspace = String(args.workspace ?? process.cwd());
          const phase = requireHandoffPhase(args.phase, "phase");
          const raw = args.data ? String(args.data) : await readStdin();
          writePhaseHandoff(workspace, phase, parseHandoffData(raw));
          console.log(
            `[handoff] Wrote ${phase} handoff to ${path.join(workspace, ".legion", `${phase}.json`)}`
          );
        }, "write handoff"),
    }),
    read: defineCommand({
      meta: { name: "read", description: "Read phase handoff data" },
      args: {
        phase: { type: "string", description: "Optional handoff phase" },
        workspace: { type: "string", description: "Workspace directory" },
      },
      run: ({ args }) =>
        runCli(async () => {
          const workspace = String(args.workspace ?? process.cwd());
          const value = args.phase
            ? readPhaseHandoff(workspace, requireHandoffPhase(args.phase, "phase"))
            : readAllHandoffs(workspace);
          console.log(JSON.stringify(value, null, 2));
        }),
    }),
    messages: defineCommand({
      meta: { name: "messages", description: "Read handoff messages" },
      args: { workspace: { type: "string", description: "Workspace directory" } },
      run: ({ args }) =>
        runCli(async () => {
          console.log(
            JSON.stringify(readMessages(String(args.workspace ?? process.cwd())), null, 2)
          );
        }),
    }),
    message: defineCommand({
      meta: { name: "message", description: "Write a handoff message" },
      args: {
        from: { type: "string", required: true, description: "Source phase" },
        to: { type: "string", required: true, description: "Destination phase" },
        body: { type: "string", required: true, description: "Message body" },
        workspace: { type: "string", description: "Workspace directory" },
      },
      run: ({ args }) =>
        runHandoff(async () => {
          const workspace = String(args.workspace ?? process.cwd());
          const from = requireHandoffPhase(args.from, "from");
          const to = requireHandoffPhase(args.to, "to");
          writeMessage(workspace, { from, to, body: String(args.body) });
          console.log(`[handoff] Wrote message from ${from} to ${to}`);
        }, "write message"),
    }),
  },
});

const startCommand = defineCommand({
  meta: { name: "start", description: "Start the Legion daemon" },
  args: {
    project: { type: "positional", description: "GitHub project owner/number" },
    config: { type: "string", description: "Path to legion.yaml" },
  },
  run: ({ args }) =>
    runCli(() => cmdStart(args.project as string | undefined, args.config as string | undefined)),
});

const stopCommand = defineCommand({
  meta: { name: "stop", description: "Stop the Legion daemon" },
  args: {
    project: { type: "positional", required: true, description: "GitHub project owner/number" },
  },
  run: ({ args }) => runCli(() => cmdStop(String(args.project))),
});

const restartCommand = defineCommand({
  meta: { name: "restart", description: "Restart the Legion daemon" },
  args: {
    project: { type: "positional", required: true, description: "GitHub project owner/number" },
    config: { type: "string", description: "Path to legion.yaml" },
  },
  run: ({ args }) =>
    runCli(() => cmdRestart(String(args.project), args.config as string | undefined)),
});

const statusCommand = defineCommand({
  meta: { name: "status", description: "Show daemon status" },
  args: {
    project: { type: "positional", required: true, description: "GitHub project owner/number" },
  },
  run: ({ args }) => runCli(() => cmdStatus(String(args.project))),
});

const legionsCommand = defineCommand({
  meta: { name: "legions", description: "List registered Legion daemons" },
  run: () => runCli(cmdLegions),
});

const ghCommand = defineCommand({
  meta: { name: "gh", description: "Run gh with a session-bound GitHub token" },
  run: () =>
    runCli(() => {
      const separator = process.argv.lastIndexOf("--");
      const args = separator === -1 ? [] : process.argv.slice(separator + 1);
      if (args.length === 0) throw new CliError("Usage: legion gh -- <gh args…>");
      return cmdGh(args, { env: process.env, fetch, spawnGh });
    }),
});

const credentialCommand = defineCommand({
  meta: { name: "credential", description: "Git credential helper for Legion grants" },
  run: () =>
    runCli(() =>
      cmdCredential({
        env: process.env,
        fetch,
        readStdin,
        write: (value) => process.stdout.write(value),
      })
    ),
});

const stateCommand = defineCommand({
  meta: { name: "state", description: "Read daemon state" },
  args: { json: { type: "boolean", default: false, description: "Output JSON" } },
  run: () =>
    runCli(async () => {
      const response = await fetch(`${daemonUrl(process.env)}/legion/v1/state`);
      if (!response.ok) throw new CliError(`Daemon request failed (${response.status})`);
      console.log(JSON.stringify(await response.json(), null, 2));
    }),
});

const approveCommand = defineCommand({
  meta: { name: "approve", description: "Apply a human approval" },
  args: { issue: { type: "positional", required: true, description: "Issue key" } },
  run: ({ args }) =>
    runCli(async () =>
      console.log(
        JSON.stringify(
          await postController("/legion/v1/gates/approve", { issue: String(args.issue) })
        )
      )
    ),
});

const admitCommand = defineCommand({
  meta: { name: "admit", description: "Admit a root issue" },
  args: { issue: { type: "positional", required: true, description: "Issue key" } },
  run: ({ args }) =>
    runCli(async () =>
      console.log(
        JSON.stringify(await postController("/legion/v1/admission", { issue: String(args.issue) }))
      )
    ),
});

const backlogCommand = defineCommand({
  meta: { name: "backlog", description: "Mark an issue as deliberately backlogged" },
  args: {
    issue: { type: "positional", required: true, description: "Issue key" },
    marker: { type: "string", required: true, description: "Backlog marker" },
  },
  run: ({ args }) =>
    runCli(async () => {
      console.log(
        JSON.stringify(
          await postController("/legion/v1/backlog", {
            issue: String(args.issue),
            marker: String(args.marker),
          })
        )
      );
    }),
});

export const mainCommand = defineCommand({
  meta: { name: "legion", description: "Wake-driven Legion daemon" },
  subCommands: {
    start: startCommand,
    stop: stopCommand,
    restart: restartCommand,
    status: statusCommand,
    legions: legionsCommand,
    handoff: handoffCommand,
    gh: ghCommand,
    credential: credentialCommand,
    state: stateCommand,
    approve: approveCommand,
    admit: admitCommand,
    backlog: backlogCommand,
  },
});

if (import.meta.main) runMain(mainCommand);
