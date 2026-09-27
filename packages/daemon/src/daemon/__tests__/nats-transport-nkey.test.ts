import { afterAll, beforeAll, describe, expect, it } from "bun:test";
import { connect, nkeys } from "nats";
import { createNatsTransport } from "../nats-transport";
import { config } from "./ci-fixtures";
import { type NatsServer, startNatsServer } from "./nats-server";

interface TestKeyPair {
  getSeed(): Uint8Array;
  getPublicKey(): string;
}

/** nats.js types its `nkeys` export `any`. */
const testKeys: { createUser(): TestKeyPair } = nkeys;

function createUser(): { readonly seed: string; readonly publicKey: string } {
  const user = testKeys.createUser();
  return { seed: new TextDecoder().decode(user.getSeed()), publicKey: user.getPublicKey() };
}

/** The daemon's own transport, on the real client, against url, as the seed's user (none when
 * undefined). */
async function transportAt(url: string, natsNkeySeed: string | undefined) {
  const transport = await createNatsTransport(
    { ...config(), natsUrls: [url], ...(natsNkeySeed === undefined ? {} : { natsNkeySeed }) },
    connect
  );
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
    }, 60_000);

    it("with no seed, connects to a server that asks for no credential", async () => {
      const transport = await transportAt(open.url, undefined);
      await transport.close();
    });

    it("with the user's seed, connects to a server that requires it", async () => {
      const transport = await transportAt(authorized.url, user.seed);
      await transport.close();
    });

    it("with no seed, is refused by a server that requires one", async () => {
      await expect(transportAt(authorized.url, undefined)).rejects.toThrow(
        /authorization violation/i
      );
    });
  }
);
