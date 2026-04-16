import os from "node:os";
import path from "node:path";
import { parse } from "yaml";
import type { LegionPaths } from "./paths";
import { resolveLegionPaths } from "./paths";

export type GitHubAppRole = "implement" | "review";

export interface GitHubAppRoleConfig {
  appId: string;
  privateKey: string;
  installations: Record<string, string>;
}

export type GitHubAppsConfig = Partial<Record<GitHubAppRole, GitHubAppRoleConfig>>;

export interface DaemonConfig {
  daemonPort: number;
  daemonPortExplicit: boolean;
  legionId?: string;
  legionDir?: string;
  paths: LegionPaths;
  checkIntervalMs: number;
  baseWorkerPort: number;
  stateFilePath: string;
  logDir: string;
  controllerSessionId?: string;
  controllerPrompt?: string;
  issueBackend: "linear" | "github";
  extraProjects?: string[];
  runtime: "opencode" | "claude-code";
  githubApps?: GitHubAppsConfig;
  envoyUrl: string;
  feedbackDisabled: boolean;
  feedbackMaxBytes: number;
  /** RSS threshold in bytes; serve restarts when exceeded. 0 = disabled. */
  maxRssBytes: number;
  /** Minimum interval between RSS checks in ms. */
  rssCheckIntervalMs: number;
  /** When true, daemon auto-dispatches next worker when current finishes. */
  autoAdvance: boolean;
  /** Maps worker mode to agent type for the initial prompt's AgentPartInput. */
  modeAgents: Partial<Record<string, string>>;
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

interface ConfigSchema {
  [key: string]: ConfigSchema | null;
  [CONFIG_ANY_KEY]?: ConfigSchema | null;
}
type ValueSource = "cli" | "config" | "env" | "default";

const BASE_DAEMON_PORT = 13370;
const DEFAULT_CHECK_INTERVAL_MS = 60_000;
const DEFAULT_BASE_WORKER_PORT = 13381;
const DEFAULT_MAX_RSS_GB = 20;
const DEFAULT_RSS_CHECK_INTERVAL_S = 60;
const DEFAULT_ENVOY_URL = "http://127.0.0.1:9020";
const DEFAULT_FEEDBACK_MAX_BYTES = 50 * 1024 * 1024;
const EXTRA_PROJECT_PATTERN = /^[^/]+\/\d+$/;
const GITHUB_APP_ROLES: GitHubAppRole[] = ["implement", "review"];
const GITHUB_APP_FIELD_NAMES = ["app_id", "private_key", "installations"] as const;
const CONFIG_SCHEMA: ConfigSchema = {
  project: null,
  extra_projects: null,
  backend: null,
  runtime: null,
  workspace: null,
  port: null,
  controller: {
    session_id: null,
    prompt: null,
  },
  github_apps: {
    implement: {
      app_id: null,
      private_key: null,
      installations: {
        [CONFIG_ANY_KEY]: null,
      },
    },
    review: {
      app_id: null,
      private_key: null,
      installations: {
        [CONFIG_ANY_KEY]: null,
      },
    },
  },
  memory: {
    max_rss_gb: null,
    rss_check_interval_seconds: null,
  },
  envoy_url: null,
  feedback: {
    disabled: null,
    max_bytes: null,
  },
  auto_advance: null,
  mode_agents: {
    [CONFIG_ANY_KEY]: null,
  },
};

function parseOptionalNumber(value: string | undefined): number | undefined {
  if (value === undefined || value === "") {
    return undefined;
  }
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function parseOptionalBoolean(value: string | undefined): boolean | undefined {
  if (value === undefined || value === "") {
    return undefined;
  }
  if (value === "true" || value === "1") {
    return true;
  }
  if (value === "false" || value === "0") {
    return false;
  }
  return undefined;
}

function resolveValue<T>(
  cliValue: T | undefined,
  configValue: T | undefined,
  envValue: T | undefined,
  defaultValue: T
): { value: T; source: ValueSource } {
  if (cliValue !== undefined) {
    return { value: cliValue, source: "cli" };
  }
  if (configValue !== undefined) {
    return { value: configValue, source: "config" };
  }
  if (envValue !== undefined) {
    return { value: envValue, source: "env" };
  }
  return { value: defaultValue, source: "default" };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function readString(value: unknown, fieldPath: string): string | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value !== "string") {
    throw new Error(`${fieldPath} must be a string`);
  }
  return value;
}

function readStringRecord(value: unknown, fieldPath: string): Record<string, string> | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (!isRecord(value)) {
    throw new Error(`${fieldPath} must be a mapping`);
  }

  const result: Record<string, string> = {};
  for (const [key, entryValue] of Object.entries(value)) {
    if (typeof entryValue !== "string") {
      throw new Error(`${fieldPath}.${key} must be a string`);
    }
    result[key] = entryValue;
  }

  return result;
}

function readNumber(value: unknown, fieldPath: string): number | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new Error(`${fieldPath} must be a number`);
  }
  return value;
}

function readBoolean(value: unknown, fieldPath: string): boolean | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value !== "boolean") {
    throw new Error(`${fieldPath} must be a boolean`);
  }
  return value;
}

function normalizeConfigPath(value: string, configDir: string): string {
  return path.isAbsolute(value) ? value : path.resolve(configDir, value);
}

export function validateControllerPrompt(prompt: string | undefined): void {
  if (!prompt) {
    return;
  }
  if (prompt.length > 10000) {
    throw new Error(
      `Controller prompt exceeds maximum length of 10000 characters (got ${prompt.length})`
    );
  }
  const hasControlChars = [...prompt].some((ch) => {
    const code = ch.charCodeAt(0);
    return (
      (code >= 0 && code <= 8) ||
      code === 11 ||
      code === 12 ||
      (code >= 14 && code <= 31) ||
      code === 127
    );
  });
  if (hasControlChars) {
    throw new Error("Controller prompt contains invalid control characters");
  }
}

export function validateBackend(
  value: string | undefined,
  sourceName: string
): "linear" | "github" | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (value !== "linear" && value !== "github") {
    throw new Error(`${sourceName} must be 'linear' or 'github' (got: ${value})`);
  }
  return value;
}

export function validateRuntime(
  value: string | undefined,
  sourceName: string
): "opencode" | "claude-code" | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (value !== "opencode" && value !== "claude-code") {
    throw new Error(`${sourceName} must be 'opencode' or 'claude-code' (got: ${value})`);
  }
  return value;
}

function validateControllerSessionId(
  value: string | undefined,
  sourceName: string
): string | undefined {
  if (!value) {
    return undefined;
  }
  if (!value.startsWith("ses_")) {
    throw new Error(`${sourceName} must start with 'ses_' (got: ${value})`);
  }
  return value;
}

function parseExtraProjects(value: string | undefined): string[] | undefined {
  if (value === undefined || value === "") {
    return undefined;
  }

  const extraProjects: string[] = [];
  const seen = new Set<string>();

  for (const rawEntry of value.split(",")) {
    const entry = rawEntry.trim();
    if (!entry) {
      continue;
    }
    if (!EXTRA_PROJECT_PATTERN.test(entry)) {
      throw new Error(`LEGION_EXTRA_PROJECTS entries must match owner/number (got: ${entry})`);
    }
    if (!seen.has(entry)) {
      seen.add(entry);
      extraProjects.push(entry);
    }
  }

  return extraProjects.length > 0 ? extraProjects : undefined;
}

function parseExtraProjectsArray(value: unknown): string[] | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (!Array.isArray(value)) {
    throw new Error("extra_projects must be an array");
  }

  const extraProjects: string[] = [];
  const seen = new Set<string>();
  for (const entry of value) {
    if (typeof entry !== "string") {
      throw new Error("extra_projects entries must be strings matching owner/number");
    }
    if (!EXTRA_PROJECT_PATTERN.test(entry)) {
      throw new Error(`extra_projects entries must match owner/number (got: ${entry})`);
    }
    if (!seen.has(entry)) {
      seen.add(entry);
      extraProjects.push(entry);
    }
  }

  return extraProjects.length > 0 ? extraProjects : undefined;
}

function parseMaxRssBytesFromGb(maxRssGb: number | undefined): number | undefined {
  if (maxRssGb === undefined) {
    return undefined;
  }
  return maxRssGb > 0 ? maxRssGb * 1024 * 1024 * 1024 : 0;
}

function parseRssCheckIntervalMsFromSeconds(seconds: number | undefined): number | undefined {
  if (seconds === undefined || seconds <= 0) {
    return undefined;
  }
  return seconds * 1000;
}

function collectUnknownKeys(
  value: unknown,
  schema: ConfigSchema | null,
  pathParts: string[],
  warnings: string[]
): void {
  if (!schema || !isRecord(value)) {
    return;
  }

  for (const [key, childValue] of Object.entries(value)) {
    const childSchema = Object.hasOwn(schema, key) ? schema[key] : schema[CONFIG_ANY_KEY];
    if (childSchema === undefined) {
      warnings.push(`Unknown config key: ${[...pathParts, key].join(".")}`);
      continue;
    }
    collectUnknownKeys(childValue, childSchema, [...pathParts, key], warnings);
  }
}

function loadGitHubAppsFromFile(value: unknown): GitHubAppsConfig | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (!isRecord(value)) {
    throw new Error("github_apps must be a mapping");
  }

  const config: GitHubAppsConfig = {};
  let hasAny = false;

  for (const role of GITHUB_APP_ROLES) {
    const roleValue = value[role];
    if (roleValue === undefined || roleValue === null) {
      continue;
    }
    if (!isRecord(roleValue)) {
      throw new Error(`github_apps.${role} must be a mapping`);
    }

    const appId = readString(roleValue.app_id, `github_apps.${role}.app_id`);
    const privateKey = readString(roleValue.private_key, `github_apps.${role}.private_key`);
    const installations = readStringRecord(
      roleValue.installations,
      `github_apps.${role}.installations`
    );

    const missing = GITHUB_APP_FIELD_NAMES.filter((fieldName) => {
      const fieldValue = roleValue[fieldName];
      return fieldValue === undefined || fieldValue === null || fieldValue === "";
    });
    const hasAnyField = GITHUB_APP_FIELD_NAMES.some((fieldName) => {
      const fieldValue = roleValue[fieldName];
      return fieldValue !== undefined && fieldValue !== null && fieldValue !== "";
    });

    if (!hasAnyField) {
      continue;
    }
    if (missing.length > 0) {
      throw new Error(`github_apps.${role} is missing required fields: ${missing.join(", ")}`);
    }

    config[role] = {
      appId: appId as string,
      privateKey: privateKey as string,
      installations: installations as Record<string, string>,
    };
    hasAny = true;
  }

  return hasAny ? config : undefined;
}

function maybeReadStringField(fields: Record<string, unknown>, key: string): string | undefined {
  const value = fields[key];
  return typeof value === "string" ? value : undefined;
}

function maybeReadNumberField(fields: Record<string, unknown>, key: string): number | undefined {
  const value = fields[key];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function maybeReadBooleanField(fields: Record<string, unknown>, key: string): boolean | undefined {
  const value = fields[key];
  return typeof value === "boolean" ? value : undefined;
}

function maybeReadStringArrayField(
  fields: Record<string, unknown>,
  key: string
): string[] | undefined {
  const value = fields[key];
  return Array.isArray(value) && value.every((entry) => typeof entry === "string")
    ? value
    : undefined;
}

function maybeReadIssueBackend(
  fields: Record<string, unknown>,
  key: string
): "linear" | "github" | undefined {
  const value = fields[key];
  return value === "linear" || value === "github" ? value : undefined;
}

function maybeReadRuntime(
  fields: Record<string, unknown>,
  key: string
): "opencode" | "claude-code" | undefined {
  const value = fields[key];
  return value === "opencode" || value === "claude-code" ? value : undefined;
}

function maybeReadGitHubApps(
  fields: Record<string, unknown>,
  key: string
): GitHubAppsConfig | undefined {
  const value = fields[key];
  return isRecord(value) ? (value as GitHubAppsConfig) : undefined;
}

function pushEnvDeprecationWarning(
  warnings: string[],
  source: ValueSource,
  env: Record<string, string | undefined>,
  envVar: string,
  yamlKey: string
): void {
  if (source === "env" && env[envVar] !== undefined) {
    warnings.push(`${envVar} is deprecated; move this value to legion.yaml as '${yamlKey}'.`);
  }
}

export function loadConfigFromFile(yamlText: string, configDir: string): LoadedConfigFile {
  let parsed: unknown;
  try {
    parsed = parse(yamlText);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new Error(`Invalid YAML config: ${message}`);
  }

  if (parsed === undefined || parsed === null) {
    return { fields: {}, warnings: [] };
  }
  if (!isRecord(parsed)) {
    throw new Error("Config file root must be a mapping");
  }

  const warnings: string[] = [];
  collectUnknownKeys(parsed, CONFIG_SCHEMA, [], warnings);

  const fields: Record<string, unknown> = {};

  const project = readString(parsed.project, "project");
  if (project !== undefined) {
    fields.legionId = project;
  }

  const workspace = readString(parsed.workspace, "workspace");
  if (workspace !== undefined) {
    fields.legionDir = normalizeConfigPath(workspace, configDir);
  }

  const port = readNumber(parsed.port, "port");
  if (port !== undefined) {
    fields.daemonPort = port;
  }

  const backend = validateBackend(readString(parsed.backend, "backend"), "backend");
  if (backend !== undefined) {
    fields.issueBackend = backend;
  }

  const runtime = validateRuntime(readString(parsed.runtime, "runtime"), "runtime");
  if (runtime !== undefined) {
    fields.runtime = runtime;
  }

  const controller = parsed.controller;
  if (controller !== undefined && controller !== null) {
    if (!isRecord(controller)) {
      throw new Error("controller must be a mapping");
    }
    const sessionId = validateControllerSessionId(
      readString(controller.session_id, "controller.session_id"),
      "controller.session_id"
    );
    const prompt = readString(controller.prompt, "controller.prompt");
    validateControllerPrompt(prompt);

    if (sessionId !== undefined) {
      fields.controllerSessionId = sessionId;
    }
    if (prompt !== undefined) {
      fields.controllerPrompt = prompt;
    }
  }

  const githubApps = loadGitHubAppsFromFile(parsed.github_apps);
  if (githubApps !== undefined) {
    fields.githubApps = githubApps;
  }

  const memory = parsed.memory;
  if (memory !== undefined && memory !== null) {
    if (!isRecord(memory)) {
      throw new Error("memory must be a mapping");
    }
    const maxRssGb = readNumber(memory.max_rss_gb, "memory.max_rss_gb");
    const rssCheckIntervalSeconds = readNumber(
      memory.rss_check_interval_seconds,
      "memory.rss_check_interval_seconds"
    );
    const maxRssBytes = parseMaxRssBytesFromGb(maxRssGb);
    const rssCheckIntervalMs = parseRssCheckIntervalMsFromSeconds(rssCheckIntervalSeconds);

    if (maxRssBytes !== undefined) {
      fields.maxRssBytes = maxRssBytes;
    }
    if (rssCheckIntervalMs !== undefined) {
      fields.rssCheckIntervalMs = rssCheckIntervalMs;
    }
  }

  const envoyUrl = readString(parsed.envoy_url, "envoy_url");
  if (envoyUrl !== undefined) {
    fields.envoyUrl = envoyUrl;
  }

  const feedback = parsed.feedback;
  if (feedback !== undefined && feedback !== null) {
    if (!isRecord(feedback)) {
      throw new Error("feedback must be a mapping");
    }
    const disabled = readBoolean(feedback.disabled, "feedback.disabled");
    const maxBytes = readNumber(feedback.max_bytes, "feedback.max_bytes");
    if (disabled !== undefined) {
      fields.feedbackDisabled = disabled;
    }
    if (maxBytes !== undefined) {
      fields.feedbackMaxBytes = maxBytes;
    }
  }

  const autoAdvance = readBoolean(parsed.auto_advance, "auto_advance");
  if (autoAdvance !== undefined) {
    fields.autoAdvance = autoAdvance;
  }

  const extraProjects = parseExtraProjectsArray(parsed.extra_projects);
  const effectiveBackend = (fields.issueBackend as "linear" | "github" | undefined) ?? "linear";
  if (extraProjects !== undefined) {
    if (effectiveBackend !== "github") {
      throw new Error("extra_projects requires backend: github");
    }
    fields.extraProjects = extraProjects;
  }

  const modeAgents = parsed.mode_agents;
  if (modeAgents !== undefined && modeAgents !== null) {
    if (!isRecord(modeAgents)) {
      throw new Error("mode_agents must be a mapping");
    }
    const validModes = new Set(["architect", "plan", "implement", "test", "review", "merge"]);
    const mapping: Record<string, string> = {};
    for (const [mode, agent] of Object.entries(modeAgents)) {
      if (!validModes.has(mode)) {
        warnings.push(`mode_agents: unknown mode '${mode}' (valid: ${[...validModes].join(", ")})`);
        continue;
      }
      const agentName = readString(agent, `mode_agents.${mode}`);
      if (agentName !== undefined) {
        mapping[mode] = agentName;
      }
    }
    if (Object.keys(mapping).length > 0) {
      fields.modeAgents = mapping;
    }
  }

  return { fields, warnings };
}

export function resolveDaemonConfig(
  opts: ResolveDaemonConfigOptions = {}
): ResolveDaemonConfigResult {
  const env = opts.env ?? {};
  const configFields = opts.configFile?.fields ?? {};
  const warnings = [...(opts.configFile?.warnings ?? [])];

  const envLegionId = env.LEGION_ID || undefined;
  const envLegionDir = env.LEGION_DIR || undefined;
  const envDaemonPort = parseOptionalNumber(env.LEGION_DAEMON_PORT);
  const envControllerSessionId = validateControllerSessionId(
    env.LEGION_CONTROLLER_SESSION_ID || undefined,
    "LEGION_CONTROLLER_SESSION_ID"
  );
  const envControllerPrompt = env.LEGION_CONTROLLER_PROMPT || undefined;
  validateControllerPrompt(envControllerPrompt);
  const envIssueBackend = validateBackend(env.LEGION_ISSUE_BACKEND, "LEGION_ISSUE_BACKEND");
  const envRuntime = validateRuntime(env.LEGION_RUNTIME, "LEGION_RUNTIME");
  const envExtraProjects = parseExtraProjects(env.LEGION_EXTRA_PROJECTS);
  const envMaxRssBytes = parseMaxRssBytesFromGb(parseOptionalNumber(env.OPENCODE_MAX_RSS_GB));
  const envRssCheckIntervalMs = parseRssCheckIntervalMsFromSeconds(
    parseOptionalNumber(env.OPENCODE_RSS_CHECK_INTERVAL)
  );
  const envEnvoyUrl = env.ENVOY_URL || undefined;
  const envFeedbackDisabled = parseOptionalBoolean(env.LEGION_FEEDBACK_DISABLED);
  const envFeedbackMaxBytes = parseOptionalNumber(env.LEGION_FEEDBACK_MAX_BYTES);
  const envAutoAdvance = parseOptionalBoolean(env.LEGION_AUTO_ADVANCE);

  const legionId = resolveValue(
    opts.cliOverrides?.legionId,
    maybeReadStringField(configFields, "legionId"),
    envLegionId,
    undefined
  );
  const legionDir = resolveValue(
    opts.cliOverrides?.legionDir,
    maybeReadStringField(configFields, "legionDir"),
    envLegionDir,
    undefined
  );
  const daemonPort = resolveValue(
    opts.cliOverrides?.daemonPort,
    maybeReadNumberField(configFields, "daemonPort"),
    envDaemonPort,
    BASE_DAEMON_PORT
  );
  const controllerSessionId = resolveValue(
    opts.cliOverrides?.controllerSessionId,
    maybeReadStringField(configFields, "controllerSessionId"),
    envControllerSessionId,
    undefined
  );
  const controllerPrompt = resolveValue(
    opts.cliOverrides?.controllerPrompt,
    maybeReadStringField(configFields, "controllerPrompt"),
    envControllerPrompt,
    undefined
  );
  const issueBackend = resolveValue<DaemonConfig["issueBackend"]>(
    opts.cliOverrides?.issueBackend,
    maybeReadIssueBackend(configFields, "issueBackend"),
    envIssueBackend,
    "linear"
  );
  const extraProjects = resolveValue(
    opts.cliOverrides?.extraProjects,
    maybeReadStringArrayField(configFields, "extraProjects"),
    envExtraProjects,
    undefined
  );
  const runtime = resolveValue<DaemonConfig["runtime"]>(
    opts.cliOverrides?.runtime,
    maybeReadRuntime(configFields, "runtime"),
    envRuntime,
    "opencode"
  );
  const githubApps = resolveValue(
    opts.cliOverrides?.githubApps,
    maybeReadGitHubApps(configFields, "githubApps"),
    undefined,
    undefined
  );
  const maxRssBytes = resolveValue(
    opts.cliOverrides?.maxRssBytes,
    maybeReadNumberField(configFields, "maxRssBytes"),
    envMaxRssBytes,
    DEFAULT_MAX_RSS_GB * 1024 * 1024 * 1024
  );
  const rssCheckIntervalMs = resolveValue(
    opts.cliOverrides?.rssCheckIntervalMs,
    maybeReadNumberField(configFields, "rssCheckIntervalMs"),
    envRssCheckIntervalMs,
    DEFAULT_RSS_CHECK_INTERVAL_S * 1000
  );
  const envoyUrl = resolveValue(
    opts.cliOverrides?.envoyUrl,
    maybeReadStringField(configFields, "envoyUrl"),
    envEnvoyUrl,
    DEFAULT_ENVOY_URL
  );
  const feedbackDisabled = resolveValue(
    opts.cliOverrides?.feedbackDisabled,
    maybeReadBooleanField(configFields, "feedbackDisabled"),
    envFeedbackDisabled,
    false
  );
  const feedbackMaxBytes = resolveValue(
    opts.cliOverrides?.feedbackMaxBytes,
    maybeReadNumberField(configFields, "feedbackMaxBytes"),
    envFeedbackMaxBytes,
    DEFAULT_FEEDBACK_MAX_BYTES
  );
  const autoAdvance = resolveValue(
    opts.cliOverrides?.autoAdvance,
    maybeReadBooleanField(configFields, "autoAdvance"),
    envAutoAdvance,
    false
  );
  if (extraProjects.value !== undefined && issueBackend.value !== "github") {
    throw new Error("extra_projects requires backend: github");
  }

  const paths = resolveLegionPaths(env, os.homedir());
  const stateFilePath = legionId.value
    ? paths.forLegion(legionId.value).workersFile
    : path.join(paths.stateDir, "daemon", "workers.json");
  const logDir = legionId.value
    ? paths.forLegion(legionId.value).logDir
    : path.join(paths.stateDir, "daemon", "logs");

  pushEnvDeprecationWarning(warnings, legionId.source, env, "LEGION_ID", "project");
  pushEnvDeprecationWarning(
    warnings,
    extraProjects.source,
    env,
    "LEGION_EXTRA_PROJECTS",
    "extra_projects"
  );
  pushEnvDeprecationWarning(warnings, issueBackend.source, env, "LEGION_ISSUE_BACKEND", "backend");
  pushEnvDeprecationWarning(warnings, runtime.source, env, "LEGION_RUNTIME", "runtime");
  pushEnvDeprecationWarning(warnings, legionDir.source, env, "LEGION_DIR", "workspace");
  pushEnvDeprecationWarning(warnings, daemonPort.source, env, "LEGION_DAEMON_PORT", "port");
  pushEnvDeprecationWarning(
    warnings,
    controllerSessionId.source,
    env,
    "LEGION_CONTROLLER_SESSION_ID",
    "controller.session_id"
  );
  pushEnvDeprecationWarning(
    warnings,
    controllerPrompt.source,
    env,
    "LEGION_CONTROLLER_PROMPT",
    "controller.prompt"
  );
  pushEnvDeprecationWarning(
    warnings,
    maxRssBytes.source,
    env,
    "OPENCODE_MAX_RSS_GB",
    "memory.max_rss_gb"
  );
  pushEnvDeprecationWarning(
    warnings,
    rssCheckIntervalMs.source,
    env,
    "OPENCODE_RSS_CHECK_INTERVAL",
    "memory.rss_check_interval_seconds"
  );
  pushEnvDeprecationWarning(warnings, envoyUrl.source, env, "ENVOY_URL", "envoy_url");
  pushEnvDeprecationWarning(
    warnings,
    feedbackDisabled.source,
    env,
    "LEGION_FEEDBACK_DISABLED",
    "feedback.disabled"
  );
  pushEnvDeprecationWarning(
    warnings,
    feedbackMaxBytes.source,
    env,
    "LEGION_FEEDBACK_MAX_BYTES",
    "feedback.max_bytes"
  );

  return {
    config: {
      daemonPort: daemonPort.value,
      daemonPortExplicit: daemonPort.source === "cli" || daemonPort.source === "config",
      legionId: legionId.value,
      legionDir: legionDir.value,
      paths,
      checkIntervalMs: DEFAULT_CHECK_INTERVAL_MS,
      baseWorkerPort: DEFAULT_BASE_WORKER_PORT,
      stateFilePath,
      logDir,
      controllerSessionId: controllerSessionId.value,
      controllerPrompt: controllerPrompt.value,
      issueBackend: issueBackend.value,
      extraProjects: extraProjects.value,
      runtime: runtime.value,
      githubApps: githubApps.value,
      envoyUrl: envoyUrl.value,
      feedbackDisabled: feedbackDisabled.value,
      feedbackMaxBytes: feedbackMaxBytes.value,
      maxRssBytes: maxRssBytes.value,
      rssCheckIntervalMs: rssCheckIntervalMs.value,
      autoAdvance: autoAdvance.value,
      modeAgents: (configFields.modeAgents as Partial<Record<string, string>> | undefined) ?? {},
    },
    warnings,
  };
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): DaemonConfig {
  return resolveDaemonConfig({ env }).config;
}
