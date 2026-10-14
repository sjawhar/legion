import { describe, expect, setSystemTime, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { type DispatchConfigResolution, dispatchSettings } from "../dispatch-config";
import type * as EnvoyConfigModule from "../envoy-config";
import {
  type EnvoyConfig,
  type EnvoyHarness,
  envoyConfigPath,
  missingConfigNotice,
  readEnvoyConfig,
} from "../envoy-config";
import { envoySettings } from "../envoy-settings";

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
      [
        "command",
        { CLAUDE_CODE_CHILD_SESSION: "1", CLAUDE_CONFIG_DIR: "claude" },
        "CLAUDE_CONFIG_DIR is claude",
      ],
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
    expect(envoyConfigPath("command", { HOME: home, CLAUDE_CONFIG_DIR: "relative" })).toBe(neutral);
  });

  test("a command reads ENVOY_CONFIG_FILE, else the omp, Claude Code and OpenCode rules in turn, else the neutral file", () => {
    const harnesses = {
      HOME: home,
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: "/profiles/work/agent",
      CLAUDE_CODE_CHILD_SESSION: "1",
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
    expect(
      envoyConfigPath("command", {
        HOME: home,
        CLAUDE_CODE_CHILD_SESSION: "1",
        CLAUDE_CONFIG_DIR: "/claude",
      })
    ).toBe("/claude/envoy.json");
    expect(envoyConfigPath("command", { HOME: home, OPENCODE_CONFIG_DIR: "/opencode" })).toBe(
      "/opencode/envoy.json"
    );
    expect(envoyConfigPath("command", { HOME: home })).toBe(neutral);
  });

  // A shell outside Claude Code can export CLAUDE_CONFIG_DIR too (Claude Code's docs suggest setting
  // it in a shell profile); only Claude Code sets CLAUDE_CODE_CHILD_SESSION=1, in every Bash, hook
  // and status line command it runs.
  test("a command counts CLAUDE_CONFIG_DIR only when CLAUDE_CODE_CHILD_SESSION is 1", () => {
    const shell = { HOME: home, CLAUDE_CONFIG_DIR: "/claude" };
    // A script run by hand from a plain terminal whose shell exports CLAUDE_CONFIG_DIR.
    expect(envoyConfigPath("command", shell)).toBe(neutral);
    expect(envoyConfigPath("command", { ...shell, CLAUDE_CODE_CHILD_SESSION: "1" })).toBe(
      "/claude/envoy.json"
    );
    for (const marker of ["0", "", "true"]) {
      expect(envoyConfigPath("command", { ...shell, CLAUDE_CODE_CHILD_SESSION: marker })).toBe(
        neutral
      );
    }
    // Passed over, Claude Code's step leaves OpenCode's next.
    expect(envoyConfigPath("command", { ...shell, OPENCODE_CONFIG_DIR: "/opencode" })).toBe(
      "/opencode/envoy.json"
    );
    // The Claude Code plugin's own rule reads CLAUDE_CONFIG_DIR with no marker.
    expect(envoyConfigPath("claude", shell)).toBe("/claude/envoy.json");
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
      missing: { link: null },
      warnings: [],
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

  test("a missing neutral file beside the OpenCode-era file reads nothing of it, and its notice names the move", () => {
    const home = tempDir();
    const marker = path.join(home, "old-token-ran");
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: {
        enabled: true,
        serverUrl: "https://dispatch.example",
        token: `!touch ${marker}; echo old-token`,
      },
      natsUrls: ["nats://nats.internal.example:4222"],
    });
    const neutral = path.join(home, ".config", "legion", "envoy.json");
    const notice = `${old} is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`;
    const unread: EnvoyConfig = {
      kind: "read",
      path: neutral,
      dispatch: {},
      envoy: {},
      checkout: null,
      missing: { link: null, oldUserFileNotice: notice },
      warnings: [],
    };
    const config = readEnvoyConfig(neutral, { HOME: home }, { cwd: tempDir() });
    expect(config).toEqual(unread);
    expect(missingNotice(config, "Dispatch and Envoy are off")).toBe(
      `no envoy.json at ${neutral}: Dispatch and Envoy are off; ${notice}`
    );
    expect(dispatchSettings({ HOME: home }, config)).toEqual({
      enabled: false,
      notice: `no envoy.json at ${neutral}: Dispatch is off; ${notice}`,
      error: null,
    });
    expect(envoySettings({ HOME: home }, config)).toMatchObject({ enabled: false, error: null });
    // XDG_CONFIG_HOME set to $HOME/.config is the same file, and says the same.
    const sameHome = { HOME: home, XDG_CONFIG_HOME: path.join(home, ".config") };
    expect(readEnvoyConfig(neutral, sameHome, { cwd: tempDir() })).toEqual(unread);
    expect(existsSync(marker)).toBe(false);

    // A neutral file that exists is read, and the old one is neither read nor named.
    writeFile(neutral, { dispatch: { enabled: true, token: "new-token" } });
    expect(readEnvoyConfig(neutral, { HOME: home }, { cwd: tempDir() })).toEqual({
      ...unread,
      dispatch: { enabled: true, token: "new-token" },
      missing: null,
    });

    // Another chosen file, neither the user's default nor a profile's nor the neutral one under
    // XDG_CONFIG_HOME, says nothing of it, and a relative XDG_CONFIG_HOME refuses nothing here.
    const handed = path.join(tempDir(), "envoy.json");
    for (const env of [{ HOME: home }, { HOME: home, XDG_CONFIG_HOME: "relative/config" }]) {
      expect(readEnvoyConfig(handed, env, { cwd: tempDir() })).toEqual({
        ...unread,
        path: handed,
        missing: { link: null },
      });
    }
  });

  test("an OpenCode-era file that is not valid refuses nothing, since it is not read", () => {
    const home = tempDir();
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), "{not json");
    const neutral = path.join(home, ".config", "legion", "envoy.json");
    expect(readEnvoyConfig(neutral, { HOME: home }, { cwd: tempDir() })).toMatchObject({
      kind: "read",
      path: neutral,
      missing: {
        link: null,
        oldUserFileNotice: `${old} is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`,
      },
    });
  });

  test("a missing harness file leaves the OpenCode-era file unread and names how to configure each profile", () => {
    const home = tempDir();
    const marker = path.join(home, "old-token-ran");
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: {
        enabled: true,
        serverUrl: "https://old.dispatch.example",
        token: `!touch ${marker}; echo old-token`,
      },
    });
    const profiles = [
      ["omp", { OMP_PROFILE: "work", PI_CODING_AGENT_DIR: path.join(home, "omp") }],
      ["claude", { CLAUDE_CONFIG_DIR: path.join(home, "claude") }],
      ["opencode", { OPENCODE_CONFIG_DIR: path.join(home, "opencode") }],
    ] as const;

    for (const [harness, profile] of profiles) {
      const chosen = envoyConfigPath(harness, { HOME: home, ...profile });
      const config = readEnvoyConfig(chosen, { HOME: home, ...profile }, { cwd: tempDir() });
      expect(config).toMatchObject({
        path: chosen,
        dispatch: {},
        envoy: {},
        missing: { link: null },
      });
      expect(missingNotice(config, "Dispatch and Envoy are off")).toBe(
        `no envoy.json at ${chosen}: Dispatch and Envoy are off; ${old} is not read for this profile; copy or link it to ${chosen}, or write that file`
      );
      expect(existsSync(marker)).toBe(false);
    }
  });

  test("a relocated neutral file leaves the OpenCode-era file unread and says why", () => {
    const home = tempDir();
    const marker = path.join(home, "old-token-ran");
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: {
        enabled: true,
        serverUrl: "https://old.dispatch.example",
        token: `!touch ${marker}; echo old-token`,
      },
    });
    const configHome = path.join(home, "relocated-config");
    const env = { HOME: home, XDG_CONFIG_HOME: configHome };
    const chosen = envoyConfigPath("omp", env);
    const config = readEnvoyConfig(chosen, env, { cwd: tempDir() });

    expect(config).toMatchObject({
      path: chosen,
      dispatch: {},
      envoy: {},
      missing: { link: null },
    });
    expect(missingNotice(config, "Dispatch and Envoy are off")).toBe(
      `no envoy.json at ${chosen}: Dispatch and Envoy are off; ${old} is not read under XDG_CONFIG_HOME=${configHome}`
    );
    expect(existsSync(marker)).toBe(false);
  });

  // An agent box bind-mounts the host's ~/.config/opencode: a `mv` inside it would move the host's
  // file into the box's own throwaway filesystem. The devices decide: the old file's directory's,
  // and that of the nearest ancestor of the new file's directory that exists.
  test("a move across filesystems names the file to create, and no mv command", () => {
    const home = tempDir();
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: dispatchOn,
    });
    const neutral = path.join(home, ".config", "legion", "envoy.json");
    const cwd = tempDir();
    const oldCheckout = writeFile(path.join(cwd, ".opencode", "envoy.json"), {
      dispatch: dispatchOn,
    });
    const checkout = path.join(cwd, ".envoy", "envoy.json");
    const mounted = new Set([path.dirname(old), path.dirname(oldCheckout)]);
    const asked: string[] = [];
    const deviceOf = (directory: string) => {
      asked.push(directory);
      if (!existsSync(directory)) return undefined;
      return mounted.has(directory) ? 2 : 1;
    };
    const across = readEnvoyConfig(neutral, { HOME: home }, { cwd, deviceOf });
    expect(across).toMatchObject({
      missing: {
        oldUserFileNotice: `${old} is not read, and is on another filesystem than ${neutral} (a mount): create ${neutral}, and move ${old} on the machine that owns it`,
      },
      warnings: [
        `${oldCheckout} is not read, and is on another filesystem than ${checkout} (a mount): create ${checkout}, and move ${oldCheckout} on the machine that owns it`,
      ],
    });
    expect(JSON.stringify(across)).not.toContain("mv ");
    // ~/.config/legion does not exist, so its parent's device is the one compared.
    expect(asked).toEqual(
      expect.arrayContaining([path.join(home, ".config", "legion"), path.join(home, ".config")])
    );

    const sameDevice = (directory: string) => (existsSync(directory) ? 1 : undefined);
    expect(readEnvoyConfig(neutral, { HOME: home }, { cwd, deviceOf: sameDevice })).toMatchObject({
      missing: {
        oldUserFileNotice: `${old} is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`,
      },
      warnings: [
        `${oldCheckout} is not read: mkdir -p ${path.dirname(checkout)} && mv ${oldCheckout} ${checkout}`,
      ],
    });
  });

  // The same rule on real filesystems: a tmpfs directory linked in as ~/.config/opencode is another
  // device than the temporary directory's.
  const shm = "/dev/shm";
  test.skipIf(!existsSync(shm) || statSync(shm).dev === statSync(os.tmpdir()).dev)(
    "a move across real filesystems names the file to create, and no mv command",
    () => {
      const home = tempDir();
      const elsewhere = mkdtempSync(path.join(shm, "envoy-config-"));
      try {
        mkdirSync(path.join(home, ".config"));
        symlinkSync(elsewhere, path.join(home, ".config", "opencode"));
        const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {});
        const neutral = path.join(home, ".config", "legion", "envoy.json");
        expect(readEnvoyConfig(neutral, { HOME: home }, { cwd: tempDir() })).toMatchObject({
          missing: {
            oldUserFileNotice: `${old} is not read, and is on another filesystem than ${neutral} (a mount): create ${neutral}, and move ${old} on the machine that owns it`,
          },
        });
      } finally {
        rmSync(elsewhere, { recursive: true, force: true });
      }
    }
  );

  test("a harness directory at the default config directory gets the default file's move, reading nothing", () => {
    const home = tempDir();
    const directory = path.join(home, ".config", "legion");
    const marker = path.join(home, "old-token-ran");
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: {
        enabled: true,
        serverUrl: "https://old.dispatch.example",
        token: `!touch ${marker}; echo old-token`,
      },
    });
    const profiles = [
      ["omp", { OMP_PROFILE: "work", PI_CODING_AGENT_DIR: directory }],
      ["claude", { CLAUDE_CONFIG_DIR: directory }],
      ["opencode", { OPENCODE_CONFIG_DIR: directory }],
    ] as const;

    // The file's path decides, so a relocated XDG_CONFIG_HOME does not change it.
    for (const [harness, profile] of profiles) {
      for (const configHome of [{}, { XDG_CONFIG_HOME: path.join(home, "relocated-config") }]) {
        const env = { HOME: home, ...configHome, ...profile };
        const chosen = envoyConfigPath(harness, env);
        const config = readEnvoyConfig(chosen, env, { cwd: tempDir() });

        expect(config).toMatchObject({ path: chosen, dispatch: {}, envoy: {} });
        expect(missingNotice(config, "Dispatch and Envoy are off")).toBe(
          `no envoy.json at ${chosen}: Dispatch and Envoy are off; ${old} is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`
        );
        expect(dispatchSettings(env, config)).toMatchObject({ enabled: false, error: null });
      }
    }
    expect(existsSync(marker)).toBe(false);
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
      missing: { link: null },
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
      warnings: [],
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
      warnings: [],
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
      warnings: [],
    });
  });

  test("a checkout's .opencode/envoy.json is not read, and a warning names the rename", () => {
    const cwd = tempDir();
    const old = writeFile(path.join(cwd, ".opencode", "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "https://old.internal.example", token: "t" },
      natsUrls: ["nats://checkout.internal.example:4222"],
      envoy: { enabled: true, natsUrls: ["nats://other.internal.example:4222"] },
    });
    const file = writeFile(path.join(tempDir(), "envoy.json"), {});
    const config = readEnvoyConfig(file, { HOME: tempDir() }, { cwd });
    expect(config).toEqual({
      kind: "read",
      path: file,
      dispatch: {},
      envoy: {},
      checkout: null,
      missing: null,
      warnings: [
        `${old} is not read: mkdir -p ${path.join(cwd, ".envoy")} && mv ${old} ${path.join(cwd, ".envoy", "envoy.json")}`,
      ],
    });
    expect(dispatchSettings({}, config)).toMatchObject({ enabled: false, error: null });
    expect(envoySettings({}, config)).toMatchObject({ enabled: false, error: null });
    // A .envoy/envoy.json beside it is the one read, and the old one is not named.
    const current = writeFile(path.join(cwd, ".envoy", "envoy.json"), {
      dispatch: { enabled: false },
    });
    expect(readEnvoyConfig(file, { HOME: tempDir() }, { cwd })).toMatchObject({
      checkout: { path: current },
      warnings: [],
    });
  });

  test("a chosen file at a checkout's .opencode/envoy.json is read as the chosen file, and not named", () => {
    const cwd = tempDir();
    const chosen = writeFile(path.join(cwd, ".opencode", "envoy.json"), { dispatch: dispatchOn });
    expect(readEnvoyConfig(chosen, { HOME: tempDir() }, { cwd })).toMatchObject({
      path: chosen,
      dispatch: dispatchOn,
      checkout: null,
      warnings: [],
    });
  });

  test("a chosen file's top-level natsUrls is refused, naming envoy.natsUrls", () => {
    for (const contents of [
      { natsUrls: ["nats://nats.internal.example:4222"] },
      {
        envoy: { natsUrls: ["nats://one.internal.example:4222"] },
        natsUrls: ["nats://two.internal.example:4222"],
      },
    ]) {
      const file = writeFile(path.join(tempDir(), "envoy.json"), contents);
      expect(errorOf(readEnvoyConfig(file, { HOME: tempDir() }, { cwd: tempDir() }))).toBe(
        `${file}: the top-level natsUrls is not read; move it to envoy.natsUrls`
      );
    }
  });

  test("a chosen file that is the checkout's own is read once, as the chosen file", () => {
    const cwd = tempDir();
    const own = writeFile(path.join(cwd, ".envoy", "envoy.json"), { envoy: envoyOn });
    const config = readEnvoyConfig(own, { HOME: tempDir() }, { cwd });
    expect(config).toMatchObject({ kind: "read", envoy: envoyOn, checkout: null });
    expect(envoySettings({}, config)).toMatchObject({ enabled: true, token: "listener-token" });
  });

  test("an unknown key in either section is refused naming the key", () => {
    const home = tempDir();
    const cases: [unknown, string][] = [
      [{ dispatch: { enabled: true, unexpected: true } }, "dispatch.unexpected"],
      [{ envoy: { enabled: true, natsUrl: "nats://nats.internal.example:4222" } }, "envoy.natsUrl"],
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

  // One rule for both sections: enabled: false is off; otherwise the section's fields apply, and
  // it is on when its address resolves, from the file or a variable.
  test("without enabled, a section is on when its address resolves, and its fields apply", () => {
    const env = { HOME: tempDir() };
    const addressed = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: { serverUrl: "https://dispatch.example", token: "user-token" },
      envoy: { url: "http://listener.internal.example:9020", token: "listener-token" },
    });
    const fromFile = readEnvoyConfig(addressed, env, { cwd: tempDir() });
    expect(dispatchSettings(env, fromFile)).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "user-token",
    });
    expect(envoySettings(env, fromFile)).toMatchObject({
      enabled: true,
      url: "http://listener.internal.example:9020",
      token: "listener-token",
      inbound: { off: "Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)" },
    });
    // The same file under a launcher that names the same servers: each section keeps its bearer.
    const launcher = {
      HOME: tempDir(),
      DISPATCH_URL: "https://dispatch.example",
      ENVOY_URL: "http://listener.internal.example:9020",
    };
    const launched = readEnvoyConfig(addressed, launcher, { cwd: tempDir() });
    expect(dispatchSettings(launcher, launched)).toMatchObject({ token: "user-token" });
    expect(envoySettings(launcher, launched)).toMatchObject({ token: "listener-token" });

    const nats = writeFile(path.join(tempDir(), "envoy.json"), {
      envoy: { natsUrls: ["nats://nats.internal.example:4222"] },
    });
    expect(envoySettings(env, readEnvoyConfig(nats, env, { cwd: tempDir() }))).toMatchObject({
      enabled: true,
      url: "http://127.0.0.1:9020",
      inbound: { natsUrls: ["nats://nats.internal.example:4222"] },
    });
  });

  test("a section whose address resolves nowhere is off; enabled: true names the default", () => {
    const env = { HOME: tempDir() };
    const tokensOnly = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: { token: "user-token" },
      envoy: { token: "listener-token" },
    });
    const config = readEnvoyConfig(tokensOnly, env, { cwd: tempDir() });
    expect(dispatchSettings(env, config)).toEqual({
      enabled: false,
      notice: `${tokensOnly} names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`,
      error: null,
    });
    expect(envoySettings(env, config)).toEqual({
      enabled: false,
      notice: `${tokensOnly} names no Envoy listener or NATS server, and neither ENVOY_URL nor ENVOY_NATS_URL is set: Envoy is off`,
      error: null,
    });
    // A variable naming the default server is that section's address, and the file's token goes
    // with it.
    const defaults = {
      HOME: tempDir(),
      DISPATCH_URL: "http://localhost:8766",
      ENVOY_URL: "http://127.0.0.1:9020",
    };
    const named = readEnvoyConfig(tokensOnly, defaults, { cwd: tempDir() });
    expect(dispatchSettings(defaults, named)).toMatchObject({ token: "user-token" });
    expect(envoySettings(defaults, named)).toMatchObject({ token: "listener-token" });

    const enabled = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: { enabled: true, token: "user-token" },
      envoy: { enabled: true, token: "listener-token" },
    });
    const on = readEnvoyConfig(enabled, env, { cwd: tempDir() });
    expect(dispatchSettings(env, on)).toEqual({
      enabled: true,
      url: "http://localhost:8766",
      token: "user-token",
    });
    expect(envoySettings(env, on)).toMatchObject({
      enabled: true,
      url: "http://127.0.0.1:9020",
      token: "listener-token",
    });
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
      inbound: { natsSeed: { value: "SUSEED", source: `envoy.natsSeed in ${file}` } },
    });
  });

  // A command relative to a directory, or one a directory configures (a mise or direnv shim), must
  // resolve against the file that names it, never whatever repository the session started in.
  test("runs in its envoy.json's directory, never the session's", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const directory = path.join(home, "config");
    writeFile(path.join(directory, "bin", "token"), "#!/bin/sh\necho relative-secret\n");
    chmodSync(path.join(directory, "bin", "token"), 0o755);
    const session = tempDir();
    writeFile(path.join(session, "bin", "token"), "#!/bin/sh\necho checkout-secret\n");
    chmodSync(path.join(session, "bin", "token"), 0o755);
    const file = writeFile(path.join(directory, "envoy.json"), {
      dispatch: { ...dispatchOn, token: "!./bin/token" },
      envoy: { ...envoyOn, token: `!pwd # ${home}` },
    });
    const original = process.cwd();
    process.chdir(session);
    try {
      const config = readEnvoyConfig(file, env, { cwd: session });
      expect(dispatchSettings(env, config)).toMatchObject({ token: "relative-secret" });
      expect(envoySettings(env, config)).toMatchObject({ token: realpathSync(directory) });
    } finally {
      process.chdir(original);
    }
  });

  test("keeps its value for the process, and a failure for 60 s, then runs again", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const runs = path.join(home, "runs");
    const failures = path.join(home, "failures");
    const late = path.join(home, "late-token");
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!echo run >> ${runs}; echo cached-secret` },
      envoy: { ...envoyOn, token: `!echo run >> ${failures}; cat ${late}` },
    });
    const resolve = () => {
      const config = readEnvoyConfig(file, env, { cwd: tempDir() });
      return { dispatch: dispatchSettings(env, config), envoy: envoySettings(env, config) };
    };
    const start = Date.now();
    try {
      for (let resolution = 0; resolution < 2; resolution += 1) {
        expect(resolve()).toMatchObject({
          dispatch: { token: "cached-secret" },
          envoy: { enabled: false },
        });
      }
      // The cause goes away, as a keyring unlocked after the session started.
      writeFileSync(late, "late-secret\n");
      setSystemTime(new Date(start + 59_000));
      expect(resolve().envoy).toMatchObject({ enabled: false });
      expect(readFileSync(failures, "utf-8")).toBe("run\n");
      setSystemTime(new Date(start + 61_000));
      expect(resolve()).toMatchObject({
        dispatch: { token: "cached-secret" },
        envoy: { enabled: true, token: "late-secret" },
      });
      expect(readFileSync(failures, "utf-8")).toBe("run\nrun\n");
      expect(readFileSync(runs, "utf-8")).toBe("run\n");
    } finally {
      setSystemTime();
    }
  });

  // A failure that ended on its own is kept for 60 s and then retried: whatever caused it (a
  // locked keyring, a file not yet written) may clear without a restart.
  test("a failure that ended on its own (non-zero exit, empty output, more than one line) is kept 60 s, then runs again", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const late = path.join(home, "late-token");
    const failing: [failure: string, command: string][] = [
      ["exited with status 1", `cat ${late}`],
      ["printed nothing", `cat ${late} 2>/dev/null; true`],
      [
        "printed more than one line, or whitespace inside its value",
        `cat ${late} 2>/dev/null || printf 'a\\nb\\n'`,
      ],
    ];
    const start = Date.now();
    try {
      for (const [index, [failure, command]] of failing.entries()) {
        setSystemTime(new Date(start));
        rmSync(late, { force: true });
        const runs = path.join(home, `runs-${index}`);
        const file = writeFile(path.join(home, `kind-${index}`, "envoy.json"), {
          dispatch: { ...dispatchOn, token: `!echo run >> ${runs}; ${command}` },
        });
        const resolve = () => dispatchSettings(env, readEnvoyConfig(file, env, { cwd: tempDir() }));
        expect(resolve()).toEqual({
          enabled: false,
          notice: null,
          error: `${file}: dispatch.token's command ${failure}`,
        });
        writeFileSync(late, "late-secret\n");
        setSystemTime(new Date(start + 59_000));
        expect(resolve()).toMatchObject({ enabled: false });
        expect(readFileSync(runs, "utf-8")).toBe("run\n");
        setSystemTime(new Date(start + 61_000));
        expect(resolve()).toMatchObject({ enabled: true, token: "late-secret" });
        expect(readFileSync(runs, "utf-8")).toBe("run\nrun\n");
      }
    } finally {
      setSystemTime();
    }
  });

  // Each run of a command that hangs blocks the process for the whole timeout, since it runs
  // synchronously: retrying it once a minute would freeze the session that often.
  test("one that ran out of time is kept failed for the life of the process", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const runs = path.join(home, "runs");
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!echo run >> ${runs}; sleep 30` },
    });
    const resolve = () => dispatchSettings(env, readEnvoyConfig(file, env, { cwd: tempDir() }));
    const start = Date.now();
    try {
      const first = resolve();
      for (const later of [61_000, 24 * 60 * 60_000]) {
        setSystemTime(new Date(start + later));
        expect(resolve()).toEqual(first);
      }
      expect(readFileSync(runs, "utf-8")).toBe("run\n");
      expect(first).toEqual({
        enabled: false,
        notice: null,
        error: `${file}: dispatch.token's command did not finish within 10 s, and does not run again in this process`,
      });
    } finally {
      setSystemTime();
    }
  }, 45_000);

  // Oh My Pi loads each extension instance as a fresh module: a cache held by one module instance
  // would run every command again in each, blocking the parent's event loop at a subagent's start.
  test("is kept for every copy of the module in the process, by command, file and directory", async () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const runs = path.join(home, "runs");
    const token = `!echo run >> ${runs}; echo shared-secret`;
    const first = writeFile(path.join(home, "one", "envoy.json"), { dispatch: { token } });
    const second = writeFile(path.join(home, "two", "envoy.json"), { dispatch: { token } });
    // Each query string on a relative specifier loads a module of its own, as the host's loader
    // does (a file: URL's query is dropped, so it would load this one).
    const source = "../envoy-config.ts";
    const copies: (typeof EnvoyConfigModule)[] = [
      await import(`${source}?a`),
      await import(`${source}?b`),
    ];
    expect(copies[0]).not.toBe(copies[1]);
    for (const copy of copies) {
      expect(copy.fileValue(token, "dispatch.token", first, env)).toBe("shared-secret");
    }
    expect(readFileSync(runs, "utf-8")).toBe("run\n");
    // Another file is another key, and its command runs in its own directory.
    for (const copy of copies) {
      expect(copy.fileValue(token, "dispatch.token", second, env)).toBe("shared-secret");
    }
    expect(readFileSync(runs, "utf-8")).toBe("run\nrun\n");
  });

  // A failing natsSeed command turns off only inbound messages: the seed is for NATS alone.
  test("that fails or prints nothing turns off only what it is for, naming the field and the file, never its output", () => {
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

    const tokenFails = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: dispatchOn,
      envoy: { ...envoyOn, token: `!printf '  \\n' # ${home}` },
    });
    const brokenToken = readEnvoyConfig(tokenFails, env, { cwd: tempDir() });
    const envoy = envoySettings(env, brokenToken);
    if (envoy.enabled) throw new Error("envoy.token's failing command was accepted");
    expect(envoy.error).toContain(`${tokenFails}: envoy.token's command`);
    expect(dispatchSettings(env, brokenToken)).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "user-token",
    });

    const seedFails = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: dispatchOn,
      envoy: { ...envoyOn, natsSeed: `!echo ${canary} >&2; echo ${canary}; false` },
    });
    const brokenSeed = readEnvoyConfig(seedFails, env, { cwd: tempDir() });
    const inboundOff = envoySettings(env, brokenSeed);
    if (!inboundOff.enabled || !("off" in inboundOff.inbound)) {
      throw new Error(`a failing natsSeed turned Envoy off, or was taken`);
    }
    expect(inboundOff).toMatchObject({
      url: "http://listener.internal.example:9020",
      token: "listener-token",
    });
    expect(inboundOff.inbound.off).toContain(`${seedFails}: envoy.natsSeed's command exited with`);
    expect(inboundOff.inbound.off).not.toContain("canary");
    expect(dispatchSettings(env, brokenSeed)).toMatchObject({ enabled: true });
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

  /** Runs `body` with TMPDIR a scratch directory, where a `!` command keeps its output, and
   * returns what is left in it afterwards. */
  function inScratchTmp(body: () => void): string[] {
    const scratch = tempDir();
    const original = process.env.TMPDIR;
    process.env.TMPDIR = scratch;
    try {
      body();
    } finally {
      if (original === undefined) delete process.env.TMPDIR;
      else process.env.TMPDIR = original;
    }
    return readdirSync(scratch);
  }

  /** Whether the process with this pid is gone within a second: a killed orphan is a zombie until
   * the process it was reparented to reaps it, and `kill(pid, 0)` still answers for a zombie. */
  function gone(pid: number): boolean {
    for (const deadline = Date.now() + 1_000; Date.now() < deadline; Bun.sleepSync(20)) {
      try {
        process.kill(pid, 0);
      } catch {
        return true;
      }
    }
    return false;
  }

  test("that exits 0 is its value at once, and a process it left running is killed", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const pidFile = path.join(home, "child.pid");
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: { ...dispatchOn, token: `!sleep 30 & echo $! > ${pidFile}; echo lingering-secret` },
    });
    const cwd = tempDir();
    let dispatch: DispatchConfigResolution | undefined;
    let elapsed = 0;
    const left = inScratchTmp(() => {
      const started = Date.now();
      dispatch = dispatchSettings(env, readEnvoyConfig(file, env, { cwd }));
      elapsed = Date.now() - started;
    });
    expect(dispatch).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "lingering-secret",
    });
    expect(elapsed).toBeLessThan(2_000);
    const child = Number(readFileSync(pidFile, "utf-8").trim());
    expect(child).toBeGreaterThan(0);
    expect(gone(child)).toBe(true);
    expect(left).toEqual([]);
  }, 20_000);

  test("still running after 10 s is an error naming the field and the file, and every process it started is killed", () => {
    const home = tempDir();
    const env = commandEnvironment(home);
    const pidFile = path.join(home, "child.pid");
    const file = writeFile(path.join(home, "envoy.json"), {
      dispatch: {
        ...dispatchOn,
        token: `!echo canary; sleep 40 & echo $! > ${pidFile}; sleep 30 # ${home}`,
      },
    });
    const cwd = tempDir();
    let dispatch: DispatchConfigResolution | undefined;
    let elapsed = 0;
    const left = inScratchTmp(() => {
      const started = Date.now();
      dispatch = dispatchSettings(env, readEnvoyConfig(file, env, { cwd }));
      elapsed = Date.now() - started;
    });
    expect(elapsed).toBeLessThan(15_000);
    expect(dispatch).toEqual({
      enabled: false,
      notice: null,
      error: `${file}: dispatch.token's command did not finish within 10 s, and does not run again in this process`,
    });
    expect(gone(Number(readFileSync(pidFile, "utf-8").trim()))).toBe(true);
    expect(left).toEqual([]);
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
      token: "listener-token",
      inbound: { natsUrls: ["nats://nats.internal.example:4222"] },
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
        token: "listener-token",
        inbound: { natsUrls: ["nats://nats.internal.example:4222"] },
      });
      expect(envoySettings(pane, readEnvoyConfig(user, pane, { cwd }))).toMatchObject({
        enabled: true,
        token: "environment-token",
        inbound: {
          natsUrls: ["nats://pane.internal.example:4222"],
          natsSeed: { value: "SUENVSEED" },
        },
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

  // One message for each refusal of a checkout's token. DISPATCH_URL replaces the checkout's server,
  // and its token with it: the chosen file's token is the one paired with DISPATCH_URL.
  test("an empty or unsendable token beside its server is refused with one message, and not used under DISPATCH_URL", () => {
    const refusals = [
      ["", (checkout: string) => `dispatch.token in ${checkout} must be a non-empty bearer token`],
      [
        "has space",
        (checkout: string) => `${checkout}: dispatch.token holds whitespace or a control character`,
      ],
    ] as const;
    for (const [token, refusal] of refusals) {
      const repo = withCheckout({
        dispatch: { serverUrl: "https://repo.internal.example", token },
      });
      const quiet = { HOME: tempDir() };
      expect(dispatchSettings(quiet, readEnvoyConfig(repo.user, quiet, { cwd: repo.cwd }))).toEqual(
        { enabled: false, notice: null, error: refusal(repo.checkout) }
      );
      const named = { HOME: tempDir(), DISPATCH_URL: "https://repo.internal.example" };
      expect(dispatchSettings(named, readEnvoyConfig(repo.user, named, { cwd: repo.cwd }))).toEqual(
        {
          enabled: false,
          notice: null,
          error: `DISPATCH_URL names https://repo.internal.example, but ${repo.user}'s dispatch.token is for https://dispatch.example; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
        }
      );
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
    const quiet = { HOME: tempDir() };
    expect(dispatchSettings(quiet, readEnvoyConfig(repo.user, quiet, { cwd: repo.cwd }))).toEqual({
      enabled: false,
      notice: null,
      error: `${repo.checkout}: dispatch.token requires dispatch.serverUrl in the same file`,
    });
    // DISPATCH_URL or a token from the environment replaces it, so it is not used, and not
    // refused: under DISPATCH_URL the chosen file's token goes to its own server.
    const named = { HOME: tempDir(), DISPATCH_URL: "https://dispatch.example" };
    expect(dispatchSettings(named, readEnvoyConfig(repo.user, named, { cwd: repo.cwd }))).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "user-token",
    });
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
      // DISPATCH_URL replaces the checkout's server and its token: the command is neither used nor
      // refused, and the chosen file's token is paired with DISPATCH_URL instead.
      const named = { ...env, DISPATCH_URL: "https://repo.internal.example" };
      expect(dispatchSettings(named, readEnvoyConfig(repo.user, named, { cwd: repo.cwd }))).toEqual(
        {
          enabled: false,
          notice: null,
          error: `DISPATCH_URL names https://repo.internal.example, but ${repo.user}'s dispatch.token is for https://dispatch.example; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
        }
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

  // A checkout contributes only its enabled: false, or a server with its own token. Its
  // enabled: true alone would name the default server and draw a variable's token to it.
  test("its dispatch.enabled: true alone turns nothing on", () => {
    const cwd = tempDir();
    writeFile(path.join(cwd, ".envoy", "envoy.json"), { dispatch: { enabled: true } });
    const env = { HOME: tempDir(), DISPATCH_TOKEN: "environment-token" };
    const quiet = writeFile(path.join(tempDir(), "envoy.json"), { envoy: envoyOn });
    expect(dispatchSettings(env, readEnvoyConfig(quiet, env, { cwd }))).toEqual({
      enabled: false,
      notice: `${quiet} names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`,
      error: null,
    });
    const missing = path.join(tempDir(), "envoy.json");
    expect(dispatchSettings(env, readEnvoyConfig(missing, env, { cwd }))).toEqual({
      enabled: false,
      notice: `no envoy.json at ${missing}: Dispatch is off`,
      error: null,
    });
    // The chosen file's own enabled: true is what turns it on.
    const own = writeFile(path.join(tempDir(), "envoy.json"), { dispatch: { enabled: true } });
    expect(dispatchSettings(env, readEnvoyConfig(own, env, { cwd }))).toEqual({
      enabled: true,
      url: "http://localhost:8766",
      token: "environment-token",
    });
  });
});
