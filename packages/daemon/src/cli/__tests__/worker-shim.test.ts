import { afterAll, afterEach, describe, expect, it } from "bun:test";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { waitFor } from "../../daemon/__tests__/ci-fixtures";
import type { ResolvedStreamClaim } from "../../daemon/api";
import { connectWorkerRpc } from "../../daemon/worker-rpc";
import {
  startWorkerStreamListener,
  type WorkerStreamListener,
} from "../../daemon/worker-stream-listener";
import {
  cmdWorkerShim,
  cmdWorkerShimConnect,
  defaultWorkerShimDeps,
  readProviderEnvDir,
  resolveWorkerShimTarget,
  runWorkingCopyAdoption,
  terminateStdinGraceMs,
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
      adopt: async () => ({ ok: true as const }),
      spawn: () => ({
        stdin: { write: () => {}, end: () => {} },
        stdout: stream,
        exited: exited.promise,
        kill: () => {},
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

  it("answers a daemon adopt-working-copy frame itself -- the adoption runs through `adopt`, never reaches the OMP child, and the result frame carries the request id, ok, and the error", async () => {
    // The Kubernetes runtime's per-assignment adoption: the daemon names an identity and a budget
    // (never a command or a path); the shim runs the shared adoption command in its own workspace
    // and answers over the same stream. A failure is an answer, not a dropped frame.
    const adoptions: Array<{ jjUser: string; jjEmail: string; timeoutMs: number }> = [];
    const childStdin: string[] = [];
    const written: string[] = [];
    let handlers:
      | {
          onLine(line: string): void;
          onConnect(socket: { write(l: string): void; end(): void }): void;
        }
      | undefined;
    const exited = Promise.withResolvers<number>();
    let fail = false;
    const deps: WorkerShimDeps = {
      spawn: () => ({
        stdin: { write: (data) => childStdin.push(new TextDecoder().decode(data)), end: () => {} },
        stdout: new ReadableStream<Uint8Array>({ start: (c) => c.close() }),
        exited: exited.promise,
        kill: () => {},
      }),
      listen: (_socketPath, h) => {
        handlers = h;
        return { stop: () => {} };
      },
      adopt: async (adoption) => {
        adoptions.push(adoption);
        if (fail)
          return { ok: false, error: "Command failed (exit 1): jj metaedit …\nno such revision" };
        return { ok: true };
      },
      log: () => {},
    };
    const shimExit = cmdWorkerShim("/tmp/legion-worker-shim-unused.sock", ["fake"], deps);
    // `cmdWorkerShim` spawns and listens before its first await, so the handlers are already set.
    if (!handlers) throw new Error("worker-shim never listened");
    handlers.onConnect({ write: (line) => written.push(line), end: () => {} });

    handlers.onLine(
      JSON.stringify({
        id: "req-1",
        type: "adopt-working-copy",
        jjUser: "legion-implementer[bot]",
        jjEmail: "implementer@users.noreply.github.com",
        timeoutMs: 300_000,
      })
    );
    await waitFor(() => written.length === 1);
    expect(adoptions).toEqual([
      {
        jjUser: "legion-implementer[bot]",
        jjEmail: "implementer@users.noreply.github.com",
        timeoutMs: 300_000,
      },
    ]);
    expect(JSON.parse(written[0] ?? "")).toEqual({
      type: "adopt-working-copy-result",
      id: "req-1",
      ok: true,
    });
    expect(childStdin).toEqual([]);

    fail = true;
    handlers.onLine(
      JSON.stringify({
        id: "req-2",
        type: "adopt-working-copy",
        jjUser: "legion-implementer[bot]",
        jjEmail: "implementer@users.noreply.github.com",
        timeoutMs: 300_000,
      })
    );
    await waitFor(() => written.length === 2);
    expect(JSON.parse(written[1] ?? "")).toEqual({
      type: "adopt-working-copy-result",
      id: "req-2",
      ok: false,
      error: "Command failed (exit 1): jj metaedit …\nno such revision",
    });
    // An ordinary RPC frame still goes to the child, untouched.
    handlers.onLine(JSON.stringify({ id: "p1", type: "prompt", message: "hi" }));
    expect(childStdin).toEqual([
      `${JSON.stringify({ id: "p1", type: "prompt", message: "hi" })}\n`,
    ]);
    exited.resolve(0);
    expect(await shimExit).toBe(0);
  });
});

const CLAIM_TOKEN = "legion-shimtest-LEGION-1-tester";
const resolveTok1 = (bootToken: string): ResolvedStreamClaim | undefined =>
  bootToken === "tok-1" ? { token: CLAIM_TOKEN, stale: false } : undefined;
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
    const baseDeps = defaultWorkerShimDeps();
    const spawnEnvs: Array<Record<string, string> | undefined> = [];
    const deps = {
      ...baseDeps,
      spawn: (argv: string[], env?: Record<string, string>) => {
        spawnEnvs.push(env);
        return baseDeps.spawn(argv, env);
      },
      log: (line: string) => logs.push(line),
    };
    const registration = listener.awaitRegistration(CLAIM_TOKEN, 5_000);
    const shimExit = cmdWorkerShimConnect(endpointOf(listener), "tok-1", ["bun", FAKE_OMP], deps, {
      LEGION_TEST_PROVIDER_KEY: "sk-1",
    });
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
    expect(spawnEnvs).toEqual([{ LEGION_TEST_PROVIDER_KEY: "sk-1" }]);
    expect(process.env.LEGION_TEST_PROVIDER_KEY).toBeUndefined();
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
          kill: () => {},
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
      adopt: async () => ({ ok: true as const }),
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
        kill: () => {},
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

  it("on SIGTERM (the pod's PID 1) closes the wrapped process's stdin like a shutdown frame, SIGTERMs it only if it is still running after the stdin grace, and exits with the child", async () => {
    // A kubelet termination -- a drain, an eviction, a delete while the stream is unregistered --
    // reaches the shim as SIGTERM and nowhere else; ignored, OMP would be SIGKILLed at the grace
    // boundary mid-turn. Two runs: a child that exits on its closed stdin (no signal sent), and
    // one that does not (SIGTERM after the grace, which the injected sleep resolves at once).
    for (const childHonoursStdin of [true, false]) {
      const logs: string[] = [];
      const sleeps: number[] = [];
      const dialer = fakeConnect(() => true);
      const exited = Promise.withResolvers<number>();
      let terminate: (() => void) | undefined;
      let stdinEnded = false;
      let killed = false;
      const deps: WorkerShimConnectDeps = {
        adopt: async () => ({ ok: true as const }),
        spawn: () => ({
          stdin: {
            write: () => {},
            end: () => {
              stdinEnded = true;
              if (childHonoursStdin) exited.resolve(0);
            },
          },
          stdout: new ReadableStream<Uint8Array>({ start: (c) => c.close() }),
          exited: exited.promise,
          kill: () => {
            killed = true;
            exited.resolve(143);
          },
        }),
        connect: dialer.connect,
        sleep: async (ms) => {
          sleeps.push(ms);
        },
        onTerminate: (handler) => {
          terminate = handler;
        },
        // The pod's grace, as the runtime sets it on the main container: the stdin grace is half.
        env: { LEGION_TERMINATION_GRACE_SECONDS: "30" },
        log: (line) => logs.push(line),
      };
      const shimExit = cmdWorkerShimConnect(
        { host: "127.0.0.1", port: 1 },
        "tok-1",
        ["fake"],
        deps
      );
      await waitFor(() => dialer.connections.length === 1);
      dialer.connections[0]?.handlers.onLine(ACK);
      if (!terminate) throw new Error("the shim registered no SIGTERM handler");
      terminate();
      expect(stdinEnded).toBe(true);
      expect(await shimExit).toBe(childHonoursStdin ? 0 : 143);
      expect(killed).toBe(!childHonoursStdin);
      expect(logs).toContain("[worker-shim] SIGTERM: closing the wrapped process's stdin");
      expect(
        logs.some((line) =>
          line.includes("still running 15000 ms after its stdin closed; sending SIGTERM")
        )
      ).toBe(!childHonoursStdin);
      expect(sleeps).toContain(15000);
    }
  });

  it("on SIGTERM before the daemon ever acked (no wrapped process yet) exits 143 at once and ends its stream", async () => {
    // A pod terminated while its shim is still dialing (the daemon down, or a hello it rejects):
    // nothing to wind down, so the shim must exit as an unhandled SIGTERM would have -- a handler
    // that only closed a child's stdin would leave the reconnect loop running until SIGKILL.
    const logs: string[] = [];
    const dialer = fakeConnect(() => true);
    let spawns = 0;
    let terminate: (() => void) | undefined;
    const deps: WorkerShimConnectDeps = {
      adopt: async () => ({ ok: true as const }),
      spawn: () => {
        spawns += 1;
        throw new Error("must not spawn");
      },
      connect: dialer.connect,
      sleep: async () => {},
      onTerminate: (handler) => {
        terminate = handler;
      },
      log: (line) => logs.push(line),
    };
    const shimExit = cmdWorkerShimConnect({ host: "127.0.0.1", port: 1 }, "tok-1", ["fake"], deps);
    await waitFor(() => dialer.connections.length === 1);
    if (!terminate) throw new Error("the shim registered no SIGTERM handler");
    terminate();
    expect(await shimExit).toBe(143);
    expect(spawns).toBe(0);
    expect(dialer.connections[0]?.ended).toBe(true);
    expect(logs).toContain("[worker-shim] SIGTERM before the wrapped process was spawned; exiting");
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
      adopt: async () => ({ ok: true as const }),
      spawn: () => {
        spawns += 1;
        return {
          stdin: { write: () => {}, end: () => {} },
          stdout: closedStdout(),
          exited: exited.promise,
          kill: () => {},
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
      adopt: async () => ({ ok: true as const }),
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
        kill: () => {},
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

describe("terminateStdinGraceMs", () => {
  it("is half the pod's termination grace, floored at 1 s, and the 5 s default without one", () => {
    expect(terminateStdinGraceMs({})).toBe(5_000);
    expect(terminateStdinGraceMs({ LEGION_TERMINATION_GRACE_SECONDS: "10" })).toBe(5_000);
    expect(terminateStdinGraceMs({ LEGION_TERMINATION_GRACE_SECONDS: "60" })).toBe(30_000);
    expect(terminateStdinGraceMs({ LEGION_TERMINATION_GRACE_SECONDS: "1" })).toBe(1_000);
    expect(terminateStdinGraceMs({ LEGION_TERMINATION_GRACE_SECONDS: "0" })).toBe(1_000);
    expect(() => terminateStdinGraceMs({ LEGION_TERMINATION_GRACE_SECONDS: "ten" })).toThrow(
      'LEGION_TERMINATION_GRACE_SECONDS must be a whole number of seconds (got "ten")'
    );
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
      providerEnv: {},
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

  it("rejects --provider-env-dir combined with --socket", () => {
    expect(() => resolveWorkerShimTarget({ socket: "/s", providerEnvDir: "/x" })).toThrow(
      "--provider-env-dir is only valid with --connect"
    );
  });

  it("reads --provider-env-dir into providerEnv through the injected readEnvDir", () => {
    const readEnvDir = (dir: string) => ({ dir, ANTHROPIC_API_KEY: "sk-1" });
    const target = resolveWorkerShimTarget(
      { connect: "tcp://h:1", bootTokenFile: "/t", providerEnvDir: "/providers" },
      readFile({ "/t": "tok" }),
      readEnvDir
    );
    expect(target).toMatchObject({
      mode: "connect",
      providerEnv: { dir: "/providers", ANTHROPIC_API_KEY: "sk-1" },
    });
  });
});

describe("runWorkingCopyAdoption", () => {
  it("runs the shared adoption command in the shim's workspace with PATH from the shim's environment and JJ_USER/JJ_EMAIL from the frame, and answers ok:false with the runner's report on failure", async () => {
    const runs: Array<{ cmd: string[]; env?: NodeJS.ProcessEnv; timeoutMs?: number }> = [];
    let exitCode = 0;
    const runner = async (
      cmd: string[],
      options?: { env?: NodeJS.ProcessEnv; timeoutMs?: number }
    ) => {
      runs.push({ cmd, env: options?.env, timeoutMs: options?.timeoutMs });
      return { stdout: "", stderr: "Error: no such revision", exitCode };
    };
    const shimEnv = {
      PATH: "/legion/worker-bin:/opt/legion/bin:/usr/bin",
      HOME: "/home/legion",
      LEGION_WORKSPACE: "/legion/workspaces/acme/widgets/legion-42",
      JJ_USER: "someone-else[bot]",
    };
    const adoption = {
      jjUser: "legion-implementer[bot]",
      jjEmail: "implementer@users.noreply.github.com",
      timeoutMs: 300_000,
    };
    expect(await runWorkingCopyAdoption(adoption, shimEnv, runner)).toEqual({ ok: true });
    expect(runs).toEqual([
      {
        cmd: [
          "jj",
          "metaedit",
          "--update-author",
          "-r",
          '@ & description(exact:"")',
          "-R",
          "/legion/workspaces/acme/widgets/legion-42",
        ],
        env: {
          PATH: "/legion/worker-bin:/opt/legion/bin:/usr/bin",
          HOME: "/home/legion",
          LEGION_WORKSPACE: "/legion/workspaces/acme/widgets/legion-42",
          // The frame's identity wins over anything the shim's own environment carried.
          JJ_USER: "legion-implementer[bot]",
          JJ_EMAIL: "implementer@users.noreply.github.com",
        },
        timeoutMs: 300_000,
      },
    ]);
    exitCode = 1;
    expect(await runWorkingCopyAdoption(adoption, shimEnv, runner)).toEqual({
      ok: false,
      error: `Command failed (exit 1): jj metaedit --update-author -r @ & description(exact:"") -R /legion/workspaces/acme/widgets/legion-42\nError: no such revision`,
    });
    expect(await runWorkingCopyAdoption(adoption, { PATH: "/usr/bin" }, runner)).toEqual({
      ok: false,
      error:
        "worker-shim: neither LEGION_WORKSPACE nor LEGION_ROOT_WORKSPACE is set; no workspace to adopt",
    });
  });
});

describe("readProviderEnvDir", () => {
  async function providerEnvDir(): Promise<string> {
    const dir = await mkdtemp(path.join(os.tmpdir(), "legion-provider-env-"));
    tempDirs.push(dir);
    return dir;
  }

  it("reads every regular file, following the Kubernetes Secret mount's symlink chain, and skips subdirectories", async () => {
    const dir = await providerEnvDir();
    await writeFile(path.join(dir, "B"), "two", "utf8");
    const timestamped = path.join(dir, "..2026_09_13_10_00_00.000");
    await mkdir(timestamped);
    await writeFile(path.join(timestamped, "A"), "one\n", "utf8");
    await symlink("..2026_09_13_10_00_00.000", path.join(dir, "..data"));
    await symlink(path.join("..data", "A"), path.join(dir, "A"));
    await mkdir(path.join(dir, "nested"));
    await writeFile(path.join(dir, "nested", "C"), "three", "utf8");

    expect(readProviderEnvDir(dir)).toEqual({ A: "one", B: "two" });
  });

  it("skips a key the pod consumes through a *_FILE pointer in the shim's own environment, so a file-pointed token is never also an environment variable of the OMP child", async () => {
    // The runtime sets DISPATCH_TOKEN_FILE on the main container to the mounted DISPATCH_TOKEN
    // file; the extension reads the file. Exporting DISPATCH_TOKEN as well would hand it to every
    // tool the agent runs. Any other key follows the same rule the moment it is file-pointed.
    const dir = await providerEnvDir();
    await writeFile(path.join(dir, "ANTHROPIC_API_KEY"), "sk-1", "utf8");
    await writeFile(path.join(dir, "DISPATCH_TOKEN"), "dt-1", "utf8");
    await writeFile(path.join(dir, "ENVOY_TOKEN"), "et-1", "utf8");
    expect(
      readProviderEnvDir(dir, undefined, {
        DISPATCH_TOKEN_FILE: "/var/run/legion/providers/DISPATCH_TOKEN",
      })
    ).toEqual({ ANTHROPIC_API_KEY: "sk-1", ENVOY_TOKEN: "et-1" });
    expect(
      readProviderEnvDir(dir, undefined, {
        DISPATCH_TOKEN_FILE: "/var/run/legion/providers/DISPATCH_TOKEN",
        ENVOY_TOKEN_FILE: "/var/run/legion/providers/ENVOY_TOKEN",
      })
    ).toEqual({ ANTHROPIC_API_KEY: "sk-1" });
    expect(readProviderEnvDir(dir, undefined, {})).toEqual({
      ANTHROPIC_API_KEY: "sk-1",
      DISPATCH_TOKEN: "dt-1",
      ENVOY_TOKEN: "et-1",
    });
  });

  it("names the flag and the path when the directory is missing", () => {
    expect(() => readProviderEnvDir("/nope")).toThrow("--provider-env-dir /nope");
  });
});

afterAll(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});
