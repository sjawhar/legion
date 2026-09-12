import { spawnSync } from "node:child_process";
import os from "node:os";
import path from "node:path";
import { parse } from "yaml";
import { z } from "zod";
import { stripDispatchEnv } from "./environment";
import { DEFAULT_OMP_INVOCATION } from "./omp-pin";

export type GitHubAppRole = "implement" | "review";

export interface GitHubAppRoleConfig {
  appId: string;
  privateKey: string;
  installations?: Record<string, string>;
}

export type GitHubAppsConfig = Partial<Record<GitHubAppRole, GitHubAppRoleConfig>>;

export const RUNTIMES = ["tmux", "kubernetes"] as const;
export type RuntimeName = (typeof RUNTIMES)[number];

export interface DaemonConfig {
  project: string;
  legionId: string;
  port: number;
  /** Which `Runtime` (`runtime.ts`) starts, probes, and stops Legion processes: `tmux` (the
   * default: panes on the daemon's private tmux server) or `kubernetes` (pods; refuses startup
   * until the Kubernetes runtime lands). */
  runtime: RuntimeName;
  /** The daemon API URL every spawned process is told (`LEGION_DAEMON_URL`), normalized with no
   * trailing slash. Under tmux it is always `http://127.0.0.1:<port>` — the default, and the only
   * accepted value (anything else is an inherited outer pane's `LEGION_DAEMON_URL`); required
   * under kubernetes, where a pod cannot reach the daemon's loopback. */
  daemonUrl: string;
  /** The API listen address. `127.0.0.1` unless `runtime` is kubernetes, where the in-cluster
   * daemon must be reachable by its pods. */
  bind: string;
  envoyUrl: string;
  /**
   * Optional dispatch service base URL (no `/mcp` suffix), passed through to
   * spawned session environments as DISPATCH_URL so the native dispatch tool
   * targets a specific service (the smoke rig points it at its own
   * instance). When unset, sessions fall back to their envoy.json dispatch
   * config.
   */
  dispatchUrl?: string;
  /**
   * Bearer token for the dispatch service, read from the `DISPATCH_TOKEN` environment
   * variable only (never a YAML key). Required when `dispatchUrl` is set — spawned panes
   * would otherwise have a dispatch service to target but no credential to authenticate
   * with, so the native dispatch tool would fail to register. Undefined whenever
   * `dispatchUrl` is unset, even if the environment variable is present.
   */
  dispatchToken?: string;
  /** The Dispatch project key that owns Legion's issue lifecycle (D1/D2): the daemon's durable
   * Dispatch consumer, key-prefix filter, and lifecycle-status writes are all scoped to this
   * project. Always required — Dispatch is the sole source of Legion's issue lifecycle. */
  dispatchProject: string;
  natsUrls: string[];
  ompInvocation: string;
  /** Argv prefix prepended to every OMP invocation inside a spawned pane — root, worker, and
   * controller alike — and to the two startup capability probes, so provider credentials (or any
   * other wrapper the operator needs) are obtained *inside* the pane process rather than carried
   * by the daemon itself. Never exported to the daemon's own environment or passed as tmux `-e`
   * pairs (see `processes.ts`'s strip invariant). Empty by default — nothing is prepended. */
  ompLaunchPrefix: string[];
  /** `owner/name` GitHub repositories the durable per-repo GitHub intake consumes; required, non-empty. */
  repos: string[];
  /** The single GitHub repository every Legion issue/tree resolves to for credential routing,
   * PR lookups, and workspace provisioning (`repos[0]`, validated at config load to be the only
   * entry — a Dispatch issue key carries no owner/repo of its own, so this is now the sole
   * source of that mapping; more than one configured repo has no way to pick one per issue and
   * is rejected at config load instead of guessing). */
  repo: `${string}/${string}`;
  admissionCap: number;
  workerCap: number;
  maxRecursionDepth: number;
  lingerHours: number;
  maxFixAttempts: number;
  resyncIntervalMs: number;
  /** Seconds to wait for a single retiring worker's shim to close its socket gracefully before
   * its pane is killed directly (a dead-socket respawn, or a boot-time reconnect probe that
   * found the socket unreachable). */
  workerStopTimeoutSeconds: number;
  /** Seconds to wait for every process under a closing/expired tree to close its shim socket
   * gracefully, each on its own clock, before that one process's pane is killed directly. */
  treeStopTimeoutSeconds: number;
  /** An observation interval, never a hard deadline: at each interval, a spawned worker with no
   * `/worker/started` confirmation yet is probed (its tmux pane, or a reachable/answering shim
   * socket) before anything happens — a live pane or socket just re-arms the watch for another
   * interval. Only a boot whose pane is gone *and* whose socket refuses a connection is retired,
   * launch-failure counted, and retried (or escalated to `worker-died` at the threshold). */
  workerBootTimeoutSeconds: number;
  /** Caps how many consecutive `workerBootTimeoutSeconds` intervals a boot may spend
   * probe-alive-but-still-unconfirmed before the watchdog stops re-arming and treats it as a
   * boot failure instead (retired, launch-failure counted, retried through the same threshold
   * path a dead pane would be) — a pane that keeps answering forever without ever registering
   * is not "slow", it never actually completed its boot. */
  workerBootRegistrationDeadlineIntervals: number;
  /** Seconds a ready-confirmed phase worker may sit idle with nothing assigned to it — its role not
   * the issue's active phase, no queued task — before the daemon retires it: a graceful `shutdown`
   * over its shim, locator cleared, OMP session file kept so the next `spawn_worker` for the role
   * resumes the same agent (`--resume`). Never applies to an `architect` role: an architect has no
   * phase of its own (it is never `phases[issue].phase`, so the not-active-phase test would pass on
   * every idle), it parks by design between wakes for the life of its subtree, and each wake to a
   * retired one would relaunch it through the no-holder recovery — one relaunch per wake costs more
   * than one idle process per child issue. `worker_idle_retire_seconds` /
   * `LEGION_WORKER_IDLE_RETIRE_SECONDS`; default 600; the literal `0` disables the timer entirely (a
   * finished worker then stays resident until its tree closes); at most
   * `MAX_WORKER_IDLE_RETIRE_SECONDS`. */
  workerIdleRetireSeconds: number;
  /** Seconds before a single worker RPC request over a `legion worker-shim` unix socket
   * (`negotiate_protocol`/`get_state`/`prompt`) times out. Governs the connect-time
   * `negotiate_protocol` round trip that `markTreeReady`/`workerReady`/`markControllerReady`
   * kick off in the background after `/process/ready`/`/worker/ready`/`/controller/ready`
   * already responded — small by default, raised only under measured load sensitivity, never a
   * substitute for those routes responding before they dial back into the caller's own socket. */
  workerRpcTimeoutSeconds: number;
  /** TCP port the worker stream listener (`worker-stream-listener.ts`) accepts reverse-dialed
   * `legion worker-shim --connect` streams on, bound to the same address as the API.
   * `worker_stream_port` / `LEGION_WORKER_STREAM_PORT`; default `port + 1`. */
  workerStreamPort: number;
  gates: { design: "root-issues" | "off" };
  githubApps: GitHubAppsConfig;
  stateDir: string;
}

export interface LoadedConfigFile {
  fields: Record<string, unknown>;
}

export interface LoadConfigFileOptions {
  /**
   * When false, github_apps.<role>.private_key_command is validated for
   * presence but never executed — the private key becomes the placeholder
   * "(not executed)". Used by `legion start --check-config` so a config
   * validation pass never runs an arbitrary shell command from the file.
   * Defaults to true (the daemon always resolves real secrets).
   */
  resolveSecrets?: boolean;
}

export interface ResolveDaemonConfigOptions {
  env?: Record<string, string | undefined>;
  configFile?: LoadedConfigFile;
  cliOverrides?: Partial<DaemonConfig>;
}

export interface ResolveDaemonConfigResult {
  config: DaemonConfig;
}

const CONFIG_ANY_KEY = Symbol("config-any-key");

type ConfigSchema = {
  [key: string]: ConfigSchema | null;
  [CONFIG_ANY_KEY]?: ConfigSchema | null;
};
type ValueSource = "cli" | "config" | "env" | "default";

const DEFAULT_PORT = 13370;
const DEFAULT_BIND = "127.0.0.1";
const DEFAULT_ENVOY_URL = "http://127.0.0.1:9020";
const DEFAULT_ADMISSION_CAP = 4;
const DEFAULT_WORKER_CAP = 10;
const DEFAULT_MAX_RECURSION_DEPTH = 8;
const DEFAULT_LINGER_HOURS = 72;
const DEFAULT_MAX_FIX_ATTEMPTS = 3;
const DEFAULT_RESYNC_INTERVAL_MS = 600_000;
const DEFAULT_WORKER_STOP_TIMEOUT_SECONDS = 10;
const DEFAULT_TREE_STOP_TIMEOUT_SECONDS = 60;
const DEFAULT_WORKER_BOOT_TIMEOUT_SECONDS = 120;
const DEFAULT_WORKER_BOOT_REGISTRATION_DEADLINE_INTERVALS = 3;
const DEFAULT_WORKER_RPC_TIMEOUT_SECONDS = 5;
const DEFAULT_WORKER_IDLE_RETIRE_SECONDS = 600;
/** The largest whole number of seconds whose millisecond delay still fits the signed 32-bit timer
 * delay `armIdleRetire` hands to `setTimeout` (`seconds * 1000 <= 2_147_483_647`). Beyond it the
 * runtime clamps the delay to 1 ms — every finished worker retired the instant it went idle, the
 * feature inverted for an operator who set a huge value to mean "never" — so a larger value is a
 * startup error that points at `0`, the real disable value. */
const MAX_WORKER_IDLE_RETIRE_SECONDS = 2_147_483;

const CONFIG_SCHEMA: ConfigSchema = {
  project: null,
  port: null,
  runtime: null,
  daemon_url: null,
  bind: null,
  envoy_url: null,
  // Recognized (not an "unknown key") so setting it surfaces the specific replaced-by-dispatch_url
  // error below instead of the generic "Unknown config key" message. Never mapped to a field.
  dispatch_mcp_url: null,
  dispatch_url: null,
  dispatch_project: null,
  nats_urls: null,
  omp_invocation: null,
  omp_launch_prefix: null,
  // Recognized (not an "unknown key") so setting it surfaces the specific replaced-by-dispatch_project
  // error below instead of the generic "Unknown config key" message. Never mapped to a field.
  board_project_ids: null,
  repos: null,
  // Recognized (not an "unknown key") so setting it surfaces the specific removed-setting error
  // below instead of the generic "Unknown config key" message. Never mapped to a field.
  app_logins: null,
  admission_cap: null,
  worker_cap: null,
  // Recognized (not an "unknown key") so setting it surfaces the specific replaced-by-worker_cap
  // error below instead of the generic "Unknown config key" message. Never mapped to a field.
  worker_budget: null,
  max_recursion_depth: null,
  linger_hours: null,
  max_fix_attempts: null,
  resync_interval_seconds: null,
  worker_stop_timeout_seconds: null,
  tree_stop_timeout_seconds: null,
  worker_boot_timeout_seconds: null,
  worker_idle_retire_seconds: null,
  worker_boot_registration_deadline_intervals: null,
  worker_rpc_timeout_seconds: null,
  worker_stream_port: null,
  state_dir: null,
  // `merge` is recognized (not an "unknown key") so setting it surfaces the specific
  // removed-setting error `parseGates` throws below instead of the generic "Unknown config key"
  // message. Never mapped to a field.
  gates: { design: null, merge: null },
  github_apps: {
    implement: {
      app_id: null,
      private_key: null,
      private_key_command: null,
      installations: { [CONFIG_ANY_KEY]: null },
    },
    review: {
      app_id: null,
      private_key: null,
      private_key_command: null,
      installations: { [CONFIG_ANY_KEY]: null },
    },
  },
};

const UnknownRecordSchema = z.record(z.unknown());

function resolveValue<T>(
  cliValue: T | undefined,
  configValue: T | undefined,
  envValue: T | undefined,
  defaultValue: T
): { value: T; source: ValueSource } {
  if (cliValue !== undefined) return { value: cliValue, source: "cli" };
  if (configValue !== undefined) return { value: configValue, source: "config" };
  if (envValue !== undefined) return { value: envValue, source: "env" };
  return { value: defaultValue, source: "default" };
}

function readString(value: unknown, field: string): string | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== "string") throw new Error(`${field} must be a string`);
  return value;
}

function readStringArray(value: unknown, field: string): string[] | undefined {
  if (value === undefined || value === null) return undefined;
  if (
    !Array.isArray(value) ||
    value.some((entry) => typeof entry !== "string" || entry.length === 0)
  ) {
    throw new Error(`${field} must be an array of non-empty strings`);
  }
  return [...new Set(value)];
}

/** Like `readStringArray`, but for an ordered argv list where position and duplicate entries are
 * both meaningful — a launch prefix is a command line, not a set, so (unlike `readStringArray`)
 * this never deduplicates or otherwise reorders its entries. */
function readArgv(value: unknown, field: string): string[] | undefined {
  if (value === undefined || value === null) return undefined;
  if (
    !Array.isArray(value) ||
    value.some((entry) => typeof entry !== "string" || entry.length === 0)
  ) {
    throw new Error(`${field} must be an array of non-empty strings`);
  }
  return [...value] as string[];
}

function readNumber(value: unknown, field: string): number | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new Error(`${field} must be a finite number`);
  }
  return value;
}

function readPositiveInteger(value: unknown, field: string): number | undefined {
  const number = readNumber(value, field);
  if (number === undefined) return undefined;
  if (!Number.isSafeInteger(number) || number <= 0) {
    throw new Error(`${field} must be a positive integer`);
  }
  return number;
}

function parseEnvPositiveInteger(value: string | undefined, field: string): number | undefined {
  if (value === undefined || value === "") return undefined;
  const number = Number(value);
  if (!Number.isSafeInteger(number) || number <= 0) {
    throw new Error(`${field} must be a positive integer`);
  }
  return number;
}

/** The one validation every source of `worker_idle_retire_seconds` (file, environment, cli
 * override) funnels through: a whole number from `0` (the timer disabled) to
 * `MAX_WORKER_IDLE_RETIRE_SECONDS` inclusive. `-0` is rejected explicitly — the YAML loader hands
 * it through as a negative zero, and `-0 < 0` is false — because "disabled" must be the literal
 * `0`, never a value that merely computes to zero. */
function checkIdleRetireSeconds(number: number, field: string): number {
  if (!Number.isInteger(number) || number < 0 || Object.is(number, -0)) {
    throw new Error(`${field} must be a non-negative integer`);
  }
  if (number > MAX_WORKER_IDLE_RETIRE_SECONDS) {
    throw new Error(
      `${field} must be at most ${MAX_WORKER_IDLE_RETIRE_SECONDS} (the largest whole number of seconds whose millisecond delay fits a 32-bit timer); use 0 to disable idle retirement`
    );
  }
  return number;
}

/** As `readPositiveInteger`, but for `worker_idle_retire_seconds`, the one lifecycle number where
 * zero is a meaningful setting ("never retire an idle worker") rather than the typo it would be for
 * a cap or a timeout — and the one with an upper bound (see `MAX_WORKER_IDLE_RETIRE_SECONDS`). */
function readIdleRetireSeconds(value: unknown, field: string): number | undefined {
  const number = readNumber(value, field);
  if (number === undefined) return undefined;
  return checkIdleRetireSeconds(number, field);
}

/** The environment-variable twin of `readIdleRetireSeconds`. An unset or empty variable is "unset",
 * exactly as `parseEnvPositiveInteger` treats it; anything else must be the canonical decimal
 * spelling of a whole number. `Number()` alone would read `"  "` and `"-0"` as zero and `"05"` or
 * `"+5"` as five, and a mistyped variable that silently disables the timer is precisely the
 * fallback this key must never have. */
function parseEnvIdleRetireSeconds(value: string | undefined, field: string): number | undefined {
  if (value === undefined || value === "") return undefined;
  if (!/^(0|[1-9]\d*)$/.test(value)) {
    throw new Error(`${field} must be a non-negative integer`);
  }
  return checkIdleRetireSeconds(Number(value), field);
}

function parseCsv(value: string | undefined, field: string): string[] | undefined {
  if (value === undefined) return undefined;
  const values = value
    .split(",")
    .map((entry) => entry.trim())
    .filter((entry) => entry.length > 0);
  if (value.trim().length > 0 && values.length === 0) {
    throw new Error(`${field} must contain at least one value`);
  }
  return [...new Set(values)];
}

/** Splits `LEGION_OMP_LAUNCH_PREFIX` into argv the way a POSIX shell tokenizes a command line —
 * whitespace-separated words, `'...'`/`"..."` quoting, and backslash escapes outside single
 * quotes — since the YAML form (`omp_launch_prefix`) is already an array and only the single-
 * string environment form needs splitting. Order and duplicate entries are preserved (this is
 * argv, not a set); an empty/whitespace-only value resolves to an empty prefix, not "unset". */
function parseShellWords(value: string | undefined, field: string): string[] | undefined {
  if (value === undefined) return undefined;
  const words: string[] = [];
  let current = "";
  let hasCurrent = false;
  let quote: "'" | '"' | undefined;
  for (let index = 0; index < value.length; index += 1) {
    const char = value[index] as string;
    if (quote === "'") {
      if (char === "'") quote = undefined;
      else current += char;
      continue;
    }
    if (quote === '"') {
      if (char === '"') {
        quote = undefined;
      } else if (
        char === "\\" &&
        index + 1 < value.length &&
        '"\\$`'.includes(value[index + 1] as string)
      ) {
        index += 1;
        current += value[index] as string;
      } else {
        current += char;
      }
      continue;
    }
    if (char === "'" || char === '"') {
      quote = char;
      hasCurrent = true;
      continue;
    }
    if (char === "\\") {
      if (index + 1 >= value.length) {
        throw new Error(`${field} has a trailing unescaped backslash`);
      }
      index += 1;
      current += value[index] as string;
      hasCurrent = true;
      continue;
    }
    if (/\s/.test(char)) {
      if (hasCurrent) {
        words.push(current);
        current = "";
        hasCurrent = false;
      }
      continue;
    }
    current += char;
    hasCurrent = true;
  }
  if (quote !== undefined) throw new Error(`${field} has an unterminated ${quote} quote`);
  if (hasCurrent) words.push(current);
  if (words.some((word) => word.length === 0)) {
    throw new Error(`${field} must not contain an empty argument (e.g. a bare '' or "")`);
  }
  return words;
}

function requireNonEmpty(value: string, field: string): string {
  if (value.trim().length === 0) throw new Error(`${field} must not be empty`);
  return value;
}

function parseRuntime(value: string | undefined, field: string): RuntimeName | undefined {
  if (value === undefined) return undefined;
  if (!RUNTIMES.some((runtime) => runtime === value)) {
    throw new Error(`${field} must be 'tmux' or 'kubernetes'`);
  }
  return value as RuntimeName;
}

function validateUrl(value: string, field: string): string {
  try {
    new URL(value);
  } catch {
    throw new Error(`${field} must be a valid URL`);
  }
  return value;
}

function validateRepoSlug(value: string, field: string): string {
  if (!/^[^/]+\/[^/]+$/.test(value)) {
    throw new Error(`${field} entries must be "owner/name" (got "${value}")`);
  }
  return value;
}

const DISPATCH_PROJECT_PATTERN = /^[A-Z][A-Z0-9]*$/;

function validateDispatchProject(value: string, field: string): string {
  if (!DISPATCH_PROJECT_PATTERN.test(value)) {
    throw new Error(`${field} must match ^[A-Z][A-Z0-9]*$`);
  }
  return value;
}

/** The dispatch service base URL never carries its clients' `/mcp` alias; the clients strip it
 * themselves. Rejecting it here surfaces a stale config value instead of silently misrouting.
 * Checked against the parsed URL's pathname (trailing slash stripped) rather than the raw
 * string, so `.../mcp/` and `.../mcp?query=1` are caught too, not just an exact `/mcp` suffix. */
function requireNoMcpSuffix(value: string, field: string): string {
  const pathname = new URL(value).pathname.replace(/\/+$/, "");
  if (pathname.endsWith("/mcp")) {
    throw new Error(`${field} must be the dispatch service base URL, not the /mcp endpoint`);
  }
  return value;
}

/** Canonicalizes a validated base URL to have no trailing slash, so appending a path segment
 * (e.g. `` `${dispatchUrl}/mcp` ``) never doubles the slash regardless of how the operator wrote
 * the configured value (`http://x` and `http://x/` both resolve to `http://x`). Rejects a query
 * string or fragment outright — string-concatenating a path segment onto either would build a
 * broken URL (the query/fragment landing before the appended path), so there is no correct way
 * to canonicalize one. */
function normalizeBaseUrl(value: string, field: string): string {
  const url = new URL(value);
  if (url.search || url.hash) {
    throw new Error(`${field} must not include a query string or fragment`);
  }
  return url.toString().replace(/\/+$/, "");
}

function collectUnknownKeys(
  value: unknown,
  schema: ConfigSchema | null,
  pathParts: string[],
  unknownKeys: string[]
): void {
  const parsed = UnknownRecordSchema.safeParse(value);
  if (!schema || !parsed.success) return;
  for (const [key, child] of Object.entries(parsed.data)) {
    const childSchema = Object.hasOwn(schema, key) ? schema[key] : schema[CONFIG_ANY_KEY];
    if (childSchema === undefined) {
      unknownKeys.push([...pathParts, key].join("."));
      continue;
    }
    collectUnknownKeys(child, childSchema, [...pathParts, key], unknownKeys);
  }
}

function readStringRecord(value: unknown, field: string): Record<string, string> | undefined {
  if (value === undefined || value === null) return undefined;
  const parsed = z.record(z.string()).safeParse(value);
  if (!parsed.success) throw new Error(`${field} must be a mapping of strings`);
  return parsed.data;
}

function executePrivateKeyCommand(command: string, field: string): string {
  const result = spawnSync("sh", ["-c", command], {
    encoding: "utf8",
    env: stripDispatchEnv(process.env),
  });
  if (result.error || result.status !== 0) {
    const status = result.status === null ? "unknown" : String(result.status);
    const stderr = result.stderr?.trim();
    throw new Error(`${field} failed (exit ${status})${stderr ? `: ${stderr}` : ""}`);
  }
  const privateKey = result.stdout?.trim() ?? "";
  if (!privateKey) throw new Error(`${field} produced empty output`);
  return privateKey;
}

function loadGitHubApps(value: unknown, resolveSecrets: boolean): GitHubAppsConfig | undefined {
  if (value === undefined || value === null) return undefined;
  const parsedApps = UnknownRecordSchema.safeParse(value);
  if (!parsedApps.success) throw new Error("github_apps must be a mapping");

  const apps: GitHubAppsConfig = {};
  for (const role of ["implement", "review"] as const) {
    const roleValue = parsedApps.data[role];
    if (roleValue === undefined || roleValue === null) continue;
    const parsedRole = UnknownRecordSchema.safeParse(roleValue);
    if (!parsedRole.success) throw new Error(`github_apps.${role} must be a mapping`);

    const appId = readString(parsedRole.data.app_id, `github_apps.${role}.app_id`);
    const inlineKey = readString(parsedRole.data.private_key, `github_apps.${role}.private_key`);
    const command = readString(
      parsedRole.data.private_key_command,
      `github_apps.${role}.private_key_command`
    );
    const hasInlineKey = inlineKey !== undefined && inlineKey !== "";
    const hasCommand = command !== undefined && command !== "";
    if (appId === undefined || appId === "") {
      throw new Error(`github_apps.${role} is missing required fields: app_id`);
    }
    if (hasInlineKey === hasCommand) {
      throw new Error(
        `github_apps.${role} requires exactly one of private_key or private_key_command`
      );
    }
    let privateKey: string;
    if (inlineKey !== undefined && inlineKey !== "") {
      privateKey = inlineKey;
    } else if (command !== undefined && command !== "") {
      privateKey = resolveSecrets
        ? executePrivateKeyCommand(command, `github_apps.${role}.private_key_command`)
        : "(not executed)";
    } else {
      throw new Error(`github_apps.${role} requires a private key source`);
    }
    apps[role] = {
      appId,
      privateKey,
      installations:
        readStringRecord(parsedRole.data.installations, `github_apps.${role}.installations`) ?? {},
    };
  }
  return apps;
}

/** `gates` is optional in the file: absent means the `DaemonConfig` default (`design:
 * root-issues`), applied by `resolveDaemonConfig`. A present block must be a mapping; `design`
 * is individually optional, and a present `merge` key is rejected -- human approval of a pull
 * request is the repository's own branch protection or CODEOWNERS rule, which Legion never
 * reads or writes. */
function parseGates(value: unknown, field: string): DaemonConfig["gates"] | undefined {
  if (value === undefined) return undefined;
  const parsed = UnknownRecordSchema.safeParse(value);
  if (!parsed.success) throw new Error(`${field} must be a mapping`);
  if (parsed.data.merge !== undefined) {
    throw new Error(
      "gates.merge is not a Legion setting: human approval of a pull request is the repository's own branch protection or CODEOWNERS rule, which Legion never reads or writes"
    );
  }
  const design = readString(parsed.data.design, `${field}.design`) ?? "root-issues";
  if (design !== "root-issues" && design !== "off") {
    throw new Error(`${field}.design must be 'root-issues' or 'off'`);
  }
  return { design };
}

function fileString(fields: Record<string, unknown>, key: string): string | undefined {
  const value = fields[key];
  return typeof value === "string" ? value : undefined;
}

function fileStringArray(fields: Record<string, unknown>, key: string): string[] | undefined {
  const value = fields[key];
  return Array.isArray(value) && value.every((entry) => typeof entry === "string")
    ? (value as string[])
    : undefined;
}

function fileNumber(fields: Record<string, unknown>, key: string): number | undefined {
  const value = fields[key];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function fileGates(fields: Record<string, unknown>): DaemonConfig["gates"] | undefined {
  const parsed = UnknownRecordSchema.safeParse(fields.gates);
  return parsed.success ? (parsed.data as DaemonConfig["gates"]) : undefined;
}

function fileGitHubApps(fields: Record<string, unknown>): GitHubAppsConfig | undefined {
  const parsed = UnknownRecordSchema.safeParse(fields.githubApps);
  return parsed.success ? (parsed.data as GitHubAppsConfig) : undefined;
}

export function loadConfigFromFile(
  yamlText: string,
  configDir: string,
  options: LoadConfigFileOptions = {}
): LoadedConfigFile {
  let parsed: unknown;
  try {
    parsed = parse(yamlText);
  } catch (error) {
    throw new Error(
      `Invalid YAML config: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  if (parsed === undefined || parsed === null) return { fields: {} };
  const parsedRoot = UnknownRecordSchema.safeParse(parsed);
  if (!parsedRoot.success) throw new Error("Config file root must be a mapping");
  const config = parsedRoot.data;

  const unknownKeys: string[] = [];
  collectUnknownKeys(config, CONFIG_SCHEMA, [], unknownKeys);
  if (unknownKeys.length > 0) {
    throw new Error(unknownKeys.map((key) => `Unknown config key "${key}"`).join("; "));
  }
  const fields: Record<string, unknown> = {};

  const project = readString(config.project, "project");
  if (project !== undefined) fields.legionId = requireNonEmpty(project, "project");
  const port = readPositiveInteger(config.port, "port");
  if (port !== undefined) {
    if (port > 65535) throw new Error("port must be at most 65535");
    fields.port = port;
  }
  const runtime = parseRuntime(readString(config.runtime, "runtime"), "runtime");
  if (runtime !== undefined) fields.runtime = runtime;
  const daemonUrl = readString(config.daemon_url, "daemon_url");
  if (daemonUrl !== undefined) fields.daemonUrl = validateUrl(daemonUrl, "daemon_url");
  const bind = readString(config.bind, "bind");
  if (bind !== undefined) fields.bind = requireNonEmpty(bind, "bind");
  const envoyUrl = readString(config.envoy_url, "envoy_url");
  if (envoyUrl !== undefined) fields.envoyUrl = validateUrl(envoyUrl, "envoy_url");
  if (config.dispatch_mcp_url !== undefined) {
    throw new Error(
      "dispatch_mcp_url was replaced by dispatch_url (the service base URL, no /mcp)"
    );
  }
  if (config.worker_budget !== undefined) {
    throw new Error("worker_budget was replaced by worker_cap");
  }
  const dispatchUrlField = readString(config.dispatch_url, "dispatch_url");
  if (dispatchUrlField !== undefined) {
    fields.dispatchUrl = validateUrl(dispatchUrlField, "dispatch_url");
  }
  const dispatchProjectField = readString(config.dispatch_project, "dispatch_project");
  if (dispatchProjectField !== undefined) {
    fields.dispatchProject = validateDispatchProject(dispatchProjectField, "dispatch_project");
  }
  const natsUrls = readStringArray(config.nats_urls, "nats_urls");
  if (natsUrls !== undefined) fields.natsUrls = natsUrls;
  const ompInvocation = readString(config.omp_invocation, "omp_invocation");
  if (ompInvocation !== undefined) {
    fields.ompInvocation = requireNonEmpty(ompInvocation, "omp_invocation");
  }
  const ompLaunchPrefix = readArgv(config.omp_launch_prefix, "omp_launch_prefix");
  if (ompLaunchPrefix !== undefined) fields.ompLaunchPrefix = ompLaunchPrefix;
  if (config.board_project_ids !== undefined) {
    throw new Error("board_project_ids was replaced by dispatch_project");
  }
  const repos = readStringArray(config.repos, "repos");
  if (repos !== undefined) fields.repos = repos.map((repo) => validateRepoSlug(repo, "repos"));
  if (config.app_logins !== undefined) {
    throw new Error(
      "app_logins is not a Legion setting: human approval of a pull request is the repository's own branch protection or CODEOWNERS rule, which Legion never reads or writes"
    );
  }

  for (const [fileKey, configKey] of [
    ["admission_cap", "admissionCap"],
    ["worker_cap", "workerCap"],
    ["max_recursion_depth", "maxRecursionDepth"],
    ["linger_hours", "lingerHours"],
    ["max_fix_attempts", "maxFixAttempts"],
    ["worker_stop_timeout_seconds", "workerStopTimeoutSeconds"],
    ["tree_stop_timeout_seconds", "treeStopTimeoutSeconds"],
    ["worker_boot_timeout_seconds", "workerBootTimeoutSeconds"],
    ["worker_boot_registration_deadline_intervals", "workerBootRegistrationDeadlineIntervals"],
    ["worker_rpc_timeout_seconds", "workerRpcTimeoutSeconds"],
  ] as const) {
    const value = readPositiveInteger(config[fileKey], fileKey);
    if (value !== undefined) fields[configKey] = value;
  }
  // Outside the positive-integer loop above on purpose: `0` is a valid value here (it disables the
  // idle-retire timer) and the key carries an upper bound no other lifecycle number has.
  const workerIdleRetireSeconds = readIdleRetireSeconds(
    config.worker_idle_retire_seconds,
    "worker_idle_retire_seconds"
  );
  if (workerIdleRetireSeconds !== undefined) {
    fields.workerIdleRetireSeconds = workerIdleRetireSeconds;
  }
  const resyncIntervalSeconds = readPositiveInteger(
    config.resync_interval_seconds,
    "resync_interval_seconds"
  );
  if (resyncIntervalSeconds !== undefined) fields.resyncIntervalMs = resyncIntervalSeconds * 1000;
  const workerStreamPort = readPositiveInteger(config.worker_stream_port, "worker_stream_port");
  if (workerStreamPort !== undefined) {
    if (workerStreamPort > 65535) throw new Error("worker_stream_port must be at most 65535");
    fields.workerStreamPort = workerStreamPort;
  }

  const stateDir = readString(config.state_dir, "state_dir");
  if (stateDir !== undefined) {
    fields.stateDir = path.isAbsolute(stateDir) ? stateDir : path.resolve(configDir, stateDir);
  }
  const gates = parseGates(config.gates, "gates");
  if (gates !== undefined) fields.gates = gates;
  const githubApps = loadGitHubApps(config.github_apps, options.resolveSecrets ?? true);
  if (githubApps !== undefined) fields.githubApps = githubApps;

  return { fields };
}

export function resolveDaemonConfig(
  opts: ResolveDaemonConfigOptions = {}
): ResolveDaemonConfigResult {
  const env = opts.env ?? {};
  const fields = opts.configFile?.fields ?? {};

  const legionId = resolveValue(
    opts.cliOverrides?.legionId,
    fileString(fields, "legionId"),
    env.LEGION_ID,
    undefined
  );
  if (!legionId.value || legionId.value.trim().length === 0) {
    throw new Error("LEGION_ID is required (or set project in legion.yaml)");
  }
  const project = legionId.value.toLowerCase().replace(/[^a-z0-9]/g, "");
  if (!project) throw new Error("LEGION_ID must include at least one alphanumeric character");

  const port = resolveValue(
    opts.cliOverrides?.port,
    fileNumber(fields, "port"),
    parseEnvPositiveInteger(env.LEGION_DAEMON_PORT, "LEGION_DAEMON_PORT"),
    DEFAULT_PORT
  );
  if (!Number.isSafeInteger(port.value) || port.value > 65535) {
    throw new Error("LEGION_DAEMON_PORT must be a valid TCP port");
  }
  const runtime = resolveValue<RuntimeName>(
    opts.cliOverrides?.runtime,
    parseRuntime(fileString(fields, "runtime"), "runtime"),
    parseRuntime(env.LEGION_RUNTIME, "LEGION_RUNTIME"),
    "tmux"
  );
  // `LEGION_DAEMON_URL` is both this env key and the variable every Legion pane carries, so a
  // daemon started from inside a pane inherits the OUTER daemon's URL from its environment and
  // would tell its own processes to register there. Under tmux the only correct value is the
  // daemon's own loopback address, so anything else is refused outright — a file/cli
  // `daemon_url` equal to the default (the smoke rig writes one) is the documented way to pin
  // it. The loopback default needs the resolved port, so it is applied here rather than passed
  // through `resolveValue`.
  const daemonUrl = resolveValue(
    opts.cliOverrides?.daemonUrl,
    fileString(fields, "daemonUrl"),
    env.LEGION_DAEMON_URL,
    undefined
  );
  const loopbackDaemonUrl = `http://127.0.0.1:${port.value}`;
  let resolvedDaemonUrl: string;
  if (daemonUrl.value === undefined) {
    if (runtime.value === "kubernetes") {
      throw new Error(
        "daemon_url is required when runtime is kubernetes (or set LEGION_DAEMON_URL)"
      );
    }
    resolvedDaemonUrl = loopbackDaemonUrl;
  } else {
    const field = daemonUrl.source === "env" ? "LEGION_DAEMON_URL" : "daemon_url";
    resolvedDaemonUrl = normalizeBaseUrl(validateUrl(daemonUrl.value, field), field);
  }
  if (runtime.value === "tmux" && resolvedDaemonUrl !== loopbackDaemonUrl) {
    throw new Error(
      `daemon_url must be ${loopbackDaemonUrl} when runtime is tmux (got ${resolvedDaemonUrl}; an inherited LEGION_DAEMON_URL from an outer Legion pane?)`
    );
  }
  const bind = resolveValue(
    opts.cliOverrides?.bind,
    fileString(fields, "bind"),
    env.LEGION_BIND,
    DEFAULT_BIND
  );
  requireNonEmpty(bind.value, bind.source === "env" ? "LEGION_BIND" : "bind");
  if (runtime.value !== "kubernetes" && bind.value !== DEFAULT_BIND) {
    throw new Error("bind must be 127.0.0.1 unless runtime is kubernetes");
  }
  const envoyUrl = resolveValue(
    opts.cliOverrides?.envoyUrl,
    fileString(fields, "envoyUrl"),
    env.ENVOY_URL,
    DEFAULT_ENVOY_URL
  );
  const dispatchUrl = resolveValue(
    undefined,
    fileString(fields, "dispatchUrl"),
    env.DISPATCH_URL,
    undefined
  );
  const resolvedDispatchUrl =
    dispatchUrl.value === undefined
      ? undefined
      : requireNoMcpSuffix(
          normalizeBaseUrl(validateUrl(dispatchUrl.value, "DISPATCH_URL"), "DISPATCH_URL"),
          "DISPATCH_URL"
        );
  if (env.DISPATCH_MCP_URL !== undefined) {
    throw new Error("DISPATCH_MCP_URL was replaced by DISPATCH_URL");
  }
  // Trimmed, not merely trim-checked: a token with surrounding whitespace (a copy-paste artifact
  // in whatever sets this env var) would otherwise pass this presence check but boot the daemon
  // with a value Dispatch's own auth never matches, since Dispatch compares byte-for-byte.
  const dispatchTokenEnv =
    env.DISPATCH_TOKEN !== undefined && env.DISPATCH_TOKEN.trim().length > 0
      ? env.DISPATCH_TOKEN.trim()
      : undefined;
  if (resolvedDispatchUrl !== undefined && dispatchTokenEnv === undefined) {
    throw new Error(
      "dispatch_url is set but DISPATCH_TOKEN is not; the dispatch tools would not register"
    );
  }
  const dispatchToken = resolvedDispatchUrl === undefined ? undefined : dispatchTokenEnv;
  if (env.LEGION_WORKER_BUDGET !== undefined) {
    throw new Error("worker_budget was replaced by worker_cap");
  }
  if (env.LEGION_BOARD_PROJECT_IDS !== undefined) {
    throw new Error("LEGION_BOARD_PROJECT_IDS was replaced by DISPATCH_PROJECT");
  }
  if (env.LEGION_APP_LOGINS !== undefined) {
    throw new Error(
      "app_logins is not a Legion setting: human approval of a pull request is the repository's own branch protection or CODEOWNERS rule, which Legion never reads or writes"
    );
  }
  const natsUrls = resolveValue(
    opts.cliOverrides?.natsUrls,
    fileStringArray(fields, "natsUrls"),
    parseCsv(env.ENVOY_NATS_URL, "ENVOY_NATS_URL"),
    undefined
  );
  if (!natsUrls.value || natsUrls.value.length === 0) {
    throw new Error("ENVOY_NATS_URL is required (or set nats_urls in legion.yaml)");
  }
  for (const url of natsUrls.value) validateUrl(url, "ENVOY_NATS_URL");

  const ompInvocation = resolveValue(
    opts.cliOverrides?.ompInvocation,
    fileString(fields, "ompInvocation"),
    env.LEGION_OMP_INVOCATION,
    DEFAULT_OMP_INVOCATION
  );
  const ompLaunchPrefix = resolveValue(
    opts.cliOverrides?.ompLaunchPrefix,
    fileStringArray(fields, "ompLaunchPrefix"),
    parseShellWords(env.LEGION_OMP_LAUNCH_PREFIX, "LEGION_OMP_LAUNCH_PREFIX"),
    []
  );

  const repos = resolveValue(
    opts.cliOverrides?.repos,
    fileStringArray(fields, "repos"),
    parseCsv(env.LEGION_REPOS, "LEGION_REPOS"),
    []
  );
  if (repos.value.length === 0) throw new Error("repos is required");
  for (const repo of repos.value) validateRepoSlug(repo, "LEGION_REPOS");
  if (repos.value.length > 1) {
    throw new Error("multiple repos require an issue→repo mapping; not supported");
  }
  const repo = repos.value[0] as `${string}/${string}`;
  const dispatchProject = resolveValue(
    opts.cliOverrides?.dispatchProject,
    fileString(fields, "dispatchProject"),
    env.DISPATCH_PROJECT,
    undefined
  );
  if (!dispatchProject.value) {
    throw new Error("dispatch_project is required");
  }
  const resolvedDispatchProject = validateDispatchProject(
    dispatchProject.value,
    "DISPATCH_PROJECT"
  );
  const admissionCap = resolveValue(
    opts.cliOverrides?.admissionCap,
    fileNumber(fields, "admissionCap"),
    parseEnvPositiveInteger(env.LEGION_ADMISSION_CAP, "LEGION_ADMISSION_CAP"),
    DEFAULT_ADMISSION_CAP
  );
  const workerCap = resolveValue(
    opts.cliOverrides?.workerCap,
    fileNumber(fields, "workerCap"),
    parseEnvPositiveInteger(env.LEGION_WORKER_CAP, "LEGION_WORKER_CAP"),
    DEFAULT_WORKER_CAP
  );
  const maxRecursionDepth = resolveValue(
    opts.cliOverrides?.maxRecursionDepth,
    fileNumber(fields, "maxRecursionDepth"),
    parseEnvPositiveInteger(env.LEGION_MAX_RECURSION_DEPTH, "LEGION_MAX_RECURSION_DEPTH"),
    DEFAULT_MAX_RECURSION_DEPTH
  );
  const lingerHours = resolveValue(
    opts.cliOverrides?.lingerHours,
    fileNumber(fields, "lingerHours"),
    parseEnvPositiveInteger(env.LEGION_LINGER_HOURS, "LEGION_LINGER_HOURS"),
    DEFAULT_LINGER_HOURS
  );
  const maxFixAttempts = resolveValue(
    opts.cliOverrides?.maxFixAttempts,
    fileNumber(fields, "maxFixAttempts"),
    parseEnvPositiveInteger(env.LEGION_MAX_FIX_ATTEMPTS, "LEGION_MAX_FIX_ATTEMPTS"),
    DEFAULT_MAX_FIX_ATTEMPTS
  );
  const resyncIntervalMs = resolveValue(
    opts.cliOverrides?.resyncIntervalMs,
    fileNumber(fields, "resyncIntervalMs"),
    parseEnvPositiveInteger(env.LEGION_RESYNC_INTERVAL_SECONDS, "LEGION_RESYNC_INTERVAL_SECONDS"),
    DEFAULT_RESYNC_INTERVAL_MS
  );
  const workerStopTimeoutSeconds = resolveValue(
    opts.cliOverrides?.workerStopTimeoutSeconds,
    fileNumber(fields, "workerStopTimeoutSeconds"),
    parseEnvPositiveInteger(
      env.LEGION_WORKER_STOP_TIMEOUT_SECONDS,
      "LEGION_WORKER_STOP_TIMEOUT_SECONDS"
    ),
    DEFAULT_WORKER_STOP_TIMEOUT_SECONDS
  );
  const treeStopTimeoutSeconds = resolveValue(
    opts.cliOverrides?.treeStopTimeoutSeconds,
    fileNumber(fields, "treeStopTimeoutSeconds"),
    parseEnvPositiveInteger(
      env.LEGION_TREE_STOP_TIMEOUT_SECONDS,
      "LEGION_TREE_STOP_TIMEOUT_SECONDS"
    ),
    DEFAULT_TREE_STOP_TIMEOUT_SECONDS
  );
  const workerBootTimeoutSeconds = resolveValue(
    opts.cliOverrides?.workerBootTimeoutSeconds,
    fileNumber(fields, "workerBootTimeoutSeconds"),
    parseEnvPositiveInteger(
      env.LEGION_WORKER_BOOT_TIMEOUT_SECONDS,
      "LEGION_WORKER_BOOT_TIMEOUT_SECONDS"
    ),
    DEFAULT_WORKER_BOOT_TIMEOUT_SECONDS
  );
  const workerBootRegistrationDeadlineIntervals = resolveValue(
    opts.cliOverrides?.workerBootRegistrationDeadlineIntervals,
    fileNumber(fields, "workerBootRegistrationDeadlineIntervals"),
    parseEnvPositiveInteger(
      env.LEGION_WORKER_BOOT_REGISTRATION_DEADLINE_INTERVALS,
      "LEGION_WORKER_BOOT_REGISTRATION_DEADLINE_INTERVALS"
    ),
    DEFAULT_WORKER_BOOT_REGISTRATION_DEADLINE_INTERVALS
  );
  const workerRpcTimeoutSeconds = resolveValue(
    opts.cliOverrides?.workerRpcTimeoutSeconds,
    fileNumber(fields, "workerRpcTimeoutSeconds"),
    parseEnvPositiveInteger(
      env.LEGION_WORKER_RPC_TIMEOUT_SECONDS,
      "LEGION_WORKER_RPC_TIMEOUT_SECONDS"
    ),
    DEFAULT_WORKER_RPC_TIMEOUT_SECONDS
  );
  const workerIdleRetireSeconds = resolveValue(
    opts.cliOverrides?.workerIdleRetireSeconds,
    fileNumber(fields, "workerIdleRetireSeconds"),
    parseEnvIdleRetireSeconds(
      env.LEGION_WORKER_IDLE_RETIRE_SECONDS,
      "LEGION_WORKER_IDLE_RETIRE_SECONDS"
    ),
    DEFAULT_WORKER_IDLE_RETIRE_SECONDS
  );
  const workerStreamPort = resolveValue(
    opts.cliOverrides?.workerStreamPort,
    fileNumber(fields, "workerStreamPort"),
    parseEnvPositiveInteger(env.LEGION_WORKER_STREAM_PORT, "LEGION_WORKER_STREAM_PORT"),
    port.value + 1
  );
  if (!Number.isSafeInteger(workerStreamPort.value) || workerStreamPort.value > 65535) {
    if (workerStreamPort.source === "default") {
      throw new Error(
        `worker_stream_port defaults to port + 1 (${workerStreamPort.value}), which is not a valid TCP port; set worker_stream_port`
      );
    }
    const settingBySource: Record<Exclude<ValueSource, "default">, string> = {
      cli: "workerStreamPort override",
      config: "worker_stream_port",
      env: "LEGION_WORKER_STREAM_PORT",
    };
    throw new Error(`${settingBySource[workerStreamPort.source]} must be a valid TCP port`);
  }
  if (workerStreamPort.value === port.value) {
    throw new Error(`worker_stream_port must differ from port (both ${port.value})`);
  }

  const lifecycleNumbers: Record<string, number> = {
    admissionCap: admissionCap.value,
    workerCap: workerCap.value,
    maxRecursionDepth: maxRecursionDepth.value,
    lingerHours: lingerHours.value,
    maxFixAttempts: maxFixAttempts.value,
    resyncIntervalMs: resyncIntervalMs.value,
    workerStopTimeoutSeconds: workerStopTimeoutSeconds.value,
    treeStopTimeoutSeconds: treeStopTimeoutSeconds.value,
    workerBootTimeoutSeconds: workerBootTimeoutSeconds.value,
    workerBootRegistrationDeadlineIntervals: workerBootRegistrationDeadlineIntervals.value,
    workerRpcTimeoutSeconds: workerRpcTimeoutSeconds.value,
  };
  for (const [field, value] of Object.entries(lifecycleNumbers)) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw new Error(`${field} must be a positive integer`);
    }
  }
  // `workerIdleRetireSeconds` is the one lifecycle number that admits 0 (timer disabled) and carries
  // an upper bound, so it is validated here rather than in the positive-integer loop above (this
  // also covers a cliOverride, which the file and env parsers never see).
  checkIdleRetireSeconds(workerIdleRetireSeconds.value, "workerIdleRetireSeconds");

  const gates = resolveValue(opts.cliOverrides?.gates, fileGates(fields), undefined, {
    design: "root-issues",
  } as const);
  const parsedGates = parseGates(gates.value, "gates");
  if (!parsedGates) throw new Error("gates must be configured");
  const githubApps = resolveValue(
    opts.cliOverrides?.githubApps,
    fileGitHubApps(fields),
    undefined,
    {}
  );
  const stateDir = resolveValue(
    opts.cliOverrides?.stateDir,
    fileString(fields, "stateDir"),
    env.LEGION_STATE_DIR,
    path.join(os.homedir(), ".legion", project)
  );

  return {
    config: {
      project,
      legionId: legionId.value,
      port: port.value,
      runtime: runtime.value,
      daemonUrl: resolvedDaemonUrl,
      bind: bind.value,
      envoyUrl: validateUrl(envoyUrl.value, "ENVOY_URL"),
      dispatchUrl: resolvedDispatchUrl,
      dispatchToken,
      dispatchProject: resolvedDispatchProject,
      natsUrls: natsUrls.value,
      ompInvocation: requireNonEmpty(ompInvocation.value, "LEGION_OMP_INVOCATION"),
      ompLaunchPrefix: ompLaunchPrefix.value,
      repos: repos.value,
      repo,
      admissionCap: admissionCap.value,
      workerCap: workerCap.value,
      maxRecursionDepth: maxRecursionDepth.value,
      lingerHours: lingerHours.value,
      maxFixAttempts: maxFixAttempts.value,
      resyncIntervalMs: resyncIntervalMs.value * (resyncIntervalMs.source === "env" ? 1000 : 1),
      workerStopTimeoutSeconds: workerStopTimeoutSeconds.value,
      treeStopTimeoutSeconds: treeStopTimeoutSeconds.value,
      workerBootTimeoutSeconds: workerBootTimeoutSeconds.value,
      workerBootRegistrationDeadlineIntervals: workerBootRegistrationDeadlineIntervals.value,
      workerRpcTimeoutSeconds: workerRpcTimeoutSeconds.value,
      workerIdleRetireSeconds: workerIdleRetireSeconds.value,
      workerStreamPort: workerStreamPort.value,
      gates: parsedGates,
      githubApps: githubApps.value,
      stateDir: stateDir.value,
    },
  };
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): DaemonConfig {
  return resolveDaemonConfig({ env }).config;
}
