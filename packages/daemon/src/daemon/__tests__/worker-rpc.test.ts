import { afterEach, describe, expect, it, vi } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  connectWorkerRpc,
  createWorkerRpcClient,
  type WorkerRpcClient,
  type WorkerRpcSocketData,
  workerRpcSocketHandlers,
} from "../worker-rpc";

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

type FrameHandler = (frame: Record<string, unknown>, write: (frame: object) => void) => void;

interface FakeShim {
  stop(): void;
  write(frame: object): void;
}

interface Transport {
  name: "unix" | "tcp";
  /** Starts a fake shim over this transport; `connect()` yields a fresh client reaching it. */
  start(
    handleFrame: FrameHandler
  ): Promise<{ shim: FakeShim; connect(): Promise<WorkerRpcClient> }>;
}

/** The transport-independent half of a fake worker-shim: line framing, frame dispatch, and a
 * drain-aware write queue so a line larger than the socket's send buffer (a 256 KiB `rpc_chunk`
 * payload) is never truncated by a partial `socket.write()`. `handlers` is the table either
 * `Bun.listen` (unix) or `Bun.connect` (tcp) binds. */
function fakeShimCore(handleFrame: FrameHandler) {
  let current: Bun.Socket<undefined> | undefined;
  let buffer = "";
  const pendingWrites: Buffer[] = [];
  const flush = (): void => {
    if (!current) return;
    while (pendingWrites.length > 0) {
      const head = pendingWrites[0];
      const written = current.write(head);
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
  const handlers: Bun.SocketHandler<undefined> = {
    open(socket) {
      current = socket;
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
      current = undefined;
    },
    drain() {
      flush();
    },
  };
  return {
    handlers,
    write(frame: object) {
      enqueueWrite(`${JSON.stringify(frame)}\n`);
    },
    end() {
      current?.end();
    },
  };
}

const unixTransport: Transport = {
  name: "unix",
  async start(handleFrame) {
    const target = await socketPath();
    const core = fakeShimCore(handleFrame);
    const server = Bun.listen<undefined>({ unix: target, socket: core.handlers });
    return {
      shim: { stop: () => server.stop(true), write: core.write },
      connect: () => connectWorkerRpc(target),
    };
  },
};

/** Production shape for TCP: the daemon listens, the shim dials in, and the client wraps the
 * accepted socket (`createWorkerRpcClient` on the listener side). */
const tcpTransport: Transport = {
  name: "tcp",
  async start(handleFrame) {
    const core = fakeShimCore(handleFrame);
    const accepted = Promise.withResolvers<WorkerRpcClient>();
    const server = Bun.listen<WorkerRpcSocketData>({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        ...workerRpcSocketHandlers,
        open(socket) {
          socket.data = { handlers: undefined };
          accepted.resolve(createWorkerRpcClient(socket));
        },
      },
    });
    return {
      shim: {
        stop: () => {
          core.end();
          server.stop(true);
        },
        write: core.write,
      },
      connect: async () => {
        await Bun.connect<undefined>({
          hostname: "127.0.0.1",
          port: server.port,
          socket: core.handlers,
        });
        return accepted.promise;
      },
    };
  },
};

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

for (const transport of [unixTransport, tcpTransport]) {
  describe(`WorkerRpcClient over ${transport.name}`, () => {
    it("negotiates protocol v2 and correlates the response by id", async () => {
      const { shim, connect } = await transport.start((frame, write) => {
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
        const client = await connect();
        await expect(client.negotiate()).resolves.toBeUndefined();
        client.close();
      } finally {
        shim.stop();
      }
    });

    it("rejects negotiate when the shim reports failure", async () => {
      const { shim, connect } = await transport.start((frame, write) => {
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
        const client = await connect();
        await expect(client.negotiate()).rejects.toThrow("nope");
        client.close();
      } finally {
        shim.stop();
      }
    });

    it("resolves prompt on its immediate ack without waiting for the agent turn", async () => {
      const { shim, connect } = await transport.start((frame, write) => {
        if (frame.type === "prompt") {
          write({ id: frame.id, type: "response", command: "prompt", success: true });
        }
      });
      try {
        const client = await connect();
        await expect(client.prompt("verify #41")).resolves.toBeUndefined();
        client.close();
      } finally {
        shim.stop();
      }
    });

    it("returns get_state data and rejects after its timeout when the shim never answers", async () => {
      const { shim, connect } = await transport.start((frame, write) => {
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
        const client = await connect();
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
      const { shim, connect } = await transport.start(() => {
        // Never respond.
      });
      try {
        const client = await connect();
        await expect(client.getState(50)).rejects.toThrow(/timed out/);
        client.close();
      } finally {
        shim.stop();
      }
    });

    it("writes a bare shutdown frame without an id, never SIGTERM", async () => {
      const receivedShutdown = Promise.withResolvers<Record<string, unknown>>();
      const { shim, connect } = await transport.start((frame) => {
        receivedShutdown.resolve(frame);
      });
      try {
        const client = await connect();
        client.shutdown();
        expect(await receivedShutdown.promise).toEqual({ type: "shutdown" });
        client.close();
      } finally {
        shim.stop();
      }
    });

    it("resolves `closed` once the shim's socket disconnects", async () => {
      const { shim, connect } = await transport.start(() => {});
      const client = await connect();
      shim.stop();
      await expect(client.closed).resolves.toBeUndefined();
    });

    it("reassembles a chunked agent_end frame and marks the worker idle", async () => {
      const bigAgentEnd = {
        type: "agent_end",
        messages: [{ role: "assistant", content: "x".repeat(1_400_000) }],
      };
      const chunks = encodeRpcChunks(bigAgentEnd, "chunk-agent-end");
      const { shim, connect } = await transport.start((frame, write) => {
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
        const client = await connect();
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
      const bigData = { isStreaming: false, note: "x".repeat(1_400_000) };
      const { shim, connect } = await transport.start((frame, write) => {
        if (frame.type === "get_state") {
          const chunks = encodeRpcChunks(
            { id: frame.id, type: "response", command: "get_state", success: true, data: bigData },
            "chunk-get-state"
          );
          for (const chunk of chunks) write(chunk);
        }
      });
      try {
        const client = await connect();
        const response = await client.getState(2_000);
        expect(response).toMatchObject({ command: "get_state", success: true, data: bigData });
        client.close();
      } finally {
        shim.stop();
      }
    });

    it("drops a chunk sequence interrupted by a plain frame without touching runState", async () => {
      // A large agent_end-shaped payload chunked into several lines, deliberately never finished.
      const abortedChunks = encodeRpcChunks(
        { type: "agent_end", messages: [{ role: "assistant", content: "x".repeat(1_400_000) }] },
        "chunk-aborted"
      );
      const { shim, connect } = await transport.start((frame, write) => {
        if (frame.type === "prompt") {
          write(abortedChunks[0]);
          write({ id: frame.id, type: "response", command: "prompt", success: true });
        }
      });
      const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
      try {
        const client = await connect();
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
}
