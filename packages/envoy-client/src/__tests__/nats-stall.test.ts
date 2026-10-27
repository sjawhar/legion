import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { connect, Events, StringCodec } from "nats";
import { type EnvoyStack, envoyStackEnabled, startEnvoyStack } from "../../test/envoy-stack";
import { startStallProxy } from "../../test/stall-proxy";
import { natsConnectionOptions } from "../nats-connection";

/** The spec's bound: a session finds a path that answers within a minute of the freeze. */
const RECOVERY_BOUND_MS = 60_000;

const codec = StringCodec();

describe.skipIf(!envoyStackEnabled)("a NATS connection whose path freezes", () => {
  let stack: EnvoyStack | undefined;

  beforeAll(async () => {
    stack = await startEnvoyStack();
  }, 120_000);
  afterAll(async () => {
    await stack?.stop();
  }, 60_000);

  for (const { row, slug, stalledRedials, unackedBytes } of [
    {
      row: "A: with natsConnectionOptions it reconnects and receives again within 60 s of the freeze",
      slug: "a",
      stalledRedials: 0,
      unackedBytes: 0,
    },
    {
      row: "B: with natsConnectionOptions it reconnects and receives again within 60 s of the freeze when its first redial also hangs, and closes the hung redial's socket once it gives up on it",
      slug: "b",
      stalledRedials: 1,
      unackedBytes: 0,
    },
    {
      row: "C: with natsConnectionOptions it reconnects and receives again within 60 s of the freeze when it wrote data the frozen path never acknowledged",
      slug: "c",
      stalledRedials: 0,
      unackedBytes: 4 * 1024 * 1024,
    },
  ]) {
    test(row, async () => {
      if (stack === undefined) throw new Error("the Envoy stack did not start");
      const proxy = await startStallProxy(stack.natsHostPort);
      const publisher = await connect({ servers: [stack.natsUrl], name: `nats-stall-pub-${slug}` });
      const client = await connect(
        natsConnectionOptions({ servers: [proxy.url], name: `nats-stall-${slug}`, seed: undefined })
      );
      try {
        const subject = `nats-stall.${slug}.${crypto.randomUUID()}`;
        const before = Promise.withResolvers<void>();
        let arrivedAt: number | undefined;
        client.subscribe(subject, {
          callback: (error, message) => {
            if (error !== null) return;
            const body = codec.decode(message.data);
            if (body === "before") before.resolve();
            else arrivedAt ??= Date.now();
          },
        });
        await client.flush();
        publisher.publish(subject, codec.encode("before"));
        await publisher.flush();
        await before.promise;

        let reconnectedAt: number | undefined;
        void (async () => {
          for await (const status of client.status()) {
            if (status.type === Events.Reconnect) reconnectedAt ??= Date.now();
          }
        })();
        proxy.stallNext(stalledRedials);
        const frozenAt = proxy.freeze();
        // The client's own writes after the freeze, in chunks below natsd's max_payload: more than
        // the frozen path's kernel buffers take, so they stay queued in its socket. Nothing awaits
        // a flush of them, which cannot complete.
        const chunk = new Uint8Array(64 * 1024);
        for (let sent = 0; sent < unackedBytes; sent += chunk.length) {
          client.publish(`${subject}.unacked`, chunk);
        }
        // What is measured is nats.js's own wall-clock ping and dial timers, so the wait is real
        // time. A publish reaches the client only once it has subscribed again on a path that
        // answers, and each one before that goes to the frozen connection and is lost, so one
        // goes out every quarter second until one arrives or the bound passes.
        for (
          let attempt = 0;
          arrivedAt === undefined && Date.now() - frozenAt < RECOVERY_BOUND_MS;
          attempt += 1
        ) {
          publisher.publish(subject, codec.encode(`after-${attempt}`));
          await publisher.flush();
          await Bun.sleep(250);
        }

        const reconnectMs = reconnectedAt === undefined ? undefined : reconnectedAt - frozenAt;
        const receiveMs = arrivedAt === undefined ? undefined : arrivedAt - frozenAt;
        console.log(
          `nats-stall row ${slug.toUpperCase()}: reconnect ${reconnectMs === undefined ? "none" : `${(reconnectMs / 1000).toFixed(1)} s`}, first publish received ${receiveMs === undefined ? "never" : `${(receiveMs / 1000).toFixed(1)} s`} after the freeze`
        );
        expect(reconnectMs).toBeDefined();
        expect(reconnectMs ?? Number.POSITIVE_INFINITY).toBeLessThan(RECOVERY_BOUND_MS);
        expect(receiveMs).toBeDefined();
        expect(receiveMs ?? Number.POSITIVE_INFINITY).toBeLessThan(RECOVERY_BOUND_MS);
        const redials = proxy.connections.filter((opened) => opened.acceptedAt >= frozenAt);
        // The stalled redials never subscribed; the one after them did.
        expect(redials.length).toBeGreaterThan(stalledRedials);
        expect(
          redials.slice(0, stalledRedials).every((opened) => opened.subscribedAt === undefined)
        ).toBe(true);
        expect(redials[stalledRedials]?.subscribedAt).toBeDefined();
        // nats.js abandons a dial after its 20 s timeout; the socket it opened must go with it, so
        // the client holds no socket to a redial it gave up on. The proxy never closes a stalled
        // connection, so its close is the client's. Real nats.js dial timers: the wait polls the
        // real clock.
        for (const hung of redials.slice(0, stalledRedials)) {
          while (hung.closedAt === undefined && Date.now() - hung.acceptedAt < 30_000) {
            await Bun.sleep(250);
          }
          console.log(
            `nats-stall row ${slug.toUpperCase()} hung redial: accepted ${((hung.acceptedAt - frozenAt) / 1000).toFixed(1)} s after the freeze, ${hung.closedAt === undefined ? "still open" : `closed ${((hung.closedAt - hung.acceptedAt) / 1000).toFixed(1)} s after it was accepted`}`
          );
          expect(hung.closedAt).toBeDefined();
        }
      } finally {
        await client.close();
        await publisher.close();
        await proxy.close();
      }
    }, 120_000);
  }
});
