import { afterAll, afterEach, beforeEach, describe, expect, it, mock } from "bun:test";
import { createServer } from "node:net";
import { type DaemonConfig, resolveDaemonConfig } from "../config";
import { resolveLegionPaths } from "../paths";
import type { ResyncPassResult } from "../resync";
import type { RuntimeAdapter } from "../runtime/types";
import type { WorkerEntry } from "../serve-manager";
import type { ServerOptions } from "../server";
import type { PersistedWorkerState } from "../state-file";
import { createMockEnvoyServer, type MockEnvoyServer } from "./mock-envoy-server";

let currentMockEnvoy: MockEnvoyServer | null = null;

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

// Mirrors DEFAULT_RESYNC_INTERVAL_SECONDS * 1000 from config.ts (600s = 600_000ms)
const RESYNC_INTERVAL_MS = 600 * 1000;

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
  sessionExists?: (sessionId: string) => Promise<boolean>;
  getSessionStatus?: (sessionId: string) => Promise<{ data?: unknown; error?: unknown }>;
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
    getSessionStatus: overrides?.getSessionStatus ?? (async () => ({ data: undefined })),
    deleteSession: async () => {},
    sessionExists: overrides?.sessionExists ?? (async () => false),
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
    envoyUrl: overrides.envoyUrl ?? currentMockEnvoy?.url ?? "",
    reviewerAppId: 12345,
    reviewerAppLogin: "legion-reviewer[bot]",
    paths: TEST_PATHS,
    legionId,
    logDir: instancePaths.logDir,
    stateFilePath: overrides.stateFilePath ?? instancePaths.workersFile,
    ...overrides,
  };

  const testDeps: DaemonDepsOverride = {
    runResyncPass: async (): Promise<ResyncPassResult> => ({
      recommendations: [],
      errors: [],
    }),
    ...deps,
  };
  if (deps?.startServer) {
    const startServer = deps.startServer;
    testDeps.startServer = (...args) => ({
      refreshResyncIssueRefs: async () => {},
      listNonTerminalIssues: () => [],
      getLiveWorkers: async () => ({}),
      ...startServer(...args),
    });
  }

  return startDaemon(config, {
    readLegionsRegistry: async () => mockedRegistry,
    cleanupStaleServes: async () => ({}),
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
    deps: testDeps,
  });
}

describe("daemon entry", () => {
  const originalOn = process.on;
  const originalExit = process.exit;
  const originalFetch = globalThis.fetch;
  const startServerCalls: ServerOptions[] = [];
  let mockEnvoy: MockEnvoyServer;

  beforeEach(() => {
    mockEnvoy = createMockEnvoyServer();
    currentMockEnvoy = mockEnvoy;
  });

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
    mockEnvoy.stop();
    currentMockEnvoy = null;
  });

  describe("resync lifecycle", () => {
    it("runs once at startup and once for each configured timer tick without dispatching", async () => {
      const resyncIntervalMs = 2_000;
      const scheduledCallbacks: Array<{ callback: TimeoutCallback; delay: number }> = [];
      const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];
      const resyncPass = mock(
        async (): Promise<ResyncPassResult> => ({
          recommendations: [],
          errors: [],
        })
      );
      const refreshResyncIssueRefs = mock(async () => {});
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, delay?: number) => {
          scheduledCallbacks.push({ callback, delay: delay ?? 0 });
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      const handle = await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
          issueBackend: "github",
          resyncIntervalMs,
        },
        {
          readStateFile: async () => ({ workers: {}, crashHistory: {} }),
          writeStateFile: async () => {},
          adapter: makeAdapter({ createSessionCalls }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
            refreshResyncIssueRefs,
          }),
          runResyncPass: resyncPass,
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: originalFetch,
        }
      );

      expect(resyncPass).toHaveBeenCalledTimes(1);
      expect(refreshResyncIssueRefs).toHaveBeenCalledTimes(1);
      expect(createSessionCalls).toHaveLength(0);

      const resyncTimer = scheduledCallbacks.find(({ delay }) => delay === resyncIntervalMs);
      if (!resyncTimer) {
        throw new Error("Expected resync timer to be scheduled");
      }
      await resyncTimer.callback();

      expect(resyncPass).toHaveBeenCalledTimes(2);
      expect(refreshResyncIssueRefs).toHaveBeenCalledTimes(2);
      expect(createSessionCalls).toHaveLength(0);
      await handle.stop();
    });

    it("disables resync scheduling and startup execution for a Linear daemon", async () => {
      // Given a Linear-backed daemon with observable timers, startup output, and resync collaborators.
      const resyncIntervalMs = 2_000;
      const scheduledCallbacks: Array<{ callback: TimeoutCallback; delay: number }> = [];
      const logCalls: string[] = [];
      const originalLog = console.log;
      const resyncPass = mock(
        async (): Promise<ResyncPassResult> => ({ recommendations: [], errors: [] })
      );
      const refreshResyncIssueRefs = mock(async () => {});
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, delay?: number) => {
          scheduledCallbacks.push({ callback, delay: delay ?? 0 });
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      console.log = (...args: unknown[]) => {
        logCalls.push(args.map((arg) => String(arg)).join(" "));
      };

      try {
        // When the daemon starts.
        const handle = await startDaemonForTest(
          {
            stateFilePath: "/tmp/daemon-workers.json",
            legionId: TEAM_ID,
            controllerSessionId: "ses_test",
            issueBackend: "linear",
            resyncIntervalMs,
          },
          {
            readStateFile: async () => ({ workers: {}, crashHistory: {} }),
            writeStateFile: async () => {},
            adapter: makeAdapter(),
            startServer: () => ({
              server: { port: 15555 } as ReturnType<typeof Bun.serve>,
              stop: () => {},
              fetchAndProcessState: async () => {},
              refreshResyncIssueRefs,
            }),
            runResyncPass: resyncPass,
            setTimeout: mockSetTimeout,
            clearTimeout: () => {},
            fetch: originalFetch,
          }
        );

        // Then neither startup nor recurring resync runs, and the reason is logged once.
        expect(resyncPass).not.toHaveBeenCalled();
        expect(refreshResyncIssueRefs).not.toHaveBeenCalled();
        expect(scheduledCallbacks.some(({ delay }) => delay === resyncIntervalMs)).toBe(false);
        expect(
          logCalls.filter(
            (message) => message === "[resync] disabled: requires github issue backend"
          )
        ).toEqual(["[resync] disabled: requires github issue backend"]);
        await handle.stop();
      } finally {
        console.log = originalLog;
      }
    });

    it("logs errors returned by a completed resync pass", async () => {
      const originalError = console.error;
      const errorCalls: unknown[][] = [];
      console.error = (...args: unknown[]) => {
        errorCalls.push(args);
      };

      try {
        const handle = await startDaemonForTest(
          {
            stateFilePath: "/tmp/daemon-workers.json",
            legionId: TEAM_ID,
            controllerSessionId: "ses_test",
            issueBackend: "github",
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
            runResyncPass: async (): Promise<ResyncPassResult> => ({
              recommendations: [],
              errors: [{ issueId: "acme-api-42", message: "GitHub unavailable" }],
            }),
            setTimeout: silentSetTimeout,
            clearTimeout: noopClearTimeout,
            fetch: originalFetch,
          }
        );

        await Promise.resolve();

        expect(errorCalls.flat().join(" ")).toContain("GitHub unavailable");
        await handle.stop();
      } finally {
        console.error = originalError;
      }
    });

    it("logs a configuration error when the reviewer app identity is missing", async () => {
      const originalError = console.error;
      const errorCalls: unknown[][] = [];
      console.error = (...args: unknown[]) => {
        errorCalls.push(args);
      };

      try {
        const handle = await startDaemonForTest(
          {
            stateFilePath: "/tmp/daemon-workers.json",
            legionId: TEAM_ID,
            controllerSessionId: "ses_test",
            reviewerAppId: undefined,
            reviewerAppLogin: undefined,
            issueBackend: "github",
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

        await Promise.resolve();

        expect(errorCalls.flat().join(" ")).toContain("reviewerAppId");
        await handle.stop();
      } finally {
        console.error = originalError;
      }
    });

    it("publishes resync recommendations to the controller topic", async () => {
      const published: Array<{ topic: string; message: string }> = [];
      let resolveResyncStarted: () => void = () => {};
      const resyncStarted = new Promise<void>((resolve) => {
        resolveResyncStarted = resolve;
      });
      const recommendation = {
        issueId: "acme-api-42",
        mode: "test" as const,
        reason: "artifact_no_live_owner" as const,
      };
      const fetch = Object.assign(
        async (url: string | URL | Request, init?: RequestInit) => {
          if (new URL(String(url)).pathname === "/v1/messages/publish") {
            const body = JSON.parse(String(init?.body)) as { topic: string; message: string };
            published.push(body);
            return Response.json({ event_id: "test" });
          }
          return originalFetch(url, init);
        },
        { preconnect: originalFetch.preconnect }
      );

      const handle = await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
          issueBackend: "github",
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
          runResyncPass: async (deps): Promise<ResyncPassResult> => {
            await deps.emitToController({
              type: "legion.resync",
              recommendations: [recommendation],
              errors: [],
            });
            resolveResyncStarted();
            return { recommendations: [recommendation], errors: [] };
          },
          setTimeout: silentSetTimeout,
          clearTimeout: noopClearTimeout,
          fetch,
        }
      );

      await resyncStarted;

      expect(published).toEqual([
        {
          topic: "notifications.legion.controller",
          message: JSON.stringify({
            type: "legion.resync",
            recommendations: [recommendation],
            errors: [],
          }),
        },
      ]);
      await handle.stop();
    });

    it("uses LEGION_RESYNC_INTERVAL_SECONDS for the resync schedule", () => {
      const { config } = resolveDaemonConfig({
        env: { LEGION_RESYNC_INTERVAL_SECONDS: "25" },
      });

      expect(config.resyncIntervalMs).toBe(25_000);
    });

    it("reads the reviewer identity from Legion environment variables", () => {
      const { config } = resolveDaemonConfig({
        env: {
          LEGION_REVIEWER_APP_ID: "12345",
          LEGION_REVIEWER_APP_LOGIN: "legion-reviewer[bot]",
        },
      });

      expect(config.reviewerAppId).toBe(12345);
      expect(config.reviewerAppLogin).toBe("legion-reviewer[bot]");
    });
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
      expect(writeLegionEntryCalls[0].entry.servePort).toBeGreaterThanOrEqual(occupiedPort + 101);
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
            return {};
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

    const workerWithTopics: WorkerEntry = {
      ...baseEntry,
      envoyTopics: ["notifications.github.acme.widgets.issue.42.>"],
    };
    const workerWithoutTopics: WorkerEntry = {
      ...secondEntry,
      envoyTopics: undefined,
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
        fetch: originalFetch,
      }
    );

    mockEnvoy.subscribeCalls.length = 0;

    if (!timeoutCallback) {
      throw new Error("Expected health loop callback to be scheduled");
    }
    await (timeoutCallback as () => Promise<void>)();
    await Bun.sleep(100); // Flush fire-and-forget Envoy calls

    // Worker with topics should be re-subscribed
    const workerSubscriptions = mockEnvoy.subscribeCalls.filter(
      (c) => c.session_id === workerWithTopics.sessionId
    );
    expect(workerSubscriptions).toHaveLength(1);
    expect(workerSubscriptions[0].topics).toEqual(["notifications.github.acme.widgets.issue.42.>"]);

    // Worker without topics should NOT be subscribed
    const noTopicSubscriptions = mockEnvoy.subscribeCalls.filter(
      (c) => c.session_id === workerWithoutTopics.sessionId
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

    const workerWithTopics: WorkerEntry = {
      ...baseEntry,
      envoyTopics: ["notifications.github.acme.widgets.issue.42.>"],
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
        fetch: originalFetch,
      }
    );

    mockEnvoy.subscribeCalls.length = 0;

    if (!timeoutCallback) {
      throw new Error("Expected health loop callback to be scheduled");
    }
    await (timeoutCallback as () => Promise<void>)();

    // No envoy subscribes should have happened since all sessions failed to recreate
    const workerSubscriptions = mockEnvoy.subscribeCalls.filter(
      (c) => c.session_id === workerWithTopics.sessionId
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
        issueBackend: "github",
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
    expect(clearedTimeout).toBe(2);
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
  it("does not stop serve when restartFn triggers shutdown with keepServe", async () => {
    const stopServeCalls: number[] = [];
    let restartFnCaptured: (() => void | Promise<void>) | undefined;
    let exitCode: number | null = null;

    process.exit = ((code?: number) => {
      exitCode = code ?? 0;
      return undefined as never;
    }) as typeof process.exit;

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
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
          stopServeCalls,
          healthy: async () => true,
        }),
        startServer: (opts) => {
          restartFnCaptured = opts.restartFn;
          return {
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
          };
        },
        setTimeout: ((cb: () => void) => {
          // Execute the callback immediately for testing
          void Promise.resolve().then(cb);
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      }
    );

    expect(restartFnCaptured).toBeDefined();
    await restartFnCaptured?.();

    // Wait for the async setTimeout callback to execute
    await new Promise((resolve) => globalThis.setTimeout(resolve, 50));

    // Serve should NOT have been stopped (keepServe=true)
    expect(stopServeCalls).toHaveLength(0);
    // Registry entry should be preserved so cleanupStaleServes can find the serve
    expect(removeLegionEntryCalls).toHaveLength(0);
    // But the process should have exited
    expect(exitCode).not.toBeNull();
    expect(exitCode as unknown as number).toBe(0);
  });

  it("adopts preserved serve PID from cleanupStaleServes", async () => {
    const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];
    let adapterStartCalled = false;

    await startDaemonForTest(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        legionId: TEAM_ID,
        controllerSessionId: "ses_test",
      },
      {
        readStateFile: async () => ({
          workers: { [baseEntry.id]: baseEntry },
          crashHistory: {},
        }),
        writeStateFile: async () => {},
        adapter: Object.assign(
          makeAdapter({
            createSessionCalls,
            healthy: async () => true,
          }),
          {
            start: async () => {
              adapterStartCalled = true;
            },
            configure: () => {},
            adoptServe: (pid: number) => {
              // Verify adoptServe is called with the preserved PID
              expect(pid).toBe(42424);
            },
          }
        ) as RuntimeAdapter,
        startServer: () => ({
          server: { port: 15555 } as ReturnType<typeof Bun.serve>,
          stop: () => {},
        }),
        setTimeout: silentSetTimeout,
        clearTimeout: noopClearTimeout,
        fetch: originalFetch,
      },
      {
        cleanupStaleServes: async () => ({
          preservedServePid: 42424,
          preservedServePort: 13381,
        }),
      }
    );

    // Adapter.start should NOT have been called (serve was already healthy)
    expect(adapterStartCalled).toBe(false);
    // Sessions should have been recreated
    expect(createSessionCalls.length).toBeGreaterThan(0);
  });

  it("passes serve env vars to adapter.start on startup", async () => {
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
    expect(JSON.parse(opts.env?.OPENCODE_CONFIG_CONTENT ?? "null")).toEqual({
      plugin: ["@sjawhar/opencode-legion@latest", "@sjawhar/opencode-legion-envoy@latest"],
    });
  });

  it("passes serve env vars to adapter.start on health restart", async () => {
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
    expect(JSON.parse(restartOpts.env?.OPENCODE_CONFIG_CONTENT ?? "null")).toEqual({
      plugin: ["@sjawhar/opencode-legion@latest", "@sjawhar/opencode-legion-envoy@latest"],
    });
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

    it("marks running worker as dead when session is missing from serve", async () => {
      const timeoutCallbacks: TimeoutCallback[] = [];
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, delay?: number, ..._args: unknown[]) => {
          if (delay !== RESYNC_INTERVAL_MS) {
            timeoutCallbacks.push(callback);
          }
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      const patchCalls: Array<{ url: string; body: Record<string, unknown> }> = [];
      const workerSessionId = "ses_abc123def456ABCDEFGHIJKLMN";
      const workerId = "test-repo-42-implement";
      const workerEntry: WorkerEntry = {
        ...baseEntry,
        id: workerId,
        sessionId: workerSessionId,
        status: "running",
      };

      const daemonPort = 13370;
      mockedAllocatedPorts = { daemonPort, servePort: 13381 };

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({
            workers: { [workerId]: workerEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            // Session does not match worker → worker should be reaped
            sessionExists: async () => false,
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: Object.assign(
            async (url: string | URL | Request, init?: RequestInit) => {
              const urlStr = String(url);
              if (init?.method === "PATCH" && urlStr.includes(`/workers/${workerId}`)) {
                const body = JSON.parse(init.body as string) as Record<string, unknown>;
                patchCalls.push({ url: urlStr, body });
                return new Response(JSON.stringify({ ok: true }), { status: 200 });
              }
              return originalFetch(url, init);
            },
            { preconnect: originalFetch.preconnect }
          ),
        }
      );

      if (!timeoutCallbacks[0]) throw new Error("Expected health loop callback to be scheduled");
      // Need 3 consecutive ticks for the failure threshold
      await (timeoutCallbacks[0] as () => Promise<void>)();
      await (timeoutCallbacks[1] as () => Promise<void>)();
      await (timeoutCallbacks[2] as () => Promise<void>)();

      expect(patchCalls).toHaveLength(1);
      expect(patchCalls[0].body.status).toBe("dead");
      expect(patchCalls[0].body.crashCount).toBe(workerEntry.crashCount + 1);
    });

    it("does not mark worker dead when session is present in serve", async () => {
      let timeoutCallback: TimeoutCallback | null = null;
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
          timeoutCallback = callback as TimeoutCallback;
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      const patchCalls: Array<{ url: string }> = [];
      const workerSessionId = "ses_abc123def456ABCDEFGHIJKLMN";
      const workerId = "test-repo-42-implement";
      const workerEntry: WorkerEntry = {
        ...baseEntry,
        id: workerId,
        sessionId: workerSessionId,
        status: "running",
      };

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({
            workers: { [workerId]: workerEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            // Session IS the worker → worker should NOT be reaped
            sessionExists: async (id) => id === workerSessionId,
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: Object.assign(
            async (url: string | URL | Request, init?: RequestInit) => {
              const urlStr = String(url);
              if (init?.method === "PATCH" && urlStr.includes("/workers/")) {
                patchCalls.push({ url: urlStr });
              }
              return originalFetch(url, init);
            },
            { preconnect: originalFetch.preconnect }
          ),
        }
      );

      if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
      await (timeoutCallback as () => Promise<void>)();

      expect(patchCalls).toHaveLength(0);
    });

    it("skips liveness sweep when serve is unhealthy", async () => {
      let timeoutCallback: TimeoutCallback | null = null;
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
          timeoutCallback = callback as TimeoutCallback;
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      const sessionExistsCalls: number[] = [];
      const workerEntry: WorkerEntry = {
        ...baseEntry,
        status: "running",
      };

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({
            workers: { [workerEntry.id]: workerEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            healthy: async () => false,
            sessionExists: async () => {
              sessionExistsCalls.push(1);
              return false;
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

      if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
      await (timeoutCallback as () => Promise<void>)();

      // Liveness sweep must not run when serve is unhealthy (AC3)
      expect(sessionExistsCalls).toHaveLength(0);
    });

    it("skips liveness sweep when serve was just restarted", async () => {
      let timeoutCallback: TimeoutCallback | null = null;
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
          timeoutCallback = callback as TimeoutCallback;
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      const sessionExistsCalls: number[] = [];
      // Track whether the health tick has started (vs startup calls)
      let tickStarted = false;
      let tickHealthCallCount = 0;
      const workerEntry: WorkerEntry = {
        ...baseEntry,
        status: "running",
      };

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({
            workers: { [workerEntry.id]: workerEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            healthy: async () => {
              if (!tickStarted) return true; // startup calls: always healthy
              tickHealthCallCount += 1;
              // First tick call: unhealthy (triggers restart)
              return tickHealthCallCount > 1;
            },
            sessionExists: async () => {
              sessionExistsCalls.push(1);
              return false;
            },
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: Object.assign(
            (callback: TimeoutCallback, delay?: number, ...args: unknown[]) => {
              // Capture the callback and mark tick as started
              tickStarted = true;
              return mockSetTimeout(callback, delay, ...args);
            },
            { __promisify__: setTimeout.__promisify__ }
          ) as unknown as typeof setTimeout,
          clearTimeout: () => {},
          fetch: originalFetch,
        }
      );

      if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
      await (timeoutCallback as () => Promise<void>)();

      // Liveness sweep must not run when serve was just restarted (AC3)
      expect(sessionExistsCalls).toHaveLength(0);
    });

    it("skips liveness sweep and marks no workers dead when sessionExists throws", async () => {
      let timeoutCallback: TimeoutCallback | null = null;
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, _delay?: number, ..._args: unknown[]) => {
          timeoutCallback = callback as TimeoutCallback;
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      const patchCalls: Array<{ url: string }> = [];
      const workerEntry: WorkerEntry = {
        ...baseEntry,
        status: "running",
      };

      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({
            workers: { [workerEntry.id]: workerEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            sessionExists: async () => {
              throw new Error("network error");
            },
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: Object.assign(
            async (url: string | URL | Request, init?: RequestInit) => {
              const urlStr = String(url);
              if (init?.method === "PATCH" && urlStr.includes("/workers/")) {
                patchCalls.push({ url: urlStr });
              }
              return originalFetch(url, init);
            },
            { preconnect: originalFetch.preconnect }
          ),
        }
      );

      if (!timeoutCallback) throw new Error("Expected health loop callback to be scheduled");
      await (timeoutCallback as () => Promise<void>)();

      // AC4: transient error → no workers marked dead
      expect(patchCalls).toHaveLength(0);
    });
  });

  describe("liveness sweep hardening", () => {
    it("does not reap workers within 120s startup grace period", async () => {
      const timeoutCallbacks: TimeoutCallback[] = [];
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, delay?: number, ..._args: unknown[]) => {
          if (delay !== RESYNC_INTERVAL_MS) {
            timeoutCallbacks.push(callback);
          }
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      const patchCalls: Array<{ url: string; body: Record<string, unknown> }> = [];
      const workerId = "test-repo-42-implement";
      const workerSessionId = "ses_abc123def456ABCDEFGHIJKLMN";

      // Worker started 90s ago — past the current 60s grace but within the desired 120s
      const workerEntry: WorkerEntry = {
        ...baseEntry,
        id: workerId,
        sessionId: workerSessionId,
        startedAt: new Date(Date.now() - 90_000).toISOString(),
        status: "running",
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
            workers: { [workerId]: workerEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            healthy: async () => true,
            sessionExists: async () => false,
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
            cleanupDeadWorkers: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: Object.assign(
            async (url: string | URL | Request, init?: RequestInit) => {
              const urlStr = String(url);
              if (init?.method === "PATCH" && urlStr.includes("/workers/")) {
                const body = JSON.parse(init.body as string) as Record<string, unknown>;
                patchCalls.push({ url: urlStr, body });
                return new Response(JSON.stringify({ ok: true }), { status: 200 });
              }
              return originalFetch(url, init);
            },
            { preconnect: originalFetch.preconnect }
          ),
        }
      );

      if (!timeoutCallbacks[0]) throw new Error("Expected health loop callback");
      await (timeoutCallbacks[0] as () => Promise<void>)();

      // Worker should NOT be reaped — it's within the 120s grace period
      const deadPatches = patchCalls.filter((c) => c.body.status === "dead");
      expect(deadPatches).toHaveLength(0);
    });

    it("only reaps workers after 3 consecutive liveness failures", async () => {
      const timeoutCallbacks: TimeoutCallback[] = [];
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, delay?: number, ..._args: unknown[]) => {
          if (delay !== RESYNC_INTERVAL_MS) {
            timeoutCallbacks.push(callback);
          }
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      const patchCalls: Array<{ url: string; body: Record<string, unknown> }> = [];
      const workerId = "test-repo-42-implement";
      const workerSessionId = "ses_abc123def456ABCDEFGHIJKLMN";

      // Worker started 200s ago — well past any grace period
      const workerEntry: WorkerEntry = {
        ...baseEntry,
        id: workerId,
        sessionId: workerSessionId,
        startedAt: new Date(Date.now() - 200_000).toISOString(),
        status: "running",
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
            workers: { [workerId]: workerEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            healthy: async () => true,
            sessionExists: async () => false,
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
            cleanupDeadWorkers: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: Object.assign(
            async (url: string | URL | Request, init?: RequestInit) => {
              const urlStr = String(url);
              if (init?.method === "PATCH" && urlStr.includes("/workers/")) {
                const body = JSON.parse(init.body as string) as Record<string, unknown>;
                patchCalls.push({ url: urlStr, body });
                return new Response(JSON.stringify({ ok: true }), { status: 200 });
              }
              return originalFetch(url, init);
            },
            { preconnect: originalFetch.preconnect }
          ),
        }
      );

      // First health tick — miss 1, should NOT reap
      await (timeoutCallbacks[0] as () => Promise<void>)();
      expect(patchCalls.filter((c) => c.body.status === "dead")).toHaveLength(0);

      // Second health tick — miss 2, should NOT reap
      await (timeoutCallbacks[1] as () => Promise<void>)();
      expect(patchCalls.filter((c) => c.body.status === "dead")).toHaveLength(0);

      // Third health tick — miss 3, NOW should reap
      await (timeoutCallbacks[2] as () => Promise<void>)();
      expect(patchCalls.filter((c) => c.body.status === "dead")).toHaveLength(1);
    });

    it("resets consecutive miss counter when worker session reappears", async () => {
      const timeoutCallbacks: TimeoutCallback[] = [];
      const mockSetTimeout: typeof setTimeout = Object.assign(
        ((callback: TimeoutCallback, delay?: number, ..._args: unknown[]) => {
          if (delay !== RESYNC_INTERVAL_MS) {
            timeoutCallbacks.push(callback);
          }
          return {} as ReturnType<typeof setTimeout>;
        }) as unknown as typeof setTimeout,
        { __promisify__: setTimeout.__promisify__ }
      );

      const patchCalls: Array<{ url: string; body: Record<string, unknown> }> = [];
      const workerId = "test-repo-42-implement";
      const workerSessionId = "ses_abc123def456ABCDEFGHIJKLMN";

      const workerEntry: WorkerEntry = {
        ...baseEntry,
        id: workerId,
        sessionId: workerSessionId,
        startedAt: new Date(Date.now() - 200_000).toISOString(),
        status: "running",
      };

      let listCallCount = 0;
      await startDaemonForTest(
        {
          stateFilePath: "/tmp/daemon-workers.json",
          checkIntervalMs: 1000,
          legionId: TEAM_ID,
          controllerSessionId: "ses_test",
        },
        {
          readStateFile: async () => ({
            workers: { [workerId]: workerEntry },
            crashHistory: {},
          }),
          writeStateFile: async () => {},
          adapter: makeAdapter({
            healthy: async () => true,
            sessionExists: async (id) => {
              listCallCount++;
              // Tick 1: missing, Tick 2: missing, Tick 3: present, Tick 4: missing, Tick 5: missing
              if (listCallCount === 3) return id === workerSessionId;
              return false;
            },
          }),
          startServer: () => ({
            server: { port: 15555 } as ReturnType<typeof Bun.serve>,
            stop: () => {},
            fetchAndProcessState: async () => {},
            cleanupDeadWorkers: async () => {},
          }),
          setTimeout: mockSetTimeout,
          clearTimeout: () => {},
          fetch: Object.assign(
            async (url: string | URL | Request, init?: RequestInit) => {
              const urlStr = String(url);
              if (init?.method === "PATCH" && urlStr.includes("/workers/")) {
                const body = JSON.parse(init.body as string) as Record<string, unknown>;
                patchCalls.push({ url: urlStr, body });
                return new Response(JSON.stringify({ ok: true }), { status: 200 });
              }
              return originalFetch(url, init);
            },
            { preconnect: originalFetch.preconnect }
          ),
        }
      );

      // Tick 1: miss 1 — no reap
      await (timeoutCallbacks[0] as () => Promise<void>)();
      expect(patchCalls).toHaveLength(0);

      // Tick 2: miss 2 — no reap
      await (timeoutCallbacks[1] as () => Promise<void>)();
      expect(patchCalls).toHaveLength(0);

      // Tick 3: session reappears — counter resets
      await (timeoutCallbacks[2] as () => Promise<void>)();
      expect(patchCalls).toHaveLength(0);

      // Tick 4: miss 1 again (counter was reset) — no reap
      await (timeoutCallbacks[3] as () => Promise<void>)();
      expect(patchCalls).toHaveLength(0);

      // Tick 5: miss 2 — still no reap (need 3 consecutive)
      await (timeoutCallbacks[4] as () => Promise<void>)();
      expect(patchCalls).toHaveLength(0);
    });
  });
});
