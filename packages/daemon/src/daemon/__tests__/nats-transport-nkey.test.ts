import { afterAll, beforeAll, describe, expect, it } from "bun:test";
import { connect, nkeys } from "nats";
import type { DaemonConfig } from "../config";
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

/** The daemon's own transport, on the real client, against url, with `seeds` (the pane seed
 * `natsNkeySeed` and the daemon seed `natsDaemonNkeySeed`, each optional), recording its log;
 * `errors(n)` resolves once n error lines are recorded. */
async function transportAt(
  url: string,
  seeds: Pick<DaemonConfig, "natsNkeySeed" | "natsDaemonNkeySeed"> = {}
) {
  const lines = { info: [] as string[], error: [] as string[] };
  const waiters: { count: number; resolve: () => void }[] = [];
  const transport = await createNatsTransport({ ...config(), natsUrls: [url], ...seeds }, connect, {
    info: (line) => lines.info.push(line),
    error: (line) => {
      lines.error.push(line);
      for (const waiter of waiters) if (lines.error.length >= waiter.count) waiter.resolve();
    },
  });
  await transport.ready();
  const errors = (count: number) =>
    lines.error.length >= count
      ? Promise.resolve()
      : new Promise<void>((resolve) => waiters.push({ count, resolve }));
  return { transport, lines, errors };
}

/** The exceptions lane the daemon subscribes to (events.ts), which only the daemon user holds. */
const EXCEPTIONS = "notifications.envoy.exceptions.notifications.role.>";

// Docker-backed: CI runs the daemon's tests with LEGION_E2E=1 after pulling the image.
describe.skipIf(process.env.LEGION_E2E !== "1")(
  "the daemon's NATS transport against a real NATS",
  () => {
    const pane = createUser();
    const daemon = createUser();
    let open: NatsServer;
    let authorized: NatsServer;
    let daemonOnly: NatsServer;
    let denying: NatsServer;

    beforeAll(async () => {
      [open, authorized, daemonOnly, denying] = await Promise.all([
        startNatsServer(),
        startNatsServer([{ nkey: pane.publicKey }]),
        startNatsServer([{ nkey: daemon.publicKey }]),
        startNatsServer([
          {
            nkey: daemon.publicKey,
            permissions: {
              subscribe: { deny: [EXCEPTIONS] },
              publish: { deny: ["notifications.role.denied"] },
            },
          },
        ]),
      ]);
    }, 120_000);
    afterAll(() => {
      for (const server of [open, authorized, daemonOnly, denying]) server?.stop();
    }, 60_000);

    it("with no seed, connects to a server that asks for no credential", async () => {
      const { transport } = await transportAt(open.url);
      await transport.close();
    });

    it("with the pane seed alone, connects as the pane user to a server that requires it", async () => {
      const { transport } = await transportAt(authorized.url, { natsNkeySeed: pane.seed });
      await transport.close();
    });

    it("with no seed, is refused by a server that requires one", async () => {
      await expect(transportAt(authorized.url)).rejects.toThrow(/authorization violation/i);
    });

    it("with both seeds, connects as the daemon user, which a server admitting only it accepts", async () => {
      const { transport } = await transportAt(daemonOnly.url, {
        natsNkeySeed: pane.seed,
        natsDaemonNkeySeed: daemon.seed,
      });
      await transport.close();
      await expect(transportAt(daemonOnly.url, { natsNkeySeed: pane.seed })).rejects.toThrow(
        /authorization violation/i
      );
    });

    it("logs a subscription and a publish the server refuses at error, naming each subject", async () => {
      const { transport, lines, errors } = await transportAt(denying.url, {
        natsDaemonNkeySeed: daemon.seed,
      });
      transport.subscribe(EXCEPTIONS, () => {});
      transport.publish("notifications.role.denied", "{}");
      // The server answers each refusal before the flush's PONG; without a handler reporting them,
      // this wait is what times out.
      await transport.flush();
      await errors(2);
      await transport.close();
      expect(lines.error).toEqual([
        `[legion] NATS refused the daemon's subscription to ${EXCEPTIONS} (Permissions Violation): the daemon's NATS user lacks that grant`,
        "[legion] NATS refused the daemon's publish to notifications.role.denied (Permissions Violation): the daemon's NATS user lacks that grant",
      ]);
      for (const line of [...lines.info, ...lines.error]) expect(line).not.toContain(daemon.seed);
    });
  }
);
