import { mkdirSync } from "node:fs";
import { computeControllerSessionId } from "../state/types";
import { type DaemonConfig, loadConfig } from "./config";
import {
  createSession,
  createWorkerClient,
  healthCheck,
  type SharedServeOptions,
  type SharedServeState,
  spawnSharedServe,
  stopServe,
  waitForHealthy,
} from "./serve-manager";
import { type ServeManagerInterface, startServer } from "./server";
import { type ControllerState, readStateFile, writeStateFile } from "./state-file";

type ServerHandle = ReturnType<typeof startServer>;

interface DaemonServeManager extends ServeManagerInterface {
  spawnSharedServe(opts: SharedServeOptions): Promise<SharedServeState>;
  waitForHealthy(port: number, maxRetries?: number, delayMs?: number): Promise<void>;
  stopServe(
    port: number,
    pid: number,
    waitTimeoutMs?: number,
    pollIntervalMs?: number,
    disposeTimeoutMs?: number
  ): Promise<void>;
}

interface DaemonDependencies {
  serveManager: DaemonServeManager;
  startServer: typeof startServer;
  readStateFile: typeof readStateFile;
  writeStateFile: typeof writeStateFile;
  fetch: typeof fetch;
  setTimeout: typeof setTimeout;
  clearTimeout: typeof clearTimeout;
}

export interface DaemonHandle {
  server: ServerHandle["server"];
  stop: () => Promise<void>;
  config: DaemonConfig;
}

function resolveDependencies(
  _config: DaemonConfig,
  overrides?: Partial<DaemonDependencies>
): DaemonDependencies {
  return {
    serveManager: overrides?.serveManager ?? {
      spawnSharedServe,
      waitForHealthy,
      createSession,
      healthCheck,
      stopServe,
    },
    startServer: overrides?.startServer ?? startServer,
    readStateFile: overrides?.readStateFile ?? readStateFile,
    writeStateFile: overrides?.writeStateFile ?? writeStateFile,
    fetch: overrides?.fetch ?? globalThis.fetch,
    setTimeout: overrides?.setTimeout ?? setTimeout,
    clearTimeout: overrides?.clearTimeout ?? clearTimeout,
  };
}

export async function startDaemon(
  overrides: Partial<DaemonConfig> = {},
  deps?: Partial<DaemonDependencies>
): Promise<DaemonHandle> {
  const config = { ...loadConfig(), ...overrides };
  if (!config.teamId) {
    throw new Error("Missing teamId for daemon");
  }
  mkdirSync(config.logDir, { recursive: true });
  const resolvedDeps = resolveDependencies(config, deps);

  const sharedServePort = config.baseWorkerPort;
  let sharedServePid = 0;

  const existingHealthy = await resolvedDeps.serveManager.healthCheck(sharedServePort);
  if (existingHealthy) {
    console.log(`Adopted existing shared serve on port ${sharedServePort}`);
  } else {
    const serve = await resolvedDeps.serveManager.spawnSharedServe({
      port: sharedServePort,
      workspace: config.legionDir ?? "",
      logDir: config.logDir,
    });
    sharedServePid = serve.pid;
    await resolvedDeps.serveManager.waitForHealthy(sharedServePort);
    console.log(`Shared serve started on port ${sharedServePort} pid=${sharedServePid}`);
  }

  const preState = await resolvedDeps.readStateFile(config.stateFilePath);
  let controllerState: ControllerState | undefined = preState.controller;

  for (const entry of Object.values(preState.workers)) {
    try {
      await resolvedDeps.serveManager.createSession(
        sharedServePort,
        entry.sessionId,
        entry.workspace
      );
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

    if (sharedServePid > 0) {
      await resolvedDeps.serveManager.stopServe(sharedServePort, sharedServePid);
    }

    controllerState = undefined;
    const state = await resolvedDeps.readStateFile(config.stateFilePath);
    await resolvedDeps.writeStateFile(config.stateFilePath, {
      workers: {},
      crashHistory: state.crashHistory,
      controller: undefined,
    });
    stopServer();
    if (exitAfter) {
      process.exit(0);
    }
  };

  const { server, stop } = resolvedDeps.startServer({
    port: config.daemonPort,
    hostname: "127.0.0.1",
    teamId: config.teamId,
    legionDir: config.legionDir ?? "",
    shortId: config.shortId ?? "default",
    serveManager: resolvedDeps.serveManager,
    sharedServePort,
    stateFilePath: config.stateFilePath,
    logDir: config.logDir,
    getControllerState: () => controllerState,
    shutdownFn: async () => {
      resolvedDeps.setTimeout(async () => {
        await shutdown(true);
      }, 100);
    },
  });
  stopServer = stop;

  const existingController = controllerState;

  if (config.controllerSessionId) {
    if (existingController && existingController.sessionId !== config.controllerSessionId) {
      if (existingController.port) {
        const oldAlive = await resolvedDeps.serveManager.healthCheck(existingController.port);
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
    const sessionId = computeControllerSessionId(config.teamId!);
    try {
      await resolvedDeps.serveManager.createSession(
        sharedServePort,
        sessionId,
        config.legionDir ?? ""
      );
      const client = createWorkerClient(sharedServePort, config.legionDir ?? "");
      await client.session.promptAsync({
        sessionID: sessionId,
        parts: [{ type: "text", text: "/legion-controller" }],
      });
      controllerState = { sessionId, port: sharedServePort };
      console.log(`Controller started: session=${sessionId} port=${sharedServePort}`);
    } catch (error) {
      console.error(`Failed to start controller: ${error}`);
    }
  }

  const scheduleHealthTick = () => {
    healthTickTimeout = resolvedDeps.setTimeout(async () => {
      try {
        const serveHealthy = await resolvedDeps.serveManager.healthCheck(sharedServePort);

        if (!serveHealthy) {
          console.error("Shared serve is unhealthy, attempting restart...");

          try {
            const serve = await resolvedDeps.serveManager.spawnSharedServe({
              port: sharedServePort,
              workspace: config.legionDir ?? "",
              logDir: config.logDir,
            });
            sharedServePid = serve.pid;
            await resolvedDeps.serveManager.waitForHealthy(sharedServePort);
            console.log(`Shared serve restarted on port ${sharedServePort}`);

            const state = await resolvedDeps.readStateFile(config.stateFilePath);
            for (const entry of Object.values(state.workers)) {
              try {
                await resolvedDeps.serveManager.createSession(
                  sharedServePort,
                  entry.sessionId,
                  entry.workspace
                );
              } catch {
                // Best-effort session re-creation
              }
            }

            if (controllerState?.port) {
              try {
                await resolvedDeps.serveManager.createSession(
                  sharedServePort,
                  controllerState.sessionId,
                  config.legionDir ?? ""
                );
                const client = createWorkerClient(sharedServePort, config.legionDir ?? "");
                await client.session.promptAsync({
                  sessionID: controllerState.sessionId,
                  parts: [{ type: "text", text: "/legion-controller" }],
                });
                console.log(`Controller re-created: session=${controllerState.sessionId}`);
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
