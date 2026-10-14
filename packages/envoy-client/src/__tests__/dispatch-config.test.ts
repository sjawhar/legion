import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { activeDispatchSettings, type DispatchEnvironment } from "../dispatch-config";
import type { EnvoyHarness } from "../envoy-config";
import { sessionConfigFile, sessionDispatchSettings } from "../session-settings";

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

const command = { harness: "command" } as const;

/** Dispatch for a caller running in `harness`, from the file that harness's rule chooses. */
function dispatchFor(
  env: DispatchEnvironment,
  options: { readonly harness: EnvoyHarness; readonly cwd: string }
) {
  return sessionDispatchSettings(sessionConfigFile(options.harness, env), env, {
    cwd: options.cwd,
  }).dispatch;
}

describe("Dispatch from a harness's envoy.json", () => {
  test("ignores DISPATCH_MCP_URL even when a file token resolves, so no host ever receives it", () => {
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { token: "file-token" },
    });

    expect(
      dispatchFor(
        { HOME: home, DISPATCH_MCP_URL: "http://attacker.test/mcp" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: `${file} names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`,
      error: null,
    });
  });

  test("reads an optional token from enabled envoy.json dispatch configuration", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://dispatch.test/", token: "file-token" },
    });

    expect(dispatchFor({ HOME: home }, { ...command, cwd: tempDir() })).toEqual({
      enabled: true,
      url: "http://dispatch.test",
      token: "file-token",
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

    expect(dispatchFor(env, { harness: "omp", cwd: tempDir() })).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "profile-token",
    });
    expect(dispatchFor(env, { harness: "claude", cwd: tempDir() })).toEqual({
      enabled: true,
      url: "https://neutral.internal.example",
      token: "neutral",
    });
  });

  test("uses DISPATCH_URL and DISPATCH_TOKEN ahead of file settings", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: "file-token" },
    });

    expect(
      dispatchFor(
        {
          HOME: home,
          DISPATCH_URL: "http://override.test/",
          DISPATCH_TOKEN: "environment-token",
        },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "http://override.test", token: "environment-token" });
  });

  test("refuses DISPATCH_URL at another origin than the file's server when the token is the file's", () => {
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "file-token" },
    });

    expect(
      dispatchFor(
        { HOME: home, DISPATCH_URL: "https://other.internal.example/api" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: `DISPATCH_URL names https://other.internal.example, but ${file}'s dispatch.token is for https://dispatch.example; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
    });
    // An origin is scheme, host and port: each differs from the file's server on its own.
    for (const url of ["http://dispatch.example", "https://dispatch.example:8443"]) {
      expect(
        dispatchFor({ HOME: home, DISPATCH_URL: url }, { ...command, cwd: tempDir() })
      ).toMatchObject({ enabled: false, error: expect.stringContaining(`DISPATCH_URL names`) });
    }
  });

  test("a file's token with no server of its own goes only to the default server", () => {
    const home = tempDir();
    const file = writeUserConfig(home, { dispatch: { token: "file-token" } });
    expect(
      dispatchFor(
        { HOME: home, DISPATCH_URL: "https://other.internal.example" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: `DISPATCH_URL names https://other.internal.example, but ${file}'s dispatch.token is for http://localhost:8766; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
    });
    expect(
      dispatchFor(
        { HOME: home, DISPATCH_URL: "http://localhost:8766/" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "http://localhost:8766", token: "file-token" });
  });

  test("pairs DISPATCH_URL with the file's token when both name the same origin", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "file-token" },
    });

    expect(
      dispatchFor(
        { HOME: home, DISPATCH_URL: "https://dispatch.example/" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "https://dispatch.example", token: "file-token" });
  });

  test("a file with dispatch.enabled false is off whatever the variables say", () => {
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { enabled: false, serverUrl: "https://dispatch.example", token: "file-token" },
    });

    expect(
      dispatchFor(
        {
          HOME: home,
          DISPATCH_URL: "https://dispatch.example",
          DISPATCH_TOKEN: "environment-token",
        },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: `${file} sets dispatch.enabled to false: Dispatch is off`,
      error: null,
    });
  });

  test("does not enable the dispatch tools until both URL and token resolve, naming the file to fix", () => {
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://dispatch.test" },
    });

    expect(dispatchFor({ HOME: home }, { ...command, cwd: tempDir() })).toEqual({
      enabled: false,
      notice: null,
      error: `${file}: dispatch is enabled and no token is: set dispatch.token, or DISPATCH_TOKEN or DISPATCH_TOKEN_FILE`,
    });
    writeUserConfig(home, { dispatch: { enabled: true, token: "" } });
    expect(dispatchFor({ HOME: home }, { ...command, cwd: tempDir() })).toEqual({
      enabled: false,
      notice: null,
      error: `dispatch.token in ${file} must be a non-empty bearer token`,
    });
    // DISPATCH_URL with no token anywhere names every place one goes.
    const bare = tempDir();
    const bareFile = path.join(bare, ".config", "legion", "envoy.json");
    expect(
      dispatchFor(
        { HOME: bare, DISPATCH_URL: "https://dispatch.example" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: `DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or dispatch.token in ${bareFile}`,
    });
  });

  test("reports malformed and invalid envoy.json rather than silently opting out", () => {
    const invalidHome = tempDir();
    writeUserConfig(invalidHome, { dispatch: { enabled: true, unexpected: true } });
    const invalid = dispatchFor({ HOME: invalidHome }, { ...command, cwd: tempDir() });
    expect(invalid).toMatchObject({
      enabled: false,
      error: expect.stringContaining("dispatch.unexpected"),
    });

    const malformedHome = tempDir();
    writeUserConfig(malformedHome, "{");
    const malformed = dispatchFor({ HOME: malformedHome }, { ...command, cwd: tempDir() });
    expect(malformed).toMatchObject({
      enabled: false,
      error: expect.stringContaining("invalid JSON"),
    });
  });

  test("refuses a DISPATCH_URL that is not an http or https URL, and a DISPATCH_TOKEN that cannot be sent", () => {
    for (const url of ["not-a-url", "localhost:8766", "ftp://dispatch.test"]) {
      expect(
        dispatchFor(
          { HOME: tempDir(), DISPATCH_URL: url, DISPATCH_TOKEN: "token" },
          { ...command, cwd: tempDir() }
        )
      ).toEqual({
        enabled: false,
        notice: null,
        error: "DISPATCH_URL must be an http or https URL",
      });
    }
    expect(
      dispatchFor(
        { HOME: tempDir(), DISPATCH_URL: "http://dispatch.test", DISPATCH_TOKEN: "S3CRET\nnotes" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: "DISPATCH_TOKEN holds whitespace or a control character",
    });
  });

  // A plugin passes its whole environment, the dispatch command drops empty variables: both read
  // an empty one as unset, so the command reads what its plugin read.
  test("an empty DISPATCH_URL, DISPATCH_TOKEN or DISPATCH_TOKEN_FILE reads as unset", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "file-token" },
    });
    const fromFile = {
      enabled: true,
      url: "https://dispatch.example",
      token: "file-token",
    } as const;
    expect(
      dispatchFor(
        { HOME: home, DISPATCH_URL: "", DISPATCH_TOKEN: "", DISPATCH_TOKEN_FILE: "" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual(fromFile);
    // A checkout's dispatch.enabled: false holds against an empty DISPATCH_URL.
    const cwd = tempDir();
    mkdirSync(path.join(cwd, ".envoy"));
    const checkout = path.join(cwd, ".envoy", "envoy.json");
    writeFileSync(checkout, JSON.stringify({ dispatch: { enabled: false } }));
    expect(dispatchFor({ HOME: home, DISPATCH_URL: "" }, { ...command, cwd })).toEqual({
      enabled: false,
      notice: `${checkout} sets dispatch.enabled to false: Dispatch is off`,
      error: null,
    });
  });

  test("resolves the token from DISPATCH_TOKEN_FILE ahead of DISPATCH_TOKEN and dispatch.token", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: "file-token" },
    });
    const tokenFile = path.join(tempDir(), "dispatch-token");
    writeFileSync(tokenFile, "pointer-token\n");

    expect(
      dispatchFor(
        {
          HOME: home,
          DISPATCH_URL: "http://override.test",
          DISPATCH_TOKEN: "environment-token",
          DISPATCH_TOKEN_FILE: tokenFile,
        },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "http://override.test", token: "pointer-token" });
  });

  test("disables Dispatch naming DISPATCH_TOKEN_FILE and its path when the file is unreadable or empty, without falling back", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: "file-token" },
    });
    const missing = path.join(tempDir(), "missing");
    const unreadable = dispatchFor(
      { HOME: home, DISPATCH_TOKEN: "environment-token", DISPATCH_TOKEN_FILE: missing },
      { ...command, cwd: tempDir() }
    );
    expect(unreadable).toMatchObject({
      enabled: false,
      error: expect.stringContaining(
        `DISPATCH_TOKEN_FILE names ${missing}, which could not be read`
      ),
    });

    const empty = path.join(tempDir(), "empty");
    writeFileSync(empty, "  \n");
    expect(
      dispatchFor({ HOME: home, DISPATCH_TOKEN_FILE: empty }, { ...command, cwd: tempDir() })
    ).toEqual({
      enabled: false,
      notice: null,
      error: `DISPATCH_TOKEN_FILE names ${empty}, which is empty`,
    });
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

    expect(dispatchFor({ HOME: home }, { ...command, cwd })).toEqual({
      enabled: false,
      notice: null,
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
      dispatchFor({ HOME: home, DISPATCH_TOKEN: "environment-token" }, { ...command, cwd })
    ).toEqual({ enabled: true, url: "https://repo.internal.example", token: "repo-token" });
  });

  test("compares DISPATCH_URL with the server of the chosen file's token; a checkout's server and token are not used beside it", () => {
    const home = tempDir();
    const user = writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "user-token" },
    });
    const cwd = tempDir();
    mkdirSync(path.join(cwd, ".envoy"), { recursive: true });
    const checkout = path.join(cwd, ".envoy", "envoy.json");
    writeFileSync(
      checkout,
      JSON.stringify({ dispatch: { serverUrl: "https://staging.internal.example" } })
    );
    const env = { HOME: home, DISPATCH_URL: "https://staging.internal.example" };
    const refusal = {
      enabled: false,
      notice: null,
      error: `DISPATCH_URL names https://staging.internal.example, but ${user}'s dispatch.token is for https://dispatch.example; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
    } as const;
    expect(dispatchFor(env, { ...command, cwd })).toEqual(refusal);

    // A checkout's own token goes nowhere DISPATCH_URL names, its own server included.
    writeFileSync(
      checkout,
      JSON.stringify({
        dispatch: { serverUrl: "https://staging.internal.example", token: "repo-token" },
      })
    );
    expect(dispatchFor(env, { ...command, cwd })).toEqual(refusal);
  });

  test("a relative XDG_CONFIG_HOME refuses nothing when the harness names its own directory", () => {
    expect(
      dispatchFor(
        {
          HOME: tempDir(),
          XDG_CONFIG_HOME: "relative/config",
          CLAUDE_CONFIG_DIR: tempDir(),
          DISPATCH_URL: "https://dispatch.example",
          DISPATCH_TOKEN: "environment-token",
        },
        { harness: "claude", cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "https://dispatch.example", token: "environment-token" });
  });

  test("a file location that cannot be worked out is an error", () => {
    expect(
      dispatchFor(
        { HOME: tempDir(), ENVOY_CONFIG_FILE: "envoy.json" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: "ENVOY_CONFIG_FILE is envoy.json, which is not an absolute path",
    });
  });
});

// The one "is Dispatch configured" check every host shares: a configuration that resolves is
// returned with its URL and token, none is null, and a broken file throws rather than reading as
// unconfigured.
describe("activeDispatchSettings", () => {
  test("returns the resolved URL and token, null when nothing is configured, and throws on a broken file", () => {
    expect(
      activeDispatchSettings(
        dispatchFor(
          { HOME: tempDir(), DISPATCH_URL: "http://dispatch.test/", DISPATCH_TOKEN: "env-token" },
          { ...command, cwd: tempDir() }
        )
      )
    ).toEqual({ enabled: true, url: "http://dispatch.test", token: "env-token" });
    expect(
      activeDispatchSettings(dispatchFor({ HOME: tempDir() }, { ...command, cwd: tempDir() }))
    ).toBeNull();
    const home = tempDir();
    writeUserConfig(home, "{not json");
    expect(() =>
      activeDispatchSettings(dispatchFor({ HOME: home }, { ...command, cwd: tempDir() }))
    ).toThrow(/^dispatch config: /);
  });
});
