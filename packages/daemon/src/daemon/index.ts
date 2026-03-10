import { mkdirSync } from "node:fs";
import { computeControllerSessionId } from "../state/types";
import { type DaemonConfig, loadConfig, validateControllerPrompt } from "./config";
import {
  allocatePort,
  readLegionsRegistry,
  removeLegionEntry,
  writeLegionEntry,
} from "./legions-registry";
import { isPortFree } from "./ports";
import { createAdapter } from "./runtime";
import type { RuntimeAdapter } from "./runtime/types";
import { startServer } from "./server";
import { type ControllerState, readStateFile, writeStateFile } from "./state-file";

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

function buildControllerEnv(config: DaemonConfig): Record<string, string> {
  // config.legionId is guaranteed non-empty by the startDaemon guard at line 107-109.
  // The "" fallback is unreachable dead code — kept to satisfy the type checker.
  const legionId = config.legionId ?? "";
  return {
    LEGION_ID: legionId,
    LEGION_ISSUE_BACKEND: config.issueBackend,
    LEGION_SHORT_ID: legionId.slice(0, 8),
    LEGION_DAEMON_PORT: String(config.daemonPort),
  };
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
  const controllerWorkspace = config.paths.forLegion(legionId).legionStateDir;

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

  const preState = await resolvedDeps.readStateFile(config.stateFilePath);
  let controllerState: ControllerState | undefined = preState.controller;

  for (const entry of Object.values(preState.workers)) {
    try {
      const actualId = await resolvedDeps.adapter.createSession(entry.sessionId, entry.workspace);
      if (actualId !== entry.sessionId) {
        console.warn(`Worker ${entry.id}: session ID changed ${entry.sessionId} -> ${actualId}`);
      }
    } catch (error) {
      console.error(`Failed to re-create session for ${entry.id}: ${error}`);
    }
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

    controllerState = undefined;
    const state = await resolvedDeps.readStateFile(config.stateFilePath);
    await resolvedDeps.writeStateFile(config.stateFilePath, {
      workers: state.workers,
      crashHistory: state.crashHistory,
      controller: undefined,
    });
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
      } catch (error) {
        console.error(`Controller session created but prompt failed: ${error}`);
        console.error("Health loop will retry on next tick.");
      }
    }
  }

  const scheduleHealthTick = () => {
    healthTickTimeout = resolvedDeps.setTimeout(async () => {
      try {
        const serveHealthy = await resolvedDeps.adapter.healthy();

        if (!serveHealthy) {
          console.error("Shared serve is unhealthy, attempting restart...");

          try {
            await resolvedDeps.adapter.start({
              env: buildControllerEnv(config),
              workspace: controllerWorkspace,
              logDir: config.logDir,
            });
            console.log(`Shared serve restarted on port ${sharedServePort}`);

            const state = await resolvedDeps.readStateFile(config.stateFilePath);
            for (const entry of Object.values(state.workers)) {
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
      } finally {
        if (!shuttingDown) {
          scheduleHealthTick();
        }
      }
    }, config.checkIntervalMs);
  };
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
  void startDaemon();
}
