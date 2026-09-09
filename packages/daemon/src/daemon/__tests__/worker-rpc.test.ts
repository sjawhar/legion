import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { connectWorkerRpc } from "../worker-rpc";

const tempDirs: string[] = [];

async function socketPath(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "legion-worker-rpc-"));
  tempDirs.push(dir);
  return path.join(dir, "worker.sock");
}

/** A fake worker-shim: echoes negotiate/prompt/get_state responses and lets the test push events. */
function fakeShimServer(
  target: string,
  handleFrame: (frame: Record<string, unknown>, write: (frame: object) => void) => void
): { stop(): void; write(frame: object): void } {
  let currentSocket: Bun.Socket<undefined> | undefined;
  let buffer = "";
  const server = Bun.listen({
    unix: target,
    socket: {
      open(socket) {
        currentSocket = socket;
      },
      data(socket, data) {
        buffer += data.toString("utf8");
        let index = buffer.indexOf("\n");
        while (index !== -1) {
          const line = buffer.slice(0, index).trim();
          buffer = buffer.slice(index + 1);
          if (line) {
            const frame = JSON.parse(line) as Record<string, unknown>;
            handleFrame(frame, (response) => socket.write(`${JSON.stringify(response)}\n`));
          }
          index = buffer.indexOf("\n");
        }
      },
      close() {
        currentSocket = undefined;
      },
    },
  });
  return {
    stop() {
      server.stop(true);
    },
    write(frame) {
      currentSocket?.write(`${JSON.stringify(frame)}\n`);
    },
  };
}

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

describe("WorkerRpcClient", () => {
  it("negotiates protocol v2 and correlates the response by id", async () => {
    const target = await socketPath();
    const shim = fakeShimServer(target, (frame, write) => {
      if (frame.type === "negotiate_protocol") {
        write({
          id: frame.id,
          type: "response",
          command: "negotiate_protocol",
          success: true,
          data: { protocolVersion: 2 },
        });
      }
    });
    try {
      const client = await connectWorkerRpc(target);
      await expect(client.negotiate()).resolves.toBeUndefined();
      client.close();
    } finally {
      shim.stop();
    }
  });

  it("rejects negotiate when the shim reports failure", async () => {
    const target = await socketPath();
    const shim = fakeShimServer(target, (frame, write) => {
      if (frame.type === "negotiate_protocol") {
        write({
          id: frame.id,
          type: "response",
          command: "negotiate_protocol",
          success: false,
          error: "nope",
        });
      }
    });
    try {
      const client = await connectWorkerRpc(target);
      await expect(client.negotiate()).rejects.toThrow("nope");
      client.close();
    } finally {
      shim.stop();
    }
  });

  it("resolves prompt on its immediate ack without waiting for the agent turn", async () => {
    const target = await socketPath();
    const shim = fakeShimServer(target, (frame, write) => {
      if (frame.type === "prompt") {
        write({ id: frame.id, type: "response", command: "prompt", success: true });
      }
    });
    try {
      const client = await connectWorkerRpc(target);
      await expect(client.prompt("verify #41")).resolves.toBeUndefined();
      client.close();
    } finally {
      shim.stop();
    }
  });

  it("returns get_state data and rejects after its timeout when the shim never answers", async () => {
    const target = await socketPath();
    const shim = fakeShimServer(target, (frame, write) => {
      if (frame.type === "get_state") {
        write({
          id: frame.id,
          type: "response",
          command: "get_state",
          success: true,
          data: { isStreaming: true },
        });
      }
      // "hang" type: never respond, to exercise the timeout path.
    });
    try {
      const client = await connectWorkerRpc(target);
      const state = await client.getState(2_000);
      expect(state).toMatchObject({
        command: "get_state",
        success: true,
        data: { isStreaming: true },
      });
      client.close();
    } finally {
      shim.stop();
    }
  });

  it("times out a get_state call the shim never answers", async () => {
    const target = await socketPath();
    const shim = fakeShimServer(target, () => {
      // Never respond.
    });
    try {
      const client = await connectWorkerRpc(target);
      await expect(client.getState(50)).rejects.toThrow(/timed out/);
      client.close();
    } finally {
      shim.stop();
    }
  });

  it("writes a bare shutdown frame without an id, never SIGTERM", async () => {
    const target = await socketPath();
    const receivedShutdown = Promise.withResolvers<Record<string, unknown>>();
    const shim = fakeShimServer(target, (frame) => {
      receivedShutdown.resolve(frame);
    });
    try {
      const client = await connectWorkerRpc(target);
      client.shutdown();
      expect(await receivedShutdown.promise).toEqual({ type: "shutdown" });
      client.close();
    } finally {
      shim.stop();
    }
  });

  it("resolves `closed` once the shim's socket disconnects", async () => {
    const target = await socketPath();
    const shim = fakeShimServer(target, () => {});
    const client = await connectWorkerRpc(target);
    shim.stop();
    await expect(client.closed).resolves.toBeUndefined();
  });
});
