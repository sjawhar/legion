#!/usr/bin/env bun
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  HANDOFF_PHASES,
  type HandoffPhase,
  isHandoffPhase,
  LegionDaemonApi,
} from "@legion/contracts";
import { defineCommand, runMain } from "citty";
import { verifyLegionPluginLoaded, verifyOmpAgentsCapability } from "../daemon/boot-probes";
import {
  type DaemonConfig,
  type LoadConfigFileOptions,
  type LoadedConfigFile,
  loadConfigFromFile,
  resolveDaemonConfig,
} from "../daemon/config";
import { buildGitHubTokenEnv } from "../daemon/github-app-env";
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
import { type CommandRunner, defaultRunner } from "../state/fetch";
import { CliError } from "./errors";
import {
  cmdWorkerShim,
  cmdWorkerShimConnect,
  defaultWorkerShimDeps,
  resolveWorkerShimTarget,
} from "./worker-shim";

type Fetch = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;
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

interface HandoffCompleteCommandDeps {
  env: NodeJS.ProcessEnv;
  fetch: Fetch;
  daemonUrl?: string;
}

interface ProbeImageCommandDeps {
  env: NodeJS.ProcessEnv;
  runner: CommandRunner;
  sleep(ms: number): Promise<void>;
  readPluginManifest(manifestPath: string): Promise<string>;
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

/** True when the forwarded `gh` argv would merge a PR: a `pr … merge` subcommand invocation (the
 * non-flag tokens contain `pr` followed later by `merge` — `gh pr merge`'s own flags like
 * `--repo <value>` insert extra non-flag tokens between them without changing the subcommand), or
 * a raw REST `gh api` call whose path token ends in `/merge`. A merge invocation is redeemed with
 * `merge: true`, which the daemon grants only to the controller's own grant; every phase-worker
 * grant is refused server-side. */
function isPrMergeInvocation(args: string[]): boolean {
  const positional = args.filter((arg) => !arg.startsWith("-"));
  const prIndex = positional.indexOf("pr");
  if (prIndex !== -1 && positional.slice(prIndex + 1).includes("merge")) return true;
  return positional.includes("api") && positional.some((token) => token.endsWith("/merge"));
}

/** The `error` field of a daemon JSON error body, or undefined when the body is not one. */
function daemonErrorReason(bodyText: string): string | undefined {
  try {
    const parsed: unknown = JSON.parse(bodyText);
    if (typeof parsed === "object" && parsed !== null && "error" in parsed) {
      const { error } = parsed;
      if (typeof error === "string" && error.length > 0) return error;
    }
  } catch {
    // Not JSON: no reason to surface beyond the status.
  }
  return undefined;
}

export async function cmdGh(args: string[], deps: GhCommandDeps): Promise<void> {
  const merge = isPrMergeInvocation(args);
  const response = await deps.fetch(`${daemonUrl(deps.env, deps.daemonUrl)}/legion/v1/gh-token`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ grantId: grantFrom(deps.env), ...(merge ? { merge: true } : {}) }),
  });
  if (!response.ok) {
    const reason = daemonErrorReason(await response.text());
    const suffix = reason === undefined ? "" : `: ${reason}`;
    if (merge && response.status === 403) {
      throw new CliError(`this grant cannot merge; publish READY to the controller${suffix}`);
    }
    throw new CliError(`Unable to redeem LEGION_GRANT (${response.status})${suffix}`);
  }
  const payload = LegionDaemonApi.GitHubToken.response.safeParse(await response.json());
  if (!payload.success) {
    throw new CliError("Daemon returned an invalid GitHub credential response");
  }
  const exitCode = await deps.spawnGh(args, buildGitHubTokenEnv(payload.data.token, deps.env));
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

export async function cmdHandoffComplete(
  summary: string,
  deps: HandoffCompleteCommandDeps
): Promise<void> {
  const response = await deps.fetch(
    `${daemonUrl(deps.env, deps.daemonUrl)}/legion/v1/phase/complete`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ grantId: grantFrom(deps.env), summary }),
    }
  );
  if (!response.ok) {
    const payload = await response.text();
    throw new CliError(`Unable to report phase completion (${response.status}): ${payload}`);
  }
  if (response.status === 202) {
    console.log("[handoff] Warning: phase recorded; no architect was live to receive the summary");
    return;
  }
  console.log("[handoff] Reported phase completion");
}

/** The daemon's two boot probes (boot-probes.ts) against one OMP executable, with no launch prefix
 * — an image carries no `secrets` wrapper. The worker image build runs this as its last step; a
 * failure is the daemon's own probe message, exit 1, so a broken image never publishes. */
export async function cmdProbeImage(
  omp: string | undefined,
  deps: ProbeImageCommandDeps
): Promise<void> {
  const ompPath = omp ?? deps.env.LEGION_OMP_PATH;
  if (!ompPath) {
    throw new CliError(
      "probe-image: set LEGION_OMP_PATH (or pass --omp) to the OMP executable to probe"
    );
  }
  try {
    await verifyOmpAgentsCapability(ompPath, [], deps.runner, deps.sleep);
    await verifyLegionPluginLoaded(ompPath, [], deps.runner, deps.readPluginManifest, deps.sleep);
  } catch (error) {
    throw new CliError(error instanceof Error ? error.message : String(error));
  }
  console.log(`probe-image: OK (${ompPath})`);
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
  configPath: string | undefined,
  env: NodeJS.ProcessEnv,
  options: LoadConfigFileOptions = {}
): DaemonConfig {
  let configFile: LoadedConfigFile | undefined;
  if (configPath) {
    const absolutePath = fs.realpathSync(configPath);
    configFile = loadConfigFromFile(
      fs.readFileSync(absolutePath, "utf8"),
      fs.realpathSync("."),
      options
    );
  } else if (fs.existsSync("legion.yaml")) {
    configFile = loadConfigFromFile(fs.readFileSync("legion.yaml", "utf8"), process.cwd(), options);
  }
  return resolveDaemonConfig({
    env,
    configFile,
    cliOverrides: project ? { legionId: project } : undefined,
  }).config;
}

export async function cmdCheckConfig(
  project: string | undefined,
  configPath: string | undefined,
  env: NodeJS.ProcessEnv
): Promise<void> {
  const config = loadStartConfig(project, configPath, env, { resolveSecrets: false });
  console.log(`Config OK: project=${config.project}`);
}

async function cmdStart(
  project: string | undefined,
  configPath: string | undefined,
  env: NodeJS.ProcessEnv
): Promise<void> {
  const config = loadStartConfig(project, configPath, env);
  const daemon = await startDaemon(config);
  const paths = resolveLegionPaths(env, os.homedir());
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

async function cmdRestart(
  project: string,
  configPath: string | undefined,
  env: NodeJS.ProcessEnv
): Promise<void> {
  await cmdStop(project);
  await cmdStart(project, configPath, env);
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

/** `LEGION_CONTROLLER_SECRET_FILE` (the 0600 file the daemon hands its controller pane; trimmed
 * contents) ahead of `LEGION_CONTROLLER_SECRET` (an interactive operator's own export). A set
 * pointer is authoritative: a missing, unreadable, or empty file is an error naming both the
 * variable and the path, never a fallback to the plain variable. */
export function resolveControllerSecret(env: NodeJS.ProcessEnv): string {
  const file = env.LEGION_CONTROLLER_SECRET_FILE;
  if (file !== undefined) {
    let contents: string;
    try {
      contents = fs.readFileSync(file, "utf8");
    } catch (error) {
      throw new CliError(
        `LEGION_CONTROLLER_SECRET_FILE names ${file}, which could not be read: ${error instanceof Error ? error.message : String(error)}`
      );
    }
    const secret = contents.trim();
    if (!secret) throw new CliError(`LEGION_CONTROLLER_SECRET_FILE names ${file}, which is empty`);
    return secret;
  }
  const secret = env.LEGION_CONTROLLER_SECRET;
  if (!secret) {
    throw new CliError(
      "LEGION_CONTROLLER_SECRET or LEGION_CONTROLLER_SECRET_FILE is required for controller commands"
    );
  }
  return secret;
}

async function postController(pathname: string, body: Record<string, unknown>): Promise<unknown> {
  const response = await fetch(`${daemonUrl(process.env)}${pathname}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ ...body, secret: resolveControllerSecret(process.env) }),
  });
  const payload: unknown = await response.json();
  if (!response.ok)
    throw new CliError(`Daemon request failed (${response.status}): ${JSON.stringify(payload)}`);
  return payload;
}

async function cmdSetIssueStatus(issue: string, status: string): Promise<void> {
  console.log(JSON.stringify(await postController("/legion/v1/issues/status", { issue, status })));
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
        data: {
          type: "string",
          description: "JSON data (reads stdin when omitted)",
        },
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
      args: {
        workspace: { type: "string", description: "Workspace directory" },
      },
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
        to: {
          type: "string",
          required: true,
          description: "Destination phase",
        },
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
    complete: defineCommand({
      meta: {
        name: "complete",
        description:
          "Report phase completion to this issue's architect. Authenticates exactly like " +
          "`legion gh`/`legion credential`: reads LEGION_GRANT from the environment (the " +
          "pi-envoy worker extension injects it into every worker bash call) and redeems it " +
          "for this worker's issue/role/session — never a live session secret in the request.",
      },
      args: {
        summary: {
          type: "string",
          required: true,
          description: "Two-sentence summary of this phase for the architect",
        },
      },
      run: ({ args }) =>
        runHandoff(
          () => cmdHandoffComplete(String(args.summary), { env: process.env, fetch }),
          "report phase completion"
        ),
    }),
  },
});

const startCommand = defineCommand({
  meta: { name: "start", description: "Start the Legion daemon" },
  args: {
    project: { type: "positional", description: "Legion daemon identity (legionId)" },
    config: { type: "string", description: "Path to legion.yaml" },
    checkConfig: {
      type: "boolean",
      default: false,
      description: "Load and validate config, then exit without starting the daemon",
    },
  },
  run: ({ args }) => {
    const project = args.project as string | undefined;
    const configPath = args.config as string | undefined;
    return runCli(() =>
      args.checkConfig
        ? cmdCheckConfig(project, configPath, process.env)
        : cmdStart(project, configPath, process.env)
    );
  },
});

const stopCommand = defineCommand({
  meta: { name: "stop", description: "Stop the Legion daemon" },
  args: {
    project: {
      type: "positional",
      required: true,
      description: "Legion daemon identity (legionId)",
    },
  },
  run: ({ args }) => runCli(() => cmdStop(String(args.project))),
});

const restartCommand = defineCommand({
  meta: { name: "restart", description: "Restart the Legion daemon" },
  args: {
    project: {
      type: "positional",
      required: true,
      description: "Legion daemon identity (legionId)",
    },
    config: { type: "string", description: "Path to legion.yaml" },
  },
  run: ({ args }) =>
    runCli(() => cmdRestart(String(args.project), args.config as string | undefined, process.env)),
});

const statusCommand = defineCommand({
  meta: {
    name: "status",
    description:
      "Show daemon status for a project, or set an issue's Dispatch lifecycle status " +
      "(todo|backlog|icebox) when a status argument is given",
  },
  args: {
    target: {
      type: "positional",
      required: true,
      description: "Legion daemon identity (legionId), or an issue key when status is also given",
    },
    status: {
      type: "positional",
      description: "Lifecycle status to set on target (todo|backlog|icebox)",
    },
  },
  run: ({ args }) =>
    runCli(() =>
      args.status
        ? cmdSetIssueStatus(String(args.target), String(args.status))
        : cmdStatus(String(args.target))
    ),
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
  meta: {
    name: "credential",
    description: "Git credential helper for Legion grants",
  },
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

const workerShimCommand = defineCommand({
  meta: {
    name: "worker-shim",
    description:
      "Bridge a headless OMP worker to the daemon over a unix socket (--socket) or a reverse-dialed TCP stream (--connect)",
  },
  args: {
    socket: { type: "string", description: "Unix socket path to listen on (tmux runtime)" },
    connect: {
      type: "string",
      description:
        "Daemon worker stream endpoint to dial, tcp://<host>:<port> (Kubernetes runtime)",
    },
    bootTokenFile: {
      type: "string",
      description:
        "File whose trimmed contents are the boot token sent in the hello line (--connect only)",
    },
  },
  run: ({ args }) =>
    runCli(async () => {
      // The first `--` marks the boundary here (unlike `gh`'s `lastIndexOf` above): everything
      // after it is the wrapped OMP argv verbatim, which may itself contain a `--` of its own
      // (e.g. a further-nested command), so taking the first one keeps that intact.
      const separator = process.argv.indexOf("--");
      const argv = separator === -1 ? [] : process.argv.slice(separator + 1);
      const target = resolveWorkerShimTarget({
        socket: args.socket as string | undefined,
        connect: args.connect as string | undefined,
        bootTokenFile: args.bootTokenFile as string | undefined,
      });
      const deps = defaultWorkerShimDeps();
      const exitCode =
        target.mode === "socket"
          ? await cmdWorkerShim(target.socketPath, argv, deps)
          : await cmdWorkerShimConnect(target.endpoint, target.bootToken, argv, deps);
      process.exit(exitCode);
    }),
});

const stateCommand = defineCommand({
  meta: { name: "state", description: "Read daemon state" },
  args: {
    json: { type: "boolean", default: false, description: "Output JSON" },
  },
  run: () =>
    runCli(async () => {
      const response = await fetch(`${daemonUrl(process.env)}/legion/v1/state`);
      if (!response.ok) throw new CliError(`Daemon request failed (${response.status})`);
      console.log(JSON.stringify(await response.json(), null, 2));
    }),
});

const probeImageCommand = defineCommand({
  meta: {
    name: "probe-image",
    description: "Run the daemon's OMP boot probes against this image's OMP executable",
    hidden: true,
  },
  args: { omp: { type: "string", description: "OMP executable (default: $LEGION_OMP_PATH)" } },
  run: ({ args }) =>
    runCli(() =>
      cmdProbeImage(args.omp as string | undefined, {
        env: process.env,
        runner: defaultRunner,
        sleep: (ms) => Bun.sleep(ms),
        readPluginManifest: (manifestPath) => fs.promises.readFile(manifestPath, "utf8"),
      })
    ),
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
    "worker-shim": workerShimCommand,
    state: stateCommand,
    "probe-image": probeImageCommand,
  },
});

if (import.meta.main) runMain(mainCommand);
