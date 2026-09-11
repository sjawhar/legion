import { accessSync, constants, realpathSync } from "node:fs";
import { chmod, mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
import type { CommandRunner, CommandRunnerOptions } from "../state/fetch";

const REQUIRED_DAEMON_TOOLS = ["jj", "git", "gh", "tmux"] as const;
type DaemonTool = (typeof REQUIRED_DAEMON_TOOLS)[number];

type ResolveExecutable = (command: string, searchPath?: string) => string | undefined;

export interface ResolveDaemonEnvironmentDeps {
  readonly env?: NodeJS.ProcessEnv;
  readonly resolveExecutable?: ResolveExecutable;
  readonly run: CommandRunner;
  /** Legion daemon state directory. The `legion` CLI launcher (see `legionCliLauncherScript`) is
   * written to `<stateDir>/bin/legion` and that directory is prepended to `paneEnv.PATH`, so every
   * spawned pane's ambient `legion` resolves to a CLI that matches this daemon instead of
   * whatever (if anything) happens to be installed on the operator's own PATH. */
  readonly stateDir: string;
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

/** `DISPATCH_URL`/`DISPATCH_TOKEN` are configured pane-only exports: the only place they belong
 * is the explicit, config-driven `-e` pairs `processes.ts` adds to a spawned pane's own tmux
 * environment. `DISPATCH_MCP_URL` is a retired alias with no legitimate destination at all — the
 * daemon never emits it and strips it from every child process it spawns (an `-e` pair can only
 * add or override a key for a new pane, never remove one the pane would otherwise inherit from
 * the tmux server's own environment, so this key must never reach that environment in the first
 * place). Every other child process the daemon spawns (mise/tool resolution here,
 * `executePrivateKeyCommand`'s `sh -c` in `config.ts`, GitHub App role/`gh` CLI children in
 * `github-app-env.ts`, and any other daemon subprocess) must never see any of the three, even
 * when the daemon's own process (or mise's) happens to carry one for unrelated reasons. Shared
 * by `fullMiseEnvironment`/`resolveOmpInvocation` below, by `config.ts`'s
 * `executePrivateKeyCommand`, and by `github-app-env.ts`'s base-env copy, so every consumer
 * strips the same three keys the same way. */
const DISPATCH_ENV_KEYS = ["DISPATCH_TOKEN", "DISPATCH_URL", "DISPATCH_MCP_URL"] as const;

export function stripDispatchEnv<T extends NodeJS.ProcessEnv>(env: T): T {
  const stripped = { ...env };
  for (const key of DISPATCH_ENV_KEYS) delete stripped[key];
  return stripped;
}

async function fullMiseEnvironment(
  mise: string,
  env: NodeJS.ProcessEnv,
  run: CommandRunner
): Promise<FullMiseEnvironment> {
  const result = await run([mise, "env", "--json"], { env: stripDispatchEnv(env) });
  if (result.exitCode !== 0) {
    // stdout is `mise`'s env dump on partial success — never interpolated here, only stderr.
    const detail = result.stderr.trim();
    throw new Error(
      `[legion] Could not load the full mise environment (exit ${result.exitCode})${detail ? `: ${detail}` : ""}`
    );
  }
  const merged: NodeJS.ProcessEnv = {
    ...env,
    ...parseMiseEnvironment(result.stdout),
  };
  return stripDispatchEnv(merged) as FullMiseEnvironment;
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

  const result = await run([mise, "where", tool], { env: stripDispatchEnv(env) });
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

/** Builds the `<stateDir>/bin/legion` launcher script: a thin `sh` wrapper that re-execs this
 * same running daemon's own runtime/entry, so every pane the daemon spawns resolves an ambient
 * `legion` invocation (`legion state`, `legion gh`, `legion credential`, `legion handoff`, …) to a
 * CLI build that matches the daemon that set `LEGION_STATE_DIR`/`LEGION_DAEMON_URL` for it —
 * never a stale or mismatched `legion` some other install left earlier on PATH. Two shapes,
 * detected from the running process's own entry:
 * - Source under bun (`argv1` ends with `cli/index.ts`, and this isn't secretly a compiled binary
 *   whose bundled entry happens to match that suffix — hence the `bunMain !== execPath` guard):
 *   re-exec bun against that same absolute source entry.
 * - A compiled `legion` binary (`bunMain === execPath`) or any other/no `.ts` entry: re-exec the
 *   runtime directly, since a compiled binary parses its own subcommands from argv. */
export function legionCliLauncherScript(
  execPath: string,
  argv1: string | undefined,
  bunMain: string | undefined
): string {
  if (!path.isAbsolute(execPath)) {
    throw new Error(
      "[legion] process.execPath must be an absolute path to build the legion CLI launcher"
    );
  }
  const isSourceEntry =
    bunMain !== execPath && argv1 !== undefined && argv1.endsWith("cli/index.ts");
  if (!isSourceEntry) {
    return `#!/bin/sh\nexec "${execPath}" "$@"\n`;
  }
  return `#!/bin/sh\nexec "${execPath}" "${path.resolve(argv1)}" "$@"\n`;
}

/** Writes the `legion` CLI launcher (mode 0755, no secrets) to `<stateDir>/bin/legion` and
 * returns that directory, so callers can prepend it to a pane's PATH. */
async function installLegionCliLauncher(stateDir: string): Promise<string> {
  const binDir = path.join(stateDir, "bin");
  await mkdir(binDir, { recursive: true });
  const launcherPath = path.join(binDir, "legion");
  const script = legionCliLauncherScript(process.execPath, process.argv[1], Bun.main);
  await writeFile(launcherPath, script, "utf8");
  await chmod(launcherPath, 0o755);
  return binDir;
}

/**
 * Resolves all commands before the daemon owns state or accepts work. mise env
 * restores the user's complete tool environment; every daemon child then gets
 * explicit tool paths and that same PATH instead of the launcher context. Also installs the
 * `legion` CLI launcher (see `legionCliLauncherScript`) and prepends its directory to the pane
 * PATH every root, worker, and controller pane inherits.
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

  const miseEnv = await fullMiseEnvironment(mise, env, deps.run);
  const legionBinDir = await installLegionCliLauncher(deps.stateDir);
  const paneEnv: FullMiseEnvironment = {
    ...miseEnv,
    PATH: `${legionBinDir}${path.delimiter}${miseEnv.PATH}`,
  };
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
