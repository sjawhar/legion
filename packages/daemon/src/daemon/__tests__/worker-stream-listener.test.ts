import { afterEach, describe, expect, it } from "bun:test";
import type { IssueKey } from "@legion/contracts";
import type { ResolvedWorkerClaim } from "../api/auth";
import type { WorkerRoleClaim } from "../legion-state";
import {
  startWorkerStreamListener,
  type WorkerStreamListener,
  type WorkerStreamListenerOptions,
} from "../worker-stream-listener";

const CLAIM_TOKEN = "legion-acme-LEGION-1-tester";
const claim: WorkerRoleClaim = { issue: "LEGION-1" as IssueKey, role: "tester", generation: 2 };
const resolvedClaim: ResolvedWorkerClaim = { token: CLAIM_TOKEN, claim, boot: undefined };

const listeners: WorkerStreamListener[] = [];
afterEach(() => {
  for (const listener of listeners.splice(0)) listener.close();
});

function start(
  resolveBootToken: (token: string) => ResolvedWorkerClaim | undefined = (token) =>
    token === "tok-1" ? resolvedClaim : undefined,
  options: Partial<WorkerStreamListenerOptions> = {}
) {
  const logs: string[] = [];
  const listener = startWorkerStreamListener({
    hostname: "127.0.0.1",
    port: 0,
    rpcTimeoutMs: 2_000,
    resolveBootToken,
    log: (line) => logs.push(line),
    ...options,
  });
  listeners.push(listener);
  return { listener, logs };
}

/** A raw shim stand-in: dials the listener, collects newline-delimited lines, answers
 * negotiate_protocol like the real OMP would, and resolves `closed` when the daemon hangs up. */
async function dial(port: number) {
  const lines: string[] = [];
  const closed = Promise.withResolvers<void>();
  let buffer = "";
  const socket = await Bun.connect<undefined>({
    hostname: "127.0.0.1",
    port,
    socket: {
      data(sock, data) {
        buffer += data.toString("utf8");
        let index = buffer.indexOf("\n");
        while (index !== -1) {
          const line = buffer.slice(0, index).trim();
          buffer = buffer.slice(index + 1);
          index = buffer.indexOf("\n");
          if (!line) continue;
          lines.push(line);
          const frame = JSON.parse(line) as Record<string, unknown>;
          if (frame.type === "negotiate_protocol") {
            sock.write(
              `${JSON.stringify({
                id: frame.id,
                type: "response",
                command: "negotiate_protocol",
                success: true,
                data: { protocolVersion: 2 },
              })}\n`
            );
          }
        }
      },
      close() {
        closed.resolve();
      },
      error() {},
    },
  });
  return {
    lines,
    closed: closed.promise,
    write: (text: string) => socket.write(text),
    end: () => socket.end(),
  };
}

const hello = (bootToken: string) => `${JSON.stringify({ type: "hello", bootToken })}\n`;

/** Polls `predicate` every 10 ms for up to 5 s. Real-socket I/O cannot be driven by fake timers,
 * so this awaits the observable condition itself rather than a guessed duration. */
async function waitFor(predicate: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 500; attempt += 1) {
    if (predicate()) return;
    await Bun.sleep(10);
  }
  throw new Error("condition never became true");
}

describe("WorkerStreamListener", () => {
  it("acks a hello whose token resolves, registers the stream, and speaks RPC over it — including bytes that followed the hello in the same write", async () => {
    const { listener, logs } = start();
    const shim = await dial(listener.port);
    const registered = listener.awaitRegistration(CLAIM_TOKEN, 5_000);
    shim.write(`${hello("tok-1")}${JSON.stringify({ type: "agent_start" })}\n`);
    const client = await registered;
    expect(listener.registrations.get(CLAIM_TOKEN)).toBe(client);
    // The waiter resolves the instant the listener registers; the ack bytes reach this socket on
    // a later event-loop turn.
    await waitFor(() => shim.lines.length === 1);
    expect(shim.lines[0]).toBe(JSON.stringify({ type: "hello_ack" }));
    expect(client.runState).toBe("running");
    await expect(client.negotiate()).resolves.toBeUndefined();
    expect(logs).toEqual([]);
    // Same token while the first stream lives: refused, first stream untouched.
    const second = await dial(listener.port);
    second.write(hello("tok-1"));
    await second.closed;
    expect(logs).toEqual(["worker-stream: rejected hello (already bound to a live stream)"]);
    expect(listener.registrations.get(CLAIM_TOKEN)).toBe(client);
    // Once the first stream closes, the same token registers again (a reconnecting shim).
    shim.end();
    await client.closed;
    expect(listener.registrations.has(CLAIM_TOKEN)).toBe(false);
    const third = await dial(listener.port);
    third.write(hello("tok-1"));
    const reconnected = await listener.awaitRegistration(CLAIM_TOKEN, 5_000);
    expect(reconnected).not.toBe(client);
    await waitFor(() => third.lines.length === 1);
    expect(third.lines[0]).toBe(JSON.stringify({ type: "hello_ack" }));
  });

  it.each([
    ["not json", "nope\n", 0],
    ["malformed hello", `${JSON.stringify({ type: "hi", bootToken: "tok-1" })}\n`, 0],
    ["malformed hello", `${JSON.stringify({ type: "hello" })}\n`, 0],
    ["malformed hello", `${JSON.stringify({ type: "hello", bootToken: "" })}\n`, 0],
    ["unknown boot token", hello("tok-nope"), 1],
    ["hello too long", `${"a".repeat(5_000)}`, 0],
  ])("rejects a hello (%s): closes the connection, logs once, resolves the token at most once, changes nothing", async (reason, payload, resolverCalls) => {
    let calls = 0;
    const { listener, logs } = start((token) => {
      calls += 1;
      return token === "tok-1" ? resolvedClaim : undefined;
    });
    const shim = await dial(listener.port);
    shim.write(payload);
    await shim.closed;
    expect(logs).toEqual([`worker-stream: rejected hello (${reason})`]);
    expect(calls).toBe(resolverCalls);
    expect(listener.registrations.size).toBe(0);
    expect(shim.lines).toEqual([]);
  });

  it("rejects a hello whose in-memory mint record is a stale generation", async () => {
    const { listener, logs } = start(() => ({
      token: CLAIM_TOKEN,
      claim,
      boot: {
        tree: "LEGION-1" as IssueKey,
        issue: "LEGION-1" as IssueKey,
        role: "tester",
        generation: 1,
      },
    }));
    const shim = await dial(listener.port);
    shim.write(hello("tok-1"));
    await shim.closed;
    expect(logs).toEqual(["worker-stream: rejected hello (stale worker generation)"]);
    expect(listener.registrations.size).toBe(0);
  });

  it("logs nothing for a connection that closes before sending a line", async () => {
    const { listener, logs } = start();
    const shim = await dial(listener.port);
    shim.end();
    // A negative check over a real socket: the only "signal" is the absence of a log line once
    // the listener has had a turn to observe the close, so a short real wait is unavoidable here.
    await Bun.sleep(20);
    expect(logs).toEqual([]);
  });

  /** An injectable clock: every armed timer is recorded (never fires on its own) and every
   * clear is recorded, so a test drives expiry by hand and can assert what was cancelled. */
  function fakeClock() {
    const timers: Array<{ callback: () => void; delayMs: number }> = [];
    const cleared: unknown[] = [];
    return {
      timers,
      cleared,
      options: {
        setTimeout: (callback: () => void, delayMs: number) => {
          const timer = { callback, delayMs };
          timers.push(timer);
          return timer;
        },
        clearTimeout: (timer: unknown) => {
          cleared.push(timer);
        },
      },
    };
  }

  it("awaitRegistration rejects on its own fake-clock timeout and clears the timer when the hello lands first", async () => {
    const clock = fakeClock();
    const { listener } = start(undefined, clock.options);
    const late = listener.awaitRegistration("legion-acme-LEGION-9-planner", 30_000);
    expect(clock.timers).toHaveLength(1);
    expect(clock.timers[0]?.delayMs).toBe(30_000);
    clock.timers[0]?.callback();
    await expect(late).rejects.toThrow(
      "worker stream for legion-acme-LEGION-9-planner did not register within 30000ms"
    );

    const inTime = listener.awaitRegistration(CLAIM_TOKEN, 30_000);
    const shim = await dial(listener.port);
    shim.write(hello("tok-1"));
    await expect(inTime).resolves.toBeDefined();
    // The connection's own hello deadline (armed in `open`, after the waiter's timer) is cleared
    // first, then the waiter's timer as it settles.
    expect(clock.timers).toHaveLength(3);
    expect(clock.timers[2]?.delayMs).toBe(2_000);
    expect(clock.cleared).toEqual([clock.timers[2], clock.timers[1]]);
  });

  it("rejects a connection whose hello deadline fires before any line, and clears the deadline once a hello lands first", async () => {
    const clock = fakeClock();
    const { listener, logs } = start(undefined, clock.options);
    const idle = await dial(listener.port);
    // The deadline is armed in the listener's own `open` turn, not the dialer's.
    await waitFor(() => clock.timers.length === 1);
    expect(clock.timers[0]?.delayMs).toBe(2_000); // rpcTimeoutMs; no separate setting
    clock.timers[0]?.callback();
    await idle.closed;
    expect(logs).toEqual(["worker-stream: rejected hello (hello timeout)"]);
    expect(listener.registrations.size).toBe(0);
    expect(idle.lines).toEqual([]);

    const prompt = await dial(listener.port);
    await waitFor(() => clock.timers.length === 2);
    prompt.write(hello("tok-1"));
    await listener.awaitRegistration(CLAIM_TOKEN, 5_000);
    expect(clock.cleared).toContain(clock.timers[1]);
    expect(logs).toHaveLength(1); // no late rejection of the registered stream
    expect(listener.registrations.size).toBe(1);
  });

  it("close() rejects pending waiters, closes registered streams, and refuses later dials", async () => {
    const { listener } = start();
    const shim = await dial(listener.port);
    shim.write(hello("tok-1"));
    const client = await listener.awaitRegistration(CLAIM_TOKEN, 5_000);
    const pending = listener.awaitRegistration("legion-acme-LEGION-9-planner", 60_000);
    const port = listener.port;
    listener.close();
    await expect(pending).rejects.toThrow("worker stream listener closed");
    await client.closed;
    await shim.closed;
    expect(listener.registrations.size).toBe(0);
    await expect(listener.awaitRegistration(CLAIM_TOKEN, 1)).rejects.toThrow(
      "worker stream listener closed"
    );
    await expect(
      Bun.connect<undefined>({ hostname: "127.0.0.1", port, socket: { data() {} } })
    ).rejects.toThrow();
  });

  it("names worker_stream_port when the port is already bound", () => {
    const occupied = Bun.listen<undefined>({
      hostname: "127.0.0.1",
      port: 0,
      socket: { data() {} },
    });
    try {
      expect(() => start(undefined, { port: occupied.port })).toThrow(
        `worker_stream_port ${occupied.port} on 127.0.0.1 is unavailable`
      );
    } finally {
      occupied.stop(true);
    }
  });
});
