import { describe, expect, test } from "bun:test";
import { QueuedIteratorImpl } from "nats/lib/nats-base-client/queued_iterator";
import { readArrivals } from "../delivery";

// Lets every queued promise callback and I/O turn run.
async function settle(): Promise<void> {
  for (let turn = 0; turn < 10; turn += 1) {
    const { promise, resolve } = Promise.withResolvers<void>();
    setImmediate(resolve);
    await promise;
  }
}

/** Everything `iterator` still yields, until it ends. */
async function rest<Value>(iterator: AsyncIterator<Value>): Promise<Value[]> {
  const values: Value[] = [];
  for (let read = await iterator.next(); read.done !== true; read = await iterator.next()) {
    values.push(read.value);
  }
  return values;
}

// Each subscription below is nats.js's own iterator, the one its subscriptions extend: messages
// pushed into it as they arrive, and `stop` ending it, with an error or without.
describe("a subscription's messages read as they arrive", () => {
  test("are stamped when they arrived, not when the caller took them, and taken in order", async () => {
    const source = new QueuedIteratorImpl<string>();
    let clock = 1_000;
    const arrivals = readArrivals(source, () => clock);

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

  test("are all yielded, in order, when the subscription stops while the caller is busy", async () => {
    // nats.js's iterator, read directly: the caller takes `a`, and the subscription stops while
    // the caller is still busy with it.
    const direct = new QueuedIteratorImpl<string>();
    for (const message of ["a", "b", "c"]) direct.push(message);
    const iterator = direct[Symbol.asyncIterator]();
    const first = await iterator.next();
    direct.stop();
    const directly = [first.value, ...(await rest(iterator))];

    const source = new QueuedIteratorImpl<string>();
    for (const message of ["a", "b", "c"]) source.push(message);
    const arrivals = readArrivals(source);
    const taken = await arrivals.next();
    await settle();
    source.stop();
    await settle();
    const read = [taken.value?.message, ...(await rest(arrivals)).map(({ message }) => message)];

    expect(read).toEqual(["a", "b", "c"]);
    expect(read).toEqual(directly);
    expect((await arrivals.next()).done).toBe(true);
  });

  test("are all yielded before the subscription's error is thrown", async () => {
    const source = new QueuedIteratorImpl<string>();
    const arrivals = readArrivals(source);
    source.push("a");
    source.push("b");
    expect((await arrivals.next()).value?.message).toBe("a");
    await settle();

    source.stop(new Error("connection lost"));
    await settle();

    expect((await arrivals.next()).value?.message).toBe("b");
    await expect(arrivals.next()).rejects.toThrow("connection lost");
  });

  test("stop the subscription when the caller stops reading", async () => {
    const source = new QueuedIteratorImpl<string>();
    source.push("a");
    const taken: string[] = [];
    for await (const { message } of readArrivals(source)) {
      taken.push(message);
      break;
    }
    // nats.js takes the return once the read in flight settles: at the next message.
    source.push("b");
    await settle();

    expect(taken).toEqual(["a"]);
    expect(source.done).toBe(true);
  });
});
