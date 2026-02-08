import { loadConfig, type DaemonConfig } from "./config";
import {
  adoptExistingWorkers,
  healthCheck,
  killWorker,
  spawnServe,
  type WorkerEntry,
} from "./serve-manager";
import {
  startServer,
  type PortAllocatorInterface,
  type ServeManagerInterface,
} from "./server";
import { PortAllocator } from "./ports";
import { readStateFile, writeStateFile, type WorkerState } from "./state-file";

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
}

export interface DaemonHandle {
  server: ServerHandle["server"];
  stop: () => Promise<void>;
  config: DaemonConfig;
}

function mapToState(entries: Map<string, WorkerEntry>): WorkerState {
  const state: WorkerState = {};
  for (const [id, entry] of entries.entries()) {
    state[id] = entry;
  }
  return state;
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

async function fetchWorkers(
  baseUrl: string,
  fetchFn: typeof fetch
): Promise<WorkerEntry[]> {
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
        if (!healthy) {
          await fetchFn(`${baseUrl}/workers/${entry.id}`, { method: "DELETE" });
        }
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
  };
}

export async function startDaemon(
  overrides: Partial<DaemonConfig> = {},
  deps?: Partial<DaemonDependencies>
): Promise<DaemonHandle> {
  const config = { ...loadConfig(), ...overrides };
  const resolvedDeps = resolveDependencies(config, deps);

  const adopted = await resolvedDeps.adoptExistingWorkers(config.stateFilePath);
  await resolvedDeps.writeStateFile(config.stateFilePath, mapToState(adopted));
  seedAllocator(resolvedDeps.portAllocator, adopted.values());

  const { server, stop } = resolvedDeps.startServer({
    port: config.daemonPort,
    hostname: "127.0.0.1",
    serveManager: resolvedDeps.serveManager,
    portAllocator: resolvedDeps.portAllocator,
    stateFilePath: config.stateFilePath,
  });

  const baseUrl = `http://127.0.0.1:${server.port}`;
  const intervalId = resolvedDeps.setInterval(
    () => healthTick(baseUrl, resolvedDeps.serveManager, resolvedDeps.fetch),
    config.checkIntervalMs
  );

  let shuttingDown = false;
  const shutdown = async (exitAfter = false) => {
    if (shuttingDown) {
      return;
    }
    shuttingDown = true;
    resolvedDeps.clearInterval(intervalId);

    const state = await resolvedDeps.readStateFile(config.stateFilePath);
    const entries = Object.values(state);
    await Promise.allSettled(entries.map(async (entry) => resolvedDeps.serveManager.killWorker(entry)));
    for (const entry of entries) {
      resolvedDeps.portAllocator.release(entry.port);
    }

    await resolvedDeps.writeStateFile(config.stateFilePath, {});
    stop();
    if (exitAfter) {
      process.exit(0);
    }
  };

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
