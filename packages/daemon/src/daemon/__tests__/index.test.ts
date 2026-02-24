import { afterEach, describe, expect, it, mock } from "bun:test";
import type { RuntimeAdapter } from "../runtime/types";
import type { WorkerEntry } from "../serve-manager";
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
  });

  it("re-creates sessions for persisted workers on startup", async () => {
    const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];

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

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        teamId: TEAM_ID,
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

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        teamId: TEAM_ID,
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

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        checkIntervalMs: 1000,
        teamId: TEAM_ID,
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

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        teamId: TEAM_ID,
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

    await startDaemon(
      {
        stateFilePath: "/tmp/daemon-workers.json",
        teamId: TEAM_ID,
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
