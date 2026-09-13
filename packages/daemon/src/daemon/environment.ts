import { accessSync, constants, realpathSync } from "node:fs";
import { chmod, mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
import type { CommandRunner, CommandRunnerOptions } from "../state/fetch";
import { pathWithoutWorkerBin } from "./worker-bin";

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

/** Every variable a pane process reads from the daemon's own environment: Oh My Pi and its plugins
 * (HOME, the XDG base dirs, OMP_PROFILE/PI_PROFILE), jj/git/gh, the `legion` CLI launcher, mise (the
 * shims panes execute), the configured `omp_launch_prefix` (`secrets` needs HOME and, optionally,
 * SECRETSD_SOCK/XDG_RUNTIME_DIR), tmux itself (TMUX_TMPDIR, SHELL), locale and proxy policy. Nothing
 * else the daemon was started with reaches a pane, the private tmux server that hosts every pane
 * (forked by the daemon's first `tmux -L legion-<project>` command under exactly this environment),
 * a start-up probe, or any other daemon child that runs through `createDaemonRunner`: the GitHub App
 * private keys, provider keys, Dispatch/Envoy secrets, the operator session's OMP_SESSION_ID,
 * JJ_CONFIG overlay, TMUX, SSH agent, and every `LEGION_*` and `DISPATCH_*` value (those are explicit
 * per-pane `-e` pairs in `processes.ts`/`runtime-tmux.ts`, never inherited). Add a name here only
 * with the process that reads it named in the group comment; never a prefix or wildcard. */
export const PANE_ENV_ALLOW_LIST: readonly string[] = [
  // identity, locale, terminal (SHELL: tmux's default-shell for the pane's shell-command)
  "HOME",
  "USER",
  "LOGNAME",
  "SHELL",
  "TERM",
  "TZ",
  "LANG",
  "LANGUAGE",
  "LC_ADDRESS",
  "LC_ALL",
  "LC_COLLATE",
  "LC_CTYPE",
  "LC_IDENTIFICATION",
  "LC_MEASUREMENT",
  "LC_MESSAGES",
  "LC_MONETARY",
  "LC_NAME",
  "LC_NUMERIC",
  "LC_PAPER",
  "LC_TELEPHONE",
  "LC_TIME",
  // directories: temp, the tmux socket dir the daemon's own tmux commands use, XDG base dirs (OMP
  // DirResolver, `legion gh`'s GH_CONFIG_DIR, the secretsd socket)
  "TMPDIR",
  "TMUX_TMPDIR",
  "XDG_CACHE_HOME",
  "XDG_CONFIG_HOME",
  "XDG_DATA_HOME",
  "XDG_RUNTIME_DIR",
  "XDG_STATE_HOME",
  // OMP profile selection (DirResolver)
  "OMP_PROFILE",
  "PI_PROFILE",
  // mise: where the tool store lives, for `mise env`/`mise where` here and the shims panes run
  "MISE_CACHE_DIR",
  "MISE_CONFIG_DIR",
  "MISE_DATA_DIR",
  "MISE_STATE_DIR",
  // secretsd socket override honoured by the `secrets` client and the secretsd OMP extension
  "SECRETSD_SOCK",
  // outbound network policy honoured by OMP/Bun, gh, git, jj
  "HTTP_PROXY",
  "HTTPS_PROXY",
  "NO_PROXY",
  "http_proxy",
  "https_proxy",
  "no_proxy",
  "SSL_CERT_DIR",
  "SSL_CERT_FILE",
  "NODE_EXTRA_CA_CERTS",
  // the PATH `mise env` extends; mise's own PATH replaces it in paneEnv
  "PATH",
];

/** Second line of defence behind the allow-list: a credential-shaped name never reaches a child even
 * when an allowed source carries it (`mise env` output, or an allow-list entry added by mistake). Also
 * the scrub `github-app-env.ts` applies to a `gh` child's base environment. */
const SECRET_LIKE_NAME = /(?:_SECRET|_TOKEN|_GRANT|_API_KEY|_PASSWORD)(?:_FILE)?$|PRIVATE_KEY/;

export function isSecretLikeName(name: string): boolean {
  return SECRET_LIKE_NAME.test(name);
}

function withoutSecretLikeNames(env: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const kept: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(env)) {
    if (value !== undefined && !isSecretLikeName(key)) kept[key] = value;
  }
  return kept;
}

/** The allow-listed subset of the daemon's own environment: what the bootstrap `mise env` call runs
 * under, and the daemon-side half of `paneEnvironment`. */
function allowedDaemonEnvironment(daemonEnv: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const picked: NodeJS.ProcessEnv = {};
  for (const key of PANE_ENV_ALLOW_LIST) {
    const value = daemonEnv[key];
    if (value !== undefined) picked[key] = value;
  }
  return withoutSecretLikeNames(picked);
}

/** The environment every pane, the private tmux server, both start-up probes, and every daemon child
 * run through `createDaemonRunner` inherit: the allow-listed daemon variables under the complete
 * `mise env --json` output (PATH and toolchain variables), minus any credential-shaped name. Per-pane
 * values (`LEGION_*`, `DISPATCH_URL`, `DISPATCH_TOKEN_FILE`, the `*_FILE` secret pointers, GH_*) are
 * NOT part of it — `processes.ts` and `runtime-tmux.ts` add them as explicit tmux `-e` pairs. */
export function paneEnvironment(
  daemonEnv: NodeJS.ProcessEnv,
  miseEnv: FullMiseEnvironment
): FullMiseEnvironment {
  return withoutSecretLikeNames({
    ...allowedDaemonEnvironment(daemonEnv),
    ...miseEnv,
  }) as FullMiseEnvironment;
}

async function fullMiseEnvironment(
  mise: string,
  env: NodeJS.ProcessEnv,
  run: CommandRunner
): Promise<FullMiseEnvironment> {
  const result = await run([mise, "env", "--json"], { env: allowedDaemonEnvironment(env) });
  if (result.exitCode !== 0) {
    // stdout is `mise`'s env dump on partial success — never interpolated here, only stderr.
    const detail = result.stderr.trim();
    throw new Error(
      `[legion] Could not load the full mise environment (exit ${result.exitCode})${detail ? `: ${detail}` : ""}`
    );
  }
  return paneEnvironment(env, parseMiseEnvironment(result.stdout));
}

function miseToolFromInvocation(invocation: string): string | undefined {
  return /^mise x (\S+) -- omp$/.exec(invocation)?.[1];
}

/** `LEGION_OMP_PATH` is daemon configuration, read from the daemon's own `env`; the `mise where`
 * lookup runs under `paneEnv` (the finished pane environment) like every other daemon child. */
async function resolveOmpInvocation(
  invocation: string,
  mise: string,
  env: NodeJS.ProcessEnv,
  paneEnv: FullMiseEnvironment,
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

  const result = await run([mise, "where", tool], { env: paneEnv });
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
 * PATH every root, worker, and controller pane inherits. That PATH carries no `worker-bin` entry:
 * `mise env` keeps the inherited PATH head, and a daemon started from inside a Legion pane inherits
 * that pane's `<state_dir>/worker-bin`-first PATH — left in place, the daemon's own `gh` would
 * resolve to the shim (every GitHub read failing `LEGION_GRANT_FILE is missing`) and every pane
 * would carry worker-bin twice once `ProcessManager.credentialProcessEnvironment` prepends its own.
 * Stripped here, at the daemon boundary, where the rest of the daemon's own environment is
 * reduced to `PANE_ENV_ALLOW_LIST` (`paneEnvironment`).
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
    PATH: `${legionBinDir}${path.delimiter}${pathWithoutWorkerBin(miseEnv.PATH)}`,
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
      env,
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
