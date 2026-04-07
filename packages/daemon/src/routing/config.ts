import { readFileSync } from "node:fs";
import { join } from "node:path";
import { parse as parseYaml } from "yaml";
import { ROUTING_CONFIG_PATH, type RoutingConfig, RoutingConfigSchema } from "./schema";

export interface LoadRoutingConfigResult {
  config: RoutingConfig | null;
  /** Non-null when config was invalid (parse or validation error). */
  warning: string | null;
}

/**
 * Load and validate the routing config from a workspace.
 *
 * - Missing file → returns null config, no warning (silent disable).
 * - Invalid YAML or schema → returns null config with warning message.
 * - Valid config → returns parsed config.
 */
export function loadRoutingConfig(workspace: string): LoadRoutingConfigResult {
  const configPath = join(workspace, ROUTING_CONFIG_PATH);

  let raw: string;
  try {
    raw = readFileSync(configPath, "utf-8");
  } catch {
    // Missing file silently disables routing (AC: "Missing config file disables routing silently")
    return { config: null, warning: null };
  }

  let parsed: unknown;
  try {
    parsed = parseYaml(raw);
  } catch (error) {
    return {
      config: null,
      warning: `Invalid YAML in ${ROUTING_CONFIG_PATH}: ${(error as Error).message}`,
    };
  }

  const result = RoutingConfigSchema.safeParse(parsed);
  if (!result.success) {
    return {
      config: null,
      warning: `Invalid routing config in ${ROUTING_CONFIG_PATH}: ${result.error.message}`,
    };
  }

  return { config: result.data, warning: null };
}
