import fs from "node:fs";
import { createSocketLineWriter, type SocketLineWriter } from "../daemon/socket-writer";
import { CliError } from "./errors";

export interface WorkerShimSpawnedProcess {
  readonly stdin: { write(data: Uint8Array): void; end(): void };
  readonly stdout: ReadableStream<Uint8Array>;
  readonly exited: Promise<number>;
}

export interface WorkerShimSocketHandle {
  write(line: string): void;
  end(): void;
}

export interface WorkerShimListener {
  stop(): void;
}

export interface WorkerShimEndpoint {
  host: string;
  port: number;
}

/** `--socket` mode's dependencies: the tmux runtime's shim listens for the daemon to dial in. */
export interface WorkerShimDeps {
  spawn(argv: string[]): WorkerShimSpawnedProcess;
  /** Listens on `socketPath`; at most one client is connected at a time. */
  listen(
    socketPath: string,
    handlers: {
      onLine(line: string): void;
      onConnect(socket: WorkerShimSocketHandle): void;
      onDisconnect(): void;
    }
  ): WorkerShimListener;
  log(line: string): void;
}

/** `--connect` mode's dependencies: the shim dials the daemon's worker stream listener. */
export interface WorkerShimConnectDeps {
  spawn(argv: string[]): WorkerShimSpawnedProcess;
  /** Dials `endpoint` once: resolves with the open connection's handle, rejects if the dial fails.
   * `onLine`/`onDisconnect` fire for that one connection only; `onDisconnect` never fires for a
   * dial that never opened. */
  connect(
    endpoint: WorkerShimEndpoint,
    handlers: { onLine(line: string): void; onDisconnect(): void }
  ): Promise<WorkerShimSocketHandle>;
  /** Backoff sleep between reconnect attempts (`Bun.sleep`); tests inject a recorder. */
  sleep(ms: number): Promise<void>;
  log(line: string): void;
}

export type WorkerShimTarget =
  | { mode: "socket"; socketPath: string }
  | { mode: "connect"; endpoint: WorkerShimEndpoint; bootToken: string };

const BACKLOG_LIMIT = 1000;
const RECONNECT_BASE_MS = 200;
const RECONNECT_CAP_MS = 5_000;

function summarizeShimFrame(frame: unknown): string | undefined {
  if (typeof frame !== "object" || frame === null) return undefined;
  const record = frame as Record<string, unknown>;
  switch (record.type) {
    case "agent_start":
      return "agent_start";
    case "agent_end":
      return "agent_end";
    case "tool_execution_start": {
      const toolName = typeof record.toolName === "string" ? record.toolName : "unknown";
      const args = JSON.stringify(record.args ?? {}).slice(0, 80);
      return `tool_execution_start ${toolName} ${args}`;
    }
    case "response":
      if (record.success === false) {
        return `error ${typeof record.command === "string" ? record.command : ""} ${typeof record.error === "string" ? record.error : ""}`.trim();
      }
      return undefined;
    case "error":
      return `error ${typeof record.message === "string" ? record.message : JSON.stringify(record)}`;
    default:
      return undefined;
  }
}

function safeParseJsonLine(line: string): unknown {
  try {
    return JSON.parse(line);
  } catch {
    return undefined;
  }
}

function isShimFrame(line: string, type: string): boolean {
  const parsed = safeParseJsonLine(line);
  return (
    typeof parsed === "object" &&
    parsed !== null &&
    (parsed as Record<string, unknown>).type === type
  );
}

/** The transport-independent half of the shim, shared by `--socket` and `--connect`: summarizes
 * and forwards the wrapped process's stdout frames to the connected daemon (or the bounded
 * backlog while none is connected, replayed in order on the next connect), and forwards daemon
 * frames to the process's stdin — except `shutdown`, which closes that stdin instead. */
interface ShimBridge {
  /** Starts pumping `child`'s stdout. Called once, when the child is spawned, before any daemon
   * frame can reach `onLine`. */
  attach(child: WorkerShimSpawnedProcess): void;
  onConnect(client: WorkerShimSocketHandle): void;
  onDisconnect(): void;
  onLine(line: string): void;
}

function createShimBridge(log: (line: string) => void): ShimBridge {
  let child: WorkerShimSpawnedProcess | undefined;
  let client: WorkerShimSocketHandle | undefined;
  const backlog: string[] = [];
  let droppedBacklogFrames = false;

  const forwardToSocket = (line: string): void => {
    const summary = summarizeShimFrame(safeParseJsonLine(line));
    if (summary) log(summary);
    if (client) {
      client.write(line);
      return;
    }
    backlog.push(line);
    if (backlog.length > BACKLOG_LIMIT) {
      backlog.shift();
      if (!droppedBacklogFrames) {
        droppedBacklogFrames = true;
        log(
          `[worker-shim] no daemon client connected; dropping oldest frames beyond the last ${BACKLOG_LIMIT}`
        );
      }
    }
  };

  return {
    attach(spawned) {
      child = spawned;
      void (async () => {
        const decoder = new TextDecoder();
        let buffer = "";
        for await (const chunk of spawned.stdout as unknown as AsyncIterable<Uint8Array>) {
          buffer += decoder.decode(chunk, { stream: true });
          let index = buffer.indexOf("\n");
          while (index !== -1) {
            forwardToSocket(buffer.slice(0, index));
            buffer = buffer.slice(index + 1);
            index = buffer.indexOf("\n");
          }
        }
      })();
    },
    onConnect(socket) {
      client = socket;
      for (const line of backlog.splice(0)) socket.write(line);
    },
    onDisconnect() {
      client = undefined;
    },
    onLine(line) {
      if (!child) {
        // Both modes spawn before any daemon frame can arrive (`--socket` spawns before it
        // listens; `--connect` spawns inside the ack callback, ahead of any later line), so a
        // frame here is a broken invariant, not a condition to paper over.
        throw new Error(
          "worker-shim: daemon frame received before the wrapped process was spawned"
        );
      }
      if (isShimFrame(line, "shutdown")) {
        child.stdin.end();
        return;
      }
      child.stdin.write(new TextEncoder().encode(`${line}\n`));
    },
  };
}

/**
 * Spawns `argv` (an OMP process in `--mode rpc`) with piped stdio, forwards newline-delimited
 * RPC frames between the child's stdio and a unix socket, and prints a one-line human summary
 * of agent_start/tool_execution_start/agent_end/error frames to its own stdout. A `{type:"shutdown"}`
 * frame from the socket closes the child's stdin instead of being forwarded; the shim exits with
 * the child's exit code. Frames arriving before a socket client connects are buffered, bounded to
 * the most recent 1000 so a stalled daemon cannot make this process grow without limit.
 */
export async function cmdWorkerShim(
  socketPath: string,
  argv: string[],
  deps: WorkerShimDeps
): Promise<number> {
  if (argv.length === 0)
    throw new CliError("Usage: legion worker-shim --socket <path> -- <omp argv…>");
  const bridge = createShimBridge(deps.log);
  const child = deps.spawn(argv);
  const listener = deps.listen(socketPath, {
    onConnect: bridge.onConnect,
    onDisconnect: bridge.onDisconnect,
    onLine: bridge.onLine,
  });
  bridge.attach(child);
  const exitCode = await child.exited;
  listener.stop();
  return exitCode;
}

/**
 * Reverse-dial mode: dials the daemon's worker stream listener, sends one
 * `{"type":"hello","bootToken"}` line, and — only once the daemon answers `{"type":"hello_ack"}` —
 * spawns `argv` and bridges its stdio exactly as `cmdWorkerShim` does over a unix socket. While
 * the child lives, a dropped stream (or a dial the daemon refuses) is retried with exponential
 * backoff from 200 ms capped at 5 s, `hello` is re-sent, and frames produced during the gap are
 * delivered from the backlog in order after the next ack. Exits with the child's exit code.
 */
export async function cmdWorkerShimConnect(
  endpoint: WorkerShimEndpoint,
  bootToken: string,
  argv: string[],
  deps: WorkerShimConnectDeps
): Promise<number> {
  if (argv.length === 0) {
    throw new CliError(
      "Usage: legion worker-shim --connect tcp://<host>:<port> --boot-token-file <path> -- <omp argv…>"
    );
  }
  const bridge = createShimBridge(deps.log);
  const helloLine = JSON.stringify({ type: "hello", bootToken });
  const exit = Promise.withResolvers<number>();
  let exited = false;
  let child: WorkerShimSpawnedProcess | undefined;
  let failures = 0;

  while (!exited) {
    const acked = Promise.withResolvers<void>();
    void acked.promise.catch(() => undefined); // observed through the race below; never left dangling
    const disconnected = Promise.withResolvers<void>();
    let ackSeen = false;
    let dropped = false;
    let handle: WorkerShimSocketHandle | undefined;
    try {
      handle = await deps.connect(endpoint, {
        onLine: (line) => {
          if (ackSeen) {
            bridge.onLine(line);
            return;
          }
          if (!isShimFrame(line, "hello_ack")) {
            deps.log(
              `[worker-shim] ignoring a frame received before hello_ack: ${line.slice(0, 80)}`
            );
            return;
          }
          ackSeen = true;
          // Spawned here, synchronously inside the transport callback, not after the race below:
          // the daemon's first RPC frame can share a read with the ack (the listener acks and a
          // resolved waiter negotiates in one turn), so the very next `onLine` must already find
          // a child to write to.
          if (!child) {
            child = deps.spawn(argv);
            bridge.attach(child);
            void child.exited.then((code) => {
              exited = true;
              exit.resolve(code);
            });
          }
          acked.resolve();
        },
        onDisconnect: () => {
          // Synchronously, inside the transport callback: a frame the child emits on the very
          // next tick must already find no client and land in the backlog, never in a dead writer.
          dropped = true;
          bridge.onDisconnect();
          acked.reject(new Error("stream closed before hello_ack"));
          disconnected.resolve();
        },
      });
      handle.write(helloLine);
      await Promise.race([acked.promise, exit.promise]);
      if (exited) break;
      failures = 0;
      // The stream can drop in the same tick as the ack (a daemon that acks and dies); attaching a
      // dead handle would swallow the frames emitted before the next dial.
      if (!dropped) bridge.onConnect(handle);
      await Promise.race([disconnected.promise, exit.promise]);
    } catch (error) {
      const delay = Math.min(RECONNECT_BASE_MS * 2 ** failures, RECONNECT_CAP_MS);
      failures += 1;
      deps.log(
        `[worker-shim] daemon stream ${endpoint.host}:${endpoint.port} unavailable (${error instanceof Error ? error.message : String(error)}); retrying in ${delay}ms`
      );
      await Promise.race([deps.sleep(delay), exit.promise]);
    } finally {
      if (exited) handle?.end();
    }
  }
  return exit.promise;
}

function parseTcpEndpoint(value: string): WorkerShimEndpoint {
  const invalid = () =>
    new CliError(
      `legion worker-shim: --connect must be tcp://<host>:<port>, got ${JSON.stringify(value)}`
    );
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw invalid();
  }
  const port = Number(url.port);
  if (
    url.protocol !== "tcp:" ||
    url.hostname === "" ||
    url.port === "" ||
    !Number.isInteger(port) ||
    port < 1 ||
    port > 65535 ||
    (url.pathname !== "" && url.pathname !== "/") ||
    url.search !== "" ||
    url.hash !== "" ||
    url.username !== "" ||
    url.password !== ""
  ) {
    throw invalid();
  }
  const host =
    url.hostname.startsWith("[") && url.hostname.endsWith("]")
      ? url.hostname.slice(1, -1)
      : url.hostname;
  return { host, port };
}

/** Turns the `worker-shim` flags into a target, or throws the CLI error the operator sees: the
 * two modes are mutually exclusive, `--connect` needs a readable non-blank `--boot-token-file`,
 * and every failure names the flag (and path) involved. Nothing is spawned before this succeeds. */
export function resolveWorkerShimTarget(
  flags: { socket?: string; connect?: string; bootTokenFile?: string },
  readFile: (path: string) => string = (path) => fs.readFileSync(path, "utf8")
): WorkerShimTarget {
  const { socket, connect, bootTokenFile } = flags;
  if (socket !== undefined && connect !== undefined) {
    throw new CliError(
      "legion worker-shim: --socket and --connect are mutually exclusive; pass exactly one"
    );
  }
  if (socket !== undefined) {
    if (bootTokenFile !== undefined) {
      throw new CliError("legion worker-shim: --boot-token-file is only valid with --connect");
    }
    return { mode: "socket", socketPath: socket };
  }
  if (connect === undefined) {
    throw new CliError(
      "Usage: legion worker-shim (--socket <path> | --connect tcp://<host>:<port> --boot-token-file <path>) -- <omp argv…>"
    );
  }
  if (bootTokenFile === undefined) {
    throw new CliError("legion worker-shim: --connect requires --boot-token-file <path>");
  }
  const endpoint = parseTcpEndpoint(connect);
  let contents: string;
  try {
    contents = readFile(bootTokenFile);
  } catch (error) {
    throw new CliError(
      `legion worker-shim: --boot-token-file ${bootTokenFile} is unreadable: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const token = contents.trim();
  if (token.length === 0) {
    throw new CliError(`legion worker-shim: --boot-token-file ${bootTokenFile} is blank`);
  }
  return { mode: "connect", endpoint, bootToken: token };
}

export function defaultWorkerShimDeps(): WorkerShimDeps & WorkerShimConnectDeps {
  return {
    spawn: (argv) => {
      const proc = Bun.spawn(argv, { stdin: "pipe", stdout: "pipe", stderr: "inherit" });
      return {
        stdin: proc.stdin,
        stdout: proc.stdout as unknown as ReadableStream<Uint8Array>,
        exited: proc.exited,
      };
    },
    listen: (socketPath, handlers) => {
      fs.rmSync(socketPath, { force: true });
      let buffer = "";
      const writers = new WeakMap<object, SocketLineWriter>();
      const server = Bun.listen({
        unix: socketPath,
        socket: {
          open(socket) {
            const writer = createSocketLineWriter(socket);
            writers.set(socket, writer);
            handlers.onConnect({
              write: (line) => writer.write(line),
              end: () => socket.end(),
            });
          },
          drain(socket) {
            writers.get(socket)?.drain();
          },
          data(_socket, data) {
            buffer += data.toString("utf8");
            let index = buffer.indexOf("\n");
            while (index !== -1) {
              const line = buffer.slice(0, index).trim();
              buffer = buffer.slice(index + 1);
              if (line) handlers.onLine(line);
              index = buffer.indexOf("\n");
            }
          },
          close(socket) {
            writers.get(socket)?.clear();
            writers.delete(socket);
            buffer = "";
            handlers.onDisconnect();
          },
        },
      });
      return { stop: () => server.stop(true) };
    },
    connect: async (endpoint, handlers) => {
      let buffer = "";
      let writer: SocketLineWriter | undefined;
      let opened = false;
      const socket = await Bun.connect<undefined>({
        hostname: endpoint.host,
        port: endpoint.port,
        socket: {
          open() {
            opened = true;
          },
          drain() {
            writer?.drain();
          },
          data(_socket, data) {
            buffer += data.toString("utf8");
            let index = buffer.indexOf("\n");
            while (index !== -1) {
              const line = buffer.slice(0, index).trim();
              buffer = buffer.slice(index + 1);
              if (line) handlers.onLine(line);
              index = buffer.indexOf("\n");
            }
          },
          close() {
            writer?.clear();
            buffer = "";
            if (opened) handlers.onDisconnect();
          },
          error() {},
        },
      });
      writer = createSocketLineWriter(socket);
      const lineWriter = writer;
      return { write: (line) => lineWriter.write(line), end: () => socket.end() };
    },
    sleep: (ms) => Bun.sleep(ms),
    log: (line) => {
      console.log(line);
    },
  };
}
