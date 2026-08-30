import { describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { resolveDispatchMcpUrl } from "../dispatch-config";

function tempDir(): string {
  return mkdtempSync(path.join(os.tmpdir(), "dispatch-config-"));
}

function writeUserConfig(home: string, config: unknown): void {
  const dir = path.join(home, ".config", "opencode");
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, "envoy.json"), JSON.stringify(config));
}

function writeRepoConfig(cwd: string, config: unknown): void {
  const dir = path.join(cwd, ".opencode");
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, "envoy.json"), JSON.stringify(config));
}

describe("resolveDispatchMcpUrl", () => {
  it("prefers an explicit DISPATCH_MCP_URL over any config", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: false } });
    const url = resolveDispatchMcpUrl(
      { DISPATCH_MCP_URL: "http://example.test/mcp" },
      { home, cwd: tempDir() }
    );
    expect(url).toBe("http://example.test/mcp");
  });

  it("returns the user-config server URL with /mcp when dispatch is enabled", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "http://box:9999/" } });
    expect(resolveDispatchMcpUrl({}, { home, cwd: tempDir() })).toBe("http://box:9999/mcp");
  });

  it("defaults the server URL when enabled without serverUrl", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true } });
    expect(resolveDispatchMcpUrl({}, { home, cwd: tempDir() })).toBe("http://localhost:8766/mcp");
  });

  it("lets repo config keys override user config keys", () => {
    const home = tempDir();
    const cwd = tempDir();
    writeUserConfig(home, { dispatch: { enabled: false, serverUrl: "http://user:1111" } });
    writeRepoConfig(cwd, { dispatch: { enabled: true } });
    expect(resolveDispatchMcpUrl({}, { home, cwd })).toBe("http://user:1111/mcp");
  });

  it("returns null when dispatch is not enabled anywhere", () => {
    expect(resolveDispatchMcpUrl({}, { home: tempDir(), cwd: tempDir() })).toBe(null);
  });

  it("returns null when serverUrl is not a valid URL", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "not a url" } });
    expect(resolveDispatchMcpUrl({}, { home, cwd: tempDir() })).toBe(null);
  });

  it("returns null when the dispatch object carries unknown keys", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, typo: true } });
    expect(resolveDispatchMcpUrl({}, { home, cwd: tempDir() })).toBe(null);
  });

  it("returns null on malformed config files", () => {
    const home = tempDir();
    const dir = path.join(home, ".config", "opencode");
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "envoy.json"), "{");
    expect(resolveDispatchMcpUrl({}, { home, cwd: tempDir() })).toBe(null);
  });
});
