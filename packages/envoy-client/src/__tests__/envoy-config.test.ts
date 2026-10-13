import { describe, expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { envoySettings } from "../defaults";
import { dispatchSettings } from "../dispatch-config";
import {
  type EnvoyConfig,
  type EnvoyHarness,
  envoyConfigPath,
  missingConfigNotice,
  readEnvoyConfig,
} from "../envoy-config";

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

/** The error a read failed with, or a test failure naming what it read instead. */
function errorOf(config: EnvoyConfig): string {
  if (config.kind !== "failed") throw new Error(`read ${config.path} without an error`);
  return config.error;
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
    // A named profile with no directory named reads what a profile-less session does.
    expect(envoyConfigPath("omp", { HOME: home, OMP_PROFILE: "work" })).toBe(neutral);
    expect(
      envoyConfigPath("omp", {
        HOME: home,
        OMP_PROFILE: "work",
        ENVOY_CONFIG_FILE: "/handed/envoy.json",
      })
    ).toBe("/handed/envoy.json");
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

  // A relative path would resolve against whatever directory each process starts in, a cloned
  // repository's included, whose envoy.json would then be the chosen file.
  test("a relative ENVOY_CONFIG_FILE or harness directory is refused naming it, when the rule uses it", () => {
    const cases: [EnvoyHarness, Record<string, string>, string][] = [
      ["command", { ENVOY_CONFIG_FILE: "envoy.json" }, "ENVOY_CONFIG_FILE is envoy.json"],
      ["omp", { ENVOY_CONFIG_FILE: "rel/envoy.json" }, "ENVOY_CONFIG_FILE is rel/envoy.json"],
      [
        "omp",
        { OMP_PROFILE: "work", PI_CODING_AGENT_DIR: "agent" },
        "PI_CODING_AGENT_DIR is agent",
      ],
      ["claude", { CLAUDE_CONFIG_DIR: "~/.claude" }, "CLAUDE_CONFIG_DIR is ~/.claude"],
      ["opencode", { OPENCODE_CONFIG_DIR: "config" }, "OPENCODE_CONFIG_DIR is config"],
      ["command", { CLAUDE_CONFIG_DIR: "claude" }, "CLAUDE_CONFIG_DIR is claude"],
    ];
    for (const [harness, variables, refusal] of cases) {
      expect(() => envoyConfigPath(harness, { HOME: home, ...variables })).toThrow(
        `${refusal}, which is not an absolute path`
      );
    }
    // A variable the rule never reaches is not refused.
    expect(
      envoyConfigPath("claude", {
        HOME: home,
        CLAUDE_CONFIG_DIR: "/claude",
        ENVOY_CONFIG_FILE: "x",
      })
    ).toBe("/claude/envoy.json");
    expect(envoyConfigPath("omp", { HOME: home, CLAUDE_CONFIG_DIR: "relative" })).toBe(neutral);
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

/** The notice a read's missing chosen file gives for `off`, or null when the file is there. */
function missingNotice(config: EnvoyConfig, off: string): string | null {
  if (config.kind !== "read" || config.missing === null) return null;
  return missingConfigNotice(config.path, config.missing, off);
}

describe("readEnvoyConfig", () => {
  test("a missing file reads as empty, its notice naming it", () => {
    const home = tempDir();
    const file = path.join(home, ".config", "legion", "envoy.json");
    const config = readEnvoyConfig(file, { HOME: home }, { cwd: tempDir() });
    expect(config).toEqual({
      kind: "read",
      path: file,
      dispatch: {},
      envoy: {},
      checkout: null,
      missing: { link: null, hint: null },
    });
    expect(missingNotice(config, "Dispatch and Envoy are off")).toBe(
      `no envoy.json at ${file}: Dispatch and Envoy are off`
    );
  });

  test("a missing file that is a dangling link names the file it points to", () => {
    const directory = tempDir();
    const target = path.join(directory, "gone", "envoy.json");
    const file = path.join(directory, "envoy.json");
    symlinkSync(target, file);
    const config = readEnvoyConfig(file, { HOME: tempDir() }, { cwd: tempDir() });
    expect(missingNotice(config, "Dispatch is off")).toBe(
      `no envoy.json at ${file} (a link to ${target}, which does not exist): Dispatch is off`
    );
  });

  test("a missing neutral file beside the old OpenCode file names both and says to move it", () => {
    const home = tempDir();
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "old-token" },
    });
    const neutral = path.join(home, ".config", "legion", "envoy.json");
    const config = readEnvoyConfig(neutral, { HOME: home }, { cwd: tempDir() });
    expect(config).toMatchObject({ kind: "read", dispatch: {}, envoy: {} });
    const notice = `no envoy.json at ${neutral}: Dispatch and Envoy are off; ${old} is no longer read: move it to ${neutral}`;
    expect(missingNotice(config, "Dispatch and Envoy are off")).toBe(notice);
    // XDG_CONFIG_HOME set to $HOME/.config is the same file, and gets the same hint.
    const sameHome = { HOME: home, XDG_CONFIG_HOME: path.join(home, ".config") };
    expect(
      missingNotice(
        readEnvoyConfig(neutral, sameHome, { cwd: tempDir() }),
        "Dispatch and Envoy are off"
      )
    ).toBe(notice);
    // A neutral file under another XDG_CONFIG_HOME (a Legion pane's, under the daemon's state
    // directory) is not where the user's file goes, so the notice names it alone.
    const pane = { HOME: home, XDG_CONFIG_HOME: "/var/lib/legion/home/.config" };
    const paneFile = "/var/lib/legion/home/.config/legion/envoy.json";
    expect(
      missingNotice(
        readEnvoyConfig(paneFile, pane, { cwd: tempDir() }),
        "Dispatch and Envoy are off"
      )
    ).toBe(`no envoy.json at ${paneFile}: Dispatch and Envoy are off`);
    // Another chosen file is named alone, and the neutral path is not worked out for it, so a
    // relative XDG_CONFIG_HOME refuses nothing here.
    const profile = path.join(tempDir(), "envoy.json");
    for (const env of [{ HOME: home }, { HOME: home, XDG_CONFIG_HOME: "relative/config" }]) {
      expect(
        missingNotice(
          readEnvoyConfig(profile, env, { cwd: tempDir() }),
          "Dispatch and Envoy are off"
        )
      ).toBe(`no envoy.json at ${profile}: Dispatch and Envoy are off`);
    }
  });

  test("a chosen file that cannot be read is an error, never a missing file; a path through a file is missing", () => {
    const directory = tempDir();
    const isDirectory = path.join(directory, "envoy.json");
    mkdirSync(isDirectory);
    expect(errorOf(readEnvoyConfig(isDirectory, { HOME: directory }, { cwd: tempDir() }))).toMatch(
      `${isDirectory}: could not be read`
    );
    const notADirectory = writeFile(path.join(tempDir(), "claude-config"), "a file");
    const throughFile = path.join(notADirectory, "envoy.json");
    expect(readEnvoyConfig(throughFile, { HOME: directory }, { cwd: tempDir() })).toMatchObject({
      kind: "read",
      missing: { link: null, hint: null },
    });
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
      kind: "read",
      path: file,
      dispatch: written.dispatch,
      envoy: written.envoy,
      checkout: null,
      missing: null,
    });
    expect(existsSync(runs)).toBe(false);
  });

  test("reads envoy.natsUrls trimmed, and refuses an empty one", () => {
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      envoy: {
        natsUrls: [" nats://one.internal.example:4222 ", "nats://two.internal.example:4222\n"],
      },
    });
    expect(readEnvoyConfig(file, { HOME: tempDir() }, { cwd: tempDir() })).toMatchObject({
      envoy: { natsUrls: ["nats://one.internal.example:4222", "nats://two.internal.example:4222"] },
    });
    const empty = writeFile(path.join(tempDir(), "envoy.json"), {
      envoy: { natsUrls: ["nats://one.internal.example:4222", "  "] },
    });
    expect(errorOf(readEnvoyConfig(empty, { HOME: tempDir() }, { cwd: tempDir() }))).toBe(
      `${empty}: envoy.natsUrls.1: must not be empty`
    );
  });

  test("refuses an address that is not an http or https URL", () => {
    for (const [section, key] of [
      ["dispatch", "serverUrl"],
      ["envoy", "url"],
    ] as const) {
      for (const address of ["localhost:8766", "mailto:a@b.example", "ftp://a.example", "/x"]) {
        const file = writeFile(path.join(tempDir(), "envoy.json"), {
          [section]: { [key]: address },
        });
        expect(errorOf(readEnvoyConfig(file, { HOME: tempDir() }, { cwd: tempDir() }))).toBe(
          `${file}: ${section}.${key}: must be an http or https URL`
        );
      }
    }
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
      kind: "read",
      path: file,
      dispatch: own.dispatch,
      envoy: own.envoy,
      checkout: { kind: "valid", path: checkout, dispatch: { enabled: false }, envoyError: null },
      missing: null,
    });

    // An envoy section there is not read, whatever it holds, and is named.
    writeFile(checkout, {
      dispatch: { enabled: false },
      envoy: { natsUrl: "nats://checkout.internal.example:4222" },
    });
    expect(readEnvoyConfig(file, { HOME: tempDir() }, { cwd })).toMatchObject({
      kind: "read",
      checkout: {
        kind: "valid",
        path: checkout,
        dispatch: { enabled: false },
        envoyError: `${checkout}: envoy is not read from a checkout's envoy.json; put it in ${file}`,
      },
    });
  });

  test("a checkout's file that cannot be used is the checkout's failure, not the chosen file's", () => {
    const cwd = tempDir();
    const file = writeFile(path.join(tempDir(), "envoy.json"), { dispatch: dispatchOn });
    const checkout = writeFile(path.join(cwd, ".envoy", "envoy.json"), {
      natsUrls: ["nats://repo.internal.example:4222"],
    });
    expect(readEnvoyConfig(file, { HOME: tempDir() }, { cwd })).toEqual({
      kind: "read",
      path: file,
      dispatch: dispatchOn,
      envoy: {},
      checkout: {
        kind: "invalid",
        path: checkout,
        error: `${checkout}: the top-level natsUrls is not read, and a checkout's envoy.json holds only a dispatch section`,
      },
      missing: null,
    });
  });

  test("the old .opencode/envoy.json in a checkout is not read", () => {
    const cwd = tempDir();
    writeFile(path.join(cwd, ".opencode", "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "https://old.internal.example", token: "t" },
    });
    const file = writeFile(path.join(tempDir(), "envoy.json"), {});
    expect(readEnvoyConfig(file, { HOME: tempDir() }, { cwd })).toMatchObject({ checkout: null });
  });

  test("a chosen file that is the checkout's own is read once, as the chosen file", () => {
    const cwd = tempDir();
    const own = writeFile(path.join(cwd, ".envoy", "envoy.json"), { envoy: envoyOn });
    const config = readEnvoyConfig(own, { HOME: tempDir() }, { cwd });
    expect(config).toMatchObject({ kind: "read", envoy: envoyOn, checkout: null });
    expect(envoySettings({}, config)).toMatchObject({ enabled: true, token: "listener-token" });
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
      const error = errorOf(readEnvoyConfig(file, { HOME: home }, { cwd: tempDir() }));
      expect(error).toContain(file);
      expect(error).toContain(key);
    }
  });

  // The parser's own message quotes the token it stopped at, which can be an unquoted secret.
  test("malformed JSON is refused by its position, quoting none of the file, chosen or checkout", () => {
    const cwd = tempDir();
    const chosen = writeFile(
      path.join(tempDir(), "envoy.json"),
      '{\n  "dispatch": {"token": hunter2secret}\n}'
    );
    expect(errorOf(readEnvoyConfig(chosen, { HOME: tempDir() }, { cwd }))).toBe(
      `${chosen}: invalid JSON at line 2, column 25`
    );
    const file = writeFile(path.join(tempDir(), "envoy.json"), {});
    const checkout = writeFile(
      path.join(cwd, ".envoy", "envoy.json"),
      '{"dispatch": {"token": SUSEED}}'
    );
    expect(readEnvoyConfig(file, { HOME: tempDir() }, { cwd })).toMatchObject({
      checkout: { kind: "invalid", error: `${checkout}: invalid JSON at line 1, column 24` },
    });
    expect(
      errorOf(readEnvoyConfig(writeFile(path.join(tempDir(), "envoy.json"), "{"), {}, { cwd }))
    ).toMatch(/invalid JSON at line 1, column 2$/);
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
      notice: `${dispatchOff} sets dispatch.enabled to false: Dispatch is off`,
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
      expect(dispatchSettings(env, config)).toMatchObject({ token: "cached-secret" });
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
    if (dispatch.enabled) throw new Error("dispatch.token's failing command was accepted");
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
      });
    }
  });

  // A multi-line bearer reaches fetch, whose refusal of the header quotes it whole.
  test("that prints more than one line, or a value with whitespace inside, is refused without echoing it", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    for (const [token, refusal] of [
      [
        `!printf 'S3CRET-TOKEN\\nurl: https://example' # ${home}`,
        "dispatch.token's command printed more than one line, or whitespace inside its value",
      ],
      [`!echo 'S3CRET TOKEN' # ${home}`, "dispatch.token's command printed more than one line"],
      ["S3CRET-TOKEN\nnotes", "dispatch.token holds whitespace or a control character"],
      ["S3CRET\tTOKEN", "dispatch.token holds whitespace or a control character"],
    ] as const) {
      const file = writeFile(path.join(tempDir(), "envoy.json"), {
        dispatch: { ...dispatchOn, token },
      });
      const dispatch = dispatchSettings(env, readEnvoyConfig(file, env, { cwd: tempDir() }));
      if (dispatch.enabled) throw new Error(`${JSON.stringify(token)} was accepted`);
      expect(dispatch.error).toContain(`${file}: ${refusal}`);
      expect(dispatch.error).not.toContain("S3CRET");
    }
  });

  test("that exits 0 is its value, even when a process it started holds its stdout past 10 s", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!sleep 12 & echo lingering-secret # ${home}` },
    });
    expect(dispatchSettings(env, readEnvoyConfig(file, env, { cwd: tempDir() }))).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "lingering-secret",
    });
  }, 20_000);

  test("still running after 10 s is an error naming the field and the file, and every process it started is killed", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    // A duration no other process on the machine sleeps for, so pgrep finds only this one.
    const nap = `29.${Date.now() % 1_000_000}`;
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!echo canary; sleep ${nap} & sleep 30 # ${home}` },
    });
    const started = Date.now();
    const dispatch = dispatchSettings(env, readEnvoyConfig(file, env, { cwd: tempDir() }));
    expect(Date.now() - started).toBeLessThan(15_000);
    expect(dispatch).toEqual({
      enabled: false,
      notice: null,
      error: `${file}: dispatch.token's command did not finish within 10 s`,
    });
    expect(Bun.spawnSync(["pgrep", "-f", `sleep ${nap}`]).exitCode).toBe(1);
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
  // the user's NATS seed signs in to, nor take away the Envoy the session's file and environment
  // configure.
  test("an envoy section is not read, whatever it holds, runs nothing, and leaves Envoy as configured", () => {
    const home = tempDir();
    const runs = path.join(home, "runs");
    const seedFile = writeFile(path.join(home, "seed"), "SUENVSEED\n");
    const pane = {
      ...commandEnvironment(home),
      ENVOY_NATS_URL: "nats://pane.internal.example:4222",
      ENVOY_TOKEN: "environment-token",
      NATS_NKEY_SEED_FILE: seedFile,
    };
    for (const envoy of [
      { natsUrls: ["nats://repo.internal.example:4222"] },
      { url: "http://repo.internal.example:9020", token: "repo-token" },
      { natsSeed: `!echo seed >> ${runs}; echo SUREPOSEED` },
      { enabled: false },
      { enabled: true, natsUrl: "nats://repo.internal.example:4222" },
    ]) {
      const { user, cwd } = withCheckout({ envoy });
      const config = readEnvoyConfig(user, commandEnvironment(home), { cwd });
      expect(envoySettings(commandEnvironment(home), config)).toMatchObject({
        enabled: true,
        url: "http://listener.internal.example:9020",
        natsUrls: ["nats://nats.internal.example:4222"],
        token: "listener-token",
      });
      expect(envoySettings(pane, readEnvoyConfig(user, pane, { cwd }))).toMatchObject({
        enabled: true,
        natsUrls: ["nats://pane.internal.example:4222"],
        token: "environment-token",
        natsSeed: { value: "SUENVSEED" },
      });
      expect(dispatchSettings(pane, config)).toEqual({
        enabled: true,
        url: "https://dispatch.example",
        token: "user-token",
      });
    }
    expect(existsSync(runs)).toBe(false);
  });

  // Ruling 55's rule for a file that cannot be read: a repository cannot turn off what the
  // session's environment configured.
  test("a file that cannot be used turns Dispatch off while it would be read, and never Envoy", () => {
    const tokenFile = writeFile(path.join(tempDir(), "dispatch-token"), "daemon-token\n");
    for (const contents of [
      "{",
      { dispatch: { enabled: true, bogus: 1 } },
      { dispatch: { serverUrl: "not a url" } },
      { natsUrls: ["nats://repo.internal.example:4222"] },
    ]) {
      const repo = withCheckout(contents);
      const quiet = { HOME: tempDir() };
      const config = readEnvoyConfig(repo.user, quiet, { cwd: repo.cwd });
      const dispatch = dispatchSettings(quiet, config);
      if (dispatch.enabled) throw new Error(`${JSON.stringify(contents)} left Dispatch on`);
      expect(dispatch.error).toStartWith(`${repo.checkout}: `);
      expect(envoySettings(quiet, config)).toMatchObject({
        enabled: true,
        token: "listener-token",
      });

      const pane = {
        HOME: tempDir(),
        DISPATCH_URL: "https://daemon.internal.example",
        DISPATCH_TOKEN_FILE: tokenFile,
        ENVOY_URL: "http://pane-listener.internal.example:9020",
        ENVOY_NATS_URL: "nats://pane.internal.example:4222",
        ENVOY_TOKEN: "pane-token",
      };
      const paneConfig = readEnvoyConfig(repo.user, pane, { cwd: repo.cwd });
      expect(dispatchSettings(pane, paneConfig)).toEqual({
        enabled: true,
        url: "https://daemon.internal.example",
        token: "daemon-token",
      });
      expect(envoySettings(pane, paneConfig)).toMatchObject({
        enabled: true,
        url: "http://pane-listener.internal.example:9020",
        token: "pane-token",
      });
    }
  });

  test("a server without its own token is refused", () => {
    const env = { HOME: tempDir() };
    const server = withCheckout({ dispatch: { serverUrl: "https://collector.attacker.test" } });
    const serverConfig = readEnvoyConfig(server.user, env, { cwd: server.cwd });
    expect(dispatchSettings(env, serverConfig)).toEqual({
      enabled: false,
      notice: null,
      error: `${server.checkout}: dispatch.serverUrl requires dispatch.token in the same file`,
    });
    expect(envoySettings(env, serverConfig)).toMatchObject({ enabled: true });
  });

  // The token belongs to the server its file names: beside the user's server it would make the
  // session act as the repository's author on the user's Dispatch.
  test("a token without its own server is refused when it would be used", () => {
    const repo = withCheckout({ dispatch: { token: "repo-token" } });
    const refusal = {
      enabled: false,
      notice: null,
      error: `${repo.checkout}: dispatch.token requires dispatch.serverUrl in the same file`,
    } as const;
    for (const env of [
      { HOME: tempDir() },
      { HOME: tempDir(), DISPATCH_URL: "https://dispatch.example" },
    ]) {
      expect(dispatchSettings(env, readEnvoyConfig(repo.user, env, { cwd: repo.cwd }))).toEqual(
        refusal
      );
    }
    // A token from the environment replaces it, so it is not used, and not refused.
    const environment = { HOME: tempDir(), DISPATCH_TOKEN: "environment-token" };
    expect(
      dispatchSettings(environment, readEnvoyConfig(repo.user, environment, { cwd: repo.cwd }))
    ).toEqual({ enabled: true, url: "https://dispatch.example", token: "environment-token" });
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
    });
    expect(envoySettings(dispatchEnv, serverConfig)).toMatchObject({ enabled: true });
  });

  test("a command in it is refused beside its own server, and never runs", () => {
    const home = tempDir();
    const ran = path.join(home, "ran");
    const env = commandEnvironment(home);
    for (const dispatch of [
      { serverUrl: "https://repo.internal.example", token: `!touch ${ran}; echo x` },
      { enabled: true, serverUrl: "https://repo.internal.example", token: `!touch ${ran}; echo x` },
    ]) {
      const repo = withCheckout({ dispatch });
      const config = readEnvoyConfig(repo.user, env, { cwd: repo.cwd });
      const refusal = {
        enabled: false,
        notice: null,
        error: `${repo.checkout}: dispatch.token is a command, which a checkout's envoy.json cannot run`,
      } as const;
      expect(dispatchSettings(env, config)).toEqual(refusal);
      // DISPATCH_URL naming the checkout's own server takes its token too, and still runs nothing.
      const named = { ...env, DISPATCH_URL: "https://repo.internal.example" };
      expect(dispatchSettings(named, readEnvoyConfig(repo.user, named, { cwd: repo.cwd }))).toEqual(
        refusal
      );
      expect(envoySettings(env, config)).toMatchObject({ enabled: true });
    }
    expect(existsSync(ran)).toBe(false);
  });

  test("its dispatch.enabled: false yields to DISPATCH_URL", () => {
    const tokenFile = writeFile(path.join(tempDir(), "dispatch-token"), "daemon-token\n");
    const off = withCheckout({ dispatch: { enabled: false } });
    const quiet = { HOME: tempDir() };
    const quietConfig = readEnvoyConfig(off.user, quiet, { cwd: off.cwd });
    expect(dispatchSettings(quiet, quietConfig)).toEqual({
      enabled: false,
      notice: `${off.checkout} sets dispatch.enabled to false: Dispatch is off`,
      error: null,
    });

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
    });
  });
});
