import { mkdirSync } from "node:fs";
import { computeControllerSessionId } from "../state/types";
import { type DaemonConfig, loadConfig } from "./config";
import { PortAllocator } from "./ports";
import {
  adoptExistingWorkers,
  createWorkerClient,
  healthCheck,
  initializeSession,
  killWorker,
  spawnServe,
  type WorkerEntry,
} from "./serve-manager";
import { type PortAllocatorInterface, type ServeManagerInterface, startServer } from "./server";
import {
  type ControllerState,
  type CrashHistoryEntry,
  type PersistedWorkerState,
  readStateFile,
  writeStateFile,
} from "./state-file";

type ServerHandle = ReturnType<typeof startServer>;

interface DaemonDependencies {
  serveManager: ServeManagerInterface;
  startServer: typeof startServer;
  portAllocator: PortAllocatorInterface;
  adoptExistingWorkers: typeof adoptExistingWorkers;
  readStateFile: typeof readStateFile;
  writeStateFile: typeof writeStateFile;
  fetch: typeof fetch;
  setInterval: typeof setInterval;
  clearInterval: typeof clearInterval;
  setTimeout: typeof setTimeout;
  clearTimeout: typeof clearTimeout;
}

export interface DaemonHandle {
  server: ServerHandle["server"];
  stop: () => Promise<void>;
  config: DaemonConfig;
}

function mapToState(
  entries: Map<string, WorkerEntry>,
  crashHistory: Record<string, CrashHistoryEntry>,
  controller?: ControllerState
): PersistedWorkerState {
  const state: Record<string, WorkerEntry> = {};
  for (const [id, entry] of entries.entries()) {
    state[id] = entry;
  }
  return { workers: state, crashHistory, controller };
}

function seedAllocator(allocator: PortAllocatorInterface, entries: Iterable<WorkerEntry>): void {
  const ports = Array.from(entries, (entry) => entry.port);
  if (ports.length === 0) {
    return;
  }
  const uniquePorts = Array.from(new Set(ports)).sort((a, b) => a - b);
  const desired = new Set(uniquePorts);
  const allocated: number[] = [];

  for (const port of uniquePorts) {
    let allocatedPort = allocator.allocate();
    allocated.push(allocatedPort);
    while (allocatedPort < port) {
      allocatedPort = allocator.allocate();
      allocated.push(allocatedPort);
    }
  }

  for (const port of allocated) {
    if (!desired.has(port)) {
      allocator.release(port);
    }
  }
}

async function fetchWorkers(baseUrl: string, fetchFn: typeof fetch): Promise<WorkerEntry[]> {
  const response = await fetchFn(`${baseUrl}/workers`);
  if (!response.ok) {
    return [];
  }
  return (await response.json()) as WorkerEntry[];
}

async function healthTick(
  baseUrl: string,
  serveManager: ServeManagerInterface,
  fetchFn: typeof fetch
): Promise<void> {
  let workers: WorkerEntry[] = [];
  try {
    workers = await fetchWorkers(baseUrl, fetchFn);
  } catch {
    return;
  }

  await Promise.all(
    workers.map(async (entry) => {
      try {
        const healthy = await serveManager.healthCheck(entry.port);
        if (healthy) {
          await fetchFn(`${baseUrl}/workers/${entry.id}`, {
            method: "PATCH",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({ status: "running" }),
          });
          return;
        }

        entry.crashCount = (entry.crashCount ?? 0) + 1;
        entry.lastCrashAt = new Date().toISOString();
        entry.status = "dead";

        await fetchFn(`${baseUrl}/workers/${entry.id}`, {
          method: "PATCH",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            status: "dead",
            crashCount: entry.crashCount,
            lastCrashAt: entry.lastCrashAt,
          }),
        });
        await fetchFn(`${baseUrl}/workers/${entry.id}`, { method: "DELETE" });
      } catch {
        return;
      }
    })
  );
}

function resolveDependencies(
  config: DaemonConfig,
  overrides?: Partial<DaemonDependencies>
): DaemonDependencies {
  return {
    serveManager: overrides?.serveManager ?? {
      spawnServe,
      initializeSession,
      killWorker,
      healthCheck,
    },
    startServer: overrides?.startServer ?? startServer,
    portAllocator: overrides?.portAllocator ?? new PortAllocator(config.baseWorkerPort),
    adoptExistingWorkers: overrides?.adoptExistingWorkers ?? adoptExistingWorkers,
    readStateFile: overrides?.readStateFile ?? readStateFile,
    writeStateFile: overrides?.writeStateFile ?? writeStateFile,
    fetch: overrides?.fetch ?? globalThis.fetch,
    setInterval: overrides?.setInterval ?? setInterval,
    clearInterval: overrides?.clearInterval ?? clearInterval,
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

  // Read existing state to preserve controller across the initial write
  const preState = await resolvedDeps.readStateFile(config.stateFilePath);
  let controllerState: ControllerState | undefined = preState.controller;

  const adopted = await resolvedDeps.adoptExistingWorkers(config.stateFilePath);
  await resolvedDeps.writeStateFile(
    config.stateFilePath,
    mapToState(adopted.workers, adopted.crashHistory, controllerState)
  );
  seedAllocator(resolvedDeps.portAllocator, adopted.workers.values());

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

    const state = await resolvedDeps.readStateFile(config.stateFilePath);
    const entries = Object.values(state.workers);
    await Promise.allSettled(
      entries.map(async (entry) => resolvedDeps.serveManager.killWorker(entry))
    );
    for (const entry of entries) {
      resolvedDeps.portAllocator.release(entry.port);
    }

    if (controllerProcess) {
      await resolvedDeps.serveManager.killWorker(controllerProcess);
      if (controllerProcess.port) {
        resolvedDeps.portAllocator.release(controllerProcess.port);
      }
    }

    controllerState = undefined;
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
    portAllocator: resolvedDeps.portAllocator,
    stateFilePath: config.stateFilePath,
    logDir: config.logDir,
    getControllerState: () => controllerState,
    shutdownFn: async () => {
      // Shutdown asynchronously after response is sent
      setTimeout(async () => {
        await shutdown(true);
      }, 100);
    },
  });
  stopServer = stop;

  const baseUrl = `http://127.0.0.1:${server.port}`;
  let controllerProcess: WorkerEntry | null = null;
  const existingController = controllerState;

  if (config.controllerSessionId) {
    if (existingController && existingController.sessionId !== config.controllerSessionId) {
      if (existingController.port) {
        const oldAlive = await resolvedDeps.serveManager.healthCheck(existingController.port);
        if (oldAlive) {
          throw new Error(
            `Another controller is running (session=${existingController.sessionId}, port=${existingController.port})`
          );
        }
      }
    }
    console.log(`External controller: session=${config.controllerSessionId}`);
    controllerState = { sessionId: config.controllerSessionId };
    const extState = await resolvedDeps.readStateFile(config.stateFilePath);
    await resolvedDeps.writeStateFile(config.stateFilePath, {
      ...extState,
      controller: controllerState,
    });
  } else {
    if (existingController?.port) {
      const alive = await resolvedDeps.serveManager.healthCheck(existingController.port);
      if (alive && existingController.pid) {
        console.log(`Adopted existing controller: session=${existingController.sessionId}`);
        controllerProcess = {
          id: "controller",
          port: existingController.port,
          pid: existingController.pid,
          sessionId: existingController.sessionId,
          workspace: config.legionDir ?? process.cwd(),
          startedAt: new Date().toISOString(),
          status: "running",
          crashCount: 0,
          lastCrashAt: null,
        };
      }
    }
    if (!controllerProcess) {
      const port = resolvedDeps.portAllocator.allocate();
      try {
        const sessionId = computeControllerSessionId(config.teamId!);
        controllerProcess = await resolvedDeps.serveManager.spawnServe({
          issueId: "controller",
          mode: "controller",
          workspace: config.legionDir ?? "",
          port,
          sessionId,
          logDir: config.logDir,
          env: {
            LINEAR_TEAM_ID: config.teamId!,
            LEGION_DIR: config.legionDir ?? "",
            LEGION_SHORT_ID: config.shortId ?? "default",
            LEGION_DAEMON_PORT: String(server.port),
          },
        });
        const controllerWorkspace = config.legionDir ?? process.cwd();
        await resolvedDeps.serveManager.initializeSession(port, sessionId, controllerWorkspace);
        controllerProcess = { ...controllerProcess, status: "running" };
        try {
          const client = createWorkerClient(port, controllerWorkspace);
          await client.session.promptAsync({
            sessionID: sessionId,
            parts: [{ type: "text", text: "/legion-controller" }],
          });
        } catch (error) {
          console.error(`Failed to prompt controller: ${error}`);
        }
        console.log(`Controller started: session=${sessionId} port=${port}`);
      } catch (error) {
        resolvedDeps.portAllocator.release(port);
        console.error(`Failed to spawn controller: ${error}`);
      }
    }
    if (controllerProcess) {
      controllerState = {
        sessionId: controllerProcess.sessionId,
        port: controllerProcess.port,
        pid: controllerProcess.pid,
      };
      const intState = await resolvedDeps.readStateFile(config.stateFilePath);
      await resolvedDeps.writeStateFile(config.stateFilePath, {
        ...intState,
        controller: controllerState,
      });
    }
  }
  const scheduleHealthTick = () => {
    healthTickTimeout = resolvedDeps.setTimeout(async () => {
      try {
        await healthTick(baseUrl, resolvedDeps.serveManager, resolvedDeps.fetch);
        if (controllerProcess?.port) {
          const alive = await resolvedDeps.serveManager.healthCheck(controllerProcess.port);
          if (!alive) {
            console.error(
              `Controller died (session=${controllerProcess.sessionId}, port=${controllerProcess.port}). Daemon continues headless.`
            );
            resolvedDeps.portAllocator.release(controllerProcess.port);
            controllerProcess = null;
            controllerState = undefined;
            const state = await resolvedDeps.readStateFile(config.stateFilePath);
            await resolvedDeps.writeStateFile(config.stateFilePath, {
              ...state,
              controller: undefined,
            });
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
