import { afterAll, beforeAll, describe, expect, it } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { connect, nkeys } from "nats";
import { createNatsTransport, natsAuthOptions } from "../nats-transport";
import { config } from "./ci-fixtures";
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

/** The daemon's own transport, on the real client, against url, with env as its environment. */
async function transportAt(url: string, env: NodeJS.ProcessEnv) {
  const transport = await createNatsTransport({ ...config(), natsUrls: [url] }, connect, env);
  await transport.ready();
  return transport;
}

// Docker-backed: CI runs the daemon's tests with LEGION_E2E=1 after pulling the image.
describe.skipIf(process.env.LEGION_E2E !== "1")(
  "the daemon's NATS transport against a real NATS",
  () => {
    const user = createUser();
    let open: NatsServer;
    let authorized: NatsServer;

    beforeAll(async () => {
      [open, authorized] = await Promise.all([startNatsServer(), startNatsServer(user.publicKey)]);
    }, 120_000);
    afterAll(() => {
      open?.stop();
      authorized?.stop();
    });

    it("with no seed, connects to a server that asks for no credential", async () => {
      const transport = await transportAt(open.url, {});
      await transport.close();
    });

    it("with the user's seed, from the file (which wins) or the variable, connects to a server that requires it", async () => {
      for (const env of [
        { NATS_NKEY_SEED_FILE: seedFile(`\n  ${user.seed}\n`), NATS_NKEY_SEED: "not a seed" },
        { NATS_NKEY_SEED: user.seed },
      ]) {
        const transport = await transportAt(authorized.url, env);
        await transport.close();
      }
    });

    it("with no seed, is refused by a server that requires one", async () => {
      await expect(transportAt(authorized.url, {})).rejects.toThrow(/authorization violation/i);
    });
  }
);

describe("natsAuthOptions", () => {
  const user = createUser();

  it("is no option when neither variable is set", () => {
    expect(natsAuthOptions({})).toEqual({});
  });

  it("refuses an unusable seed naming its variable and path before any dial, never falling back", async () => {
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
      let dialed = false;
      const refusal = createNatsTransport(
        config(),
        async () => {
          dialed = true;
          throw new Error("dialed");
        },
        env
      );
      await expect(refusal).rejects.toThrow(message);
      const text = await refusal.catch((error: unknown) => String(error));
      expect(text).not.toContain(user.seed);
      expect(text).not.toContain("hunter2");
      expect(dialed).toBe(false);
    }
  });
});
