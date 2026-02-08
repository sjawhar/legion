import { readStateFile, writeStateFile } from "./state-file";
import type { SpawnOptions, WorkerEntry } from "./serve-manager";

type Server = ReturnType<typeof Bun.serve>;

export interface ServeManagerInterface {
  spawnServe(opts: SpawnOptions): Promise<WorkerEntry>;
  killWorker(entry: WorkerEntry): Promise<void>;
  healthCheck(port: number, timeoutMs?: number): Promise<boolean>;
}

export interface PortAllocatorInterface {
  allocate(): number;
  release(port: number): void;
  isAllocated?(port: number): boolean;
}

export interface ServerOptions {
  port?: number;
  hostname?: string;
  serveManager: ServeManagerInterface;
  portAllocator: PortAllocatorInterface;
  stateFilePath: string;
}

interface ErrorResponse {
  error: string;
}

const JSON_HEADERS = { "content-type": "application/json" };

function jsonResponse(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), { status, headers: JSON_HEADERS });
}

function badRequest(message: string): Response {
  return jsonResponse({ error: message } satisfies ErrorResponse, 400);
}

function notFound(message = "not_found"): Response {
  return jsonResponse({ error: message } satisfies ErrorResponse, 404);
}

function serverError(message = "server_error"): Response {
  return jsonResponse({ error: message } satisfies ErrorResponse, 500);
}

function badGateway(message = "worker_unreachable"): Response {
  return jsonResponse({ error: message } satisfies ErrorResponse, 502);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

async function parseJson(request: Request): Promise<Record<string, unknown>> {
  try {
    const payload = await request.json();
    if (!isRecord(payload)) {
      throw new Error("invalid_body");
    }
    return payload;
  } catch {
    throw new Error("invalid_json");
  }
}

export function startServer(opts: ServerOptions): { server: Server; stop: () => void } {
  const hostname = opts.hostname ?? "127.0.0.1";
  const port = opts.port ?? 13370;
  const startedAt = Date.now();
  const workers = new Map<string, WorkerEntry>();

  const persistState = async (): Promise<void> => {
    const state: Record<string, WorkerEntry> = {};
    for (const [id, entry] of workers.entries()) {
      state[id] = entry;
    }
    await writeStateFile(opts.stateFilePath, state);
  };

  const loadState = async (): Promise<void> => {
    const state = await readStateFile(opts.stateFilePath);
    const entries = Object.entries(state);
    if (entries.length === 0) {
      return;
    }
    await Promise.all(
      entries.map(async ([id, entry]) => {
        try {
          const healthy = await opts.serveManager.healthCheck(entry.port);
          if (healthy) {
            workers.set(id, { ...entry, status: "running" });
          }
        } catch {
          return;
        }
      })
    );
    await persistState();
  };

  void loadState();

  const server = Bun.serve({
    hostname,
    port,
    async fetch(request: Request): Promise<Response> {
      try {
        const url = new URL(request.url);
        const method = request.method.toUpperCase();
        const segments = url.pathname.split("/").filter(Boolean);

        if (method === "GET" && url.pathname === "/health") {
          return jsonResponse({
            status: "ok",
            uptime: Date.now() - startedAt,
            workerCount: workers.size,
          });
        }

        if (segments.length === 1 && segments[0] === "workers") {
          if (method === "GET") {
            return jsonResponse(Array.from(workers.values()));
          }
          if (method === "POST") {
            let payload: Record<string, unknown>;
            try {
              payload = await parseJson(request);
            } catch {
              return badRequest("invalid_json");
            }

            const issueId = payload.issueId;
            const mode = payload.mode;
            const workspace = payload.workspace;
            const env = payload.env;

            if (typeof issueId !== "string" || typeof mode !== "string" || typeof workspace !== "string") {
              return badRequest("missing_fields");
            }
            if (env !== undefined && !isRecord(env)) {
              return badRequest("invalid_env");
            }

            const port = opts.portAllocator.allocate();
            const sessionId = crypto.randomUUID();
            let entry: WorkerEntry;
            try {
              entry = await opts.serveManager.spawnServe({
                issueId,
                mode,
                workspace,
                port,
                sessionId,
                env: env as Record<string, string> | undefined,
              });
            } catch (error) {
              opts.portAllocator.release(port);
              return serverError((error as Error).message);
            }

            workers.set(entry.id, entry);
            await persistState();

            return jsonResponse({ id: entry.id, port: entry.port, sessionId: entry.sessionId });
          }
        }

        if (segments.length === 2 && segments[0] === "workers") {
          const id = segments[1];
          const entry = workers.get(id);
          if (!entry) {
            return notFound();
          }
          if (method === "GET") {
            return jsonResponse(entry);
          }
          if (method === "DELETE") {
            await opts.serveManager.killWorker(entry);
            opts.portAllocator.release(entry.port);
            workers.delete(id);
            await persistState();
            return jsonResponse({ status: "stopped" });
          }
        }

        if (segments.length === 3 && segments[0] === "workers" && segments[2] === "status") {
          if (method !== "GET") {
            return notFound();
          }
          const id = segments[1];
          const entry = workers.get(id);
          if (!entry) {
            return notFound();
          }

          try {
            const response = await fetch(`http://127.0.0.1:${entry.port}/session/status`);
            if (!response.ok) {
              return badGateway();
            }
            const data = await response.json();
            return jsonResponse(data);
          } catch {
            return badGateway();
          }
        }

        return notFound();
      } catch {
        return serverError();
      }
    },
  });

  return {
    server,
    stop: () => {
      server.stop(true);
    },
  };
}
