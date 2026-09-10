import { randomUUID } from "node:crypto";

const DEFAULT_RPC_TIMEOUT_MS = 5_000;

/**
 * A worker's run state, tracked from the moment its socket connects: `"unknown"` until the first
 * `agent_start`/`agent_end` frame is observed (including ones replayed from the shim's
 * pre-connect backlog on a reconnect), `"running"` between an `agent_start` and its matching
 * `agent_end`, `"idle"` otherwise. Callers treat anything other than `"idle"` as occupying a
 * running-worker slot — `"unknown"` is the conservative default for a freshly-connected or
 * mid-turn worker whose state hasn't been observed yet.
 */
export type WorkerRunState = "unknown" | "running" | "idle";

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
  /** The worker's run state, updated from every `agent_start`/`agent_end` frame seen on the
   * socket since it connected (including a reconnect's replayed backlog), and seeded from
   * `getState()`'s `isStreaming` field on a fresh connection that hasn't observed a frame yet. */
  readonly runState: WorkerRunState;
  negotiate(): Promise<void>;
  /** Marks `runState` as `"running"` before the request is even sent, so a concurrent caller
   * checking occupancy never sees free capacity in the gap between sending a prompt to an
   * already-idle worker and its `agent_start` frame arriving. If the prompt is refused (an
   * ordinary `{success:false}` response, socket and shim still alive) `runState` is restored to
   * whatever it was before this call instead of staying wrongly stuck at `"running"` — a rejected
   * prompt never started a real turn, so nothing will ever emit the `agent_end` frame that would
   * otherwise be the only way back to `"idle"`. */
  prompt(message: string): Promise<void>;
  /** Also seeds `runState` from the response's `isStreaming` field (`true` -> `"running"`,
   * `false` -> `"idle"`, firing `onIdle` on a transition into idle) when present, so a worker
   * that was already idle before this connection existed — e.g. reconnected after a daemon
   * restart — is not stuck at the conservative `"unknown"` default forever. */
  getState(timeoutMs?: number): Promise<Record<string, unknown>>;
  /** Asks the shim to close the wrapped OMP process's stdin; never SIGTERMs it. */
  shutdown(): void;
  close(): void;
  /**
   * Registers a callback fired once when `runState` transitions to `"idle"` from a non-idle
   * state. Pure trigger — "something may have freed capacity, re-check occupancy" — never a
   * source of occupancy itself (that's always `runState`/a fresh `runningWorkerCount()`
   * computation). Never fired on socket close: a closed socket's run state goes to `"unknown"`
   * (still occupying, conservatively), not `"idle"` — a worker whose socket just died might
   * still be mid-turn and about to reconnect, and firing this callback would wrongly signal
   * freed capacity for one the daemon hasn't yet confirmed is actually gone. Replaces any
   * previously registered callback.
   */
  onIdle(callback: () => void): void;
}

interface PendingRequest {
  resolve: (value: Record<string, unknown>) => void;
  reject: (error: Error) => void;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

/** Connects to a running worker-shim's unix socket and negotiates nothing by itself — call
 * `negotiate()` next. `connectTimeoutMs` (the daemon's configured `worker_rpc_timeout_seconds`,
 * `DEFAULT_RPC_TIMEOUT_MS` when a caller omits it, e.g. a test fixture) becomes this client's
 * default per-request timeout for `negotiate()` and any `getState()` call that does not pass its
 * own override. */
export async function connectWorkerRpc(
  socketPath: string,
  connectTimeoutMs: number = DEFAULT_RPC_TIMEOUT_MS
): Promise<WorkerRpcClient> {
  let buffer = "";
  const pending = new Map<string, PendingRequest>();
  const closedResolvers = Promise.withResolvers<void>();
  let runState: WorkerRunState = "unknown";
  let idleCallback: (() => void) | undefined;
  let loggedMissingIsStreaming = false;
  const markIdle = (): void => {
    const wasIdle = runState === "idle";
    runState = "idle";
    if (!wasIdle) idleCallback?.();
  };

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
    if (frame.type === "agent_start") {
      runState = "running";
    } else if (frame.type === "agent_end") {
      markIdle();
    }
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
        // Conservative, not idle: the daemon has not yet confirmed this worker is actually
        // gone (it may reconnect), so its slot must keep counting as occupied until
        // `markWorkerDead` decides otherwise — firing the idle trigger here would wrongly
        // signal freed capacity.
        runState = "unknown";
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
    timeoutMs = connectTimeoutMs
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
    get runState() {
      return runState;
    },
    async negotiate() {
      const response = await request("negotiate_protocol", { protocolVersion: 2 });
      if (response.command !== "negotiate_protocol" || response.success !== true) {
        throw new Error("Worker RPC protocol v2 negotiation failed");
      }
    },
    async prompt(message) {
      const previousRunState = runState;
      runState = "running";
      try {
        await request("prompt", { message });
      } catch (error) {
        // An ordinary {success:false} rejection means the worker refused the prompt but the
        // socket and its shim are still alive -- restore whatever runState was before this
        // attempt (usually "idle") instead of leaving it wrongly stuck at "running" forever,
        // which would make a live, idle worker look permanently busy to every later admission
        // decision. A socket-close rejection is different: the close handler above already
        // reset runState to "unknown" *synchronously*, before this catch ever runs (promise
        // rejection handling is always a later microtask) -- only restore when we are still
        // marked "running" (nothing else has touched it since), so a close's more authoritative
        // "unknown" is never clobbered back to the stale pre-prompt value.
        if (runState === "running") runState = previousRunState;
        throw error;
      }
    },
    getState(timeoutMs = connectTimeoutMs) {
      return request("get_state", {}, timeoutMs).then((response) => {
        const data = isRecord(response.data) ? response.data : undefined;
        if (typeof data?.isStreaming === "boolean") {
          if (data.isStreaming) runState = "running";
          else markIdle();
        } else if (!loggedMissingIsStreaming) {
          // Logged once per client, not once per call: a shim that never reports
          // `isStreaming` would otherwise repeat this on every `get_state` a reconnect or
          // periodic probe issues against it.
          loggedMissingIsStreaming = true;
          console.error(
            "[legion] worker RPC get_state response missing data.isStreaming; leaving runState unchanged"
          );
        }
        return response;
      });
    },
    shutdown() {
      socket.write(`${JSON.stringify({ type: "shutdown" })}\n`);
    },
    close() {
      socket.end();
    },
    onIdle(callback) {
      idleCallback = callback;
    },
  };
}

/** The shared connect-then-probe facts a `WorkerRpcClient`-reaching caller needs to decide
 * liveness — one of `client`/`connectError` is always set, never both. `stateAnswered` is only
 * meaningful when `client` is set: a connected socket whose `get_state` call itself rejects
 * (`stateError` set) still reached the shim — only a caller that requires an *answering* shim
 * (as opposed to a merely-reachable one) should treat `stateAnswered === false` as not alive. */
export interface SocketProbeResult {
  client: WorkerRpcClient | undefined;
  connectError: unknown;
  stateAnswered: boolean;
  stateError: unknown;
}

/** Connects to `socketPath` (via the caller's own connect-and-cache `connect` function — every
 * caller in this daemon reuses `ProcessManager.workerClient`'s per-token cache/negotiate) and
 * probes `get_state`, gathering the raw facts every liveness dialect in this package needs
 * (`reconnectWorkers`, `onWorkerClientClosed`, `spawnWorker`'s resume check, and the boot
 * watchdog's own alive probe) instead of each duplicating this same connect-then-getState
 * sequence. Deliberately does not itself decide or log anything: a connect failure means
 * different things for a boot watchdog (still slow, re-arm) versus a confirmed worker
 * (unambiguously dead), and a `get_state` failure means "busy, not dead" everywhere except
 * `spawnWorker`'s own resume decision (which requires an answering shim before prompting it
 * directly rather than relaunching) — every caller keeps its own verdict and its own logging
 * around these facts. `timeoutMs` should be the caller's own configured
 * `worker_rpc_timeout_seconds` (`DEFAULT_RPC_TIMEOUT_MS` when omitted, e.g. a test fixture) so a
 * probe's `get_state` call respects the same timeout the connection itself was opened with. */
export async function probeWorkerSocket(
  connect: (socketPath: string) => Promise<WorkerRpcClient>,
  socketPath: string,
  timeoutMs = DEFAULT_RPC_TIMEOUT_MS
): Promise<SocketProbeResult> {
  let client: WorkerRpcClient | undefined;
  let connectError: unknown;
  try {
    client = await connect(socketPath);
  } catch (error) {
    connectError = error;
  }
  if (!client) {
    return { client: undefined, connectError, stateAnswered: false, stateError: undefined };
  }
  let stateAnswered = true;
  let stateError: unknown;
  try {
    await client.getState(timeoutMs);
  } catch (error) {
    stateAnswered = false;
    stateError = error;
  }
  return { client, connectError: undefined, stateAnswered, stateError };
}
