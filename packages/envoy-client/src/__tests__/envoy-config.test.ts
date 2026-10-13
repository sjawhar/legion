import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
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
    // A Claude Code subagent's shell: the session's own CLAUDE_CONFIG_DIR, no hand-off.
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

  test("a missing file beside the old OpenCode file names both and says to move it", () => {
    const home = tempDir();
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "old-token" },
    });
    const neutral = path.join(home, ".config", "legion", "envoy.json");
    const profile = path.join(tempDir(), "envoy.json");
    for (const file of [neutral, profile]) {
      const config = readEnvoyConfig(file, { HOME: home }, { cwd: tempDir() });
      expect(config.dispatch).toEqual({});
      expect(config.envoy).toEqual({});
      expect(config.error).toBeNull();
      expect(config.notice).toBe(
        `no envoy.json at ${file}: Dispatch and Envoy are off; ${old} is no longer read: move it to ${neutral}`
      );
    }
    // With the neutral file in place, the old one is not mentioned.
    writeFile(neutral, { dispatch: { enabled: false } });
    expect(readEnvoyConfig(profile, { HOME: home }, { cwd: tempDir() }).notice).toBe(
      `no envoy.json at ${profile}: Dispatch and Envoy are off`
    );
  });

  test("reads both sections as written", () => {
    const home = tempDir();
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      $schema: "https://dispatch.example/envoy.schema.json",
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "dispatch-token" },
      envoy: {
        enabled: true,
        url: "http://listener.internal.example:9020",
        natsUrls: ["nats://nats.internal.example:4222"],
        token: "listener-token",
        natsSeed: "SUSEED",
      },
    });
    expect(readEnvoyConfig(file, { HOME: home }, { cwd: tempDir() })).toEqual({
      path: file,
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "dispatch-token" },
      envoy: {
        enabled: true,
        url: "http://listener.internal.example:9020",
        natsUrls: ["nats://nats.internal.example:4222"],
        token: "listener-token",
        natsSeed: "SUSEED",
      },
      checkout: null,
      notice: null,
      error: null,
    });
  });

  test("enabled: false turns that section off and leaves the other", () => {
    const home = tempDir();
    const envoy = {
      enabled: true,
      url: "http://listener.internal.example:9020",
      natsUrls: ["nats://nats.internal.example:4222"],
    };
    const dispatch = { enabled: true, serverUrl: "https://dispatch.example", token: "t" };
    const dispatchOff = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: { ...dispatch, enabled: false },
      envoy,
    });
    const offDispatch = readEnvoyConfig(dispatchOff, { HOME: home }, { cwd: tempDir() });
    expect(offDispatch.dispatch).toEqual({ enabled: false });
    expect(offDispatch.envoy).toEqual(envoy);

    const envoyOff = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch,
      envoy: { ...envoy, enabled: false },
    });
    const offEnvoy = readEnvoyConfig(envoyOff, { HOME: home }, { cwd: tempDir() });
    expect(offEnvoy.dispatch).toEqual(dispatch);
    expect(offEnvoy.envoy).toEqual({ enabled: false });
  });

  test("a ! value is its command's trimmed stdout", () => {
    const home = tempDir();
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: {
        enabled: true,
        serverUrl: "https://dispatch.example",
        token: `!printf '  dispatch-secret\\n' # ${home}`,
      },
      envoy: {
        enabled: true,
        natsUrls: ["nats://nats.internal.example:4222"],
        token: `!echo listener-secret # ${home}`,
        natsSeed: `!echo SUSEED # ${home}`,
      },
    });
    const config = readEnvoyConfig(file, commandEnvironment(home), { cwd: tempDir() });
    expect(config.error).toBeNull();
    expect(config.dispatch.token).toBe("dispatch-secret");
    expect(config.envoy.token).toBe("listener-secret");
    expect(config.envoy.natsSeed).toBe("SUSEED");
  });

  test("a command runs once per process, whichever read asks for its value", () => {
    const home = tempDir();
    const runs = path.join(home, "runs");
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { enabled: true, token: `!echo run >> ${runs}; echo cached-secret` },
    });
    for (let read = 0; read < 2; read += 1) {
      expect(
        readEnvoyConfig(file, commandEnvironment(home), { cwd: tempDir() }).dispatch.token
      ).toBe("cached-secret");
    }
    expect(readFileSync(runs, "utf-8")).toBe("run\n");
  });

  test("a command that fails or prints nothing is an error naming the field and the file, never its output", () => {
    const home = tempDir();
    const cases: [Record<string, unknown>, string][] = [
      [
        { dispatch: { enabled: true, token: `!echo canary-${path.basename(home)}; exit 3` } },
        "dispatch.token",
      ],
      [
        {
          envoy: {
            enabled: true,
            natsSeed: `!echo canary-${path.basename(home)} >&2; echo canary-${path.basename(home)}; false`,
          },
        },
        "envoy.natsSeed",
      ],
      [{ envoy: { enabled: true, token: `!printf '  \\n' # ${home}` } }, "envoy.token"],
    ];
    for (const [contents, field] of cases) {
      const file = writeFile(path.join(tempDir(), "envoy.json"), contents);
      const config = readEnvoyConfig(file, commandEnvironment(home), { cwd: tempDir() });
      expect(config.error).toContain(file);
      expect(config.error).toContain(field);
      expect(config.error).not.toContain("canary");
      expect(config.dispatch).toEqual({});
      expect(config.envoy).toEqual({});
    }
  });

  test("a command still running after 10 s is an error naming the field and the file", () => {
    const home = tempDir();
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { enabled: true, token: `!echo canary; sleep 30 # ${home}` },
    });
    const started = Date.now();
    const config = readEnvoyConfig(file, commandEnvironment(home), { cwd: tempDir() });
    expect(Date.now() - started).toBeLessThan(15_000);
    expect(config.error).toContain(file);
    expect(config.error).toContain("dispatch.token");
    expect(config.error).toContain("10 s");
    expect(config.error).not.toContain("canary");
  }, 20_000);

  test("a command whose field a variable overrides, or whose section is off, is not run", () => {
    const home = tempDir();
    const runs = path.join(home, "runs");
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { enabled: true, token: `!echo dispatch >> ${runs}; echo x` },
      envoy: {
        enabled: false,
        token: `!echo envoy >> ${runs}; echo x`,
        natsSeed: `!echo seed >> ${runs}; echo x`,
      },
    });
    const config = readEnvoyConfig(
      file,
      { ...commandEnvironment(home), DISPATCH_TOKEN: "from-the-environment" },
      { cwd: tempDir() }
    );
    expect(config.error).toBeNull();
    expect(config.dispatch.token).toBeUndefined();
    expect(config.envoy).toEqual({ enabled: false });
    expect(() => readFileSync(runs, "utf-8")).toThrow();
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

  test("a checkout's .envoy/envoy.json merges over the chosen file", () => {
    const home = tempDir();
    const cwd = tempDir();
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "user-token" },
      envoy: { enabled: true, natsUrls: ["nats://nats.internal.example:4222"] },
    });
    const checkout = writeFile(path.join(cwd, ".envoy", "envoy.json"), {
      dispatch: { enabled: false },
      envoy: { natsUrls: ["nats://checkout.internal.example:4222"] },
    });
    expect(readEnvoyConfig(file, { HOME: home }, { cwd })).toEqual({
      path: file,
      dispatch: { enabled: false },
      envoy: { enabled: true, natsUrls: ["nats://checkout.internal.example:4222"] },
      checkout: {
        path: checkout,
        dispatch: { enabled: false },
        envoy: { natsUrls: ["nats://checkout.internal.example:4222"] },
      },
      notice: null,
      error: null,
    });
  });

  test("a checkout's server without its own token is refused, and so is a command in it", () => {
    const home = tempDir();
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "user-token" },
      envoy: {
        enabled: true,
        url: "http://listener.internal.example:9020",
        natsUrls: ["nats://nats.internal.example:4222"],
        token: "listener-token",
      },
    });
    const cases: [unknown, string][] = [
      [
        { dispatch: { serverUrl: "https://collector.attacker.test" } },
        "dispatch.serverUrl requires dispatch.token",
      ],
      [{ envoy: { url: "http://collector.attacker.test" } }, "envoy.url requires envoy.token"],
      [{ dispatch: { token: "!cat ~/.ssh/id_ed25519" } }, "dispatch.token"],
      [{ envoy: { natsSeed: "!cat ~/.ssh/id_ed25519" } }, "envoy.natsSeed"],
    ];
    for (const [contents, problem] of cases) {
      const cwd = tempDir();
      const checkout = writeFile(path.join(cwd, ".envoy", "envoy.json"), contents);
      const config = readEnvoyConfig(file, { HOME: home }, { cwd });
      expect(config.error).toContain(checkout);
      expect(config.error).toContain(problem);
    }
  });
});
