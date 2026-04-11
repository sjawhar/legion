import { afterAll, afterEach, describe, expect, it, mock } from "bun:test";
import { createServer } from "node:net";
import { type DaemonConfig, resolveDaemonConfig } from "../config";
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
  entry: { port: number; servePort: number; pid: number; servePid?: number; startedAt: string };
}> = [];
const removeLegionEntryCalls: Array<{ filePath: string; projectId: string }> = [];
let mockedRegistry: Record<
  string,
  { port: number; servePort: number; pid: number; servePid?: number; startedAt: string }
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

type DaemonStartOpts = NonNullable<Parameters<typeof startDaemon>[1]>;
type DaemonDepsOverride = NonNullable<DaemonStartOpts["deps"]>;

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
  getServePid?: () => number;
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
    getServePid: overrides?.getServePid ?? (() => 0),
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
    deleteSession: async () => {},
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
  overrides: Partial<DaemonConfig>,
  deps?: Partial<DaemonDepsOverride>,
  startOpts?: Omit<DaemonStartOpts, "deps">
): Promise<Awaited<ReturnType<typeof startDaemon>>> {
  const legionId = overrides.legionId ?? TEAM_ID;
  const instancePaths = TEST_PATHS.forLegion(legionId);
  const { config: baseConfig } = resolveDaemonConfig({
    env: {
      LEGION_ID: legionId,
      XDG_DATA_HOME: "/tmp/legion-test-data",
      XDG_STATE_HOME: "/tmp/legion-test-state",
    },
  });
  const config: DaemonConfig = {
    ...baseConfig,
    paths: TEST_PATHS,
    legionId,
    logDir: instancePaths.logDir,
    stateFilePath: overrides.stateFilePath ?? instancePaths.workersFile,
    ...overrides,
  };

  return startDaemon(config, {
    readLegionsRegistry: async () => mockedRegistry,
    cleanupStaleServes: async () => {},
    allocatePort: () => mockedAllocatedPorts,
    writeLegionEntry: async (
      filePath: string,
      projectId: string,
      entry: {
        port: number;
        servePort: number;
        pid: number;
        servePid?: number;
        startedAt: string;
      }
    ) => {
      writeLegionEntryCalls.push({ filePath, projectId, entry });
    },
    removeLegionEntry: async (filePath: string, projectId: string) => {
      removeLegionEntryCalls.push({ filePath, projectId });
    },
    ...startOpts,
    deps,
  });
}

describe("daemon entry", () => {
  const originalOn = process.on;
  const originalExit = process.exit;
  const originalFetch = globalThis.fetch;
  const startServerCalls: ServerOptions[] = [];

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
    startServerCalls.length = 0;
  });

  describe("port allocation", () => {
    it("uses the exact explicit daemon port when it is free", async () => {
      const explicitPort = await getFreePort();
      const startServerCalls: ServerOptions[] = [];

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
          daemonPort: explicitPort,
          daemonPortExplicit: true,
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
              fetchAndProcessState: async () => {},
            };
          },
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      expect(startServerCalls).toHaveLength(1);
      expect(startServerCalls[0].port).toBe(explicitPort);
      expect(writeLegionEntryCalls[0].entry.port).toBe(explicitPort);
    });

    it("throws when the explicit daemon port is occupied", async () => {
      const occupiedPort = await getFreePort();
      const blocker = createServer();
      await new Promise<void>((resolve, reject) => {
        blocker.once("error", reject);
        blocker.listen(occupiedPort, "127.0.0.1", () => resolve());
      });

      try {
        try {
          await startDaemonForTest(
            {
              stateFilePath: "/tmp/daemon-workers.json",
              legionId: TEAM_ID,
              controllerSessionId: "ses_test",
              daemonPort: occupiedPort,
              daemonPortExplicit: true,
            },
            {
              readStateFile: async () => ({ workers: {}, crashHistory: {} }),
              writeStateFile: async () => {},
              adapter: makeAdapter(),
              startServer: (opts) => ({
                server: { port: opts.port } as ReturnType<typeof Bun.serve>,
                stop: () => {},
                fetchAndProcessState: async () => {},
              }),
              setTimeout: silentSetTimeout,
              clearTimeout: noopClearTimeout,
              fetch: originalFetch,
            }
          );
          throw new Error("Expected startDaemonForTest to reject");
        } catch (error) {
          expect(error).toBeInstanceOf(Error);
        }
      } finally {
        await new Promise<void>((resolve, reject) => {
          blocker.close((error) => {
            if (error) {
              reject(error);
              return;
            }
            resolve();
          });
        });
      }
    });

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
              fetchAndProcessState: async () => {},
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

    it("keeps allocation behavior when daemon port is not explicit", async () => {
      const allocatedPort = await getFreePort();
      mockedAllocatedPorts = { daemonPort: allocatedPort, servePort: allocatedPort + 100 };

      const startServerCalls: ServerOptions[] = [];

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
          daemonPort: allocatedPort - 10,
          daemonPortExplicit: false,
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
              fetchAndProcessState: async () => {},
            };
          },
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      expect(startServerCalls).toHaveLength(1);
      expect(startServerCalls[0].port).toBe(allocatedPort);
      expect(writeLegionEntryCalls[0].entry.port).toBe(allocatedPort);
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
              fetchAndProcessState: async () => {},
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
              fetchAndProcessState: async () => {},
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

  describe("stale serve cleanup", () => {
    it("calls cleanupStaleServes on startup before port allocation", async () => {
      const cleanupCalls: Array<{ filePath: string; legionId: string }> = [];

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
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        },
        {
          cleanupStaleServes: async (filePath: string, legionId: string) => {
            cleanupCalls.push({ filePath, legionId });
          },
        }
      );

      expect(cleanupCalls).toHaveLength(1);
      expect(cleanupCalls[0].legionId).toBe(TEAM_ID);
      expect(cleanupCalls[0].filePath).toBe(TEST_PATHS.legionsFile);
    });

    it("writes servePid to registry when adapter provides it", async () => {
      const servePid = 42424;

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({ workers: {}, crashHistory: {} }),
          writeStateFile: async () => {},
          adapter: {
            ...makeAdapter(),
            getServePid: () => servePid,
          },
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      expect(writeLegionEntryCalls).toHaveLength(1);
      expect(writeLegionEntryCalls[0].entry.servePid).toBe(servePid);
    });

    it("omits servePid from registry when adapter returns 0", async () => {
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
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      expect(writeLegionEntryCalls).toHaveLength(1);
      expect(writeLegionEntryCalls[0].entry.servePid).toBeUndefined();
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
          fetchAndProcessState: async () => {},
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
            fetchAndProcessState: async () => {},
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
          fetchAndProcessState: async () => {},
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

  it("re-subscribes workers to envoy topics after serve restart", async () => {
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
    const envoySubscribeCalls: Array<{
      url: string;
      body: { session_id: string; topics: string[] };
    }> = [];

    const workerWithTopics: WorkerEntry = {
      ...baseEntry,
      envoyTopics: ["notifications.github.acme.widgets.issue.42.>"],
    };
    const workerWithoutTopics: WorkerEntry = {
      ...secondEntry,
      envoyTopics: undefined,
    };

    const mockFetch = async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/interests/subscribe")) {
        envoySubscribeCalls.push({
          url,
          body: JSON.parse(init?.body as string),
        });
        return new Response("{}", { status: 200 });
      }
      return originalFetch(input, init);
    };

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        checkIntervalMs: 1000,
        legionId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        readStateFile: async () => ({
          workers: {
            [workerWithTopics.id]: workerWithTopics,
            [workerWithoutTopics.id]: workerWithoutTopics,
          },
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
          fetchAndProcessState: async () => {},
        }),
        setTimeout: mockSetTimeout,
        clearTimeout: () => {},
        fetch: Object.assign(mockFetch, {
          preconnect: originalFetch.preconnect,
        }),
      }
    );

    envoySubscribeCalls.length = 0;

    // subscribeWorkerToEnvoy uses globalThis.fetch directly (not DI'd)
    globalThis.fetch = Object.assign(mockFetch, {
      preconnect: originalFetch.preconnect,
    });

    if (!timeoutCallback) {
      throw new Error("Expected health loop callback to be scheduled");
    }
    await (timeoutCallback as () => Promise<void>)();

    // Worker with topics should be re-subscribed
    const workerSubscriptions = envoySubscribeCalls.filter(
      (c) => c.body.session_id === workerWithTopics.sessionId
    );
    expect(workerSubscriptions).toHaveLength(1);
    expect(workerSubscriptions[0].body.topics).toEqual([
      "notifications.github.acme.widgets.issue.42.>",
    ]);

    // Worker without topics should NOT be subscribed
    const noTopicSubscriptions = envoySubscribeCalls.filter(
      (c) => c.body.session_id === workerWithoutTopics.sessionId
    );
    expect(noTopicSubscriptions).toHaveLength(0);
  });

  it("does not re-subscribe workers to envoy when session recreation fails", async () => {
    let timeoutCallback: TimeoutCallback | null = null;
    const mockSetTimeout: typeof setTimeout = Object.assign(
      ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
        timeoutCallback = callback as TimeoutCallback;
        return {} as ReturnType<typeof setTimeout>;
      }) as unknown as typeof setTimeout,
      { __promisify__: setTimeout.__promisify__ }
    );

    let healthCallCount = 0;
    const envoySubscribeCalls: Array<{
      url: string;
      body: { session_id: string; topics: string[] };
    }> = [];

    const workerWithTopics: WorkerEntry = {
      ...baseEntry,
      envoyTopics: ["notifications.github.acme.widgets.issue.42.>"],
    };

    const mockFetch = async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/interests/subscribe")) {
        envoySubscribeCalls.push({
          url,
          body: JSON.parse(init?.body as string),
        });
        return new Response("{}", { status: 200 });
      }
      return originalFetch(input, init);
    };

    // Adapter whose createSession always throws
    const failingAdapter: RuntimeAdapter = {
      ...makeAdapter({
        healthy: async () => {
          healthCallCount += 1;
          if (healthCallCount === 1) {
            return true;
          }
          return healthCallCount > 2;
        },
      }),
      createSession: async () => {
        throw new Error("session creation failed");
      },
    };

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        checkIntervalMs: 1000,
        legionId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        readStateFile: async () => ({
          workers: { [workerWithTopics.id]: workerWithTopics },
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        adapter: failingAdapter,
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
          fetchAndProcessState: async () => {},
        }),
        setTimeout: mockSetTimeout,
        clearTimeout: () => {},
        fetch: Object.assign(mockFetch, {
          preconnect: originalFetch.preconnect,
        }),
      }
    );

    envoySubscribeCalls.length = 0;

    globalThis.fetch = Object.assign(mockFetch, {
      preconnect: originalFetch.preconnect,
    });

    if (!timeoutCallback) {
      throw new Error("Expected health loop callback to be scheduled");
    }
    await (timeoutCallback as () => Promise<void>)();

    // No envoy subscribes should have happened since all sessions failed to recreate
    const workerSubscriptions = envoySubscribeCalls.filter(
      (c) => c.body.session_id === workerWithTopics.sessionId
    );
    expect(workerSubscriptions).toHaveLength(0);
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
          fetchAndProcessState: async () => {},
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
          fetchAndProcessState: async () => {},
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
          fetchAndProcessState: async () => {},
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
          fetchAndProcessState: async () => {},
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
          fetchAndProcessState: async () => {},
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
          fetchAndProcessState: async () => {},
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
          fetchAndProcessState: async () => {},
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
          fetchAndProcessState: async () => {},
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
        readStateFile: async () => ({
          workers: {
            "test-implement": {
              id: "test-implement",
              port: 13381,
              sessionId: "ses_worker",
              workspace: "/test",
              startedAt: new Date().toISOString(),
              status: "running" as const,
              crashCount: 0,
              lastCrashAt: null,
            },
          },
          crashHistory: {},
        }),
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
          fetchAndProcessState: async () => {},
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
            fetchAndProcessState: async () => {},
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
            fetchAndProcessState: async () => {},
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

  describe("feedback logger wiring", () => {
    it("creates and passes a feedback logger when feedback is enabled", async () => {
      const handle = await startDaemonForTest(
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
              fetchAndProcessState: async () => {},
            };
          },
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      expect(startServerCalls).toHaveLength(1);
      expect(startServerCalls[0].feedbackLogger).toBeDefined();

      await handle.stop();
    });

    it("skips feedback logger creation when feedback is disabled in config", async () => {
      const handle = await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
          feedbackDisabled: true,
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
              fetchAndProcessState: async () => {},
            };
          },
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      expect(startServerCalls).toHaveLength(1);
      expect(startServerCalls[0].feedbackLogger).toBeUndefined();

      await handle.stop();
    });

    it("flushes the feedback logger during shutdown", async () => {
      const handle = await startDaemonForTest(
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
              fetchAndProcessState: async () => {},
            };
          },
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch: originalFetch,
        }
      );

      const feedbackLogger = startServerCalls[0].feedbackLogger;
      if (!feedbackLogger) {
        throw new Error("Expected feedbackLogger to be created");
      }

      const flush = mock(async () => {});
      feedbackLogger.flush = flush;

      await handle.stop();

      expect(flush).toHaveBeenCalledTimes(1);
    });
  });
  describe("RSS-based serve restart", () => {
    it("triggers stop + start when RSS exceeds threshold (Linux only)", async () => {
      // Skip on non-Linux where /proc doesn't exist
      if (process.platform !== "linux") return;

      let timeoutCallback: TimeoutCallback | null = null;
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
          timeoutCallback = callback as TimeoutCallback;
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      let _healthCallCount = 0;
      const stopCalls: number[] = [];
      const startCalls: Array<{
        workspace: string;
        logDir?: string;
        env?: Record<string, string>;
      }> = [];
      const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          checkIntervalMs: 1000,
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
          // 1 byte threshold \u2014 any real process will exceed this
          maxRssBytes: 1,
          rssCheckIntervalMs: 0,
        },
        {
          readStateFile: async () => ({
            workers: { [baseEntry.id]: baseEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: {
            ...makeAdapter({
              createSessionCalls,
              stopServeCalls: stopCalls,
              healthy: async () => {
                _healthCallCount += 1;
                return true;
              },
              // Use the real test process PID so /proc/<pid>/statm exists
              getServePid: () => process.pid,
            }),
            start: async (opts) => {
              startCalls.push(opts);
            },
          },
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: originalFetch,
        }
      );

      if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
      await (timeoutCallback as () => Promise<void>)();

      // RSS exceeded: stop was called (graceful shutdown before restart)
      expect(stopCalls.length).toBeGreaterThanOrEqual(1);
      // Then start was called (restart)
      expect(startCalls.length).toBeGreaterThanOrEqual(1);
      // Worker session was re-created after restart
      const reCreated = createSessionCalls.filter((c) => c.sessionId === baseEntry.sessionId);
      expect(reCreated.length).toBeGreaterThanOrEqual(1);
    });

    it("does not check RSS when serve is unhealthy", async () => {
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
          maxRssBytes: 1 * 1024 * 1024 * 1024,
          rssCheckIntervalMs: 0,
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
              if (healthCallCount === 1) return true;
              if (healthCallCount === 2) return false;
              return true;
            },
            getServePid: () => 12345,
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: originalFetch,
        }
      );

      if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
      await (timeoutCallback as () => Promise<void>)();

      // Unhealthy restart happened (session recreated)
      const reCreatedSessions = createSessionCalls.filter(
        (c) => c.sessionId === baseEntry.sessionId
      );
      expect(reCreatedSessions.length).toBeGreaterThanOrEqual(1);
    });

    it("skips RSS check when maxRssBytes is 0 (disabled)", async () => {
      let timeoutCallback: TimeoutCallback | null = null;
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
          timeoutCallback = callback as TimeoutCallback;
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      let _healthCallCount = 0;
      const stopCalls: number[] = [];

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          checkIntervalMs: 1000,
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
          maxRssBytes: 0,
          rssCheckIntervalMs: 0,
        },
        {
          readStateFile: async () => ({
            workers: {},
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            stopServeCalls: stopCalls,
            healthy: async () => {
              _healthCallCount += 1;
              return true;
            },
            getServePid: () => 12345,
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: originalFetch,
        }
      );

      if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
      await (timeoutCallback as () => Promise<void>)();

      // No restart should happen — serve is healthy and RSS check is disabled
      expect(stopCalls).toHaveLength(0);
    });

    it("skips RSS check when getServePid returns 0", async () => {
      let timeoutCallback: TimeoutCallback | null = null;
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
          timeoutCallback = callback as TimeoutCallback;
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      let _healthCallCount = 0;
      const stopCalls: number[] = [];

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          checkIntervalMs: 1000,
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
          maxRssBytes: 1 * 1024 * 1024 * 1024,
          rssCheckIntervalMs: 0,
        },
        {
          readStateFile: async () => ({
            workers: {},
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            stopServeCalls: stopCalls,
            healthy: async () => {
              _healthCallCount += 1;
              return true;
            },
            // PID 0 = not applicable (e.g., ClaudeCode adapter)
            getServePid: () => 0,
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: originalFetch,
        }
      );

      if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
      await (timeoutCallback as () => Promise<void>)();

      // No restart should happen — getServePid returns 0
      expect(stopCalls).toHaveLength(0);
    });
  });
});
