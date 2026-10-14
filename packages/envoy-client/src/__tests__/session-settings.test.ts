import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  handedConfigFile,
  sessionConfigFile,
  sessionDispatchSettings,
  sessionEnvoySettings,
  sessionSettings,
} from "../session-settings";

function tempDir(): string {
  return mkdtempSync(path.join(os.tmpdir(), "session-settings-"));
}

function writeFile(file: string, contents: unknown): string {
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, typeof contents === "string" ? contents : JSON.stringify(contents));
  return file;
}

const dispatchOn = {
  enabled: true,
  serverUrl: "https://dispatch.example",
  token: "file-token",
};
const envoyOn = {
  enabled: true,
  url: "http://listener.internal.example:9020",
  natsUrls: ["nats://nats.internal.example:4222"],
};

describe("sessionConfigFile", () => {
  test("names the file the harness reads", () => {
    expect(
      sessionConfigFile("omp", {
        HOME: "/home/someone",
        OMP_PROFILE: "work",
        PI_CODING_AGENT_DIR: "/agent",
      })
    ).toEqual({ path: "/agent/envoy.json", error: null });
  });

  test("turns a location that cannot be worked out into an error instead of a throw", () => {
    expect(
      sessionConfigFile("opencode", { HOME: "/home/someone", XDG_CONFIG_HOME: "rel" })
    ).toEqual({
      path: null,
      error: "XDG_CONFIG_HOME is rel, which is not an absolute path",
    });
  });
});

describe("sessionSettings", () => {
  test("a missing file turns both sections off with its notice", () => {
    const file = path.join(tempDir(), "envoy.json");
    expect(sessionSettings({ path: file, error: null }, {}, { cwd: tempDir() })).toEqual({
      dispatch: {
        enabled: false,
        notice: `no envoy.json at ${file}: Dispatch is off`,
        error: null,
      },
      envoy: {
        enabled: false,
        notice: `no envoy.json at ${file}: Envoy is off`,
        error: null,
      },
      notice: `no envoy.json at ${file}: Dispatch and Envoy are off`,
    });
  });

  // A checkout's file with its own server and token configures Dispatch for a user who has none.
  test("a missing file beside a checkout that enables Dispatch: Dispatch on, Envoy off", () => {
    const cwd = tempDir();
    writeFile(path.join(cwd, ".envoy", "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "https://repo.internal.example", token: "repo-token" },
    });
    const missing = path.join(tempDir(), "envoy.json");
    const settings = sessionSettings({ path: missing, error: null }, { HOME: tempDir() }, { cwd });
    expect(settings.dispatch).toEqual({
      enabled: true,
      url: "https://repo.internal.example",
      token: "repo-token",
    });
    expect(settings.notice).toBe(`no envoy.json at ${missing}: Envoy is off`);
  });

  test("with a missing file and a variable that turns Envoy on, the notice names Dispatch alone", () => {
    const file = path.join(tempDir(), "envoy.json");
    const settings = sessionSettings(
      { path: file, error: null },
      { ENVOY_URL: "http://listener.internal.example:9020", ENVOY_NATS_URL: "nats://n:4222" },
      { cwd: tempDir() }
    );
    expect(settings.envoy.enabled).toBe(true);
    expect(settings.notice).toBe(`no envoy.json at ${file}: Dispatch is off`);
  });

  test("a section's error is the notice, beside a section that is on", () => {
    const file = path.join(tempDir(), "envoy.json");
    const settings = sessionSettings(
      { path: file, error: null },
      {
        DISPATCH_URL: "https://dispatch.example",
        ENVOY_URL: "http://listener.internal.example:9020",
        ENVOY_NATS_URL: "nats://n:4222",
      },
      { cwd: tempDir() }
    );
    expect(settings.notice).toBe(
      `Dispatch is off: DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or dispatch.token in ${file}`
    );

    const envoyError = sessionSettings(
      {
        path: writeFile(path.join(tempDir(), "envoy.json"), { dispatch: dispatchOn }),
        error: null,
      },
      { ENVOY_TOKEN_FILE: path.join(tempDir(), "missing"), ENVOY_URL: "http://l:9020" },
      { cwd: tempDir() }
    );
    expect(envoyError.dispatch.enabled).toBe(true);
    expect(envoyError.notice).toStartWith("Envoy is off: ENVOY_TOKEN_FILE");
  });

  test("a checkout's envoy section is said, and neither section is touched by it", () => {
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: dispatchOn,
      envoy: envoyOn,
    });
    const cwd = tempDir();
    const checkout = writeFile(path.join(cwd, ".envoy", "envoy.json"), {
      envoy: { natsUrls: ["nats://repo.internal.example:4222"] },
    });
    const settings = sessionSettings({ path: file, error: null }, {}, { cwd });
    expect(settings.dispatch).toMatchObject({ enabled: true, url: "https://dispatch.example" });
    expect(settings.envoy).toMatchObject({
      enabled: true,
      inbound: { natsUrls: envoyOn.natsUrls },
    });
    expect(settings.notice).toBe(
      `${checkout}: envoy is not read from a checkout's envoy.json; put it in ${file}`
    );
  });

  test("a checkout's file that cannot be used is Dispatch's error, or said as not read while DISPATCH_URL replaces it", () => {
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: dispatchOn,
      envoy: envoyOn,
    });
    const cwd = tempDir();
    const checkout = writeFile(path.join(cwd, ".envoy", "envoy.json"), "{");
    const quiet = sessionSettings({ path: file, error: null }, {}, { cwd });
    expect(quiet.envoy.enabled).toBe(true);
    expect(quiet.notice).toBe(`Dispatch is off: ${checkout}: invalid JSON at line 1, column 2`);

    const pane = sessionSettings(
      { path: file, error: null },
      { DISPATCH_URL: "https://dispatch.example" },
      { cwd }
    );
    expect(pane.dispatch).toMatchObject({ enabled: true, url: "https://dispatch.example" });
    expect(pane.notice).toBe(
      `${checkout}: invalid JSON at line 1, column 2; not read while DISPATCH_URL is set`
    );
  });

  test("a file that turns a section off, or holds none for it, says nothing", () => {
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: { enabled: false },
      envoy: envoyOn,
    });
    const settings = sessionSettings({ path: file, error: null }, {}, { cwd: tempDir() });
    expect(settings.dispatch.enabled).toBe(false);
    expect(settings.envoy.enabled).toBe(true);
    expect(settings.notice).toBeNull();

    const empty = writeFile(path.join(tempDir(), "envoy.json"), {});
    expect(sessionSettings({ path: empty, error: null }, {}, { cwd: tempDir() }).notice).toBeNull();
  });

  test("a file that cannot be read turns both off with one error", () => {
    const file = writeFile(path.join(tempDir(), "envoy.json"), "{");
    const settings = sessionSettings({ path: file, error: null }, {}, { cwd: tempDir() });
    const error = `${file}: invalid JSON at line 1, column 2`;
    expect(settings).toEqual({
      dispatch: { enabled: false, notice: null, error },
      envoy: { enabled: false, notice: null, error },
      notice: `${error}: Dispatch and Envoy are off`,
    });
  });

  test("an Envoy with no NATS server says inbound messages are off, unless the caller opens no NATS connection", () => {
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: dispatchOn,
      envoy: { url: envoyOn.url },
    });
    const settings = sessionSettings({ path: file, error: null }, {}, { cwd: tempDir() });
    expect(settings.envoy).toMatchObject({
      enabled: true,
      inbound: { off: "Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)" },
    });
    expect(settings.notice).toBe(
      "Envoy's inbound messages are off: Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL); the Envoy tools still work"
    );
    const noNats = sessionSettings(
      { path: file, error: null },
      {},
      { cwd: tempDir(), nats: false }
    );
    expect(noNats.notice).toBeNull();
  });

  // A seed is for NATS alone: its refusal leaves the listener on and turns off inbound messages.
  test("a refused NATS seed turns off inbound messages alone, and the notice says why", () => {
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: dispatchOn,
      envoy: { ...envoyOn, token: "listener-token", natsSeed: "SUFILESEED" },
    });
    const settings = sessionSettings(
      { path: file, error: null },
      { ENVOY_NATS_URL: "nats://elsewhere.internal.example:4222" },
      { cwd: tempDir() }
    );
    expect(settings.envoy).toMatchObject({
      enabled: true,
      url: envoyOn.url,
      token: "listener-token",
      inbound: {
        off: `ENVOY_NATS_URL names other servers than ${file}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`,
      },
    });
    expect(settings.notice).toBe(
      `Envoy's inbound messages are off: ENVOY_NATS_URL names other servers than ${file}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them; the Envoy tools still work`
    );
  });

  test("a location that cannot be worked out turns both off with that error", () => {
    const error = "XDG_CONFIG_HOME is rel, which is not an absolute path";
    expect(sessionSettings({ path: null, error }, {}, { cwd: tempDir() })).toEqual({
      dispatch: { enabled: false, notice: null, error },
      envoy: { enabled: false, notice: null, error },
      notice: `${error}: Dispatch and Envoy are off`,
    });
  });
});

describe("sessionDispatchSettings", () => {
  test("reads the chosen file again on every call", () => {
    const file = writeFile(path.join(tempDir(), "envoy.json"), { dispatch: dispatchOn });
    const chosen = { path: file, error: null } as const;
    expect(sessionDispatchSettings(chosen, {}, { cwd: tempDir() }).dispatch).toMatchObject({
      enabled: true,
      url: "https://dispatch.example",
    });
    writeFile(file, { dispatch: { ...dispatchOn, serverUrl: "https://moved.dispatch.example" } });
    expect(sessionDispatchSettings(chosen, {}, { cwd: tempDir() }).dispatch).toMatchObject({
      enabled: true,
      url: "https://moved.dispatch.example",
    });
  });

  test("a location that cannot be worked out is an error", () => {
    expect(sessionDispatchSettings({ path: null, error: "no location" }, {}, { cwd: "/" })).toEqual(
      {
        dispatch: { enabled: false, notice: null, error: "no location" },
        warnings: [],
        missing: null,
      }
    );
  });

  test("returns what the read found beside Dispatch: the files it did not read, and a missing file", () => {
    const cwd = tempDir();
    const old = writeFile(path.join(cwd, ".opencode", "envoy.json"), { dispatch: dispatchOn });
    const file = writeFile(path.join(tempDir(), "envoy.json"), { dispatch: dispatchOn });
    expect(sessionDispatchSettings({ path: file, error: null }, {}, { cwd })).toEqual({
      dispatch: { enabled: true, url: "https://dispatch.example", token: "file-token" },
      warnings: [
        `${old} is not read: mkdir -p ${path.join(cwd, ".envoy")} && mv ${old} ${path.join(cwd, ".envoy", "envoy.json")}`,
      ],
      missing: null,
    });
    const absent = path.join(tempDir(), "envoy.json");
    expect(
      sessionDispatchSettings(
        { path: absent, error: null },
        { HOME: tempDir() },
        { cwd: tempDir() }
      )
    ).toEqual({
      dispatch: {
        enabled: false,
        notice: `no envoy.json at ${absent}: Dispatch is off`,
        error: null,
      },
      warnings: [],
      missing: { link: null },
    });
  });
});

describe("handedConfigFile", () => {
  test("names the file ENVOY_CONFIG_FILE names, refuses a relative one, and is null when it is unset or empty", () => {
    expect(handedConfigFile({ ENVOY_CONFIG_FILE: "/handed/envoy.json" })).toEqual({
      path: "/handed/envoy.json",
      error: null,
    });
    expect(handedConfigFile({ ENVOY_CONFIG_FILE: "envoy.json" })).toEqual({
      path: null,
      error: "ENVOY_CONFIG_FILE is envoy.json, which is not an absolute path",
    });
    expect(handedConfigFile({})).toBeNull();
    expect(handedConfigFile({ ENVOY_CONFIG_FILE: "" })).toBeNull();
  });
});

describe("sessionEnvoySettings", () => {
  test("is Envoy from the file, and a location that cannot be worked out is an error", () => {
    const file = writeFile(path.join(tempDir(), "envoy.json"), { envoy: envoyOn });
    expect(
      sessionEnvoySettings({ path: file, error: null }, {}, { cwd: tempDir(), nats: false })
    ).toMatchObject({
      enabled: true,
      url: envoyOn.url,
      inbound: { off: "this session opens no NATS connection" },
    });
    expect(sessionEnvoySettings({ path: null, error: "no location" }, {}, { cwd: "/" })).toEqual({
      enabled: false,
      notice: null,
      error: "no location",
    });
  });
});

describe("the OpenCode-era file", () => {
  test("is not read in place of a missing neutral file: each section still off names the move", () => {
    const home = tempDir();
    const marker = path.join(home, "old-token-ran");
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: {
        enabled: true,
        serverUrl: "https://dispatch.example",
        token: `!touch ${marker}; echo old-token`,
      },
    });
    const neutral = path.join(home, ".config", "legion", "envoy.json");
    const move = `${old} is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`;
    const chosen = { path: neutral, error: null } as const;

    const settings = sessionSettings(chosen, { HOME: home }, { cwd: tempDir() });
    expect(settings.dispatch).toEqual({
      enabled: false,
      notice: `no envoy.json at ${neutral}: Dispatch is off; ${move}`,
      error: null,
    });
    expect(settings.envoy).toMatchObject({ enabled: false, error: null });
    expect(settings.notice).toBe(
      `no envoy.json at ${neutral}: Dispatch and Envoy are off; ${move}`
    );
    const envoyOn = { HOME: home, ENVOY_NATS_URL: "nats://nats.internal.example:4222" };
    expect(sessionSettings(chosen, envoyOn, { cwd: tempDir() }).notice).toBe(
      `no envoy.json at ${neutral}: Dispatch is off; ${move}`
    );
    const dispatchOn = {
      HOME: home,
      DISPATCH_URL: "https://dispatch.example",
      DISPATCH_TOKEN: "environment-token",
    };
    expect(sessionSettings(chosen, dispatchOn, { cwd: tempDir() }).notice).toBe(
      `no envoy.json at ${neutral}: Envoy is off; ${move}`
    );
    expect(existsSync(marker)).toBe(false);

    // A neutral file that exists is read, and nothing is said about the old one.
    writeFile(neutral, { dispatch: { enabled: true, token: "new-token" } });
    expect(sessionSettings(chosen, { HOME: home }, { cwd: tempDir() })).toMatchObject({
      dispatch: { token: "new-token" },
      notice: null,
    });
  });

  test("a checkout's .opencode/envoy.json is named in the start notice, and nothing of it is used", () => {
    const cwd = tempDir();
    const old = writeFile(path.join(cwd, ".opencode", "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "https://old.internal.example", token: "t" },
    });
    const file = writeFile(path.join(tempDir(), "envoy.json"), {
      dispatch: dispatchOn,
      envoy: envoyOn,
    });
    const settings = sessionSettings({ path: file, error: null }, {}, { cwd });
    expect(settings.dispatch).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "file-token",
    });
    expect(settings.notice).toBe(
      `${old} is not read: mkdir -p ${path.join(cwd, ".envoy")} && mv ${old} ${path.join(cwd, ".envoy", "envoy.json")}`
    );
  });

  test("is not read when XDG_CONFIG_HOME puts the neutral file in another directory", () => {
    const home = tempDir();
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {
      dispatch: { enabled: true, token: "old-token" },
    });
    // A Legion pane in the default omp profile: the daemon moves XDG_CONFIG_HOME under its state
    // directory and leaves HOME the operator's.
    const pane = { HOME: home, XDG_CONFIG_HOME: "/var/lib/legion/home/.config" };
    const chosen = sessionConfigFile("omp", pane);
    expect(chosen).toEqual({ path: "/var/lib/legion/home/.config/legion/envoy.json", error: null });
    expect(sessionSettings(chosen, pane, { cwd: tempDir() }).notice).toBe(
      `no envoy.json at /var/lib/legion/home/.config/legion/envoy.json: Dispatch and Envoy are off; ${old} is not read under XDG_CONFIG_HOME=${pane.XDG_CONFIG_HOME}`
    );
  });
});

describe("old user file notices", () => {
  test("a profile's missing file stays off, tells every harness where the old file belongs, and never runs its token", () => {
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
      const env = { HOME: home, ...profile };
      const chosen = sessionConfigFile(harness, env);
      if (chosen.path === null) throw new Error(chosen.error);
      const settings = sessionSettings(chosen, env, { cwd: tempDir() });
      expect(settings.dispatch).toEqual({
        enabled: false,
        notice: `no envoy.json at ${chosen.path}: Dispatch is off; ${old} is not read for this profile; copy or link it to ${chosen.path}, or write that file`,
        error: null,
      });
      expect(settings.envoy).toEqual({
        enabled: false,
        notice: `no envoy.json at ${chosen.path}: Envoy is off; ${old} is not read for this profile; copy or link it to ${chosen.path}, or write that file`,
        error: null,
      });
      expect(settings.notice).toBe(
        `no envoy.json at ${chosen.path}: Dispatch and Envoy are off; ${old} is not read for this profile; copy or link it to ${chosen.path}, or write that file`
      );
      expect(existsSync(marker)).toBe(false);
    }
  });

  test("a relocated XDG neutral file stays off, without reading or asking to move the old file", () => {
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
    const chosen = sessionConfigFile("omp", env);
    if (chosen.path === null) throw new Error(chosen.error);

    const settings = sessionSettings(chosen, env, { cwd: tempDir() });
    expect(settings.notice).toBe(
      `no envoy.json at ${chosen.path}: Dispatch and Envoy are off; ${old} is not read under XDG_CONFIG_HOME=${configHome}`
    );
    expect(settings.notice).not.toContain("move");
    expect(existsSync(marker)).toBe(false);
  });
});
