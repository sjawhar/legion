import { describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { dispatchShimGate } from "../dispatch-mcp-shim";

function tempDir(): string {
  return mkdtempSync(path.join(os.tmpdir(), "dispatch-shim-gate-"));
}

function writeUserConfig(home: string, config: unknown): void {
  const dir = path.join(home, ".config", "opencode");
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, "envoy.json"), JSON.stringify(config));
}

describe("dispatchShimGate", () => {
  it("declines in a Legion tree environment even with an explicit URL", () => {
    const gate = dispatchShimGate(
      { LEGION_TREE: "owner/repo#1", DISPATCH_MCP_URL: "http://example.test/mcp" },
      { home: tempDir(), cwd: tempDir() }
    );
    expect(gate).toEqual({
      serve: false,
      reason:
        "Legion sessions use envoy_dispatch via the daemon; not serving the raw dispatch tool",
    });
  });

  it("declines in a Legion controller environment", () => {
    const gate = dispatchShimGate({ LEGION_CONTROLLER: "1" }, { home: tempDir(), cwd: tempDir() });
    expect(gate.serve).toBe(false);
  });

  it("declines when dispatch is not enabled anywhere", () => {
    const gate = dispatchShimGate({}, { home: tempDir(), cwd: tempDir() });
    expect(gate).toEqual({
      serve: false,
      reason: "dispatch is not enabled — set dispatch.enabled in envoy.json or DISPATCH_MCP_URL",
    });
  });

  it("serves with an explicit DISPATCH_MCP_URL outside Legion", () => {
    const gate = dispatchShimGate(
      { DISPATCH_MCP_URL: "http://example.test/mcp" },
      { home: tempDir(), cwd: tempDir() }
    );
    expect(gate).toEqual({ serve: true, remoteUrl: "http://example.test/mcp" });
  });

  it("serves from an enabled envoy.json outside Legion", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "http://box:9999" } });
    const gate = dispatchShimGate({}, { home, cwd: tempDir() });
    expect(gate).toEqual({ serve: true, remoteUrl: "http://box:9999/mcp" });
  });
});
