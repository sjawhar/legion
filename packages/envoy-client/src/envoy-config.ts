import {
  chmodSync,
  closeSync,
  lstatSync,
  mkdtempSync,
  openSync,
  readFileSync,
  readlinkSync,
  rmSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
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

/** The omp profile's directory: `PI_CODING_AGENT_DIR` when `OMP_PROFILE` names a profile other
 * than `default`. */
function ompProfileDirectory(env: EnvoyConfigEnvironment): string | undefined {
  const profile = nonEmpty(env.OMP_PROFILE);
  return profile === undefined || profile === "default"
    ? undefined
    : absolute("PI_CODING_AGENT_DIR", nonEmpty(env.PI_CODING_AGENT_DIR));
}

function harnessDirectory(harness: EnvoyHarness, env: EnvoyConfigEnvironment): string | undefined {
  switch (harness) {
    case "omp":
      return ompProfileDirectory(env);
    case "claude":
      return absolute("CLAUDE_CONFIG_DIR", nonEmpty(env.CLAUDE_CONFIG_DIR));
    case "opencode":
      return absolute("OPENCODE_CONFIG_DIR", nonEmpty(env.OPENCODE_CONFIG_DIR));
    case "command":
      return (
        ompProfileDirectory(env) ??
        absolute("CLAUDE_CONFIG_DIR", nonEmpty(env.CLAUDE_CONFIG_DIR)) ??
        absolute("OPENCODE_CONFIG_DIR", nonEmpty(env.OPENCODE_CONFIG_DIR))
      );
    default: {
      const unknown: never = harness;
      throw new Error(`unknown harness ${String(unknown)}`);
    }
  }
}

/**
 * The envoy.json a caller reads. A plugin reads its own harness's directory (omp: a named
 * profile's `PI_CODING_AGENT_DIR`; Claude Code: `CLAUDE_CONFIG_DIR`; OpenCode:
 * `OPENCODE_CONFIG_DIR`) and no other harness's, else the file `ENVOY_CONFIG_FILE` names, else
 * `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`. A script (`command`) reads
 * `ENVOY_CONFIG_FILE` first, then the omp, Claude Code and OpenCode directories in that order, then
 * that same neutral file. Throws when the path it would use is relative: `ENVOY_CONFIG_FILE`, the
 * harness directory, or `XDG_CONFIG_HOME` for the neutral file.
 */
export function envoyConfigPath(harness: EnvoyHarness, env: EnvoyConfigEnvironment): string {
  const handed = nonEmpty(env.ENVOY_CONFIG_FILE);
  if (harness === "command" && handed !== undefined) {
    return path.resolve(absolute("ENVOY_CONFIG_FILE", handed));
  }
  const directory = harnessDirectory(harness, env);
  if (directory !== undefined) return path.resolve(directory, "envoy.json");
  if (handed !== undefined) return path.resolve(absolute("ENVOY_CONFIG_FILE", handed));
  const neutral = neutralConfig(env);
  if ("error" in neutral) throw new Error(neutral.error);
  return neutral.path;
}

const httpUrlSchema = z.url({ protocol: /^https?$/, error: "must be an http or https URL" });

// Strict per section, http(s) addresses, tolerant top level. The Dispatch server reads the same
// file and follows these validation and migration rules.
const DispatchSectionSchema = z.strictObject({
  enabled: z.boolean().optional(),
  serverUrl: httpUrlSchema.optional(),
  token: z.string().optional(),
});

const NatsUrlsSchema = z.array(z.string().trim().min(1, "must not be empty"));

const EnvoySectionSchema = z.strictObject({
  enabled: z.boolean().optional(),
  url: httpUrlSchema.optional(),
  natsUrls: NatsUrlsSchema.optional(),
  token: z.string().optional(),
  natsSeed: z.string().optional(),
});

// A top-level `natsUrls` is the layout before the `envoy` section: `readEnvoyConfig` reads it as
// `envoy.natsUrls` with a warning until it moves; a later release stops reading it.
const EnvoyFileSchema = z.looseObject({
  $schema: z.string().optional(),
  dispatch: DispatchSectionSchema.optional(),
  envoy: EnvoySectionSchema.optional(),
  natsUrls: NatsUrlsSchema.optional(),
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

/** A chosen file that does not exist: a dangling link's target (`missingConfigNotice`). */
export interface MissingConfig {
  readonly link: string | null;
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
      /** The file read: the one the harness rule chose (`envoyConfigPath`), or the OpenCode-era
       * file read in place of a missing neutral one. */
      readonly path: string;
      /** The chosen file's sections; empty when it does not exist. */
      readonly dispatch: DispatchFileSettings;
      readonly envoy: EnvoyFileSettings;
      /** The checkout's own file, when the working directory has one. */
      readonly checkout: CheckoutEnvoyConfig | null;
      /** Set when the chosen file does not exist. */
      readonly missing: MissingConfig | null;
      /** What was read from a place or shape a later release stops reading, and how to move it. */
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

/** Where `text` stops being JSON, as `line L, column C`, worked out without the parser's message,
 * which quotes the offending token: an unquoted token or seed would land in the error. */
function jsonErrorPosition(text: string): string | undefined {
  let at = 0;
  const fail = (): never => {
    throw new RangeError("not JSON");
  };
  const space = (): void => {
    while (at < text.length && " \t\n\r".includes(text.charAt(at))) at += 1;
  };
  const expect = (char: string): void => {
    if (text.charAt(at) !== char) fail();
    at += 1;
  };
  const string = (): void => {
    expect('"');
    while (at < text.length) {
      const char = text.charAt(at);
      if (char === '"') {
        at += 1;
        return;
      }
      if (char < " ") fail();
      at += char === "\\" ? 2 : 1;
    }
    fail();
  };
  const value = (): void => {
    space();
    const char = text.charAt(at);
    if (char === "{" || char === "[") {
      const close = char === "{" ? "}" : "]";
      at += 1;
      space();
      if (text.charAt(at) === close) {
        at += 1;
        return;
      }
      for (;;) {
        if (close === "}") {
          space();
          string();
          space();
          expect(":");
        }
        value();
        space();
        if (text.charAt(at) === ",") {
          at += 1;
          continue;
        }
        expect(close);
        return;
      }
    }
    if (char === '"') {
      string();
      return;
    }
    for (const word of ["true", "false", "null"]) {
      if (text.startsWith(word, at)) {
        at += word.length;
        return;
      }
    }
    const number = /^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(text.slice(at));
    if (number === null) fail();
    at += number?.[0].length ?? 0;
  };
  try {
    value();
    space();
    if (at < text.length) fail();
    return undefined;
  } catch {
    const before = text.slice(0, at).split("\n");
    return `line ${before.length}, column ${(before.at(-1)?.length ?? 0) + 1}`;
  }
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
  if (
    checkout &&
    typeof parsedJson === "object" &&
    parsedJson !== null &&
    "natsUrls" in parsedJson
  ) {
    return {
      kind: "invalid",
      reason: `${filePath}: the top-level natsUrls is not read, and a checkout's envoy.json holds only a dispatch section`,
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

/** Whether `value` can go in a header as one token: no whitespace and no control character. A
 * multi-line value would otherwise reach a `fetch` error, which quotes the header it refuses. */
function sendable(value: string): boolean {
  return !/[\s\p{Cc}]/u.test(value);
}

function runCommand(command: string, env: EnvoyConfigEnvironment): CommandOutcome {
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
        env: childEnv,
        stdin: "ignore",
        stdout: fd,
        stderr: "ignore",
        timeout: COMMAND_TIMEOUT_MS,
        detached: true,
      });
    } catch (error) {
      return { failure: `could not start: ${messageFor(error)}` };
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
      return { failure: `did not finish within ${COMMAND_TIMEOUT_MS / 1000} s` };
    }
    if (!result.success) {
      return { failure: `exited with ${result.signalCode ?? `status ${result.exitCode}`}` };
    }
    const value = readFileSync(output, "utf-8").trim();
    if (value.length === 0) return { failure: "printed nothing" };
    if (!sendable(value)) {
      return { failure: "printed more than one line, or whitespace inside its value" };
    }
    return { value };
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

/**
 * A value from the chosen file: as written, or, for one starting with `!`, its command's trimmed
 * stdout. The command runs under `/bin/sh -c` with `env`, in its own process group, for at most
 * 10 s, once per process: its outcome, value or failure, is kept by command. Its exit status
 * decides, and the wait is for the shell alone: once it exits or runs out of time, every process
 * of its group still running is killed, then its output is read. A non-zero exit, a timeout or
 * empty output throws naming the field and the file. A value with whitespace or a control
 * character inside it, as written or as printed, throws too, since it cannot be sent as one
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
  let outcome = commandOutcomes.get(command);
  if (outcome === undefined) {
    outcome = runCommand(command, env);
    commandOutcomes.set(command, outcome);
  }
  if ("failure" in outcome) throw new Error(`${filePath}: ${field}'s command ${outcome.failure}`);
  return outcome.value;
}

/** The refusal of a plain variable's token with whitespace or a control character inside it. */
export function unsendableRefusal(source: string, value: string): string | null {
  return sendable(value) ? null : `${source} holds whitespace or a control character`;
}

/** The refusal of a `!` value in a checkout's file, which the repository controls. */
export function checkoutCommandRefusal(checkoutPath: string, field: string): string {
  return `${checkoutPath}: ${field} is a command, which a checkout's envoy.json cannot run`;
}

/** The notice for a chosen file that does not exist, saying what is `off` for want of it, and
 * naming the link's target when the file is a dangling link (`readEnvoyConfig`). */
export function missingConfigNotice(filePath: string, missing: MissingConfig, off: string): string {
  const link = missing.link === null ? "" : ` (a link to ${missing.link}, which does not exist)`;
  return `no envoy.json at ${filePath}${link}: ${off}`;
}

/** The OpenCode-era user file, read in place of a missing chosen file: only when that file is the
 * neutral one at `$HOME/.config/legion/envoy.json` (`XDG_CONFIG_HOME` unset, empty or
 * `$HOME/.config`). An `XDG_CONFIG_HOME` that names another directory, such as the one a Legion
 * pane is given under the daemon's state directory, gets no such file: the user's does not belong
 * there. */
function openCodeUserFile(filePath: string, env: EnvoyConfigEnvironment): string | null {
  const neutral = neutralConfig(env);
  if ("error" in neutral || neutral.path !== path.resolve(filePath)) return null;
  const home = nonEmpty(env.HOME) ?? homedir();
  if (neutral.path !== path.resolve(home, ".config", "legion", "envoy.json")) return null;
  return path.join(home, ".config", "opencode", "envoy.json");
}

/** A chosen file's sections, with a top-level `natsUrls` read as `envoy.natsUrls` with a warning
 * until it moves (a later release stops reading it), and refused when it is also set in `envoy`. */
function chosenSections(
  filePath: string,
  settings: EnvoyFile,
  warnings: string[],
  warn: boolean
):
  | { readonly kind: "valid"; readonly settings: EnvoyFile }
  | { readonly kind: "invalid"; readonly error: string } {
  const { natsUrls, ...rest } = settings;
  if (natsUrls === undefined) return { kind: "valid", settings };
  if (rest.envoy?.natsUrls !== undefined) {
    return {
      kind: "invalid",
      error: `${filePath}: natsUrls and envoy.natsUrls are both set; keep envoy.natsUrls`,
    };
  }
  if (warn) {
    warnings.push(
      `${filePath}: the top-level natsUrls is read as envoy.natsUrls until it moves (a later release stops reading it): move natsUrls into envoy.natsUrls`
    );
  }
  return { kind: "valid", settings: { ...rest, envoy: { ...rest.envoy, natsUrls } } };
}

/**
 * Read the envoy.json at `filePath` (`envoyConfigPath`) and the checkout's `.envoy/envoy.json`
 * under `cwd`, whose `dispatch` section Dispatch's resolver merges over the chosen file's. Runs no
 * command. A missing chosen file sets `missing`; a chosen file that cannot be read or is not valid
 * is `failed`, naming the file and the key. The checkout's file keeps its own outcome: one that
 * cannot be read or is not valid is an `invalid` checkout, which only Dispatch reads, and an
 * `envoy` section in it sets `checkout.envoyError` rather than configuring anything.
 *
 * Three saved inputs from before this layout are read until they move, each with a warning in
 * `warnings`: in place of a missing neutral file at `$HOME/.config/legion/envoy.json`, the
 * OpenCode-era `~/.config/opencode/envoy.json`, trusted as the chosen file is; in place of a
 * missing `.envoy/envoy.json`, the checkout's `.opencode/envoy.json`, under every checkout rule;
 * and a chosen file's top-level `natsUrls` as `envoy.natsUrls`. A later release stops reading
 * each input; a checkout's top-level `natsUrls` and one set beside `envoy.natsUrls` remain
 * refused.
 */
export function readEnvoyConfig(
  filePath: string,
  env: EnvoyConfigEnvironment,
  options: { readonly cwd: string }
): EnvoyConfig {
  const warnings: string[] = [];
  let readPath = filePath;
  let chosen = parseFile(filePath, EnvoyFileSchema, false);
  const openCode = chosen.kind === "absent" ? openCodeUserFile(filePath, env) : null;
  if (openCode !== null) {
    const old = parseFile(openCode, EnvoyFileSchema, false);
    if (old.kind !== "absent") {
      chosen = old;
      readPath = openCode;
      warnings.push(
        `${openCode} is read until it moves (a later release stops reading it): mkdir -p ~/.config/legion && mv ${openCode} ${filePath}, then move a top-level natsUrls into envoy.natsUrls`
      );
    }
  }
  if (chosen.kind === "invalid") return { kind: "failed", path: readPath, error: chosen.reason };
  const sections =
    chosen.kind === "valid"
      ? chosenSections(readPath, chosen.settings, warnings, readPath === filePath)
      : { kind: "valid" as const, settings: {} };
  if (sections.kind === "invalid") return { kind: "failed", path: readPath, error: sections.error };
  const own = sections.settings;

  const resolvedChosen = path.resolve(filePath);
  const checkoutFile = (candidate: string): ParsedFile<z.infer<typeof CheckoutFileSchema>> =>
    candidate === resolvedChosen
      ? { kind: "absent", link: null }
      : parseFile(candidate, CheckoutFileSchema, true);
  let checkoutPath = path.resolve(options.cwd, ".envoy", "envoy.json");
  let parsedCheckout = checkoutFile(checkoutPath);
  if (parsedCheckout.kind === "absent") {
    const oldCheckout = path.resolve(options.cwd, ".opencode", "envoy.json");
    const old = checkoutFile(oldCheckout);
    if (old.kind !== "absent") {
      warnings.push(
        `${oldCheckout} is read until it moves (a later release stops reading it): mkdir -p ${path.dirname(checkoutPath)} && mv ${oldCheckout} ${checkoutPath}`
      );
      parsedCheckout = old;
      checkoutPath = oldCheckout;
    }
  }
  let checkout: CheckoutEnvoyConfig | null = null;
  if (parsedCheckout.kind === "invalid") {
    checkout = { kind: "invalid", path: checkoutPath, error: parsedCheckout.reason };
  } else if (parsedCheckout.kind === "valid") {
    checkout = {
      kind: "valid",
      path: checkoutPath,
      dispatch: parsedCheckout.settings.dispatch ?? {},
      envoyError: Object.hasOwn(parsedCheckout.settings, "envoy")
        ? `${checkoutPath}: envoy is not read from a checkout's envoy.json; put it in ${filePath}`
        : null,
    };
  }
  return {
    kind: "read",
    path: readPath,
    dispatch: own.dispatch ?? {},
    envoy: own.envoy ?? {},
    checkout,
    missing: chosen.kind === "absent" ? { link: chosen.link } : null,
    warnings,
  };
}
