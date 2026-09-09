import fs from "node:fs";
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
  const child = deps.spawn(argv);
  const childStdin = child.stdin;
  let client: WorkerShimSocketHandle | undefined;
  const backlog: string[] = [];
  const BACKLOG_LIMIT = 1000;
  let droppedBacklogFrames = false;

  const forwardToSocket = (line: string): void => {
    const summary = summarizeShimFrame(safeParseJsonLine(line));
    if (summary) deps.log(summary);
    if (client) {
      client.write(line);
      return;
    }
    backlog.push(line);
    if (backlog.length > BACKLOG_LIMIT) {
      backlog.shift();
      if (!droppedBacklogFrames) {
        droppedBacklogFrames = true;
        deps.log(
          `[worker-shim] no daemon client connected; dropping oldest frames beyond the last ${BACKLOG_LIMIT}`
        );
      }
    }
  };

  const listener = deps.listen(socketPath, {
    onConnect: (socket) => {
      client = socket;
      for (const line of backlog.splice(0)) socket.write(line);
    },
    onDisconnect: () => {
      client = undefined;
    },
    onLine: (line) => {
      const parsed = safeParseJsonLine(line);
      if (
        typeof parsed === "object" &&
        parsed !== null &&
        (parsed as Record<string, unknown>).type === "shutdown"
      ) {
        childStdin.end();
        return;
      }
      childStdin.write(new TextEncoder().encode(`${line}\n`));
    },
  });

  void (async () => {
    const decoder = new TextDecoder();
    let buffer = "";
    for await (const chunk of child.stdout as unknown as AsyncIterable<Uint8Array>) {
      buffer += decoder.decode(chunk, { stream: true });
      let index = buffer.indexOf("\n");
      while (index !== -1) {
        forwardToSocket(buffer.slice(0, index));
        buffer = buffer.slice(index + 1);
        index = buffer.indexOf("\n");
      }
    }
  })();

  const exitCode = await child.exited;
  listener.stop();
  return exitCode;
}

function safeParseJsonLine(line: string): unknown {
  try {
    return JSON.parse(line);
  } catch {
    return undefined;
  }
}

export function defaultWorkerShimDeps(): WorkerShimDeps {
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
      const server = Bun.listen({
        unix: socketPath,
        socket: {
          open(socket) {
            handlers.onConnect({
              write: (line) => socket.write(`${line}\n`),
              end: () => socket.end(),
            });
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
            buffer = "";
            handlers.onDisconnect();
          },
        },
      });
      return { stop: () => server.stop(true) };
    },
    log: (line) => {
      console.log(line);
    },
  };
}
