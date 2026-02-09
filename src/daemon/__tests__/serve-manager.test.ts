import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { adoptExistingWorkers, healthCheck, killWorker, spawnServe } from "../serve-manager";

const baseEntry = {
  id: "eng-42-implement",
  port: 15001,
  pid: 2222,
  sessionId: "ses_test",
  startedAt: "2026-01-01T00:00:00.000Z",
  status: "running" as const,
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

  it("kills a worker by pid", async () => {
    const killed = { pid: 0, called: false };
    process.kill = ((pid: number, _signal?: NodeJS.Signals) => {
      killed.pid = pid;
      killed.called = true;
      return true;
    }) as typeof process.kill;

    await killWorker(baseEntry);
    expect(killed.called).toBe(true);
    expect(killed.pid).toBe(baseEntry.pid);
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
      "eng-42-implement": baseEntry,
      "eng-99-implement": { ...baseEntry, id: "eng-99-implement", port: 16000 },
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
    expect(adopted.size).toBe(1);
    const adoptedEntry = adopted.get("eng-42-implement");
    expect(adoptedEntry?.status).toBe("running");

    await rm(tempDir, { recursive: true, force: true });
  });
});
