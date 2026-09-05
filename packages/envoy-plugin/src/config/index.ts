import { existsSync, readFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { type EnvoyConfig, EnvoyConfigSchema } from "./schema";

export interface LoadEnvoyConfigOptions {
  homeDir?: string;
}

/** A present but unusable envoy.json. The plugin refuses to load rather than run with dispatch silently off. */
export class EnvoyConfigError extends Error {
  readonly filePath: string;
  constructor(filePath: string, detail: string) {
    super(`[envoy-plugin] invalid config at ${filePath}: ${detail}`);
    this.name = "EnvoyConfigError";
    this.filePath = filePath;
  }
}

function readConfigFile(filePath: string): EnvoyConfig | null {
  if (!existsSync(filePath)) return null;
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(filePath, "utf-8")) as unknown;
  } catch (error) {
    throw new EnvoyConfigError(filePath, messageFor(error));
  }
  const parsed = EnvoyConfigSchema.safeParse(raw);
  if (!parsed.success) {
    const issues = parsed.error.issues
      .map((issue) => `${issue.path.join(".")}: ${issue.message}`)
      .join(", ");
    throw new EnvoyConfigError(filePath, issues);
  }
  return parsed.data;
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
