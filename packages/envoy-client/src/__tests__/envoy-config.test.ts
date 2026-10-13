import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { envoySettings } from "../defaults";
import { dispatchSettings } from "../dispatch-config";
import { envoyConfigPath, readEnvoyConfig } from "../envoy-config";

function tempDir(): string {
  return mkdtempSync(path.join(os.tmpdir(), "envoy-config-"));
}

function writeFile(file: string, contents: unknown): string {
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, typeof contents === "string" ? contents : JSON.stringify(contents));
  return file;
}

/** The environment a read runs a `!` command under: a temporary HOME and the caller's PATH. */
function commandEnvironment(home: string): Record<string, string | undefined> {
  return { HOME: home, PATH: process.env.PATH };
}

describe("envoyConfigPath", () => {
  const home = "/home/someone";
  const neutral = "/home/someone/.config/legion/envoy.json";

  test("omp reads a named profile's directory, and never Claude Code's", () => {
    const agent = "/profiles/work/agent";
    expect(
      envoyConfigPath("omp", { HOME: home, OMP_PROFILE: "work", PI_CODING_AGENT_DIR: agent })
    ).toBe("/profiles/work/agent/envoy.json");
    for (const profile of [undefined, "", "default"]) {
      expect(
        envoyConfigPath("omp", { HOME: home, OMP_PROFILE: profile, PI_CODING_AGENT_DIR: agent })
      ).toBe(neutral);
    }
    expect(
      envoyConfigPath("omp", {
        HOME: home,
        OMP_PROFILE: "work",
        PI_CODING_AGENT_DIR: agent,
        CLAUDE_CONFIG_DIR: "/claude",
      })
    ).toBe("/profiles/work/agent/envoy.json");
    expect(envoyConfigPath("omp", { HOME: home, CLAUDE_CONFIG_DIR: "/claude" })).toBe(neutral);
  });

  test("claude reads CLAUDE_CONFIG_DIR and opencode reads OPENCODE_CONFIG_DIR, each ignoring the others", () => {
    const all = {
      HOME: home,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: "/profiles/work/agent",
      CLAUDE_CONFIG_DIR: "/claude",
      OPENCODE_CONFIG_DIR: "/opencode",
    };
    expect(envoyConfigPath("claude", all)).toBe("/claude/envoy.json");
    expect(envoyConfigPath("opencode", all)).toBe("/opencode/envoy.json");
    expect(envoyConfigPath("claude", { ...all, CLAUDE_CONFIG_DIR: undefined })).toBe(neutral);
    expect(envoyConfigPath("opencode", { ...all, OPENCODE_CONFIG_DIR: undefined })).toBe(neutral);
  });

  test("a plugin whose harness names no directory reads ENVOY_CONFIG_FILE, else the neutral file", () => {
    for (const harness of ["omp", "claude", "opencode"] as const) {
      expect(
        envoyConfigPath(harness, { HOME: home, ENVOY_CONFIG_FILE: "/handed/envoy.json" })
      ).toBe("/handed/envoy.json");
      expect(envoyConfigPath(harness, { HOME: home, XDG_CONFIG_HOME: "/xdg" })).toBe(
        "/xdg/legion/envoy.json"
      );
      expect(envoyConfigPath(harness, { HOME: home })).toBe(neutral);
    }
    // The harness's own directory wins over a file an outer session handed down.
    expect(
      envoyConfigPath("claude", {
        HOME: home,
        CLAUDE_CONFIG_DIR: "/claude",
        ENVOY_CONFIG_FILE: "/handed/envoy.json",
      })
    ).toBe("/claude/envoy.json");
  });

  test("a relative XDG_CONFIG_HOME is refused naming it; an empty one reads as unset", () => {
    for (const harness of ["omp", "claude", "opencode", "command"] as const) {
      expect(() =>
        envoyConfigPath(harness, { HOME: home, XDG_CONFIG_HOME: "relative/config" })
      ).toThrow("XDG_CONFIG_HOME is relative/config, which is not an absolute path");
      expect(envoyConfigPath(harness, { HOME: home, XDG_CONFIG_HOME: "" })).toBe(neutral);
    }
    // A harness directory or a handed file is the answer, so the neutral file is never worked out.
    expect(
      envoyConfigPath("claude", {
        HOME: home,
        XDG_CONFIG_HOME: "relative/config",
        CLAUDE_CONFIG_DIR: "/claude",
      })
    ).toBe("/claude/envoy.json");
  });

  test("a command reads ENVOY_CONFIG_FILE, else the omp, Claude Code and OpenCode rules in turn, else the neutral file", () => {
    const harnesses = {
      HOME: home,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: "/profiles/work/agent",
      CLAUDE_CONFIG_DIR: "/claude",
      OPENCODE_CONFIG_DIR: "/opencode",
    };
    expect(
      envoyConfigPath("command", { ...harnesses, ENVOY_CONFIG_FILE: "/handed/envoy.json" })
    ).toBe("/handed/envoy.json");
    expect(envoyConfigPath("command", harnesses)).toBe("/profiles/work/agent/envoy.json");
    expect(envoyConfigPath("command", { ...harnesses, OMP_PROFILE: "default" })).toBe(
      "/claude/envoy.json"
    );
    // A script in a Claude Code session's shell, handed no file: the session's CLAUDE_CONFIG_DIR.
    expect(envoyConfigPath("command", { HOME: home, CLAUDE_CONFIG_DIR: "/claude" })).toBe(
      "/claude/envoy.json"
    );
    expect(envoyConfigPath("command", { HOME: home, OPENCODE_CONFIG_DIR: "/opencode" })).toBe(
      "/opencode/envoy.json"
    );
    expect(envoyConfigPath("command", { HOME: home })).toBe(neutral);
  });
});

describe("readEnvoyConfig", () => {
  test("a missing file turns both sections off, with a notice naming it", () => {
    const home = tempDir();
    const file = path.join(home, ".config", "legion", "envoy.json");
    expect(readEnvoyConfig(file, { HOME: home }, { cwd: tempDir() })).toEqual({
      path: file,
      dispatch: {},
      envoy: {},
      checkout: null,
      notice: `no envoy.json at ${file}: Dispatch and Envoy are off`,
      error: null,
    });
  });

  test("a missing neutral file beside the old OpenCode file names both and says to move it", () => {
    const home = tempDir();
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "old-token" },
    });
    const neutral = path.join(home, ".config", "legion", "envoy.json");
    const config = readEnvoyConfig(neutral, { HOME: home }, { cwd: tempDir() });
    expect(config).toMatchObject({ dispatch: {}, envoy: {}, error: null });
    expect(config.notice).toBe(
      `no envoy.json at ${neutral}: Dispatch and Envoy are off; ${old} is no longer read: move it to ${neutral}`
    );
    // XDG_CONFIG_HOME set to $HOME/.config is the same file, and gets the same hint.
    const sameHome = { HOME: home, XDG_CONFIG_HOME: path.join(home, ".config") };
    expect(readEnvoyConfig(neutral, sameHome, { cwd: tempDir() }).notice).toBe(config.notice);
    // A neutral file under another XDG_CONFIG_HOME (a Legion pane's, under the daemon's state
    // directory) is not where the user's file goes, so the notice names it alone.
    const pane = { HOME: home, XDG_CONFIG_HOME: "/var/lib/legion/home/.config" };
    const paneFile = "/var/lib/legion/home/.config/legion/envoy.json";
    expect(readEnvoyConfig(paneFile, pane, { cwd: tempDir() }).notice).toBe(
      `no envoy.json at ${paneFile}: Dispatch and Envoy are off`
    );
    // Another chosen file is named alone, and the neutral path is not worked out for it, so a
    // relative XDG_CONFIG_HOME refuses nothing here.
    const profile = path.join(tempDir(), "envoy.json");
    for (const env of [{ HOME: home }, { HOME: home, XDG_CONFIG_HOME: "relative/config" }]) {
      expect(readEnvoyConfig(profile, env, { cwd: tempDir() })).toMatchObject({
        notice: `no envoy.json at ${profile}: Dispatch and Envoy are off`,
        error: null,
      });
    }
  });

  test("reads both sections as written, commands included, and runs none of them", () => {
    const home = tempDir();
    const runs = path.join(home, "runs");
    const written = {
      $schema: "https://dispatch.example/envoy.schema.json",
      dispatch: {
        enabled: true,
        serverUrl: "https://dispatch.example",
        token: `!echo dispatch >> ${runs}; echo x`,
      },
      envoy: {
        enabled: true,
        url: "http://listener.internal.example:9020",
        natsUrls: ["nats://nats.internal.example:4222"],
        token: `!echo envoy >> ${runs}; echo y`,
        natsSeed: `!echo seed >> ${runs}; echo z`,
      },
    };
    const file = writeFile(path.join(tempDir(), "envoy.json"), written);
    expect(readEnvoyConfig(file, commandEnvironment(home), { cwd: tempDir() })).toEqual({
      path: file,
      dispatch: written.dispatch,
      envoy: written.envoy,
      checkout: null,
      notice: null,
      error: null,
    });
    expect(existsSync(runs)).toBe(false);
  });

  test("keeps a checkout's .envoy/envoy.json beside the chosen file, its dispatch section alone", () => {
    const cwd = tempDir();
    const own = {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "user-token" },
      envoy: { enabled: true, natsUrls: ["nats://nats.internal.example:4222"] },
    };
    const file = writeFile(path.join(tempDir(), "envoy.json"), own);
    const checkout = writeFile(path.join(cwd, ".envoy", "envoy.json"), {
      dispatch: { enabled: false },
    });
    expect(readEnvoyConfig(file, { HOME: tempDir() }, { cwd })).toEqual({
      path: file,
      dispatch: own.dispatch,
      envoy: own.envoy,
      checkout: { path: checkout, dispatch: { enabled: false }, envoyError: null },
      notice: null,
      error: null,
    });

    // An envoy section there is not read, whatever it holds, and is named for Envoy alone.
    writeFile(checkout, {
      dispatch: { enabled: false },
      envoy: { natsUrl: "nats://checkout.internal.example:4222" },
    });
    expect(readEnvoyConfig(file, { HOME: tempDir() }, { cwd })).toMatchObject({
      checkout: {
        path: checkout,
        dispatch: { enabled: false },
        envoyError: `${checkout}: envoy is not read from a checkout's envoy.json; put it in ${file}`,
      },
      error: null,
    });
  });

  test("an unknown key in either section, or the top-level natsUrls, is refused naming the key", () => {
    const home = tempDir();
    const cases: [unknown, string][] = [
      [{ dispatch: { enabled: true, unexpected: true } }, "dispatch.unexpected"],
      [{ envoy: { enabled: true, natsUrl: "nats://nats.internal.example:4222" } }, "envoy.natsUrl"],
      [{ natsUrls: ["nats://nats.internal.example:4222"] }, "envoy.natsUrls"],
    ];
    for (const [contents, key] of cases) {
      const file = writeFile(path.join(tempDir(), "envoy.json"), contents);
      const config = readEnvoyConfig(file, { HOME: home }, { cwd: tempDir() });
      expect(config.error).toContain(file);
      expect(config.error).toContain(key);
    }
    const malformed = writeFile(path.join(tempDir(), "envoy.json"), "{");
    expect(readEnvoyConfig(malformed, { HOME: home }, { cwd: tempDir() }).error).toContain(
      "invalid JSON"
    );
  });
});

const dispatchOn = { enabled: true, serverUrl: "https://dispatch.example", token: "user-token" };
const envoyOn = {
  enabled: true,
  url: "http://listener.internal.example:9020",
  natsUrls: ["nats://nats.internal.example:4222"],
  token: "listener-token",
};

describe("each section of envoy.json", () => {
  test("enabled: false turns that section off and leaves the other", () => {
    const env = { HOME: tempDir() };
    const dispatchOff = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: { ...dispatchOn, enabled: false },
      envoy: envoyOn,
    });
    const offDispatch = readEnvoyConfig(dispatchOff, env, { cwd: tempDir() });
    expect(dispatchSettings(env, offDispatch)).toEqual({
      enabled: false,
      url: null,
      token: null,
      error: null,
    });
    expect(envoySettings(env, offDispatch)).toMatchObject({
      enabled: true,
      token: "listener-token",
    });

    const envoyOff = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: dispatchOn,
      envoy: { ...envoyOn, enabled: false },
    });
    const offEnvoy = readEnvoyConfig(envoyOff, env, { cwd: tempDir() });
    expect(dispatchSettings(env, offEnvoy)).toMatchObject({ enabled: true, token: "user-token" });
    expect(envoySettings(env, offEnvoy)).toMatchObject({ enabled: false, error: null });
  });
});

describe("a ! command in envoy.json", () => {
  test("is its command's trimmed stdout, run when its section is resolved", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!printf '  dispatch-secret\\n' # ${home}` },
      envoy: {
        ...envoyOn,
        token: `!echo listener-secret # ${home}`,
        natsSeed: `!echo SUSEED # ${home}`,
      },
    });
    const config = readEnvoyConfig(file, env, { cwd: tempDir() });
    expect(dispatchSettings(env, config)).toMatchObject({
      enabled: true,
      token: "dispatch-secret",
    });
    expect(envoySettings(env, config)).toMatchObject({
      enabled: true,
      token: "listener-secret",
      natsSeed: { value: "SUSEED", source: `envoy.natsSeed in ${file}` },
    });
  });

  test("runs once per process, its value or its failure kept for every later resolution", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const runs = path.join(home, "runs");
    const failures = path.join(home, "failures");
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!echo run >> ${runs}; echo cached-secret` },
      envoy: { ...envoyOn, token: `!echo run >> ${failures}; exit 1` },
    });
    for (let resolution = 0; resolution < 2; resolution += 1) {
      const config = readEnvoyConfig(file, env, { cwd: tempDir() });
      expect(dispatchSettings(env, config).token).toBe("cached-secret");
      expect(envoySettings(env, config)).toMatchObject({ enabled: false });
    }
    expect(readFileSync(runs, "utf-8")).toBe("run\n");
    expect(readFileSync(failures, "utf-8")).toBe("run\n");
  });

  test("that fails or prints nothing turns off only its own section, naming the field and the file, never its output", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const canary = `canary-${path.basename(home)}`;

    const dispatchFails = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!echo ${canary}; exit 3` },
      envoy: envoyOn,
    });
    const brokenDispatch = readEnvoyConfig(dispatchFails, env, { cwd: tempDir() });
    const dispatch = dispatchSettings(env, brokenDispatch);
    expect(dispatch).toMatchObject({ enabled: false, token: null });
    expect(dispatch.error).toContain(`${dispatchFails}: dispatch.token's command exited with`);
    expect(dispatch.error).not.toContain("canary");
    expect(envoySettings(env, brokenDispatch)).toMatchObject({ enabled: true });

    for (const [field, value] of [
      ["natsSeed", `!echo ${canary} >&2; echo ${canary}; false`],
      ["token", `!printf '  \\n' # ${home}`],
    ] as const) {
      const envoyFails = writeFile(path.join(tempDir(), "envoy.json"), {
        dispatch: dispatchOn,
        envoy: { ...envoyOn, [field]: value },
      });
      const brokenEnvoy = readEnvoyConfig(envoyFails, env, { cwd: tempDir() });
      const envoy = envoySettings(env, brokenEnvoy);
      if (envoy.enabled) throw new Error(`envoy.${field}'s failing command was accepted`);
      expect(envoy.error).toContain(`${envoyFails}: envoy.${field}'s command`);
      expect(envoy.error).not.toContain("canary");
      expect(dispatchSettings(env, brokenEnvoy)).toEqual({
        enabled: true,
        url: "https://dispatch.example",
        token: "user-token",
        error: null,
      });
    }
  });

  test("still running after 10 s is an error naming the field and the file", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!echo canary; sleep 30 # ${home}` },
    });
    const started = Date.now();
    const error = dispatchSettings(env, readEnvoyConfig(file, env, { cwd: tempDir() })).error;
    expect(Date.now() - started).toBeLessThan(15_000);
    expect(error).toBe(`${file}: dispatch.token's command did not finish within 10 s`);
  }, 20_000);

  test("whose field a variable overrides, or whose section is off, is not run", () => {
    const home = tempDir();
    const runs = path.join(home, "runs");
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!echo dispatch >> ${runs}; echo x` },
      envoy: {
        ...envoyOn,
        enabled: false,
        token: `!echo envoy >> ${runs}; echo x`,
        natsSeed: `!echo seed >> ${runs}; echo x`,
      },
    });
    const env = { ...commandEnvironment(home), DISPATCH_TOKEN: "from-the-environment" };
    const config = readEnvoyConfig(file, env, { cwd: tempDir() });
    expect(dispatchSettings(env, config)).toMatchObject({ token: "from-the-environment" });
    expect(envoySettings(env, config)).toMatchObject({ enabled: false, error: null });
    expect(existsSync(runs)).toBe(false);
  });
});

describe("a checkout's .envoy/envoy.json", () => {
  /** The chosen file holding both sections on, and a checkout holding `contents`. */
  function withCheckout(contents: unknown) {
    const cwd = tempDir();
    const user = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: dispatchOn,
      envoy: envoyOn,
    });
    const checkout = writeFile(path.join(cwd, ".envoy", "envoy.json"), contents);
    return { user, checkout, cwd };
  }

  test("merges its dispatch section over the chosen file, and Envoy is the chosen file's", () => {
    const env = { HOME: tempDir() };
    const repo = withCheckout({
      dispatch: { serverUrl: "https://repo.internal.example", token: "repo-token" },
    });
    const repoConfig = readEnvoyConfig(repo.user, env, { cwd: repo.cwd });
    expect(dispatchSettings(env, repoConfig)).toEqual({
      enabled: true,
      url: "https://repo.internal.example",
      token: "repo-token",
      error: null,
    });
    expect(envoySettings(env, repoConfig)).toMatchObject({
      enabled: true,
      url: "http://listener.internal.example:9020",
      natsUrls: ["nats://nats.internal.example:4222"],
      token: "listener-token",
    });

    const off = withCheckout({ dispatch: { enabled: false } });
    const offConfig = readEnvoyConfig(off.user, env, { cwd: off.cwd });
    expect(dispatchSettings(env, offConfig)).toMatchObject({ enabled: false, error: null });
    expect(envoySettings(env, offConfig)).toMatchObject({ enabled: true });
  });

  // A cloned repository must not choose where a session's deliveries come from, nor which servers
  // the user's NATS seed signs in to.
  test("an envoy section is refused for Envoy alone, whatever it holds, and runs nothing", () => {
    const home = tempDir();
    const runs = path.join(home, "runs");
    const seedFile = writeFile(path.join(home, "seed"), "SUENVSEED\n");
    const environments = [
      commandEnvironment(home),
      {
        ...commandEnvironment(home),
        ENVOY_NATS_URL: "nats://repo.internal.example:4222",
        ENVOY_TOKEN: "environment-token",
        NATS_NKEY_SEED_FILE: seedFile,
      },
    ];
    for (const envoy of [
      { natsUrls: ["nats://repo.internal.example:4222"] },
      { url: "http://repo.internal.example:9020", token: "repo-token" },
      { natsSeed: `!echo seed >> ${runs}; echo SUREPOSEED` },
      { enabled: false },
      { enabled: true, natsUrl: "nats://repo.internal.example:4222" },
    ]) {
      const { user, checkout, cwd } = withCheckout({ envoy });
      for (const env of environments) {
        const config = readEnvoyConfig(user, env, { cwd });
        expect(envoySettings(env, config)).toEqual({
          enabled: false,
          notice: null,
          error: `${checkout}: envoy is not read from a checkout's envoy.json; put it in ${user}`,
        });
        expect(dispatchSettings(env, config)).toEqual({
          enabled: true,
          url: "https://dispatch.example",
          token: "user-token",
          error: null,
        });
      }
    }
    expect(existsSync(runs)).toBe(false);
  });

  test("a server without its own token is refused", () => {
    const env = { HOME: tempDir() };
    const server = withCheckout({ dispatch: { serverUrl: "https://collector.attacker.test" } });
    const serverConfig = readEnvoyConfig(server.user, env, { cwd: server.cwd });
    expect(dispatchSettings(env, serverConfig)).toMatchObject({
      enabled: false,
      token: null,
      error: `${server.checkout}: dispatch.serverUrl requires dispatch.token in the same file`,
    });
    expect(envoySettings(env, serverConfig)).toMatchObject({ enabled: true });
  });

  test("a server DISPATCH_URL replaces is not refused, and the variables are used", () => {
    const server = withCheckout({ dispatch: { serverUrl: "https://collector.attacker.test" } });
    const dispatchEnv = {
      HOME: tempDir(),
      DISPATCH_URL: "https://other.internal.example",
      DISPATCH_TOKEN: "environment-token",
    };
    const serverConfig = readEnvoyConfig(server.user, dispatchEnv, { cwd: server.cwd });
    expect(dispatchSettings(dispatchEnv, serverConfig)).toEqual({
      enabled: true,
      url: "https://other.internal.example",
      token: "environment-token",
      error: null,
    });
    expect(envoySettings(dispatchEnv, serverConfig)).toMatchObject({ enabled: true });
  });

  test("a command in it is refused, for Dispatch only", () => {
    const env = { HOME: tempDir() };
    const token = withCheckout({ dispatch: { token: "!cat ~/.ssh/id_ed25519" } });
    const tokenConfig = readEnvoyConfig(token.user, env, { cwd: token.cwd });
    expect(dispatchSettings(env, tokenConfig)).toMatchObject({
      enabled: false,
      token: null,
      error: `${token.checkout}: dispatch.token is a command, which a checkout's envoy.json cannot run`,
    });
    expect(envoySettings(env, tokenConfig)).toMatchObject({ enabled: true });
  });

  test("its dispatch.enabled: false yields to DISPATCH_URL", () => {
    const tokenFile = writeFile(path.join(tempDir(), "dispatch-token"), "daemon-token\n");
    const off = withCheckout({ dispatch: { enabled: false } });
    const quiet = { HOME: tempDir() };
    const quietConfig = readEnvoyConfig(off.user, quiet, { cwd: off.cwd });
    expect(dispatchSettings(quiet, quietConfig)).toMatchObject({ enabled: false, error: null });

    const daemon = {
      HOME: tempDir(),
      DISPATCH_URL: "https://daemon.internal.example",
      DISPATCH_TOKEN_FILE: tokenFile,
    };
    const daemonConfig = readEnvoyConfig(off.user, daemon, { cwd: off.cwd });
    expect(dispatchSettings(daemon, daemonConfig)).toEqual({
      enabled: true,
      url: "https://daemon.internal.example",
      token: "daemon-token",
      error: null,
    });
  });
});
