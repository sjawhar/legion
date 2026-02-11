import { mkdirSync } from "node:fs";
import { type DaemonConfig, loadConfig } from "./config";
import { PortAllocator } from "./ports";
import {
  adoptExistingWorkers,
  healthCheck,
  killWorker,
  spawnServe,
  type WorkerEntry,
} from "./serve-manager";
import { type PortAllocatorInterface, type ServeManagerInterface, startServer } from "./server";
import {
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
  crashHistory: Record<string, CrashHistoryEntry>
): PersistedWorkerState {
  const state: Record<string, WorkerEntry> = {};
  for (const [id, entry] of entries.entries()) {
    state[id] = entry;
  }
  return { workers: state, crashHistory };
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
    serveManager: overrides?.serveManager ?? { spawnServe, killWorker, healthCheck },
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

  const adopted = await resolvedDeps.adoptExistingWorkers(config.stateFilePath);
  await resolvedDeps.writeStateFile(
    config.stateFilePath,
    mapToState(adopted.workers, adopted.crashHistory)
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

    await resolvedDeps.writeStateFile(config.stateFilePath, {
      workers: {},
      crashHistory: state.crashHistory,
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
    serveManager: resolvedDeps.serveManager,
    portAllocator: resolvedDeps.portAllocator,
    stateFilePath: config.stateFilePath,
    logDir: config.logDir,
    shutdownFn: async () => {
      // Shutdown asynchronously after response is sent
      setTimeout(async () => {
        await shutdown(true);
      }, 100);
    },
  });
  stopServer = stop;

  const baseUrl = `http://127.0.0.1:${server.port}`;
  try {
    const controllerRes = await resolvedDeps.fetch(`${baseUrl}/workers`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        issueId: "controller",
        mode: "controller",
        workspace: config.legionDir,
        env: {
          LINEAR_TEAM_ID: config.teamId,
          LEGION_DIR: config.legionDir,
          LEGION_DAEMON_PORT: String(server.port),
          LEGION_SHORT_ID: config.shortId ?? "default",
        },
      }),
    });
    if (controllerRes.ok) {
      const data = (await controllerRes.json()) as { port: number; sessionId: string };
      await resolvedDeps.fetch(
        `http://127.0.0.1:${data.port}/session/${data.sessionId}/prompt_async`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            parts: [{ type: "text", text: "/legion-controller" }],
          }),
        }
      );
      console.log(`Controller started: session=${data.sessionId} port=${data.port}`);
    }
  } catch (error) {
    console.error(`Failed to spawn controller: ${error}`);
  }
  const scheduleHealthTick = () => {
    healthTickTimeout = resolvedDeps.setTimeout(async () => {
      try {
        await healthTick(baseUrl, resolvedDeps.serveManager, resolvedDeps.fetch);
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
