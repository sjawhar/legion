import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import * as path from "node:path";
import type { SyncSubprocess } from "bun";
import { z } from "zod";
import { hasErrnoCode, messageFor } from "./errors";

/** Where a caller runs: one of the three plugins, or a command outside any of them. */
export type EnvoyHarness = "omp" | "claude" | "opencode" | "command";

/** The variables a read consults, by name, over whatever else the environment holds. */
export type EnvoyConfigEnvironment = {
  readonly HOME?: string | undefined;
  readonly XDG_CONFIG_HOME?: string | undefined;
  readonly ENVOY_CONFIG_FILE?: string | undefined;
  readonly OMP_PROFILE?: string | undefined;
  readonly PI_CODING_AGENT_DIR?: string | undefined;
  readonly CLAUDE_CONFIG_DIR?: string | undefined;
  readonly OPENCODE_CONFIG_DIR?: string | undefined;
  readonly DISPATCH_TOKEN?: string | undefined;
  readonly DISPATCH_TOKEN_FILE?: string | undefined;
  readonly ENVOY_TOKEN?: string | undefined;
  readonly ENVOY_TOKEN_FILE?: string | undefined;
  readonly NATS_NKEY_SEED?: string | undefined;
  readonly NATS_NKEY_SEED_FILE?: string | undefined;
} & Readonly<Record<string, string | undefined>>;

/** How long a `!` command may run before its field is an error. */
const COMMAND_TIMEOUT_MS = 10_000;

/** A variable's value, with an empty one read as unset. */
function setValue(value: string | undefined): string | undefined {
  return value === undefined || value === "" ? undefined : value;
}

/** `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`, the file that belongs to no harness. A
 * relative `XDG_CONFIG_HOME` is invalid under the XDG Base Directory spec and is refused. */
function neutralConfigPath(env: EnvoyConfigEnvironment): string {
  const xdgConfigHome = setValue(env.XDG_CONFIG_HOME);
  if (xdgConfigHome !== undefined && !path.isAbsolute(xdgConfigHome)) {
    throw new Error(`XDG_CONFIG_HOME is ${xdgConfigHome}, which is not an absolute path`);
  }
  // env is the call's one source of truth: os.homedir() alone is not a seam, since Bun resolves it
  // once at startup and ignores later HOME changes.
  const configHome = xdgConfigHome ?? path.join(setValue(env.HOME) ?? homedir(), ".config");
  return path.resolve(configHome, "legion", "envoy.json");
}

/** The omp profile's directory: `PI_CODING_AGENT_DIR` when `OMP_PROFILE` names a profile other
 * than `default`. */
function ompProfileDirectory(env: EnvoyConfigEnvironment): string | undefined {
  const profile = setValue(env.OMP_PROFILE);
  return profile === undefined || profile === "default"
    ? undefined
    : setValue(env.PI_CODING_AGENT_DIR);
}

function harnessDirectory(harness: EnvoyHarness, env: EnvoyConfigEnvironment): string | undefined {
  switch (harness) {
    case "omp":
      return ompProfileDirectory(env);
    case "claude":
      return setValue(env.CLAUDE_CONFIG_DIR);
    case "opencode":
      return setValue(env.OPENCODE_CONFIG_DIR);
    case "command":
      return (
        ompProfileDirectory(env) ??
        setValue(env.CLAUDE_CONFIG_DIR) ??
        setValue(env.OPENCODE_CONFIG_DIR)
      );
  }
}

/**
 * The envoy.json a caller reads. A plugin reads its own harness's directory (omp: a named
 * profile's `PI_CODING_AGENT_DIR`; Claude Code: `CLAUDE_CONFIG_DIR`; OpenCode:
 * `OPENCODE_CONFIG_DIR`) and no other harness's, else the file `ENVOY_CONFIG_FILE` names, else
 * `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`. A command reads `ENVOY_CONFIG_FILE` first,
 * then the omp, Claude Code and OpenCode directories in that order, then that same neutral file.
 * Throws when the neutral file is the answer and `XDG_CONFIG_HOME` is set to a relative path.
 */
export function envoyConfigPath(harness: EnvoyHarness, env: EnvoyConfigEnvironment): string {
  const handed = setValue(env.ENVOY_CONFIG_FILE);
  if (harness === "command" && handed !== undefined) return path.resolve(handed);
  const directory = harnessDirectory(harness, env);
  if (directory !== undefined) return path.resolve(directory, "envoy.json");
  if (handed !== undefined) return path.resolve(handed);
  return neutralConfigPath(env);
}

// Strict per section, URL-shaped addresses, tolerant top level. The Dispatch server reads the
// same file (the Go loader in packages/envoy/internal/dispatch/config).
const DispatchSectionSchema = z.strictObject({
  enabled: z.boolean().optional(),
  serverUrl: z.url().optional(),
  token: z.string().optional(),
});

const EnvoySectionSchema = z.strictObject({
  enabled: z.boolean().optional(),
  url: z.url().optional(),
  natsUrls: z.array(z.string()).optional(),
  token: z.string().optional(),
  natsSeed: z.string().optional(),
});

const EnvoyFileSchema = z.looseObject({
  $schema: z.string().optional(),
  dispatch: DispatchSectionSchema.optional(),
  envoy: EnvoySectionSchema.optional(),
});

type EnvoyFile = z.infer<typeof EnvoyFileSchema>;

export interface DispatchFileSettings {
  readonly enabled?: boolean | undefined;
  readonly serverUrl?: string | undefined;
  readonly token?: string | undefined;
}

export interface EnvoyFileSettings {
  readonly enabled?: boolean | undefined;
  readonly url?: string | undefined;
  readonly natsUrls?: readonly string[] | undefined;
  readonly token?: string | undefined;
  readonly natsSeed?: string | undefined;
}

/** A checkout's own `.envoy/envoy.json`, as written. */
export interface CheckoutEnvoyConfig {
  readonly path: string;
  readonly dispatch: DispatchFileSettings;
  readonly envoy: EnvoyFileSettings;
}

/**
 * One envoy.json, read. `dispatch` and `envoy` are the checkout's `.envoy/envoy.json` merged over
 * the chosen file, key by key; a section off (`enabled: false`) reads as `{ enabled: false }`. A
 * `!` value has been replaced by its command's output, except where a variable overrides that
 * field or its section is off: such a command is not run, and the field reads as unset.
 */
export interface EnvoyConfig {
  /** The file the harness rule chose (`envoyConfigPath`). */
  readonly path: string;
  readonly dispatch: DispatchFileSettings;
  readonly envoy: EnvoyFileSettings;
  /** The checkout's own file, when the working directory has one. */
  readonly checkout: CheckoutEnvoyConfig | null;
  /** Set when the chosen file does not exist: both sections are then off in the file. */
  readonly notice: string | null;
  /** Set when a file cannot be read or used: both sections are then empty. */
  readonly error: string | null;
}

/** Whether a variable overriding `dispatch.token` is set, as `resolveDispatchConfig` reads it. */
export function dispatchTokenVariableSet(env: EnvoyConfigEnvironment): boolean {
  return env.DISPATCH_TOKEN_FILE !== undefined || env.DISPATCH_TOKEN !== undefined;
}

/** Whether a variable overriding `envoy.token` is set, as `envoySettings` reads it. */
export function envoyTokenVariableSet(env: EnvoyConfigEnvironment): boolean {
  return setValue(env.ENVOY_TOKEN_FILE) !== undefined || setValue(env.ENVOY_TOKEN) !== undefined;
}

type ParsedFile =
  | { readonly kind: "absent" }
  | { readonly kind: "invalid"; readonly reason: string }
  | { readonly kind: "valid"; readonly settings: EnvoyFile };

/** Describe the first schema-validation issue, naming the file and offending key. */
function describeSchemaIssue(filePath: string, error: z.ZodError): string {
  const issue = error.issues[0];
  if (!issue) return `${filePath}: invalid envoy.json`;
  if (issue.code === "unrecognized_keys") {
    const section = issue.path.join(".");
    const keys = issue.keys.map((key) => `${section}.${key}`).join(", ");
    return `${filePath}: unrecognized ${section} key(s): ${keys}`;
  }
  return `${filePath}: ${issue.path.join(".")}: ${issue.message}`;
}

function parseFile(filePath: string): ParsedFile {
  let raw: string;
  try {
    raw = readFileSync(filePath, "utf-8");
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT") || hasErrnoCode(error, "ENOTDIR")) return { kind: "absent" };
    return { kind: "invalid", reason: `${filePath}: could not be read (${messageFor(error)})` };
  }
  let parsedJson: unknown;
  try {
    parsedJson = JSON.parse(raw);
  } catch (error) {
    return { kind: "invalid", reason: `${filePath}: invalid JSON (${messageFor(error)})` };
  }
  if (typeof parsedJson === "object" && parsedJson !== null && "natsUrls" in parsedJson) {
    return {
      kind: "invalid",
      reason: `${filePath}: the top-level natsUrls is not read; move it to envoy.natsUrls`,
    };
  }
  const parsed = EnvoyFileSchema.safeParse(parsedJson);
  if (!parsed.success) {
    return { kind: "invalid", reason: describeSchemaIssue(filePath, parsed.error) };
  }
  return { kind: "valid", settings: parsed.data };
}

/** Values of `!` commands already run in this process, by command. */
const commandValues = new Map<string, string>();

/** A `!` value's command output: run under `/bin/sh -c` with `env`, at most 10 s, its trimmed
 * stdout the value. Neither stream ever reaches an error or a log. */
function commandValue(
  value: string,
  field: string,
  filePath: string,
  env: EnvoyConfigEnvironment
): string {
  const command = value.slice(1);
  const cached = commandValues.get(command);
  if (cached !== undefined) return cached;
  const childEnv: Record<string, string> = {};
  for (const [name, variable] of Object.entries(env)) {
    if (variable !== undefined) childEnv[name] = variable;
  }
  let result: SyncSubprocess<"pipe", "pipe">;
  try {
    result = Bun.spawnSync(["/bin/sh", "-c", command], {
      env: childEnv,
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
      timeout: COMMAND_TIMEOUT_MS,
    });
  } catch (error) {
    throw new Error(`${filePath}: ${field}'s command could not start: ${messageFor(error)}`);
  }
  if (result.exitedDueToTimeout) {
    throw new Error(
      `${filePath}: ${field}'s command did not finish within ${COMMAND_TIMEOUT_MS / 1000} s`
    );
  }
  if (!result.success) {
    const status = result.signalCode ?? `status ${result.exitCode}`;
    throw new Error(`${filePath}: ${field}'s command exited with ${status}`);
  }
  const output = result.stdout.toString().trim();
  if (output.length === 0) throw new Error(`${filePath}: ${field}'s command printed nothing`);
  commandValues.set(command, output);
  return output;
}

/** A field's value: as written, or its command's output when it starts with `!` and `runs`;
 * unset for a command that does not run. */
function fieldValue(
  value: string | undefined,
  runs: boolean,
  field: string,
  filePath: string,
  env: EnvoyConfigEnvironment
): string | undefined {
  if (value === undefined || !value.startsWith("!")) return value;
  return runs ? commandValue(value, field, filePath, env) : undefined;
}

/** A checkout's file: commands refused, and an address only with the token written beside it. */
function checkoutProblem(
  checkout: CheckoutEnvoyConfig,
  dispatch: DispatchFileSettings,
  envoy: EnvoyFileSettings
): string | null {
  const commands: [string, string | undefined][] = [
    ["dispatch.token", checkout.dispatch.token],
    ["envoy.token", checkout.envoy.token],
    ["envoy.natsSeed", checkout.envoy.natsSeed],
  ];
  for (const [field, value] of commands) {
    if (value?.startsWith("!")) {
      return `${checkout.path}: ${field} is a command, which a checkout's envoy.json cannot run`;
    }
  }
  if (
    dispatch.enabled === true &&
    checkout.dispatch.serverUrl !== undefined &&
    !checkout.dispatch.token
  ) {
    return `${checkout.path}: dispatch.serverUrl requires dispatch.token in the same file`;
  }
  if (envoy.enabled === true && checkout.envoy.url !== undefined && !checkout.envoy.token) {
    return `${checkout.path}: envoy.url requires envoy.token in the same file`;
  }
  return null;
}

function failed(filePath: string, error: string): EnvoyConfig {
  return { path: filePath, dispatch: {}, envoy: {}, checkout: null, notice: null, error };
}

/** The notice for a chosen file that does not exist, naming the old OpenCode file when it is still
 * there and the neutral file is not. */
function missingNotice(filePath: string, env: EnvoyConfigEnvironment): string {
  const notice = `no envoy.json at ${filePath}: Dispatch and Envoy are off`;
  const old = path.join(setValue(env.HOME) ?? homedir(), ".config", "opencode", "envoy.json");
  const neutral = neutralConfigPath(env);
  if (!existsSync(old) || existsSync(neutral)) return notice;
  return `${notice}; ${old} is no longer read: move it to ${neutral}`;
}

/**
 * Read the envoy.json at `filePath` (`envoyConfigPath`) and the checkout's `.envoy/envoy.json`
 * under `cwd`, which merges over it. A missing chosen file sets `notice`; a file that cannot be
 * read, is not valid, or whose `!` command fails sets `error`, naming the file and the key.
 *
 * A `token` or `natsSeed` value starting with `!` is a command, run once per process (see
 * `EnvoyConfig` for when it is not run at all). Only the chosen file may hold one: a checkout's
 * file is the repository's, so its commands are refused, and its `dispatch.serverUrl` or
 * `envoy.url` is refused without the token written beside it.
 */
export function readEnvoyConfig(
  filePath: string,
  env: EnvoyConfigEnvironment,
  options: { readonly cwd: string }
): EnvoyConfig {
  const chosen = parseFile(filePath);
  if (chosen.kind === "invalid") return failed(filePath, chosen.reason);
  const checkoutPath = path.resolve(options.cwd, ".envoy", "envoy.json");
  const checkoutFile =
    checkoutPath === path.resolve(filePath) ? { kind: "absent" as const } : parseFile(checkoutPath);
  if (checkoutFile.kind === "invalid") return failed(filePath, checkoutFile.reason);

  const own: EnvoyFile = chosen.kind === "valid" ? chosen.settings : {};
  const checkout: CheckoutEnvoyConfig | null =
    checkoutFile.kind === "valid"
      ? {
          path: checkoutPath,
          dispatch: checkoutFile.settings.dispatch ?? {},
          envoy: checkoutFile.settings.envoy ?? {},
        }
      : null;
  const mergedDispatch: DispatchFileSettings = { ...own.dispatch, ...checkout?.dispatch };
  const mergedEnvoy: EnvoyFileSettings = { ...own.envoy, ...checkout?.envoy };
  if (checkout !== null) {
    const problem = checkoutProblem(checkout, mergedDispatch, mergedEnvoy);
    if (problem !== null) return failed(filePath, problem);
  }

  let dispatch: DispatchFileSettings = { enabled: false };
  let envoy: EnvoyFileSettings = { enabled: false };
  let notice: string | null = null;
  try {
    if (mergedDispatch.enabled !== false) {
      const { token, ...rest } = mergedDispatch;
      const runs = !dispatchTokenVariableSet(env);
      const value = fieldValue(token, runs, "dispatch.token", filePath, env);
      dispatch = value === undefined ? rest : { ...rest, token: value };
    }
    if (mergedEnvoy.enabled !== false) {
      const { token, natsSeed, ...rest } = mergedEnvoy;
      const on = mergedEnvoy.enabled === true;
      const tokenRuns = on && !envoyTokenVariableSet(env);
      // As natsSeedFromEnvironment reads them: a set variable, even an empty one, overrides.
      const seedRuns =
        on && env.NATS_NKEY_SEED_FILE === undefined && env.NATS_NKEY_SEED === undefined;
      const tokenValue = fieldValue(token, tokenRuns, "envoy.token", filePath, env);
      const seedValue = fieldValue(natsSeed, seedRuns, "envoy.natsSeed", filePath, env);
      envoy = {
        ...rest,
        ...(tokenValue === undefined ? {} : { token: tokenValue }),
        ...(seedValue === undefined ? {} : { natsSeed: seedValue }),
      };
    }
    if (chosen.kind === "absent") notice = missingNotice(filePath, env);
  } catch (error) {
    return failed(filePath, messageFor(error));
  }

  return { path: filePath, dispatch, envoy, checkout, notice, error: null };
}
