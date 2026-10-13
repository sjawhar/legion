import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { sessionConfigFile, sessionDispatchSettings, sessionSettings } from "../session-settings";

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
    expect(settings.envoy).toMatchObject({ enabled: true, natsUrls: envoyOn.natsUrls });
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
    expect(sessionDispatchSettings(chosen, {}, { cwd: tempDir() })).toMatchObject({
      enabled: true,
      url: "https://dispatch.example",
    });
    writeFile(file, { dispatch: { ...dispatchOn, serverUrl: "https://moved.dispatch.example" } });
    expect(sessionDispatchSettings(chosen, {}, { cwd: tempDir() })).toMatchObject({
      enabled: true,
      url: "https://moved.dispatch.example",
    });
  });

  test("a location that cannot be worked out is an error", () => {
    expect(sessionDispatchSettings({ path: null, error: "no location" }, {}, { cwd: "/" })).toEqual(
      { enabled: false, notice: null, error: "no location" }
    );
  });
});

describe("the old OpenCode file", () => {
  test("is named whenever the neutral file is missing and it is still there, a section on or not", () => {
    const home = tempDir();
    const old = writeFile(path.join(home, ".config", "opencode", "envoy.json"), {});
    const neutral = path.join(home, ".config", "legion", "envoy.json");
    const hint = `${old} is no longer read: move it to ${neutral}`;
    const chosen = { path: neutral, error: null } as const;

    expect(sessionSettings(chosen, { HOME: home }, { cwd: tempDir() }).notice).toBe(
      `no envoy.json at ${neutral}: Dispatch and Envoy are off; ${hint}`
    );
    const envoyOn = { HOME: home, ENVOY_NATS_URL: "nats://nats.internal.example:4222" };
    expect(sessionSettings(chosen, envoyOn, { cwd: tempDir() }).notice).toBe(
      `no envoy.json at ${neutral}: Dispatch is off; ${hint}`
    );
    const dispatchOn = {
      HOME: home,
      DISPATCH_URL: "https://dispatch.example",
      DISPATCH_TOKEN: "environment-token",
    };
    expect(sessionSettings(chosen, dispatchOn, { cwd: tempDir() }).notice).toBe(
      `no envoy.json at ${neutral}: Envoy is off; ${hint}`
    );
  });

  test("is not named when XDG_CONFIG_HOME puts the neutral file in another directory", () => {
    const home = tempDir();
    writeFile(path.join(home, ".config", "opencode", "envoy.json"), {});
    // A Legion pane in the default omp profile: the daemon moves XDG_CONFIG_HOME under its state
    // directory and leaves HOME the operator's.
    const pane = { HOME: home, XDG_CONFIG_HOME: "/var/lib/legion/home/.config" };
    const chosen = sessionConfigFile("omp", pane);
    expect(chosen).toEqual({ path: "/var/lib/legion/home/.config/legion/envoy.json", error: null });
    expect(sessionSettings(chosen, pane, { cwd: tempDir() }).notice).toBe(
      "no envoy.json at /var/lib/legion/home/.config/legion/envoy.json: Dispatch and Envoy are off"
    );
  });
});
