import { afterEach, describe, expect, it } from "bun:test";
import { startDaemon } from "../index";
import { PortAllocator } from "../ports";
import type { WorkerEntry } from "../serve-manager";
import type { PersistedWorkerState } from "../state-file";

type IntervalCallback = (...args: any[]) => Promise<void> | void;

const baseEntry: WorkerEntry = {
  id: "eng-1-implement",
  port: 15000,
  pid: 2222,
  sessionId: "ses-1",
  workspace: "/tmp/test-workspace",
  startedAt: "2026-02-01T00:00:00.000Z",
  status: "running",
  crashCount: 0,
  lastCrashAt: null,
};

const secondEntry: WorkerEntry = {
  id: "eng-2-plan",
  port: 15002,
  pid: 3333,
  sessionId: "ses-2",
  workspace: "/tmp/test-workspace",
  startedAt: "2026-02-01T01:00:00.000Z",
  status: "running",
  crashCount: 0,
  lastCrashAt: null,
};

const TEAM_ID = "123e4567-e89b-12d3-a456-426614174000";
const noopSetTimeout: typeof setTimeout = Object.assign(
  ((callback: (...args: any[]) => void) => {
    callback();
    return {} as ReturnType<typeof setTimeout>;
  }) as unknown as typeof setTimeout,
  { __promisify__: setTimeout.__promisify__ }
);
const noopClearTimeout = (() => {}) as typeof clearTimeout;

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
    const adoptExistingWorkers = async () => ({
      workers: new Map<string, WorkerEntry>([
        [baseEntry.id, baseEntry],
        [secondEntry.id, secondEntry],
      ]),
      crashHistory: {
        [baseEntry.id]: { crashCount: 1, lastCrashAt: "2026-02-02T00:00:00.000Z" },
      },
    });

    let writtenState: PersistedWorkerState | null = null;
    let currentState: PersistedWorkerState = { workers: {}, crashHistory: {} };
    const writeStateFile = async (_path: string, state: PersistedWorkerState) => {
      writtenState = state;
      currentState = state;
    };

    const startServer = () => ({
      server: { port: 15555 } as ReturnType<typeof Bun.serve>,
      stop: () => {},
    });

    const allocator = new PortAllocator(15000, 5);

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        teamId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        adoptExistingWorkers,
        writeStateFile,
        readStateFile: async () => currentState,
        serveManager: {
          spawnServe: async () => baseEntry,
          initializeSession: async () => {},
          killWorker: async () => {},
          healthCheck: async () => true,
        },
        startServer,
        portAllocator: allocator,
        setInterval: () => 1 as unknown as ReturnType<typeof globalThis.setInterval>,
        clearInterval: () => {},
        setTimeout: noopSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    if (!writtenState) {
      throw new Error("Expected state file to be written");
    }

    expect(writtenState as PersistedWorkerState).toEqual({
      workers: {
        [baseEntry.id]: baseEntry,
        [secondEntry.id]: secondEntry,
      },
      crashHistory: {
        [baseEntry.id]: { crashCount: 1, lastCrashAt: "2026-02-02T00:00:00.000Z" },
      },
      controller: {
        sessionId: "ses_test",
      },
    });

    expect(allocator.isAllocated(baseEntry.port)).toBe(true);
    expect(allocator.isAllocated(secondEntry.port)).toBe(true);
    expect(allocator.isAllocated(15001)).toBe(false);
  });

  it("runs health loop ticks and cleans up dead workers", async () => {
    let timeoutCallback: IntervalCallback | null = null;
    const mockSetTimeout: typeof setTimeout = Object.assign(
      ((callback: IntervalCallback, _delay?: number, ..._args: any[]) => {
        timeoutCallback = callback as IntervalCallback;
        return {} as ReturnType<typeof setTimeout>;
      }) as unknown as typeof setTimeout,
      { __promisify__: setTimeout.__promisify__ }
    );

    const deleteCalls: string[] = [];
    const patchCalls: Array<{ url: string; status: string }> = [];
    globalThis.fetch = (async (input: Request | string, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.url;
      if (url.endsWith("/workers") && (!init?.method || init.method === "GET")) {
        return {
          ok: true,
          json: async () => [baseEntry, secondEntry],
        } as Response;
      }
      if (url.includes(`/workers/${baseEntry.id}`) && init?.method === "PATCH") {
        patchCalls.push({ url, status: JSON.parse(init.body as string).status });
        return {
          ok: true,
          json: async () => ({ status: "running" }),
        } as Response;
      }
      if (url.includes(`/workers/${secondEntry.id}`) && init?.method === "PATCH") {
        patchCalls.push({ url, status: JSON.parse(init.body as string).status });
        return {
          ok: true,
          json: async () => ({ status: "dead" }),
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
        teamId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        adoptExistingWorkers: async () => ({ workers: new Map(), crashHistory: {} }),
        writeStateFile: async () => {},
        readStateFile: async () => ({ workers: {}, crashHistory: {} }),
        serveManager: {
          spawnServe: async () => baseEntry,
          initializeSession: async () => {},
          killWorker: async () => {},
          healthCheck: async (port: number) => port === baseEntry.port,
        },
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        portAllocator: new PortAllocator(15000, 5),
        setInterval: () => 1 as unknown as ReturnType<typeof globalThis.setInterval>,
        clearInterval: () => {},
        setTimeout: mockSetTimeout,
        clearTimeout: () => {},
        fetch: globalThis.fetch,
      }
    );

    if (!timeoutCallback) {
      throw new Error("Expected health loop callback to be scheduled");
    }
    const result = (timeoutCallback as () => Promise<void>)();
    await result;

    expect(deleteCalls).toHaveLength(1);
    expect(deleteCalls[0]).toContain(secondEntry.id);
    expect(patchCalls).toEqual(
      expect.arrayContaining([
        { url: expect.stringContaining(baseEntry.id), status: "running" },
        { url: expect.stringContaining(secondEntry.id), status: "dead" },
      ])
    );
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
    let clearedTimeout = 0;
    let finalState: PersistedWorkerState | null = null;

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        teamId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        adoptExistingWorkers: async () => ({ workers: new Map(), crashHistory: {} }),
        readStateFile: async () => ({
          workers: {
            [baseEntry.id]: baseEntry,
            [secondEntry.id]: secondEntry,
          },
          crashHistory: {
            [secondEntry.id]: { crashCount: 2, lastCrashAt: "2026-02-02T02:00:00.000Z" },
          },
        }),
        writeStateFile: async (_path, state) => {
          finalState = state;
        },
        serveManager: {
          spawnServe: async () => baseEntry,
          initializeSession: async () => {},
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
        clearInterval: () => {},
        setTimeout: noopSetTimeout,
        clearTimeout: (() => {
          clearedTimeout += 1;
        }) as typeof globalThis.clearTimeout,
        fetch: originalFetch,
      }
    );

    expect(handlers.map((entry) => entry.signal).sort()).toEqual(["SIGINT", "SIGTERM"]);

    const termHandler = handlers.find((entry) => entry.signal === "SIGTERM")?.handler;
    await Promise.resolve(termHandler?.());

    expect(killed).toEqual([baseEntry, secondEntry]);
    expect(stopCalls).toBe(1);
    expect(clearedTimeout).toBe(1);
    if (!finalState) {
      throw new Error("Expected final state to be written");
    }
    expect(finalState as PersistedWorkerState).toEqual({
      workers: {},
      crashHistory: {
        [secondEntry.id]: { crashCount: 2, lastCrashAt: "2026-02-02T02:00:00.000Z" },
      },
    });
    if (exitCode === null) {
      throw new Error("Expected process exit to be called");
    }
    expect(exitCode as number).toBe(0);
  });
});
