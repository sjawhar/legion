import { accessSync, constants, realpathSync, statSync } from "node:fs";
import { chmod, mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { LEGION_ROLES } from "@legion/contracts";
import type { CommandRunner, CommandRunnerOptions } from "../state/fetch";
import type { RuntimeName } from "./config";
import { shellPath } from "./runtime";
import { pathWithoutWorkerBin } from "./worker-bin";

/** The tools every daemon runs by absolute path. `tmux` exists only under the tmux runtime: a
 * daemon in a pod (`runtime: kubernetes`) has no terminal multiplexer and no local OMP to probe. */
const KUBERNETES_DAEMON_TOOLS = ["jj", "git", "gh"] as const;
const TMUX_DAEMON_TOOLS = [...KUBERNETES_DAEMON_TOOLS, "tmux"] as const;
type KubernetesDaemonTool = (typeof KUBERNETES_DAEMON_TOOLS)[number];
type DaemonTool = (typeof TMUX_DAEMON_TOOLS)[number];

type ResolveExecutable = (command: string, searchPath?: string) => string | undefined;

export interface ResolveDaemonEnvironmentDeps<R extends RuntimeName = RuntimeName> {
  readonly env?: NodeJS.ProcessEnv;
  readonly resolveExecutable?: ResolveExecutable;
  readonly run: CommandRunner;
  /** Legion daemon state directory. The `legion` CLI launcher (see `legionCliLauncherScript`) is
   * written to `<stateDir>/bin/legion` and that directory is prepended to `paneEnv.PATH`, so every
   * spawned pane's ambient `legion` resolves to a CLI that matches this daemon instead of
   * whatever (if anything) happens to be installed on the operator's own PATH. */
  readonly stateDir: string;
  /** `config.runtime.name`: which tools this daemon needs and where its environment comes from
   * (tmux: `mise env`; kubernetes: the pod's own PATH). The literal decides which
   * `DaemonEnvironment` variant `resolveDaemonEnvironment` returns. */
  readonly runtime: R;
}

export interface FullMiseEnvironment extends NodeJS.ProcessEnv {
  readonly PATH: string;
}

/** Every role prompt part the daemon hands a process as the first part of its system prompt
 * (`processes.ts`): the root architect's and the controller's, then one per `LegionRole` — a
 * sub-architect on a child issue runs `architect.md`. `core/oracle.md` and
 * `mechanics/interactive.md` are not daemon-spawned, but are included so one completeness gate
 * validates the entire roles directory the worker image ships. `resolveRolePromptsDir` proves
 * each exists at boot, so a missing prompt is a named startup refusal rather than the first
 * spawn's ENOENT. */
export const ROLE_PROMPT_FILES: readonly string[] = [
  "architect-root.md",
  "controller-root.md",
  ...LEGION_ROLES.map((role) => `${role}.md`),
  ...(["planner", "implementer", "tester", "reviewer"] as const).map((role) => `core/${role}.md`),
  "core/oracle.md",
  "mechanics/headless.md",
  "mechanics/interactive.md",
];

/** The checkout's own copy, `packages/pi-envoy/roles`: what a daemon run from source (every tmux
 * host today) reads. Resolved relative to this module, so it is only meaningful when the daemon
 * runs from its source tree — inside the compiled `legion` binary `import.meta.dir` is Bun's
 * virtual `/$bunfs/root`, so this resolves to `/pi-envoy/roles`, a path that exists nowhere (the
 * first spawn's `ENOENT` on round 1), which is why the worker image sets `LEGION_ROLE_PROMPTS_DIR`
 * (`/opt/legion/roles`, `worker.Dockerfile`) instead. */
export const SOURCE_ROLE_PROMPTS_DIR = path.resolve(import.meta.dir, "../../../pi-envoy/roles");

/** The tmux daemon's resolved tools, the pinned OMP launch fragment it probes and every pane runs,
 * and the complete `mise env` environment (allow-listed, launcher-prepended) it hands to panes. */
export interface TmuxDaemonEnvironment {
  readonly runtime: "tmux";
  readonly commands: Record<DaemonTool, string>;
  readonly ompInvocation: string;
  readonly paneEnv: FullMiseEnvironment;
  /** The directory holding every `ROLE_PROMPT_FILES` entry — see `resolveRolePromptsDir`. */
  readonly rolePromptsDir: string;
}

/** The in-cluster daemon's: jj, git, and gh from the image's own PATH — no mise, no tmux, and no
 * OMP invocation, since the pod probes the worker image in a probe pod (`worker-image-probe.ts`)
 * rather than a local OMP, and its child processes are only the runner's own tool calls. */
export interface KubernetesDaemonEnvironment {
  readonly runtime: "kubernetes";
  readonly commands: Record<KubernetesDaemonTool, string>;
  readonly paneEnv: FullMiseEnvironment;
  readonly rolePromptsDir: string;
}

export type DaemonEnvironment = TmuxDaemonEnvironment | KubernetesDaemonEnvironment;
/** The variant `resolveDaemonEnvironment` returns for a runtime literal. */
export type DaemonEnvironmentFor<R extends RuntimeName> = Extract<
  DaemonEnvironment,
  { runtime: R }
>;

/** Where the role prompts live: `LEGION_ROLE_PROMPTS_DIR` from the daemon's own environment (daemon
 * configuration like `LEGION_OMP_PATH`, never inherited by a pane), else the checkout's
 * `SOURCE_ROLE_PROMPTS_DIR`. Every `ROLE_PROMPT_FILES` entry must be a file there; the refusal
 * names the directory, each missing prompt, and the override. Under kubernetes the runtime reads
 * each prompt from this daemon's own filesystem and inlines it into the pod command
 * (`runtime-kubernetes.ts`); under tmux the pane's shell `$(cat)`s the path, so the directory must
 * be one the panes share with the daemon — on a tmux host it always is. */
export function resolveRolePromptsDir(env: NodeJS.ProcessEnv): string {
  const configured = env.LEGION_ROLE_PROMPTS_DIR;
  if (configured !== undefined && !path.isAbsolute(configured)) {
    throw new Error(
      `[legion] LEGION_ROLE_PROMPTS_DIR must be an absolute path (got ${configured})`
    );
  }
  const directory = configured ?? SOURCE_ROLE_PROMPTS_DIR;
  const missing = ROLE_PROMPT_FILES.filter((file) => {
    try {
      return !statSync(path.join(directory, file)).isFile();
    } catch {
      return true;
    }
  });
  if (missing.length > 0) {
    throw new Error(
      `[legion] Role prompts directory ${directory} is missing ${missing.join(", ")} (set LEGION_ROLE_PROMPTS_DIR to the directory holding pi-envoy's roles/*.md)`
    );
  }
  return directory;
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
  // mise: where the tool store lives, for `mise env` here and the `mise x` every pane runs
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
 * the scrub `github-app-env.ts` applies to a `gh` child's base environment — in a pane, where no
 * allow-list precedes it, this predicate is the only line. A trailing segment `_SECRET`, `_TOKEN`,
 * `_GRANT`, `_KEY` (so `_API_KEY`, `_CLIENT_KEY`, `_ACCESS_KEY`, `_SESSION_KEY`), `_PASSWORD`,
 * `_PASSWD`, `_PAT`, or `_CREDENTIALS`, optionally followed by `_FILE` (the pointer twin), or
 * `PRIVATE_KEY` anywhere; case-insensitive, so a lowercase spelling is caught too. The segment must
 * end the name: `TOKENIZER`, `X_PATH`, `X_KEYBOARD` are not credentials. */
const SECRET_LIKE_NAME =
  /(?:_SECRET|_TOKEN|_GRANT|_KEY|_PASSWORD|_PASSWD|_PAT|_CREDENTIALS)(?:_FILE)?$|PRIVATE_KEY/i;

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

/** The OMP launch fragment every pane and boot probe runs. `LEGION_OMP_PATH` is daemon
 * configuration, read from the daemon's own `env`: it names an explicit direct binary (the
 * Kubernetes worker image, a non-release build under test) and is used as given. Otherwise the
 * configured `mise x <tool> -- omp` invocation is kept verbatim, with `mise` pinned to the absolute
 * path resolved above: mise activates the pinned tool inside the pane — its `bin` first on PATH,
 * its declared environment, an install if the pin is missing — exactly as an operator's shell
 * does. The daemon never turns the invocation into the tool's install path and execs that binary
 * itself: that skips mise's activation and quietly runs whatever happens to sit in the install
 * directory. Whether the pin actually runs is proven by the boot probes, not here. */
function resolveOmpInvocation(
  invocation: string,
  mise: string,
  env: NodeJS.ProcessEnv,
  resolveExecutable: ResolveExecutable
): string {
  const configured = configuredPath(env, "omp");
  if (configured) {
    const resolved = resolveExecutable(configured);
    if (resolved) return resolved;
    throw new Error(`[legion] LEGION_OMP_PATH is not an executable: ${configured}`);
  }

  const tool = /^mise x (\S+) -- omp$/.exec(invocation)?.[1];
  if (!tool) {
    throw new Error(
      "[legion] OMP invocation must be 'mise x <tool> -- omp'. Set LEGION_OMP_PATH to an absolute executable path."
    );
  }
  return `${shellPath(mise)} x ${tool} -- omp`;
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
export async function installLegionCliLauncher(stateDir: string): Promise<string> {
  const binDir = path.join(stateDir, "bin");
  await mkdir(binDir, { recursive: true });
  const launcherPath = path.join(binDir, "legion");
  const script = legionCliLauncherScript(process.execPath, process.argv[1], Bun.main);
  await writeFile(launcherPath, script, "utf8");
  await chmod(launcherPath, 0o755);
  return binDir;
}

/** Resolves each of `tools` by its `LEGION_<TOOL>_PATH` override or on `searchPath`; one error
 * names every missing tool and its override. */
function resolveRequiredTools<T extends string>(
  tools: readonly T[],
  env: NodeJS.ProcessEnv,
  searchPath: string,
  resolveExecutable: ResolveExecutable
): Record<T, string> {
  const missing: string[] = [];
  const commands = {} as Record<T, string>;
  for (const tool of tools) {
    const resolved = resolveConfiguredOrFound(tool, env, searchPath, resolveExecutable);
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
  return commands;
}

/**
 * Resolves all commands before the daemon owns state or accepts work. Under tmux, mise env
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
 *
 * Under kubernetes the daemon is a pod of the worker image: no mise (`mise env` is never run), no
 * tmux, and no local OMP to resolve — `ompInvocation` is ignored. The same launcher is installed,
 * the same allow-list reduces the pod's own environment, and the same strip applies to its PATH;
 * jj, git, and gh must be on it (the image's `/usr/local/bin`), each overridable by
 * `LEGION_<TOOL>_PATH` exactly as under tmux.
 */
export async function resolveDaemonEnvironment<R extends RuntimeName>(
  ompInvocation: string,
  deps: ResolveDaemonEnvironmentDeps<R>
): Promise<DaemonEnvironmentFor<R>> {
  const env = deps.env ?? process.env;
  const resolveExecutable = deps.resolveExecutable ?? defaultResolveExecutable;
  const rolePromptsDir = resolveRolePromptsDir(env);
  // `deps.runtime` is the literal that picks the variant; the cast states what the branch below
  // guarantees and what the caller's `R` already named.
  const environment: DaemonEnvironment =
    deps.runtime === "kubernetes"
      ? await kubernetesDaemonEnvironment(deps.stateDir, env, resolveExecutable, rolePromptsDir)
      : await tmuxDaemonEnvironment(ompInvocation, deps, env, resolveExecutable, rolePromptsDir);
  return environment as DaemonEnvironmentFor<R>;
}

async function kubernetesDaemonEnvironment(
  stateDir: string,
  env: NodeJS.ProcessEnv,
  resolveExecutable: ResolveExecutable,
  rolePromptsDir: string
): Promise<KubernetesDaemonEnvironment> {
  const legionBinDir = await installLegionCliLauncher(stateDir);
  const paneEnv = paneEnvironment(env, {
    PATH: `${legionBinDir}${path.delimiter}${pathWithoutWorkerBin(env.PATH ?? "")}`,
  });
  return {
    runtime: "kubernetes",
    commands: resolveRequiredTools(KUBERNETES_DAEMON_TOOLS, env, paneEnv.PATH, resolveExecutable),
    paneEnv,
    rolePromptsDir,
  };
}

async function tmuxDaemonEnvironment(
  ompInvocation: string,
  deps: ResolveDaemonEnvironmentDeps,
  env: NodeJS.ProcessEnv,
  resolveExecutable: ResolveExecutable,
  rolePromptsDir: string
): Promise<TmuxDaemonEnvironment> {
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
  const commands = resolveRequiredTools(TMUX_DAEMON_TOOLS, env, paneEnv.PATH, resolveExecutable);

  return {
    runtime: "tmux",
    commands,
    ompInvocation: resolveOmpInvocation(ompInvocation, mise, env, resolveExecutable),
    paneEnv,
    rolePromptsDir,
  };
}

/** Runs known daemon tools by their startup-resolved path and with the full mise environment. */
export function createDaemonRunner(
  environment: DaemonEnvironment,
  runner: CommandRunner
): CommandRunner {
  const commands: Partial<Record<DaemonTool, string>> = environment.commands;
  return (command, options) => {
    const executable = commands[command[0] as DaemonTool];
    const resolvedCommand = executable ? [executable, ...command.slice(1)] : command;
    const resolvedOptions: CommandRunnerOptions = {
      ...options,
      env: { ...environment.paneEnv, ...options?.env },
    };
    return runner(resolvedCommand, resolvedOptions);
  };
}
