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
  test("strips the deprecated MCP suffix and warns only once", () => {
    const previousWarn = console.warn;
    const warnings: string[] = [];
    console.warn = (message: unknown) => warnings.push(String(message));
    try {
      const first = resolveDispatchConfig({
        DISPATCH_MCP_URL: "http://dispatch.test/mcp/",
        DISPATCH_TOKEN: "token",
      });
      const second = resolveDispatchConfig({
        DISPATCH_MCP_URL: "http://other.test/mcp",
        DISPATCH_TOKEN: "token",
      });

      expect(first).toEqual({
        enabled: true,
        url: "http://dispatch.test",
        token: "token",
      });
      expect(second.url).toBe("http://other.test");
      expect(warnings).toEqual([expect.stringContaining("DISPATCH_URL")]);
    } finally {
      console.warn = previousWarn;
    }
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
          DISPATCH_MCP_URL: "http://deprecated.test/mcp",
          DISPATCH_TOKEN: "environment-token",
        },
        { home, cwd: tempDir() }
      )
    ).toEqual({
      enabled: true,
      url: "http://override.test",
      token: "environment-token",
    });
  });

  test("does not enable the dispatch tools until both URL and token resolve", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "http://dispatch.test" } });

    expect(resolveDispatchConfig({}, { home, cwd: tempDir() })).toEqual({
      enabled: false,
      url: "http://dispatch.test",
      token: null,
    });
  });
});
