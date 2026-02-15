import { afterEach, describe, expect, it, mock } from "bun:test";
import type { WorkerEntry } from "../serve-manager";
import type { PersistedWorkerState } from "../state-file";

const promptAsyncCalls: Array<{ sessionID: string; parts: unknown[] }> = [];

mock.module("@opencode-ai/sdk/v2", () => ({
  createOpencodeClient: () => ({
    session: {
      promptAsync: async (opts: { sessionID: string; parts: unknown[] }) => {
        promptAsyncCalls.push(opts);
        return { data: { id: "prompt-1" } };
      },
    },
  }),
}));

import { startDaemon } from "../index";

type TimeoutCallback = (...args: unknown[]) => Promise<void> | void;

const baseEntry: WorkerEntry = {
  id: "eng-1-implement",
  port: 13381,
  sessionId: "ses-1",
  workspace: "/tmp/test-workspace",
  startedAt: "2026-02-01T00:00:00.000Z",
  status: "running",
  crashCount: 0,
  lastCrashAt: null,
};

const secondEntry: WorkerEntry = {
  id: "eng-2-plan",
  port: 13381,
  sessionId: "ses-2",
  workspace: "/tmp/test-workspace",
  startedAt: "2026-02-01T01:00:00.000Z",
  status: "running",
  crashCount: 0,
  lastCrashAt: null,
};

const TEAM_ID = "123e4567-e89b-12d3-a456-426614174000";
const silentSetTimeout: typeof setTimeout = Object.assign(
  ((_callback: (...args: unknown[]) => void) => {
    return {} as ReturnType<typeof setTimeout>;
  }) as unknown as typeof setTimeout,
  { __promisify__: setTimeout.__promisify__ }
);
const noopClearTimeout = (() => {}) as typeof clearTimeout;

function makeServeManager(overrides?: {
  healthCheck?: (port: number) => Promise<boolean>;
  stopServeCalls?: number[];
  createSessionCalls?: Array<{ port: number; sessionId: string; workspace: string }>;
}) {
  const createSessionCalls = overrides?.createSessionCalls ?? [];
  const stopServeCalls = overrides?.stopServeCalls ?? [];
  return {
    spawnSharedServe: async () => ({ port: 13381, pid: 9999, status: "starting" as const }),
    waitForHealthy: async () => {},
    createSession: async (port: number, sessionId: string, workspace: string) => {
      createSessionCalls.push({ port, sessionId, workspace });
    },
    healthCheck: overrides?.healthCheck ?? (async () => true),
    stopServe: async (_port: number, _pid: number) => {
      stopServeCalls.push(_pid);
    },
  };
}

describe("daemon entry", () => {
  const originalOn = process.on;
  const originalExit = process.exit;
  const originalFetch = globalThis.fetch;

  afterEach(() => {
    process.on = originalOn;
    process.exit = originalExit;
    globalThis.fetch = originalFetch;
    promptAsyncCalls.length = 0;
  });

  it("re-creates sessions for persisted workers on startup", async () => {
    const createSessionCalls: Array<{ port: number; sessionId: string; workspace: string }> = [];

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        teamId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        readStateFile: async () => ({
          workers: {
            [baseEntry.id]: baseEntry,
            [secondEntry.id]: secondEntry,
          },
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        serveManager: makeServeManager({ createSessionCalls }),
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    const workerSessions = createSessionCalls.filter(
      (c) => c.sessionId === "ses-1" || c.sessionId === "ses-2"
    );
    expect(workerSessions).toHaveLength(2);
  });

  it("checks shared serve health and restarts on failure", async () => {
    let timeoutCallback: TimeoutCallback | null = null;
    const mockSetTimeout: typeof setTimeout = Object.assign(
      ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
        timeoutCallback = callback as TimeoutCallback;
        return {} as ReturnType<typeof setTimeout>;
      }) as unknown as typeof setTimeout,
      { __promisify__: setTimeout.__promisify__ }
    );

    let healthCallCount = 0;
    const createSessionCalls: Array<{ port: number; sessionId: string; workspace: string }> = [];

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        checkIntervalMs: 1000,
        teamId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        readStateFile: async () => ({
          workers: { [baseEntry.id]: baseEntry },
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        serveManager: makeServeManager({
          createSessionCalls,
          healthCheck: async () => {
            healthCallCount += 1;
            if (healthCallCount === 1) {
              return true;
            }
            return healthCallCount > 2;
          },
        }),
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: mockSetTimeout,
        clearTimeout: () => {},
        fetch: originalFetch,
      }
    );

    if (!timeoutCallback) {
      throw new Error("Expected health loop callback to be scheduled");
    }
    await (timeoutCallback as () => Promise<void>)();

    const reCreatedSessions = createSessionCalls.filter((c) => c.sessionId === baseEntry.sessionId);
    expect(reCreatedSessions.length).toBeGreaterThanOrEqual(1);
  });

  it("re-creates internal controller session after serve crash+restart", async () => {
    let timeoutCallback: TimeoutCallback | null = null;
    const mockSetTimeout: typeof setTimeout = Object.assign(
      ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
        timeoutCallback = callback as TimeoutCallback;
        return {} as ReturnType<typeof setTimeout>;
      }) as unknown as typeof setTimeout,
      { __promisify__: setTimeout.__promisify__ }
    );

    let healthCallCount = 0;
    const createSessionCalls: Array<{ port: number; sessionId: string; workspace: string }> = [];

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        checkIntervalMs: 1000,
        teamId: TEAM_ID,
        controllerSessionId: undefined,
      },
      {
        readStateFile: async () => ({
          workers: {},
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        serveManager: makeServeManager({
          createSessionCalls,
          healthCheck: async () => {
            healthCallCount += 1;
            if (healthCallCount === 1) {
              return true;
            }
            if (healthCallCount === 2) {
              return false;
            }
            return true;
          },
        }),
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: mockSetTimeout,
        clearTimeout: () => {},
        fetch: originalFetch,
      }
    );

    const controllerSessionsBefore = createSessionCalls.length;
    const promptsBefore = promptAsyncCalls.length;

    if (!timeoutCallback) {
      throw new Error("Expected health loop callback to be scheduled");
    }
    await (timeoutCallback as () => Promise<void>)();

    const newSessions = createSessionCalls.slice(controllerSessionsBefore);
    expect(newSessions.length).toBeGreaterThanOrEqual(1);

    const newPrompts = promptAsyncCalls.slice(promptsBefore);
    const controllerRePrompts = newPrompts.filter((p) =>
      p.parts.some(
        (part) =>
          typeof part === "object" &&
          part !== null &&
          "text" in part &&
          (part as { text: string }).text === "/legion-controller"
      )
    );
    expect(controllerRePrompts.length).toBe(1);
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

    let stopCalls = 0;
    let clearedTimeout = 0;
    let finalState: PersistedWorkerState | null = null;
    const stopServeCalls: number[] = [];
    let startupHealthCheck = false;

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        teamId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
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
        serveManager: makeServeManager({
          stopServeCalls,
          healthCheck: async () => {
            if (!startupHealthCheck) {
              startupHealthCheck = true;
              return false;
            }
            return true;
          },
        }),
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {
            stopCalls += 1;
          },
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: (() => {
          clearedTimeout += 1;
        }) as typeof globalThis.clearTimeout,
        fetch: originalFetch,
      }
    );

    expect(handlers.map((entry) => entry.signal).sort()).toEqual(["SIGINT", "SIGTERM"]);

    const termHandler = handlers.find((entry) => entry.signal === "SIGTERM")?.handler;
    await Promise.resolve(termHandler?.());

    expect(stopServeCalls).toHaveLength(1);
    expect(stopCalls).toBe(1);
    expect(clearedTimeout).toBe(1);
    if (!finalState) {
      throw new Error("Expected final state to be written");
    }
    expect((finalState as PersistedWorkerState).workers).toEqual({});
    expect((finalState as PersistedWorkerState).crashHistory).toEqual({
      [secondEntry.id]: { crashCount: 2, lastCrashAt: "2026-02-02T02:00:00.000Z" },
    });
    if (exitCode === null) {
      throw new Error("Expected process exit to be called");
    }
    expect(exitCode as number).toBe(0);
  });
});
