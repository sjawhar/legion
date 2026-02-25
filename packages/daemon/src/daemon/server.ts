import { isAbsolute } from "node:path";
import { getBackend, isBackendName } from "../state/backends/index";
import { buildCollectedState } from "../state/decision";
import { enrichParsedIssues } from "../state/fetch";
import {
  CollectedState,
  computeSessionId,
  WorkerMode,
  type WorkerModeLiteral,
} from "../state/types";
import { createWorkerClient, type WorkerEntry } from "./serve-manager";
import {
  type ControllerState,
  type CrashHistoryEntry,
  type PersistedWorkerState,
  readStateFile,
  writeStateFile,
} from "./state-file";

type Server = ReturnType<typeof Bun.serve>;

export interface ServeManagerInterface {
  createSession(port: number, sessionId: string, workspace: string): Promise<string>;
  healthCheck(port: number, timeoutMs?: number): Promise<boolean>;
}

export interface ServerOptions {
  port?: number;
  hostname?: string;
  teamId: string;
  legionDir: string;
  serveManager: ServeManagerInterface;
  sharedServePort: number;
  stateFilePath: string;
  logDir?: string;
  shutdownFn?: () => void | Promise<void>;
  getControllerState?: () => ControllerState | undefined;
}

interface ErrorResponse {
  error: string;
}

const JSON_HEADERS = { "content-type": "application/json" };
const MAX_CRASHES = 3;
const ONE_HOUR_MS = 60 * 60 * 1000;

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
  const crashHistory = new Map<string, CrashHistoryEntry>();

  const persistState = async (): Promise<void> => {
    const state: PersistedWorkerState = { workers: {}, crashHistory: {} };
    for (const [id, entry] of workers.entries()) {
      state.workers[id] = entry;
    }
    for (const [id, history] of crashHistory.entries()) {
      state.crashHistory[id] = history;
    }
    state.controller = opts.getControllerState?.();
    await writeStateFile(opts.stateFilePath, state);
  };

  const loadState = async (): Promise<void> => {
    const state = await readStateFile(opts.stateFilePath);
    for (const [id, history] of Object.entries(state.crashHistory)) {
      crashHistory.set(id.toLowerCase(), history);
    }
    for (const [id, entry] of Object.entries(state.workers)) {
      const normalizedId = id.toLowerCase();
      workers.set(normalizedId, { ...entry, id: normalizedId });
    }
  };

  const stateLoaded = loadState();

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
            await stateLoaded;
            return jsonResponse(Array.from(workers.values()));
          }
          if (method === "POST") {
            await stateLoaded;
            let payload: Record<string, unknown>;
            try {
              payload = await parseJson(request);
            } catch {
              return badRequest("invalid_json");
            }

            const issueId = payload.issueId;
            const mode = payload.mode;
            const workspace = payload.workspace;

            if (
              typeof issueId !== "string" ||
              typeof mode !== "string" ||
              typeof workspace !== "string"
            ) {
              return badRequest("missing_fields");
            }
            if (!isAbsolute(workspace)) {
              return badRequest("workspace must be an absolute path");
            }
            const validModes = Object.values(WorkerMode);
            if (!validModes.includes(mode as WorkerModeLiteral)) {
              return badRequest(`invalid_mode: must be one of ${validModes.join(", ")}`);
            }

            const normalizedIssueId = issueId.toLowerCase();
            const workerId = `${normalizedIssueId}-${mode}`.toLowerCase();
            const existing = workers.get(workerId);
            if (existing && existing.status !== "dead") {
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
            if (existing) {
              workers.delete(workerId);
            }

            let crashHistoryEntry = crashHistory.get(workerId);
            if ((crashHistoryEntry?.crashCount ?? 0) >= MAX_CRASHES) {
              const lastCrashAtMs = crashHistoryEntry?.lastCrashAt
                ? new Date(crashHistoryEntry.lastCrashAt).getTime()
                : 0;
              if (Date.now() - lastCrashAtMs > ONE_HOUR_MS) {
                crashHistory.delete(workerId);
                await persistState();
                crashHistoryEntry = undefined;
              } else {
                return jsonResponse(
                  {
                    error: "crash_limit_exceeded",
                    id: workerId,
                    crashCount: crashHistoryEntry?.crashCount ?? 0,
                    message: "Worker has crashed too many times. Add user-input-needed label.",
                  },
                  429
                );
              }
            }

            const sessionId = computeSessionId(opts.teamId, issueId, mode as WorkerModeLiteral);

            let actualSessionId = sessionId;
            try {
              actualSessionId = await opts.serveManager.createSession(
                opts.sharedServePort,
                sessionId,
                workspace
              );
            } catch (error) {
              return serverError(`Failed to create session: ${(error as Error).message}`);
            }

            const entry: WorkerEntry = {
              id: workerId,
              port: opts.sharedServePort,
              sessionId: actualSessionId,
              workspace,
              startedAt: new Date().toISOString(),
              status: "running",
              crashCount: crashHistoryEntry?.crashCount ?? 0,
              lastCrashAt: crashHistoryEntry?.lastCrashAt ?? null,
            };

            workers.set(entry.id, entry);
            await persistState();

            return jsonResponse({
              id: entry.id,
              port: opts.sharedServePort,
              sessionId: entry.sessionId,
            });
          }
        }

        if (segments.length === 3 && segments[0] === "workers" && segments[2] === "crashes") {
          await stateLoaded;
          if (method !== "DELETE") {
            return notFound();
          }
          const workerId = segments[1].toLowerCase();
          crashHistory.delete(workerId);
          await persistState();
          return jsonResponse({ reset: true, id: workerId });
        }

        if (segments.length === 2 && segments[0] === "workers") {
          await stateLoaded;
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

            const hasCrashCount = "crashCount" in payload;
            const hasLastCrashAt = "lastCrashAt" in payload;
            const crashCount = payload.crashCount;
            const lastCrashAt = payload.lastCrashAt;
            if (hasCrashCount && typeof crashCount !== "number") {
              return badRequest("invalid_crash_count");
            }
            if (hasLastCrashAt && typeof lastCrashAt !== "string" && lastCrashAt !== null) {
              return badRequest("invalid_last_crash_at");
            }

            const updated: WorkerEntry = {
              ...entry,
              status: status as WorkerEntry["status"],
              crashCount: hasCrashCount ? (crashCount as number) : entry.crashCount,
              lastCrashAt: hasLastCrashAt ? (lastCrashAt as string | null) : entry.lastCrashAt,
            };
            workers.set(id, updated);
            if (status === "dead") {
              crashHistory.set(id, {
                crashCount: updated.crashCount,
                lastCrashAt: updated.lastCrashAt,
              });
            }
            await persistState();
            return jsonResponse(updated);
          }
          if (method === "DELETE") {
            crashHistory.set(id, {
              crashCount: entry.crashCount,
              lastCrashAt: entry.lastCrashAt,
            });
            workers.delete(id);
            await persistState();
            return jsonResponse({ status: "stopped" });
          }
        }

        if (segments.length === 3 && segments[0] === "workers" && segments[2] === "status") {
          await stateLoaded;
          if (method !== "GET") {
            return notFound();
          }
          const id = segments[1].toLowerCase();
          const entry = workers.get(id);
          if (!entry) {
            return notFound();
          }

          try {
            // Client is lightweight (fetch wrapper); no caching needed at current
            // polling frequency. Revisit if status endpoint becomes a hot path.
            const client = createWorkerClient(entry.port, entry.workspace);
            const result = await client.session.status();
            if (result.error || !result.data) {
              return badGateway();
            }
            return jsonResponse(result.data);
          } catch {
            return badGateway();
          }
        }

        if (method === "POST" && url.pathname === "/state/collect") {
          let payload: Record<string, unknown>;
          try {
            payload = await parseJson(request);
          } catch {
            return badRequest("invalid_json");
          }

          const backend = payload.backend;
          if (!isBackendName(backend)) {
            return badRequest("invalid_backend");
          }

          const issues = payload.issues;
          if (issues === undefined || issues === null || typeof issues !== "object") {
            return badRequest("invalid_issues");
          }

          try {
            const tracker = getBackend(backend);
            const parsed = tracker.parseIssues(issues);
            const daemonUrl = `http://127.0.0.1:${server.port}`;
            const issuesData = await enrichParsedIssues(parsed, daemonUrl);
            const state = buildCollectedState(issuesData, opts.teamId);
            return jsonResponse(CollectedState.toDict(state));
          } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            console.error(`[collect] backend=${backend} error=${message}`);
            return serverError("collect_failed");
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
