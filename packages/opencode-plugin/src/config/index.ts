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

const PluginConfigSchema = z
  .object({
    $schema: z.string().optional(),
    agents: z.record(z.string(), AgentOverrideSchema).optional(),
    categories: z.record(z.string(), CategoryConfigSchema).optional(),
    permission: PermissionConfigSchema.optional(),
  })
  .passthrough();

export interface AgentOverrideConfig {
  model?: string;
  temperature?: number;
  permission?: PermissionConfig;
}

export interface PluginConfig {
  agents?: {
    [agentName: string]: AgentOverrideConfig;
  };
  categories?: Record<string, CategoryOverrideConfig>;
  permission?: PermissionConfig;
}

const DEFAULT_CONFIG: PluginConfig = {};

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

function mergeConfig(base: PluginConfig, override: PluginConfig): PluginConfig {
  return {
    ...base,
    ...override,
    agents: mergeAgentOverrides(base.agents, override.agents),
    categories: mergeCategoryOverrides(base.categories, override.categories),
    permission: mergePermission(base.permission, override.permission),
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

  let merged = DEFAULT_CONFIG;
  const userConfig = readConfigFile(userConfigPath);
  if (userConfig) {
    merged = mergeConfig(merged, userConfig);
  }

  const repoConfig = readConfigFile(repoConfigPath);
  if (repoConfig) {
    merged = mergeConfig(merged, repoConfig);
  }

  return merged;
};

export const mergePermissionConfig = mergePermission;
