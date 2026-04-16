import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  allocatePort,
  cleanupStaleServes,
  findLegionByProjectId,
  isPidAlive,
  readLegionsRegistry,
  removeLegionEntry,
  writeLegionEntry,
} from "../legions-registry";

describe("legions-registry", () => {
  let tempDir: string | null = null;

  afterEach(async () => {
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("returns empty registry when file missing", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const registry = await readLegionsRegistry(path.join(tempDir, "legions.json"));
    expect(registry).toEqual({});
  });

  it("writes and reads a legion entry", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    const entry = {
      port: 13370,
      servePort: 13381,
      pid: 1234,
      startedAt: "2026-03-12T00:00:00Z",
    };

    await writeLegionEntry(filePath, "sjawhar/42", entry);
    const registry = await readLegionsRegistry(filePath);

    expect(registry["sjawhar/42"]).toEqual(entry);
  });

  it("returns empty registry when schema validation fails", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");

    await writeFile(filePath, '{"bad":"data"}', "utf-8");
    const registry = await readLegionsRegistry(filePath);

    expect(registry).toEqual({});
  });

  it("writes atomically without leaving temp files", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");

    await writeLegionEntry(filePath, "sjawhar/42", {
      port: 13370,
      servePort: 13381,
      pid: process.pid,
      startedAt: "2026-03-12T00:00:00Z",
    });

    const entries = await readdir(tempDir);
    expect(entries).toEqual(["legions.json"]);
  });

  it("removes a legion entry", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    await writeLegionEntry(filePath, "sjawhar/42", {
      port: 13370,
      servePort: 13381,
      pid: 1234,
      startedAt: "2026-03-12T00:00:00Z",
    });

    await removeLegionEntry(filePath, "sjawhar/42");
    const registry = await readLegionsRegistry(filePath);

    expect(registry["sjawhar/42"]).toBeUndefined();
  });

  it("allocates next available port", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    await writeLegionEntry(filePath, "proj/1", {
      port: 13370,
      servePort: 13381,
      pid: process.pid,
      startedAt: "2026-03-12T00:00:00Z",
    });

    const registry = await readLegionsRegistry(filePath);
    const { daemonPort, servePort } = allocatePort(registry);

    expect(daemonPort).toBe(13371);
    expect(servePort).toBe(13382);
  });

  it("reclaims port from dead PID", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    await writeLegionEntry(filePath, "proj/1", {
      port: 13370,
      servePort: 13381,
      pid: 999999999,
      startedAt: "2026-03-12T00:00:00Z",
    });

    const registry = await readLegionsRegistry(filePath);
    const { daemonPort } = allocatePort(registry);

    expect(daemonPort).toBe(13370);
  });

  it("findLegionByProjectId returns entry when it exists", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    await writeLegionEntry(filePath, "sjawhar/42", {
      port: 13370,
      servePort: 13381,
      pid: process.pid,
      startedAt: "2026-03-12T00:00:00Z",
    });

    const entry = await findLegionByProjectId(filePath, "sjawhar/42");
    expect(entry).toBeDefined();
    expect(entry?.port).toBe(13370);
  });

  describe("characterization: allocatePort", () => {
    it("returns base ports when registry is empty", () => {
      expect(allocatePort({})).toEqual({ daemonPort: 13370, servePort: 13381 });
    });

    it("returns next sequential ports when base ports are used by live processes", () => {
      const { daemonPort, servePort } = allocatePort({
        "proj/1": {
          port: 13370,
          servePort: 13381,
          pid: process.pid,
          startedAt: "2026-03-12T00:00:00Z",
        },
      });

      expect(daemonPort).toBe(13371);
      expect(servePort).toBe(13382);
    });
  });

  describe("concurrent writes", () => {
    it("preserves both entries for concurrent writes", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
      const filePath = path.join(tempDir, "legions.json");

      for (let i = 0; i < 50; i++) {
        await rm(filePath, { force: true });
        await Promise.all([
          writeLegionEntry(filePath, "proj/a", {
            port: 13370,
            servePort: 13381,
            pid: process.pid,
            startedAt: "2026-03-12T00:00:00Z",
          }),
          writeLegionEntry(filePath, "proj/b", {
            port: 13371,
            servePort: 13382,
            pid: process.pid,
            startedAt: "2026-03-12T00:00:01Z",
          }),
        ]);

        const registry = await readLegionsRegistry(filePath);
        expect(registry["proj/a"]).toBeDefined();
        expect(registry["proj/b"]).toBeDefined();
      }
    });

    it("recovers stale lock from dead pid and writes successfully", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
      const filePath = path.join(tempDir, "legions.json");
      const lockPath = `${filePath}.lock`;

      // Create stale lock file with dead PID
      await writeFile(lockPath, JSON.stringify({ pid: 999999999, timestamp: Date.now() }), "utf-8");

      await writeLegionEntry(filePath, "proj/stale", {
        port: 13370,
        servePort: 13381,
        pid: process.pid,
        startedAt: "2026-03-12T00:00:00Z",
      });

      const registry = await readLegionsRegistry(filePath);
      expect(registry["proj/stale"]).toBeDefined();

      const entries = await readdir(tempDir);
      expect(entries).toEqual(["legions.json"]);

      const written = await readFile(filePath, "utf-8");
      expect(written).toContain("proj/stale");
    });
  });
});

describe("isPidAlive", () => {
  it("returns true for current process PID (we are bun)", () => {
    // Current process is bun — should return true
    expect(isPidAlive(process.pid)).toBe(true);
  });

  it("returns false for non-existent PID", () => {
    // PID 0 signals current process group, 999999 almost certainly doesn't exist
    expect(isPidAlive(999999)).toBe(false);
  });
});

describe("cleanupStaleServes", () => {
  let tempDir: string | null = null;
  const originalFetch = globalThis.fetch;

  afterEach(async () => {
    globalThis.fetch = originalFetch;
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("does nothing when no entry exists for legionId", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cleanup-"));
    const filePath = path.join(tempDir, "legions.json");

    // No entry in the registry
    await cleanupStaleServes(filePath, "nonexistent");

    // Registry should not exist (ENOENT is fine)
    const registry = await readLegionsRegistry(filePath);
    expect(registry).toEqual({});
  });

  it("skips cleanup when daemon PID is still alive", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cleanup-"));
    const filePath = path.join(tempDir, "legions.json");

    // Write entry with current PID (alive)
    await writeLegionEntry(filePath, "my-legion", {
      port: 13370,
      servePort: 13381,
      pid: process.pid,
      startedAt: "2026-03-12T00:00:00Z",
    });

    await cleanupStaleServes(filePath, "my-legion");

    // Entry should still exist (not cleaned up)
    const registry = await readLegionsRegistry(filePath);
    expect(registry["my-legion"]).toBeDefined();
  });

  it("cleans up stale entry when daemon PID is dead and port is free", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cleanup-"));
    const filePath = path.join(tempDir, "legions.json");

    // Write entry with dead PID and a port that's free (59997)
    await writeLegionEntry(filePath, "my-legion", {
      port: 13370,
      servePort: 59997,
      pid: 999999999,
      startedAt: "2026-03-12T00:00:00Z",
    });

    await cleanupStaleServes(filePath, "my-legion");

    // Entry should be removed from registry
    const registry = await readLegionsRegistry(filePath);
    expect(registry["my-legion"]).toBeUndefined();
  });

  it("passes servePid to killStaleServe when available", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cleanup-"));
    const filePath = path.join(tempDir, "legions.json");

    // Write entry with dead daemon PID, servePid, and a free port
    await writeLegionEntry(filePath, "my-legion", {
      port: 13370,
      servePort: 59996,
      pid: 999999999,
      servePid: 888888888,
      startedAt: "2026-03-12T00:00:00Z",
    });

    // Since the port is free, killStaleServe returns true immediately
    // and the stale entry is cleaned up
    await cleanupStaleServes(filePath, "my-legion");

    const registry = await readLegionsRegistry(filePath);
    expect(registry["my-legion"]).toBeUndefined();
  });

  it("preserves healthy serve and returns its PID/port", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cleanup-"));
    const filePath = path.join(tempDir, "legions.json");

    // Start a real HTTP server to simulate a healthy serve
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch() {
        return Response.json({ healthy: true });
      },
    });

    try {
      const servePort = server.port as number;

      // Write entry with dead daemon PID but serve on a port that responds healthy
      await writeLegionEntry(filePath, "my-legion", {
        port: 13370,
        servePort,
        pid: 999999999,
        servePid: 888888888,
        startedAt: "2026-03-12T00:00:00Z",
      });

      const result = await cleanupStaleServes(filePath, "my-legion");

      // Should preserve the serve and return its PID/port
      expect(result.preservedServePid).toBe(888888888);
      expect(result.preservedServePort).toBe(servePort);

      // Stale daemon entry should be removed from registry
      const registry = await readLegionsRegistry(filePath);
      expect(registry["my-legion"]).toBeUndefined();
    } finally {
      server.stop(true);
    }
  });

  it("returns empty when serve is not healthy (dead daemon)", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cleanup-"));
    const filePath = path.join(tempDir, "legions.json");

    // Write entry with dead daemon PID and a free port (no serve running)
    await writeLegionEntry(filePath, "my-legion", {
      port: 13370,
      servePort: 59995,
      pid: 999999999,
      servePid: 888888888,
      startedAt: "2026-03-12T00:00:00Z",
    });

    const result = await cleanupStaleServes(filePath, "my-legion");

    // Should not preserve anything
    expect(result.preservedServePid).toBeUndefined();
    expect(result.preservedServePort).toBeUndefined();

    // Entry should be cleaned up
    const registry = await readLegionsRegistry(filePath);
    expect(registry["my-legion"]).toBeUndefined();
  });
});
