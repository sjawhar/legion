import { describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { resolveDispatchConfig } from "../dispatch-config";

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

describe("resolveDispatchConfig — url", () => {
  it("prefers an explicit DISPATCH_MCP_URL over any config", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: false } });
    const url = resolveDispatchConfig(
      { DISPATCH_MCP_URL: "http://example.test/mcp" },
      { home, cwd: tempDir() }
    ).url;
    expect(url).toBe("http://example.test/mcp");
  });

  it("returns the user-config server URL with /mcp when dispatch is enabled", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "http://box:9999/" } });
    expect(resolveDispatchConfig({}, { home, cwd: tempDir() }).url).toBe("http://box:9999/mcp");
  });

  it("defaults the server URL when enabled without serverUrl", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true } });
    expect(resolveDispatchConfig({}, { home, cwd: tempDir() }).url).toBe("http://localhost:8766/mcp");
  });

  it("lets repo config keys override user config keys", () => {
    const home = tempDir();
    const cwd = tempDir();
    writeUserConfig(home, { dispatch: { enabled: false, serverUrl: "http://user:1111" } });
    writeRepoConfig(cwd, { dispatch: { enabled: true } });
    expect(resolveDispatchConfig({}, { home, cwd }).url).toBe("http://user:1111/mcp");
  });

  it("returns null when dispatch is not enabled anywhere", () => {
    expect(resolveDispatchConfig({}, { home: tempDir(), cwd: tempDir() }).url).toBe(null);
  });

  it("reads the user config from env.HOME when no explicit home is given", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "http://from-env-home:8766" } });
    expect(resolveDispatchConfig({ HOME: home }, { cwd: tempDir() }).url).toBe(
      "http://from-env-home:8766/mcp"
    );
    expect(resolveDispatchConfig({ HOME: tempDir() }, { cwd: tempDir() }).url).toBeNull();
  });

  it("returns null when serverUrl is not a valid URL", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "not a url" } });
    expect(resolveDispatchConfig({}, { home, cwd: tempDir() }).url).toBe(null);
  });

  it("returns null when the dispatch object carries unknown keys", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, typo: true } });
    expect(resolveDispatchConfig({}, { home, cwd: tempDir() }).url).toBe(null);
  });

  it("returns null on malformed config files", () => {
    const home = tempDir();
    const dir = path.join(home, ".config", "opencode");
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "envoy.json"), "{");
    expect(resolveDispatchConfig({}, { home, cwd: tempDir() }).url).toBe(null);
  });
});

describe("resolveDispatchConfig — error", () => {
  it("names the file and the removed defaultRepo key", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, defaultRepo: "acme/widgets" } });
    const error = resolveDispatchConfig({}, { home, cwd: tempDir() }).error;
    expect(error).toContain("envoy.json");
    expect(error).toContain("dispatch.defaultRepo");
  });

  it("names the removed appClientId key", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, appClientId: "Iv23liXYZ" } });
    const error = resolveDispatchConfig({}, { home, cwd: tempDir() }).error;
    expect(error).toContain("dispatch.appClientId");
  });

  it("is null when dispatch is simply disabled, not an error", () => {
    expect(resolveDispatchConfig({}, { home: tempDir(), cwd: tempDir() }).error).toBeNull();
  });

  it("is null when config is valid and enabled", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true } });
    expect(resolveDispatchConfig({}, { home, cwd: tempDir() }).error).toBeNull();
  });

  it("is null when an explicit DISPATCH_MCP_URL bypasses config entirely", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, defaultRepo: "acme/widgets" } });
    const error = resolveDispatchConfig(
      { DISPATCH_MCP_URL: "http://example.test/mcp" },
      { home, cwd: tempDir() }
    ).error;
    expect(error).toBeNull();
  });

  it("names a bad value on an otherwise-valid key", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "not a url" } });
    const error = resolveDispatchConfig({}, { home, cwd: tempDir() }).error;
    expect(error).toContain("dispatch.serverUrl");
  });

  it("reports malformed JSON", () => {
    const home = tempDir();
    const dir = path.join(home, ".config", "opencode");
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "envoy.json"), "{");
    const error = resolveDispatchConfig({}, { home, cwd: tempDir() }).error;
    expect(error).toContain("envoy.json");
    expect(error).toContain("JSON");
  });
});
