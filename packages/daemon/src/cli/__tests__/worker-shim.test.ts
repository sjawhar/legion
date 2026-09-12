import { afterAll, afterEach, describe, expect, it } from "bun:test";
import { existsSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { IssueKey } from "@legion/contracts";
import type { ResolvedWorkerClaim } from "../../daemon/api/auth";
import type { WorkerRoleClaim } from "../../daemon/legion-state";
import { connectWorkerRpc } from "../../daemon/worker-rpc";
import {
  startWorkerStreamListener,
  type WorkerStreamListener,
} from "../../daemon/worker-stream-listener";
import {
  cmdWorkerShim,
  cmdWorkerShimConnect,
  defaultWorkerShimDeps,
  resolveWorkerShimTarget,
  type WorkerShimConnectDeps,
  type WorkerShimDeps,
} from "../worker-shim";

const FAKE_OMP = path.join(import.meta.dir, "fixtures", "fake-omp-rpc.ts");
const tempDirs: string[] = [];

async function socketPath(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "legion-worker-shim-"));
  tempDirs.push(dir);
  return path.join(dir, "worker.sock");
}

/** Waits for the shim's unix socket file to exist so the test's client does not race the listener. */
async function waitForSocket(target: string): Promise<void> {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    if (existsSync(target)) return;
    await Bun.sleep(10);
  }
  throw new Error(`worker-shim socket never appeared at ${target}`);
}

/** Polls `predicate` every 10 ms for up to 5 s. Real-socket I/O cannot be driven by fake timers,
 * so this awaits the observable condition itself rather than a guessed duration. */
async function waitFor(predicate: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 500; attempt += 1) {
    if (predicate()) return;
    await Bun.sleep(10);
  }
  throw new Error("condition never became true");
}

/** The daemon side reduced to the wire contract: accepts, expects `hello` with `tok-1`, answers
 * `hello_ack`, then records the `n` of every later frame. `port` 0 binds ephemeral; pass the
 * previous port to "restart" it. */
function fakeDaemon(port = 0): {
  readonly port: number;
  readonly seen: number[];
  readonly hellos: number;
  writeShutdown(): void;
  stop(): void;
} {
  const seen: number[] = [];
  let hellos = 0;
  let current: Bun.Socket<{ acked: boolean; buffer: string }> | undefined;
  const server = Bun.listen<{ acked: boolean; buffer: string }>({
    hostname: "127.0.0.1",
    port,
    socket: {
      open(socket) {
        socket.data = { acked: false, buffer: "" };
        current = socket;
      },
      data(socket, data) {
        socket.data.buffer += data.toString("utf8");
        let index = socket.data.buffer.indexOf("\n");
        while (index !== -1) {
          const line = socket.data.buffer.slice(0, index).trim();
          socket.data.buffer = socket.data.buffer.slice(index + 1);
          index = socket.data.buffer.indexOf("\n");
          if (!line) continue;
          const frame = JSON.parse(line) as Record<string, unknown>;
          if (!socket.data.acked) {
            expect(frame).toEqual({ type: "hello", bootToken: "tok-1" });
            hellos += 1;
            socket.data.acked = true;
            socket.write(`${JSON.stringify({ type: "hello_ack" })}\n`);
            continue;
          }
          if (typeof frame.n === "number") seen.push(frame.n);
        }
      },
      close(socket) {
        if (current === socket) current = undefined;
      },
    },
  });
  return {
    get port() {
      return server.port;
    },
    seen,
    get hellos() {
      return hellos;
    },
    writeShutdown: () => {
      current?.write(`${JSON.stringify({ type: "shutdown" })}\n`);
    },
    stop: () => server.stop(true),
  };
}

/** Narrows a written `{"type":"state","n":<number>}` test frame line to its `n` field. */
function frameNumber(line: string): number {
  const parsed: unknown = JSON.parse(line);
  if (typeof parsed !== "object" || parsed === null || !("n" in parsed)) {
    throw new Error(`Expected a frame with an "n" field, got: ${line}`);
  }
  const { n } = parsed;
  if (typeof n !== "number") {
    throw new Error(`Expected frame "n" to be a number, got: ${line}`);
  }
  return n;
}

describe("cmdWorkerShim", () => {
  it("forwards negotiate/prompt frames to the wrapped OMP process, logs a summary per event, and exits with its status on stdin close", async () => {
    const target = await socketPath();
    const logs: string[] = [];
    const deps = { ...defaultWorkerShimDeps(), log: (line: string) => logs.push(line) };
    const shimExit = cmdWorkerShim(target, ["bun", FAKE_OMP], deps);
    await waitForSocket(target);

    // Reads raw newline-delimited frames directly off the shim's socket (bypassing
    // WorkerRpcClient's request/response correlation) to observe the uncorrelated
    // agent_start/agent_end frames the shim forwards from the wrapped fake OMP process.
    const frames: Record<string, unknown>[] = [];
    const gotAgentEnd = Promise.withResolvers<void>();
    let buffer = "";
    const socket = await Bun.connect({
      unix: target,
      socket: {
        data(_socket, data) {
          buffer += data.toString("utf8");
          let index = buffer.indexOf("\n");
          while (index !== -1) {
            const line = buffer.slice(0, index).trim();
            buffer = buffer.slice(index + 1);
            if (line) {
              const frame = JSON.parse(line) as Record<string, unknown>;
              if (frame.id === undefined) {
                frames.push(frame);
                if (frame.type === "agent_end") gotAgentEnd.resolve();
              }
            }
            index = buffer.indexOf("\n");
          }
        },
        close() {},
        error() {},
      },
    });
    socket.write(
      `${JSON.stringify({ id: "negotiate", type: "negotiate_protocol", protocolVersion: 2 })}\n`
    );
    socket.write(`${JSON.stringify({ id: "prompt", type: "prompt", message: "verify #41" })}\n`);
    await gotAgentEnd.promise;

    expect(frames).toEqual([
      { type: "agent_start" },
      { type: "agent_end", messages: [{ role: "user", content: "verify #41" }] },
    ]);
    expect(logs).toContain("agent_start");
    expect(logs).toContain("agent_end");

    socket.write(`${JSON.stringify({ type: "shutdown" })}\n`);
    const exitCode = await shimExit;
    expect(exitCode).toBe(0);
    socket.end();
  });

  it("buffers frames emitted before a socket client connects", async () => {
    const target = await socketPath();
    const deps = defaultWorkerShimDeps();
    const shimExit = cmdWorkerShim(target, ["bun", FAKE_OMP], deps);
    await waitForSocket(target);

    // Connect, negotiate immediately (a real frame the fake omp answers before any later client
    // reconnect), then disconnect without reading further — proves the shim keeps running and a
    // second connection still gets a working RPC session rather than crashing on client loss.
    const first = await connectWorkerRpc(target);
    await first.negotiate();
    first.close();

    const second = await connectWorkerRpc(target);
    await second.negotiate();
    second.shutdown();
    expect(await shimExit).toBe(0);
    second.close();
  });

  it("delivers a chunked agent_end whole across the socket so the daemon client goes idle", async () => {
    // Each rpc_chunk line is ~350 KiB, larger than a unix socket's send buffer; a shim that
    // dropped the bytes the kernel refused would leave the daemon with unparseable lines and a
    // worker that never reads as idle.
    const target = await socketPath();
    const deps = defaultWorkerShimDeps();
    const shimExit = cmdWorkerShim(target, ["bun", FAKE_OMP], deps);
    await waitForSocket(target);

    const client = await connectWorkerRpc(target);
    await client.negotiate();
    const idle = Promise.withResolvers<void>();
    client.onIdle(() => idle.resolve());
    await client.prompt("chunked: long turn");
    await Promise.race([
      idle.promise,
      Bun.sleep(5_000).then(() => {
        throw new Error(`client never went idle (runState=${client.runState})`);
      }),
    ]);
    expect(client.runState).toBe("idle");

    client.shutdown();
    expect(await shimExit).toBe(0);
    client.close();
  });

  it("rejects invocation without a wrapped command", async () => {
    const target = await socketPath();
    await expect(cmdWorkerShim(target, [], defaultWorkerShimDeps())).rejects.toThrow(
      /Usage: legion worker-shim/
    );
  });

  it("bounds its pre-connect backlog to the most recent frames and logs once when dropping", async () => {
    const logs: string[] = [];
    const written: string[] = [];
    let onConnect: ((socket: { write(line: string): void; end(): void }) => void) | undefined;
    const frameCount = 1005;
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        const encoder = new TextEncoder();
        for (let index = 0; index < frameCount; index += 1) {
          controller.enqueue(encoder.encode(`${JSON.stringify({ type: "state", n: index })}\n`));
        }
        controller.close();
      },
    });
    const exited = Promise.withResolvers<number>();
    const deps: WorkerShimDeps = {
      spawn: () => ({
        stdin: { write: () => {}, end: () => {} },
        stdout: stream,
        exited: exited.promise,
      }),
      listen: (_socketPath, handlers) => {
        onConnect = handlers.onConnect;
        return { stop: () => {} };
      },
      log: (line) => logs.push(line),
    };

    const shimExit = cmdWorkerShim("/tmp/legion-worker-shim-unused.sock", ["fake"], deps);
    await Bun.sleep(50);
    if (!onConnect) throw new Error("worker-shim never listened for a socket connection");
    onConnect({ write: (line) => written.push(line), end: () => {} });
    exited.resolve(0);

    expect(await shimExit).toBe(0);
    expect(written).toHaveLength(1000);
    expect(frameNumber(written[0])).toBe(frameCount - 1000);
    expect(frameNumber(written[999])).toBe(frameCount - 1);
    expect(logs.filter((line) => line.includes("dropping oldest frames"))).toHaveLength(1);
  });
});

const CLAIM_TOKEN = "legion-shimtest-LEGION-1-tester";
const claim: WorkerRoleClaim = { issue: "LEGION-1" as IssueKey, role: "tester", generation: 1 };
const resolveTok1 = (bootToken: string): ResolvedWorkerClaim | undefined =>
  bootToken === "tok-1" ? { token: CLAIM_TOKEN, claim, boot: undefined } : undefined;
const listeners: WorkerStreamListener[] = [];
/** The real daemon listener with a fake resolver: `port` 0 binds ephemeral; pass the previous
 * port to "restart" it. */
function startListener(port = 0, logs: string[] = []): WorkerStreamListener {
  const listener = startWorkerStreamListener({
    hostname: "127.0.0.1",
    port,
    rpcTimeoutMs: 5_000,
    resolveBootToken: resolveTok1,
    log: (line) => logs.push(line),
  });
  listeners.push(listener);
  return listener;
}
const endpointOf = (listener: WorkerStreamListener) => ({ host: "127.0.0.1", port: listener.port });

/** A fake `connect` dep: every dial is recorded as a connection whose `handlers` the test drives
 * and whose `written` lines it inspects; a dial made while `reachable()` is false rejects. */
function fakeConnect(reachable: () => boolean) {
  const connections: Array<{
    handlers: { onLine(line: string): void; onDisconnect(): void };
    written: string[];
    ended: boolean;
  }> = [];
  const connect: WorkerShimConnectDeps["connect"] = async (_endpoint, handlers) => {
    if (!reachable()) throw new Error("ECONNREFUSED");
    const connection = { handlers, written: [] as string[], ended: false };
    connections.push(connection);
    return {
      write: (line) => connection.written.push(line),
      end: () => {
        connection.ended = true;
      },
    };
  };
  return { connect, connections };
}
const ACK = JSON.stringify({ type: "hello_ack" });
const closedStdout = () => new ReadableStream<Uint8Array>({ start: (c) => c.close() });

describe("cmdWorkerShimConnect", () => {
  afterEach(() => {
    for (const listener of listeners.splice(0)) listener.close();
  });

  it("dials, sends hello, spawns OMP only after hello_ack, forwards negotiate/prompt frames, logs a summary per event, and exits with the child's status on stdin close", async () => {
    const listener = startListener();
    const logs: string[] = [];
    const deps = { ...defaultWorkerShimDeps(), log: (line: string) => logs.push(line) };
    const registration = listener.awaitRegistration(CLAIM_TOKEN, 5_000);
    const shimExit = cmdWorkerShimConnect(endpointOf(listener), "tok-1", ["bun", FAKE_OMP], deps);
    const client = await registration;
    // "spawned only after the ack" is pinned by the fakeDaemon and backoff cases below; here the
    // real fake-omp child is simply driven through the registered client.
    await client.negotiate();
    const idle = Promise.withResolvers<void>();
    client.onIdle(() => idle.resolve());
    await client.prompt("verify #41");
    await idle.promise;
    expect(logs).toContain("agent_start");
    expect(logs).toContain("agent_end");
    client.shutdown();
    expect(await shimExit).toBe(0);
    await client.closed;
  });

  it("keeps running across a daemon-side disconnect and registers again on the next hello", async () => {
    const listener = startListener();
    const shimExit = cmdWorkerShimConnect(
      endpointOf(listener),
      "tok-1",
      ["bun", FAKE_OMP],
      defaultWorkerShimDeps()
    );
    const first = await listener.awaitRegistration(CLAIM_TOKEN, 5_000);
    await first.negotiate();
    first.close();
    await first.closed;
    const second = await listener.awaitRegistration(CLAIM_TOKEN, 5_000);
    expect(second).not.toBe(first);
    await second.negotiate();
    second.shutdown();
    expect(await shimExit).toBe(0);
  });

  it("delivers a chunked agent_end whole over TCP so the daemon client goes idle", async () => {
    const listener = startListener();
    const shimExit = cmdWorkerShimConnect(
      endpointOf(listener),
      "tok-1",
      ["bun", FAKE_OMP],
      defaultWorkerShimDeps()
    );
    const client = await listener.awaitRegistration(CLAIM_TOKEN, 5_000);
    await client.negotiate();
    const idle = Promise.withResolvers<void>();
    client.onIdle(() => idle.resolve());
    await client.prompt("chunked: long turn");
    await Promise.race([
      idle.promise,
      Bun.sleep(5_000).then(() => {
        throw new Error(`client never went idle (runState=${client.runState})`);
      }),
    ]);
    expect(client.runState).toBe("idle");
    client.shutdown();
    expect(await shimExit).toBe(0);
  });

  it("rejects invocation without a wrapped command", async () => {
    await expect(
      cmdWorkerShimConnect({ host: "127.0.0.1", port: 1 }, "tok-1", [], defaultWorkerShimDeps())
    ).rejects.toThrow(/Usage: legion worker-shim/);
  });

  it("survives the daemon listener being killed and restarted mid-stream: hello re-sent, no frame lost, none reordered", async () => {
    // A test-controlled "OMP" emits frames exactly when the test says, so the assertion is about
    // the shim's buffering — frames produced while the shim knows it is disconnected — not about
    // bytes TCP had already accepted into a socket whose peer just vanished (no application-level
    // ack exists to recover those, and the spec promises only the gap). The daemon side is
    // `fakeDaemon`: a bare `Bun.listen` that speaks only the hello/hello_ack wire contract and
    // records every frame's `n` — proving the shim needs nothing more from a daemon.
    let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
    const stdout = new ReadableStream<Uint8Array>({
      start: (c) => {
        controller = c;
      },
    });
    const exited = Promise.withResolvers<number>();
    const encoder = new TextEncoder();
    const emit = (n: number) =>
      controller?.enqueue(encoder.encode(`${JSON.stringify({ type: "state", n })}\n`));
    let spawns = 0;
    const retries: string[] = [];
    const deps: WorkerShimConnectDeps = {
      ...defaultWorkerShimDeps(),
      spawn: () => {
        spawns += 1;
        return {
          stdin: { write: () => {}, end: () => exited.resolve(0) },
          stdout,
          exited: exited.promise,
        };
      },
      log: (line) => {
        if (line.includes("unavailable")) retries.push(line);
      },
    };

    const first = fakeDaemon();
    const shimExit = cmdWorkerShimConnect(
      { host: "127.0.0.1", port: first.port },
      "tok-1",
      ["fake"],
      deps
    );
    await waitFor(() => first.hellos === 1);
    await waitFor(() => spawns === 1); // spawned only after the ack
    emit(1);
    emit(2);
    emit(3);
    await waitFor(() => first.seen.length === 3);
    const port = first.port;
    first.stop(); // kill the daemon side under the shim
    // The shim has observed the drop once its immediate redial is refused and logged.
    await waitFor(() => retries.length === 1);
    expect(retries[0]).toContain("retrying in 200ms");
    emit(4);
    emit(5);
    emit(6); // produced during the gap
    // A second refused dial proves the 200 ms backoff ran against the real socket before the
    // "restart" below (fake timers cannot drive Bun.connect); the 400 ms retry then finds it.
    await waitFor(() => retries.length === 2);
    expect(retries[1]).toContain("retrying in 400ms");
    const second = fakeDaemon(port); // "restart" on the same port
    await waitFor(() => second.hellos === 1); // hello re-sent on the new stream
    emit(7);
    await waitFor(() => second.seen.length === 4);
    expect(first.seen).toEqual([1, 2, 3]);
    expect(second.seen).toEqual([4, 5, 6, 7]);
    expect(spawns).toBe(1);
    second.writeShutdown();
    expect(await shimExit).toBe(0);
    second.stop();
  });

  it("bounds the frames buffered while disconnected to the newest 1000 and logs the drop once", async () => {
    // Mirror of the socket-mode backlog case: the wrapped process emits 1005 frames while the
    // daemon is unreachable; on the next ack the newest 1000 go out in order.
    const logs: string[] = [];
    let reachable = true;
    const dialer = fakeConnect(() => reachable);
    const exited = Promise.withResolvers<number>();
    const deps: WorkerShimConnectDeps = {
      spawn: () => ({
        stdin: { write: () => {}, end: () => {} },
        stdout: new ReadableStream<Uint8Array>({
          start(controller) {
            const encoder = new TextEncoder();
            for (let index = 0; index < 1005; index += 1) {
              controller.enqueue(
                encoder.encode(`${JSON.stringify({ type: "state", n: index })}\n`)
              );
            }
            controller.close();
          },
        }),
        exited: exited.promise,
      }),
      connect: dialer.connect,
      // A macrotask, not an immediately-resolved promise: an unreachable daemon must not turn the
      // retry loop into a microtask spin that starves the stdout pump.
      sleep: () => Bun.sleep(1),
      log: (line) => logs.push(line),
    };
    const shimExit = cmdWorkerShimConnect({ host: "127.0.0.1", port: 1 }, "tok-1", ["fake"], deps);
    await waitFor(() => dialer.connections.length === 1);
    const first = dialer.connections[0];
    if (!first) throw new Error("no first connection");
    expect(first.written).toEqual([JSON.stringify({ type: "hello", bootToken: "tok-1" })]);
    // Ack, then drop the stream in the same tick: the child is spawned (after the ack) but every
    // frame it emits finds no connected daemon and lands in the backlog.
    reachable = false;
    first.handlers.onLine(ACK);
    first.handlers.onDisconnect();
    await waitFor(() => logs.some((line) => line.includes("dropping oldest frames")));
    expect(first.written).toHaveLength(1); // nothing was ever written to the dropped stream
    reachable = true;
    await waitFor(() => dialer.connections.length === 2);
    const second = dialer.connections[1];
    if (!second) throw new Error("no second connection");
    second.handlers.onLine(ACK);
    await waitFor(() => second.written.length === 1 + 1000); // hello + the newest 1000 frames
    const states = second.written.slice(1);
    expect(frameNumber(states[0] ?? "")).toBe(5);
    expect(frameNumber(states[999] ?? "")).toBe(1004);
    expect(logs.filter((line) => line.includes("dropping oldest frames"))).toHaveLength(1);
    exited.resolve(0);
    expect(await shimExit).toBe(0);
    expect(second.ended).toBe(true); // the shim ends its stream when the child exits
  });

  it("backs off 200ms → 5s across failed dials and rejected hellos, resets after an ack, and never spawns before the first ack", async () => {
    const sleeps: number[] = [];
    let spawns = 0;
    let attempt = 0;
    // Dials 1–7 are refused; dial 8 opens but the test closes it before the ack (a rejected hello);
    // dial 9 is refused again (still in the failure series → capped 5 s); dial 10 opens and is
    // acked; the test then drops it; dial 11 is refused (fresh series → 200 ms); dial 12 opens.
    const refused = new Set([1, 2, 3, 4, 5, 6, 7, 9, 11]);
    const dialer = fakeConnect(() => {
      attempt += 1;
      return !refused.has(attempt);
    });
    const exited = Promise.withResolvers<number>();
    const deps: WorkerShimConnectDeps = {
      spawn: () => {
        spawns += 1;
        return {
          stdin: { write: () => {}, end: () => {} },
          stdout: closedStdout(),
          exited: exited.promise,
        };
      },
      connect: dialer.connect,
      sleep: async (ms) => {
        sleeps.push(ms);
      },
      log: () => {},
    };
    const shimExit = cmdWorkerShimConnect({ host: "127.0.0.1", port: 1 }, "tok-1", ["fake"], deps);
    await waitFor(() => dialer.connections.length === 1); // dial 8
    expect(sleeps).toEqual([200, 400, 800, 1600, 3200, 5000, 5000]);
    expect(spawns).toBe(0);
    // Closed before the ack counts as a failure: the series continues at the cap.
    dialer.connections[0]?.handlers.onDisconnect();
    await waitFor(() => dialer.connections.length === 2); // dial 9 refused, dial 10 open
    expect(sleeps.slice(7)).toEqual([5000, 5000]);
    expect(spawns).toBe(0);
    dialer.connections[1]?.handlers.onLine(ACK);
    await waitFor(() => spawns === 1);
    // A drop after an ack starts a fresh series at 200 ms.
    dialer.connections[1]?.handlers.onDisconnect();
    await waitFor(() => dialer.connections.length === 3); // dial 11 refused, dial 12 open
    expect(sleeps.slice(9)).toEqual([200]);
    expect(spawns).toBe(1);
    exited.resolve(7);
    expect(await shimExit).toBe(7);
    expect(dialer.connections[2]?.ended).toBe(true);
  });

  it("closes the child's stdin on a shutdown frame and forwards every other daemon frame to stdin — including one sharing the ack's read", async () => {
    const stdinWrites: string[] = [];
    let ended = false;
    let handlers: { onLine(line: string): void; onDisconnect(): void } | undefined;
    const exited = Promise.withResolvers<number>();
    const deps: WorkerShimConnectDeps = {
      spawn: () => ({
        stdin: {
          write: (data) => stdinWrites.push(new TextDecoder().decode(data)),
          end: () => {
            ended = true;
            exited.resolve(0);
          },
        },
        stdout: closedStdout(),
        exited: exited.promise,
      }),
      connect: async (_endpoint, h) => {
        handlers = h;
        return { write: () => {}, end: () => {} };
      },
      sleep: async () => {},
      log: () => {},
    };
    const shimExit = cmdWorkerShimConnect({ host: "127.0.0.1", port: 1 }, "tok-1", ["fake"], deps);
    await waitFor(() => handlers !== undefined);
    const negotiate = JSON.stringify({ id: "n", type: "negotiate_protocol", protocolVersion: 2 });
    // The daemon's first RPC frame can arrive in the same read as the ack (the listener acks and
    // a resolved waiter negotiates in one turn): both lines land synchronously, and the second
    // must already find a spawned child to be written to.
    handlers?.onLine(ACK);
    handlers?.onLine(negotiate);
    await waitFor(() => stdinWrites.length === 1);
    expect(stdinWrites[0]).toBe(`${negotiate}\n`);
    handlers?.onLine(JSON.stringify({ type: "shutdown" }));
    expect(await shimExit).toBe(0);
    expect(ended).toBe(true);
    expect(stdinWrites).toHaveLength(1);
  });
});

describe("resolveWorkerShimTarget", () => {
  const readFile = (contents: Record<string, string>) => (path: string) => {
    if (!(path in contents)) throw new Error(`ENOENT: ${path}`);
    return contents[path] as string;
  };

  it("returns socket mode for --socket alone and connect mode for --connect with a readable token file", () => {
    expect(resolveWorkerShimTarget({ socket: "/tmp/w.sock" })).toEqual({
      mode: "socket",
      socketPath: "/tmp/w.sock",
    });
    expect(
      resolveWorkerShimTarget(
        { connect: "tcp://daemon.legion.svc:13371", bootTokenFile: "/run/legion/boot" },
        readFile({ "/run/legion/boot": "  tok-1\n" })
      )
    ).toEqual({
      mode: "connect",
      endpoint: { host: "daemon.legion.svc", port: 13371 },
      bootToken: "tok-1",
    });
    expect(
      resolveWorkerShimTarget(
        { connect: "tcp://[::1]:13371", bootTokenFile: "/t" },
        readFile({ "/t": "tok" })
      )
    ).toMatchObject({ endpoint: { host: "::1", port: 13371 } });
  });

  it("names both flags when --socket and --connect are combined", () => {
    expect(() => resolveWorkerShimTarget({ socket: "/tmp/w.sock", connect: "tcp://h:1" })).toThrow(
      "--socket and --connect are mutually exclusive"
    );
  });

  it("requires one of the two modes, a token file with --connect, and no token file with --socket", () => {
    expect(() => resolveWorkerShimTarget({})).toThrow(/Usage: legion worker-shim/);
    expect(() => resolveWorkerShimTarget({ connect: "tcp://h:1" })).toThrow(
      "--connect requires --boot-token-file <path>"
    );
    expect(() => resolveWorkerShimTarget({ socket: "/tmp/w.sock", bootTokenFile: "/x" })).toThrow(
      "--boot-token-file is only valid with --connect"
    );
  });

  it("fails naming the path for an unreadable or blank token file, and rejects a non-tcp endpoint", () => {
    expect(() =>
      resolveWorkerShimTarget({ connect: "tcp://h:1", bootTokenFile: "/missing" }, readFile({}))
    ).toThrow("--boot-token-file /missing is unreadable");
    expect(() =>
      resolveWorkerShimTarget(
        { connect: "tcp://h:1", bootTokenFile: "/blank" },
        readFile({ "/blank": " \n" })
      )
    ).toThrow("--boot-token-file /blank is blank");
    for (const bad of [
      "http://h:1",
      "tcp://h",
      "tcp://:1",
      "tcp://h:0",
      "tcp://h:70000",
      "h:1",
      "tcp://h:1/path",
      "tcp://h:1?q",
      "tcp://u:p@h:1",
    ]) {
      expect(() =>
        resolveWorkerShimTarget({ connect: bad, bootTokenFile: "/t" }, readFile({ "/t": "tok" }))
      ).toThrow("--connect must be tcp://<host>:<port>");
    }
  });
});

afterAll(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});
