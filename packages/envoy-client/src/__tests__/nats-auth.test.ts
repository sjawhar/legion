import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { connect, nkeys } from "nats";
import { natsAuthOptions } from "../nats-auth";
import { type NatsServer, startNatsServer } from "./nats-server";

interface TestKeyPair {
  getSeed(): Uint8Array;
  getPublicKey(): string;
}

/** nats.js types its `nkeys` export `any`. */
const testKeys: { createUser(): TestKeyPair; createAccount(): TestKeyPair } = nkeys;

function createUser(): { readonly seed: string; readonly publicKey: string } {
  const user = testKeys.createUser();
  return { seed: new TextDecoder().decode(user.getSeed()), publicKey: user.getPublicKey() };
}

function seedFile(contents: string): string {
  const file = path.join(mkdtempSync(path.join(os.tmpdir(), "nats-seed-")), "seed");
  writeFileSync(file, contents, { mode: 0o600 });
  return file;
}

/** Connects as a client of this package does: `servers` plus the authenticator the environment
 * names, and nothing else that bears on authentication. */
async function connectWith(url: string, env: NodeJS.ProcessEnv) {
  const connection = await connect({
    servers: [url],
    name: "nats-auth-test",
    ...natsAuthOptions(env),
    reconnect: false,
  });
  await connection.flush();
  return connection;
}

// Docker-backed: CI runs this package's tests with LEGION_E2E=1 after pulling the image.
describe.skipIf(process.env.LEGION_E2E !== "1")("natsAuthOptions against a real NATS", () => {
  const user = createUser();
  let open: NatsServer;
  let authorized: NatsServer;

  beforeAll(async () => {
    [open, authorized] = await Promise.all([startNatsServer(), startNatsServer(user.publicKey)]);
  }, 120_000);
  afterAll(() => {
    open?.stop();
    authorized?.stop();
  }, 60_000);

  test("with no seed, connects to a server that asks for no credential", async () => {
    const connection = await connectWith(open.url, {});
    await connection.close();
  });

  test("with the user's seed, from the file (which wins) or the variable, connects to a server that requires it", async () => {
    for (const env of [
      { NATS_NKEY_SEED_FILE: seedFile(`\n  ${user.seed}\n`), NATS_NKEY_SEED: "not a seed" },
      { NATS_NKEY_SEED: user.seed },
    ]) {
      const connection = await connectWith(authorized.url, env);
      await connection.close();
    }
  });

  test("with no seed, is refused by a server that requires one", async () => {
    await expect(connectWith(authorized.url, {})).rejects.toThrow(/authorization violation/i);
  });
});

describe("natsAuthOptions", () => {
  const user = createUser();

  test("is no option when neither variable is set", () => {
    expect(natsAuthOptions({})).toEqual({});
  });

  test("refuses an unusable seed naming its variable and path, never falling back", () => {
    const dir = mkdtempSync(path.join(os.tmpdir(), "nats-seed-"));
    const missing = path.join(dir, "missing");
    const blank = seedFile(" \n");
    const notASeed = seedFile("SUNOTASEED");
    const account = seedFile(new TextDecoder().decode(testKeys.createAccount().getSeed()));
    const cases: [NodeJS.ProcessEnv, string][] = [
      [
        { NATS_NKEY_SEED_FILE: missing, NATS_NKEY_SEED: user.seed },
        `NATS_NKEY_SEED_FILE names ${missing}, which could not be read`,
      ],
      [{ NATS_NKEY_SEED_FILE: blank }, `NATS_NKEY_SEED_FILE names ${blank}, which is empty`],
      [{ NATS_NKEY_SEED_FILE: "" }, "NATS_NKEY_SEED_FILE is set but empty"],
      [
        { NATS_NKEY_SEED_FILE: notASeed },
        `NATS_NKEY_SEED_FILE (${notASeed}) does not hold a valid nkey seed`,
      ],
      [
        { NATS_NKEY_SEED_FILE: account },
        `NATS_NKEY_SEED_FILE (${account}) holds an nkey seed that is not a user's`,
      ],
      [{ NATS_NKEY_SEED: "  " }, "NATS_NKEY_SEED is set but empty"],
      [{ NATS_NKEY_SEED: "hunter2" }, "NATS_NKEY_SEED does not hold a valid nkey seed"],
    ];
    for (const [env, message] of cases) {
      let thrown: unknown;
      try {
        natsAuthOptions(env);
      } catch (error) {
        thrown = error;
      }
      if (!(thrown instanceof Error)) throw new Error(`accepted ${JSON.stringify(env)}`);
      const text = thrown.message;
      expect(text).toContain(message);
      expect(text).not.toContain(user.seed);
      expect(text).not.toContain("hunter2");
    }
  });
});
