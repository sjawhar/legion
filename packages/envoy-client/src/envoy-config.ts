import {
  chmodSync,
  closeSync,
  existsSync,
  lstatSync,
  mkdtempSync,
  openSync,
  readFileSync,
  readlinkSync,
  rmSync,
  statSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import * as path from "node:path";
import type { SyncSubprocess } from "bun";
import { z } from "zod";
import { hasErrnoCode, messageFor } from "./errors";
import { jsonErrorPosition } from "./json-position";
import { sendable } from "./secret-file";

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
  readonly CLAUDE_CODE_CHILD_SESSION?: string | undefined;
  readonly OPENCODE_CONFIG_DIR?: string | undefined;
  readonly DISPATCH_URL?: string | undefined;
} & Readonly<Record<string, string | undefined>>;

/** How long a `!` command may run before its field is an error. */
const COMMAND_TIMEOUT_MS = 10_000;

/** A variable's value, with an empty one read as unset: the one meaning of `""` for every variable
 * Dispatch and Envoy read. */
export function nonEmpty(value: string | undefined): string | undefined {
  return value === undefined || value === "" ? undefined : value;
}

/** A path variable's value, refused when relative: it would resolve against whatever directory
 * each process happens to start in, a cloned repository's included. */
function absolute(name: string, value: string): string;
function absolute(name: string, value: string | undefined): string | undefined;
function absolute(name: string, value: string | undefined): string | undefined {
  if (value !== undefined && !path.isAbsolute(value)) {
    throw new Error(`${name} is ${value}, which is not an absolute path`);
  }
  return value;
}

/** `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`, the file that belongs to no harness, or
 * the refusal of a relative `XDG_CONFIG_HOME`, which the XDG Base Directory spec makes invalid. */
function neutralConfig(env: EnvoyConfigEnvironment): { path: string } | { error: string } {
  const xdgConfigHome = nonEmpty(env.XDG_CONFIG_HOME);
  if (xdgConfigHome !== undefined && !path.isAbsolute(xdgConfigHome)) {
    return { error: `XDG_CONFIG_HOME is ${xdgConfigHome}, which is not an absolute path` };
  }
  // env is the call's one source of truth: os.homedir() alone is not a seam, since Bun resolves it
  // once at startup and ignores later HOME changes.
  const configHome = xdgConfigHome ?? path.join(nonEmpty(env.HOME) ?? homedir(), ".config");
  return { path: path.resolve(configHome, "legion", "envoy.json") };
}

/** The variable naming each plugin's own directory. */
const HARNESS_DIRECTORY_VARIABLE = {
  omp: "PI_CODING_AGENT_DIR",
  claude: "CLAUDE_CONFIG_DIR",
  opencode: "OPENCODE_CONFIG_DIR",
} as const;

type PluginHarness = keyof typeof HARNESS_DIRECTORY_VARIABLE;

const PLUGIN_HARNESSES = Object.keys(HARNESS_DIRECTORY_VARIABLE) as readonly PluginHarness[];

/** The directory a plugin's harness names, as written: Oh My Pi's `PI_CODING_AGENT_DIR` when
 * `OMP_PROFILE` names a profile other than `default`, Claude Code's `CLAUDE_CONFIG_DIR`,
 * OpenCode's `OPENCODE_CONFIG_DIR`. */
function namedDirectory(harness: PluginHarness, env: EnvoyConfigEnvironment): string | undefined {
  if (harness === "omp") {
    const profile = nonEmpty(env.OMP_PROFILE);
    if (profile === undefined || profile === "default") return undefined;
  }
  return nonEmpty(env[HARNESS_DIRECTORY_VARIABLE[harness]]);
}

/** The directory `harness` reads its envoy.json from, refused when relative: a plugin's own
 * (`namedDirectory`), or for a command the first one named of Oh My Pi's, Claude Code's and
 * OpenCode's, in that order, Claude Code's only under `CLAUDE_CODE_CHILD_SESSION=1`. */
function harnessDirectory(harness: EnvoyHarness, env: EnvoyConfigEnvironment): string | undefined {
  for (const plugin of harness === "command" ? PLUGIN_HARNESSES : [harness]) {
    // A shell outside Claude Code can export CLAUDE_CONFIG_DIR too; only Claude Code sets
    // CLAUDE_CODE_CHILD_SESSION=1, in every Bash, hook and status line command it runs.
    if (harness === "command" && plugin === "claude" && env.CLAUDE_CODE_CHILD_SESSION !== "1") {
      continue;
    }
    const directory = namedDirectory(plugin, env);
    if (directory !== undefined) return absolute(HARNESS_DIRECTORY_VARIABLE[plugin], directory);
  }
  return undefined;
}

/** The file `ENVOY_CONFIG_FILE` names, the one a plugin hands the commands its session runs, or
 * undefined when it is unset or empty. Throws when it is relative. */
export function handedConfigPath(env: EnvoyConfigEnvironment): string | undefined {
  const handed = nonEmpty(env.ENVOY_CONFIG_FILE);
  return handed === undefined ? undefined : path.resolve(absolute("ENVOY_CONFIG_FILE", handed));
}

/**
 * The envoy.json a caller reads. A plugin reads its own harness's directory (omp: a named
 * profile's `PI_CODING_AGENT_DIR`; Claude Code: `CLAUDE_CONFIG_DIR`; OpenCode:
 * `OPENCODE_CONFIG_DIR`) and no other harness's, else the file `ENVOY_CONFIG_FILE` names, else
 * `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`. A script (`command`) reads
 * `ENVOY_CONFIG_FILE` first, then the omp, Claude Code and OpenCode directories in that order
 * (Claude Code's only under `CLAUDE_CODE_CHILD_SESSION=1`), then that same neutral file. Throws
 * when the path it would use is relative: `ENVOY_CONFIG_FILE`, the harness directory, or
 * `XDG_CONFIG_HOME` for the neutral file.
 */
export function envoyConfigPath(harness: EnvoyHarness, env: EnvoyConfigEnvironment): string {
  const handedFirst = harness === "command" ? handedConfigPath(env) : undefined;
  if (handedFirst !== undefined) return handedFirst;
  const directory = harnessDirectory(harness, env);
  if (directory !== undefined) return path.resolve(directory, "envoy.json");
  const handed = handedConfigPath(env);
  if (handed !== undefined) return handed;
  const neutral = neutralConfig(env);
  if ("error" in neutral) throw new Error(neutral.error);
  return neutral.path;
}

const httpUrlSchema = z.url({ protocol: /^https?$/, error: "must be an http or https URL" });

// Strict per section, http(s) addresses, tolerant top level. The Dispatch server reads the same
// file and follows these validation rules.
const DispatchSectionSchema = z.strictObject({
  enabled: z.boolean().optional(),
  serverUrl: httpUrlSchema.optional(),
  token: z.string().optional(),
});

const EnvoySectionSchema = z.strictObject({
  enabled: z.boolean().optional(),
  url: httpUrlSchema.optional(),
  natsUrls: z.array(z.string().trim().min(1, "must not be empty")).optional(),
  token: z.string().optional(),
  natsSeed: z.string().optional(),
});

// A top-level `natsUrls` is refused before this schema runs (`parseFile`): it belongs in
// `envoy.natsUrls`.
const EnvoyFileSchema = z.looseObject({
  $schema: z.string().optional(),
  dispatch: DispatchSectionSchema.optional(),
  envoy: EnvoySectionSchema.optional(),
});

// A checkout's file is the repository's: it may configure Dispatch and no Envoy. Its `envoy` key
// passes this schema unchecked, and `readEnvoyConfig` names it as not read.
const CheckoutFileSchema = z.looseObject({
  $schema: z.string().optional(),
  dispatch: DispatchSectionSchema.optional(),
});

type EnvoyFile = z.infer<typeof EnvoyFileSchema>;

/** A `dispatch` section as written. */
export type DispatchFileSettings = Readonly<z.infer<typeof DispatchSectionSchema>>;

/** An `envoy` section as written, its `natsUrls` trimmed. */
export type EnvoyFileSettings = Readonly<z.infer<typeof EnvoySectionSchema>>;

/** A checkout's own `.envoy/envoy.json`: as written, or why it could not be used. It holds no
 * Envoy settings: an `envoy` section there is `envoyError`. Its failure is its own: Dispatch reads
 * it, Envoy never does. */
export type CheckoutEnvoyConfig =
  | {
      readonly kind: "valid";
      readonly path: string;
      readonly dispatch: DispatchFileSettings;
      /** Set when the file holds an `envoy` section, which a checkout cannot write. */
      readonly envoyError: string | null;
    }
  | { readonly kind: "invalid"; readonly path: string; readonly error: string };

/** A chosen file that does not exist: a dangling link's target and any old-file notice
 * (`missingConfigNotice`). */
export interface MissingConfig {
  readonly link: string | null;
  readonly oldUserFileNotice?: string;
}

/**
 * The chosen envoy.json and the checkout's file beside it, as written: a `!` value is still its
 * command, which only the section's own resolver runs (`dispatchSettings`, `envoySettings`), and
 * only when that section uses the value. A chosen file that cannot be read or is not valid
 * (`failed`) has no sections at all.
 */
export type EnvoyConfig =
  | { readonly kind: "failed"; readonly path: string; readonly error: string }
  | {
      readonly kind: "read";
      /** The file the harness rule chose (`envoyConfigPath`). */
      readonly path: string;
      /** The chosen file's sections; empty when it does not exist. */
      readonly dispatch: DispatchFileSettings;
      readonly envoy: EnvoyFileSettings;
      /** The checkout's own file, when the working directory has one. */
      readonly checkout: CheckoutEnvoyConfig | null;
      /** Set when the chosen file does not exist. */
      readonly missing: MissingConfig | null;
      /** Files found and not read, each naming its move: a checkout's `.opencode/envoy.json` while
       * the checkout has no `.envoy/envoy.json`. */
      readonly warnings: readonly string[];
    };

/** A chosen file that was read. */
export type ReadEnvoyConfig = Extract<EnvoyConfig, { readonly kind: "read" }>;

/** A section that is off: a `notice` says why when nothing is wrong (a file turns it off, or
 * nothing names it), an `error` names what could not be read or used. */
export type SectionOff =
  | { readonly enabled: false; readonly notice: string; readonly error: null }
  | { readonly enabled: false; readonly notice: null; readonly error: string };

export function offWithNotice(notice: string): SectionOff {
  return { enabled: false, notice, error: null };
}

export function offWithError(error: string): SectionOff {
  return { enabled: false, notice: null, error };
}

/** `value` as an http or https URL, or undefined for anything else. */
export function httpUrl(value: string): URL | undefined {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return undefined;
  }
  return url.protocol === "http:" || url.protocol === "https:" ? url : undefined;
}

type ParsedFile<Settings> =
  | { readonly kind: "absent"; readonly link: string | null }
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

/** A dangling link's target, for a chosen file that does not exist. */
function danglingLink(filePath: string): string | null {
  try {
    return lstatSync(filePath).isSymbolicLink() ? readlinkSync(filePath) : null;
  } catch {
    return null;
  }
}

function parseFile<Settings>(
  filePath: string,
  schema: z.ZodType<Settings>,
  checkout: boolean
): ParsedFile<Settings> {
  let raw: string;
  try {
    raw = readFileSync(filePath, "utf-8");
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT") || hasErrnoCode(error, "ENOTDIR")) {
      return { kind: "absent", link: danglingLink(filePath) };
    }
    return { kind: "invalid", reason: `${filePath}: could not be read (${messageFor(error)})` };
  }
  let parsedJson: unknown;
  try {
    parsedJson = JSON.parse(raw);
  } catch {
    const position = jsonErrorPosition(raw);
    return {
      kind: "invalid",
      reason: `${filePath}: invalid JSON${position === undefined ? "" : ` at ${position}`}`,
    };
  }
  if (typeof parsedJson === "object" && parsedJson !== null && "natsUrls" in parsedJson) {
    return {
      kind: "invalid",
      reason: checkout
        ? `${filePath}: the top-level natsUrls is not read, and a checkout's envoy.json holds only a dispatch section`
        : `${filePath}: the top-level natsUrls is not read; move it to envoy.natsUrls`,
    };
  }
  const parsed = schema.safeParse(parsedJson);
  if (!parsed.success) {
    return { kind: "invalid", reason: describeSchemaIssue(filePath, parsed.error) };
  }
  return { kind: "valid", settings: parsed.data };
}

/** What a `!` command came to: its value, or why it gave none, and whether that was its running out
 * of time rather than ending on its own. */
type CommandOutcome =
  | { readonly value: string }
  | { readonly failure: string; readonly timedOut: boolean };

/** A kept outcome, and when it was kept. */
type KeptOutcome = CommandOutcome & { readonly at: number };

/** How long a `!` command's failure that ended on its own is kept before it runs again: a transient
 * cause (a locked keyring, a file not yet written) clears without a restart. A command that ran out
 * of time is kept failed for the life of the process instead: each run blocks the process for the
 * whole timeout, so a hanging command retried once a minute would freeze its session that often. */
const FAILURE_KEPT_MS = 60_000;

/** The process-wide store of kept outcomes, by command, file and working directory. It lives on
 * `globalThis` under a registered symbol because Oh My Pi loads each extension instance as a fresh
 * module, and every bundle of this module in one process (pi-envoy's, pi-legion's, released
 * separately) shares it; an entry's shape (`KeptOutcome`) is its contract across them. A change to
 * that shape takes a new key (`.v2`), so an older bundle in the same process never misreads an
 * entry a newer one wrote. */
const COMMAND_OUTCOMES: unique symbol = Symbol.for("legion.envoy-config.commands.v1");
const processGlobal = globalThis as typeof globalThis & {
  [COMMAND_OUTCOMES]?: Map<string, KeptOutcome>;
};
const commandOutcomes: Map<string, KeptOutcome> = processGlobal[COMMAND_OUTCOMES] ?? new Map();
processGlobal[COMMAND_OUTCOMES] = commandOutcomes;

function runCommand(command: string, env: EnvoyConfigEnvironment, cwd: string): CommandOutcome {
  const childEnv: Record<string, string> = {};
  for (const [name, variable] of Object.entries(env)) {
    if (variable !== undefined) childEnv[name] = variable;
  }
  // stdout goes to a file in a private directory, so the wait is for the shell alone: a pipe would
  // be held open by any process the command left running, until the timeout.
  const directory = mkdtempSync(path.join(tmpdir(), "envoy-command-"));
  try {
    chmodSync(directory, 0o700);
    const output = path.join(directory, "stdout");
    const fd = openSync(output, "wx", 0o600);
    let result: SyncSubprocess<number, "ignore">;
    try {
      // Its own process group, so every process the command started is killed with it.
      result = Bun.spawnSync(["/bin/sh", "-c", command], {
        cwd,
        env: childEnv,
        stdin: "ignore",
        stdout: fd,
        stderr: "ignore",
        timeout: COMMAND_TIMEOUT_MS,
        detached: true,
      });
    } catch (error) {
      return { failure: `could not start: ${messageFor(error)}`, timedOut: false };
    } finally {
      closeSync(fd);
    }
    // Whatever the outcome, nothing the command started outlives it, and so nothing it left
    // running can change the value read below.
    try {
      process.kill(-result.pid, "SIGKILL");
    } catch {
      // The group is already gone.
    }
    if (result.exitedDueToTimeout) {
      return {
        failure: `did not finish within ${COMMAND_TIMEOUT_MS / 1000} s, and does not run again in this process`,
        timedOut: true,
      };
    }
    if (!result.success) {
      return {
        failure: `exited with ${result.signalCode ?? `status ${result.exitCode}`}`,
        timedOut: false,
      };
    }
    const value = readFileSync(output, "utf-8").trim();
    if (value.length === 0) return { failure: "printed nothing", timedOut: false };
    if (!sendable(value)) {
      return {
        failure: "printed more than one line, or whitespace inside its value",
        timedOut: false,
      };
    }
    return { value };
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

/**
 * A value from the chosen file: as written, or, for one starting with `!`, its command's trimmed
 * stdout. The command runs under `/bin/sh -c` with `env`, in the directory of the file that names
 * it (never the session's working directory, so a relative command or a directory-configured shim
 * resolves against that file), in its own process group, for at most 10 s. Its value is kept for
 * the process, shared by every copy of this module loaded in it, keyed by the command, its file and
 * its working directory; so is a run out of time, while a failure that ended on its own (a non-zero
 * exit, empty output, more than one line) is kept for 60 s, then the command runs again. Its exit
 * status decides, and the wait is for the shell alone: once it exits or runs out of time, every
 * process of its group still running is killed, then its output is read. A non-zero exit, a
 * timeout or empty output throws naming the field and the file. A value with whitespace or a
 * control character inside it, as written or as printed, throws too, since it cannot be sent as one
 * bearer. No error carries the value or either stream.
 */
export function fileValue(
  value: string,
  field: string,
  filePath: string,
  env: EnvoyConfigEnvironment
): string {
  if (!value.startsWith("!")) {
    if (!sendable(value)) {
      throw new Error(`${filePath}: ${field} holds whitespace or a control character`);
    }
    return value;
  }
  const command = value.slice(1);
  const cwd = path.dirname(path.resolve(filePath));
  const key = JSON.stringify([command, filePath, cwd]);
  let outcome = commandOutcomes.get(key);
  if (
    outcome === undefined ||
    ("failure" in outcome && !outcome.timedOut && Date.now() - outcome.at >= FAILURE_KEPT_MS)
  ) {
    outcome = { ...runCommand(command, env, cwd), at: Date.now() };
    commandOutcomes.set(key, outcome);
  }
  if ("failure" in outcome) throw new Error(`${filePath}: ${field}'s command ${outcome.failure}`);
  return outcome.value;
}

/** The device a directory is on, as `stat` gives it, or undefined when it cannot be read. */
export type DeviceOf = (directory: string) => number | undefined;

function statDevice(directory: string): number | undefined {
  try {
    return statSync(directory).dev;
  } catch {
    return undefined;
  }
}

/** What to do with `oldFile`, which is not read, so that `newFile` holds it: `move`, the `mv`
 * command, unless the old file's directory and the nearest existing ancestor of the new file's
 * directory are on different filesystems (their devices differ, as for a directory an agent box
 * bind-mounts from its host). There a `mv` would move the host's file into the box's own
 * filesystem, so the notice names the file to create and says to move the old one on the machine
 * that owns it, with no command. */
function moveNotice(oldFile: string, newFile: string, move: string, deviceOf: DeviceOf): string {
  const oldDevice = deviceOf(path.dirname(oldFile));
  let ancestor = path.dirname(newFile);
  let newDevice = deviceOf(ancestor);
  while (newDevice === undefined && path.dirname(ancestor) !== ancestor) {
    ancestor = path.dirname(ancestor);
    newDevice = deviceOf(ancestor);
  }
  if (oldDevice !== undefined && newDevice !== undefined && oldDevice !== newDevice) {
    return `${oldFile} is not read, and is on another filesystem than ${newFile} (a mount): create ${newFile}, and move ${oldFile} on the machine that owns it`;
  }
  return `${oldFile} is not read: ${move}`;
}

/** What a missing chosen file says about the OpenCode-era user file,
 * `$HOME/.config/opencode/envoy.json`, which nothing reads, while that file is still there. The
 * chosen file's path decides, not the rule that chose it: the user's default file at
 * `$HOME/.config/legion/envoy.json` names the move (`moveNotice`); a profile's own file names the
 * file to copy or link it to; the neutral file under a relocated `XDG_CONFIG_HOME` (a Legion
 * pane's, under the daemon's state directory) says it is not read there, without asking anyone to
 * move it. Any other file says nothing of it. */
function oldUserFileNotice(
  filePath: string,
  env: EnvoyConfigEnvironment,
  deviceOf: DeviceOf
): string | undefined {
  const home = nonEmpty(env.HOME) ?? homedir();
  const old = path.join(home, ".config", "opencode", "envoy.json");
  if (!existsSync(old)) return undefined;

  const selected = path.resolve(filePath);
  const userFile = path.resolve(home, ".config", "legion", "envoy.json");
  if (selected === userFile) {
    return moveNotice(
      old,
      userFile,
      "mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/",
      deviceOf
    );
  }

  // A relative directory chose no file here (`harnessDirectory` refuses it), so it is skipped.
  const profileFile = PLUGIN_HARNESSES.map((harness) => namedDirectory(harness, env)).some(
    (directory) =>
      directory !== undefined &&
      path.isAbsolute(directory) &&
      selected === path.resolve(directory, "envoy.json")
  );
  if (profileFile) {
    return `${old} is not read for this profile; copy or link it to ${filePath}, or write that file`;
  }

  const configHome = nonEmpty(env.XDG_CONFIG_HOME);
  const neutral = neutralConfig(env);
  if (
    configHome !== undefined &&
    path.isAbsolute(configHome) &&
    "path" in neutral &&
    selected === neutral.path
  ) {
    return `${old} is not read under XDG_CONFIG_HOME=${configHome}`;
  }
  return undefined;
}

/** The notice for a chosen file that does not exist, saying what is `off` for want of it, and
 * naming the link's target and any unread old user file (`readEnvoyConfig`). */
export function missingConfigNotice(filePath: string, missing: MissingConfig, off: string): string {
  const link = missing.link === null ? "" : ` (a link to ${missing.link}, which does not exist)`;
  const old = missing.oldUserFileNotice === undefined ? "" : `; ${missing.oldUserFileNotice}`;
  return `no envoy.json at ${filePath}${link}: ${off}${old}`;
}

/**
 * Read the envoy.json at `filePath` (`envoyConfigPath`) and the checkout's `.envoy/envoy.json`
 * under `cwd`, whose `dispatch` section Dispatch's resolver reads beside the chosen file's. Runs no
 * command. A missing chosen file sets `missing`; a chosen file that cannot be read or is not valid
 * is `failed`, naming the file and the key. The checkout's file keeps its own outcome: one that
 * cannot be read or is not valid is an `invalid` checkout, which only Dispatch reads, and an
 * `envoy` section in it sets `checkout.envoyError` rather than configuring anything.
 *
 * The OpenCode-era files are never read. While the chosen file is missing, its notice names
 * `$HOME/.config/opencode/envoy.json` when that file is there (`oldUserFileNotice`); while the
 * checkout has no `.envoy/envoy.json`, `warnings` names its `.opencode/envoy.json` and the rename.
 * Neither gives a `mv` across filesystems (`moveNotice`); `deviceOf` says which filesystem a
 * directory is on, `stat`'s device by default.
 */
export function readEnvoyConfig(
  filePath: string,
  env: EnvoyConfigEnvironment,
  options: { readonly cwd: string; readonly deviceOf?: DeviceOf | undefined }
): EnvoyConfig {
  const deviceOf = options.deviceOf ?? statDevice;
  const chosen = parseFile(filePath, EnvoyFileSchema, false);
  if (chosen.kind === "invalid") return { kind: "failed", path: filePath, error: chosen.reason };
  const resolvedChosen = path.resolve(filePath);
  const checkoutPath = path.resolve(options.cwd, ".envoy", "envoy.json");
  const checkoutFile =
    checkoutPath === resolvedChosen
      ? { kind: "absent" as const, link: null }
      : parseFile(checkoutPath, CheckoutFileSchema, true);
  const own: EnvoyFile = chosen.kind === "valid" ? chosen.settings : {};
  let checkout: CheckoutEnvoyConfig | null = null;
  if (checkoutFile.kind === "invalid") {
    checkout = { kind: "invalid", path: checkoutPath, error: checkoutFile.reason };
  } else if (checkoutFile.kind === "valid") {
    checkout = {
      kind: "valid",
      path: checkoutPath,
      dispatch: checkoutFile.settings.dispatch ?? {},
      envoyError: Object.hasOwn(checkoutFile.settings, "envoy")
        ? `${checkoutPath}: envoy is not read from a checkout's envoy.json; put it in ${filePath}`
        : null,
    };
  }
  const oldCheckout = path.resolve(options.cwd, ".opencode", "envoy.json");
  const warnings =
    oldCheckout !== resolvedChosen && !existsSync(checkoutPath) && existsSync(oldCheckout)
      ? [
          moveNotice(
            oldCheckout,
            checkoutPath,
            `mkdir -p ${path.dirname(checkoutPath)} && mv ${oldCheckout} ${checkoutPath}`,
            deviceOf
          ),
        ]
      : [];
  const unreadOldUserFile =
    chosen.kind === "absent" ? oldUserFileNotice(filePath, env, deviceOf) : undefined;
  return {
    kind: "read",
    path: filePath,
    dispatch: own.dispatch ?? {},
    envoy: own.envoy ?? {},
    checkout,
    missing:
      chosen.kind === "absent"
        ? {
            link: chosen.link,
            ...(unreadOldUserFile === undefined ? {} : { oldUserFileNotice: unreadOldUserFile }),
          }
        : null,
    warnings,
  };
}
