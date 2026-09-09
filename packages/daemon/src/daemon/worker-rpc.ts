import { randomUUID } from "node:crypto";

const DEFAULT_RPC_TIMEOUT_MS = 5_000;

/**
 * Minimal client for the OMP RPC protocol v2, reached through a worker's
 * `legion worker-shim` unix socket rather than a spawned process's stdio. The
 * shim forwards every frame between the socket and the wrapped `omp --mode rpc`
 * process unchanged, so this client speaks the same newline-delimited JSON
 * protocol `packages/coding-agent/src/modes/rpc/rpc-mode.ts` implements.
 */
export interface WorkerRpcClient {
  /** Resolves once the underlying socket connection closes. */
  readonly closed: Promise<void>;
  negotiate(): Promise<void>;
  prompt(message: string): Promise<void>;
  getState(timeoutMs?: number): Promise<Record<string, unknown>>;
  /** Asks the shim to close the wrapped OMP process's stdin; never SIGTERMs it. */
  shutdown(): void;
  close(): void;
}

interface PendingRequest {
  resolve: (value: Record<string, unknown>) => void;
  reject: (error: Error) => void;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

/** Connects to a running worker-shim's unix socket and negotiates nothing by itself — call `negotiate()` next. */
export async function connectWorkerRpc(socketPath: string): Promise<WorkerRpcClient> {
  let buffer = "";
  const pending = new Map<string, PendingRequest>();
  const closedResolvers = Promise.withResolvers<void>();

  const failAllPending = (error: Error): void => {
    for (const request of pending.values()) request.reject(error);
    pending.clear();
  };

  const handleLine = (line: string): void => {
    if (!line) return;
    let frame: unknown;
    try {
      frame = JSON.parse(line);
    } catch {
      return;
    }
    if (!isRecord(frame)) return;
    const id = frame.id;
    if (typeof id === "string" && pending.has(id)) {
      const request = pending.get(id);
      pending.delete(id);
      if (frame.type === "response" && frame.success === false) {
        const message = typeof frame.error === "string" ? frame.error : "Worker RPC command failed";
        request?.reject(new Error(message));
      } else {
        request?.resolve(frame);
      }
    }
  };

  const socket = await Bun.connect({
    unix: socketPath,
    socket: {
      data(_socket, data) {
        buffer += data.toString("utf8");
        let index = buffer.indexOf("\n");
        while (index !== -1) {
          handleLine(buffer.slice(0, index).trim());
          buffer = buffer.slice(index + 1);
          index = buffer.indexOf("\n");
        }
      },
      close() {
        failAllPending(new Error("Worker RPC socket closed"));
        closedResolvers.resolve();
      },
      error(_socket, error) {
        failAllPending(error);
        closedResolvers.reject(error);
      },
    },
  });

  const request = (
    type: string,
    extra: Record<string, unknown> = {},
    timeoutMs = DEFAULT_RPC_TIMEOUT_MS
  ): Promise<Record<string, unknown>> => {
    const id = randomUUID();
    const settled = Promise.withResolvers<Record<string, unknown>>();
    pending.set(id, settled);
    const timer = setTimeout(() => {
      if (pending.delete(id)) {
        settled.reject(new Error(`Worker RPC "${type}" timed out after ${timeoutMs}ms`));
      }
    }, timeoutMs);
    socket.write(`${JSON.stringify({ id, type, ...extra })}\n`);
    return settled.promise.finally(() => clearTimeout(timer));
  };

  return {
    closed: closedResolvers.promise,
    async negotiate() {
      const response = await request("negotiate_protocol", { protocolVersion: 2 });
      if (response.command !== "negotiate_protocol" || response.success !== true) {
        throw new Error("Worker RPC protocol v2 negotiation failed");
      }
    },
    async prompt(message) {
      await request("prompt", { message });
    },
    getState(timeoutMs = DEFAULT_RPC_TIMEOUT_MS) {
      return request("get_state", {}, timeoutMs);
    },
    shutdown() {
      socket.write(`${JSON.stringify({ type: "shutdown" })}\n`);
    },
    close() {
      socket.end();
    },
  };
}
