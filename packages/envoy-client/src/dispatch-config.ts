import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import * as path from "node:path";
import { z } from "zod";
import { messageFor } from "./errors";

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
    })
    .optional(),
});

type DispatchSettings = NonNullable<z.infer<typeof EnvoyFileSchema>["dispatch"]>;

type EnvoyFileResult =
  | { readonly kind: "absent" }
  | { readonly kind: "invalid"; readonly reason: string }
  | { readonly kind: "valid"; readonly settings: DispatchSettings | null };

/** Describe the first schema-validation issue, naming the file and offending key. */
function describeSchemaIssue(filePath: string, error: z.ZodError): string {
  const issue = error.issues[0];
  if (!issue) return `${filePath}: invalid dispatch config`;
  if (issue.code === "unrecognized_keys") {
    const keys = issue.keys.map((key) => `dispatch.${key}`).join(", ");
    return `${filePath}: unrecognized dispatch key(s): ${keys}`;
  }
  return `${filePath}: ${issue.path.join(".")}: ${issue.message}`;
}

function readEnvoyFile(filePath: string): EnvoyFileResult {
  let raw: string;
  try {
    raw = readFileSync(filePath, "utf-8");
  } catch {
    return { kind: "absent" };
  }
  let parsedJson: unknown;
  try {
    parsedJson = JSON.parse(raw);
  } catch (err) {
    return { kind: "invalid", reason: `${filePath}: invalid JSON (${messageFor(err)})` };
  }
  const parsed = EnvoyFileSchema.safeParse(parsedJson);
  if (!parsed.success) {
    return { kind: "invalid", reason: describeSchemaIssue(filePath, parsed.error) };
  }
  return { kind: "valid", settings: parsed.data.dispatch ?? null };
}

export interface DispatchConfigResolution {
  readonly url: string | null;
  /** Set when a config file failed validation and explains why resolution yielded nothing. */
  readonly error: string | null;
}

/**
 * Load the dispatch server URL from the shared envoy.json contract.
 *
 * Precedence: an explicit DISPATCH_MCP_URL wins (the OpenCode plugin injects
 * it into the MCP entry it builds); otherwise the shared envoy.json opt-in
 * decides — user config (~/.config/opencode/envoy.json) shallow-merged with
 * repo config (<cwd>/.opencode/envoy.json, repo keys win), the same files the
 * dispatch server and the OpenCode plugin read, validated against the same
 * contract. `url` is null when dispatch is not enabled or a config file is
 * invalid; `error` names the invalid file and key when that is the cause.
 */
export function resolveDispatchConfig(
  env: Record<string, string | undefined>,
  options: { readonly cwd?: string; readonly home?: string } = {}
): DispatchConfigResolution {
  const explicit = env["DISPATCH_MCP_URL"];
  if (explicit) return { url: explicit, error: null };

  const home = options.home ?? homedir();
  const cwd = options.cwd ?? process.cwd();
  const userFile = readEnvoyFile(path.join(home, ".config", "opencode", "envoy.json"));
  const repoFile = readEnvoyFile(path.join(cwd, ".opencode", "envoy.json"));

  for (const file of [userFile, repoFile]) {
    if (file.kind === "invalid") return { url: null, error: file.reason };
  }

  const merged: DispatchSettings = {
    ...(userFile.kind === "valid" ? userFile.settings : null),
    ...(repoFile.kind === "valid" ? repoFile.settings : null),
  };
  if (merged.enabled !== true) return { url: null, error: null };
  const baseUrl = (merged.serverUrl ?? DEFAULT_SERVER_URL).replace(/\/+$/, "");
  return { url: `${baseUrl}/mcp`, error: null };
}

/** Resolve the dispatch server's /mcp endpoint for a shim process. */
export function resolveDispatchMcpUrl(
  env: Record<string, string | undefined>,
  options: { readonly cwd?: string; readonly home?: string } = {}
): string | null {
  return resolveDispatchConfig(env, options).url;
}

/**
 * Name why config resolution yielded no URL, when the cause is an invalid
 * envoy.json (bad JSON, a removed/unknown dispatch key, or a bad value) — as
 * opposed to dispatch simply being disabled, which has no error to report.
 */
export function dispatchConfigError(
  env: Record<string, string | undefined>,
  options: { readonly cwd?: string; readonly home?: string } = {}
): string | null {
  return resolveDispatchConfig(env, options).error;
}
