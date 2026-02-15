import { existsSync, readFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { tool } from "@opencode-ai/plugin";
import type { CategoryOverrideConfig } from "../delegation";

const z = tool.schema;

const PermissionActionSchema = z.enum(["ask", "allow", "deny"]);
const PermissionRuleSchema = z.union([
  PermissionActionSchema,
  z.record(z.string(), PermissionActionSchema),
]);
const PermissionConfigSchema = z.union([
  PermissionActionSchema,
  z.record(
    z.string(),
    z.union([PermissionRuleSchema, z.array(z.string()), PermissionActionSchema])
  ),
]);

export type PermissionActionConfig = "ask" | "allow" | "deny";
export type PermissionRuleConfig = PermissionActionConfig | Record<string, PermissionActionConfig>;
export type PermissionConfig =
  | PermissionActionConfig
  | Record<string, PermissionRuleConfig | PermissionActionConfig | string[]>;

const AgentOverrideSchema = z
  .object({
    model: z.string().optional(),
    temperature: z.number().optional(),
    permission: PermissionConfigSchema.optional(),
  })
  .passthrough();

const CategoryConfigSchema = z
  .object({
    defaultModel: z.string().optional(),
    description: z.string().optional(),
    systemPrompt: z.string().optional(),
  })
  .strict();

const ConcurrencyConfigSchema = z
  .object({
    perModel: z.number().optional(),
    global: z.number().optional(),
  })
  .strict();

const RetryConfigSchema = z
  .object({
    maxRetries: z.number().optional(),
    delayMs: z.number().optional(),
    fallbackModel: z.string().optional(),
  })
  .strict();

const PluginConfigSchema = z
  .object({
    $schema: z.string().optional(),
    agents: z.record(z.string(), AgentOverrideSchema).optional(),
    categories: z.record(z.string(), CategoryConfigSchema).optional(),
    permission: PermissionConfigSchema.optional(),
    continuationGracePeriodMs: z.number().optional(),
    concurrency: ConcurrencyConfigSchema.optional(),
    inactivityAlertMs: z.number().optional(),
    retry: RetryConfigSchema.optional(),
    taskRetentionMs: z.number().optional(),
  })
  .passthrough();

export interface AgentOverrideConfig {
  model?: string;
  temperature?: number;
  permission?: PermissionConfig;
}

export interface ConcurrencyConfig {
  perModel?: number;
  global?: number;
}

export interface RetryConfig {
  maxRetries?: number;
  delayMs?: number;
  fallbackModel?: string;
}

export interface PluginConfig {
  agents?: {
    [agentName: string]: AgentOverrideConfig;
  };
  categories?: Record<string, CategoryOverrideConfig>;
  permission?: PermissionConfig;
  continuationGracePeriodMs?: number;
  concurrency?: ConcurrencyConfig;
  inactivityAlertMs?: number;
  retry?: RetryConfig;
  taskRetentionMs?: number;
}

const DEFAULT_CONFIG: PluginConfig = {
  concurrency: {
    perModel: 5,
    global: 15,
  },
  inactivityAlertMs: 600000,
  retry: {
    maxRetries: 1,
    delayMs: 2000,
  },
  taskRetentionMs: 3600000,
};

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value);
}

function mergePermission(
  base?: PermissionConfig,
  override?: PermissionConfig
): PermissionConfig | undefined {
  if (!override) return base;
  if (!base) return override;
  if (typeof base === "string" || typeof override === "string") return override;

  const merged: Record<string, PermissionRuleConfig | PermissionActionConfig | string[]> = {
    ...base,
  };

  for (const [key, value] of Object.entries(override)) {
    const existing = merged[key];
    if (isPlainRecord(existing) && isPlainRecord(value)) {
      merged[key] = { ...existing, ...value } as PermissionRuleConfig;
      continue;
    }
    merged[key] = value as PermissionRuleConfig | PermissionActionConfig | string[];
  }

  return merged;
}

function mergeAgentOverrides(
  base?: Record<string, AgentOverrideConfig>,
  override?: Record<string, AgentOverrideConfig>
): Record<string, AgentOverrideConfig> | undefined {
  if (!base) return override;
  if (!override) return base;
  const merged: Record<string, AgentOverrideConfig> = { ...base };
  for (const [key, value] of Object.entries(override)) {
    const existing = merged[key] ?? {};
    merged[key] = { ...existing, ...value };
  }
  return merged;
}

function mergeCategoryOverrides(
  base?: Record<string, CategoryOverrideConfig>,
  override?: Record<string, CategoryOverrideConfig>
): Record<string, CategoryOverrideConfig> | undefined {
  if (!base) return override;
  if (!override) return base;
  const merged: Record<string, CategoryOverrideConfig> = { ...base };
  for (const [key, value] of Object.entries(override)) {
    const existing = merged[key] ?? {};
    merged[key] = { ...existing, ...value };
  }
  return merged;
}

function mergeConcurrency(
  base?: ConcurrencyConfig,
  override?: ConcurrencyConfig
): ConcurrencyConfig | undefined {
  if (!base) return override;
  if (!override) return base;
  return { ...base, ...override };
}

function mergeRetry(base?: RetryConfig, override?: RetryConfig): RetryConfig | undefined {
  if (!base) return override;
  if (!override) return base;
  return { ...base, ...override };
}

function mergeConfig(base: PluginConfig, override: PluginConfig): PluginConfig {
  return {
    ...base,
    ...override,
    agents: mergeAgentOverrides(base.agents, override.agents),
    categories: mergeCategoryOverrides(base.categories, override.categories),
    permission: mergePermission(base.permission, override.permission),
    concurrency: mergeConcurrency(base.concurrency, override.concurrency),
    retry: mergeRetry(base.retry, override.retry),
  };
}

function applyDefaults(config: PluginConfig): PluginConfig {
  return {
    ...config,
    concurrency: {
      perModel: config.concurrency?.perModel ?? DEFAULT_CONFIG.concurrency?.perModel,
      global: config.concurrency?.global ?? DEFAULT_CONFIG.concurrency?.global,
    },
    inactivityAlertMs: config.inactivityAlertMs ?? DEFAULT_CONFIG.inactivityAlertMs,
    retry: {
      maxRetries: config.retry?.maxRetries ?? DEFAULT_CONFIG.retry?.maxRetries,
      delayMs: config.retry?.delayMs ?? DEFAULT_CONFIG.retry?.delayMs,
      fallbackModel: config.retry?.fallbackModel ?? DEFAULT_CONFIG.retry?.fallbackModel,
    },
    taskRetentionMs: config.taskRetentionMs ?? DEFAULT_CONFIG.taskRetentionMs,
  };
}

function readConfigFile(filePath: string): PluginConfig | null {
  if (!existsSync(filePath)) return null;
  try {
    const content = readFileSync(filePath, "utf-8");
    const raw = JSON.parse(content) as unknown;
    const parsed = PluginConfigSchema.safeParse(raw);
    if (!parsed.success) {
      const issues = parsed.error.issues
        .map((issue) => `${issue.path.join(".")}: ${issue.message}`)
        .join(", ");
      console.warn(`[opencode-legion] Invalid config at ${filePath}: ${issues}`);
      return null;
    }
    return parsed.data as PluginConfig;
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    console.warn(`[opencode-legion] Failed to load config at ${filePath}: ${message}`);
    return null;
  }
}

export interface LoadPluginConfigOptions {
  homeDir?: string;
}

export const loadPluginConfig = async (
  directory: string,
  options: LoadPluginConfigOptions = {}
): Promise<PluginConfig> => {
  const homeDir = options.homeDir ?? os.homedir();
  const userConfigPath = path.join(homeDir, ".config", "opencode", "opencode-legion.json");
  const repoConfigPath = path.join(directory, ".opencode", "opencode-legion.json");

  let merged: PluginConfig = {};
  const userConfig = readConfigFile(userConfigPath);
  if (userConfig) {
    merged = mergeConfig(merged, userConfig);
  }

  const repoConfig = readConfigFile(repoConfigPath);
  if (repoConfig) {
    merged = mergeConfig(merged, repoConfig);
  }

  return applyDefaults(merged);
};

export const mergePermissionConfig = mergePermission;
