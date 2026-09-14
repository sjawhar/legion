import fs from "node:fs";
import path from "node:path";
import { runAdoptWorkingCopy } from "@legion/workspace";
import { createLineReader } from "../daemon/line-reader";
import { createSocketLineWriter, type SocketLineWriter } from "../daemon/socket-writer";
import { defaultRunner } from "../state/fetch";
import { CliError } from "./errors";
import { processEnvRunner } from "./workspace-init";

export interface WorkerShimSpawnedProcess {
  readonly stdin: { write(data: Uint8Array): void; end(): void };
  readonly stdout: ReadableStream<Uint8Array>;
  readonly exited: Promise<number>;
  /** `SIGTERM` to the wrapped process: the fallback when closing its stdin did not end it. */
  kill(): void;
}

/** How long a terminating shim gives the wrapped OMP process to exit on its closed stdin before
 * falling back to SIGTERM, when nothing tells it the pod's grace: `--socket` mode (no pod), or a
 * `--connect` shim without `LEGION_TERMINATION_GRACE_SECONDS`. Half the pod's default
 * `terminationGracePeriodSeconds` (the daemon's `worker_stop_timeout_seconds`, 10). */
const DEFAULT_TERMINATE_STDIN_GRACE_MS = 5_000;

/** The stdin grace for a pod's shim: half its `terminationGracePeriodSeconds` (the runtime sets
 * `LEGION_TERMINATION_GRACE_SECONDS` on the main container), floored at 1 s -- the kubelet's own
 * SIGKILL lands at the full grace, and the fallback SIGTERM must have had time to work before it.
 * Unset: the default above. A malformed value is a bug in the only writer and refused. */
export function terminateStdinGraceMs(env: NodeJS.ProcessEnv): number {
  const value = env.LEGION_TERMINATION_GRACE_SECONDS;
  if (value === undefined) return DEFAULT_TERMINATE_STDIN_GRACE_MS;
  if (!/^[0-9]+$/.test(value)) {
    throw new CliError(
      `LEGION_TERMINATION_GRACE_SECONDS must be a whole number of seconds (got ${JSON.stringify(value)})`
    );
  }
  return Math.max(1_000, Math.floor((Number(value) * 1000) / 2));
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

/** The one command the daemon may ask a shim to run, as a frame of its own
 * (`{type: "adopt-working-copy", id, jjUser, jjEmail, timeoutMs}`, answered by
 * `{type: "adopt-working-copy-result", id, ok, error?}`): the working-copy adoption
 * (`adoptWorkingCopyCommand`) in the process's own workspace under the given jj identity. The
 * daemon names the identity and the budget, never a command or a path -- the shim owns both. */
export interface WorkerShimAdoption {
  jjUser: string;
  jjEmail: string;
  timeoutMs: number;
}

export type WorkerShimAdoptionResult = { ok: true } | { ok: false; error: string };

/** `--socket` mode's dependencies: the tmux runtime's shim listens for the daemon to dial in. */
export interface WorkerShimDeps {
  spawn(argv: string[], env?: Record<string, string>): WorkerShimSpawnedProcess;
  /** Listens on `socketPath`; at most one client is connected at a time. */
  listen(
    socketPath: string,
    handlers: {
      onLine(line: string): void;
      onConnect(socket: WorkerShimSocketHandle): void;
      onDisconnect(): void;
    }
  ): WorkerShimListener;
  /** Runs the working-copy adoption the daemon asked for (see `WorkerShimAdoption`). */
  adopt(adoption: WorkerShimAdoption): Promise<WorkerShimAdoptionResult>;
  log(line: string): void;
}

/** `--connect` mode's dependencies: the shim dials the daemon's worker stream listener. */
export interface WorkerShimConnectDeps {
  spawn(argv: string[], env?: Record<string, string>): WorkerShimSpawnedProcess;
  /** Dials `endpoint` once: resolves with the open connection's handle, rejects if the dial fails.
   * `onLine`/`onDisconnect` fire for that one connection only; `onDisconnect` never fires for a
   * dial that never opened. */
  connect(
    endpoint: WorkerShimEndpoint,
    handlers: { onLine(line: string): void; onDisconnect(): void }
  ): Promise<WorkerShimSocketHandle>;
  /** Backoff sleep between reconnect attempts (`Bun.sleep`); tests inject a recorder. */
  sleep(ms: number): Promise<void>;
  /** Runs the working-copy adoption the daemon asked for (see `WorkerShimAdoption`). */
  adopt(adoption: WorkerShimAdoption): Promise<WorkerShimAdoptionResult>;
  /** Registers the SIGTERM handler (`process.once("SIGTERM", …)`): the shim is the pod's PID 1,
   * so a kubelet termination -- a drain, an eviction, a `kubectl delete` while the stream is not
   * registered -- reaches it here and nowhere else. Optional for `--socket` mode's callers and
   * tests; without it a signal takes the process down as it always did. */
  onTerminate?(handler: () => void): void;
  /** The shim's own environment (`process.env`): where the runtime's
   * `LEGION_TERMINATION_GRACE_SECONDS` arrives. Absent (tests): the default stdin grace. */
  env?: NodeJS.ProcessEnv;
  log(line: string): void;
}

export type WorkerShimTarget =
  | { mode: "socket"; socketPath: string }
  | {
      mode: "connect";
      endpoint: WorkerShimEndpoint;
      bootToken: string;
      providerEnv: Record<string, string>;
    };

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

/** The daemon's `adopt-working-copy` frame, or `undefined` for any other line. A frame of that
 * type whose fields are malformed is an error naming it: the daemon is the only sender. */
function parseAdoptionFrame(line: string): (WorkerShimAdoption & { id: string }) | undefined {
  const parsed = safeParseJsonLine(line);
  if (typeof parsed !== "object" || parsed === null) return undefined;
  const frame = parsed as Record<string, unknown>;
  if (frame.type !== "adopt-working-copy") return undefined;
  const { id, jjUser, jjEmail, timeoutMs } = frame;
  if (
    typeof id !== "string" ||
    typeof jjUser !== "string" ||
    typeof jjEmail !== "string" ||
    typeof timeoutMs !== "number" ||
    !Number.isInteger(timeoutMs) ||
    timeoutMs <= 0
  ) {
    throw new Error(`worker-shim: malformed adopt-working-copy frame: ${line.slice(0, 200)}`);
  }
  return { id, jjUser, jjEmail, timeoutMs };
}

/** The transport-independent half of the shim, shared by `--socket` and `--connect`: summarizes
 * and forwards the wrapped process's stdout frames to the connected daemon (or the bounded
 * backlog while none is connected, replayed in order on the next connect), and forwards daemon
 * frames to the process's stdin — except the two shim frames: `shutdown`, which closes that
 * stdin instead, and `adopt-working-copy`, which the shim answers itself (`adopt`) with an
 * `adopt-working-copy-result` carrying the request's `id`, over the same stream the process's
 * own frames take (so an answer to a daemon that dropped meanwhile waits in the backlog). */
interface ShimBridge {
  /** Starts pumping `child`'s stdout. Called once, when the child is spawned, before any daemon
   * frame can reach `onLine`. */
  attach(child: WorkerShimSpawnedProcess): void;
  onConnect(client: WorkerShimSocketHandle): void;
  onDisconnect(): void;
  onLine(line: string): void;
  /** The shim's own SIGTERM: ends the wrapped process the way the daemon's `shutdown` frame
   * does (its stdin closed, so OMP finishes its turn and exits), then, if it is still running
   * after the stdin grace, SIGTERMs it. The shim itself exits with the child, as
   * always. A second call is a no-op. Returns false, having done nothing, when no child exists
   * yet: the caller decides how the shim itself exits then. */
  terminate(): boolean;
}

function createShimBridge(
  log: (line: string) => void,
  adopt: (adoption: WorkerShimAdoption) => Promise<WorkerShimAdoptionResult>,
  sleep: (ms: number) => Promise<void>,
  stdinGraceMs: number
): ShimBridge {
  let terminating = false;
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
      // The same reader the socket sides use: a UTF-8 character split across two stdout reads
      // is reassembled, and an empty line never spends a backlog slot.
      const reader = createLineReader(forwardToSocket);
      void (async () => {
        for await (const chunk of spawned.stdout as unknown as AsyncIterable<Uint8Array>) {
          reader.push(chunk);
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
      const adoption = parseAdoptionFrame(line);
      if (adoption) {
        const { id, ...request } = adoption;
        void adopt(request).then(
          (result) =>
            forwardToSocket(JSON.stringify({ type: "adopt-working-copy-result", id, ...result })),
          (error) =>
            forwardToSocket(
              JSON.stringify({
                type: "adopt-working-copy-result",
                id,
                ok: false,
                error: error instanceof Error ? error.message : String(error),
              })
            )
        );
        return;
      }
      child.stdin.write(new TextEncoder().encode(`${line}\n`));
    },
    terminate() {
      if (!child) return false;
      if (terminating) return true;
      terminating = true;
      const spawned = child;
      log("[worker-shim] SIGTERM: closing the wrapped process's stdin");
      spawned.stdin.end();
      let exitedFirst = false;
      void spawned.exited.then(() => {
        exitedFirst = true;
      });
      void sleep(stdinGraceMs).then(() => {
        if (exitedFirst) return;
        log(
          `[worker-shim] wrapped process still running ${stdinGraceMs} ms after its stdin closed; sending SIGTERM`
        );
        spawned.kill();
      });
      return true;
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
  const bridge = createShimBridge(
    deps.log,
    deps.adopt,
    (ms) => Bun.sleep(ms),
    DEFAULT_TERMINATE_STDIN_GRACE_MS
  );
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
  deps: WorkerShimConnectDeps,
  childEnv?: Record<string, string>
): Promise<number> {
  if (argv.length === 0) {
    throw new CliError(
      "Usage: legion worker-shim --connect tcp://<host>:<port> --boot-token-file <path> -- <omp argv…>"
    );
  }
  const bridge = createShimBridge(
    deps.log,
    deps.adopt,
    deps.sleep,
    terminateStdinGraceMs(deps.env ?? {})
  );
  const helloLine = JSON.stringify({ type: "hello", bootToken });
  const exit = Promise.withResolvers<number>();
  let exited = false;
  let child: WorkerShimSpawnedProcess | undefined;
  let failures = 0;
  deps.onTerminate?.(() => {
    if (bridge.terminate()) return;
    // No wrapped process yet (still dialing, or the daemon never acked): nothing to wind down,
    // so the shim exits as an unhandled SIGTERM would have, 128 + 15.
    deps.log("[worker-shim] SIGTERM before the wrapped process was spawned; exiting");
    exited = true;
    exit.resolve(143);
  });

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
            child = deps.spawn(argv, childEnv);
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

/** `NAME=trimmed contents` for every regular file in `dir` (symlinks followed — a Kubernetes
 * Secret mount is `KEY -> ..data/KEY` plus `..data`/`..<timestamp>` directories, which are
 * skipped as directories, never read as files) — except a `NAME` the pod already consumes through
 * a `NAME_FILE` pointer in `pointerEnv` (the shim's own environment, where the runtime set
 * `DISPATCH_TOKEN_FILE` to the mounted `DISPATCH_TOKEN` file): that value is read from the file
 * by its consumer and must not also sit in the OMP child's environment, where every tool the agent
 * runs would inherit it. Never touches `process.env`. Throws a CliError naming the flag and path
 * when the directory is missing or unreadable; an empty directory yields `{}`. */
export function readProviderEnvDir(
  dir: string,
  fsOps: {
    readdirSync: typeof fs.readdirSync;
    statSync: typeof fs.statSync;
    readFileSync: typeof fs.readFileSync;
  } = fs,
  pointerEnv: NodeJS.ProcessEnv = process.env
): Record<string, string> {
  let names: string[];
  try {
    names = fsOps.readdirSync(dir) as string[];
  } catch (error) {
    throw new CliError(
      `legion worker-shim: --provider-env-dir ${dir} is unreadable: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const env: Record<string, string> = {};
  for (const name of names) {
    const entryPath = path.join(dir, name);
    if (!fsOps.statSync(entryPath).isFile()) continue;
    if (pointerEnv[`${name}_FILE`] !== undefined) continue;
    env[name] = fsOps.readFileSync(entryPath, "utf8").trim();
  }
  return env;
}

/** Turns the `worker-shim` flags into a target, or throws the CLI error the operator sees: the
 * two modes are mutually exclusive, `--connect` needs a readable non-blank `--boot-token-file`,
 * and every failure names the flag (and path) involved. Nothing is spawned before this succeeds. */
export function resolveWorkerShimTarget(
  flags: { socket?: string; connect?: string; bootTokenFile?: string; providerEnvDir?: string },
  readFile: (path: string) => string = (path) => fs.readFileSync(path, "utf8"),
  readEnvDir: typeof readProviderEnvDir = readProviderEnvDir
): WorkerShimTarget {
  const { socket, connect, bootTokenFile, providerEnvDir } = flags;
  if (socket !== undefined && connect !== undefined) {
    throw new CliError(
      "legion worker-shim: --socket and --connect are mutually exclusive; pass exactly one"
    );
  }
  if (socket !== undefined) {
    if (bootTokenFile !== undefined) {
      throw new CliError("legion worker-shim: --boot-token-file is only valid with --connect");
    }
    if (providerEnvDir !== undefined) {
      throw new CliError("legion worker-shim: --provider-env-dir is only valid with --connect");
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
  const providerEnv = providerEnvDir === undefined ? {} : readEnvDir(providerEnvDir);
  return { mode: "connect", endpoint, bootToken: token, providerEnv };
}

export function defaultWorkerShimDeps(): WorkerShimDeps & WorkerShimConnectDeps {
  return {
    spawn: (argv, env) => {
      const proc = Bun.spawn(argv, {
        stdin: "pipe",
        stdout: "pipe",
        stderr: "inherit",
        env: { ...process.env, ...env },
      });
      return {
        stdin: proc.stdin,
        stdout: proc.stdout as unknown as ReadableStream<Uint8Array>,
        exited: proc.exited,
        kill: () => proc.kill("SIGTERM"),
      };
    },
    listen: (socketPath, handlers) => {
      fs.rmSync(socketPath, { force: true });
      const reader = createLineReader(handlers.onLine);
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
            reader.push(data);
          },
          close(socket) {
            writers.get(socket)?.clear();
            writers.delete(socket);
            reader.reset();
            handlers.onDisconnect();
          },
        },
      });
      return { stop: () => server.stop(true) };
    },
    connect: async (endpoint, handlers) => {
      const reader = createLineReader(handlers.onLine);
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
            reader.push(data);
          },
          close() {
            writer?.clear();
            reader.reset();
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
    adopt: (adoption) => runWorkingCopyAdoption(adoption, process.env),
    onTerminate: (handler) => {
      process.once("SIGTERM", handler);
    },
    env: process.env,
    log: (line) => {
      console.log(line);
    },
  };
}

/** The default `adopt`: runs `adoptWorkingCopyCommand` on the workspace this shim wraps -- the
 * one the runtime exported as `LEGION_WORKSPACE` (a phase worker) or `LEGION_ROOT_WORKSPACE` (a
 * tree root) into the process's environment -- with the shim's own environment (the image's
 * PATH, HOME) beneath the requested `JJ_USER`/`JJ_EMAIL`, under the requested budget. A shim
 * with neither workspace variable was not started by a runtime and refuses. A failed or killed
 * command answers `ok: false` with the runner's report, never a thrown error: the daemon decides
 * what a failed adoption means. */
export async function runWorkingCopyAdoption(
  adoption: WorkerShimAdoption,
  env: NodeJS.ProcessEnv,
  runner: typeof defaultRunner = defaultRunner
): Promise<WorkerShimAdoptionResult> {
  const workspaceDir = env.LEGION_WORKSPACE ?? env.LEGION_ROOT_WORKSPACE;
  if (workspaceDir === undefined) {
    return {
      ok: false,
      error:
        "worker-shim: neither LEGION_WORKSPACE nor LEGION_ROOT_WORKSPACE is set; no workspace to adopt",
    };
  }
  try {
    await runAdoptWorkingCopy(
      processEnvRunner(env, runner),
      workspaceDir,
      { jjUser: adoption.jjUser, jjEmail: adoption.jjEmail },
      adoption.timeoutMs
    );
    return { ok: true };
  } catch (error) {
    return { ok: false, error: error instanceof Error ? error.message : String(error) };
  }
}
