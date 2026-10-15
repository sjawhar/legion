import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { dispatchSettings } from "../dispatch-config";
import { type EnvoyConfig, readEnvoyConfig } from "../envoy-config";
import { envoySettings } from "../envoy-settings";

/** The error a read failed with, or a test failure naming what it read instead. */
function errorOf(config: EnvoyConfig): string {
  if (config.kind !== "failed") throw new Error(`read ${config.path} without an error`);
  return config.error;
}

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
      token: "file-token",
      inbound: {
        natsUrls: ["nats://nats.internal.example:4222"],
        natsSeed: { value: "SUFILESEED", source: `envoy.natsSeed in ${config.path}` },
      },
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
      token: "environment-token",
      inbound: {
        natsUrls: ["nats://one.internal.example:4222", "nats://two.internal.example:4222"],
        natsSeed: { value: "SUENVSEED", source: "NATS_NKEY_SEED" },
      },
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
      inbound: {
        natsSeed: { value: "SUPOINTERSEED", source: `NATS_NKEY_SEED_FILE (${seedFile})` },
      },
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
      token: undefined,
      inbound: { natsUrls: ["nats://nats.internal.example:4222"], natsSeed: undefined },
      heartbeatMs: 120_000,
    });
  });

  test("a missing file is off with the file's notice", () => {
    const home = tempDir();
    const file = path.join(home, ".config", "legion", "envoy.json");
    const config = readEnvoyConfig(file, { HOME: home }, { cwd: tempDir() });
    expect(envoySettings({}, config)).toEqual({
      enabled: false,
      notice: `no envoy.json at ${file}: Envoy is off`,
      error: null,
    });
  });

  test("an invalid file is off with its error", () => {
    const config = configFrom({ envoy: { enabled: true, bogus: 1 } });
    expect(envoySettings({}, config)).toEqual({
      enabled: false,
      notice: null,
      error: errorOf(config),
    });
    expect(errorOf(config)).toContain("envoy.bogus");
  });

  // An empty variable reads as unset everywhere, as a launcher that writes `X=` means; the two
  // seed variables alone refuse it, since a set seed is authoritative. A seed is for NATS alone, so
  // its refusal turns off inbound messages and leaves the listener on.
  test("an empty ENVOY_* variable reads as unset; an empty seed variable is refused, inbound alone", () => {
    const config = configFrom({ envoy: listener });
    const fromFile = envoySettings({}, config);
    if (!fromFile.enabled) throw new Error(`the file's Envoy is off: ${fromFile.error}`);
    expect(
      envoySettings(
        { ENVOY_URL: "", ENVOY_NATS_URL: "", ENVOY_TOKEN: "", ENVOY_TOKEN_FILE: "" },
        config
      )
    ).toEqual(fromFile);
    for (const [env, refusal] of [
      [{ NATS_NKEY_SEED_FILE: "" }, "NATS_NKEY_SEED_FILE is set but empty"],
      [{ NATS_NKEY_SEED: "" }, "NATS_NKEY_SEED is set but empty"],
    ] as const) {
      expect(envoySettings(env, config)).toEqual({ ...fromFile, inbound: { off: refusal } });
    }
  });

  test("an empty envoy.token is refused, as an empty dispatch.token is", () => {
    const config = configFrom({ envoy: { enabled: true, token: "" } });
    expect(envoySettings({}, config)).toEqual({
      enabled: false,
      notice: null,
      error: `envoy.token in ${config.path} must be a non-empty bearer token`,
    });
  });

  test("a plain ENVOY_TOKEN with whitespace inside is refused without echoing it", () => {
    const config = configFrom({ envoy: listener });
    expect(envoySettings({ ENVOY_TOKEN: "S3CRET\nnotes" }, config)).toEqual({
      enabled: false,
      notice: null,
      error: "ENVOY_TOKEN holds whitespace or a control character",
    });
  });

  test("an ENVOY_URL that is not an http or https URL is refused", () => {
    const config = configFrom({ envoy: { enabled: true } });
    for (const url of ["listener:9020", "nats://listener.internal.example:4222", "not a url"]) {
      expect(envoySettings({ ENVOY_URL: url }, config)).toEqual({
        enabled: false,
        notice: null,
        error: "ENVOY_URL must be an http or https URL",
      });
    }
  });

  test("a section without enabled lends its values, and a seed with no server to use it is not run", () => {
    const home = tempDir();
    const ran = path.join(home, "ran");
    const notEnabled = configFrom({
      envoy: { url: "http://listener.internal.example:9020", token: "file-token" },
    });
    expect(
      envoySettings({ ENVOY_NATS_URL: "nats://nats.internal.example:4222" }, notEnabled)
    ).toMatchObject({
      enabled: true,
      url: "http://listener.internal.example:9020",
      token: "file-token",
    });
    const noServers = configFrom({ envoy: { enabled: true, natsSeed: `!touch ${ran}; echo SU` } });
    expect(envoySettings({ PATH: process.env.PATH }, noServers)).toMatchObject({
      enabled: true,
      inbound: { off: "Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)" },
    });
    expect(existsSync(ran)).toBe(false);
  });

  // OpenCode opens no NATS connection: a seed it would never send must neither run a command nor
  // turn its Envoy off.
  test("nats: false resolves no NATS server and no seed", () => {
    const home = tempDir();
    const ran = path.join(home, "ran");
    const config = configFrom({
      envoy: { ...listener, natsSeed: `!touch ${ran}; exit 1` },
    });
    for (const env of [
      { PATH: process.env.PATH },
      { PATH: process.env.PATH, NATS_NKEY_SEED_FILE: path.join(home, "missing") },
      { PATH: process.env.PATH, ENVOY_NATS_URL: "nats://elsewhere.internal.example:4222" },
    ]) {
      expect(envoySettings(env, config, { nats: false })).toEqual({
        enabled: true,
        url: "http://listener.internal.example:9020",
        token: "file-token",
        inbound: { off: "this session opens no NATS connection" },
        heartbeatMs: 120_000,
      });
    }
    expect(existsSync(ran)).toBe(false);
  });

  test("an unusable ENVOY_TOKEN_FILE is an error naming it, never a fallback", () => {
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
    ];
    for (const [env, message] of cases) {
      const settings = envoySettings(env, config);
      expect(settings).toMatchObject({ enabled: false, notice: null });
      if (settings.enabled) throw new Error(`accepted ${JSON.stringify(env)}`);
      expect(settings.error).toContain(message);
      expect(settings.error).not.toContain("plain-token");
    }
  });

  test("an unusable seed pointer turns off inbound messages alone, naming it, never a fallback", () => {
    const config = configFrom({ envoy: listener });
    const missing = path.join(tempDir(), "missing");
    for (const [env, message] of [
      [
        { NATS_NKEY_SEED_FILE: missing, NATS_NKEY_SEED: "SUENVSEED" },
        `NATS_NKEY_SEED_FILE names ${missing}, which could not be read`,
      ],
      [{ NATS_NKEY_SEED: "  " }, "NATS_NKEY_SEED is set but empty"],
    ] as const) {
      const settings = envoySettings(env, config);
      expect(settings).toMatchObject({
        enabled: true,
        url: "http://listener.internal.example:9020",
        token: "file-token",
      });
      if (!settings.enabled || !("off" in settings.inbound)) {
        throw new Error(`refused ${JSON.stringify(env)}, or took its seed`);
      }
      expect(settings.inbound.off).toContain(message);
      expect(settings.inbound.off).not.toContain("SUENVSEED");
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
    // An origin is scheme, host and port: each differs from the file's listener on its own.
    for (const url of [
      "https://listener.internal.example:9020",
      "http://listener.internal.example:9021",
      "http://listener.internal.example",
    ]) {
      expect(envoySettings({ ENVOY_URL: url }, config)).toMatchObject({ enabled: false });
    }
  });

  test("a file's token with no listener of its own goes only to the default listener", () => {
    const bare = configFrom({ envoy: { enabled: true, token: "file-token" } });
    expect(envoySettings({ ENVOY_URL: "http://other.internal.example:9020" }, bare)).toEqual({
      enabled: false,
      notice: null,
      error: `ENVOY_URL names http://other.internal.example:9020, but ${bare.path}'s envoy.token is for http://127.0.0.1:9020; set ENVOY_TOKEN or ENVOY_TOKEN_FILE for it`,
    });
    expect(envoySettings({ ENVOY_URL: "http://127.0.0.1:9020/v1" }, bare)).toMatchObject({
      enabled: true,
      token: "file-token",
    });
  });
});

// nkey sign-in sends a signature of the server's nonce, never the seed, but a server that is not the
// real one can relay the real server's nonce and replay the signature. A file's seed therefore goes
// only with the servers written in that same file.
describe("the NATS seed in envoy.json", () => {
  const fileServers = ["nats://one.internal.example:4222", "nats://two.internal.example:4222"];

  test("a lone file's seed goes with its own servers, trimmed as read", () => {
    const config = configFrom({
      envoy: {
        enabled: true,
        natsUrls: ["nats://one.internal.example:4222 "],
        natsSeed: "SUFILESEED",
      },
    });
    expect(envoySettings({}, config)).toMatchObject({
      enabled: true,
      inbound: {
        natsUrls: ["nats://one.internal.example:4222"],
        natsSeed: { value: "SUFILESEED", source: `envoy.natsSeed in ${config.path}` },
      },
    });
  });

  // A refused seed turns off inbound messages alone: the listener, its bearer and its tools stay.
  test("goes with ENVOY_NATS_URL only when it names the file's servers, and its command does not run otherwise", () => {
    const home = tempDir();
    const runs = path.join(home, "runs");
    const config = configFrom({
      envoy: {
        enabled: true,
        url: "http://listener.internal.example:9020",
        token: "file-token",
        natsUrls: fileServers,
        natsSeed: `!echo run >> ${runs}; echo SUSEED`,
      },
    });
    const other = {
      PATH: process.env.PATH,
      ENVOY_NATS_URL: "nats://elsewhere.internal.example:4222",
    };
    expect(envoySettings(other, config)).toEqual({
      enabled: true,
      url: "http://listener.internal.example:9020",
      token: "file-token",
      inbound: {
        off: `ENVOY_NATS_URL names other servers than ${config.path}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`,
      },
      heartbeatMs: 120_000,
    });
    expect(existsSync(runs)).toBe(false);
    // A set holding one other server is other servers too, whether it replaces one of the file's
    // or adds to them.
    for (const mixed of [
      "nats://one.internal.example:4222,nats://elsewhere.internal.example:4222",
      "nats://one.internal.example:4222,nats://two.internal.example:4222,nats://elsewhere.internal.example:4222",
    ]) {
      expect(
        envoySettings({ PATH: process.env.PATH, ENVOY_NATS_URL: mixed }, config)
      ).toMatchObject({ enabled: true, inbound: { off: expect.any(String) } });
    }
    expect(existsSync(runs)).toBe(false);

    const same = {
      PATH: process.env.PATH,
      ENVOY_NATS_URL:
        " nats://two.internal.example:4222,nats://one.internal.example:4222, nats://two.internal.example:4222",
    };
    expect(envoySettings(same, config)).toMatchObject({
      enabled: true,
      inbound: { natsSeed: { value: "SUSEED", source: `envoy.natsSeed in ${config.path}` } },
    });
  });

  // Two spellings of one server pair: scheme and host lower-cased, a trailing `/` dropped, and the
  // default port 4222 written when none is.
  test("compares ENVOY_NATS_URL with the file's servers normalised", () => {
    const config = configFrom({
      envoy: { natsUrls: ["nats://one.internal.example:4222"], natsSeed: "SUFILESEED" },
    });
    for (const named of [
      "nats://one.internal.example:4222/",
      "NATS://One.Internal.Example:4222",
      "nats://one.internal.example",
    ]) {
      expect(envoySettings({ ENVOY_NATS_URL: named }, config)).toMatchObject({
        enabled: true,
        inbound: { natsUrls: [named], natsSeed: { value: "SUFILESEED" } },
      });
    }
    for (const other of ["nats://one.internal.example:4223", "tls://one.internal.example:4222"]) {
      expect(envoySettings({ ENVOY_NATS_URL: other }, config)).toMatchObject({
        enabled: true,
        inbound: { off: expect.stringContaining("ENVOY_NATS_URL names other servers") },
      });
    }
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
      inbound: {
        natsUrls: ["nats://elsewhere.internal.example:4222"],
        natsSeed: { value: "SUPOINTERSEED", source: `NATS_NKEY_SEED_FILE (${seedFile})` },
      },
    });
  });

  test("a checkout's servers are not read, and a seed in the variables goes with the pane's servers", () => {
    const user = path.join(tempDir(), "envoy.json");
    writeFileSync(
      user,
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "user-token" },
        envoy: { enabled: true, natsUrls: fileServers },
      })
    );
    const cwd = tempDir();
    mkdirSync(path.join(cwd, ".envoy"));
    writeFileSync(
      path.join(cwd, ".envoy", "envoy.json"),
      JSON.stringify({ envoy: { natsUrls: ["nats://repo.internal.example:4222"] } })
    );
    const env = { HOME: tempDir(), NATS_NKEY_SEED: "SUENVSEED" };
    const config = readEnvoyConfig(user, env, { cwd });
    expect(envoySettings(env, config)).toMatchObject({
      enabled: true,
      inbound: {
        natsUrls: fileServers,
        natsSeed: { value: "SUENVSEED", source: "NATS_NKEY_SEED" },
      },
    });
    expect(dispatchSettings(env, config)).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: "user-token",
    });
  });
});
