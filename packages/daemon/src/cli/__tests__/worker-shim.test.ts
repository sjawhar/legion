import { afterAll, describe, expect, it } from "bun:test";
import { existsSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { connectWorkerRpc } from "../../daemon/worker-rpc";
import { cmdWorkerShim, defaultWorkerShimDeps, type WorkerShimDeps } from "../worker-shim";

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

afterAll(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});
