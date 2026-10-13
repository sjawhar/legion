import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { envoySettings } from "../defaults";
import { dispatchSettings } from "../dispatch-config";
import { readEnvoyConfig } from "../envoy-config";

function tempDir(): string {
  return mkdtempSync(path.join(os.tmpdir(), "envoy-settings-"));
}

/** The configuration read from a file holding `contents`, with no checkout file beside it. */
function configFrom(contents: unknown) {
  const file = path.join(tempDir(), "envoy.json");
  writeFileSync(file, JSON.stringify(contents));
  return readEnvoyConfig(file, { HOME: tempDir() }, { cwd: tempDir() });
}

function secretFile(contents: string): string {
  const file = path.join(tempDir(), "secret");
  writeFileSync(file, contents, { mode: 0o600 });
  return file;
}

const listener = {
  enabled: true,
  url: "http://listener.internal.example:9020/",
  natsUrls: ["nats://nats.internal.example:4222"],
  token: "file-token",
  natsSeed: "SUFILESEED",
};

describe("envoySettings", () => {
  test("takes url, natsUrls, token and natsSeed from the file", () => {
    const config = configFrom({ envoy: listener });
    expect(envoySettings({}, config)).toEqual({
      enabled: true,
      url: "http://listener.internal.example:9020",
      natsUrls: ["nats://nats.internal.example:4222"],
      token: "file-token",
      natsSeed: { value: "SUFILESEED", source: `envoy.natsSeed in ${config.path}` },
      heartbeatMs: 120_000,
    });
  });

  test("each variable overrides its own field", () => {
    const config = configFrom({ envoy: listener });
    expect(
      envoySettings(
        {
          ENVOY_URL: "http://other-listener.internal.example:9020/",
          ENVOY_NATS_URL: "nats://one.internal.example:4222, nats://two.internal.example:4222",
          ENVOY_TOKEN: "environment-token",
          NATS_NKEY_SEED: " SUENVSEED ",
          ENVOY_HEARTBEAT_MS: "10",
        },
        config
      )
    ).toEqual({
      enabled: true,
      url: "http://other-listener.internal.example:9020",
      natsUrls: ["nats://one.internal.example:4222", "nats://two.internal.example:4222"],
      token: "environment-token",
      natsSeed: { value: "SUENVSEED", source: "NATS_NKEY_SEED" },
      heartbeatMs: 25,
    });

    const tokenFile = secretFile("  pointer-token\n");
    const seedFile = secretFile("\nSUPOINTERSEED\n");
    const pointers = envoySettings(
      {
        ENVOY_TOKEN: "environment-token",
        ENVOY_TOKEN_FILE: tokenFile,
        NATS_NKEY_SEED: "SUENVSEED",
        NATS_NKEY_SEED_FILE: seedFile,
      },
      config
    );
    expect(pointers).toMatchObject({
      enabled: true,
      token: "pointer-token",
      natsSeed: { value: "SUPOINTERSEED", source: `NATS_NKEY_SEED_FILE (${seedFile})` },
    });
  });

  test("a section with enabled: false is off whatever the variables say, and the notice says so", () => {
    const config = configFrom({ envoy: { ...listener, enabled: false } });
    expect(
      envoySettings(
        {
          ENVOY_URL: "http://listener.internal.example:9020",
          ENVOY_NATS_URL: "nats://nats.internal.example:4222",
          ENVOY_TOKEN: "environment-token",
        },
        config
      )
    ).toEqual({
      enabled: false,
      notice: `${config.path} sets envoy.enabled to false: Envoy is off`,
      error: null,
    });
  });

  test("no envoy section and no variables is off, as a missing ENVOY_NATS_URL is", () => {
    const config = configFrom({ dispatch: { enabled: false } });
    const settings = envoySettings({}, config);
    expect(settings.enabled).toBe(false);
    expect(settings).toMatchObject({ error: null });

    // The variables alone still name an Envoy: how a Legion pane gets the one its daemon names.
    expect(envoySettings({ ENVOY_NATS_URL: "nats://nats.internal.example:4222" }, config)).toEqual({
      enabled: true,
      url: "http://127.0.0.1:9020",
      natsUrls: ["nats://nats.internal.example:4222"],
      token: undefined,
      natsSeed: undefined,
      heartbeatMs: 120_000,
    });
  });

  test("a missing file is off with the file's notice", () => {
    const home = tempDir();
    const file = path.join(home, ".config", "legion", "envoy.json");
    const config = readEnvoyConfig(file, { HOME: home }, { cwd: tempDir() });
    expect(envoySettings({}, config)).toEqual({
      enabled: false,
      notice: `no envoy.json at ${file}: Dispatch and Envoy are off`,
      error: null,
    });
  });

  test("an invalid file is off with its error", () => {
    const config = configFrom({ envoy: { enabled: true, bogus: 1 } });
    expect(envoySettings({}, config)).toEqual({
      enabled: false,
      notice: null,
      error: config.error,
    });
    expect(config.error).toContain("envoy.bogus");
  });

  test("an unusable ENVOY_TOKEN_FILE or seed pointer is an error naming it, never a fallback", () => {
    const config = configFrom({ envoy: listener });
    const dir = tempDir();
    const missing = path.join(dir, "missing");
    const blank = secretFile(" \n");
    const cases: [Record<string, string>, string][] = [
      [
        { ENVOY_TOKEN_FILE: missing, ENVOY_TOKEN: "plain-token" },
        `ENVOY_TOKEN_FILE names ${missing}, which could not be read: ENOENT`,
      ],
      [{ ENVOY_TOKEN_FILE: blank }, `ENVOY_TOKEN_FILE names ${blank}, which is empty`],
      [
        { NATS_NKEY_SEED_FILE: missing, NATS_NKEY_SEED: "SUENVSEED" },
        `NATS_NKEY_SEED_FILE names ${missing}, which could not be read`,
      ],
      [{ NATS_NKEY_SEED: "  " }, "NATS_NKEY_SEED is set but empty"],
    ];
    for (const [env, message] of cases) {
      const settings = envoySettings(env, config);
      expect(settings).toMatchObject({ enabled: false, notice: null });
      if (settings.enabled) throw new Error(`accepted ${JSON.stringify(env)}`);
      expect(settings.error).toContain(message);
      expect(settings.error).not.toContain("plain-token");
    }
  });

  test("refuses ENVOY_URL at another origin than the file's listener when the token is the file's", () => {
    const config = configFrom({ envoy: listener });
    expect(envoySettings({ ENVOY_URL: "http://other.internal.example:9020" }, config)).toEqual({
      enabled: false,
      notice: null,
      error: `ENVOY_URL names http://other.internal.example:9020, but ${config.path}'s envoy.token is for http://listener.internal.example:9020; set ENVOY_TOKEN or ENVOY_TOKEN_FILE for it`,
    });
    expect(
      envoySettings({ ENVOY_URL: "http://listener.internal.example:9020/v1" }, config)
    ).toMatchObject({
      enabled: true,
      url: "http://listener.internal.example:9020/v1",
      token: "file-token",
    });
  });

  test("a checkout's listener goes only with the checkout's own token, never ENVOY_TOKEN", () => {
    const user = path.join(tempDir(), "envoy.json");
    writeFileSync(user, JSON.stringify({ envoy: listener }));
    const cwd = tempDir();
    mkdirSync(path.join(cwd, ".envoy"));
    writeFileSync(
      path.join(cwd, ".envoy", "envoy.json"),
      JSON.stringify({ envoy: { url: "http://repo.internal.example:9020", token: "repo-token" } })
    );
    const config = readEnvoyConfig(user, { HOME: tempDir() }, { cwd });
    expect(envoySettings({ ENVOY_TOKEN: "environment-token" }, config)).toMatchObject({
      enabled: true,
      url: "http://repo.internal.example:9020",
      token: "repo-token",
    });
  });

  test("compares ENVOY_URL with the listener of the file the token comes from, never a checkout's listener beside another file's token", () => {
    const user = path.join(tempDir(), "envoy.json");
    writeFileSync(user, JSON.stringify({ envoy: listener }));
    const cwd = tempDir();
    mkdirSync(path.join(cwd, ".envoy"));
    const checkout = path.join(cwd, ".envoy", "envoy.json");
    writeFileSync(
      checkout,
      JSON.stringify({ envoy: { url: "http://staging.internal.example:9020" } })
    );
    const env = { ENVOY_URL: "http://staging.internal.example:9020" };

    expect(envoySettings(env, readEnvoyConfig(user, { HOME: tempDir() }, { cwd }))).toEqual({
      enabled: false,
      notice: null,
      error: `ENVOY_URL names http://staging.internal.example:9020, but ${user}'s envoy.token is for http://listener.internal.example:9020; set ENVOY_TOKEN or ENVOY_TOKEN_FILE for it`,
    });

    // The checkout's own token goes with the checkout's own listener.
    writeFileSync(
      checkout,
      JSON.stringify({
        envoy: { url: "http://staging.internal.example:9020", token: "repo-token" },
      })
    );
    expect(envoySettings(env, readEnvoyConfig(user, { HOME: tempDir() }, { cwd }))).toMatchObject({
      enabled: true,
      url: "http://staging.internal.example:9020",
      token: "repo-token",
    });
  });
});

// nkey sign-in sends a signature of the server's nonce, never the seed, but a server that is not the
// real one can relay the real server's nonce and replay the signature. A file's seed therefore goes
// only with the servers written in that same file.
describe("the NATS seed in envoy.json", () => {
  const fileServers = ["nats://one.internal.example:4222", "nats://two.internal.example:4222"];

  test("a lone file's seed goes with its own servers, whitespace and all", () => {
    const config = configFrom({
      envoy: {
        enabled: true,
        natsUrls: ["nats://one.internal.example:4222 "],
        natsSeed: "SUFILESEED",
      },
    });
    expect(envoySettings({}, config)).toMatchObject({
      enabled: true,
      natsSeed: { value: "SUFILESEED", source: `envoy.natsSeed in ${config.path}` },
    });
  });

  test("goes with ENVOY_NATS_URL only when it names the file's servers, and its command does not run otherwise", () => {
    const home = tempDir();
    const runs = path.join(home, "runs");
    const config = configFrom({
      envoy: {
        enabled: true,
        natsUrls: fileServers,
        natsSeed: `!echo run >> ${runs}; echo SUSEED`,
      },
    });
    const other = {
      PATH: process.env.PATH,
      ENVOY_NATS_URL: "nats://elsewhere.internal.example:4222",
    };
    expect(envoySettings(other, config)).toEqual({
      enabled: false,
      notice: null,
      error: `ENVOY_NATS_URL names other servers than ${config.path}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`,
    });
    expect(existsSync(runs)).toBe(false);

    const same = {
      PATH: process.env.PATH,
      ENVOY_NATS_URL:
        " nats://two.internal.example:4222,nats://one.internal.example:4222, nats://two.internal.example:4222",
    };
    expect(envoySettings(same, config)).toMatchObject({
      enabled: true,
      natsSeed: { value: "SUSEED", source: `envoy.natsSeed in ${config.path}` },
    });
  });

  test("a seed from the variables goes with whatever servers ENVOY_NATS_URL names", () => {
    const config = configFrom({
      envoy: { enabled: true, natsUrls: fileServers, natsSeed: "SUFILESEED" },
    });
    const seedFile = secretFile("SUPOINTERSEED\n");
    expect(
      envoySettings(
        {
          ENVOY_NATS_URL: "nats://elsewhere.internal.example:4222",
          NATS_NKEY_SEED_FILE: seedFile,
        },
        config
      )
    ).toMatchObject({
      enabled: true,
      natsUrls: ["nats://elsewhere.internal.example:4222"],
      natsSeed: { value: "SUPOINTERSEED", source: `NATS_NKEY_SEED_FILE (${seedFile})` },
    });
  });

  test("a checkout's servers need the checkout's own seed, for Envoy only", () => {
    const user = path.join(tempDir(), "envoy.json");
    writeFileSync(
      user,
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "user-token" },
        envoy: { enabled: true, natsUrls: fileServers, natsSeed: "SUFILESEED" },
      })
    );
    const cwd = tempDir();
    mkdirSync(path.join(cwd, ".envoy"));
    const checkout = path.join(cwd, ".envoy", "envoy.json");
    writeFileSync(
      checkout,
      JSON.stringify({ envoy: { natsUrls: ["nats://repo.internal.example:4222"] } })
    );
    const env = { HOME: tempDir() };
    const withoutSeed = readEnvoyConfig(user, env, { cwd });
    expect(envoySettings(env, withoutSeed)).toEqual({
      enabled: false,
      notice: null,
      error: `${checkout}: envoy.natsUrls requires envoy.natsSeed in the same file`,
    });
    expect(dispatchSettings(env, withoutSeed)).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "user-token",
      error: null,
    });

    writeFileSync(
      checkout,
      JSON.stringify({
        envoy: { natsUrls: ["nats://repo.internal.example:4222"], natsSeed: "SUREPOSEED" },
      })
    );
    expect(envoySettings(env, readEnvoyConfig(user, env, { cwd }))).toMatchObject({
      enabled: true,
      natsUrls: ["nats://repo.internal.example:4222"],
      natsSeed: { value: "SUREPOSEED", source: `envoy.natsSeed in ${checkout}` },
    });
  });
});
