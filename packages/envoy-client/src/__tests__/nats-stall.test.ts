import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { connect, Events, StringCodec } from "nats";
import { natsConnectionOptions } from "../nats-connection";
import { type EnvoyStack, envoyStackEnabled, startEnvoyStack } from "../testing/envoy-stack";
import { startStallProxy } from "../testing/stall-proxy";

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

  for (const { row, slug, stalledRedials } of [
    {
      row: "A: with natsConnectionOptions it reconnects and receives again within 60 s of the freeze",
      slug: "a",
      stalledRedials: 0,
    },
    {
      row: "B: with natsConnectionOptions it reconnects and receives again within 60 s of the freeze when its first redial also hangs",
      slug: "b",
      stalledRedials: 1,
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
      } finally {
        await client.close();
        await publisher.close();
        await proxy.close();
      }
    }, 120_000);
  }
});
