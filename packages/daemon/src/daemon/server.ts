import { isAbsolute } from "node:path";
import type { CodebaseIndexResponse } from "../index/types";
import { getBackend, isBackendName } from "../state/backends/index";
import { buildCollectedState, canDispatchMode } from "../state/decision";
import { enrichParsedIssues } from "../state/fetch";
import { fetchGitHubProjectItems } from "../state/github-fetch";
import {
  CollectedState,
  computeSessionId,
  type IssueState,
  WorkerMode,
  type WorkerModeLiteral,
} from "../state/types";
import type { FeedbackLogger } from "./feedback";
import type { TokenManager } from "./github-apps";
import { modeToRole } from "./github-apps";
import type { LegionPaths } from "./paths";
import {
  cleanupWorkspace,
  ensureWorkspace,
  parseIssueRepo,
  type RepoManagerDeps,
} from "./repo-manager";
import type { RuntimeAdapter } from "./runtime/types";
import type { WorkerEntry as BaseWorkerEntry } from "./serve-manager";
import {
  type ControllerState,
  type CrashHistoryEntry,
  type PersistedWorkerState,
  readStateFile,
  writeStateFile,
} from "./state-file";
import { registerGauges } from "./telemetry";

type Server = ReturnType<typeof Bun.serve>;

interface WorkerEntry extends BaseWorkerEntry {
  env?: Record<string, string>;
  version?: number;
}
export interface ServerOptions {
  port?: number;
  hostname?: string;
  legionId: string;
  projectId?: string;
  legionDir?: string;
  paths?: LegionPaths;
  adapter: RuntimeAdapter;
  repoManagerDeps?: RepoManagerDeps;
  stateFilePath: string;
  logDir?: string;
  shutdownFn?: () => void | Promise<void>;
  getControllerState?: () => ControllerState | undefined;
  runtime?: string;
  tmuxSession?: string;
  getWorkerAdapter?: (mode: WorkerModeLiteral) => RuntimeAdapter;
  tokenManager?: TokenManager;
  indexManager?: {
    getResponse: () => CodebaseIndexResponse;
    rebuild: () => Promise<CodebaseIndexResponse>;
  };
  feedbackLogger?: FeedbackLogger;
}

interface ErrorResponse {
  error: string;
}

const JSON_HEADERS = { "content-type": "application/json" };
const MAX_CRASHES = 3;
const ONE_HOUR_MS = 60 * 60 * 1000;

/**
 * Delay between session creation and prompt delivery to allow the serve process
 * to bootstrap the Instance context for the new session's directory.
 * See: https://github.com/sjawhar/legion/issues/237
 */
export const SESSION_READY_DELAY_MS = 2000;

const PROMPT_RETRY_ATTEMPTS = 3;
const PROMPT_RETRY_BASE_MS = 100;

function jsonResponse(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: JSON_HEADERS,
  });
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

function extractIssueIdFromWorkerId(workerId: string): string | null {
  for (const mode of Object.values(WorkerMode)) {
    const suffix = `-${mode}`;
    if (workerId.endsWith(suffix)) {
      return workerId.slice(0, -suffix.length);
    }
  }
  return null;
}

function extractModeFromWorkerId(workerId: string): WorkerModeLiteral | null {
  for (const mode of Object.values(WorkerMode)) {
    if (workerId.endsWith(`-${mode}`)) {
      return mode;
    }
  }
  return null;
}

function buildIssueEnvoyTopics(owner: string, repo: string, issueNumber: number): string[] {
  return [
    `notifications.github.${owner}.${repo}.issue.${issueNumber}.>`,
    `notifications.github.${owner}.${repo}.pr.${issueNumber}.>`,
  ];
}

export function subscribeWorkerToEnvoy(sessionId: string, topics: string[]): void {
  if (topics.length === 0) return;
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  fetch(`${envoyUrl}/v1/interests/subscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      session_id: sessionId,
      topics,
    }),
  })
    .then((res) => {
      if (!res.ok) {
        console.warn(
          `Envoy worker subscribe returned ${res.status} for session=${sessionId} (non-fatal)`
        );
      }
    })
    .catch((err) => {
      console.warn(`Envoy worker subscribe failed for session=${sessionId} (non-fatal): ${err}`);
    });
}

function detachWorkerFromEnvoy(entry: BaseWorkerEntry, reason: string): void {
  const hadTopics = entry.envoyTopics;
  entry.envoyTopics = undefined;
  if (!hadTopics?.length) return;
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  fetch(`${envoyUrl}/v1/interests/unsubscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_id: entry.sessionId, topics: [] }),
  })
    .then((res) => {
      if (!res.ok) {
        console.warn(
          `Envoy worker unsubscribe (${reason}) returned ${res.status} for session=${entry.sessionId} (non-fatal)`
        );
      }
    })
    .catch((err) => {
      console.warn(
        `Envoy worker unsubscribe (${reason}) failed for session=${entry.sessionId} (non-fatal): ${err}`
      );
    });
}

function unsubscribeAllWorkerTopics(sessionId: string): void {
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  fetch(`${envoyUrl}/v1/interests/unsubscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_id: sessionId, topics: [] }),
  })
    .then((res) => {
      if (!res.ok) {
        console.warn(
          `Envoy worker unsubscribe returned ${res.status} for session=${sessionId} (non-fatal)`
        );
      }
    })
    .catch((err) => {
      console.warn(`Envoy worker unsubscribe failed for session=${sessionId} (non-fatal): ${err}`);
    });
}

export function subscribeControllerToCiEnvoy(sessionId: string, owner: string, repo: string): void {
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  const topic = `notifications.github.${owner}.${repo}.ci`;
  fetch(`${envoyUrl}/v1/interests/subscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      session_id: sessionId,
      topics: [topic],
    }),
  })
    .then((res) => {
      if (!res.ok) {
        console.warn(
          `Envoy CI subscribe returned ${res.status} for session=${sessionId} repo=${owner}/${repo} (non-fatal)`
        );
      }
    })
    .catch((err) => {
      console.warn(
        `Envoy CI subscribe failed for session=${sessionId} repo=${owner}/${repo} (non-fatal): ${err}`
      );
    });
}

export function startServer(opts: ServerOptions): {
  server: Server;
  stop: () => void;
} {
  const hostname = opts.hostname ?? "127.0.0.1";
  const port = opts.port ?? 13370;
  const startedAt = Date.now();
  const workers = new Map<string, WorkerEntry>();
  const crashHistory = new Map<string, CrashHistoryEntry>();
  const issueStateCache = new Map<string, IssueState>();
  const subscribedCiRepos = new Set<string>();
  const releaseGauges = registerGauges("daemon-server", () => {
    let starting = 0;
    let running = 0;
    let stopped = 0;
    let dead = 0;
    for (const entry of workers.values()) {
      if (entry.status === "starting") starting += 1;
      if (entry.status === "running") running += 1;
      if (entry.status === "stopped") stopped += 1;
      if (entry.status === "dead") dead += 1;
    }
    return {
      daemon_workers: workers.size,
      daemon_workers_starting: starting,
      daemon_workers_running: running,
      daemon_workers_stopped: stopped,
      daemon_workers_dead: dead,
      daemon_crash_entries: crashHistory.size,
      daemon_controller_present: opts.getControllerState?.() ? 1 : 0,
      daemon_uptime_s: Math.floor((Date.now() - startedAt) / 1000),
    };
  });

  let pendingWrite: Promise<void> = Promise.resolve();

  const persistState = async (): Promise<void> => {
    const doWrite = async () => {
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

    pendingWrite = pendingWrite.then(doWrite, doWrite);
    await pendingWrite;
  };

  const loadState = async (): Promise<void> => {
    const state = await readStateFile(opts.stateFilePath);
    for (const [id, history] of Object.entries(state.crashHistory)) {
      crashHistory.set(id.toLowerCase(), history);
    }
    for (const [id, entry] of Object.entries(state.workers)) {
      const normalizedId = id.toLowerCase();
      const loadedEntry: WorkerEntry = { ...entry, id: normalizedId };
      workers.set(normalizedId, loadedEntry);
      // Seed CI repo tracking from persisted workers
      if (entry.repo) {
        subscribedCiRepos.add(entry.repo);
      }

      if (loadedEntry.status === "dead") {
        continue;
      }

      const recoveredTopics = (() => {
        if (loadedEntry.envoyTopics?.length) {
          return loadedEntry.envoyTopics;
        }
        if (loadedEntry.repo && loadedEntry.issueNumber !== undefined) {
          const repoRef = parseIssueRepo(loadedEntry.repo);
          if (repoRef) {
            return buildIssueEnvoyTopics(repoRef.owner, repoRef.repo, loadedEntry.issueNumber);
          }
        }
        return undefined;
      })();

      if (recoveredTopics?.length) {
        loadedEntry.envoyTopics = recoveredTopics;
        subscribeWorkerToEnvoy(loadedEntry.sessionId, recoveredTopics);
      }
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
            runtime: opts.runtime ?? "opencode",
            ...(opts.tmuxSession ? { tmuxSession: opts.tmuxSession } : {}),
          });
        }

        if (url.pathname === "/index") {
          if (method === "GET") {
            return jsonResponse(
              opts.indexManager?.getResponse() ?? {
                version: 1,
                dependencyGraph: {},
                apiSurface: {},
                testMapping: {
                  sourceToTests: {},
                  testToSources: {},
                },
                hotspots: [],
                metadata: {},
              }
            );
          }

          if (method === "POST") {
            if (!opts.indexManager) {
              return serverError("index_manager_unavailable");
            }
            return jsonResponse(await opts.indexManager.rebuild());
          }
        }

        if (method === "POST" && url.pathname === "/index/rebuild") {
          if (!opts.indexManager) {
            return serverError("index_manager_unavailable");
          }
          return jsonResponse(await opts.indexManager.rebuild());
        }

        if (segments.length === 1 && segments[0] === "workers") {
          if (method === "GET") {
            await stateLoaded;
            const safeWorkers = Array.from(workers.values()).map(({ env: _env, ...rest }) => rest);
            return jsonResponse(safeWorkers);
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
            const repo = payload.repo;
            const workspace = payload.workspace;
            const version = typeof payload.version === "number" ? payload.version : 0;
            const envPayload = payload.env;
            const issueNumber =
              typeof payload.issueNumber === "number" ? payload.issueNumber : undefined;

            const prompt = payload.prompt;
            if (typeof issueId !== "string" || typeof mode !== "string") {
              return badRequest("missing_fields");
            }
            if (typeof repo === "string" && typeof workspace === "string") {
              return badRequest(
                "repo and workspace are mutually exclusive — provide one or neither"
              );
            }
            if (typeof repo !== "string" && typeof workspace !== "string") {
              return badRequest("missing repo or workspace");
            }
            if (
              version !== undefined &&
              (typeof version !== "number" ||
                !Number.isInteger(version) ||
                !Number.isSafeInteger(version) ||
                version < 0)
            ) {
              return badRequest("version must be a non-negative integer");
            }
            const validModes = Object.values(WorkerMode);
            if (!validModes.includes(mode as WorkerModeLiteral)) {
              return badRequest(`invalid_mode: must be one of ${validModes.join(", ")}`);
            }

            // Phase prerequisite validation for gated modes
            const normalizedIssueId = issueId.toLowerCase();
            const forceDispatch = payload.force === true;
            if (!forceDispatch) {
              const cachedState = issueStateCache.get(normalizedIssueId);
              const validation = canDispatchMode(cachedState, mode as WorkerModeLiteral);
              if (!validation.valid) {
                return jsonResponse(
                  {
                    error: "phase_prerequisite_unmet",
                    attemptedMode: mode,
                    suggestedAction: validation.suggestedAction,
                    reason: validation.reason,
                  },
                  422
                );
              }
            } else {
              console.warn(
                `[dispatch] force=true for ${normalizedIssueId} mode=${mode} — skipping phase validation`
              );
            }

            let resolvedWorkspace: string | null = null;
            const repoRef = typeof repo === "string" ? parseIssueRepo(repo) : null;
            if (typeof repo === "string") {
              if (!opts.paths) {
                return badRequest("repo_resolution_unavailable");
              }
              if (!repoRef) {
                return badRequest("invalid_repo: expected owner/repo");
              }
              try {
                resolvedWorkspace = await ensureWorkspace(
                  opts.paths,
                  opts.legionId,
                  issueId,
                  repoRef,
                  opts.repoManagerDeps
                );
              } catch (error) {
                return serverError(`Failed to resolve workspace: ${(error as Error).message}`);
              }
            } else if (typeof workspace === "string") {
              if (!isAbsolute(workspace)) {
                return badRequest("workspace must be an absolute path");
              }
              resolvedWorkspace = workspace;
            }
            if (!resolvedWorkspace) {
              return badRequest("missing repo or workspace");
            }

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

            const sessionId = computeSessionId(
              opts.legionId,
              issueId,
              mode as WorkerModeLiteral,
              version
            );

            const workerAdapter =
              opts.getWorkerAdapter?.(mode as WorkerModeLiteral) ?? opts.adapter;

            // Auto-inject role credentials when GitHub Apps configured
            let workerEnv: Record<string, string> | undefined = isRecord(envPayload)
              ? (envPayload as Record<string, string>)
              : undefined;
            if (workerEnv) {
              for (const [k, v] of Object.entries(workerEnv)) {
                if (typeof v !== "string") {
                  return badRequest(`env values must be strings (key "${k}")`);
                }
              }
            }
            if (opts.tokenManager) {
              try {
                const role = modeToRole(mode);
                if (opts.tokenManager.isConfigured(role)) {
                  const cred = await opts.tokenManager.getToken(role);
                  workerEnv = {
                    ...workerEnv,
                    GH_TOKEN: cred.token,
                    GIT_AUTHOR_NAME: cred.gitIdentity.name,
                    GIT_AUTHOR_EMAIL: cred.gitIdentity.email,
                    GIT_COMMITTER_NAME: cred.gitIdentity.name,
                    GIT_COMMITTER_EMAIL: cred.gitIdentity.email,
                    LEGION_APP_ROLE: role,
                  };
                }
              } catch (error) {
                console.error(
                  `Failed to inject role credentials for ${mode}: ${(error as Error).message}`
                );
              }
            }

            let actualSessionId = sessionId;
            try {
              actualSessionId = await workerAdapter.createSession(sessionId, resolvedWorkspace);
            } catch (error) {
              return serverError(`Failed to create session: ${(error as Error).message}`);
            }

            const entry: WorkerEntry = {
              id: workerId,
              port: workerAdapter.getPort(),
              sessionId: actualSessionId,
              workspace: resolvedWorkspace,
              startedAt: new Date().toISOString(),
              status: "running",
              crashCount: crashHistoryEntry?.crashCount ?? 0,
              lastCrashAt: crashHistoryEntry?.lastCrashAt ?? null,
              version,
              ...(typeof repo === "string" ? { repo } : {}),
              ...(issueNumber !== undefined ? { issueNumber } : {}),
              ...(workerEnv ? { env: workerEnv } : {}),
            };

            workers.set(entry.id, entry);
            await persistState();

            // Cross-mode cleanup: unsubscribe same-issue workers from Envoy
            let workerEntriesChanged = false;
            for (const [existingId, existingEntry] of workers) {
              if (existingId === workerId) continue;
              const existingIssueId = extractIssueIdFromWorkerId(existingId);
              if (existingIssueId === normalizedIssueId) {
                if (existingEntry.envoyTopics?.length) {
                  workerEntriesChanged = true;
                }
                detachWorkerFromEnvoy(existingEntry, "cross-mode-cleanup");
              }
            }

            // Mode-based Envoy subscription (GitHub-only, fire-and-forget)
            // Only plan mode subscribes to issue topics at dispatch.
            // Implement self-subscribes to PR topics after PR creation via envoy_subscribe.
            if (mode === WorkerMode.PLAN && repoRef && issueNumber !== undefined) {
              const topics = buildIssueEnvoyTopics(repoRef.owner, repoRef.repo, issueNumber);
              subscribeWorkerToEnvoy(actualSessionId, topics);
              entry.envoyTopics = topics;
              workerEntriesChanged = true;
            }

            if (repoRef) {
              const repoKey = `${repoRef.owner}/${repoRef.repo}`;
              if (!subscribedCiRepos.has(repoKey)) {
                subscribedCiRepos.add(repoKey);
                const controllerSessionId = opts.getControllerState?.()?.sessionId;
                if (controllerSessionId) {
                  subscribeControllerToCiEnvoy(controllerSessionId, repoRef.owner, repoRef.repo);
                }
              }
            }

            if (workerEntriesChanged) {
              await persistState();
            }

            // Deliver initial prompt with delay for session bootstrap (#237)
            let promptDelivered: boolean | undefined;
            if (typeof prompt === "string" && prompt.length > 0) {
              try {
                await new Promise((resolve) => setTimeout(resolve, SESSION_READY_DELAY_MS));
                let lastError: Error = new Error("All prompt retry attempts failed");
                for (let attempt = 0; attempt < PROMPT_RETRY_ATTEMPTS; attempt++) {
                  try {
                    await workerAdapter.sendPrompt(actualSessionId, prompt);
                    promptDelivered = true;
                    break;
                  } catch (error) {
                    lastError = error instanceof Error ? error : new Error(String(error));
                    if (attempt < PROMPT_RETRY_ATTEMPTS - 1) {
                      await new Promise((resolve) =>
                        setTimeout(resolve, PROMPT_RETRY_BASE_MS * 2 ** attempt)
                      );
                    }
                  }
                }
                if (promptDelivered !== true) {
                  throw lastError;
                }
              } catch (error) {
                console.error(
                  `[dispatch] Failed to deliver prompt to ${workerId}: ${(error as Error).message}`
                );
                promptDelivered = false;
              }
            }
            opts.feedbackLogger?.log({
              event: "worker.dispatched",
              issueId: normalizedIssueId,
              mode: mode as string,
              workerId: entry.id,
              sessionId: entry.sessionId,
              version,
              workspace: resolvedWorkspace,
              crashCount: entry.crashCount,
            });

            return jsonResponse({
              id: entry.id,
              port: workerAdapter.getPort(),
              sessionId: entry.sessionId,
              ...(promptDelivered !== undefined ? { promptDelivered } : {}),
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

        if (segments.length === 3 && segments[0] === "workers" && segments[2] === "workspace") {
          await stateLoaded;
          if (method !== "DELETE") {
            return notFound();
          }
          const workerId = segments[1].toLowerCase();
          const entry = workers.get(workerId);
          if (!entry) {
            return notFound();
          }
          if (!opts.paths) {
            return badRequest("workspace_cleanup_unavailable");
          }

          let payload: Record<string, unknown>;
          try {
            payload = await parseJson(request);
          } catch {
            return badRequest("invalid_json");
          }

          const repo = payload.repo;
          if (typeof repo !== "string") {
            return badRequest("missing_fields");
          }
          const repoRef = parseIssueRepo(repo);
          if (!repoRef) {
            return badRequest("invalid_repo: expected owner/repo");
          }
          const issueId = extractIssueIdFromWorkerId(entry.id);
          if (!issueId) {
            return badRequest("invalid_worker_id");
          }

          try {
            await cleanupWorkspace(
              opts.paths,
              opts.legionId,
              issueId,
              repoRef,
              opts.repoManagerDeps
            );
          } catch (error) {
            return serverError(`Failed to cleanup workspace: ${(error as Error).message}`);
          }

          // Also remove the worker entry — workspace deletion means the issue is done
          workers.delete(workerId);
          crashHistory.delete(workerId);
          await persistState();
          unsubscribeWorkerFromEnvoy(entry.sessionId);

          return jsonResponse({ status: "cleaned", workerRemoved: true });
        }

        if (segments.length === 3 && segments[0] === "workers" && segments[2] === "env") {
          await stateLoaded;
          if (method !== "GET") {
            return notFound();
          }
          const id = segments[1].toLowerCase();
          const entry = workers.get(id);
          if (!entry) {
            return notFound();
          }
          const safeEnv: Record<string, string> = {};
          for (const [k, v] of Object.entries(entry.env ?? {})) {
            if (
              k !== "GH_TOKEN" &&
              !k.startsWith("GIT_AUTHOR_") &&
              !k.startsWith("GIT_COMMITTER_") &&
              k !== "LEGION_APP_ROLE"
            ) {
              safeEnv[k] = v;
            }
          }
          return jsonResponse({ env: safeEnv });
        }

        if (segments.length === 2 && segments[0] === "workers" && segments[1] === "prune") {
          await stateLoaded;
          if (method !== "POST") {
            return notFound();
          }
          let payload: Record<string, unknown>;
          try {
            payload = await parseJson(request);
          } catch {
            return badRequest("invalid_json");
          }

          const issueIds = payload.issueIds;
          if (!Array.isArray(issueIds) || !issueIds.every((id) => typeof id === "string")) {
            return badRequest("issueIds must be an array of strings");
          }

          const normalizedIssueIds = new Set(issueIds.map((id: string) => id.toLowerCase()));
          const prunedWorkers: string[] = [];
          const prunedCrashHistory: string[] = [];
          const prunedSessionIds: string[] = [];

          for (const [workerId, entry] of workers.entries()) {
            const workerIssueId = extractIssueIdFromWorkerId(workerId);
            if (workerIssueId && normalizedIssueIds.has(workerIssueId.toLowerCase())) {
              prunedWorkers.push(workerId);
              prunedSessionIds.push(entry.sessionId);
            }
          }
          for (const id of prunedWorkers) {
            workers.delete(id);
          }

          for (const crashId of [...crashHistory.keys()]) {
            const crashIssueId = extractIssueIdFromWorkerId(crashId);
            if (crashIssueId && normalizedIssueIds.has(crashIssueId.toLowerCase())) {
              prunedCrashHistory.push(crashId);
              crashHistory.delete(crashId);
            }
          }

          for (const issueId of normalizedIssueIds) {
            issueStateCache.delete(issueId);
          }

          if (prunedWorkers.length > 0 || prunedCrashHistory.length > 0) {
            await persistState();
          }

          for (const sessionId of prunedSessionIds) {
            unsubscribeWorkerFromEnvoy(sessionId);
          }

          return jsonResponse({
            pruned: prunedWorkers,
            crashHistoryPruned: prunedCrashHistory,
          });
        }

        if (segments.length === 2 && segments[0] === "workers") {
          await stateLoaded;
          const id = segments[1].toLowerCase();
          const entry = workers.get(id);
          if (!entry) {
            return notFound();
          }
          if (method === "GET") {
            const { env: _getEnv, ...safeEntry } = entry;
            return jsonResponse(safeEntry);
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
              // Clean up Envoy subscriptions on transition to dead (fire-and-forget)
              if (entry.status !== "dead") {
                detachWorkerFromEnvoy(updated, "worker-dead");
              }
            }
            await persistState();

            opts.feedbackLogger?.log({
              event: "worker.status_changed",
              workerId: id,
              issueId: extractIssueIdFromWorkerId(id) ?? id,
              mode: extractModeFromWorkerId(id) ?? "unknown",
              sessionId: updated.sessionId,
              version: updated.version ?? 0,
              fromStatus: entry.status,
              toStatus: updated.status,
              crashCount: updated.crashCount,
              uptimeMs: updated.startedAt
                ? Date.now() - new Date(updated.startedAt).getTime()
                : null,
            });
            const { env: _patchEnv, ...safeUpdated } = updated;
            return jsonResponse(safeUpdated);
          }
          if (method === "DELETE") {
            crashHistory.set(id, {
              crashCount: entry.crashCount,
              lastCrashAt: entry.lastCrashAt,
            });
            entry.envoyTopics = undefined;
            workers.delete(id);
            await persistState();
            unsubscribeAllWorkerTopics(entry.sessionId);
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
            const statusMode = extractModeFromWorkerId(entry.id);
            const statusAdapter = statusMode
              ? (opts.getWorkerAdapter?.(statusMode) ?? opts.adapter)
              : opts.adapter;
            const result = await statusAdapter.getSessionStatus(entry.sessionId);
            if (result.error || !result.data) {
              return badGateway();
            }
            return jsonResponse(result.data);
          } catch {
            return badGateway();
          }
        }

        if (segments.length === 3 && segments[0] === "workers" && segments[2] === "prompt") {
          await stateLoaded;
          if (method !== "POST") {
            return notFound();
          }
          const id = segments[1].toLowerCase();
          const entry = workers.get(id);
          if (!entry) {
            return notFound();
          }

          let payload: Record<string, unknown>;
          try {
            payload = await parseJson(request);
          } catch {
            return badRequest("invalid_json");
          }
          const text = payload.text;
          if (typeof text !== "string") {
            return badRequest("missing_fields");
          }
          try {
            const promptMode = extractModeFromWorkerId(entry.id);
            const promptAdapter = promptMode
              ? (opts.getWorkerAdapter?.(promptMode) ?? opts.adapter)
              : opts.adapter;
            await promptAdapter.sendPrompt(entry.sessionId, text);

            if (entry.envoyTopics?.length) {
              subscribeWorkerToEnvoy(entry.sessionId, entry.envoyTopics);
            }

            return jsonResponse({ ok: true });
          } catch (error) {
            return serverError(`Failed to send prompt: ${(error as Error).message}`);
          }
        }

        // Credential endpoint removed — credentials are auto-injected in POST /workers

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
            const state = buildCollectedState(issuesData, opts.legionId);

            // Populate dispatch validation cache
            for (const [issueId, issueState] of Object.entries(state.issues)) {
              issueStateCache.set(issueId.toLowerCase(), issueState);
            }

            if (opts.feedbackLogger) {
              for (const [issueId, issueState] of Object.entries(state.issues)) {
                opts.feedbackLogger.log({
                  event: "state.collected",
                  issueId,
                  status: issueState.status,
                  suggestedAction: issueState.suggestedAction,
                  hasLiveWorker: issueState.hasLiveWorker,
                  workerMode: issueState.workerMode,
                  workerStatus: issueState.workerStatus,
                  hasPr: issueState.hasPr,
                  prIsDraft: issueState.prIsDraft,
                  ciStatus: issueState.ciStatus,
                  mergeableStatus: issueState.mergeableStatus,
                  labels: issueState.labels,
                });
              }
            }

            return jsonResponse(CollectedState.toDict(state));
          } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            console.error(`[collect] backend=${backend} error=${message}`);
            return serverError("collect_failed");
          }
        }

        if (method === "POST" && url.pathname === "/state/fetch-and-collect") {
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

          try {
            let rawIssues: unknown;
            if (backend === "github") {
              const legionId = (payload.legionId as string) ?? opts.legionId;
              const parts = legionId.split("/");
              if (parts.length !== 2 || !parts[1]) {
                return badRequest("invalid_team_id: expected owner/project-number");
              }
              const [owner, numStr] = parts;
              const projectNumber = Number(numStr);
              if (!Number.isFinite(projectNumber)) {
                return badRequest("invalid_team_id: project number not a number");
              }
              rawIssues = await fetchGitHubProjectItems(owner, projectNumber);
            } else {
              return badRequest("fetch-and-collect only supports github backend currently");
            }

            const tracker = getBackend(backend);
            const parsed = tracker.parseIssues(rawIssues);
            const daemonUrl = `http://127.0.0.1:${server.port}`;
            const issuesData = await enrichParsedIssues(parsed, daemonUrl);
            const state = buildCollectedState(issuesData, opts.legionId);

            // Populate dispatch validation cache
            for (const [issueId, issueState] of Object.entries(state.issues)) {
              issueStateCache.set(issueId.toLowerCase(), issueState);
            }

            return jsonResponse(CollectedState.toDict(state));
          } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            console.error(`[fetch-and-collect] backend=${backend} error=${message}`);
            return serverError(`fetch_and_collect_failed: ${message}`);
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
      releaseGauges();
      server.stop(true);
    },
  };
}
