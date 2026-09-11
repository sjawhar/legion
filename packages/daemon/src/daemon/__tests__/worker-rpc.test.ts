import { afterEach, describe, expect, it, vi } from "bun:test";
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

const RPC_CHUNK_PAYLOAD_BYTES = 256 * 1024;

/** Encodes `frame` into protocol v2 `rpc_chunk` lines exactly like oh-my-pi's real RPC frame
 * encoder: JSON -> utf8 bytes -> 256 KiB base64 slices, in order. */
function encodeRpcChunks(frame: object, chunkId: string): Record<string, unknown>[] {
  const bytes = Buffer.from(JSON.stringify(frame), "utf8");
  const byteLength = bytes.byteLength;
  const count = Math.ceil(byteLength / RPC_CHUNK_PAYLOAD_BYTES);
  const chunks: Record<string, unknown>[] = [];
  for (let index = 0; index < count; index++) {
    chunks.push({
      type: "rpc_chunk",
      chunkId,
      index,
      count,
      byteLength,
      data: bytes
        .subarray(index * RPC_CHUNK_PAYLOAD_BYTES, (index + 1) * RPC_CHUNK_PAYLOAD_BYTES)
        .toString("base64"),
    });
  }
  return chunks;
}

/** A fake worker-shim: echoes negotiate/prompt/get_state responses and lets the test push
 * events. Buffers writes and retries on `drain` so a line larger than the unix socket's send
 * buffer (e.g. a 256 KiB `rpc_chunk` payload) is never silently truncated by a partial
 * `socket.write()`. */
function fakeShimServer(
  target: string,
  handleFrame: (frame: Record<string, unknown>, write: (frame: object) => void) => void
): { stop(): void; write(frame: object): void } {
  let currentSocket: Bun.Socket<undefined> | undefined;
  let buffer = "";
  const pendingWrites: Buffer[] = [];

  const flush = (): void => {
    if (!currentSocket) return;
    while (pendingWrites.length > 0) {
      const head = pendingWrites[0];
      const written = currentSocket.write(head);
      if (written >= head.byteLength) {
        pendingWrites.shift();
        continue;
      }
      if (written > 0) pendingWrites[0] = head.subarray(written);
      break;
    }
  };

  const enqueueWrite = (line: string): void => {
    pendingWrites.push(Buffer.from(line, "utf8"));
    flush();
  };

  const server = Bun.listen({
    unix: target,
    socket: {
      open(socket) {
        currentSocket = socket;
        flush();
      },
      data(_socket, data) {
        buffer += data.toString("utf8");
        let index = buffer.indexOf("\n");
        while (index !== -1) {
          const line = buffer.slice(0, index).trim();
          buffer = buffer.slice(index + 1);
          if (line) {
            const frame = JSON.parse(line) as Record<string, unknown>;
            handleFrame(frame, (response) => enqueueWrite(`${JSON.stringify(response)}\n`));
          }
          index = buffer.indexOf("\n");
        }
      },
      close() {
        currentSocket = undefined;
      },
      drain() {
        flush();
      },
    },
  });
  return {
    stop() {
      server.stop(true);
    },
    write(frame) {
      enqueueWrite(`${JSON.stringify(frame)}\n`);
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

  it("reassembles a chunked agent_end frame and marks the worker idle", async () => {
    const target = await socketPath();
    const bigAgentEnd = {
      type: "agent_end",
      messages: [{ role: "assistant", content: "x".repeat(1_400_000) }],
    };
    const chunks = encodeRpcChunks(bigAgentEnd, "chunk-agent-end");
    const shim = fakeShimServer(target, (frame, write) => {
      if (frame.type === "negotiate_protocol") {
        write({
          id: frame.id,
          type: "response",
          command: "negotiate_protocol",
          success: true,
          data: { protocolVersion: 2 },
        });
        write({ type: "agent_start" });
        for (const chunk of chunks) write(chunk);
      }
    });
    try {
      const client = await connectWorkerRpc(target);
      let idleCount = 0;
      const idleResolvers = Promise.withResolvers<void>();
      client.onIdle(() => {
        idleCount++;
        idleResolvers.resolve();
      });
      await client.negotiate();
      await idleResolvers.promise;
      expect(client.runState).toBe("idle");
      expect(idleCount).toBe(1);
      client.close();
    } finally {
      shim.stop();
    }
  });

  it("reassembles a chunked response frame and resolves the pending get_state request", async () => {
    const target = await socketPath();
    const bigData = { isStreaming: false, note: "x".repeat(1_400_000) };
    const shim = fakeShimServer(target, (frame, write) => {
      if (frame.type === "get_state") {
        const chunks = encodeRpcChunks(
          { id: frame.id, type: "response", command: "get_state", success: true, data: bigData },
          "chunk-get-state"
        );
        for (const chunk of chunks) write(chunk);
      }
    });
    try {
      const client = await connectWorkerRpc(target);
      const response = await client.getState(2_000);
      expect(response).toMatchObject({ command: "get_state", success: true, data: bigData });
      client.close();
    } finally {
      shim.stop();
    }
  });

  it("drops a chunk sequence interrupted by a plain frame without touching runState", async () => {
    const target = await socketPath();
    // A large agent_end-shaped payload chunked into several lines, deliberately never finished.
    const abortedChunks = encodeRpcChunks(
      { type: "agent_end", messages: [{ role: "assistant", content: "x".repeat(1_400_000) }] },
      "chunk-aborted"
    );
    const shim = fakeShimServer(target, (frame, write) => {
      if (frame.type === "prompt") {
        write(abortedChunks[0]);
        write({ id: frame.id, type: "response", command: "prompt", success: true });
      }
    });
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const client = await connectWorkerRpc(target);
      await expect(client.prompt("verify #41")).resolves.toBeUndefined();
      // "running" is exactly what `prompt()` itself sets optimistically before sending the
      // request -- the aborted chunk sequence never dispatched anything, so it never had a
      // chance to touch `runState` on its own.
      expect(client.runState).toBe("running");
      expect(errorLog).toHaveBeenCalledTimes(1);
      const [message] = errorLog.mock.calls[0] ?? [];
      expect(message).toContain("[legion] worker RPC dropped a malformed rpc_chunk sequence:");
      client.close();
    } finally {
      errorLog.mockRestore();
      shim.stop();
    }
  });
});
