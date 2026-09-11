import { describe, expect, test } from "bun:test";
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

describe("resolveDispatchConfig", () => {
  test("ignores DISPATCH_MCP_URL even when a file token resolves, so no host ever receives it", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { token: "file-token" },
    });

    expect(
      resolveDispatchConfig(
        { DISPATCH_MCP_URL: "http://attacker.test/mcp" },
        { home, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      url: null,
      token: "file-token",
      error: null,
    });
  });

  test("reads an optional token from enabled envoy.json dispatch configuration", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://dispatch.test/", token: "file-token" },
    });

    expect(resolveDispatchConfig({}, { home, cwd: tempDir() })).toEqual({
      enabled: true,
      url: "http://dispatch.test",
      token: "file-token",
      error: null,
    });
  });

  test("uses DISPATCH_URL and DISPATCH_TOKEN ahead of file settings", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: "file-token" },
    });

    expect(
      resolveDispatchConfig(
        {
          DISPATCH_URL: "http://override.test/",
          DISPATCH_TOKEN: "environment-token",
        },
        { home, cwd: tempDir() }
      )
    ).toEqual({
      enabled: true,
      url: "http://override.test",
      token: "environment-token",
      error: null,
    });
  });

  test("does not enable the dispatch tools until both URL and token resolve", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "http://dispatch.test" } });

    expect(resolveDispatchConfig({}, { home, cwd: tempDir() })).toEqual({
      enabled: false,
      url: "http://dispatch.test",
      token: null,
      error: "dispatch.token must be a non-empty bearer token",
    });
  });

  test("reports malformed and invalid envoy.json rather than silently opting out", () => {
    const invalidHome = tempDir();
    writeUserConfig(invalidHome, { dispatch: { enabled: true, unexpected: true } });
    const invalid = resolveDispatchConfig({}, { home: invalidHome, cwd: tempDir() });
    expect(invalid.enabled).toBe(false);
    expect(invalid.error).toContain("dispatch.unexpected");

    const malformedHome = tempDir();
    const configDir = path.join(malformedHome, ".config", "opencode");
    mkdirSync(configDir, { recursive: true });
    writeFileSync(path.join(configDir, "envoy.json"), "{");
    const malformed = resolveDispatchConfig({}, { home: malformedHome, cwd: tempDir() });
    expect(malformed.enabled).toBe(false);
    expect(malformed.error).toContain("invalid JSON");
  });

  test("rejects malformed URLs and empty environment tokens with the setting named", () => {
    const invalidUrl = resolveDispatchConfig(
      { DISPATCH_URL: "not-a-url", DISPATCH_TOKEN: "token" },
      { home: tempDir(), cwd: tempDir() }
    );
    expect(invalidUrl.enabled).toBe(false);
    expect(invalidUrl.error).toContain("DISPATCH_URL");

    const emptyToken = resolveDispatchConfig(
      { DISPATCH_URL: "http://dispatch.test", DISPATCH_TOKEN: "" },
      { home: tempDir(), cwd: tempDir() }
    );
    expect(emptyToken.enabled).toBe(false);
    expect(emptyToken.error).toContain("DISPATCH_TOKEN");
  });

  test("resolves the token from DISPATCH_TOKEN_FILE ahead of DISPATCH_TOKEN and dispatch.token", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: "file-token" },
    });
    const tokenFile = path.join(tempDir(), "dispatch-token");
    writeFileSync(tokenFile, "pointer-token\n");

    expect(
      resolveDispatchConfig(
        {
          DISPATCH_URL: "http://override.test",
          DISPATCH_TOKEN: "environment-token",
          DISPATCH_TOKEN_FILE: tokenFile,
        },
        { home, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "http://override.test", token: "pointer-token", error: null });
  });

  test("disables Dispatch naming DISPATCH_TOKEN_FILE and its path when the file is unreadable or empty, without falling back", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: "file-token" },
    });
    const missing = path.join(tempDir(), "missing");
    const unreadable = resolveDispatchConfig(
      { DISPATCH_TOKEN: "environment-token", DISPATCH_TOKEN_FILE: missing },
      { home, cwd: tempDir() }
    );
    expect(unreadable.enabled).toBe(false);
    expect(unreadable.token).toBeNull();
    expect(unreadable.error).toContain(
      `DISPATCH_TOKEN_FILE names ${missing}, which could not be read`
    );

    const empty = path.join(tempDir(), "empty");
    writeFileSync(empty, "  \n");
    const blank = resolveDispatchConfig({ DISPATCH_TOKEN_FILE: empty }, { home, cwd: tempDir() });
    expect(blank.enabled).toBe(false);
    expect(blank.token).toBeNull();
    expect(blank.error).toBe(`DISPATCH_TOKEN_FILE names ${empty}, which is empty`);
  });
});
