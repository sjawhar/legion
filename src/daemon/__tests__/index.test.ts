import { afterEach, describe, expect, it } from "bun:test";
import { PortAllocator } from "../ports";
import type { WorkerEntry } from "../serve-manager";
import type { WorkerState } from "../state-file";
import { startDaemon } from "../index";

type IntervalCallback = (...args: any[]) => void;

const baseEntry: WorkerEntry = {
  id: "ENG-1-implement",
  port: 15000,
  pid: 2222,
  sessionId: "ses-1",
  startedAt: "2026-02-01T00:00:00.000Z",
  status: "running",
};

const secondEntry: WorkerEntry = {
  id: "ENG-2-plan",
  port: 15002,
  pid: 3333,
  sessionId: "ses-2",
  startedAt: "2026-02-01T01:00:00.000Z",
  status: "running",
};

describe("daemon entry", () => {
  const originalOn = process.on;
  const originalExit = process.exit;
  const originalFetch = globalThis.fetch;

  afterEach(() => {
    process.on = originalOn;
    process.exit = originalExit;
    globalThis.fetch = originalFetch;
  });

  it("adopts existing workers and seeds the port allocator", async () => {
    const adoptExistingWorkers = async () =>
      new Map<string, WorkerEntry>([
        [baseEntry.id, baseEntry],
        [secondEntry.id, secondEntry],
      ]);

    let writtenState: WorkerState | null = null;
    const writeStateFile = async (_path: string, state: WorkerState) => {
      writtenState = state;
    };

    const startServer = () => ({
      server: { port: 15555 } as ReturnType<typeof Bun.serve>,
      stop: () => {},
    });

    const allocator = new PortAllocator(15000, 5);

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
      },
      {
        adoptExistingWorkers,
        writeStateFile,
        readStateFile: async () => ({}),
        serveManager: {
          spawnServe: async () => baseEntry,
          killWorker: async () => {},
          healthCheck: async () => true,
        },
        startServer,
        portAllocator: allocator,
        setInterval: () => 1 as unknown as ReturnType<typeof globalThis.setInterval>,
        clearInterval: () => {},
        fetch: originalFetch,
      }
    );

    if (!writtenState) {
      throw new Error("Expected state file to be written");
    }

    expect(writtenState as WorkerState).toEqual({
      [baseEntry.id]: baseEntry,
      [secondEntry.id]: secondEntry,
    });

    expect(allocator.isAllocated(baseEntry.port)).toBe(true);
    expect(allocator.isAllocated(secondEntry.port)).toBe(true);
    expect(allocator.isAllocated(15001)).toBe(false);
  });

  it("runs health loop ticks and cleans up dead workers", async () => {
    let intervalCallback: IntervalCallback | null = null;
    const setInterval: typeof globalThis.setInterval = (
      callback: IntervalCallback,
      _delay?: number,
      ..._args: any[]
    ) => {
      intervalCallback = callback as IntervalCallback;
      return 1 as unknown as ReturnType<typeof globalThis.setInterval>;
    };

    const deleteCalls: string[] = [];
    globalThis.fetch = (async (input: Request | string, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.url;
      if (url.endsWith("/workers") && (!init?.method || init.method === "GET")) {
        return {
          ok: true,
          json: async () => [baseEntry, secondEntry],
        } as Response;
      }
      if (url.includes(`/workers/${secondEntry.id}`) && init?.method === "DELETE") {
        deleteCalls.push(url);
        return {
          ok: true,
          json: async () => ({ status: "stopped" }),
        } as Response;
      }
      return {
        ok: true,
        json: async () => ({}),
      } as Response;
    }) as typeof fetch;

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        checkIntervalMs: 1000,
      },
      {
        adoptExistingWorkers: async () => new Map(),
        writeStateFile: async () => {},
        readStateFile: async () => ({}),
        serveManager: {
          spawnServe: async () => baseEntry,
          killWorker: async () => {},
          healthCheck: async (port: number) => port === baseEntry.port,
        },
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        portAllocator: new PortAllocator(15000, 5),
        setInterval,
        clearInterval: () => {},
        fetch: globalThis.fetch,
      }
    );

    if (!intervalCallback) {
      throw new Error("Expected health loop callback to be scheduled");
    }
    await (intervalCallback as IntervalCallback)();

    expect(deleteCalls).toHaveLength(1);
    expect(deleteCalls[0]).toContain(secondEntry.id);
  });

  it("registers signal handlers and shuts down cleanly", async () => {
    const handlers: Array<{ signal: NodeJS.Signals; handler: () => void | Promise<void> }> = [];
    process.on = ((signal: NodeJS.Signals, handler: () => void | Promise<void>) => {
      handlers.push({ signal, handler });
      return process;
    }) as typeof process.on;

    let exitCode: number | null = null;
    process.exit = ((code?: number) => {
      exitCode = code ?? 0;
      return undefined as never;
    }) as typeof process.exit;

    const killed: WorkerEntry[] = [];
    let stopCalls = 0;
    let clearedInterval = 0;
    let finalState: WorkerState | null = null;

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
      },
      {
        adoptExistingWorkers: async () => new Map(),
        readStateFile: async () => ({
          [baseEntry.id]: baseEntry,
          [secondEntry.id]: secondEntry,
        }),
        writeStateFile: async (_path, state) => {
          finalState = state;
        },
        serveManager: {
          spawnServe: async () => baseEntry,
          killWorker: async (entry: WorkerEntry) => {
            killed.push(entry);
          },
          healthCheck: async () => true,
        },
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {
            stopCalls += 1;
          },
        }),
        portAllocator: new PortAllocator(15000, 5),
        setInterval: () => 1 as unknown as ReturnType<typeof globalThis.setInterval>,
        clearInterval: () => {
          clearedInterval += 1;
        },
        fetch: originalFetch,
      }
    );

    expect(handlers.map((entry) => entry.signal).sort()).toEqual(["SIGINT", "SIGTERM"]);

    const termHandler = handlers.find((entry) => entry.signal === "SIGTERM")?.handler;
    await Promise.resolve(termHandler?.());

    expect(killed).toEqual([baseEntry, secondEntry]);
    expect(stopCalls).toBe(1);
    expect(clearedInterval).toBe(1);
    if (!finalState) {
      throw new Error("Expected final state to be written");
    }
    expect(finalState as WorkerState).toEqual({});
    if (exitCode === null) {
      throw new Error("Expected process exit to be called");
    }
    expect(exitCode as number).toBe(0);
  });
});
