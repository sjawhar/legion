import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import * as path from "node:path";
import { z } from "zod";

/** Default dispatch server base URL, matching the Go server's listen address. */
const DEFAULT_SERVER_URL = "http://localhost:8766";

// Mirrors the shared envoy.json contract (packages/envoy-plugin/src/config/
// schema.ts and the Go loader in packages/envoy/internal/dispatch/config):
// strict dispatch keys, URL-shaped serverUrl, tolerant top level. A file the
// other loaders reject as invalid must not enable the shim either.
const EnvoyFileSchema = z.looseObject({
  $schema: z.string().optional(),
  natsUrls: z.array(z.string()).optional(),
  dispatch: z
    .strictObject({
      enabled: z.boolean().optional(),
      serverUrl: z.url().optional(),
      defaultRepo: z
        .string()
        .regex(/^[^/]+\/[^/]+$/)
        .optional(),
      appClientId: z.string().optional(),
    })
    .optional(),
});

type DispatchSettings = NonNullable<z.infer<typeof EnvoyFileSchema>["dispatch"]>;

function dispatchSettingsFrom(filePath: string): DispatchSettings | null {
  let raw: string;
  try {
    raw = readFileSync(filePath, "utf-8");
  } catch {
    return null;
  }
  let parsedJson: unknown;
  try {
    parsedJson = JSON.parse(raw);
  } catch {
    return null;
  }
  const parsed = EnvoyFileSchema.safeParse(parsedJson);
  if (!parsed.success) return null;
  return parsed.data.dispatch ?? null;
}

/**
 * Resolve the dispatch server's /mcp endpoint for a shim process.
 *
 * Precedence: an explicit DISPATCH_MCP_URL wins (the OpenCode plugin injects
 * it into the MCP entry it builds); otherwise the shared envoy.json opt-in
 * decides — user config (~/.config/opencode/envoy.json) shallow-merged with
 * repo config (<cwd>/.opencode/envoy.json, repo keys win), the same files the
 * dispatch server and the OpenCode plugin read, validated against the same
 * contract. Returns null when dispatch is not enabled or the config is
 * invalid, so an always-mounted shim can decline to serve.
 */
export function resolveDispatchMcpUrl(
  env: Record<string, string | undefined>,
  options: { readonly cwd?: string; readonly home?: string } = {}
): string | null {
  const explicit = env.DISPATCH_MCP_URL;
  if (explicit) return explicit;
  const home = options.home ?? homedir();
  const cwd = options.cwd ?? process.cwd();
  const merged = {
    ...dispatchSettingsFrom(path.join(home, ".config", "opencode", "envoy.json")),
    ...dispatchSettingsFrom(path.join(cwd, ".opencode", "envoy.json")),
  };
  if (merged.enabled !== true) return null;
  const baseUrl = (merged.serverUrl ?? DEFAULT_SERVER_URL).replace(/\/+$/, "");
  return `${baseUrl}/mcp`;
}
