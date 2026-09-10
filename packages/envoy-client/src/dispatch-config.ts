import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import * as path from "node:path";
import { z } from "zod";
import { messageFor } from "./errors";

/** Default dispatch server base URL, matching the Go server's listen address. */
const DEFAULT_SERVER_URL = "http://localhost:8766";

function normalizeDispatchUrl(url: string): string {
  return url.replace(/\/+$/, "");
}

function parsedDispatchUrl(
  value: string,
  source: string
): { url: string | null; error: string | null } {
  const url = normalizeDispatchUrl(value);
  try {
    new URL(url);
    return { url, error: null };
  } catch {
    return { url: null, error: `${source} must be a valid URL` };
  }
}

// Mirrors the shared envoy.json contract (the Go loader in
// packages/envoy/internal/dispatch/config, which the dispatch server reads):
// strict dispatch keys, URL-shaped serverUrl, tolerant top level. A file the
// Go loader rejects as invalid must not enable the dispatch tool either.
const EnvoyFileSchema = z.looseObject({
  $schema: z.string().optional(),
  natsUrls: z.array(z.string()).optional(),
  dispatch: z
    .strictObject({
      enabled: z.boolean().optional(),
      serverUrl: z.url().optional(),
      token: z.string().optional(),
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

type DispatchEnvironment = {
  readonly DISPATCH_URL?: string;
  readonly DISPATCH_TOKEN?: string;
  readonly HOME?: string;
} & Record<string, string | undefined>;

export interface DispatchConfigResolution {
  readonly enabled: boolean;
  readonly url: string | null;
  readonly token: string | null;
  /** Set when a file, URL, or token prevents Dispatch from being enabled. */
  readonly error: string | null;
}

/**
 * Load Dispatch's URL and bearer token from the shared envoy.json contract.
 *
 * DISPATCH_URL and DISPATCH_TOKEN override file settings. Dispatch tools are
 * available only when both a URL and a bearer token resolve.
 */
export function resolveDispatchConfig(
  env: DispatchEnvironment,
  options: { readonly cwd?: string; readonly home?: string } = {}
): DispatchConfigResolution {
  const explicitUrl = env.DISPATCH_URL;

  // env is the call's one source of truth: a caller that hands us an
  // environment with HOME set is read from there. os.homedir() alone is not a
  // seam — Bun resolves it once at startup and ignores later HOME changes.
  const home = options.home ?? env.HOME ?? homedir();
  const cwd = options.cwd ?? process.cwd();
  const userFile = readEnvoyFile(path.join(home, ".config", "opencode", "envoy.json"));
  const repoFile = readEnvoyFile(path.join(cwd, ".opencode", "envoy.json"));

  for (const file of [userFile, repoFile]) {
    if (file.kind === "invalid") {
      return { enabled: false, url: null, token: null, error: file.reason };
    }
  }

  const merged: DispatchSettings = {
    ...(userFile.kind === "valid" ? userFile.settings : null),
    ...(repoFile.kind === "valid" ? repoFile.settings : null),
  };
  const rawUrl =
    explicitUrl !== undefined
      ? { value: explicitUrl, source: "DISPATCH_URL" }
      : merged.enabled === true
        ? { value: merged.serverUrl ?? DEFAULT_SERVER_URL, source: "dispatch.serverUrl" }
        : null;
  const url = rawUrl ? parsedDispatchUrl(rawUrl.value, rawUrl.source) : { url: null, error: null };
  const token = env.DISPATCH_TOKEN ?? merged.token ?? null;
  const tokenSource = env.DISPATCH_TOKEN === undefined ? "dispatch.token" : "DISPATCH_TOKEN";
  if (url.error !== null) return { enabled: false, url: null, token, error: url.error };
  if (url.url === null) return { enabled: false, url: null, token, error: null };
  if (!token) {
    return {
      enabled: false,
      url: url.url,
      token,
      error: `${tokenSource} must be a non-empty bearer token`,
    };
  }
  return { enabled: true, url: url.url, token, error: null };
}
