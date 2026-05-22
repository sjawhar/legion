import { existsSync, readFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { logger } from "../log";
import { type EnvoyConfig, EnvoyConfigSchema } from "./schema";

export interface LoadEnvoyConfigOptions {
  homeDir?: string;
}

function readConfigFile(filePath: string): EnvoyConfig | null {
  if (!existsSync(filePath)) return null;
  try {
    const content = readFileSync(filePath, "utf-8");
    const raw = JSON.parse(content) as unknown;
    const parsed = EnvoyConfigSchema.safeParse(raw);
    if (!parsed.success) {
      const issues = parsed.error.issues
        .map((issue) => `${issue.path.join(".")}: ${issue.message}`)
        .join(", ");
      logger.warn(`[envoy-plugin] Invalid config at ${filePath}: ${issues}`);
      return null;
    }
    return parsed.data as EnvoyConfig;
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    logger.warn(`[envoy-plugin] Failed to load config at ${filePath}: ${message}`);
    return null;
  }
}

function mergeConfig(base: EnvoyConfig, override: EnvoyConfig): EnvoyConfig {
  return {
    ...base,
    ...override,
    dispatch:
      base.dispatch || override.dispatch
        ? {
            ...base.dispatch,
            ...override.dispatch,
          }
        : undefined,
  };
}

export async function loadEnvoyConfig(
  directory: string,
  options: LoadEnvoyConfigOptions = {}
): Promise<EnvoyConfig> {
  const homeDir = options.homeDir ?? os.homedir();
  const userConfigPath = path.join(homeDir, ".config", "opencode", "envoy.json");
  const repoConfigPath = path.join(directory, ".opencode", "envoy.json");

  let merged: EnvoyConfig = {};
  const userConfig = readConfigFile(userConfigPath);
  if (userConfig) merged = mergeConfig(merged, userConfig);
  const repoConfig = readConfigFile(repoConfigPath);
  if (repoConfig) merged = mergeConfig(merged, repoConfig);
  return merged;
}

export type { DispatchConfig, EnvoyConfig } from "./schema";
