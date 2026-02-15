import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readdir, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { readStateFile, writeStateFile } from "../state-file";

const sampleState = {
  workers: {
    "eng-42-implement": {
      id: "eng-42-implement",
      port: 14444,
      pid: 9999,
      sessionId: "ses_test",
      workspace: "/tmp/test-workspace",
      startedAt: "2026-01-01T00:00:00.000Z",
      status: "running" as const,
      crashCount: 0,
      lastCrashAt: null,
    },
  },
  crashHistory: {
    "eng-42-implement": { crashCount: 1, lastCrashAt: "2026-01-02T00:00:00.000Z" },
  },
};

describe("state-file", () => {
  let tempDir: string | null = null;

  afterEach(async () => {
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("returns empty state when file missing", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    const state = await readStateFile(path.join(tempDir, "workers.json"));
    expect(state).toEqual({ workers: {}, crashHistory: {} });
  });

  it("writes and reads state file roundtrip", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    const filePath = path.join(tempDir, "workers.json");

    await writeStateFile(filePath, sampleState);
    const state = await readStateFile(filePath);

    expect(state).toEqual(sampleState);
  });

  it("writes atomically without leaving temp files", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    const filePath = path.join(tempDir, "workers.json");

    await writeStateFile(filePath, sampleState);
    const entries = await readdir(tempDir);

    expect(entries).toEqual(["workers.json"]);
  });

  it("roundtrips controller state", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    const filePath = path.join(tempDir, "workers.json");

    const stateWithController = {
      ...sampleState,
      controller: { sessionId: "ses_abc", port: 13381, pid: 1234 },
    };

    await writeStateFile(filePath, stateWithController);
    const state = await readStateFile(filePath);

    expect(state.controller).toEqual({
      sessionId: "ses_abc",
      port: 13381,
      pid: 1234,
    });
  });

  it("migrates legacy controller-controller worker entry to controller field", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    const filePath = path.join(tempDir, "workers.json");

    const stateWithLegacy = {
      workers: {
        "eng-42-implement": sampleState.workers["eng-42-implement"],
        "controller-controller": {
          id: "controller-controller",
          port: 13370,
          pid: 5555,
          sessionId: "ses_legacy",
          workspace: "/tmp/test-workspace",
          startedAt: "2026-01-01T00:00:00.000Z",
          status: "running" as const,
          crashCount: 0,
          lastCrashAt: null,
        },
      },
      crashHistory: {
        ...sampleState.crashHistory,
        "controller-controller": { crashCount: 1, lastCrashAt: "2026-01-03T00:00:00.000Z" },
      },
    };

    await writeStateFile(filePath, stateWithLegacy);
    const state = await readStateFile(filePath);

    expect(state.workers["controller-controller"]).toBeUndefined();
    expect(state.crashHistory["controller-controller"]).toBeUndefined();
    expect(state.workers["eng-42-implement"]).toBeDefined();
    expect(state.controller).toEqual({
      sessionId: "ses_legacy",
      port: 13370,
      pid: 5555,
    });
  });

  it("reads state files without controller field (backward compat)", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    const filePath = path.join(tempDir, "workers.json");

    const stateWithoutController = {
      workers: sampleState.workers,
      crashHistory: sampleState.crashHistory,
    };

    await writeFile(filePath, JSON.stringify(stateWithoutController, null, 2));
    const state = await readStateFile(filePath);

    expect(state.workers).toEqual(sampleState.workers);
    expect(state.crashHistory).toEqual(sampleState.crashHistory);
    expect(state.controller).toBeUndefined();
  });
});
