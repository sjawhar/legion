import { createOpencodeClient } from "@opencode-ai/sdk/v2";
import {
  computeControllerSessionId,
  computeSessionId,
  WorkerMode,
  type WorkerModeLiteral,
} from "../state/types";
import type { SpawnOptions, WorkerEntry } from "./serve-manager";
import { readStateFile, writeStateFile } from "./state-file";

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
  teamId: string;
  serveManager: ServeManagerInterface;
  portAllocator: PortAllocatorInterface;
  stateFilePath: string;
  shutdownFn?: () => void | Promise<void>;
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
            const normalizedId = id.toLowerCase();
            workers.set(normalizedId, { ...entry, id: normalizedId, status: "running" });
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

            if (
              typeof issueId !== "string" ||
              typeof mode !== "string" ||
              typeof workspace !== "string"
            ) {
              return badRequest("missing_fields");
            }
            const validModes = Object.values(WorkerMode);
            if (!validModes.includes(mode as WorkerModeLiteral)) {
              return badRequest(`invalid_mode: must be one of ${validModes.join(", ")}`);
            }
            if (env !== undefined) {
              if (!isRecord(env)) {
                return badRequest("invalid_env");
              }
              for (const [, val] of Object.entries(env)) {
                if (typeof val !== "string") {
                  return badRequest("env values must be strings");
                }
              }
            }

            const normalizedIssueId = issueId.toLowerCase();
            const workerId = `${normalizedIssueId}-${mode}`.toLowerCase();
            const existing = workers.get(workerId);
            if (existing) {
              return jsonResponse(
                {
                  error: "worker_already_exists",
                  id: workerId,
                  port: existing.port,
                  sessionId: existing.sessionId,
                },
                409
              );
            }

            const port = opts.portAllocator.allocate();
            const sessionId =
              mode === "controller"
                ? computeControllerSessionId(opts.teamId)
                : computeSessionId(opts.teamId, issueId, mode as WorkerModeLiteral);
            let entry: WorkerEntry;
            try {
              entry = await opts.serveManager.spawnServe({
                issueId: normalizedIssueId,
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
          const id = segments[1].toLowerCase();
          const entry = workers.get(id);
          if (!entry) {
            return notFound();
          }
          if (method === "GET") {
            return jsonResponse(entry);
          }
          if (method === "PATCH") {
            let payload: Record<string, unknown>;
            try {
              payload = await parseJson(request);
            } catch {
              return badRequest("invalid_json");
            }

            const status = payload.status;
            if (typeof status !== "string") {
              return badRequest("missing_fields");
            }
            if (!"starting running stopped dead".split(" ").includes(status)) {
              return badRequest("invalid_status");
            }

            const updated = { ...entry, status: status as WorkerEntry["status"] };
            workers.set(id, updated);
            await persistState();
            return jsonResponse(updated);
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
          const id = segments[1].toLowerCase();
          const entry = workers.get(id);
          if (!entry) {
            return notFound();
          }

          try {
            const client = createOpencodeClient({ baseUrl: `http://127.0.0.1:${entry.port}` });
            const result = await client.session.status();
            if (result.error || !result.data) {
              return badGateway();
            }
            return jsonResponse(result.data);
          } catch {
            return badGateway();
          }
        }

        if (method === "POST" && url.pathname === "/shutdown") {
          await opts.shutdownFn?.();
          return jsonResponse({ status: "shutting_down" });
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
