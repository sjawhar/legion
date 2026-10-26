import { describe, expect, test } from "bun:test";
import { readArrivals } from "../delivery";

/**
 * A subscription as nats.js gives one: messages pushed into its iterator as they arrive, which a
 * reader takes in order, and which ends or throws once stopped.
 */
function subscription() {
  const queued: string[] = [];
  let stopped: { readonly error?: Error } | undefined;
  let wake: (() => void) | undefined;
  const signal = () => {
    wake?.();
    wake = undefined;
  };
  const iterable: AsyncIterable<string> = {
    [Symbol.asyncIterator]: () => ({
      next: async (): Promise<IteratorResult<string>> => {
        for (;;) {
          if (stopped?.error !== undefined) throw stopped.error;
          if (stopped !== undefined) return { done: true, value: undefined };
          const message = queued.shift();
          if (message !== undefined) return { done: false, value: message };
          const pending = Promise.withResolvers<void>();
          wake = pending.resolve;
          await pending.promise;
        }
      },
    }),
  };
  return {
    iterable,
    push: (message: string) => {
      queued.push(message);
      signal();
    },
    stop: (error?: Error) => {
      stopped = error === undefined ? {} : { error };
      signal();
    },
  };
}

// Lets every queued promise callback and I/O turn run.
async function settle(): Promise<void> {
  for (let turn = 0; turn < 10; turn += 1) {
    const { promise, resolve } = Promise.withResolvers<void>();
    setImmediate(resolve);
    await promise;
  }
}

describe("a subscription's messages read as they arrive", () => {
  test("are stamped when they arrived, not when the caller took them, and taken in order", async () => {
    const source = subscription();
    let clock = 1_000;
    const arrivals = readArrivals(source.iterable, () => clock);

    source.push("a");
    const first = await arrivals.next();
    // The caller is busy with `a` while `b` and `c` arrive, a second apart.
    clock = 2_000;
    source.push("b");
    await settle();
    clock = 3_000;
    source.push("c");
    await settle();
    clock = 9_000;

    expect(first.value).toEqual({ message: "a", receivedAt: 1_000 });
    expect((await arrivals.next()).value).toEqual({ message: "b", receivedAt: 2_000 });
    expect((await arrivals.next()).value).toEqual({ message: "c", receivedAt: 3_000 });
  });

  test("end with the subscription, dropping what the caller had not taken yet", async () => {
    const source = subscription();
    const arrivals = readArrivals(source.iterable);
    source.push("a");
    await arrivals.next();
    source.push("b");
    await settle();

    // The subscription stops while the caller is still busy with `a`.
    source.stop();
    await settle();

    expect((await arrivals.next()).done).toBe(true);
  });

  test("throw the subscription's error at the caller's next read", async () => {
    const source = subscription();
    const arrivals = readArrivals(source.iterable);
    source.push("a");
    await arrivals.next();
    source.push("b");
    await settle();

    source.stop(new Error("connection lost"));
    await settle();

    await expect(arrivals.next()).rejects.toThrow("connection lost");
  });
});
