import { existsSync, mkdirSync } from "node:fs";
import path from "node:path";
import { CodebaseIndexManager } from "../index/manager";
import {
  CODEBASE_INDEX_VERSION,
  type CodebaseIndex,
  createEmptyCodebaseIndexResponse,
} from "../index/types";
import { computeControllerSessionId } from "../state/types";
import { type DaemonConfig, loadConfig, validateControllerPrompt } from "./config";
import { modeToRole, TokenManager } from "./github-apps";
import {
  allocatePort,
  readLegionsRegistry,
  removeLegionEntry,
  writeLegionEntry,
} from "./legions-registry";
import { RoleServeManager } from "./multi-serve";
import { isPortFree } from "./ports";
import { createAdapter } from "./runtime";
import type { RuntimeAdapter } from "./runtime/types";
import { startServer } from "./server";
import { type ControllerState, readStateFile, writeStateFile } from "./state-file";
import {
  registerGauges,
  registerSignals,
  start as startMemoryTelemetry,
  stop as stopMemoryTelemetry,
} from "./telemetry";

type ServerHandle = ReturnType<typeof startServer>;

interface DaemonDependencies {
  adapter: RuntimeAdapter;
  startServer: typeof startServer;
  readStateFile: typeof readStateFile;
  writeStateFile: typeof writeStateFile;
  fetch: typeof fetch;
  setTimeout: typeof setTimeout;
  clearTimeout: typeof clearTimeout;
}

interface DaemonOverrides extends Partial<DaemonConfig> {
  readLegionsRegistry?: typeof readLegionsRegistry;
  allocatePort?: typeof allocatePort;
  writeLegionEntry?: typeof writeLegionEntry;
  removeLegionEntry?: typeof removeLegionEntry;
}

export interface DaemonHandle {
  server: ServerHandle["server"];
  stop: () => Promise<void>;
  config: DaemonConfig;
}

function resolveCodebaseIndexPath(config: DaemonConfig, legionId: string): string {
  if (config.legionDir) {
    return path.join(config.legionDir, ".legion", "daemon", "index.json");
  }

  return path.join(config.paths.forLegion(legionId).legionStateDir, "index.json");
}

interface RuntimeIndexManager {
  initialize: () => Promise<CodebaseIndex>;
  incrementalUpdate: () => Promise<CodebaseIndex>;
  rebuild: () => Promise<CodebaseIndex>;
  getResponse: () => ReturnType<typeof createEmptyCodebaseIndexResponse> | CodebaseIndex;
}

function createNoopIndexManager(): RuntimeIndexManager {
  const emptyIndex: CodebaseIndex = {
    version: CODEBASE_INDEX_VERSION,
    dependencyGraph: {},
    apiSurface: {},
    testMapping: {
      sourceToTests: {},
      testToSources: {},
    },
    hotspots: [],
    metadata: {
      generatedAt: new Date(0).toISOString(),
      rootDir: "",
      fileCount: 0,
      mtimes: {},
    },
  };

  return {
    initialize: async () => emptyIndex,
    incrementalUpdate: async () => emptyIndex,
    rebuild: async () => emptyIndex,
    getResponse: () => createEmptyCodebaseIndexResponse(),
  };
}

function resolveDependencies(
  config: DaemonConfig,
  overrides?: Partial<DaemonDependencies>
): DaemonDependencies {
  const defaultAdapter = createAdapter(config.runtime, {
    port: config.baseWorkerPort,
    shortId: config.legionId ?? "default",
  });
  return {
    adapter: overrides?.adapter ?? defaultAdapter,
    startServer: overrides?.startServer ?? startServer,
    readStateFile: overrides?.readStateFile ?? readStateFile,
    writeStateFile: overrides?.writeStateFile ?? writeStateFile,
    fetch: overrides?.fetch ?? globalThis.fetch,
    setTimeout: overrides?.setTimeout ?? setTimeout,
    clearTimeout: overrides?.clearTimeout ?? clearTimeout,
  };
}

async function sendPromptWithRetry(
  adapter: RuntimeAdapter,
  sessionId: string,
  text: string,
  deps: { setTimeout: typeof globalThis.setTimeout }
): Promise<void> {
  let lastError: Error = new Error("All retry attempts failed");
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      await adapter.sendPrompt(sessionId, text);
      return;
    } catch (error) {
      lastError = error instanceof Error ? error : new Error(String(error));
      if (attempt < 2) {
        await new Promise((resolve) => deps.setTimeout(resolve, 100 * 2 ** attempt));
      }
    }
  }
  throw lastError;
}

function subscribeControllerToEnvoy(sessionId: string) {
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  fetch(`${envoyUrl}/v1/interests/subscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      session_id: sessionId,
      topics: ["notifications.legion.controller", "notifications.slack.*.*.mention"],
    }),
  })
    .then(() => {
      console.log(`Controller subscribed to Envoy: session=${sessionId}`);
    })
    .catch((err) => {
      console.warn(`Envoy subscribe failed (non-fatal): ${err}`);
    });
}

function buildControllerEnv(config: DaemonConfig): Record<string, string> {
  // config.legionId is guaranteed non-empty by the startDaemon guard at line 107-109.
  // The "" fallback is unreachable dead code — kept to satisfy the type checker.
  const legionId = config.legionId ?? "";
  const env: Record<string, string> = {
    LEGION_ID: legionId,
    LEGION_ISSUE_BACKEND: config.issueBackend,
    LEGION_SHORT_ID: legionId.slice(0, 8),
    LEGION_DAEMON_PORT: String(config.daemonPort),
  };
  if (config.controllerSessionId) {
    env.LEGION_CONTROLLER_SESSION_ID = config.controllerSessionId;
  }
  return env;
}

export async function startDaemon(
  overrides: DaemonOverrides = {},
  deps?: Partial<DaemonDependencies>
): Promise<DaemonHandle> {
  const {
    readLegionsRegistry: readLegionsRegistryOverride,
    allocatePort: allocatePortOverride,
    writeLegionEntry: writeLegionEntryOverride,
    removeLegionEntry: removeLegionEntryOverride,
    ...configOverrides
  } = overrides;

  const readLegionsRegistryFn = readLegionsRegistryOverride ?? readLegionsRegistry;
  const allocatePortFn = allocatePortOverride ?? allocatePort;
  const writeLegionEntryFn = writeLegionEntryOverride ?? writeLegionEntry;
  const removeLegionEntryFn = removeLegionEntryOverride ?? removeLegionEntry;

  let config = { ...loadConfig(), ...configOverrides };
  const legionId = config.legionId;
  if (!legionId) {
    throw new Error("Missing legionId for daemon");
  }
  validateControllerPrompt(config.controllerPrompt);
  mkdirSync(config.logDir, { recursive: true });

  const registry = await readLegionsRegistryFn(config.paths.legionsFile);
  const { daemonPort, servePort } = allocatePortFn(registry);

  let actualDaemonPort = daemonPort;
  while (!(await isPortFree(actualDaemonPort))) {
    actualDaemonPort++;
  }

  let actualServePort = servePort;
  while (!(await isPortFree(actualServePort))) {
    actualServePort++;
  }

  config = {
    ...config,
    daemonPort: actualDaemonPort,
    baseWorkerPort: actualServePort,
  };

  const resolvedDeps = resolveDependencies(config, deps);

  const sharedServePort = config.baseWorkerPort;
  let healthTicks = 0;
  let sharedServeRestarts = 0;
  let roleServeRestarts = 0;
  let controllerRecreates = 0;
  let indexInitializations = 0;
  let indexIncrementalUpdates = 0;

  let controllerState: ControllerState | undefined;

  registerSignals();
  startMemoryTelemetry();
  const releaseDaemonGauges = registerGauges("daemon-index", () => ({
    daemon_port: config.daemonPort,
    daemon_shared_serve_port: sharedServePort,
    daemon_health_ticks: healthTicks,
    daemon_shared_restarts: sharedServeRestarts,
    daemon_role_restarts: roleServeRestarts,
    daemon_controller_recreates: controllerRecreates,
    daemon_index_initializations: indexInitializations,
    daemon_index_incrementals: indexIncrementalUpdates,
    daemon_controller_present: controllerState ? 1 : 0,
    daemon_role_serves: roleServeManager?.getEntries().length ?? 0,
  }));
  const controllerWorkspace = config.paths.forLegion(legionId).legionStateDir;
  const hasIndexRoot = !!config.legionDir && existsSync(config.legionDir);
  if (config.legionDir && !hasIndexRoot) {
    console.warn(`Codebase index skipped: LEGION_DIR does not exist (${config.legionDir})`);
  }

  const indexManager: RuntimeIndexManager =
    hasIndexRoot && config.legionDir
      ? new CodebaseIndexManager(config.legionDir, resolveCodebaseIndexPath(config, legionId), {
          warn: (message) => console.warn(message),
        })
      : createNoopIndexManager();

  const existingHealthy = await resolvedDeps.adapter.healthy();
  if (existingHealthy) {
    console.log(`Adopted existing shared serve on port ${sharedServePort}`);
  } else {
    await resolvedDeps.adapter.start({
      env: buildControllerEnv(config),
      workspace: controllerWorkspace,
      logDir: config.logDir,
    });
    console.log(`Shared serve started on port ${sharedServePort}`);
  }

  // Log when shared serve exits but don't auto-restart — it idles out with 0 sessions.
  // The serve will be restarted on-demand when workers are created.
  const adapterAny = resolvedDeps.adapter as { onServeExit?: ((code: number | null) => void) | null };
  if (adapterAny.onServeExit !== undefined) {
    adapterAny.onServeExit = (code) => {
      if (shuttingDown) return;
      console.log(`Shared serve exited (code=${code}), will restart on next worker creation`);
    };
  }

  // Initialize per-role serves for credential isolation (when GitHub Apps configured)
  let roleServeManager: RoleServeManager | undefined;
  let tokenManager: TokenManager | undefined;
  if (config.githubApps) {
    tokenManager = new TokenManager(config.githubApps);
    roleServeManager = new RoleServeManager({
      githubApps: config.githubApps,
      tokenManager,
      runtime: config.runtime,
      basePort: sharedServePort + 1,
      shortId: legionId.slice(0, 8),
      fallbackAdapter: resolvedDeps.adapter,
    });
    try {
      await roleServeManager.start(buildControllerEnv(config), controllerWorkspace, config.logDir);
      const entries = roleServeManager.getEntries();
      console.log(`Role serves started: ${entries.map((e) => `${e.role}:${e.port}`).join(", ")}`);
    } catch (error) {
      console.error(`Failed to start role serves: ${error}`);
      console.error("Falling back to shared serve for all workers");
      roleServeManager = undefined;
    }
  }

  const preState = await resolvedDeps.readStateFile(config.stateFilePath);
  controllerState = preState.controller;

  // Prune dead/stopped workers from state file on startup
  const activeWorkers: Record<string, typeof preState.workers[string]> = {};
  let pruned = 0;
  for (const [id, entry] of Object.entries(preState.workers)) {
    if (entry.status === "dead" || entry.status === "stopped") {
      pruned++;
      continue;
    }
    activeWorkers[id] = entry;
  }
  if (pruned > 0) {
    console.log(`Pruned ${pruned} dead/stopped workers from state file`);
    await resolvedDeps.writeStateFile(config.stateFilePath, {
      ...preState,
      workers: activeWorkers,
    });
  }

  // Recreate sessions for active workers in parallel (batched)
  const workerEntries = Object.values(activeWorkers);
  if (workerEntries.length > 0) {
    console.log(`Recreating ${workerEntries.length} active worker sessions...`);
    const BATCH_SIZE = 10;
    for (let i = 0; i < workerEntries.length; i += BATCH_SIZE) {
      const batch = workerEntries.slice(i, i + BATCH_SIZE);
      await Promise.allSettled(
        batch.map(async (entry) => {
          try {
            const actualId = await resolvedDeps.adapter.createSession(entry.sessionId, entry.workspace);
            if (actualId !== entry.sessionId) {
              console.warn(`Worker ${entry.id}: session ID changed ${entry.sessionId} -> ${actualId}`);
            }
          } catch (error) {
            console.error(`Failed to re-create session for ${entry.id}: ${error}`);
          }
        })
      );
    }
  }

  if (hasIndexRoot) {
    const startedIndexBuildAt = Date.now();
    await indexManager.initialize();
    indexInitializations += 1;
    console.log(`Codebase index ready in ${Date.now() - startedIndexBuildAt}ms`);
  }

  let shuttingDown = false;
  let healthTickTimeout: ReturnType<typeof setTimeout> | null = null;
  let stopServer: () => void = () => {};

  const shutdown = async (exitAfter = false) => {
    if (shuttingDown) {
      return;
    }
    shuttingDown = true;

    if (healthTickTimeout) {
      resolvedDeps.clearTimeout(healthTickTimeout);
      healthTickTimeout = null;
    }

    try {
      await removeLegionEntryFn(config.paths.legionsFile, legionId);
    } catch {}

    await resolvedDeps.adapter.stop();
    if (roleServeManager) {
      await roleServeManager.stop();
    }

    controllerState = undefined;
    const state = await resolvedDeps.readStateFile(config.stateFilePath);
    await resolvedDeps.writeStateFile(config.stateFilePath, {
      workers: state.workers,
      crashHistory: state.crashHistory,
      controller: undefined,
    });
    releaseDaemonGauges();
    stopMemoryTelemetry();
    clearInterval(keepalive);
    stopServer();
    if (exitAfter) {
      process.exit(0);
    }
  };

  let server: ServerHandle["server"];
  let stop: ServerHandle["stop"];
  let bindAttempts = 0;

  while (true) {
    try {
      const serverHandle = resolvedDeps.startServer({
        port: config.daemonPort,
        hostname: "127.0.0.1",
        legionId,
        projectId: legionId,
        legionDir: config.legionDir,
        paths: config.paths,
        adapter: resolvedDeps.adapter,
        stateFilePath: config.stateFilePath,
        logDir: config.logDir,
        runtime: config.runtime,
        tmuxSession:
          config.runtime === "claude-code" && config.legionId
            ? `legion-${config.legionId}`
            : undefined,
        getControllerState: () => controllerState,
        getWorkerAdapter: roleServeManager
          ? (mode) => roleServeManager.getAdapterForMode(mode)
          : undefined,
        tokenManager,
        indexManager,
        shutdownFn: async () => {
          resolvedDeps.setTimeout(async () => {
            await shutdown(true);
          }, 100);
        },
      });
      server = serverHandle.server;
      stop = serverHandle.stop;
      break;
    } catch (error) {
      const err = error as NodeJS.ErrnoException;
      bindAttempts++;
      if (err.code !== "EADDRINUSE" || bindAttempts >= 5) {
        throw error;
      }
      config = { ...config, daemonPort: config.daemonPort + 1 };
    }
  }

  stopServer = stop;

  await writeLegionEntryFn(config.paths.legionsFile, legionId, {
    port: config.daemonPort,
    servePort: sharedServePort,
    pid: process.pid,
    startedAt: new Date().toISOString(),
  });

  const existingController = controllerState;

  if (config.controllerSessionId) {
    if (existingController && existingController.sessionId !== config.controllerSessionId) {
      if (existingController.port) {
        const oldAlive = await resolvedDeps.adapter.healthy();
        if (oldAlive) {
          throw new Error(
            `Another controller is running (session=${existingController.sessionId})`
          );
        }
      }
    }
    console.log(`External controller: session=${config.controllerSessionId}`);
    controllerState = { sessionId: config.controllerSessionId };
    subscribeControllerToEnvoy(config.controllerSessionId);
  } else {
    const requestedSessionId = computeControllerSessionId(legionId);
    let actualSessionId: string | undefined;
    try {
      actualSessionId = await resolvedDeps.adapter.createSession(
        requestedSessionId,
        controllerWorkspace
      );
      controllerState = { sessionId: actualSessionId, port: sharedServePort };
    } catch (error) {
      console.error(`Failed to create controller session: ${error}`);
    }

    if (controllerState && actualSessionId) {
      const initialPrompt = config.controllerPrompt
        ? `/legion-controller\n\n${config.controllerPrompt}`
        : "/legion-controller";
      try {
        await sendPromptWithRetry(
          resolvedDeps.adapter,
          actualSessionId,
          initialPrompt,
          resolvedDeps
        );
        console.log(`Controller started: session=${actualSessionId} port=${sharedServePort}`);
        subscribeControllerToEnvoy(actualSessionId);
      } catch (error) {
        console.error(`Controller session created but prompt failed: ${error}`);
        console.error("Health loop will retry on next tick.");
      }
    }
  }

  const scheduleHealthTick = () => {
    healthTickTimeout = resolvedDeps.setTimeout(async () => {
      try {
        healthTicks += 1;
        const serveHealthy = await resolvedDeps.adapter.healthy();

        if (!serveHealthy) {
          console.error("Shared serve is unhealthy, attempting restart...");

          try {
            await resolvedDeps.adapter.start({
              env: buildControllerEnv(config),
              workspace: controllerWorkspace,
              logDir: config.logDir,
            });
            sharedServeRestarts += 1;
            console.log(`Shared serve restarted on port ${sharedServePort}`);

            const state = await resolvedDeps.readStateFile(config.stateFilePath);
            const liveWorkers = Object.values(state.workers).filter(
              (e) => e.status !== "dead" && e.status !== "stopped"
            );
            if (liveWorkers.length > 0) {
              console.log(`Recreating ${liveWorkers.length} active worker sessions after serve restart...`);
              const BATCH_SIZE = 10;
              for (let i = 0; i < liveWorkers.length; i += BATCH_SIZE) {
                const batch = liveWorkers.slice(i, i + BATCH_SIZE);
                await Promise.allSettled(
                  batch.map(async (entry) => {
                    try {
                      const actualId = await resolvedDeps.adapter.createSession(
                        entry.sessionId,
                        entry.workspace
                      );
                      if (actualId !== entry.sessionId) {
                        console.warn(
                          `Worker ${entry.id}: session ID changed ${entry.sessionId} -> ${actualId}`
                        );
                      }
                    } catch {
                      // Best-effort session re-creation
                    }
                  })
                );
              }
            }

            if (controllerState?.port) {
              try {
                const actualControllerSessionId = await resolvedDeps.adapter.createSession(
                  controllerState.sessionId,
                  controllerWorkspace
                );
                if (actualControllerSessionId !== controllerState.sessionId) {
                  console.warn(
                    `Controller: session ID changed ${controllerState.sessionId} -> ${actualControllerSessionId}`
                  );
                }
                controllerState = {
                  ...controllerState,
                  sessionId: actualControllerSessionId,
                  port: sharedServePort,
                };
                controllerRecreates += 1;
                await sendPromptWithRetry(
                  resolvedDeps.adapter,
                  actualControllerSessionId,
                  "/legion-controller",
                  resolvedDeps
                );
                console.log(`Controller re-created: session=${actualControllerSessionId}`);
              } catch (error) {
                console.error(`Failed to re-create controller session: ${error}`);
              }
            }
          } catch (error) {
            console.error(`Failed to restart shared serve: ${error}`);
          }
        }

        try {
          await indexManager.incrementalUpdate();
          indexIncrementalUpdates += 1;
        } catch (error) {
          console.error(`Failed to update codebase index incrementally: ${error}`);
        }

        // Check role serves health
        if (roleServeManager?.hasRoleServes()) {
          const unhealthyRoles = await roleServeManager.checkHealth();
          for (const role of unhealthyRoles) {
            console.error(`Role serve '${role}' is unhealthy, restarting...`);
            try {
              await roleServeManager.restartRole(
                role,
                buildControllerEnv(config),
                controllerWorkspace,
                config.logDir
              );
              roleServeRestarts += 1;
              console.log(`Role serve '${role}' restarted`);
              // Re-create sessions for workers on this role
              const roleAdapter = roleServeManager.getAdapterForRole(role);
              const state = await resolvedDeps.readStateFile(config.stateFilePath);
              for (const workerEntry of Object.values(state.workers)) {
                try {
                  const workerMode = workerEntry.id.split("-").pop();
                  if (workerMode && modeToRole(workerMode) === role) {
                    await roleAdapter.createSession(workerEntry.sessionId, workerEntry.workspace);
                  }
                } catch {
                  // Best effort — worker may not match this role
                }
              }
            } catch (restartError) {
              console.error(`Failed to restart role serve '${role}': ${restartError}`);
            }
          }
        }
      } finally {
        if (!shuttingDown) {
          scheduleHealthTick();
        }
      }
    }, config.checkIntervalMs);
  };
  // Keepalive: prevent Bun's event loop from draining when the shared serve child exits.
  // Bun.spawn links child lifecycle to parent — without this, the daemon exits when the serve does.
  const keepalive = setInterval(() => {}, 2_147_483_647); // max 32-bit int

  scheduleHealthTick();

  const handleSignal = async () => {
    await shutdown(true);
  };

  process.on("SIGTERM", handleSignal);
  process.on("SIGINT", handleSignal);

  return {
    server,
    stop: () => shutdown(false),
    config,
  };
}

if (import.meta.main) {
  process.on("uncaughtException", (error) => {
    console.error("[daemon] uncaught exception:", error);
  });
  process.on("unhandledRejection", (reason) => {
    console.error("[daemon] unhandled rejection:", reason);
  });
  void startDaemon();
}
