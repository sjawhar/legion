import { spawnSync } from "node:child_process";
import os from "node:os";
import path from "node:path";
import { parse } from "yaml";
import { z } from "zod";

export type GitHubAppRole = "implement" | "review";

export interface GitHubAppRoleConfig {
  appId: string;
  privateKey: string;
  installations?: Record<string, string>;
}

export type GitHubAppsConfig = Partial<Record<GitHubAppRole, GitHubAppRoleConfig>>;

export interface DaemonConfig {
  project: string;
  legionId: string;
  port: number;
  envoyUrl: string;
  natsUrls: string[];
  dispatchUrl: string;
  dispatchBearer: string;
  ompInvocation: string;
  boardProjectIds: string[];
  appLogins: string[];
  admissionCap: number;
  workerBudget: number;
  maxRecursionDepth: number;
  lingerHours: number;
  ciQuietMs: number;
  maxFixAttempts: number;
  resyncIntervalMs: number;
  gates: { design: "root-issues" | "off"; merge: "human" | "off" };
  githubApps: GitHubAppsConfig;
  stateDir: string;
}

export interface LoadedConfigFile {
  fields: Record<string, unknown>;
  warnings: string[];
}

export interface ResolveDaemonConfigOptions {
  env?: Record<string, string | undefined>;
  configFile?: LoadedConfigFile;
  cliOverrides?: Partial<DaemonConfig>;
}

export interface ResolveDaemonConfigResult {
  config: DaemonConfig;
  warnings: string[];
}

const CONFIG_ANY_KEY = Symbol("config-any-key");

type ConfigSchema = {
  [key: string]: ConfigSchema | null;
  [CONFIG_ANY_KEY]?: ConfigSchema | null;
};
type ValueSource = "cli" | "config" | "env" | "default";

const DEFAULT_PORT = 13370;
const DEFAULT_ENVOY_URL = "http://127.0.0.1:9020";
const DEFAULT_ADMISSION_CAP = 4;
const DEFAULT_WORKER_BUDGET = 6;
const DEFAULT_MAX_RECURSION_DEPTH = 8;
const DEFAULT_LINGER_HOURS = 72;
const DEFAULT_CI_QUIET_MS = 30_000;
const DEFAULT_MAX_FIX_ATTEMPTS = 3;
const DEFAULT_RESYNC_INTERVAL_MS = 600_000;
const DEFAULT_OMP_INVOCATION = "mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp";

const CONFIG_SCHEMA: ConfigSchema = {
  project: null,
  port: null,
  envoy_url: null,
  nats_urls: null,
  dispatch_url: null,
  omp_invocation: null,
  board_project_ids: null,
  app_logins: null,
  admission_cap: null,
  worker_budget: null,
  max_recursion_depth: null,
  linger_hours: null,
  ci_quiet_ms: null,
  max_fix_attempts: null,
  resync_interval_seconds: null,
  state_dir: null,
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

function requireNonEmpty(value: string, field: string): string {
  if (value.trim().length === 0) throw new Error(`${field} must not be empty`);
  return value;
}

function validateUrl(value: string, field: string): string {
  try {
    new URL(value);
  } catch {
    throw new Error(`${field} must be a valid URL`);
  }
  return value;
}

function collectUnknownKeys(
  value: unknown,
  schema: ConfigSchema | null,
  pathParts: string[],
  warnings: string[]
): void {
  const parsed = UnknownRecordSchema.safeParse(value);
  if (!schema || !parsed.success) return;
  for (const [key, child] of Object.entries(parsed.data)) {
    const childSchema = Object.hasOwn(schema, key) ? schema[key] : schema[CONFIG_ANY_KEY];
    if (childSchema === undefined) {
      warnings.push(`Unknown config key: ${[...pathParts, key].join(".")}`);
      continue;
    }
    collectUnknownKeys(child, childSchema, [...pathParts, key], warnings);
  }
}

function readStringRecord(value: unknown, field: string): Record<string, string> | undefined {
  if (value === undefined || value === null) return undefined;
  const parsed = z.record(z.string()).safeParse(value);
  if (!parsed.success) throw new Error(`${field} must be a mapping of strings`);
  return parsed.data;
}

function executePrivateKeyCommand(command: string, field: string): string {
  const result = spawnSync("sh", ["-c", command], { encoding: "utf8" });
  if (result.error || result.status !== 0) {
    const status = result.status === null ? "unknown" : String(result.status);
    const stderr = result.stderr?.trim();
    throw new Error(`${field} failed (exit ${status})${stderr ? `: ${stderr}` : ""}`);
  }
  const privateKey = result.stdout?.trim() ?? "";
  if (!privateKey) throw new Error(`${field} produced empty output`);
  return privateKey;
}

function loadGitHubApps(value: unknown): GitHubAppsConfig | undefined {
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
      privateKey = executePrivateKeyCommand(command, `github_apps.${role}.private_key_command`);
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

function parseGates(value: unknown, field: string): DaemonConfig["gates"] | undefined {
  const parsed = UnknownRecordSchema.safeParse(value);
  if (!parsed.success) throw new Error(`${field} must be a mapping`);
  const design = readString(parsed.data.design, `${field}.design`) ?? "root-issues";
  const merge = readString(parsed.data.merge, `${field}.merge`) ?? "human";
  if (design !== "root-issues" && design !== "off") {
    throw new Error(`${field}.design must be 'root-issues' or 'off'`);
  }
  if (merge !== "human" && merge !== "off") {
    throw new Error(`${field}.merge must be 'human' or 'off'`);
  }
  return { design, merge };
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

export function loadConfigFromFile(yamlText: string, configDir: string): LoadedConfigFile {
  let parsed: unknown;
  try {
    parsed = parse(yamlText);
  } catch (error) {
    throw new Error(
      `Invalid YAML config: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  if (parsed === undefined || parsed === null) return { fields: {}, warnings: [] };
  const parsedRoot = UnknownRecordSchema.safeParse(parsed);
  if (!parsedRoot.success) throw new Error("Config file root must be a mapping");
  const config = parsedRoot.data;

  const warnings: string[] = [];
  collectUnknownKeys(config, CONFIG_SCHEMA, [], warnings);
  const fields: Record<string, unknown> = {};

  const project = readString(config.project, "project");
  if (project !== undefined) fields.legionId = requireNonEmpty(project, "project");
  const port = readPositiveInteger(config.port, "port");
  if (port !== undefined) {
    if (port > 65535) throw new Error("port must be at most 65535");
    fields.port = port;
  }
  const envoyUrl = readString(config.envoy_url, "envoy_url");
  if (envoyUrl !== undefined) fields.envoyUrl = validateUrl(envoyUrl, "envoy_url");
  const natsUrls = readStringArray(config.nats_urls, "nats_urls");
  if (natsUrls !== undefined) fields.natsUrls = natsUrls;
  const dispatchUrl = readString(config.dispatch_url, "dispatch_url");
  if (dispatchUrl !== undefined) fields.dispatchUrl = validateUrl(dispatchUrl, "dispatch_url");
  const ompInvocation = readString(config.omp_invocation, "omp_invocation");
  if (ompInvocation !== undefined) {
    fields.ompInvocation = requireNonEmpty(ompInvocation, "omp_invocation");
  }
  const boardProjectIds = readStringArray(config.board_project_ids, "board_project_ids");
  if (boardProjectIds !== undefined) fields.boardProjectIds = boardProjectIds;
  const appLogins = readStringArray(config.app_logins, "app_logins");
  if (appLogins !== undefined) fields.appLogins = appLogins;

  for (const [fileKey, configKey] of [
    ["admission_cap", "admissionCap"],
    ["worker_budget", "workerBudget"],
    ["max_recursion_depth", "maxRecursionDepth"],
    ["linger_hours", "lingerHours"],
    ["ci_quiet_ms", "ciQuietMs"],
    ["max_fix_attempts", "maxFixAttempts"],
  ] as const) {
    const value = readPositiveInteger(config[fileKey], fileKey);
    if (value !== undefined) fields[configKey] = value;
  }
  const resyncIntervalSeconds = readPositiveInteger(
    config.resync_interval_seconds,
    "resync_interval_seconds"
  );
  if (resyncIntervalSeconds !== undefined) fields.resyncIntervalMs = resyncIntervalSeconds * 1000;

  const stateDir = readString(config.state_dir, "state_dir");
  if (stateDir !== undefined) {
    fields.stateDir = path.isAbsolute(stateDir) ? stateDir : path.resolve(configDir, stateDir);
  }
  const gates = parseGates(config.gates, "gates");
  if (gates !== undefined) fields.gates = gates;
  const githubApps = loadGitHubApps(config.github_apps);
  if (githubApps !== undefined) fields.githubApps = githubApps;

  return { fields, warnings };
}

export function resolveDaemonConfig(
  opts: ResolveDaemonConfigOptions = {}
): ResolveDaemonConfigResult {
  const env = opts.env ?? {};
  const fields = opts.configFile?.fields ?? {};
  const warnings = [...(opts.configFile?.warnings ?? [])];

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
  const envoyUrl = resolveValue(
    opts.cliOverrides?.envoyUrl,
    fileString(fields, "envoyUrl"),
    env.ENVOY_URL,
    DEFAULT_ENVOY_URL
  );
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

  const dispatchUrl = resolveValue(
    opts.cliOverrides?.dispatchUrl,
    fileString(fields, "dispatchUrl"),
    env.LEGION_DISPATCH_URL,
    undefined
  );
  if (!dispatchUrl.value || dispatchUrl.value.trim().length === 0) {
    throw new Error("LEGION_DISPATCH_URL is required (or set dispatch_url in legion.yaml)");
  }
  const dispatchBearer = env.LEGION_DISPATCH_BEARER;
  if (dispatchBearer === undefined) {
    throw new Error("LEGION_DISPATCH_BEARER is required");
  }
  const ompInvocation = resolveValue(
    opts.cliOverrides?.ompInvocation,
    fileString(fields, "ompInvocation"),
    env.LEGION_OMP_INVOCATION,
    DEFAULT_OMP_INVOCATION
  );

  const boardProjectIds = resolveValue(
    opts.cliOverrides?.boardProjectIds,
    fileStringArray(fields, "boardProjectIds"),
    parseCsv(env.LEGION_BOARD_PROJECT_IDS, "LEGION_BOARD_PROJECT_IDS"),
    []
  );
  const appLogins = resolveValue(
    opts.cliOverrides?.appLogins,
    fileStringArray(fields, "appLogins"),
    parseCsv(env.LEGION_APP_LOGINS, "LEGION_APP_LOGINS"),
    []
  );
  const admissionCap = resolveValue(
    opts.cliOverrides?.admissionCap,
    fileNumber(fields, "admissionCap"),
    parseEnvPositiveInteger(env.LEGION_ADMISSION_CAP, "LEGION_ADMISSION_CAP"),
    DEFAULT_ADMISSION_CAP
  );
  const workerBudget = resolveValue(
    opts.cliOverrides?.workerBudget,
    fileNumber(fields, "workerBudget"),
    parseEnvPositiveInteger(env.LEGION_WORKER_BUDGET, "LEGION_WORKER_BUDGET"),
    DEFAULT_WORKER_BUDGET
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
  const ciQuietMs = resolveValue(
    opts.cliOverrides?.ciQuietMs,
    fileNumber(fields, "ciQuietMs"),
    parseEnvPositiveInteger(env.LEGION_CI_QUIET_MS, "LEGION_CI_QUIET_MS"),
    DEFAULT_CI_QUIET_MS
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

  const lifecycleNumbers: Record<string, number> = {
    admissionCap: admissionCap.value,
    workerBudget: workerBudget.value,
    maxRecursionDepth: maxRecursionDepth.value,
    lingerHours: lingerHours.value,
    ciQuietMs: ciQuietMs.value,
    maxFixAttempts: maxFixAttempts.value,
    resyncIntervalMs: resyncIntervalMs.value,
  };
  for (const [field, value] of Object.entries(lifecycleNumbers)) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw new Error(`${field} must be a positive integer`);
    }
  }

  const gates = resolveValue(opts.cliOverrides?.gates, fileGates(fields), undefined, {
    design: "root-issues",
    merge: "human",
  } as const);
  const parsedGates = parseGates(gates.value, "gates");
  if (!parsedGates) throw new Error("gates must be configured");
  const githubApps = resolveValue(
    opts.cliOverrides?.githubApps,
    fileGitHubApps(fields),
    undefined,
    {}
  );
  if (parsedGates.merge === "human" && Object.keys(githubApps.value).length === 0) {
    throw new Error("gates.merge=human requires at least one configured GitHub App login");
  }
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
      envoyUrl: validateUrl(envoyUrl.value, "ENVOY_URL"),
      natsUrls: natsUrls.value,
      dispatchUrl: validateUrl(dispatchUrl.value, "LEGION_DISPATCH_URL"),
      dispatchBearer: requireNonEmpty(dispatchBearer, "LEGION_DISPATCH_BEARER"),
      ompInvocation: requireNonEmpty(ompInvocation.value, "LEGION_OMP_INVOCATION"),
      boardProjectIds: boardProjectIds.value,
      appLogins: appLogins.value,
      admissionCap: admissionCap.value,
      workerBudget: workerBudget.value,
      maxRecursionDepth: maxRecursionDepth.value,
      lingerHours: lingerHours.value,
      ciQuietMs: ciQuietMs.value,
      maxFixAttempts: maxFixAttempts.value,
      resyncIntervalMs: resyncIntervalMs.value * (resyncIntervalMs.source === "env" ? 1000 : 1),
      gates: parsedGates,
      githubApps: githubApps.value,
      stateDir: stateDir.value,
    },
    warnings,
  };
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): DaemonConfig {
  return resolveDaemonConfig({ env }).config;
}
