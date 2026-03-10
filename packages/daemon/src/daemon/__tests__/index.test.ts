import { afterAll, afterEach, describe, expect, it, mock } from "bun:test";
import { createServer } from "node:net";
import { resolveLegionPaths } from "../paths";
import type { RuntimeAdapter } from "../runtime/types";
import type { WorkerEntry } from "../serve-manager";
import type { ServerOptions } from "../server";
import type { PersistedWorkerState } from "../state-file";

const promptAsyncCalls: Array<{ sessionID: string; parts: unknown[] }> = [];
let promptAsyncFailures = 0;

mock.module("@opencode-ai/sdk/v2", () => ({
  createOpencodeClient: () => ({
    session: {
      promptAsync: async (opts: { sessionID: string; parts: unknown[] }) => {
        if (promptAsyncFailures > 0) {
          promptAsyncFailures--;
          throw new Error("session not ready");
        }
        promptAsyncCalls.push(opts);
        return { data: { id: "prompt-1" } };
      },
    },
  }),
}));

const writeLegionEntryCalls: Array<{
  filePath: string;
  projectId: string;
  entry: { port: number; servePort: number; pid: number; startedAt: string };
}> = [];
const removeLegionEntryCalls: Array<{ filePath: string; projectId: string }> = [];
let mockedRegistry: Record<
  string,
  { port: number; servePort: number; pid: number; startedAt: string }
> = {};
let mockedAllocatedPorts = { daemonPort: 13370, servePort: 13381 };

import { startDaemon } from "../index";

type TimeoutCallback = (...args: unknown[]) => Promise<void> | void;

afterAll(() => {
  mock.restore();
});

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
const TEST_PATHS = resolveLegionPaths(
  {
    XDG_DATA_HOME: "/tmp/legion-test-data",
    XDG_STATE_HOME: "/tmp/legion-test-state",
  },
  "/tmp/legion-test-home"
);
const CONTROLLER_WORKSPACE = TEST_PATHS.forLegion(TEAM_ID).legionStateDir;
const silentSetTimeout: typeof setTimeout = Object.assign(
  ((_callback: (...args: unknown[]) => void) => {
    return {} as ReturnType<typeof setTimeout>;
  }) as unknown as typeof setTimeout,
  { __promisify__: setTimeout.__promisify__ }
);
const noopClearTimeout = (() => {}) as typeof clearTimeout;

function filterControllerPrompts(
  prompts: Array<{ sessionID: string; parts: unknown[] }>
): Array<{ sessionID: string; parts: unknown[] }> {
  return prompts.filter((p) =>
    p.parts.some(
      (part) =>
        typeof part === "object" &&
        part !== null &&
        "text" in part &&
        (part as { text: string }).text.startsWith("/legion-controller")
    )
  );
}

function makeAdapter(overrides?: {
  healthy?: () => Promise<boolean>;
  stopServeCalls?: number[];
  createSessionCalls?: Array<{ sessionId: string; workspace: string }>;
}): RuntimeAdapter {
  const createSessionCalls = overrides?.createSessionCalls ?? [];
  const stopServeCalls = overrides?.stopServeCalls ?? [];
  return {
    start: async () => {},
    stop: async () => {
      stopServeCalls.push(1);
    },
    healthy: overrides?.healthy ?? (async () => true),
    getPort: () => 13381,
    createSession: async (sessionId: string, workspace: string) => {
      createSessionCalls.push({ sessionId, workspace });
      return sessionId;
    },
    sendPrompt: async (sessionId: string, text: string) => {
      if (promptAsyncFailures > 0) {
        promptAsyncFailures--;
        throw new Error("session not ready");
      }
      promptAsyncCalls.push({ sessionID: sessionId, parts: [{ type: "text", text }] });
    },
    getSessionStatus: async () => ({ data: undefined }),
  };
}

async function getFreePort(): Promise<number> {
  return await new Promise((resolve, reject) => {
    const server = createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      if (!address || typeof address === "string") {
        reject(new Error("Failed to obtain free port"));
        return;
      }
      const { port } = address;
      server.close((error) => {
        if (error) {
          reject(error);
          return;
        }
        resolve(port);
      });
    });
  });
}

function startDaemonForTest(
  overrides: Parameters<typeof startDaemon>[0],
  deps?: Parameters<typeof startDaemon>[1]
): Promise<Awaited<ReturnType<typeof startDaemon>>> {
  return startDaemon(
    {
      paths: TEST_PATHS,
      readLegionsRegistry: async () => mockedRegistry,
      allocatePort: () => mockedAllocatedPorts,
      writeLegionEntry: async (
        filePath: string,
        projectId: string,
        entry: { port: number; servePort: number; pid: number; startedAt: string }
      ) => {
        writeLegionEntryCalls.push({ filePath, projectId, entry });
      },
      removeLegionEntry: async (filePath: string, projectId: string) => {
        removeLegionEntryCalls.push({ filePath, projectId });
      },
      ...overrides,
    },
    deps
  );
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
    promptAsyncFailures = 0;
    writeLegionEntryCalls.length = 0;
    removeLegionEntryCalls.length = 0;
    mockedRegistry = {};
    mockedAllocatedPorts = { daemonPort: 13370, servePort: 13381 };
  });

  describe("port allocation", () => {
    it("starts on base port 13370 when registry is empty", async () => {
      const basePort = await getFreePort();
      mockedRegistry = {};
      mockedAllocatedPorts = { daemonPort: basePort, servePort: basePort + 100 };

      const startServerCalls: ServerOptions[] = [];

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({ workers: {}, crashHistory: {} }),
          writeStateFile: async () => {},
          adapter: makeAdapter(),
          startServer: (opts) => {
            startServerCalls.push(opts);
            return {
              server: { port: opts.port } as ReturnType<typeof Bun.serve>,
              stop: () => {},
            };
          },
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      expect(startServerCalls).toHaveLength(1);
      expect(startServerCalls[0].port).toBe(basePort);
      expect(writeLegionEntryCalls[0].entry.port).toBe(basePort);
    });

    it("starts on 13371 when registry already uses 13370", async () => {
      const occupiedPort = await getFreePort();
      mockedRegistry = {
        occupied: {
          port: occupiedPort,
          servePort: occupiedPort + 100,
          pid: 9999,
          startedAt: "2026-02-01T00:00:00.000Z",
        },
      };
      mockedAllocatedPorts = { daemonPort: occupiedPort + 1, servePort: occupiedPort + 101 };

      const startServerCalls: ServerOptions[] = [];

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({ workers: {}, crashHistory: {} }),
          writeStateFile: async () => {},
          adapter: makeAdapter(),
          startServer: (opts) => {
            startServerCalls.push(opts);
            return {
              server: { port: opts.port } as ReturnType<typeof Bun.serve>,
              stop: () => {},
            };
          },
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      expect(startServerCalls).toHaveLength(1);
      expect(startServerCalls[0].port).toBe(occupiedPort + 1);
      expect(writeLegionEntryCalls[0].entry.port).toBe(occupiedPort + 1);
      expect(writeLegionEntryCalls[0].entry.servePort).toBe(occupiedPort + 101);
    });

    it("retries with next port when bind throws EADDRINUSE", async () => {
      const initialPort = await getFreePort();
      mockedAllocatedPorts = { daemonPort: initialPort, servePort: initialPort + 100 };

      const startServerCalls: number[] = [];

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({ workers: {}, crashHistory: {} }),
          writeStateFile: async () => {},
          adapter: makeAdapter(),
          startServer: (opts) => {
            if (opts.port === undefined) {
              throw new Error("Expected daemon port to be set");
            }
            startServerCalls.push(opts.port);
            if (opts.port === initialPort) {
              const error = new Error("Address in use") as NodeJS.ErrnoException;
              error.code = "EADDRINUSE";
              throw error;
            }
            return {
              server: { port: opts.port } as ReturnType<typeof Bun.serve>,
              stop: () => {},
            };
          },
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      expect(startServerCalls).toEqual([initialPort, initialPort + 1]);
      expect(writeLegionEntryCalls[0].entry.port).toBe(initialPort + 1);
    });
  });

  it("re-creates sessions for persisted workers on startup", async () => {
    const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
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
        adapter: makeAdapter({ createSessionCalls }),
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

  it("registers on start, deregisters on stop, and uses legion state dir for controller workspace", async () => {
    const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];
    const startServerCalls: ServerOptions[] = [];

    const handle = await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
        controllerSessionId: undefined,
      },
      {
        readStateFile: async () => ({ workers: {}, crashHistory: {} }),
        writeStateFile: async () => {},
        adapter: makeAdapter({ createSessionCalls }),
        startServer: (opts) => {
          startServerCalls.push(opts);
          return {
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
          };
        },
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    expect(startServerCalls).toHaveLength(1);
    expect(startServerCalls[0].paths).toBe(TEST_PATHS);
    expect(startServerCalls[0].projectId).toBe(TEAM_ID);

    expect(createSessionCalls).toHaveLength(1);
    expect(createSessionCalls[0].workspace).toBe(CONTROLLER_WORKSPACE);

    expect(writeLegionEntryCalls).toHaveLength(1);
    expect(writeLegionEntryCalls[0].filePath).toBe(TEST_PATHS.legionsFile);
    expect(writeLegionEntryCalls[0].projectId).toBe(TEAM_ID);
    if (startServerCalls[0].port === undefined) {
      throw new Error("Expected daemon port to be set");
    }
    expect(writeLegionEntryCalls[0].entry.port).toBe(startServerCalls[0].port);
    expect(writeLegionEntryCalls[0].entry.servePort).toBeGreaterThanOrEqual(13381);
    expect(typeof writeLegionEntryCalls[0].entry.pid).toBe("number");

    await handle.stop();

    expect(removeLegionEntryCalls).toHaveLength(1);
    expect(removeLegionEntryCalls[0]).toEqual({
      filePath: TEST_PATHS.legionsFile,
      projectId: TEAM_ID,
    });
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
    const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        checkIntervalMs: 1000,
        legionId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        readStateFile: async () => ({
          workers: { [baseEntry.id]: baseEntry },
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        adapter: makeAdapter({
          createSessionCalls,
          healthy: async () => {
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
    const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        checkIntervalMs: 1000,
        legionId: TEAM_ID,
        controllerSessionId: undefined,
      },
      {
        readStateFile: async () => ({
          workers: {},
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        adapter: makeAdapter({
          createSessionCalls,
          healthy: async () => {
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

  it("appends controllerPrompt to initial /legion-controller prompt", async () => {
    promptAsyncCalls.length = 0;

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
        controllerSessionId: undefined,
        controllerPrompt: "Do not start new work. Focus on LEG-137 only.",
      },
      {
        readStateFile: async () => ({ workers: {}, crashHistory: {} }),
        writeStateFile: async () => {},
        adapter: makeAdapter(),
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    const controllerPrompts = filterControllerPrompts(promptAsyncCalls);
    expect(controllerPrompts.length).toBe(1);

    const text = (controllerPrompts[0].parts[0] as { text: string }).text;
    expect(text).toBe("/legion-controller\n\nDo not start new work. Focus on LEG-137 only.");
  });

  it("sends plain /legion-controller when no controllerPrompt given", async () => {
    promptAsyncCalls.length = 0;

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
        controllerSessionId: undefined,
      },
      {
        readStateFile: async () => ({ workers: {}, crashHistory: {} }),
        writeStateFile: async () => {},
        adapter: makeAdapter(),
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    const controllerPrompts = filterControllerPrompts(promptAsyncCalls);
    expect(controllerPrompts.length).toBe(1);

    const text = (controllerPrompts[0].parts[0] as { text: string }).text;
    expect(text).toBe("/legion-controller");
  });

  it("does NOT append controllerPrompt on restart after serve crash", async () => {
    let timeoutCallback: TimeoutCallback | null = null;
    const mockSetTimeout: typeof setTimeout = Object.assign(
      ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
        timeoutCallback = callback as TimeoutCallback;
        return {} as ReturnType<typeof setTimeout>;
      }) as unknown as typeof setTimeout,
      { __promisify__: setTimeout.__promisify__ }
    );

    let healthCallCount = 0;
    const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        checkIntervalMs: 1000,
        legionId: TEAM_ID,
        controllerSessionId: undefined,
        controllerPrompt: "Do not start new work. Focus on LEG-137 only.",
      },
      {
        readStateFile: async () => ({
          workers: {},
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        adapter: makeAdapter({
          createSessionCalls,
          healthy: async () => {
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

    const promptsBefore = promptAsyncCalls.length;

    if (!timeoutCallback) {
      throw new Error("Expected health loop callback to be scheduled");
    }
    await (timeoutCallback as () => Promise<void>)();

    const newPrompts = promptAsyncCalls.slice(promptsBefore);
    const controllerRePrompts = filterControllerPrompts(newPrompts);
    expect(controllerRePrompts.length).toBe(1);

    const restartPromptText = (controllerRePrompts[0].parts[0] as { text: string }).text;
    expect(restartPromptText).toBe("/legion-controller");
    expect(restartPromptText).not.toContain("Do not start new work");
  });

  it("treats empty string controllerPrompt as no prompt", async () => {
    promptAsyncCalls.length = 0;

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
        controllerSessionId: undefined,
        controllerPrompt: "",
      },
      {
        readStateFile: async () => ({ workers: {}, crashHistory: {} }),
        writeStateFile: async () => {},
        adapter: makeAdapter(),
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    const controllerPrompts = filterControllerPrompts(promptAsyncCalls);
    expect(controllerPrompts.length).toBe(1);

    const text = (controllerPrompts[0].parts[0] as { text: string }).text;
    expect(text).toBe("/legion-controller");
  });

  it("retries prompt delivery with backoff on transient failures", async () => {
    promptAsyncCalls.length = 0;
    promptAsyncFailures = 2;

    const setTimeoutDelays: number[] = [];
    const retrySetTimeout: typeof setTimeout = Object.assign(
      ((callback: (...args: unknown[]) => void, delay?: number, ..._args: unknown[]) => {
        if (delay !== undefined && delay < 1000) {
          setTimeoutDelays.push(delay);
          callback();
        }
        return {} as ReturnType<typeof setTimeout>;
      }) as unknown as typeof setTimeout,
      { __promisify__: setTimeout.__promisify__ }
    );

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
        controllerSessionId: undefined,
      },
      {
        readStateFile: async () => ({ workers: {}, crashHistory: {} }),
        writeStateFile: async () => {},
        adapter: makeAdapter(),
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: retrySetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    const controllerPrompts = filterControllerPrompts(promptAsyncCalls);
    expect(controllerPrompts.length).toBe(1);
    expect(setTimeoutDelays).toEqual([100, 200]);
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

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
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
        adapter: makeAdapter({
          stopServeCalls,
          healthy: async () => {
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
    expect((finalState as PersistedWorkerState).workers).toEqual({
      [baseEntry.id]: baseEntry,
      [secondEntry.id]: secondEntry,
    });
    expect((finalState as PersistedWorkerState).crashHistory).toEqual({
      [secondEntry.id]: { crashCount: 2, lastCrashAt: "2026-02-02T02:00:00.000Z" },
    });
    if (exitCode === null) {
      throw new Error("Expected process exit to be called");
    }
    expect(exitCode as number).toBe(0);
  });

  it("preserves workers in state file when stopped via handle.stop()", async () => {
    let savedState: PersistedWorkerState | null = null;

    const handle = await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        readStateFile: async () => ({
          workers: {
            [baseEntry.id]: baseEntry,
            [secondEntry.id]: secondEntry,
          },
          crashHistory: {
            [secondEntry.id]: { crashCount: 1, lastCrashAt: "2026-02-02T02:00:00.000Z" },
          },
        }),
        writeStateFile: async (_path, state) => {
          savedState = state;
        },
        adapter: makeAdapter(),
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    await handle.stop();

    if (!savedState) {
      throw new Error("Expected final state to be written");
    }
    const state = savedState as PersistedWorkerState;
    // Workers preserved
    expect(state.workers[baseEntry.id]).toBeDefined();
    expect(state.workers[secondEntry.id]).toBeDefined();
    // Crash history preserved
    expect(state.crashHistory[secondEntry.id]).toEqual({
      crashCount: 1,
      lastCrashAt: "2026-02-02T02:00:00.000Z",
    });
    // Controller cleared (daemon-owned)
    expect(state.controller).toBeUndefined();
  });
  it("passes controller env vars to adapter.start on startup", async () => {
    const startCalls: Array<{ workspace: string; logDir?: string; env?: Record<string, string> }> =
      [];

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
        legionDir: "/test/legion",
        daemonPort: 13370,
        issueBackend: "linear",
      },
      {
        readStateFile: async () => ({ workers: {}, crashHistory: {} }),
        writeStateFile: async () => {},
        adapter: {
          ...makeAdapter({ healthy: async () => false }),
          start: async (opts) => {
            startCalls.push(opts);
          },
        },
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    expect(startCalls).toHaveLength(1);
    const opts = startCalls[0];
    expect(opts.env).toBeDefined();
    expect(opts.env?.LEGION_ID).toBe(TEAM_ID);
    expect(opts.env?.LEGION_ISSUE_BACKEND).toBe("linear");
    expect(opts.env?.LEGION_DIR).toBeUndefined();
    expect(opts.env?.LEGION_SHORT_ID).toBe(TEAM_ID.slice(0, 8));
    expect(Number(opts.env?.LEGION_DAEMON_PORT)).toBeGreaterThanOrEqual(13370);
  });

  it("passes controller env vars to adapter.start on health restart", async () => {
    let timeoutCallback: TimeoutCallback | null = null;
    const mockSetTimeout: typeof setTimeout = Object.assign(
      ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
        timeoutCallback = callback as TimeoutCallback;
        return {} as ReturnType<typeof setTimeout>;
      }) as unknown as typeof setTimeout,
      { __promisify__: setTimeout.__promisify__ }
    );

    let healthCallCount = 0;
    const startCalls: Array<{ workspace: string; logDir?: string; env?: Record<string, string> }> =
      [];

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
        legionDir: "/test/legion",
        daemonPort: 13370,
        issueBackend: "github",
        controllerSessionId: "ses_test",
      },
      {
        readStateFile: async () => ({ workers: {}, crashHistory: {} }),
        writeStateFile: async () => {},
        adapter: {
          ...makeAdapter({
            healthy: async () => {
              healthCallCount += 1;
              if (healthCallCount === 1) return false;
              if (healthCallCount === 2) return false;
              return true;
            },
          }),
          start: async (opts) => {
            startCalls.push(opts);
          },
        },
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: mockSetTimeout,
        clearTimeout: () => {},
        fetch: originalFetch,
      }
    );

    if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
    await (timeoutCallback as () => Promise<void>)();

    expect(startCalls.length).toBeGreaterThanOrEqual(2);
    const restartOpts = startCalls[1];
    expect(restartOpts.env).toBeDefined();
    expect(restartOpts.env?.LEGION_ID).toBe(TEAM_ID);
    expect(restartOpts.env?.LEGION_ISSUE_BACKEND).toBe("github");
    expect(restartOpts.env?.LEGION_DIR).toBeUndefined();
    expect(restartOpts.env?.LEGION_SHORT_ID).toBe(TEAM_ID.slice(0, 8));
    expect(Number(restartOpts.env?.LEGION_DAEMON_PORT)).toBeGreaterThanOrEqual(13370);
  });

  it("logs warning when worker session ID changes during startup re-creation", async () => {
    const warnCalls: string[] = [];
    const originalWarn = console.warn;
    console.warn = ((msg: string) => {
      warnCalls.push(msg);
    }) as typeof console.warn;

    try {
      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({
            workers: {
              [baseEntry.id]: baseEntry,
            },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: {
            ...makeAdapter({ healthy: async () => true }),
            createSession: async (sessionId: string, _workspace: string) => {
              if (sessionId === baseEntry.sessionId) {
                return "ses_actual_different";
              }
              return sessionId;
            },
          },
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
          }),
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      const mismatchWarnings = warnCalls.filter(
        (msg) => msg.includes("session ID changed") || msg.includes(baseEntry.id)
      );
      expect(mismatchWarnings.length).toBeGreaterThan(0);
    } finally {
      console.warn = originalWarn;
    }
  });

  it("logs warning when worker session ID changes during health-loop restart", async () => {
    let timeoutCallback: TimeoutCallback | null = null;
    const mockSetTimeout: typeof setTimeout = Object.assign(
      ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
        timeoutCallback = callback as TimeoutCallback;
        return {} as ReturnType<typeof setTimeout>;
      }) as unknown as typeof setTimeout,
      { __promisify__: setTimeout.__promisify__ }
    );

    let healthCallCount = 0;
    const warnCalls: string[] = [];
    const originalWarn = console.warn;
    console.warn = ((msg: string) => {
      warnCalls.push(msg);
    }) as typeof console.warn;

    try {
      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          checkIntervalMs: 1000,
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({
            workers: { [baseEntry.id]: baseEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: {
            ...makeAdapter({
              healthy: async () => {
                healthCallCount += 1;
                if (healthCallCount === 1) return true;
                if (healthCallCount === 2) return false;
                return true;
              },
            }),
            createSession: async (sessionId: string, _workspace: string) => {
              if (sessionId === baseEntry.sessionId) {
                return "ses_actual_different_health";
              }
              return sessionId;
            },
          },
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: originalFetch,
        }
      );

      if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
      await (timeoutCallback as () => Promise<void>)();

      const mismatchWarnings = warnCalls.filter(
        (msg) => msg.includes("session ID changed") || msg.includes(baseEntry.id)
      );
      expect(mismatchWarnings.length).toBeGreaterThan(0);
    } finally {
      console.warn = originalWarn;
    }
  });
});
