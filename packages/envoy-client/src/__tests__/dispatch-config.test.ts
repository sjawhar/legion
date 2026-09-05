import { describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { dispatchConfigError, resolveDispatchMcpUrl } from "../dispatch-config";

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

describe("dispatchConfigError", () => {
  it("names the file and the removed defaultRepo key", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, defaultRepo: "acme/widgets" } });
    const error = dispatchConfigError({}, { home, cwd: tempDir() });
    expect(error).toContain("envoy.json");
    expect(error).toContain("dispatch.defaultRepo");
  });

  it("names the removed appClientId key", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, appClientId: "Iv23liXYZ" } });
    const error = dispatchConfigError({}, { home, cwd: tempDir() });
    expect(error).toContain("dispatch.appClientId");
  });

  it("is null when dispatch is simply disabled, not an error", () => {
    expect(dispatchConfigError({}, { home: tempDir(), cwd: tempDir() })).toBeNull();
  });

  it("is null when config is valid and enabled", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true } });
    expect(dispatchConfigError({}, { home, cwd: tempDir() })).toBeNull();
  });

  it("is null when an explicit DISPATCH_MCP_URL bypasses config entirely", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, defaultRepo: "acme/widgets" } });
    const error = dispatchConfigError(
      { DISPATCH_MCP_URL: "http://example.test/mcp" },
      { home, cwd: tempDir() }
    );
    expect(error).toBeNull();
  });

  it("names a bad value on an otherwise-valid key", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "not a url" } });
    const error = dispatchConfigError({}, { home, cwd: tempDir() });
    expect(error).toContain("dispatch.serverUrl");
  });

  it("reports malformed JSON", () => {
    const home = tempDir();
    const dir = path.join(home, ".config", "opencode");
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "envoy.json"), "{");
    const error = dispatchConfigError({}, { home, cwd: tempDir() });
    expect(error).toContain("envoy.json");
    expect(error).toContain("JSON");
  });
});
