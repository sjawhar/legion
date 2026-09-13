import { randomUUID } from "node:crypto";
import { createLineReader } from "./line-reader";
import { createSocketLineWriter } from "./socket-writer";

const DEFAULT_RPC_TIMEOUT_MS = 5_000;

/** Maximum UTF-8 size of one newline-delimited RPC frame, including the newline — mirrors
 * `MAX_RPC_FRAME_BYTES` in oh-my-pi's `rpc-frame.ts`. A protocol v2 `rpc_chunk` sequence's
 * declared `byteLength` must be at least this large: anything smaller would have fit in a
 * single plain frame. */
const MAX_RPC_FRAME_BYTES = 1024 * 1024;
/** Maximum UTF-8 size of one logical frame reassembled from `rpc_chunk` lines — mirrors
 * `MAX_RPC_REASSEMBLED_BYTES` in oh-my-pi's `rpc-frame.ts`. */
const MAX_RPC_REASSEMBLED_BYTES = 64 * 1024 * 1024;
/** Base64 payload size of one `rpc_chunk` line — mirrors `RPC_CHUNK_PAYLOAD_BYTES` in oh-my-pi's
 * `rpc-frame.ts`. */
const RPC_CHUNK_PAYLOAD_BYTES = 256 * 1024;
const RPC_CHUNK_BASE64_PATTERN = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;

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
 * What `WorkerRpcClient.prompt()` hands back once the shim has acknowledged the prompt. The
 * acknowledgement is not delivery: OMP's rpc mode answers `{success:true}` before it starts the
 * turn (`session.prompt(...)` is started, not awaited, in `rpc-mode.ts`'s `case "prompt"`), and a
 * fork build since commit 0e57a6ca can accept a message that starts no turn at all. The receipt
 * is how a caller learns the turn actually started.
 */
export interface PromptReceipt {
  /** Resolves when the worker's turn is observed to start: the first `agent_start` frame, or a
   * `get_state` answer with `isStreaming: true`, seen on this connection after this prompt was
   * written. Never rejects and never times out — the caller bounds it — and still resolves after
   * `abandonWait()` (a late start). Settled at most once; a later `prompt()` on the same client
   * supersedes this receipt, and a superseded receipt never settles. */
  readonly turnStarted: Promise<void>;
  /** Synchronous view of `turnStarted`, so a caller can skip building a bounded race when the
   * start already arrived (an `agent_start` can precede the acknowledgement). */
  readonly hasStarted: boolean;
  /** The caller's bound expired first: restores `runState` to its pre-prompt value without
   * firing `onIdle`, exactly like a refused prompt — only if this receipt is still the last
   * prompt sent on this client, the turn has not been observed, and `runState` is still the
   * optimistic `"running"` (a socket close's `"unknown"` is never clobbered). Idempotent. */
  abandonWait(): void;
}

/**
 * Minimal client for the OMP RPC protocol v2, reached through a worker's
 * `legion worker-shim` stream — the unix socket the daemon dials or the TCP
 * stream a `--connect` shim dialed in — rather than a spawned process's stdio.
 * The shim forwards every frame between the stream and the wrapped
 * `omp --mode rpc` process unchanged, so this client speaks the same
 * newline-delimited JSON protocol `packages/coding-agent/src/modes/rpc/rpc-mode.ts`
 * implements. Any logical frame larger than `MAX_RPC_FRAME_BYTES` arrives as a
 * sequence of `rpc_chunk` lines instead of one plain JSONL line; this client
 * reassembles that sequence before dispatching the frame through the same path a
 * plain frame takes (see `createWorkerRpcClient`'s `pushRpcChunk`). A worker's
 * `agent_end` frame — which carries the whole turn's `messages` — is the frame
 * most likely to arrive chunked, since it is the only one whose size scales with
 * turn length.
 */
export interface WorkerRpcClient {
  /** Resolves once the underlying socket connection closes. */
  readonly closed: Promise<void>;
  /** The worker's run state, updated from every `agent_start`/`agent_end` frame seen on the
   * socket since it connected (including a reconnect's replayed backlog), and seeded from
   * `getState()`'s `isStreaming` field on a fresh connection that hasn't observed a frame yet. */
  readonly runState: WorkerRunState;
  negotiate(): Promise<void>;
  /** Resolves on the shim's acknowledgement with a `PromptReceipt`; the acknowledgement alone is
   * not delivery — the receipt's `turnStarted` is how a caller learns the turn began. Marks
   * `runState` as `"running"` before the request is even sent, so a concurrent caller checking
   * occupancy never sees free capacity in the gap between sending a prompt to an already-idle
   * worker and its `agent_start` frame arriving. If the prompt is refused (an ordinary
   * `{success:false}` response, socket and shim still alive) this rejects with the shim's error
   * and `runState` is restored to whatever it was before this call instead of staying wrongly
   * stuck at `"running"` — a rejected prompt never started a real turn, so nothing will ever emit
   * the `agent_end` frame that would otherwise be the only way back to `"idle"`. A caller whose
   * bound on `turnStarted` expires performs the same restore through `receipt.abandonWait()`. */
  prompt(message: string): Promise<PromptReceipt>;
  /** Also seeds `runState` from the response's `isStreaming` field (`true` -> `"running"`,
   * `false` -> `"idle"`, firing `onIdle` on a transition into idle) when present, so a worker
   * that was already idle before this connection existed — e.g. reconnected after a daemon
   * restart — is not stuck at the conservative `"unknown"` default forever. An `isStreaming:
   * true` answer also resolves the last prompt's pending `turnStarted`. */
  getState(timeoutMs?: number): Promise<Record<string, unknown>>;
  /** Asks the shim to close the wrapped OMP process's stdin; never SIGTERMs it. */
  shutdown(): void;
  close(): void;
  /**
   * Registers a callback fired once when `runState` transitions to `"idle"` from a non-idle
   * state. Pure trigger — "something may have freed capacity, re-check occupancy" — never a
   * source of occupancy itself (that's always `runState`/a fresh `runningWorkerCount()`
   * computation). `ProcessManager` also arms the worker's idle-retire clock from this same
   * trigger (both consumers live in one composed callback in `clientFor()`), judging every
   * retire condition at that clock's expiry rather than here. Never fired on socket close: a
   * closed socket's run state goes to `"unknown"` (still occupying, conservatively), not
   * `"idle"` — a worker whose socket just died might still be mid-turn and about to reconnect,
   * and firing this callback would wrongly signal freed capacity for one the daemon hasn't yet
   * confirmed is actually gone. Replaces any previously registered callback.
   */
  onIdle(callback: () => void): void;
}

/** The settle callback of the last prompt sent on a connection, armed before the prompt is
 * written so an `agent_start` racing the acknowledgement is not missed; one slot per client. */
interface PendingTurnStart {
  resolve: () => void;
}

interface PendingRequest {
  resolve: (value: Record<string, unknown>) => void;
  reject: (error: Error) => void;
}

interface PendingRpcChunks {
  chunkId: string;
  count: number;
  byteLength: number;
  nextIndex: number;
  chunks: Buffer[];
  receivedBytes: number;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

/** Decodes one `rpc_chunk` frame's base64 `data` field, rejecting anything that isn't
 * well-formed (non-canonical) base64 — mirrors `decodeBase64` in oh-my-pi's `rpc-frame.ts`. */
function decodeRpcChunkData(data: unknown): Buffer {
  if (typeof data !== "string" || data.length === 0 || !RPC_CHUNK_BASE64_PATTERN.test(data)) {
    throw new Error("invalid rpc chunk data");
  }
  const bytes = Buffer.from(data, "base64");
  if (bytes.toString("base64") !== data) throw new Error("invalid rpc chunk data");
  return bytes;
}

/** The per-connection event sink a `WorkerRpcClient` (or a listener reading a preamble first)
 * installs in `socket.data.handlers`; `workerRpcSocketHandlers` forwards every Bun socket event
 * to whatever is installed there. */
export interface WorkerRpcSocketHandlers {
  data(chunk: Buffer): void;
  drain(): void;
  close(): void;
  error(error: Error): void;
}

export interface WorkerRpcSocketData {
  handlers: WorkerRpcSocketHandlers | undefined;
}

export type WorkerRpcSocket = Bun.Socket<WorkerRpcSocketData>;

/** The static Bun handler table every transport that carries a `WorkerRpcClient` binds at
 * `Bun.connect`/`Bun.listen`. Bun binds one table per call, not per connection, so this table
 * forwards each event to whatever `socket.data.handlers` currently holds: the client installs
 * itself there (`createWorkerRpcClient`), and a listener that has to read something first — the
 * worker stream listener's `hello` line — installs its own sink in `open` and swaps afterwards. */
export const workerRpcSocketHandlers: Bun.SocketHandler<WorkerRpcSocketData> = {
  data(socket, chunk) {
    socket.data.handlers?.data(chunk);
  },
  drain(socket) {
    socket.data.handlers?.drain();
  },
  close(socket) {
    socket.data.handlers?.close();
  },
  error(socket, error) {
    socket.data.handlers?.error(error);
  },
};

/** Wraps an already-open Bun socket — the unix socket `connectWorkerRpc` dialed, or a TCP socket
 * the worker stream listener accepted — in a protocol v2 client. Attaches by installing its
 * handlers in `socket.data.handlers`, so the socket must have been created with
 * `workerRpcSocketHandlers` (or a table that spreads it). Negotiates nothing by itself — call
 * `negotiate()` next. `timeoutMs` (the daemon's configured `worker_rpc_timeout_seconds`,
 * `DEFAULT_RPC_TIMEOUT_MS` when a caller omits it, e.g. a test fixture) is the default
 * per-request timeout for `negotiate()` and any `getState()` call without its own override. */
export function createWorkerRpcClient(
  socket: WorkerRpcSocket,
  timeoutMs: number = DEFAULT_RPC_TIMEOUT_MS
): WorkerRpcClient {
  const pending = new Map<string, PendingRequest>();
  const closedResolvers = Promise.withResolvers<void>();
  let runState: WorkerRunState = "unknown";
  let idleCallback: (() => void) | undefined;
  let loggedMissingIsStreaming = false;
  let pendingChunks: PendingRpcChunks | undefined;
  let pendingTurnStart: PendingTurnStart | undefined;
  const markIdle = (): void => {
    const wasIdle = runState === "idle";
    runState = "idle";
    if (!wasIdle) idleCallback?.();
  };
  /** A turn was observed to start (`agent_start`, or `get_state` reporting a stream): the worker
   * is running, and the last prompt's receipt — if one is still waiting — is settled. */
  const observeTurnStart = (): void => {
    runState = "running";
    const slot = pendingTurnStart;
    pendingTurnStart = undefined;
    slot?.resolve();
  };

  const failAllPending = (error: Error): void => {
    for (const request of pending.values()) request.reject(error);
    pending.clear();
  };

  const dropPendingChunks = (reason: string): void => {
    pendingChunks = undefined;
    console.error(`[legion] worker RPC dropped a malformed rpc_chunk sequence: ${reason}`);
  };

  /** Reassembles one protocol v2 `rpc_chunk` line into the pending sequence for this
   * connection, mirroring the validation semantics of `RpcFrameDecoder` in oh-my-pi's
   * `rpc-frame.ts`. Returns the decoded logical frame once the final chunk lands, and
   * `undefined` both while still waiting for more chunks and after dropping a malformed
   * sequence (logged once via `dropPendingChunks`) — callers cannot and need not tell the two
   * apart, since both mean "nothing to dispatch yet". */
  const pushRpcChunk = (value: Record<string, unknown>): Record<string, unknown> | undefined => {
    const { chunkId, index, count, byteLength, data } = value;
    if (
      typeof chunkId !== "string" ||
      chunkId.length === 0 ||
      chunkId.length > 128 ||
      typeof index !== "number" ||
      typeof count !== "number" ||
      typeof byteLength !== "number" ||
      !Number.isSafeInteger(index) ||
      !Number.isSafeInteger(count) ||
      !Number.isSafeInteger(byteLength) ||
      index < 0 ||
      count < 2 ||
      count > Math.ceil(MAX_RPC_REASSEMBLED_BYTES / RPC_CHUNK_PAYLOAD_BYTES) ||
      index >= count ||
      byteLength < MAX_RPC_FRAME_BYTES ||
      byteLength > MAX_RPC_REASSEMBLED_BYTES
    ) {
      dropPendingChunks("invalid rpc chunk metadata");
      return undefined;
    }
    let bytes: Buffer;
    try {
      bytes = decodeRpcChunkData(data);
    } catch (error) {
      dropPendingChunks(error instanceof Error ? error.message : "invalid rpc chunk data");
      return undefined;
    }
    if (bytes.byteLength > RPC_CHUNK_PAYLOAD_BYTES) {
      dropPendingChunks("rpc chunk payload exceeds the transport limit");
      return undefined;
    }

    let sequence = pendingChunks;
    if (!sequence) {
      if (index !== 0) {
        dropPendingChunks("rpc chunk sequence must start at index 0");
        return undefined;
      }
      sequence = { chunkId, count, byteLength, nextIndex: 0, chunks: [], receivedBytes: 0 };
      pendingChunks = sequence;
    }
    if (
      sequence.chunkId !== chunkId ||
      sequence.count !== count ||
      sequence.byteLength !== byteLength ||
      sequence.nextIndex !== index
    ) {
      dropPendingChunks("rpc chunk sequence mismatch");
      return undefined;
    }
    sequence.chunks.push(bytes);
    sequence.receivedBytes += bytes.byteLength;
    sequence.nextIndex++;
    if (sequence.receivedBytes > sequence.byteLength) {
      dropPendingChunks("rpc chunk sequence exceeds declared length");
      return undefined;
    }
    if (sequence.nextIndex < sequence.count) return undefined;
    if (sequence.receivedBytes !== sequence.byteLength) {
      dropPendingChunks("rpc chunk sequence length mismatch");
      return undefined;
    }

    pendingChunks = undefined;
    let decoded: string;
    try {
      decoded = new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(sequence.chunks));
    } catch {
      dropPendingChunks("reassembled payload is not valid utf-8");
      return undefined;
    }
    let reassembled: unknown;
    try {
      reassembled = JSON.parse(decoded);
    } catch {
      dropPendingChunks("reassembled payload is not valid json");
      return undefined;
    }
    if (!isRecord(reassembled)) {
      dropPendingChunks("reassembled frame must be an object");
      return undefined;
    }
    return reassembled;
  };

  const handleLine = (line: string): void => {
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      return;
    }
    if (!isRecord(parsed)) return;

    let frame: Record<string, unknown>;
    if (parsed.type === "rpc_chunk") {
      const reassembled = pushRpcChunk(parsed);
      if (reassembled === undefined) return;
      frame = reassembled;
    } else {
      if (pendingChunks)
        dropPendingChunks("a non-chunk frame arrived while a sequence was pending");
      frame = parsed;
    }

    if (frame.type === "agent_start") {
      observeTurnStart();
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

  const writer = createSocketLineWriter(socket);
  const reader = createLineReader(handleLine);
  socket.data.handlers = {
    data(chunk) {
      reader.push(chunk);
    },
    drain() {
      writer.drain();
    },
    close() {
      writer.clear();
      failAllPending(new Error("Worker RPC socket closed"));
      // Conservative, not idle: the daemon has not yet confirmed this worker is actually
      // gone (it may reconnect), so its slot must keep counting as occupied until
      // `markWorkerDead` decides otherwise — firing the idle trigger here would wrongly
      // signal freed capacity.
      runState = "unknown";
      closedResolvers.resolve();
    },
    error(error) {
      writer.clear();
      failAllPending(error);
      closedResolvers.reject(error);
    },
  };

  const request = (
    type: string,
    extra: Record<string, unknown> = {},
    requestTimeoutMs = timeoutMs
  ): Promise<Record<string, unknown>> => {
    const id = randomUUID();
    const settled = Promise.withResolvers<Record<string, unknown>>();
    pending.set(id, settled);
    const timer = setTimeout(() => {
      if (pending.delete(id)) {
        settled.reject(new Error(`Worker RPC "${type}" timed out after ${requestTimeoutMs}ms`));
      }
    }, requestTimeoutMs);
    writer.write(JSON.stringify({ id, type, ...extra }));
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
      let hasStarted = false;
      const started = Promise.withResolvers<void>();
      const slot: PendingTurnStart = {
        resolve: () => {
          hasStarted = true;
          started.resolve();
        },
      };
      // Armed before the request is written: the worker's `agent_start` can reach us before
      // the acknowledgement does, and a slot armed afterwards would miss it.
      pendingTurnStart = slot;
      runState = "running";
      try {
        await request("prompt", { message });
      } catch (error) {
        if (pendingTurnStart === slot) pendingTurnStart = undefined;
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
      return {
        turnStarted: started.promise,
        get hasStarted() {
          return hasStarted;
        },
        abandonWait() {
          // The same restore as a refused prompt, under the same "nothing else has touched it"
          // guard, plus two of its own: the turn must not have been observed, and this must
          // still be the last prompt sent (a later prompt owns runState now). The slot stays
          // armed so a turn that starts late still settles `turnStarted`.
          if (hasStarted || pendingTurnStart !== slot || runState !== "running") return;
          runState = previousRunState;
        },
      };
    },
    getState(requestTimeoutMs = timeoutMs) {
      return request("get_state", {}, requestTimeoutMs).then((response) => {
        const data = isRecord(response.data) ? response.data : undefined;
        if (typeof data?.isStreaming === "boolean") {
          if (data.isStreaming) observeTurnStart();
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
      writer.write(JSON.stringify({ type: "shutdown" }));
    },
    close() {
      socket.end();
    },
    onIdle(callback) {
      idleCallback = callback;
    },
  };
}

/** Connects to a running worker-shim's unix socket and wraps it (see `createWorkerRpcClient`).
 * `connectTimeoutMs` is the daemon's configured `worker_rpc_timeout_seconds`. */
export async function connectWorkerRpc(
  socketPath: string,
  connectTimeoutMs: number = DEFAULT_RPC_TIMEOUT_MS
): Promise<WorkerRpcClient> {
  const socket = await Bun.connect<WorkerRpcSocketData>({
    unix: socketPath,
    data: { handlers: undefined },
    socket: workerRpcSocketHandlers,
  });
  return createWorkerRpcClient(socket, connectTimeoutMs);
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
 * caller in this daemon reuses `ProcessManager`'s per-token cache/negotiate) and
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
