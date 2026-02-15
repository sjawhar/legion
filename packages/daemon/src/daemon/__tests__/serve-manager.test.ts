import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  adoptExistingWorkers,
  createWorkerClient,
  healthCheck,
  initializeSession,
  killWorker,
  spawnServe,
} from "../serve-manager";

const baseEntry = {
  id: "eng-42-implement",
  port: 15001,
  pid: 2222,
  sessionId: "ses_test",
  workspace: "/tmp/test-workspace",
  startedAt: "2026-01-01T00:00:00.000Z",
  status: "running" as const,
  crashCount: 0,
  lastCrashAt: null,
};

describe("serve-manager", () => {
  const originalSpawn = Bun.spawn;
  const originalFetch = globalThis.fetch;
  const originalKill = process.kill;

  afterEach(() => {
    Bun.spawn = originalSpawn;
    globalThis.fetch = originalFetch;
    process.kill = originalKill;
  });

  it("spawns a serve process and returns worker entry", async () => {
    const spawnArgs = {
      cmd: [] as string[],
      options: {} as any,
      called: false,
    };
    Bun.spawn = ((cmd: string[], options: any) => {
      spawnArgs.cmd = cmd;
      spawnArgs.options = options;
      spawnArgs.called = true;
      return { pid: 4242 } as any;
    }) as typeof Bun.spawn;

    const entry = await spawnServe({
      issueId: "ENG-42",
      mode: "implement",
      workspace: "/tmp",
      port: 14000,
      sessionId: "ses_123",
      env: { EXTRA_FLAG: "1" },
    });

    expect(entry.id).toBe("eng-42-implement");
    expect(entry.port).toBe(14000);
    expect(entry.pid).toBe(4242);
    expect(entry.sessionId).toBe("ses_123");
    expect(new Date(entry.startedAt).toISOString()).toBe(entry.startedAt);
    expect(entry.status).toBe("starting");

    expect(spawnArgs.called).toBe(true);
    expect(spawnArgs.cmd).toEqual(["opencode", "serve", "--port", "14000"]);
    expect(spawnArgs.options.cwd).toBe("/tmp");
    expect(spawnArgs.options.env.EXTRA_FLAG).toBe("1");
  });

  it("sets SUPERPOWERS_SKIP_BOOTSTRAP for implement mode", async () => {
    const spawnArgs = {
      options: {} as any,
    };
    Bun.spawn = ((_: string[], options: any) => {
      spawnArgs.options = options;
      return { pid: 4243 } as any;
    }) as typeof Bun.spawn;

    await spawnServe({
      issueId: "ENG-43",
      mode: "implement",
      workspace: "/tmp",
      port: 14001,
      sessionId: "ses_124",
    });

    expect(spawnArgs.options.env.SUPERPOWERS_SKIP_BOOTSTRAP).toBe("1");
  });

  it("sets OPENCODE_PERMISSION denies for implement mode", async () => {
    const spawnArgs = {
      options: {} as any,
    };
    Bun.spawn = ((_: string[], options: any) => {
      spawnArgs.options = options;
      return { pid: 4244 } as any;
    }) as typeof Bun.spawn;

    await spawnServe({
      issueId: "ENG-44",
      mode: "implement",
      workspace: "/tmp",
      port: 14002,
      sessionId: "ses_125",
    });

    const permissions = JSON.parse(spawnArgs.options.env.OPENCODE_PERMISSION);
    expect(permissions.skill["superpowers/brainstorming"]).toBe("deny");
    expect(permissions.skill["superpowers/writing-plans"]).toBe("deny");
  });

  it("sets OPENCODE_PERMISSION denies for plan mode", async () => {
    const spawnArgs = {
      options: {} as any,
    };
    Bun.spawn = ((_: string[], options: any) => {
      spawnArgs.options = options;
      return { pid: 4245 } as any;
    }) as typeof Bun.spawn;

    await spawnServe({
      issueId: "ENG-45",
      mode: "plan",
      workspace: "/tmp",
      port: 14003,
      sessionId: "ses_126",
    });

    const permissions = JSON.parse(spawnArgs.options.env.OPENCODE_PERMISSION);
    expect(permissions.skill["superpowers/brainstorming"]).toBe("deny");
    expect(permissions.skill["superpowers/executing-plans"]).toBe("deny");
  });

  it("omits OPENCODE_PERMISSION for merge mode", async () => {
    const spawnArgs = {
      options: {} as any,
    };
    Bun.spawn = ((_: string[], options: any) => {
      spawnArgs.options = options;
      return { pid: 4246 } as any;
    }) as typeof Bun.spawn;

    await spawnServe({
      issueId: "ENG-46",
      mode: "merge",
      workspace: "/tmp",
      port: 14004,
      sessionId: "ses_127",
    });

    expect(spawnArgs.options.env.OPENCODE_PERMISSION).toBeUndefined();
  });

  it("gracefully disposes and returns when process exits", async () => {
    const calls = {
      disposeUrl: "",
      disposeMethod: "",
      signals: [] as (number | undefined | NodeJS.Signals)[],
    };
    globalThis.fetch = (async (input: Request | string, init?: RequestInit) => {
      calls.disposeUrl = typeof input === "string" ? input : input.url;
      calls.disposeMethod = typeof input === "string" ? (init?.method ?? "GET") : input.method;
      return new Response(null, { status: 200 });
    }) as unknown as typeof fetch;

    process.kill = ((_: number, signal?: NodeJS.Signals) => {
      calls.signals.push(signal);
      const err = new Error("ESRCH") as NodeJS.ErrnoException;
      err.code = "ESRCH";
      throw err;
    }) as typeof process.kill;

    await killWorker(baseEntry, 50, 10, 100);

    expect(calls.disposeUrl).toBe(`http://127.0.0.1:${baseEntry.port}/global/dispose`);
    expect(calls.disposeMethod).toBe("POST");
    expect(calls.signals).toEqual([0]);
  });

  it("sends SIGKILL when dispose succeeds but process lingers", async () => {
    const calls = { sigkill: false, signalChecks: 0 };
    globalThis.fetch = (async () => new Response(null, { status: 200 })) as unknown as typeof fetch;

    process.kill = ((_: number, signal?: NodeJS.Signals) => {
      if (signal === "SIGKILL") {
        calls.sigkill = true;
        return true;
      }
      calls.signalChecks += 1;
      return true;
    }) as typeof process.kill;

    await killWorker(baseEntry, 50, 10, 100);

    expect(calls.signalChecks).toBeGreaterThan(0);
    expect(calls.sigkill).toBe(true);
  });

  it("sends SIGKILL when dispose fails and process lingers", async () => {
    globalThis.fetch = (async () => {
      throw new Error("connect ECONNREFUSED");
    }) as unknown as typeof fetch;

    const calls = { sigkill: false, signalChecks: 0 };
    process.kill = ((_: number, signal?: NodeJS.Signals) => {
      if (signal === "SIGKILL") {
        calls.sigkill = true;
        return true;
      }
      calls.signalChecks += 1;
      return true;
    }) as typeof process.kill;

    await killWorker(baseEntry, 50, 10, 100);

    expect(calls.signalChecks).toBeGreaterThan(0);
    expect(calls.sigkill).toBe(true);
  });

  it("returns immediately when process already exited", async () => {
    globalThis.fetch = (async () => {
      throw new Error("connect ECONNREFUSED");
    }) as unknown as typeof fetch;

    const calls = { sigkill: false, signalChecks: 0 };
    process.kill = ((_: number, signal?: NodeJS.Signals) => {
      if (signal === "SIGKILL") {
        calls.sigkill = true;
        return true;
      }
      calls.signalChecks += 1;
      const err = new Error("ESRCH") as NodeJS.ErrnoException;
      err.code = "ESRCH";
      throw err;
    }) as typeof process.kill;

    await killWorker(baseEntry, 50, 10, 100);

    expect(calls.signalChecks).toBe(1);
    expect(calls.sigkill).toBe(false);
  });

  it("sends x-opencode-directory header during session initialization", async () => {
    let capturedHeaders: Record<string, string> = {};
    let capturedBody: Record<string, unknown> = {};
    let healthChecked = false;

    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      if (url.includes("/global/health")) {
        healthChecked = true;
        return {
          ok: true,
          json: async () => ({ healthy: true }),
        } as any;
      }
      if (url.includes("/session")) {
        capturedHeaders = Object.fromEntries(
          Object.entries(init?.headers ?? {}).map(([k, v]) => [k.toLowerCase(), v])
        );
        capturedBody = JSON.parse(init?.body as string);
        return { ok: true, json: async () => ({}) } as any;
      }
      return { ok: false } as any;
    }) as unknown as typeof fetch;

    await initializeSession(15000, "ses_test123", "/home/user/workspace");

    expect(healthChecked).toBe(true);
    expect(capturedHeaders["x-opencode-directory"]).toBe("/home/user/workspace");
    expect(capturedBody.id).toBe("ses_test123");
  });

  it("creates an SDK client with workspace directory", () => {
    const client = createWorkerClient(15000, "/home/user/my-workspace");
    expect(client).toBeDefined();
    expect(client.session).toBeDefined();
    expect(client.session.status).toBeFunction();
    // promptAsync existence is validated by typecheck; runtime check is
    // unreliable across Bun versions due to lazy prototype resolution.
  });

  it("checks health via /global/health", async () => {
    globalThis.fetch = (async (url: string) => {
      expect(url).toBe("http://127.0.0.1:15000/global/health");
      return {
        ok: true,
        json: async () => ({ healthy: true, version: "test" }),
      } as any;
    }) as unknown as typeof fetch;

    const ok = await healthCheck(15000, 500);
    expect(ok).toBe(true);
  });

  it("returns false when health check throws", async () => {
    globalThis.fetch = (async () => {
      throw new Error("boom");
    }) as unknown as typeof fetch;

    const ok = await healthCheck(15001, 500);
    expect(ok).toBe(false);
  });

  it("adopts only healthy workers from state file", async () => {
    const tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-workers-"));
    const filePath = path.join(tempDir, "workers.json");
    const state = {
      workers: {
        "eng-42-implement": baseEntry,
        "eng-99-implement": { ...baseEntry, id: "eng-99-implement", port: 16000 },
      },
      crashHistory: {
        "eng-99-implement": { crashCount: 2, lastCrashAt: "2026-01-02T00:00:00.000Z" },
      },
    };
    await writeFile(filePath, JSON.stringify(state, null, 2));

    globalThis.fetch = (async (url: string) => {
      if (url.includes(":15001/")) {
        return {
          ok: true,
          json: async () => ({ healthy: true }),
        } as any;
      }
      return {
        ok: false,
        json: async () => ({ healthy: false }),
      } as any;
    }) as unknown as typeof fetch;

    const adopted = await adoptExistingWorkers(filePath);
    expect(adopted.workers.size).toBe(1);
    const adoptedEntry = adopted.workers.get("eng-42-implement");
    expect(adoptedEntry?.status).toBe("running");
    expect(adopted.crashHistory["eng-99-implement"]).toEqual({
      crashCount: 2,
      lastCrashAt: "2026-01-02T00:00:00.000Z",
    });

    await rm(tempDir, { recursive: true, force: true });
  });
});
