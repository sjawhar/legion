import { spawnSync } from "node:child_process";
import os from "node:os";
import path from "node:path";
import { parse } from "yaml";
import { z } from "zod";
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
   * finished worker then stays resident until its tree closes); at most `MAX_TIMER_SECONDS`. */
  workerIdleRetireSeconds: number;
  /** Seconds before a single worker RPC request over a `legion worker-shim` unix socket
   * (`negotiate_protocol`/`get_state`/`prompt`) times out. Governs the connect-time
   * `negotiate_protocol` round trip that `markTreeReady`/`workerReady`/`markControllerReady`
   * kick off in the background after `/process/ready`/`/worker/ready`/`/controller/ready`
   * already responded — small by default, raised only under measured load sensitivity, never a
   * substitute for those routes responding before they dial back into the caller's own socket. */
  workerRpcTimeoutSeconds: number;
  /** Per-attempt budget, in seconds, for every command that waits on OMP start-up, the
   * network, or a credential helper: the two boot probes and every workspace-provisioning
   * command (`jj git clone`/`fetch`, `jj workspace add`, the git config writes). The command
   * runner's generic 30 s stays for GitHub API reads.
   * `slow_command_timeout_seconds` / `LEGION_SLOW_COMMAND_TIMEOUT_SECONDS`; default 300. */
  slowCommandTimeoutSeconds: number;
  /** TCP port the worker stream listener (`worker-stream-listener.ts`) accepts reverse-dialed
   * `legion worker-shim --connect` streams on, bound to the same address as the API.
   * `worker_stream_port` / `LEGION_WORKER_STREAM_PORT`; default `port + 1`. */
  workerStreamPort: number;
  gates: { design: "root-issues" | "off" };
  githubApps: GitHubAppsConfig;
  stateDir: string;
  /** Optional operator markdown appended to every launched pane's system prompt (root architect,
   * sub-architects, phase workers, controller) as the last part of its `--append-system-prompt` —
   * the deployment's standing rules for this repository. The `instructions` file key is resolved
   * against the config file's directory when relative; `LEGION_INSTRUCTIONS` is used as given.
   * Read once at boot (`index.ts`): a missing, unreadable, or blank file refuses startup. */
  instructionsPath?: string;
}

export interface LoadedConfigFile {
  fields: Record<string, unknown>;
}

export interface LoadConfigFileOptions {
  /**
   * When false, github_apps.<role>.private_key_command is validated for presence but never
   * executed, and github_apps.<role>.private_key_secret is validated as a key name but `secrets`
   * is never run — the private key becomes the placeholder "(not executed)". Used by
   * `legion start --check-config` so a config validation pass never runs an arbitrary shell
   * command from the file and never requests a secretsd grant (a YubiKey tap). Defaults to true
   * (the daemon always resolves real secrets).
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
/** The largest whole number of seconds whose millisecond delay fits the signed 32-bit delay
 * `setTimeout` accepts (`seconds * 1000 <= 2_147_483_647`). Beyond it Bun clamps the delay to 1 ms
 * and the timer fires at once — a boot watchdog that retires every booting worker, a resync that
 * spins, an idle-retire that fires the instant a worker goes idle — so every duration setting that
 * reaches a timer is refused above this bound rather than clamped. */
const MAX_TIMER_SECONDS = 2_147_483;
/** `MAX_TIMER_SECONDS` in whole hours (596), the bound for `linger_hours`. */
const MAX_TIMER_HOURS = Math.floor(MAX_TIMER_SECONDS / 3600);
/** Also the per-attempt budget `legion probe-image` uses (`IMAGE_PROBE_TIMEOUT_MS`). */
export const DEFAULT_SLOW_COMMAND_TIMEOUT_SECONDS = 300;

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
  slow_command_timeout_seconds: null,
  worker_stream_port: null,
  state_dir: null,
  instructions: null,
  // `merge` is recognized (not an "unknown key") so setting it surfaces the specific
  // removed-setting error `parseGates` throws below instead of the generic "Unknown config key"
  // message. Never mapped to a field.
  gates: { design: null, merge: null },
  github_apps: {
    implement: {
      app_id: null,
      private_key: null,
      private_key_command: null,
      private_key_secret: null,
      installations: { [CONFIG_ANY_KEY]: null },
    },
    review: {
      app_id: null,
      private_key: null,
      private_key_command: null,
      private_key_secret: null,
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

/** The one place an upper bound is refused: `<field> must be at most <max>`, with an optional
 * `; <hint>` naming the escape hatch (e.g. the disable value). */
function checkAtMost(number: number, field: string, max: number, hint?: string): number {
  if (number > max) {
    throw new Error(`${field} must be at most ${max}${hint === undefined ? "" : `; ${hint}`}`);
  }
  return number;
}

function readPositiveInteger(value: unknown, field: string, max?: number): number | undefined {
  const number = readNumber(value, field);
  if (number === undefined) return undefined;
  if (!Number.isSafeInteger(number) || number <= 0) {
    throw new Error(`${field} must be a positive integer`);
  }
  return max === undefined ? number : checkAtMost(number, field, max);
}

function parseEnvPositiveInteger(
  value: string | undefined,
  field: string,
  max?: number
): number | undefined {
  if (value === undefined || value === "") return undefined;
  const number = Number(value);
  if (!Number.isSafeInteger(number) || number <= 0) {
    throw new Error(`${field} must be a positive integer`);
  }
  return max === undefined ? number : checkAtMost(number, field, max);
}

/** The one validation every source of `worker_idle_retire_seconds` (file, environment, cli
 * override) funnels through: a whole number from `0` (the timer disabled) to `MAX_TIMER_SECONDS`
 * inclusive. `-0` is rejected explicitly — the YAML loader hands it through as a negative zero, and
 * `-0 < 0` is false — because "disabled" must be the literal `0`, never a value that merely
 * computes to zero. */
function checkIdleRetireSeconds(number: number, field: string): number {
  if (!Number.isInteger(number) || number < 0 || Object.is(number, -0)) {
    throw new Error(`${field} must be a non-negative integer`);
  }
  return checkAtMost(number, field, MAX_TIMER_SECONDS, "use 0 to disable idle retirement");
}

/** As `readPositiveInteger`, but for `worker_idle_retire_seconds`, the one lifecycle number where
 * zero is a meaningful setting ("never retire an idle worker") rather than the typo it would be for
 * a cap or a timeout. */
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

/** A `private_key_secret` value is one secretsd key name, handed to `secrets` as a single argv
 * token — never through a shell. Whitespace means the operator pasted a command, not a name. */
function readSecretName(value: unknown, field: string): string | undefined {
  const name = readString(value, field);
  // `""` is "not provided", exactly as `private_key: ""` and `private_key_command: ""` are; it
  // falls through to the exactly-one rule in loadGitHubApps rather than a shape error.
  if (name === undefined || name === "") return name;
  if (/\s/.test(name)) {
    throw new Error(`${field} must be a single secretsd key name (no whitespace)`);
  }
  return name;
}

/** Runs with the daemon's own environment on purpose: this child exists to read the App key the
 * operator's launcher supplies (`GH_*_APP_PRIVATE_KEY_B64`); it is never a pane. Panes and every
 * other child get the allow-listed `paneEnv` (`environment.ts`). */
function executePrivateKeyCommand(command: string, field: string): string {
  const result = spawnSync("sh", ["-c", command], {
    encoding: "utf8",
    env: process.env,
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

const SecretsStatusSchema = z.object({ key: z.string(), tier: z.string() }).passthrough();

/** Runs one `secrets get …` for `name`. Like `executePrivateKeyCommand` this is the daemon acting
 * for itself, never a pane, so it runs under the daemon's own environment. Two deliberate
 * differences from that `sh -c` child: the child inherits the daemon's stdin — secretsd identifies
 * a tokenless caller by `isatty(0)` plus `/proc/self/fd/0`, so a piped stdin would be refused as
 * "neither a terminal tty nor a session token" — and `SECRETSD_SESSION_TOKEN_FILE` is dropped
 * (only that one: `SECRETSD_SOCK` is the broker's socket path, not a session), so the request is
 * always scoped to the launcher pane's terminal and the App key's grant never lands on an agent
 * session the daemon happened to be started from. No daemon-imposed timeout: secretsd's own
 * approval window is the failure, reported with the child's stderr. */
function runSecretsGet(name: string, flag: "--no-request" | "--value", field: string): string {
  const { SECRETSD_SESSION_TOKEN_FILE: _session, ...env } = process.env;
  const args = ["get", name, flag];
  const result = spawnSync("secrets", args, {
    encoding: "utf8",
    env,
    stdio: ["inherit", "pipe", "pipe"],
  });
  if (result.error) {
    if ((result.error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new Error(`${field}: the secrets command is not on PATH, so ${name} cannot be read`);
    }
    throw new Error(`${field}: could not run secrets ${args.join(" ")}: ${result.error.message}`);
  }
  if (result.status !== 0) {
    const status = result.status === null ? "unknown" : String(result.status);
    const stderr = result.stderr?.trim();
    throw new Error(
      `${field}: secrets ${args.join(" ")} failed (exit ${status})${stderr ? `: ${stderr}` : ""}`
    );
  }
  return result.stdout ?? "";
}

/** `github_apps.<role>.private_key_secret`: the App's PEM, base64-encoded, held by secretsd under
 * `name`. The tier check comes first and never costs a tap (`--no-request`); only a `human`-tier
 * key is fetched, because an agent-tier key is readable by every Legion pane and the whole point
 * of this source is that panes cannot read it (LEGION-77). The decoded value must be a PEM so a
 * wrong key name that happens to exist fails here, not at the first JWT. Nothing in any error
 * carries the value. */
function resolvePrivateKeySecret(name: string, field: string): string {
  const statusText = runSecretsGet(name, "--no-request", field);
  const unparsable = new Error(
    `${field}: secrets get ${name} --no-request printed an unparsable status (expected {"key","tier"})`
  );
  let status: z.infer<typeof SecretsStatusSchema>;
  try {
    status = SecretsStatusSchema.parse(JSON.parse(statusText.trim()));
  } catch {
    throw unparsable;
  }
  // A status for some other key is not a status for this one; one message, no new wording.
  if (status.key !== name) throw unparsable;
  if (status.tier !== "human") {
    throw new Error(
      `App private key ${name} is readable by agent-tier callers; move it to a daemon-only store`
    );
  }
  console.warn(
    `[legion] requesting ${name} from secretsd (human tier; a YubiKey tap may be needed)`
  );
  const encoded = runSecretsGet(name, "--value", field).trim();
  const decoded = Buffer.from(encoded, "base64").toString("utf8").trim();
  if (!decoded.startsWith("-----BEGIN")) {
    throw new Error(
      `${field}: ${name} did not decode to a PEM private key (expected base64 of a -----BEGIN block)`
    );
  }
  return decoded;
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
    const secretField = `github_apps.${role}.private_key_secret`;
    const secretName = readSecretName(parsedRole.data.private_key_secret, secretField);
    const hasInlineKey = inlineKey !== undefined && inlineKey !== "";
    const hasCommand = command !== undefined && command !== "";
    const hasSecret = secretName !== undefined && secretName !== "";
    if (appId === undefined || appId === "") {
      throw new Error(`github_apps.${role} is missing required fields: app_id`);
    }
    if ([hasInlineKey, hasCommand, hasSecret].filter(Boolean).length !== 1) {
      throw new Error(
        `github_apps.${role} requires exactly one of private_key, private_key_command, or private_key_secret`
      );
    }
    // The chain repeats the has* conditions on the values so tsc narrows `string | undefined`.
    let privateKey: string;
    if (inlineKey !== undefined && inlineKey !== "") {
      privateKey = inlineKey;
    } else if (command !== undefined && command !== "") {
      privateKey = resolveSecrets
        ? executePrivateKeyCommand(command, `github_apps.${role}.private_key_command`)
        : "(not executed)";
    } else if (secretName !== undefined && secretName !== "") {
      privateKey = resolveSecrets
        ? resolvePrivateKeySecret(secretName, secretField)
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
  const port = readPositiveInteger(config.port, "port", 65535);
  if (port !== undefined) fields.port = port;
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

  // Counts have no upper bound; every duration that reaches a timer is bounded at
  // `MAX_TIMER_SECONDS` (`linger_hours`, a deadline swept by the linger interval, at its hour form).
  const lifecycleKeys: ReadonlyArray<readonly [string, string, number?]> = [
    ["admission_cap", "admissionCap"],
    ["worker_cap", "workerCap"],
    ["max_recursion_depth", "maxRecursionDepth"],
    ["linger_hours", "lingerHours", MAX_TIMER_HOURS],
    ["max_fix_attempts", "maxFixAttempts"],
    ["worker_stop_timeout_seconds", "workerStopTimeoutSeconds", MAX_TIMER_SECONDS],
    ["tree_stop_timeout_seconds", "treeStopTimeoutSeconds", MAX_TIMER_SECONDS],
    ["worker_boot_timeout_seconds", "workerBootTimeoutSeconds", MAX_TIMER_SECONDS],
    ["worker_boot_registration_deadline_intervals", "workerBootRegistrationDeadlineIntervals"],
    ["worker_rpc_timeout_seconds", "workerRpcTimeoutSeconds", MAX_TIMER_SECONDS],
    ["slow_command_timeout_seconds", "slowCommandTimeoutSeconds", MAX_TIMER_SECONDS],
  ];
  for (const [fileKey, configKey, max] of lifecycleKeys) {
    const value = readPositiveInteger(config[fileKey], fileKey, max);
    if (value !== undefined) fields[configKey] = value;
  }
  // Outside the positive-integer loop above on purpose: `0` is a valid value here (it disables the
  // idle-retire timer).
  const workerIdleRetireSeconds = readIdleRetireSeconds(
    config.worker_idle_retire_seconds,
    "worker_idle_retire_seconds"
  );
  if (workerIdleRetireSeconds !== undefined) {
    fields.workerIdleRetireSeconds = workerIdleRetireSeconds;
  }
  const resyncIntervalSeconds = readPositiveInteger(
    config.resync_interval_seconds,
    "resync_interval_seconds",
    MAX_TIMER_SECONDS
  );
  if (resyncIntervalSeconds !== undefined) fields.resyncIntervalMs = resyncIntervalSeconds * 1000;
  const workerStreamPort = readPositiveInteger(
    config.worker_stream_port,
    "worker_stream_port",
    65535
  );
  if (workerStreamPort !== undefined) fields.workerStreamPort = workerStreamPort;

  const stateDir = readString(config.state_dir, "state_dir");
  if (stateDir !== undefined) {
    fields.stateDir = path.isAbsolute(stateDir) ? stateDir : path.resolve(configDir, stateDir);
  }
  const instructions = readString(config.instructions, "instructions");
  if (instructions !== undefined) {
    const instructionsPath = requireNonEmpty(instructions, "instructions");
    fields.instructionsPath = path.isAbsolute(instructionsPath)
      ? instructionsPath
      : path.resolve(configDir, instructionsPath);
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
    parseEnvPositiveInteger(env.LEGION_LINGER_HOURS, "LEGION_LINGER_HOURS", MAX_TIMER_HOURS),
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
    parseEnvPositiveInteger(
      env.LEGION_RESYNC_INTERVAL_SECONDS,
      "LEGION_RESYNC_INTERVAL_SECONDS",
      MAX_TIMER_SECONDS
    ),
    DEFAULT_RESYNC_INTERVAL_MS
  );
  const workerStopTimeoutSeconds = resolveValue(
    opts.cliOverrides?.workerStopTimeoutSeconds,
    fileNumber(fields, "workerStopTimeoutSeconds"),
    parseEnvPositiveInteger(
      env.LEGION_WORKER_STOP_TIMEOUT_SECONDS,
      "LEGION_WORKER_STOP_TIMEOUT_SECONDS",
      MAX_TIMER_SECONDS
    ),
    DEFAULT_WORKER_STOP_TIMEOUT_SECONDS
  );
  const treeStopTimeoutSeconds = resolveValue(
    opts.cliOverrides?.treeStopTimeoutSeconds,
    fileNumber(fields, "treeStopTimeoutSeconds"),
    parseEnvPositiveInteger(
      env.LEGION_TREE_STOP_TIMEOUT_SECONDS,
      "LEGION_TREE_STOP_TIMEOUT_SECONDS",
      MAX_TIMER_SECONDS
    ),
    DEFAULT_TREE_STOP_TIMEOUT_SECONDS
  );
  const workerBootTimeoutSeconds = resolveValue(
    opts.cliOverrides?.workerBootTimeoutSeconds,
    fileNumber(fields, "workerBootTimeoutSeconds"),
    parseEnvPositiveInteger(
      env.LEGION_WORKER_BOOT_TIMEOUT_SECONDS,
      "LEGION_WORKER_BOOT_TIMEOUT_SECONDS",
      MAX_TIMER_SECONDS
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
      "LEGION_WORKER_RPC_TIMEOUT_SECONDS",
      MAX_TIMER_SECONDS
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
  const slowCommandTimeoutSeconds = resolveValue(
    opts.cliOverrides?.slowCommandTimeoutSeconds,
    fileNumber(fields, "slowCommandTimeoutSeconds"),
    parseEnvPositiveInteger(
      env.LEGION_SLOW_COMMAND_TIMEOUT_SECONDS,
      "LEGION_SLOW_COMMAND_TIMEOUT_SECONDS",
      MAX_TIMER_SECONDS
    ),
    DEFAULT_SLOW_COMMAND_TIMEOUT_SECONDS
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

  // A file value arrives here already in milliseconds (`loadConfigFromFile`), a cliOverride is
  // milliseconds by contract, and only the env value is still seconds; the post-resolve check
  // below and the returned config both judge the millisecond form.
  const resyncIntervalMsValue =
    resyncIntervalMs.value * (resyncIntervalMs.source === "env" ? 1000 : 1);
  // Every source ends here, and for a cliOverride this is the only guard: a positive integer, and
  // for each duration that reaches a timer, at most `MAX_TIMER_SECONDS` in the field's own unit.
  const lifecycleNumbers: Record<string, { value: number; max?: number }> = {
    admissionCap: { value: admissionCap.value },
    workerCap: { value: workerCap.value },
    maxRecursionDepth: { value: maxRecursionDepth.value },
    lingerHours: { value: lingerHours.value, max: MAX_TIMER_HOURS },
    maxFixAttempts: { value: maxFixAttempts.value },
    resyncIntervalMs: { value: resyncIntervalMsValue, max: MAX_TIMER_SECONDS * 1000 },
    workerStopTimeoutSeconds: { value: workerStopTimeoutSeconds.value, max: MAX_TIMER_SECONDS },
    treeStopTimeoutSeconds: { value: treeStopTimeoutSeconds.value, max: MAX_TIMER_SECONDS },
    workerBootTimeoutSeconds: { value: workerBootTimeoutSeconds.value, max: MAX_TIMER_SECONDS },
    workerBootRegistrationDeadlineIntervals: {
      value: workerBootRegistrationDeadlineIntervals.value,
    },
    workerRpcTimeoutSeconds: { value: workerRpcTimeoutSeconds.value, max: MAX_TIMER_SECONDS },
    slowCommandTimeoutSeconds: { value: slowCommandTimeoutSeconds.value, max: MAX_TIMER_SECONDS },
  };
  for (const [field, { value, max }] of Object.entries(lifecycleNumbers)) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw new Error(`${field} must be a positive integer`);
    }
    if (max !== undefined) checkAtMost(value, field, max);
  }
  // `workerIdleRetireSeconds` is the one lifecycle number that admits 0 (timer disabled), so it is
  // validated here rather than in the positive-integer loop above.
  checkIdleRetireSeconds(workerIdleRetireSeconds.value, "workerIdleRetireSeconds");
  // The root and controller registration deadlines (`processes.ts`) are one timer for the product
  // of these two, so each factor fitting on its own is not enough.
  checkAtMost(
    workerBootTimeoutSeconds.value * workerBootRegistrationDeadlineIntervals.value,
    "worker_boot_timeout_seconds * worker_boot_registration_deadline_intervals",
    MAX_TIMER_SECONDS
  );

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
  const instructionsPath = resolveValue(
    opts.cliOverrides?.instructionsPath,
    fileString(fields, "instructionsPath"),
    env.LEGION_INSTRUCTIONS,
    undefined
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
      resyncIntervalMs: resyncIntervalMsValue,
      workerStopTimeoutSeconds: workerStopTimeoutSeconds.value,
      treeStopTimeoutSeconds: treeStopTimeoutSeconds.value,
      workerBootTimeoutSeconds: workerBootTimeoutSeconds.value,
      workerBootRegistrationDeadlineIntervals: workerBootRegistrationDeadlineIntervals.value,
      workerRpcTimeoutSeconds: workerRpcTimeoutSeconds.value,
      workerIdleRetireSeconds: workerIdleRetireSeconds.value,
      slowCommandTimeoutSeconds: slowCommandTimeoutSeconds.value,
      workerStreamPort: workerStreamPort.value,
      gates: parsedGates,
      githubApps: githubApps.value,
      stateDir: stateDir.value,
      instructionsPath:
        instructionsPath.value === undefined
          ? undefined
          : requireNonEmpty(instructionsPath.value, "LEGION_INSTRUCTIONS"),
    },
  };
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): DaemonConfig {
  return resolveDaemonConfig({ env }).config;
}
