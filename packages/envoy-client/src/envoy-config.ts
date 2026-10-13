import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import * as path from "node:path";
import type { SyncSubprocess } from "bun";
import { z } from "zod";
import { hasErrnoCode, messageFor } from "./errors";

/** Where a caller runs: one of the three plugins (and the `dispatch` command, which takes its host
 * plugin's rule), or a script outside any of them (`command`). */
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
  readonly DISPATCH_URL?: string | undefined;
} & Readonly<Record<string, string | undefined>>;

/** How long a `!` command may run before its field is an error. */
const COMMAND_TIMEOUT_MS = 10_000;

/** A variable's value, with an empty one read as unset. */
function setValue(value: string | undefined): string | undefined {
  return value === undefined || value === "" ? undefined : value;
}

/** `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`, the file that belongs to no harness, or
 * the refusal of a relative `XDG_CONFIG_HOME`, which the XDG Base Directory spec makes invalid. */
function neutralConfig(env: EnvoyConfigEnvironment): { path: string } | { error: string } {
  const xdgConfigHome = setValue(env.XDG_CONFIG_HOME);
  if (xdgConfigHome !== undefined && !path.isAbsolute(xdgConfigHome)) {
    return { error: `XDG_CONFIG_HOME is ${xdgConfigHome}, which is not an absolute path` };
  }
  // env is the call's one source of truth: os.homedir() alone is not a seam, since Bun resolves it
  // once at startup and ignores later HOME changes.
  const configHome = xdgConfigHome ?? path.join(setValue(env.HOME) ?? homedir(), ".config");
  return { path: path.resolve(configHome, "legion", "envoy.json") };
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
 * The envoy.json a caller reads. A plugin, and the `dispatch` command its session runs, reads its
 * own harness's directory (omp: a named profile's `PI_CODING_AGENT_DIR`; Claude Code:
 * `CLAUDE_CONFIG_DIR`; OpenCode: `OPENCODE_CONFIG_DIR`) and no other harness's, else the file
 * `ENVOY_CONFIG_FILE` names, else `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`. A script
 * (`command`) reads `ENVOY_CONFIG_FILE` first, then the omp, Claude Code and OpenCode directories
 * in that order, then that same neutral file. Throws when the neutral file is the answer and
 * `XDG_CONFIG_HOME` is set to a relative path.
 */
export function envoyConfigPath(harness: EnvoyHarness, env: EnvoyConfigEnvironment): string {
  const handed = setValue(env.ENVOY_CONFIG_FILE);
  if (harness === "command" && handed !== undefined) return path.resolve(handed);
  const directory = harnessDirectory(harness, env);
  if (directory !== undefined) return path.resolve(directory, "envoy.json");
  if (handed !== undefined) return path.resolve(handed);
  const neutral = neutralConfig(env);
  if ("error" in neutral) throw new Error(neutral.error);
  return neutral.path;
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

// A checkout's file is the repository's: it may configure Dispatch and no Envoy. Its `envoy` key
// passes this schema unchecked, and `readEnvoyConfig` refuses it for Envoy alone.
const CheckoutFileSchema = z.looseObject({
  $schema: z.string().optional(),
  dispatch: DispatchSectionSchema.optional(),
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

/** A checkout's own `.envoy/envoy.json`, as written. It holds no Envoy settings: an `envoy`
 * section there is `envoyError`, and Envoy is off with it. */
export interface CheckoutEnvoyConfig {
  readonly path: string;
  readonly dispatch: DispatchFileSettings;
  /** Set when the file holds an `envoy` section, which a checkout cannot write. */
  readonly envoyError: string | null;
}

/**
 * One envoy.json and the checkout's file beside it, as written: a `!` value is still its command,
 * which only the section's own resolver runs (`dispatchSettings`, `envoySettings`), and only when
 * that section uses the value.
 */
export interface EnvoyConfig {
  /** The file the harness rule chose (`envoyConfigPath`). */
  readonly path: string;
  /** The chosen file's sections. */
  readonly dispatch: DispatchFileSettings;
  readonly envoy: EnvoyFileSettings;
  /** The checkout's own file, when the working directory has one. */
  readonly checkout: CheckoutEnvoyConfig | null;
  /** Set when the chosen file does not exist: both sections are then off in the file. */
  readonly notice: string | null;
  /** Set when a file cannot be read or is not valid: both sections are then empty. */
  readonly error: string | null;
}

/** Whether a section's file settings count: a file that turns it off, else whether
 * `enabled: true` stands once the checkout's `dispatch` merges over the chosen file's. */
export type SectionSwitch =
  | { readonly offBy: string }
  | { readonly offBy: null; readonly enabled: boolean };

/**
 * A section's switch. The chosen file's `enabled: false` turns the section off whatever the
 * variables say. A checkout's `dispatch.enabled: false` does only while `DISPATCH_URL` is unset,
 * so a repository cannot turn off the Dispatch the session's environment configured. A checkout
 * has no say in Envoy's switch.
 */
export function sectionSwitch(
  config: EnvoyConfig,
  section: "dispatch" | "envoy",
  env: EnvoyConfigEnvironment
): SectionSwitch {
  const own = config[section].enabled;
  if (own === false) return { offBy: config.path };
  const checkout = section === "dispatch" ? config.checkout : null;
  const theirs = checkout?.dispatch.enabled;
  if (checkout === null || theirs === undefined) return { offBy: null, enabled: own === true };
  if (theirs) return { offBy: null, enabled: true };
  return env.DISPATCH_URL !== undefined
    ? { offBy: null, enabled: own === true }
    : { offBy: checkout.path };
}

type ParsedFile<Settings> =
  | { readonly kind: "absent" }
  | { readonly kind: "invalid"; readonly reason: string }
  | { readonly kind: "valid"; readonly settings: Settings };

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

function parseFile<Settings>(filePath: string, schema: z.ZodType<Settings>): ParsedFile<Settings> {
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
  const parsed = schema.safeParse(parsedJson);
  if (!parsed.success) {
    return { kind: "invalid", reason: describeSchemaIssue(filePath, parsed.error) };
  }
  return { kind: "valid", settings: parsed.data };
}

/** What a `!` command came to: its value, or why it gave none. */
type CommandOutcome = { readonly value: string } | { readonly failure: string };

/** The outcome of every `!` command already run in this process, by command. */
const commandOutcomes = new Map<string, CommandOutcome>();

function runCommand(command: string, env: EnvoyConfigEnvironment): CommandOutcome {
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
    return { failure: `could not start: ${messageFor(error)}` };
  }
  if (result.exitedDueToTimeout) {
    return { failure: `did not finish within ${COMMAND_TIMEOUT_MS / 1000} s` };
  }
  if (!result.success) {
    return { failure: `exited with ${result.signalCode ?? `status ${result.exitCode}`}` };
  }
  const value = result.stdout.toString().trim();
  return value.length === 0 ? { failure: "printed nothing" } : { value };
}

/**
 * A value from the chosen file: as written, or, for one starting with `!`, its command's trimmed
 * stdout. The command runs under `/bin/sh -c` with `env`, for at most 10 s, once per process: its
 * outcome, value or failure, is kept by command. A non-zero exit, a timeout or empty output throws
 * naming the field and the file; neither stream ever reaches an error or a log.
 */
export function fileValue(
  value: string,
  field: string,
  filePath: string,
  env: EnvoyConfigEnvironment
): string {
  if (!value.startsWith("!")) return value;
  const command = value.slice(1);
  let outcome = commandOutcomes.get(command);
  if (outcome === undefined) {
    outcome = runCommand(command, env);
    commandOutcomes.set(command, outcome);
  }
  if ("failure" in outcome) throw new Error(`${filePath}: ${field}'s command ${outcome.failure}`);
  return outcome.value;
}

/** The refusal of a `!` value in a checkout's file, which the repository controls. */
export function checkoutCommandRefusal(checkoutPath: string, field: string): string {
  return `${checkoutPath}: ${field} is a command, which a checkout's envoy.json cannot run`;
}

function failed(filePath: string, error: string): EnvoyConfig {
  return { path: filePath, dispatch: {}, envoy: {}, checkout: null, notice: null, error };
}

/** The notice for a chosen file that does not exist, saying what is `off` for want of it. When that
 * file is the neutral one at `$HOME/.config/legion/envoy.json` (`XDG_CONFIG_HOME` unset, empty or
 * `$HOME/.config`) and the old OpenCode file is still there, it names both and says to move it. An
 * `XDG_CONFIG_HOME` that names another directory, such as the one a Legion pane is given under the
 * daemon's state directory, gets no such hint: the user's file does not belong there. */
export function missingConfigNotice(
  filePath: string,
  env: EnvoyConfigEnvironment,
  off: string
): string {
  const notice = `no envoy.json at ${filePath}: ${off}`;
  const neutral = neutralConfig(env);
  if ("error" in neutral || neutral.path !== path.resolve(filePath)) return notice;
  const home = setValue(env.HOME) ?? homedir();
  if (neutral.path !== path.resolve(home, ".config", "legion", "envoy.json")) return notice;
  const old = path.join(home, ".config", "opencode", "envoy.json");
  if (!existsSync(old)) return notice;
  return `${notice}; ${old} is no longer read: move it to ${neutral.path}`;
}

/**
 * Read the envoy.json at `filePath` (`envoyConfigPath`) and the checkout's `.envoy/envoy.json`
 * under `cwd`, whose `dispatch` section Dispatch's resolver merges over the chosen file's. A
 * checkout's `envoy` section is not read: it sets `checkout.envoyError`, which turns Envoy off and
 * leaves Dispatch as it is. Runs no command. A missing chosen file sets `notice`; a file that
 * cannot be read or is not valid sets `error`, naming the file and the key.
 */
export function readEnvoyConfig(
  filePath: string,
  env: EnvoyConfigEnvironment,
  options: { readonly cwd: string }
): EnvoyConfig {
  const chosen = parseFile(filePath, EnvoyFileSchema);
  if (chosen.kind === "invalid") return failed(filePath, chosen.reason);
  const checkoutPath = path.resolve(options.cwd, ".envoy", "envoy.json");
  const checkoutFile =
    checkoutPath === path.resolve(filePath)
      ? { kind: "absent" as const }
      : parseFile(checkoutPath, CheckoutFileSchema);
  if (checkoutFile.kind === "invalid") return failed(filePath, checkoutFile.reason);
  const own: EnvoyFile = chosen.kind === "valid" ? chosen.settings : {};
  return {
    path: filePath,
    dispatch: own.dispatch ?? {},
    envoy: own.envoy ?? {},
    checkout:
      checkoutFile.kind === "valid"
        ? {
            path: checkoutPath,
            dispatch: checkoutFile.settings.dispatch ?? {},
            envoyError: Object.hasOwn(checkoutFile.settings, "envoy")
              ? `${checkoutPath}: envoy is not read from a checkout's envoy.json; put it in ${filePath}`
              : null,
          }
        : null,
    notice:
      chosen.kind === "absent"
        ? missingConfigNotice(filePath, env, "Dispatch and Envoy are off")
        : null,
    error: null,
  };
}
