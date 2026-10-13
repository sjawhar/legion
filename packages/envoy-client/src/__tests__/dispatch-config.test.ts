import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { activeDispatchConfig, resolveDispatchConfig } from "../dispatch-config";

function tempDir(): string {
  return mkdtempSync(path.join(os.tmpdir(), "dispatch-config-"));
}

/** Writes the neutral file, `$HOME/.config/legion/envoy.json`, which a command reads when it is
 * handed no file and runs under no harness. Returns its path. */
function writeUserConfig(home: string, config: unknown): string {
  const dir = path.join(home, ".config", "legion");
  mkdirSync(dir, { recursive: true });
  const file = path.join(dir, "envoy.json");
  writeFileSync(file, typeof config === "string" ? config : JSON.stringify(config));
  return file;
}

describe("resolveDispatchConfig", () => {
  test("ignores DISPATCH_MCP_URL even when a file token resolves, so no host ever receives it", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { token: "file-token" },
    });

    expect(
      resolveDispatchConfig(
        { HOME: home, DISPATCH_MCP_URL: "http://attacker.test/mcp" },
        { harness: "command", cwd: tempDir() }
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

    expect(resolveDispatchConfig({ HOME: home }, { harness: "command", cwd: tempDir() })).toEqual({
      enabled: true,
      url: "http://dispatch.test",
      token: "file-token",
      error: null,
    });
  });

  test("reads the file its harness names", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://neutral.internal.example", token: "neutral" },
    });
    const profile = tempDir();
    writeFileSync(
      path.join(profile, "envoy.json"),
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "profile-token" },
      })
    );
    const env = { HOME: home, OMP_PROFILE: "work", PI_CODING_AGENT_DIR: profile };

    expect(resolveDispatchConfig(env, { harness: "omp", cwd: tempDir() })).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "profile-token",
      error: null,
    });
    expect(resolveDispatchConfig(env, { harness: "claude", cwd: tempDir() })).toEqual({
      enabled: true,
      url: "https://neutral.internal.example",
      token: "neutral",
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
          HOME: home,
          DISPATCH_URL: "http://override.test/",
          DISPATCH_TOKEN: "environment-token",
        },
        { harness: "command", cwd: tempDir() }
      )
    ).toEqual({
      enabled: true,
      url: "http://override.test",
      token: "environment-token",
      error: null,
    });
  });

  test("refuses DISPATCH_URL at another origin than the file's server when the token is the file's", () => {
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "file-token" },
    });

    expect(
      resolveDispatchConfig(
        { HOME: home, DISPATCH_URL: "https://other.internal.example/api" },
        { harness: "command", cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      url: null,
      token: null,
      error: `DISPATCH_URL names https://other.internal.example, but ${file}'s dispatch.token is for https://dispatch.example; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
    });
  });

  test("pairs DISPATCH_URL with the file's token when both name the same origin", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "file-token" },
    });

    expect(
      resolveDispatchConfig(
        { HOME: home, DISPATCH_URL: "https://dispatch.example/" },
        { harness: "command", cwd: tempDir() }
      )
    ).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "file-token",
      error: null,
    });
  });

  test("a file with dispatch.enabled false is off whatever the variables say", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: false, serverUrl: "https://dispatch.example", token: "file-token" },
    });

    expect(
      resolveDispatchConfig(
        {
          HOME: home,
          DISPATCH_URL: "https://dispatch.example",
          DISPATCH_TOKEN: "environment-token",
        },
        { harness: "command", cwd: tempDir() }
      )
    ).toEqual({ enabled: false, url: null, token: null, error: null });
  });

  test("does not enable the dispatch tools until both URL and token resolve", () => {
    const home = tempDir();
    writeUserConfig(home, { dispatch: { enabled: true, serverUrl: "http://dispatch.test" } });

    expect(resolveDispatchConfig({ HOME: home }, { harness: "command", cwd: tempDir() })).toEqual({
      enabled: false,
      url: "http://dispatch.test",
      token: null,
      error: "dispatch.token must be a non-empty bearer token",
    });
  });

  test("reports malformed and invalid envoy.json rather than silently opting out", () => {
    const invalidHome = tempDir();
    writeUserConfig(invalidHome, { dispatch: { enabled: true, unexpected: true } });
    const invalid = resolveDispatchConfig(
      { HOME: invalidHome },
      { harness: "command", cwd: tempDir() }
    );
    expect(invalid.enabled).toBe(false);
    expect(invalid.error).toContain("dispatch.unexpected");

    const malformedHome = tempDir();
    writeUserConfig(malformedHome, "{");
    const malformed = resolveDispatchConfig(
      { HOME: malformedHome },
      { harness: "command", cwd: tempDir() }
    );
    expect(malformed.enabled).toBe(false);
    expect(malformed.error).toContain("invalid JSON");
  });

  test("rejects malformed URLs and empty environment tokens with the setting named", () => {
    const invalidUrl = resolveDispatchConfig(
      { HOME: tempDir(), DISPATCH_URL: "not-a-url", DISPATCH_TOKEN: "token" },
      { harness: "command", cwd: tempDir() }
    );
    expect(invalidUrl.enabled).toBe(false);
    expect(invalidUrl.error).toContain("DISPATCH_URL");

    const emptyToken = resolveDispatchConfig(
      { HOME: tempDir(), DISPATCH_URL: "http://dispatch.test", DISPATCH_TOKEN: "" },
      { harness: "command", cwd: tempDir() }
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
          HOME: home,
          DISPATCH_URL: "http://override.test",
          DISPATCH_TOKEN: "environment-token",
          DISPATCH_TOKEN_FILE: tokenFile,
        },
        { harness: "command", cwd: tempDir() }
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
      { HOME: home, DISPATCH_TOKEN: "environment-token", DISPATCH_TOKEN_FILE: missing },
      { harness: "command", cwd: tempDir() }
    );
    expect(unreadable.enabled).toBe(false);
    expect(unreadable.token).toBeNull();
    expect(unreadable.error).toContain(
      `DISPATCH_TOKEN_FILE names ${missing}, which could not be read`
    );

    const empty = path.join(tempDir(), "empty");
    writeFileSync(empty, "  \n");
    const blank = resolveDispatchConfig(
      { HOME: home, DISPATCH_TOKEN_FILE: empty },
      { harness: "command", cwd: tempDir() }
    );
    expect(blank.enabled).toBe(false);
    expect(blank.token).toBeNull();
    expect(blank.error).toBe(`DISPATCH_TOKEN_FILE names ${empty}, which is empty`);
  });

  test("does not pair a repository server URL with an inherited user token", () => {
    const home = tempDir();
    const cwd = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "user-token" },
    });
    const repoConfig = path.join(cwd, ".envoy");
    mkdirSync(repoConfig, { recursive: true });
    const checkout = path.join(repoConfig, "envoy.json");
    writeFileSync(
      checkout,
      JSON.stringify({ dispatch: { enabled: true, serverUrl: "https://collector.attacker.test" } })
    );

    expect(resolveDispatchConfig({ HOME: home }, { harness: "command", cwd })).toEqual({
      enabled: false,
      url: null,
      token: null,
      error: `${checkout}: dispatch.serverUrl requires dispatch.token in the same file`,
    });
  });

  test("a repository server URL goes only with the repository's own token, never DISPATCH_TOKEN", () => {
    const home = tempDir();
    const cwd = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "user-token" },
    });
    mkdirSync(path.join(cwd, ".envoy"), { recursive: true });
    writeFileSync(
      path.join(cwd, ".envoy", "envoy.json"),
      JSON.stringify({
        dispatch: { serverUrl: "https://repo.internal.example", token: "repo-token" },
      })
    );

    expect(
      resolveDispatchConfig(
        { HOME: home, DISPATCH_TOKEN: "environment-token" },
        { harness: "command", cwd }
      )
    ).toEqual({
      enabled: true,
      url: "https://repo.internal.example",
      token: "repo-token",
      error: null,
    });
  });
});

// The one "is Dispatch configured" check every host shares: a configuration that resolves is
// returned with its URL and token, none is null, and a broken file throws rather than reading as
// unconfigured.
describe("activeDispatchConfig", () => {
  test("returns the resolved URL and token, null when nothing is configured, and throws on a broken file", () => {
    expect(
      activeDispatchConfig(
        { HOME: tempDir(), DISPATCH_URL: "http://dispatch.test/", DISPATCH_TOKEN: "env-token" },
        { harness: "command", cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "http://dispatch.test", token: "env-token", error: null });
    expect(
      activeDispatchConfig({ HOME: tempDir() }, { harness: "command", cwd: tempDir() })
    ).toBeNull();
    const home = tempDir();
    writeUserConfig(home, "{not json");
    expect(() =>
      activeDispatchConfig({ HOME: home }, { harness: "command", cwd: tempDir() })
    ).toThrow(/^dispatch config: /);
  });
});
