import { describe, expect, it } from "bun:test";
import { buildDispatchMcpEntry, injectEnvoyMcp } from "../dispatch-mcp";

describe("buildDispatchMcpEntry", () => {
  it("returns null when dispatch is undefined", () => {
    const result = buildDispatchMcpEntry({ dispatch: undefined });
    expect(result).toBeNull();
  });

  it("returns null when dispatch.enabled is false", () => {
    const result = buildDispatchMcpEntry({
      dispatch: { enabled: false, serverUrl: "http://example:8766" },
    });
    expect(result).toBeNull();
  });

  it("builds a local MCP entry pointing at the shim with serverUrl in env", () => {
    const result = buildDispatchMcpEntry({
      dispatch: {
        enabled: true,
        serverUrl: "http://sami-agents-mx:8766",
      },
      shimPath: "/path/to/shim.ts",
      runtime: "bun",
    });
    expect(result).toEqual({
      type: "local",
      command: ["bun", "/path/to/shim.ts"],
      environment: {
        DISPATCH_MCP_URL: "http://sami-agents-mx:8766/mcp",
      },
      enabled: true,
    });
  });

  it("falls back to localhost:8766 when serverUrl is omitted", () => {
    const result = buildDispatchMcpEntry({
      dispatch: { enabled: true },
      shimPath: "/shim.ts",
    });
    expect(result?.environment.DISPATCH_MCP_URL).toBe("http://localhost:8766/mcp");
  });

  it("strips trailing slashes from serverUrl before appending /mcp", () => {
    const result = buildDispatchMcpEntry({
      dispatch: { enabled: true, serverUrl: "http://example:8766//" },
      shimPath: "/shim.ts",
    });
    expect(result?.environment.DISPATCH_MCP_URL).toBe("http://example:8766/mcp");
  });

  it("uses bun as the default runtime", () => {
    const result = buildDispatchMcpEntry({
      dispatch: { enabled: true },
      shimPath: "/shim.ts",
    });
    expect(result?.command[0]).toBe("bun");
  });

  it("uses the provided runtime override", () => {
    const result = buildDispatchMcpEntry({
      dispatch: { enabled: true },
      shimPath: "/shim.ts",
      runtime: "node",
    });
    expect(result?.command[0]).toBe("node");
  });

  it("default shim path resolves to bin/dispatch-mcp-shim.ts in the package root", () => {
    const result = buildDispatchMcpEntry({
      dispatch: { enabled: true },
    });
    expect(result?.command[1]).toContain("bin/dispatch-mcp-shim.ts");
  });
});

describe("injectEnvoyMcp", () => {
  const entry = {
    type: "local" as const,
    command: ["bun", "/shim.ts"],
    environment: { DISPATCH_MCP_URL: "http://test:8766/mcp" },
    enabled: true as const,
  };

  it("adds the entry to a cfg that has no mcp block yet", () => {
    const cfg: { mcp?: Record<string, unknown> } = {};
    const result = injectEnvoyMcp(cfg, entry);
    expect(result.warning).toBeUndefined();
    expect(cfg.mcp?.envoy).toEqual(entry);
  });

  it("is idempotent on its own re-write — second call does not warn", () => {
    const cfg: { mcp?: Record<string, unknown> } = {};
    injectEnvoyMcp(cfg, entry);
    const second = injectEnvoyMcp(cfg, entry);
    // No warning when the existing entry already equals what we'd inject.
    // This is the common case after InstanceState invalidation re-runs the
    // config hook against a Config-service cfg that still has our prior
    // mutation — silent no-op, not a TUI stderr alarm.
    expect(second.warning).toBeUndefined();
    expect(cfg.mcp?.envoy).toEqual(entry);
  });

  it("warns and preserves the existing entry when it differs from ours", () => {
    const userOverride = {
      type: "local" as const,
      command: ["node", "/custom-shim.js"],
      environment: { DISPATCH_MCP_URL: "http://other:9999/mcp" },
      enabled: true as const,
    };
    const cfg: { mcp?: Record<string, unknown> } = { mcp: { envoy: userOverride } };
    const result = injectEnvoyMcp(cfg, entry);
    expect(result.warning).toContain("already present");
    expect(cfg.mcp?.envoy).toEqual(userOverride);
  });
});
