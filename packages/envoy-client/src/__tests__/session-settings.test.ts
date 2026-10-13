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
      dispatch: { enabled: false, url: null, token: null, error: null },
      envoy: {
        enabled: false,
        notice: `no envoy.json at ${file}: Dispatch and Envoy are off`,
        error: null,
      },
      notice: `no envoy.json at ${file}: Dispatch and Envoy are off`,
    });
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
      "Dispatch is off: dispatch.token must be a non-empty bearer token"
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
    expect(settings.dispatch.enabled).toBe(false);
    expect(settings.envoy.enabled).toBe(false);
    expect(settings.dispatch.error).not.toBeNull();
    expect(settings.notice).toBe(`${settings.dispatch.error}: Dispatch and Envoy are off`);
  });

  test("a location that cannot be worked out turns both off with that error", () => {
    const error = "XDG_CONFIG_HOME is rel, which is not an absolute path";
    expect(sessionSettings({ path: null, error }, {}, { cwd: tempDir() })).toEqual({
      dispatch: { enabled: false, url: null, token: null, error },
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
      {
        enabled: false,
        url: null,
        token: null,
        error: "no location",
      }
    );
  });
});
