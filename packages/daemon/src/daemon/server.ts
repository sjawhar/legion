import { basename, isAbsolute, join } from "node:path";
import type { CodebaseIndexResponse } from "../index/types";
import { getBackend, isBackendName } from "../state/backends/index";
import type { BackendName } from "../state/backends/issue-tracker";
import { ACTION_TO_MODE, buildCollectedState, canDispatchMode } from "../state/decision";
import { enrichParsedIssues } from "../state/fetch";
import { fetchGitHubProjectItems } from "../state/github-fetch";
import {
  type ActionType,
  CollectedState,
  computeSessionId,
  IssueState,
  type IssueStateDict,
  IssueStatus,
  type IssueStatusLiteral,
  SESSION_ID_PATTERN,
  WorkerMode,
  type WorkerModeLiteral,
} from "../state/types";
import { getDashboardHtml } from "./dashboard-ui";
import type { FeedbackLogger } from "./feedback";
import type { TokenManager } from "./github-apps";
import { modeToRole } from "./github-apps";
import type { LegionPaths } from "./paths";
import {
  demoteSession,
  listPromotedSessions,
  promoteSession,
  readPromotedSessions,
} from "./promoted-sessions";
import {
  cleanupWorkspace,
  defaultDeps as defaultRepoManagerDeps,
  ensureWorkspace,
  parseIssueRepo,
  type RepoManagerDeps,
  type RepoRef,
  removeDir,
  startBackgroundFetch,
  verifyBranchPushed,
} from "./repo-manager";
import type { RuntimeAdapter } from "./runtime/types";
import type { WorkerEntry as BaseWorkerEntry } from "./serve-manager";
import { computeStateDelta, type StateDelta } from "./state-delta";
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
  envoyUrl?: string;
  legionId: string;
  projectId?: string;
  extraProjects?: string[];
  legionDir?: string;
  paths?: LegionPaths;
  adapter: RuntimeAdapter;
  repoManagerDeps?: RepoManagerDeps;
  stateFilePath: string;
  logDir?: string;
  shutdownFn?: () => void | Promise<void>;
  /** Graceful restart: stops daemon but keeps serve alive for session continuity. */
  restartFn?: () => void | Promise<void>;
  getControllerState?: () => ControllerState | undefined;
  runtime?: string;
  tmuxSession?: string;
  tokenManager?: TokenManager;
  indexManager?: {
    getResponse: () => CodebaseIndexResponse;
    rebuild: () => Promise<CodebaseIndexResponse>;
  };
  feedbackLogger?: FeedbackLogger;
  /** Injectable fetcher for testing — defaults to fetchGitHubProjectItems */
  fetchProjectItems?: (owner: string, projectNumber: number) => Promise<unknown>;
  /** Issue tracker backend name (needed for advance endpoint mutations) */
  issueBackend?: "linear" | "github";
  /** Maps worker mode to agent type for AgentPartInput on initial prompt. */
  modeAgents?: Partial<Record<string, string>>;
}

interface ErrorResponse {
  error: string;
}

const JSON_HEADERS = { "content-type": "application/json" };
const MAX_CRASHES = 3;
const ONE_HOUR_MS = 60 * 60 * 1000;
const MAX_RECENT_EVENTS = 50;
const ACTIVITY_FETCH_TIMEOUT_MS = 5000;

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

interface DashboardRecentEvent {
  timestamp: string;
  event: string;
  workerId: string;
  issueId: string;
  mode: string;
  details?: Record<string, unknown>;
}

function extractGitHubIssueTitles(raw: unknown): Map<string, string> {
  const titles = new Map<string, string>();
  const items: unknown[] = Array.isArray(raw)
    ? raw
    : isRecord(raw) && Array.isArray((raw as Record<string, unknown>).items)
      ? ((raw as Record<string, unknown>).items as unknown[])
      : [];
  for (const item of items) {
    if (!isRecord(item)) continue;
    const content = (item as Record<string, unknown>).content;
    if (!isRecord(content)) continue;
    const c = content as Record<string, unknown>;
    if (
      typeof c.title === "string" &&
      typeof c.number === "number" &&
      typeof c.repository === "string"
    ) {
      const parts = (c.repository as string).split("/");
      if (parts.length === 2 && parts[0] && parts[1]) {
        const id = `${parts[0]}-${parts[1]}-${c.number}`.toLowerCase();
        titles.set(id, c.title as string);
      }
    }
  }
  return titles;
}

function buildIssueTopics(owner: string, repo: string, issueNumber: number): string[] {
  return [`notifications.github.${owner}.${repo}.issue.${issueNumber}.>`];
}

export function buildPrTopics(owner: string, repo: string, prNumber: number): string[] {
  return [`notifications.github.${owner}.${repo}.pr.${prNumber}.>`];
}

export function subscribeWorkerToEnvoy(
  sessionId: string,
  topics: string[],
  envoyUrl = "http://127.0.0.1:9020"
): void {
  if (topics.length === 0) return;
  if (!envoyUrl) return;
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

function detachWorkerFromEnvoy(
  entry: BaseWorkerEntry,
  reason: string,
  envoyUrl = "http://127.0.0.1:9020"
): void {
  const hadTopics = entry.envoyTopics;
  entry.envoyTopics = undefined;
  if (!hadTopics?.length) return;
  if (!envoyUrl) return;
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

function publishStateDelta(delta: StateDelta, envoyUrl = "http://127.0.0.1:9020"): void {
  if (!envoyUrl) return;
  fetch(`${envoyUrl}/v1/messages/publish`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      topic: "notifications.legion.controller",
      message: JSON.stringify(delta),
    }),
  })
    .then((res) => {
      if (!res.ok) console.warn(`[state-delta] publish failed: ${res.status} (non-fatal)`);
    })
    .catch((err) => {
      console.warn(`[state-delta] publish error (non-fatal): ${err}`);
    });
}

function unsubscribeAllWorkerTopics(sessionId: string, envoyUrl = "http://127.0.0.1:9020"): void {
  if (!envoyUrl) return;
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

export function startServer(opts: ServerOptions): {
  server: Server;
  stop: () => void;
  fetchAndProcessState: () => Promise<void>;
  cleanupDeadWorkers: () => Promise<void>;
} {
  const hostname = opts.hostname ?? "127.0.0.1";
  const port = opts.port ?? 13370;
  const envoyUrl = opts.envoyUrl ?? "http://127.0.0.1:9020";
  const startedAt = Date.now();
  const workers = new Map<string, WorkerEntry>();
  const crashHistory = new Map<string, CrashHistoryEntry>();
  const issueStateCache = new Map<string, IssueState>();
  let previousIssueState: Record<string, IssueStateDict> | null = null;
  const issueTitleCache = new Map<string, string>();
  const recentEvents: DashboardRecentEvent[] = [];

  // Tracked issue set — in-memory only, re-populated on dispatch
  const trackedIssueIds = new Set<string>();
  // Accumulates new issues since last GET /state/materialized; drains on read
  const newIssuesSinceLastPoll: Array<{ issueId: string; state: IssueStateDict }> = [];

  function recordRecentEvent(evt: DashboardRecentEvent): void {
    recentEvents.push(evt);
    if (recentEvents.length > MAX_RECENT_EVENTS) {
      recentEvents.shift();
    }
  }
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
            return buildIssueTopics(repoRef.owner, repoRef.repo, loadedEntry.issueNumber);
          }
        }
        return undefined;
      })();

      if (recoveredTopics?.length) {
        loadedEntry.envoyTopics = recoveredTopics;
        subscribeWorkerToEnvoy(loadedEntry.sessionId, recoveredTopics, envoyUrl);
      }
    }
  };

  function runPostCollectionProcessing(
    state: CollectedState,
    titles?: Map<string, string>,
    options?: { skipDelta?: boolean }
  ): void {
    for (const [issueId, issueState] of Object.entries(state.issues)) {
      issueStateCache.set(issueId.toLowerCase(), issueState);
    }

    if (titles) {
      for (const [id, title] of titles) {
        issueTitleCache.set(id, title);
      }
    }

    // Delta computation — only for controller-initiated collections
    if (!options?.skipDelta) {
      const currentDict: Record<string, IssueStateDict> = {};
      for (const [issueId, issueState] of Object.entries(state.issues)) {
        currentDict[issueId] = IssueState.toDict(issueState);
      }

      if (previousIssueState !== null) {
        const delta = computeStateDelta(previousIssueState, currentDict, trackedIssueIds);
        if (delta) {
          // Accumulate new issues for GET /state/materialized
          for (const entry of delta.changes.new) {
            newIssuesSinceLastPoll.push(entry);
          }
          if (opts.getControllerState?.()?.sessionId) {
            publishStateDelta(delta, envoyUrl);
          }
        }
      }

      previousIssueState = currentDict;
    }
  }

  const stateLoaded = loadState();

  const cleanupDoneIssueWorkers = async (collectedState: CollectedState): Promise<void> => {
    await stateLoaded;

    const doneIssueIds = Object.entries(collectedState.issues)
      .filter(([, issueState]) => issueState.status === IssueStatus.DONE)
      .map(([issueId]) => issueId.toLowerCase());

    const doneIssueIdSet = new Set(doneIssueIds);
    const cleanedIssueIds = new Set<string>();
    const failedWorkspaceIssueIds = new Set<string>();
    const cleanedWorkspaceIssueIds = new Set<string>();
    const cleanedRepoRefs = new Map<string, RepoRef>();
    let cleanedWorkers = 0;

    // First pass: attempt workspace cleanup (once per issue)
    for (const [workerId, entry] of workers.entries()) {
      const issueId = extractIssueIdFromWorkerId(workerId)?.toLowerCase();
      if (!issueId || !doneIssueIdSet.has(issueId)) {
        continue;
      }
      if (cleanedWorkspaceIssueIds.has(issueId) || failedWorkspaceIssueIds.has(issueId)) {
        continue;
      }
      if (opts.paths && typeof entry.repo === "string") {
        const repoRef = parseIssueRepo(entry.repo);
        if (repoRef) {
          try {
            await cleanupWorkspace(
              opts.paths,
              opts.legionId,
              issueId,
              repoRef,
              opts.repoManagerDeps
            );
            cleanedWorkspaceIssueIds.add(issueId);
            const repoKey = `${repoRef.host}/${repoRef.owner}/${repoRef.repo}`;
            cleanedRepoRefs.set(repoKey, repoRef);
          } catch (error) {
            console.warn(
              `[auto-cleanup] workspace cleanup failed for ${issueId}: ${error instanceof Error ? error.message : String(error)}`
            );
            failedWorkspaceIssueIds.add(issueId);
          }
        }
      }
    }

    // Second pass: remove worker state only for issues where workspace was cleaned (or no workspace)
    for (const [workerId, entry] of workers.entries()) {
      const issueId = extractIssueIdFromWorkerId(workerId)?.toLowerCase();
      if (!issueId || !doneIssueIdSet.has(issueId)) {
        continue;
      }
      // Skip issues where workspace cleanup failed — preserve state for retry
      if (failedWorkspaceIssueIds.has(issueId)) {
        continue;
      }

      try {
        await opts.adapter.deleteSession(entry.sessionId);
      } catch (error) {
        console.warn(
          `[auto-cleanup] session delete failed for ${entry.id}: ${error instanceof Error ? error.message : String(error)}`
        );
      }

      detachWorkerFromEnvoy(entry, "auto-cleanup-done", envoyUrl);
      unsubscribeAllWorkerTopics(entry.sessionId, envoyUrl);
      workers.delete(entry.id);
      crashHistory.delete(entry.id);
      cleanedIssueIds.add(issueId);
      cleanedWorkers += 1;
    }

    for (const issueId of cleanedIssueIds) {
      issueStateCache.delete(issueId);
    }

    // Auto-untrack all Done issues (regardless of whether they had workers)
    for (const issueId of doneIssueIds) {
      trackedIssueIds.delete(issueId);
    }

    if (cleanedWorkers > 0) {
      await persistState();
      console.log(
        `[auto-cleanup] Cleaned ${cleanedWorkers} workers for ${cleanedIssueIds.size} Done issues`
      );
    }

    // Fetch repo clones for Done issues — the merge just landed, so bring the
    // default clone up to date for future dispatches. Non-blocking, best-effort.
    if (opts.paths && cleanedRepoRefs.size > 0) {
      for (const [repoKey, repoRef] of cleanedRepoRefs) {
        startBackgroundFetch(opts.paths, repoRef, opts.repoManagerDeps).catch((err) => {
          console.warn(
            `[auto-cleanup] Post-close fetch failed for ${repoKey}: ${err instanceof Error ? err.message : String(err)}`
          );
        });
      }
    }

    // Directory scan fallback: remove workspaces for issues no longer on the board
    // (not Done — just removed/archived). Shallow scan only — never recurse into workspaces.
    if (opts.paths) {
      const listDir = opts.repoManagerDeps?.listDir ?? defaultRepoManagerDeps.listDir;
      if (listDir) {
        const workspacesDir = opts.paths.forLegion(opts.legionId).workspacesDir;
        const entries = await listDir(workspacesDir);
        const boardIssueIds = new Set(
          Object.keys(collectedState.issues).map((id) => id.toLowerCase())
        );
        const activeWorkerIssueIds = new Set<string>();
        for (const workerId of workers.keys()) {
          const issueId = extractIssueIdFromWorkerId(workerId)?.toLowerCase();
          if (issueId) {
            activeWorkerIssueIds.add(issueId);
          }
        }

        // Derive repo clone path from any existing worker (all workers share the same repo)
        let fallbackClonePath: string | null = null;
        for (const workerEntry of workers.values()) {
          if (typeof workerEntry.repo === "string") {
            const ref = parseIssueRepo(workerEntry.repo);
            if (ref) {
              fallbackClonePath = opts.paths.repoClonePath(ref.host, ref.owner, ref.repo);
              break;
            }
          }
        }

        for (const entry of entries) {
          const issueId = basename(entry).toLowerCase();
          if (boardIssueIds.has(issueId) || activeWorkerIssueIds.has(issueId)) {
            continue;
          }

          // Best-effort branch push check before removing off-board workspace
          if (fallbackClonePath) {
            try {
              const pushCheck = await verifyBranchPushed(
                fallbackClonePath,
                issueId,
                opts.repoManagerDeps
              );
              if (!pushCheck.safe) {
                console.warn(
                  `[auto-cleanup] Skipping off-board workspace ${issueId}: ${pushCheck.reason}`
                );
                continue;
              }
            } catch (error) {
              console.warn(
                `[auto-cleanup] Branch push check failed for ${issueId}, skipping removal: ${error instanceof Error ? error.message : String(error)}`
              );
              continue;
            }
          }

          const workspacePath = join(workspacesDir, basename(entry));
          try {
            await removeDir(workspacePath, opts.repoManagerDeps);
            console.log(`[auto-cleanup] Removed off-board workspace: ${workspacePath}`);
          } catch (error) {
            console.warn(
              `[auto-cleanup] Failed to remove off-board workspace ${workspacePath}: ${error instanceof Error ? error.message : String(error)}`
            );
          }
        }
      }
    }
  };

  let server: Server | null = null;

  const fetchAndCollectState = async (
    backend: Parameters<typeof getBackend>[0],
    rawIssues: unknown
  ): Promise<{ state: CollectedState; titles: Map<string, string> }> => {
    if (!server) {
      throw new Error("server_not_started");
    }

    const tracker = getBackend(backend);
    const parsed = tracker.parseIssues(rawIssues);
    const daemonUrl = `http://127.0.0.1:${server.port}`;
    const issuesData = await enrichParsedIssues(parsed, daemonUrl);
    const state = buildCollectedState(issuesData, opts.legionId);
    const titles =
      backend === "github" && rawIssues
        ? extractGitHubIssueTitles(rawIssues)
        : new Map<string, string>();

    return { state, titles };
  };

  server = Bun.serve({
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

        if (method === "GET" && url.pathname === "/dashboard") {
          await stateLoaded;
          const allWorkers = Array.from(workers.values());

          // Summary stats
          const byStatus: Record<string, number> = {};
          const byPhase: Record<string, number> = {};
          for (const w of allWorkers) {
            byStatus[w.status] = (byStatus[w.status] ?? 0) + 1;
            const phase = extractModeFromWorkerId(w.id);
            if (phase) {
              byPhase[phase] = (byPhase[phase] ?? 0) + 1;
            }
          }

          // Fetch activity for all workers in parallel (with timeout)
          const activityResults = await Promise.allSettled(
            allWorkers.map(async (w) => {
              const result = await Promise.race([
                opts.adapter.getSessionStatus(w.sessionId),
                new Promise<{ data?: unknown; error: string }>((resolve) =>
                  setTimeout(() => resolve({ error: "timeout" }), ACTIVITY_FETCH_TIMEOUT_MS)
                ),
              ]);
              return { workerId: w.id, result };
            })
          );

          const activityMap = new Map<string, Record<string, unknown>>();
          for (const settled of activityResults) {
            if (settled.status === "fulfilled") {
              const { workerId, result } = settled.value;
              if (result.data && isRecord(result.data)) {
                activityMap.set(workerId, result.data as Record<string, unknown>);
              }
            }
          }

          // Group workers by repo + issueNumber
          const groups: Record<
            string,
            Record<
              string,
              {
                issueTitle: string | null;
                issueStatus: string | null;
                workers: Array<Record<string, unknown>>;
              }
            >
          > = {};

          for (const w of allWorkers) {
            const repo = w.repo ?? "_unknown";
            const issueNum = String(w.issueNumber ?? 0);
            const issueId = extractIssueIdFromWorkerId(w.id);

            if (!groups[repo]) {
              groups[repo] = {};
            }
            if (!groups[repo][issueNum]) {
              const cachedState = issueId ? issueStateCache.get(issueId.toLowerCase()) : undefined;
              groups[repo][issueNum] = {
                issueTitle:
                  (issueId ? issueTitleCache.get(issueId.toLowerCase()) : undefined) ?? null,
                issueStatus: cachedState?.status ?? null,
                workers: [],
              };
            }

            const activity = activityMap.get(w.id);
            groups[repo][issueNum].workers.push({
              id: w.id,
              phase: extractModeFromWorkerId(w.id),
              status: w.status,
              sessionId: w.sessionId,
              startedAt: w.startedAt,
              crashCount: w.crashCount,
              activity: activity
                ? {
                    type:
                      typeof activity.type === "string"
                        ? activity.type
                        : typeof activity.phase === "string"
                          ? activity.phase
                          : "unknown",
                    messageCount:
                      typeof activity.messageCount === "number" ? activity.messageCount : 0,
                    turnCount: typeof activity.turnCount === "number" ? activity.turnCount : 0,
                    tokensUsed: typeof activity.tokensUsed === "number" ? activity.tokensUsed : 0,
                    lastActivityAt:
                      typeof activity.lastActivityAt === "string" ? activity.lastActivityAt : null,
                  }
                : null,
            });
          }

          return jsonResponse({
            generatedAt: new Date().toISOString(),
            summary: {
              totalWorkers: allWorkers.length,
              byStatus,
              byPhase,
            },
            groups,
            recentEvents: recentEvents.slice().reverse(),
          });
        }

        if (method === "GET" && url.pathname === "/dashboard/ui") {
          return new Response(getDashboardHtml(), {
            status: 200,
            headers: { "content-type": "text/html; charset=utf-8" },
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
            let repo = payload.repo;
            const workspace = payload.workspace;
            const version = typeof payload.version === "number" ? payload.version : 0;
            const envPayload = payload.env;
            let issueNumber =
              typeof payload.issueNumber === "number" ? payload.issueNumber : undefined;

            const prompt = payload.prompt;
            const providedSessionId = payload.sessionId;
            if (typeof issueId !== "string" || typeof mode !== "string") {
              return badRequest("missing_fields");
            }
            if (typeof repo === "string" && typeof workspace === "string") {
              return badRequest(
                "repo and workspace are mutually exclusive — provide one or neither"
              );
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

            if (
              providedSessionId !== undefined &&
              (typeof providedSessionId !== "string" || !SESSION_ID_PATTERN.test(providedSessionId))
            ) {
              return jsonResponse(
                {
                  error: "invalid_session_id",
                  message: "sessionId must match format: ses_ + 12 hex + 14 Base62",
                },
                422
              );
            }

            // Phase prerequisite validation for gated modes
            const normalizedIssueId = issueId.toLowerCase();
            if (typeof repo !== "string" && typeof workspace !== "string") {
              const cachedState = issueStateCache.get(normalizedIssueId);
              if (cachedState?.source) {
                repo = `${cachedState.source.owner}/${cachedState.source.repo}`;
                console.log(
                  `[dispatch] auto-resolved repo ${repo} from issue state for ${issueId}`
                );
              } else {
                return badRequest(
                  "missing_repo: provide --repo or ensure issue appears in collected state"
                );
              }
            }
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
            // Auto-extract issueNumber from issueId when not explicitly provided.
            // GitHub issue IDs follow the format {owner}-{repo}-{number}[-{slug}].
            if (issueNumber === undefined && repoRef) {
              const prefix = `${repoRef.owner}-${repoRef.repo}-`.toLowerCase();
              if (normalizedIssueId.startsWith(prefix)) {
                const remainder = normalizedIssueId.slice(prefix.length);
                const match = remainder.match(/^(\d+)(?:-|$)/);
                if (match) {
                  const parsed = Number(match[1]);
                  if (Number.isInteger(parsed) && parsed > 0) {
                    issueNumber = parsed;
                  }
                }
              }
            }
            if (typeof repo === "string") {
              if (!opts.paths) {
                return badRequest("repo_resolution_unavailable");
              }
              if (!repoRef) {
                return badRequest("invalid_repo: expected owner/repo");
              }
              // For implement mode: blocking fetch BEFORE workspace creation so the
              // workspace is created from a fresh clone. For other modes: non-blocking
              // fetch after workspace creation (existing behavior).
              if (mode === WorkerMode.IMPLEMENT) {
                try {
                  await startBackgroundFetch(opts.paths, repoRef, opts.repoManagerDeps);
                } catch (err) {
                  console.warn(
                    `[dispatch] Pre-dispatch fetch failed for ${repoRef.owner}/${repoRef.repo}: ${err instanceof Error ? err.message : String(err)} — proceeding with potentially stale clone`
                  );
                }
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
              if (mode !== WorkerMode.IMPLEMENT) {
                // Non-implement modes: fire background fetch (non-blocking) — pre-warms
                // the clone for the worker's own `jj git fetch` during startup.
                startBackgroundFetch(opts.paths, repoRef, opts.repoManagerDeps).catch((err) => {
                  console.error(
                    `[dispatch] Background fetch failed for ${repoRef.owner}/${repoRef.repo}: ${err instanceof Error ? err.message : String(err)}`
                  );
                });
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
              // Delete old session from serve to release SQLite FDs before replacing
              await opts.adapter.deleteSession(existing.sessionId);
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

            const sessionId =
              typeof providedSessionId === "string"
                ? providedSessionId
                : computeSessionId(opts.legionId, issueId, mode as WorkerModeLiteral, version);

            const workerAdapter = opts.adapter;

            const workerEnv: Record<string, string> | undefined = isRecord(envPayload)
              ? (envPayload as Record<string, string>)
              : undefined;
            if (workerEnv) {
              for (const [k, v] of Object.entries(workerEnv)) {
                if (typeof v !== "string") {
                  return badRequest(`env values must be strings (key "${k}")`);
                }
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

            // Duplicate session guard: prevent same session from being tracked by multiple workers
            if (typeof providedSessionId === "string") {
              for (const [, existingEntry] of workers) {
                if (
                  existingEntry.status !== "dead" &&
                  existingEntry.sessionId === providedSessionId
                ) {
                  return jsonResponse(
                    {
                      error: "session_already_enlisted",
                      id: existingEntry.id,
                      sessionId: providedSessionId,
                    },
                    409
                  );
                }
              }
            }

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
                detachWorkerFromEnvoy(existingEntry, "cross-mode-cleanup", envoyUrl);
              }
            }

            // Mode-based Envoy subscription (GitHub-only, fire-and-forget)
            // Only plan mode subscribes to issue topics at dispatch.
            // Implement self-subscribes to PR topics after PR creation via envoy_subscribe.
            if (mode === WorkerMode.PLAN && repoRef && issueNumber !== undefined) {
              const topics = buildIssueTopics(repoRef.owner, repoRef.repo, issueNumber);
              subscribeWorkerToEnvoy(actualSessionId, topics, envoyUrl);
              entry.envoyTopics = topics;
              workerEntriesChanged = true;
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
                    const agentName = opts.modeAgents?.[mode];
                    await workerAdapter.sendPrompt(actualSessionId, prompt, agentName);
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
            // Auto-track the issue when a worker is dispatched
            trackedIssueIds.add(normalizedIssueId);

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
            recordRecentEvent({
              timestamp: new Date().toISOString(),
              event: "worker.dispatched",
              workerId: entry.id,
              issueId: normalizedIssueId,
              mode: mode as string,
              details: { version, crashCount: entry.crashCount },
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

          // Delete session from serve to release SQLite FDs and memory
          await opts.adapter.deleteSession(entry.sessionId);

          // Also remove the worker entry — workspace deletion means the issue is done
          workers.delete(workerId);
          crashHistory.delete(workerId);
          await persistState();
          unsubscribeAllWorkerTopics(entry.sessionId, envoyUrl);

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
              // Clear daemon-managed topic tracking before deletion
              detachWorkerFromEnvoy(entry, "prune-done", envoyUrl);
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

          // Delete sessions from serve BEFORE persisting state removal,
          // so a crash between persist and delete can't orphan sessions.
          for (const sessionId of prunedSessionIds) {
            await opts.adapter.deleteSession(sessionId);
          }

          if (prunedWorkers.length > 0 || prunedCrashHistory.length > 0) {
            await persistState();
          }

          // Blanket-unsubscribe all pruned sessions from Envoy.
          // detachWorkerFromEnvoy above handles daemon-managed topics, but workers
          // may also have self-managed subscriptions (e.g. implementer PR topics)
          // that aren't tracked in entry.envoyTopics.
          for (const sessionId of prunedSessionIds) {
            unsubscribeAllWorkerTopics(sessionId, envoyUrl);
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
                detachWorkerFromEnvoy(updated, "worker-dead", envoyUrl);
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
            recordRecentEvent({
              timestamp: new Date().toISOString(),
              event: "worker.status_changed",
              workerId: id,
              issueId: extractIssueIdFromWorkerId(id) ?? id,
              mode: extractModeFromWorkerId(id) ?? "unknown",
              details: {
                fromStatus: entry.status,
                toStatus: updated.status,
                crashCount: updated.crashCount,
              },
            });
            const { env: _patchEnv, ...safeUpdated } = updated;
            return jsonResponse(safeUpdated);
          }
          if (method === "DELETE") {
            // Delete session from serve to release SQLite FDs and memory
            await opts.adapter.deleteSession(entry.sessionId);
            crashHistory.set(id, {
              crashCount: entry.crashCount,
              lastCrashAt: entry.lastCrashAt,
            });
            entry.envoyTopics = undefined;
            workers.delete(id);
            await persistState();
            unsubscribeAllWorkerTopics(entry.sessionId, envoyUrl);
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
            const result = await opts.adapter.getSessionStatus(entry.sessionId);
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
            await opts.adapter.sendPrompt(entry.sessionId, text);

            if (entry.envoyTopics?.length) {
              subscribeWorkerToEnvoy(entry.sessionId, entry.envoyTopics, envoyUrl);
            }

            return jsonResponse({ ok: true });
          } catch (error) {
            return serverError(`Failed to send prompt: ${(error as Error).message}`);
          }
        }

        if (segments.length === 3 && segments[0] === "workers" && segments[2] === "token") {
          await stateLoaded;
          if (method !== "GET") {
            return notFound();
          }
          const id = segments[1].toLowerCase();
          const entry = workers.get(id);
          if (!entry) {
            return notFound();
          }
          if (!opts.tokenManager) {
            return notFound("token_manager_unavailable");
          }

          const mode = extractModeFromWorkerId(entry.id);
          if (!mode) {
            return notFound();
          }

          const repoRef = entry.repo ? parseIssueRepo(entry.repo) : null;
          if (!repoRef) {
            return badRequest("missing_repo");
          }

          const role = modeToRole(mode);
          if (!opts.tokenManager.isConfigured(role)) {
            return notFound("role_not_configured");
          }

          try {
            const credential = await opts.tokenManager.getToken(role, repoRef.owner);
            return jsonResponse({
              role,
              owner: repoRef.owner,
              expiresAt: credential.expiresAt,
              env: {
                GH_TOKEN: credential.token,
                GIT_AUTHOR_NAME: credential.gitIdentity.name,
                GIT_AUTHOR_EMAIL: credential.gitIdentity.email,
                GIT_COMMITTER_NAME: credential.gitIdentity.name,
                GIT_COMMITTER_EMAIL: credential.gitIdentity.email,
                LEGION_APP_ROLE: role,
              },
            });
          } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            if (message.startsWith("role_not_configured:")) {
              return notFound("role_not_configured");
            }
            return serverError(message);
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
            const { state, titles } = await fetchAndCollectState(backend, issues);

            runPostCollectionProcessing(state, backend === "github" ? titles : undefined);

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
                  prReviewState: issueState.prReviewState,
                  ciStatus: issueState.ciStatus,
                  mergeableStatus: issueState.mergeableStatus,
                  labels: issueState.labels,
                });
              }
            }

            cleanupDoneIssueWorkers(state).catch((err) =>
              console.error(
                "[auto-cleanup] failed:",
                err instanceof Error ? err.message : String(err)
              )
            );

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
            if (backend === "github") {
              const providedLegionId =
                typeof payload.legionId === "string" ? payload.legionId : null;
              const legionId = providedLegionId ?? opts.legionId;
              const parts = legionId.split("/");
              if (parts.length !== 2 || !parts[1]) {
                return badRequest("invalid_team_id: expected owner/project-number");
              }
              if (!Number.isFinite(Number(parts[1]))) {
                return badRequest("invalid_team_id: project number not a number");
              }

              const boardIds = [legionId, ...(opts.extraProjects ?? [])];
              console.log(
                `[fetch-and-collect] fetching from ${boardIds.length} boards: ${boardIds.join(", ")}`
              );

              const tracker = getBackend(backend);
              const collectedBoards: Array<{ boardId: string; rawIssues: unknown }> = [];
              const boardErrors: string[] = [];

              for (const boardId of boardIds) {
                try {
                  const boardParts = boardId.split("/");
                  if (boardParts.length !== 2 || !boardParts[0] || !boardParts[1]) {
                    throw new Error("invalid_team_id: expected owner/project-number");
                  }
                  const projectNumber = Number(boardParts[1]);
                  if (!Number.isFinite(projectNumber)) {
                    throw new Error("invalid_team_id: project number not a number");
                  }
                  const fetchFn = opts.fetchProjectItems ?? fetchGitHubProjectItems;
                  const rawIssues = await fetchFn(boardParts[0], projectNumber);
                  collectedBoards.push({ boardId, rawIssues });
                } catch (error) {
                  const message = error instanceof Error ? error.message : String(error);
                  boardErrors.push(`${boardId}: ${message}`);
                  console.error(`[fetch-and-collect] board=${boardId} error=${message}`);
                }
              }

              if (collectedBoards.length === 0) {
                return serverError(
                  `fetch_and_collect_failed: ${boardErrors.join("; ") || "all boards failed"}`
                );
              }

              const parsed = [];
              const seenSources = new Map<string, string>();
              const extractedTitles = new Map<string, string>();
              for (const { boardId, rawIssues } of collectedBoards) {
                const boardIssues = tracker.parseIssues(rawIssues);
                for (const issue of boardIssues) {
                  if (issue.source) {
                    const sourceKey =
                      `${issue.source.owner}/${issue.source.repo}#${issue.source.number}`.toLowerCase();
                    const existingBoard = seenSources.get(sourceKey);
                    if (existingBoard) {
                      console.warn(
                        `[fetch-and-collect] duplicate issue ${sourceKey} on boards ${existingBoard}, ${boardId} — using ${existingBoard} (primary)`
                      );
                      continue;
                    }
                    seenSources.set(sourceKey, boardId);
                  }
                  parsed.push(issue);
                }

                for (const [id, title] of extractGitHubIssueTitles(rawIssues)) {
                  if (!extractedTitles.has(id)) {
                    extractedTitles.set(id, title);
                  }
                }
              }

              if (!server) {
                return serverError("server_not_started");
              }
              const daemonUrl = `http://127.0.0.1:${server.port}`;
              const issuesData = await enrichParsedIssues(parsed, daemonUrl);
              const state = buildCollectedState(issuesData, opts.legionId);

              runPostCollectionProcessing(state, extractedTitles);

              cleanupDoneIssueWorkers(state).catch((err) =>
                console.error(
                  "[auto-cleanup] failed:",
                  err instanceof Error ? err.message : String(err)
                )
              );

              return jsonResponse({
                ...CollectedState.toDict(state),
                titles: Object.fromEntries(extractedTitles),
              });
            } else {
              return badRequest("fetch-and-collect only supports github backend currently");
            }
          } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            console.error(`[fetch-and-collect] backend=${backend} error=${message}`);
            return serverError(`fetch_and_collect_failed: ${message}`);
          }
        }

        // GET /state/track — list tracked issue IDs
        if (method === "GET" && url.pathname === "/state/track") {
          return jsonResponse({ trackedIssues: [...trackedIssueIds].sort() });
        }

        // POST /state/track — manually track an issue
        if (method === "POST" && url.pathname === "/state/track") {
          const payload = await request.json().catch(() => null);
          const payloadObj =
            payload && typeof payload === "object" ? (payload as Record<string, unknown>) : null;
          if (!payloadObj || typeof payloadObj.issueId !== "string") {
            return badRequest("issueId is required");
          }
          const issueId = payloadObj.issueId.toLowerCase();
          if (!issueId) {
            return badRequest("issueId must be non-empty");
          }
          trackedIssueIds.add(issueId);
          return jsonResponse({ tracked: true });
        }

        // DELETE /state/track/:issueId — manually untrack an issue
        if (method === "DELETE" && url.pathname.startsWith("/state/track/")) {
          const issueId = url.pathname.slice("/state/track/".length).toLowerCase();
          if (!issueId) {
            return badRequest("issueId is required");
          }
          trackedIssueIds.delete(issueId);
          return jsonResponse({ untracked: true });
        }

        // GET /state/materialized — tracked issues from cache + new-issues accumulator (drains on read)
        if (method === "GET" && url.pathname === "/state/materialized") {
          const issues: Record<string, IssueStateDict> = {};
          const titles: Record<string, string> = {};

          for (const issueId of trackedIssueIds) {
            const issueState = issueStateCache.get(issueId);
            if (issueState) {
              issues[issueId] = IssueState.toDict(issueState);
            }
            const title = issueTitleCache.get(issueId);
            if (title) {
              titles[issueId] = title;
            }
          }

          // Drain the new-issues accumulator
          const newIssues = newIssuesSinceLastPoll.splice(0);

          return jsonResponse({ issues, titles, newIssues });
        }

        // POST /state/advance — advance an issue to its next lifecycle stage
        if (method === "POST" && url.pathname === "/state/advance") {
          await stateLoaded;
          let payload: Record<string, unknown>;
          try {
            payload = await parseJson(request);
          } catch {
            return badRequest("invalid_json");
          }

          if (typeof payload.issueId !== "string" || !payload.issueId) {
            return badRequest("missing_issue_id");
          }

          const issueId = payload.issueId.toLowerCase();
          const cachedState = issueStateCache.get(issueId);

          if (!cachedState) {
            return jsonResponse(
              { error: "issue_not_in_cache", message: "Run 'legion poll <team>' first" },
              412
            );
          }

          // If --stage was provided, delegate to POST /workers with force=true
          if (typeof payload.stage === "string") {
            const mode = payload.stage as WorkerModeLiteral;
            const repo = cachedState.source
              ? `${cachedState.source.owner}/${cachedState.source.repo}`
              : undefined;
            const backendName = opts.issueBackend ?? "github";
            const defaultPrompt = repo
              ? `Invoke the /legion-worker skill for ${mode} mode for ${issueId} (${backendName} backend, repo: ${repo}). Before starting, check for project-specific skills that may be relevant to this work.`
              : `Invoke the /legion-worker skill for ${mode} mode for ${issueId}`;
            if (!server) {
              return serverError("server_not_started");
            }
            const dispatchResponse = await fetch(`http://127.0.0.1:${server.port}/workers`, {
              method: "POST",
              headers: JSON_HEADERS,
              body: JSON.stringify({
                issueId,
                mode,
                force: true,
                ...(repo ? { repo } : {}),
                prompt: defaultPrompt,
              }),
            });
            const dispatchResult = (await dispatchResponse.json()) as Record<string, unknown>;
            if (!dispatchResponse.ok) {
              return jsonResponse(
                { action: `dispatch_${mode}`, executed: "error", ...dispatchResult },
                dispatchResponse.status
              );
            }
            return jsonResponse({
              action: `dispatch_${mode}`,
              executed: "dispatched",
              workerId: dispatchResult.id,
              sessionId: dispatchResult.sessionId,
              port: dispatchResult.port,
            });
          }

          const action = cachedState.suggestedAction;

          // Skip/retry/investigate — return without executing
          if (action === "skip" || action === "retry_pr_check" || action === "retry_ci_check") {
            return jsonResponse({
              action,
              executed: "skipped",
              reason: `Issue is not ready to advance: ${action}`,
            });
          }
          if (action === "investigate_no_pr") {
            return jsonResponse({
              action,
              executed: "error",
              reason: "Issue has worker-done but no PR — needs investigation",
            });
          }
          if (action === "add_needs_approval") {
            return jsonResponse({
              action,
              executed: "skipped",
              reason: "Issue needs approval before advancing",
            });
          }

          // Dispatch and resume actions — dispatch via internal POST /workers
          if (
            action.startsWith("dispatch_") ||
            action.startsWith("resume_") ||
            action === "relay_user_feedback" ||
            action === "remove_worker_active_and_redispatch"
          ) {
            // Check for live worker before dispatch
            if (cachedState.hasLiveWorker) {
              const mode = ACTION_TO_MODE[action];
              return jsonResponse(
                {
                  error: "worker_already_running",
                  action,
                  workerId: `${issueId}-${mode}`,
                },
                409
              );
            }

            const mode = ACTION_TO_MODE[action];
            const repo = cachedState.source
              ? `${cachedState.source.owner}/${cachedState.source.repo}`
              : undefined;
            const backendName = opts.issueBackend ?? "github";
            const repoSuffix = repo ? ` (${backendName} backend, repo: ${repo})` : "";

            // Remove stale worker-active label before redispatch
            if (action === "remove_worker_active_and_redispatch") {
              try {
                const tracker = getBackend(backendName as BackendName);
                if (tracker.removeLabel) {
                  await tracker.removeLabel(
                    { issueId, source: cachedState.source },
                    "worker-active"
                  );
                }
              } catch (err) {
                console.error(
                  `[advance] Failed to remove worker-active label for ${issueId}: ${err instanceof Error ? err.message : String(err)}`
                );
              }
            }

            let prompt: string;

            switch (action) {
              case "resume_implementer_for_changes":
                prompt = `Invoke the /legion-worker skill for implement mode. The reviewer has requested changes on your PR — check the review comments and address them${repoSuffix}.`;
                break;
              case "resume_implementer_for_retro":
              case "dispatch_implementer_for_retro":
                prompt = "/legion-retro";
                break;
              case "resume_implementer_for_ci_failure":
                prompt = `Invoke the /legion-worker skill for implement mode. CI is failing on your PR — check the failures and fix${repoSuffix}.`;
                break;
              case "resume_implementer_for_test_failure":
                prompt = `Invoke the /legion-worker skill for implement mode. The tester found issues — check the test feedback and fix${repoSuffix}.`;
                break;
              case "relay_user_feedback":
                prompt = `Invoke the /legion-worker skill for implement mode. The user has responded to your escalation — check the latest issue comments for their feedback and continue${repoSuffix}.`;
                break;
              default:
                prompt = `Invoke the /legion-worker skill for ${mode} mode for ${issueId}${repoSuffix}. Before starting, check for project-specific skills that may be relevant to this work.`;
                break;
            }

            if (!server) {
              return serverError("server_not_started");
            }
            const dispatchResponse = await fetch(`http://127.0.0.1:${server.port}/workers`, {
              method: "POST",
              headers: JSON_HEADERS,
              body: JSON.stringify({
                issueId,
                mode,
                force: true,
                ...(repo ? { repo } : {}),
                prompt,
              }),
            });
            const dispatchResult = (await dispatchResponse.json()) as Record<string, unknown>;

            if (!dispatchResponse.ok) {
              return jsonResponse(
                { action, executed: "error", ...dispatchResult },
                dispatchResponse.status
              );
            }
            return jsonResponse({
              action,
              executed: "dispatched",
              workerId: dispatchResult.id,
              sessionId: dispatchResult.sessionId,
              port: dispatchResult.port,
            });
          }

          // Transition actions — update issue status in tracker
          if (action.startsWith("transition_to_")) {
            const TRANSITION_STATUS: Partial<Record<ActionType, IssueStatusLiteral>> = {
              transition_to_todo: "Todo",
              transition_to_in_progress: "In Progress",
              transition_to_testing: "Testing",
              transition_to_needs_review: "Needs Review",
              transition_to_retro: "Retro",
              transition_to_done: "Done",
            };
            const targetStatus = TRANSITION_STATUS[action];
            if (!targetStatus) {
              return jsonResponse(
                { action, executed: "error", reason: `Unknown transition action: ${action}` },
                500
              );
            }

            const backendName = (opts.issueBackend ?? "github") as BackendName;
            const tracker = getBackend(backendName);
            const mutationTarget = { issueId, source: cachedState.source };
            try {
              if (tracker.transitionIssue) {
                await tracker.transitionIssue(mutationTarget, targetStatus);
              }
              if (tracker.removeLabel) {
                await tracker.removeLabel(mutationTarget, "worker-done");
              }
            } catch (err) {
              return jsonResponse(
                {
                  action,
                  executed: "error",
                  reason: `Transition failed: ${err instanceof Error ? err.message : String(err)}`,
                },
                500
              );
            }
            return jsonResponse({ action, executed: "transitioned", newStatus: targetStatus });
          }

          // Unhandled action
          return jsonResponse({
            action,
            executed: "skipped",
            reason: `Unhandled action: ${action}`,
          });
        }

        if (method === "POST" && url.pathname === "/shutdown") {
          await opts.shutdownFn?.();
          return jsonResponse({ status: "shutting_down" });
        }

        if (method === "POST" && url.pathname === "/restart") {
          if (!opts.restartFn) {
            return serverError("restart_not_supported");
          }
          await opts.restartFn();
          return jsonResponse({ status: "restarting" });
        }

        // --- Promoted sessions ---
        if (segments.length === 1 && segments[0] === "promoted") {
          const promotedFile = opts.paths?.forLegion(opts.legionId).promotedFile;
          if (!promotedFile) {
            return serverError("paths_not_configured");
          }

          if (method === "GET") {
            const data = await readPromotedSessions(promotedFile);
            return jsonResponse(listPromotedSessions(data));
          }

          if (method === "POST") {
            let payload: Record<string, unknown>;
            try {
              payload = await parseJson(request);
            } catch {
              return badRequest("invalid_json");
            }

            const sessionId = payload.sessionId;
            const role = payload.role;
            if (typeof sessionId !== "string" || !sessionId) {
              return badRequest("missing_session_id");
            }
            if (!SESSION_ID_PATTERN.test(sessionId)) {
              return badRequest("invalid_session_id");
            }
            if (typeof role !== "string" || !role) {
              return badRequest("missing_role");
            }
            const repo = typeof payload.repo === "string" ? payload.repo : undefined;

            const session = await promoteSession(promotedFile, sessionId, role, repo);
            return jsonResponse(session, 201);
          }
        }

        if (segments.length === 2 && segments[0] === "promoted" && method === "DELETE") {
          const promotedFile = opts.paths?.forLegion(opts.legionId).promotedFile;
          if (!promotedFile) {
            return serverError("paths_not_configured");
          }

          const sessionId = decodeURIComponent(segments[1]);
          const removed = await demoteSession(promotedFile, sessionId);
          if (!removed) {
            return notFound("session_not_promoted");
          }
          return jsonResponse({ demoted: sessionId });
        }

        return notFound();
      } catch {
        return serverError();
      }
    },
  });

  /**
   * Clean up workspaces for dead workers on the health tick.
   *
   * For each worker with status === "dead":
   * 1. jj workspace forget + rm -rf the workspace directory
   * 2. Delete the serve session
   * 3. Detach from Envoy
   * 4. Remove worker + crash history from in-memory maps
   * 5. Persist state
   *
   * This prevents dead worktrees from blocking future dispatches to the same issue.
   */
  const cleanupDeadWorkers = async (): Promise<void> => {
    await stateLoaded;

    const deadWorkerIds: string[] = [];
    for (const [workerId, entry] of workers.entries()) {
      if (entry.status === "dead") {
        deadWorkerIds.push(workerId);
      }
    }

    if (deadWorkerIds.length === 0) {
      return;
    }

    let cleanedWorkers = 0;

    // Remove worker state — session, Envoy, in-memory maps.
    // Workspaces are NOT deleted here; they are only cleaned when the issue
    // moves to Done (via cleanupDoneIssueWorkers). This prevents data loss
    // when a worker is reaped due to transient liveness failures.
    for (const workerId of deadWorkerIds) {
      const entry = workers.get(workerId);
      if (!entry) continue;

      try {
        await opts.adapter.deleteSession(entry.sessionId);
      } catch (error) {
        console.warn(
          `[dead-worker-cleanup] session delete failed for ${entry.id}: ${error instanceof Error ? error.message : String(error)}`
        );
      }

      detachWorkerFromEnvoy(entry, "dead-worker-cleanup", envoyUrl);
      unsubscribeAllWorkerTopics(entry.sessionId, envoyUrl);
      workers.delete(entry.id);
      crashHistory.delete(entry.id);
      cleanedWorkers += 1;
    }

    if (cleanedWorkers > 0) {
      await persistState();
      console.log(`[dead-worker-cleanup] Cleaned ${cleanedWorkers} dead workers`);
    }
  };

  const fetchAndProcessState = async (): Promise<void> => {
    const legionId = opts.legionId;
    const parts = legionId.split("/");
    if (parts.length !== 2 || !parts[1]) {
      return;
    }

    const [owner, numStr] = parts;
    const projectNumber = Number(numStr);
    if (!Number.isFinite(projectNumber)) {
      return;
    }

    const fetchFn = opts.fetchProjectItems ?? fetchGitHubProjectItems;
    const rawIssues = await fetchFn(owner, projectNumber);
    const { state, titles: extractedTitles } = await fetchAndCollectState("github", rawIssues);

    runPostCollectionProcessing(state, extractedTitles, { skipDelta: true });

    cleanupDoneIssueWorkers(state).catch((err) =>
      console.error("[auto-cleanup] failed:", err instanceof Error ? err.message : String(err))
    );
  };

  return {
    server,
    stop: () => {
      releaseGauges();
      server.stop(true);
    },
    fetchAndProcessState,
    cleanupDeadWorkers,
  };
}
