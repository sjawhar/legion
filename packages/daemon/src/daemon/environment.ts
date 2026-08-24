import { accessSync, constants, realpathSync } from "node:fs";
import path from "node:path";
import type { CommandRunner, CommandRunnerOptions } from "../state/fetch";

const REQUIRED_DAEMON_TOOLS = ["jj", "git", "gh", "tmux"] as const;
type DaemonTool = (typeof REQUIRED_DAEMON_TOOLS)[number];

type ResolveExecutable = (command: string, searchPath?: string) => string | undefined;

export interface ResolveDaemonEnvironmentDeps {
  readonly env?: NodeJS.ProcessEnv;
  readonly resolveExecutable?: ResolveExecutable;
  readonly run: CommandRunner;
}

export interface FullMiseEnvironment extends NodeJS.ProcessEnv {
  readonly PATH: string;
}

export interface DaemonEnvironment {
  readonly commands: Record<DaemonTool, string>;
  readonly ompInvocation: string;
  readonly paneEnv: FullMiseEnvironment;
}

function defaultResolveExecutable(command: string, searchPath?: string): string | undefined {
  const candidates = path.isAbsolute(command)
    ? [command]
    : (searchPath ?? "")
        .split(path.delimiter)
        .filter(Boolean)
        .map((directory) => path.join(directory, command));

  for (const candidate of candidates) {
    try {
      accessSync(candidate, constants.X_OK);
      return realpathSync(candidate);
    } catch {}
  }
  return undefined;
}

function configuredPath(env: NodeJS.ProcessEnv, tool: string): string | undefined {
  const configured = env[`LEGION_${tool.toUpperCase()}_PATH`];
  if (configured === undefined || configured === "") return undefined;
  if (!path.isAbsolute(configured)) {
    throw new Error(`LEGION_${tool.toUpperCase()}_PATH must be an absolute executable path`);
  }
  return configured;
}

function resolveConfiguredOrFound(
  tool: string,
  env: NodeJS.ProcessEnv,
  searchPath: string | undefined,
  resolveExecutable: ResolveExecutable
): string | undefined {
  const configured = configuredPath(env, tool);
  return resolveExecutable(configured ?? tool, configured ? undefined : searchPath);
}

function parseMiseEnvironment(stdout: string): FullMiseEnvironment {
  let parsed: unknown;
  try {
    parsed = JSON.parse(stdout);
  } catch {
    throw new Error("[legion] mise env --json returned invalid JSON");
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new Error("[legion] mise env --json did not return an environment object");
  }

  const entries = Object.entries(parsed);
  if (entries.some(([, value]) => typeof value !== "string")) {
    throw new Error("[legion] mise env --json returned a non-string environment value");
  }
  const environment = Object.fromEntries(entries) as NodeJS.ProcessEnv;
  if (!environment.PATH) {
    throw new Error("[legion] mise env --json did not provide PATH");
  }
  return environment as FullMiseEnvironment;
}

async function fullMiseEnvironment(
  mise: string,
  env: NodeJS.ProcessEnv,
  run: CommandRunner
): Promise<FullMiseEnvironment> {
  const result = await run([mise, "env", "--json"]);
  if (result.exitCode !== 0) {
    const detail = [result.stderr.trim(), result.stdout.trim()].filter(Boolean).join("\n");
    throw new Error(
      `[legion] Could not load the full mise environment${detail ? `: ${detail}` : ""}`
    );
  }
  return { ...env, ...parseMiseEnvironment(result.stdout) } as FullMiseEnvironment;
}

function miseToolFromInvocation(invocation: string): string | undefined {
  return /^mise x (\S+) -- omp$/.exec(invocation)?.[1];
}

async function resolveOmpInvocation(
  invocation: string,
  mise: string,
  env: NodeJS.ProcessEnv,
  resolveExecutable: ResolveExecutable,
  run: CommandRunner
): Promise<string> {
  const configured = configuredPath(env, "omp");
  if (configured) {
    const resolved = resolveExecutable(configured);
    if (resolved) return resolved;
    throw new Error(`[legion] LEGION_OMP_PATH is not an executable: ${configured}`);
  }

  const tool = miseToolFromInvocation(invocation);
  if (!tool) {
    throw new Error(
      "[legion] OMP invocation must be 'mise x <tool> -- omp'. Set LEGION_OMP_PATH to an absolute executable path."
    );
  }

  const result = await run([mise, "where", tool]);
  const installDir = result.stdout.trim();
  const resolved =
    result.exitCode === 0 ? resolveExecutable(path.join(installDir, "bin", "omp")) : undefined;
  if (resolved) return resolved;

  const detail = [result.stderr.trim(), result.stdout.trim()].filter(Boolean).join("\n");
  throw new Error(
    `[legion] Could not resolve pinned OMP binary for ${tool}${detail ? `: ${detail}` : ""}. ` +
      "Set LEGION_OMP_PATH to an absolute executable path."
  );
}

/**
 * Resolves all commands before the daemon owns state or accepts work. mise env
 * restores the user's complete tool environment; every daemon child then gets
 * explicit tool paths and that same PATH instead of the launcher context.
 */
export async function resolveDaemonEnvironment(
  ompInvocation: string,
  deps: ResolveDaemonEnvironmentDeps
): Promise<DaemonEnvironment> {
  const env = deps.env ?? process.env;
  const resolveExecutable = deps.resolveExecutable ?? defaultResolveExecutable;
  const mise = resolveConfiguredOrFound("mise", env, env.PATH, resolveExecutable);
  if (!mise) {
    throw new Error(
      "[legion] Missing required daemon tool: mise (set LEGION_MISE_PATH to an absolute executable path)"
    );
  }

  const paneEnv = await fullMiseEnvironment(mise, env, deps.run);
  const missing: string[] = [];
  const commands = {} as Record<DaemonTool, string>;
  for (const tool of REQUIRED_DAEMON_TOOLS) {
    const resolved = resolveConfiguredOrFound(tool, env, paneEnv.PATH, resolveExecutable);
    if (resolved) commands[tool] = resolved;
    else missing.push(tool);
  }
  if (missing.length > 0) {
    throw new Error(
      `[legion] Missing required daemon tools: ${missing
        .map(
          (tool) => `${tool} (set LEGION_${tool.toUpperCase()}_PATH to an absolute executable path)`
        )
        .join(", ")}`
    );
  }

  return {
    commands,
    ompInvocation: await resolveOmpInvocation(
      ompInvocation,
      mise,
      paneEnv,
      resolveExecutable,
      deps.run
    ),
    paneEnv,
  };
}

/** Runs known daemon tools by their startup-resolved path and with the full mise environment. */
export function createDaemonRunner(
  environment: DaemonEnvironment,
  runner: CommandRunner
): CommandRunner {
  return (command, options) => {
    const tool = command[0] as DaemonTool;
    const executable = environment.commands[tool];
    const resolvedCommand = executable ? [executable, ...command.slice(1)] : command;
    const resolvedOptions: CommandRunnerOptions = {
      ...options,
      env: { ...environment.paneEnv, ...options?.env },
    };
    return runner(resolvedCommand, resolvedOptions);
  };
}
