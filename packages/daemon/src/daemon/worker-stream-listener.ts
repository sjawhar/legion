import type { ResolvedWorkerClaim } from "./api/auth";
import {
  createWorkerRpcClient,
  type WorkerRpcClient,
  type WorkerRpcSocket,
  type WorkerRpcSocketData,
  type WorkerRpcSocketHandlers,
  workerRpcSocketHandlers,
} from "./worker-rpc";

/** Bytes a connection may send before its first newline. A real hello is ~60 bytes (a UUID
 * token); anything near this bound is not a shim. */
const MAX_HELLO_BYTES = 4096;
const HELLO_ACK_LINE = `${JSON.stringify({ type: "hello_ack" })}\n`;

export interface WorkerStreamListenerOptions {
  hostname: string;
  port: number;
  /** Per-request timeout for every client this listener creates — `worker_rpc_timeout_seconds`
   * in ms. */
  rpcTimeoutMs: number;
  /** `LegionApi.resolveWorkerBootToken`; `undefined` = no claim was minted this token. */
  resolveBootToken(bootToken: string): ResolvedWorkerClaim | undefined;
  log?(line: string): void;
  setTimeout?(callback: () => void, delayMs: number): unknown;
  clearTimeout?(timer: unknown): void;
}

export interface WorkerStreamListener {
  readonly port: number;
  /** Live streams by role claim token; an entry leaves the moment its socket closes. */
  readonly registrations: ReadonlyMap<string, WorkerRpcClient>;
  /** Resolves with the live, not-yet-negotiated client for `claimToken` — at once if already
   * registered, else when its shim's hello is accepted — or rejects after `timeoutMs` or when the
   * listener closes. The caller negotiates, exactly as `ProcessManager.workerClient` does today. */
  awaitRegistration(claimToken: string, timeoutMs: number): Promise<WorkerRpcClient>;
  /** Stops accepting, closes every registered stream, rejects every pending `awaitRegistration`. */
  close(): void;
}

interface Waiter {
  resolve(client: WorkerRpcClient): void;
  reject(error: Error): void;
  timer: unknown;
}

/**
 * Accepts reverse-dialed `legion worker-shim --connect` streams. Each connection's first line
 * must be `{"type":"hello","bootToken"}`; the token is resolved through the same lookup
 * `/worker/started` uses, the daemon answers `{"type":"hello_ack"}`, and the socket becomes a
 * `WorkerRpcClient` keyed by the claim's role token. Rejections close the connection, log one
 * `worker-stream: rejected hello (<reason>)` line, and change no state.
 */
export function startWorkerStreamListener(
  options: WorkerStreamListenerOptions
): WorkerStreamListener {
  const log = options.log ?? ((line: string) => console.error(`[legion] ${line}`));
  const setTimer =
    options.setTimeout ??
    ((callback: () => void, delayMs: number) => setTimeout(callback, delayMs));
  const clearTimer = options.clearTimeout ?? ((timer: unknown) => clearTimeout(timer as number));
  const registrations = new Map<string, WorkerRpcClient>();
  const waiters = new Map<string, Waiter[]>();
  let closed = false;

  const reject = (socket: WorkerRpcSocket, reason: string): void => {
    log(`worker-stream: rejected hello (${reason})`);
    socket.data.handlers = undefined;
    socket.end();
  };

  const settleWaiters = (token: string, client: WorkerRpcClient): void => {
    const pending = waiters.get(token);
    if (!pending) return;
    waiters.delete(token);
    for (const waiter of pending) {
      clearTimer(waiter.timer);
      waiter.resolve(client);
    }
  };

  const register = (socket: WorkerRpcSocket, token: string, remainder: Buffer): void => {
    // The ack goes out before the client attaches, so the shim sees it ahead of any RPC frame a
    // waiter sends the instant it resolves. A fresh socket's send buffer is empty: a short write
    // here is not backpressure, it is a connection that is already gone.
    if (socket.write(HELLO_ACK_LINE) !== HELLO_ACK_LINE.length) {
      reject(socket, "connection closed before hello_ack");
      return;
    }
    const client = createWorkerRpcClient(socket, options.rpcTimeoutMs);
    registrations.set(token, client);
    // Registered before any caller can await `client.closed`, so this runs ahead of every later
    // continuation on it: a caller that awaits the close never sees a stale registration.
    const unregister = (): void => {
      if (registrations.get(token) === client) registrations.delete(token);
    };
    void client.closed.then(unregister, unregister);
    if (remainder.byteLength > 0) socket.data.handlers?.data(remainder);
    settleWaiters(token, client);
  };

  const helloReader = (socket: WorkerRpcSocket): WorkerRpcSocketHandlers => {
    let pending: Buffer = Buffer.alloc(0);
    return {
      data(chunk) {
        pending = pending.byteLength === 0 ? chunk : Buffer.concat([pending, chunk]);
        const newline = pending.indexOf(0x0a);
        if (newline === -1) {
          if (pending.byteLength > MAX_HELLO_BYTES) reject(socket, "hello too long");
          return;
        }
        const line = pending.subarray(0, newline).toString("utf8").trim();
        const remainder = pending.subarray(newline + 1);
        pending = Buffer.alloc(0);
        let parsed: unknown;
        try {
          parsed = JSON.parse(line);
        } catch {
          reject(socket, "not json");
          return;
        }
        const frame =
          typeof parsed === "object" && parsed !== null
            ? (parsed as Record<string, unknown>)
            : undefined;
        const bootToken = frame?.bootToken;
        if (
          !frame ||
          frame.type !== "hello" ||
          typeof bootToken !== "string" ||
          bootToken.length === 0
        ) {
          reject(socket, "malformed hello");
          return;
        }
        const resolved = options.resolveBootToken(bootToken);
        if (!resolved) {
          reject(socket, "unknown boot token");
          return;
        }
        if (resolved.boot && resolved.boot.generation !== resolved.claim.generation) {
          reject(socket, "stale worker generation");
          return;
        }
        if (registrations.has(resolved.token)) {
          reject(socket, "already bound to a live stream");
          return;
        }
        register(socket, resolved.token, remainder);
      },
      drain() {},
      close() {},
      error() {},
    };
  };

  let server: Bun.TCPSocketListener<WorkerRpcSocketData>;
  try {
    server = Bun.listen<WorkerRpcSocketData>({
      hostname: options.hostname,
      port: options.port,
      socket: {
        ...workerRpcSocketHandlers,
        open(socket) {
          socket.data = { handlers: helloReader(socket) };
        },
      },
    });
  } catch (error) {
    throw new Error(
      `worker_stream_port ${options.port} on ${options.hostname} is unavailable: ${error instanceof Error ? error.message : String(error)}`,
      { cause: error }
    );
  }

  return {
    get port() {
      return server.port;
    },
    registrations,
    awaitRegistration(claimToken, timeoutMs) {
      if (closed) return Promise.reject(new Error("worker stream listener closed"));
      const existing = registrations.get(claimToken);
      if (existing) return Promise.resolve(existing);
      return new Promise<WorkerRpcClient>((resolve, rejectWaiter) => {
        const waiter: Waiter = { resolve, reject: rejectWaiter, timer: undefined };
        waiter.timer = setTimer(() => {
          const rest = (waiters.get(claimToken) ?? []).filter((candidate) => candidate !== waiter);
          if (rest.length > 0) waiters.set(claimToken, rest);
          else waiters.delete(claimToken);
          rejectWaiter(
            new Error(`worker stream for ${claimToken} did not register within ${timeoutMs}ms`)
          );
        }, timeoutMs);
        waiters.set(claimToken, [...(waiters.get(claimToken) ?? []), waiter]);
      });
    },
    close() {
      if (closed) return;
      closed = true;
      for (const pending of waiters.values()) {
        for (const waiter of pending) {
          clearTimer(waiter.timer);
          waiter.reject(new Error("worker stream listener closed"));
        }
      }
      waiters.clear();
      for (const client of registrations.values()) client.close();
      registrations.clear();
      server.stop(true);
    },
  };
}
