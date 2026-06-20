import path from "node:path";
import type { DispatchConfig } from "./config";

/**
 * OpenCode local MCP config shape that we inject into `config.mcp`. We use
 * `type: "local"` (subprocess via stdio) instead of `type: "remote"` so we
 * can rotate the GitHub bearer transparently — the StreamableHTTPClient
 * transport snapshots static headers once at construction, which would
 * break MCP calls after the gh-app installation token expires (~1h).
 *
 * The shim subprocess mints a fresh token via `gh auth token` per request
 * (with a 50-minute in-memory cache), so OpenCode never sees an expired
 * token. The user's `gh` shim handles per-CWD profile selection via the
 * project's `.git/config` `[gh-app "<profile>"]` block.
 */
export interface DispatchMcpEntry {
  type: "local";
  command: string[];
  environment: Record<string, string>;
  enabled: true;
}

export interface BuildDispatchMcpEntryOptions {
  dispatch: DispatchConfig | undefined;
  /**
   * Absolute path to the shim entry script. Defaults to the colocated
   * `bin/dispatch-mcp-shim.ts` next to this module. Override in tests.
   */
  shimPath?: string;
  /**
   * Command used to launch the shim. Defaults to `bun`. Override in tests
   * or when a different runtime is desired (e.g. `node` with a compiled
   * shim).
   */
  runtime?: string;
}

const DEFAULT_SERVER_URL = "http://localhost:8766";

function defaultShimPath(): string {
  // import.meta.dir resolves to this file's directory in Bun, e.g.
  // /home/ubuntu/legion/default/packages/envoy-plugin/src — go up one
  // level to the package root, then into bin/.
  return path.join(import.meta.dir, "..", "bin", "dispatch-mcp-shim.ts");
}

/**
 * Build the OpenCode `mcp.envoy` entry. Returns null when dispatch
 * is not enabled in envoy.json.
 *
 * Token availability is NOT validated here — the shim subprocess handles
 * token fetching at request time. If `gh auth token` fails inside the
 * shim, the affected MCP request returns a JSON-RPC error with a helpful
 * message; other MCP servers continue to work.
 */
export function buildDispatchMcpEntry(opts: BuildDispatchMcpEntryOptions): DispatchMcpEntry | null {
  if (!opts.dispatch?.enabled) return null;

  const baseUrl = (opts.dispatch.serverUrl ?? DEFAULT_SERVER_URL).replace(/\/+$/, "");
  const shimPath = opts.shimPath ?? defaultShimPath();
  const runtime = opts.runtime ?? "bun";

  return {
    type: "local",
    command: [runtime, shimPath],
    environment: {
      DISPATCH_MCP_URL: `${baseUrl}/mcp`,
    },
    enabled: true,
  };
}

/**
 * Inject the envoy MCP entry into an OpenCode `cfg` object. Returns a
 * structured result instead of logging directly so the behavior is pure and
 * testable; the caller forwards `warning` to the plugin logger when present.
 *
 * Idempotent on the plugin's own re-writes: when the existing `cfg.mcp.envoy`
 * deep-equals the entry we'd inject, this is a silent no-op. OpenCode's
 * InstanceState invalidation can re-run the plugin's config hook against a
 * Config-service cfg that still carries our prior mutation; without the
 * idempotency check that legitimate re-entry path produces a TUI stderr
 * alarm. A warning still fires when the existing entry is genuinely
 * different from ours (a user override the plugin must not clobber).
 */
export function injectEnvoyMcp(
  cfg: { mcp?: Record<string, unknown> } & Record<string, unknown>,
  entry: DispatchMcpEntry
): { warning?: string } {
  cfg.mcp = cfg.mcp ?? {};
  const existing = (cfg.mcp as Record<string, unknown>).envoy;
  if (existing !== undefined) {
    if (JSON.stringify(existing) === JSON.stringify(entry)) {
      return {};
    }
    return {
      warning: "[envoy-plugin] envoy MCP entry already present in config; not overriding",
    };
  }
  (cfg.mcp as Record<string, unknown>).envoy = entry;
  return {};
}
